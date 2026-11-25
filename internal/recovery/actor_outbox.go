package recovery

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/oddin-gg/gosdk/types"
)

// The actor's ends towards the world: the catalog the machine reads, and the
// outbox it writes to. Both are called on the actor's goroutine. What takes
// time, the REST requests, runs on goroutines of its own, and its outcome comes
// back as a fact.

// actorCatalog is the machine's view of the producer manager.
type actorCatalog struct {
	a *recoveryActor
	// list is the producers the catalog had when the feed opened, by id.
	list []types.Producer
	// seeds are the recovery starts the client had set when each producer was
	// first read: what a lane of a producer that has not recovered starts from,
	// whatever the client sets later. Actor goroutine only.
	seeds map[int]time.Time
}

func newActorCatalog(a *recoveryActor, producers []types.Producer) *actorCatalog {
	list := slices.Clone(producers)
	slices.SortFunc(list, func(x, y types.Producer) int { return x.ID() - y.ID() })
	return &actorCatalog{a: a, list: list, seeds: make(map[int]time.Time)}
}

func (c *actorCatalog) producers() []types.Producer { return slices.Clone(c.list) }

func (c *actorCatalog) producer(id int) (types.Producer, bool) {
	p, err := c.a.pm.GetProducer(c.a.ctx, id)
	if err != nil {
		return nil, false
	}
	return p, true
}

// enabled reports whether the producer is enabled; a catalog that cannot answer
// says it is not, so nothing is asked for what may be switched off.
func (c *actorCatalog) enabled(id int) bool {
	enabled, err := c.a.pm.IsProducerEnabled(c.a.ctx, id)
	if err != nil {
		c.a.logger.WithError(err).WithField("producer_id", id).
			Warn("recovery: the producer's state is unavailable; treating it as disabled")
		return false
	}
	return enabled
}

func (c *actorCatalog) recoveryFrom(id int) time.Time {
	if at, ok := c.seeds[id]; ok {
		return at
	}
	at, err := c.a.pm.RecoveryFromTimestamp(id)
	if err != nil {
		return time.Time{}
	}
	c.seeds[id] = at
	return at
}

// errNotAccepted is the answer of a request the API did not accept, with no
// error of its own.
var errNotAccepted = errors.New("the API did not accept the request")

// answerOf is what a REST worker tells the machine of its request's outcome.
func answerOf(success bool, err error) error {
	switch {
	case err != nil:
		return err
	case !success:
		return errNotAccepted
	}
	return nil
}

// ---- the outbox

// outbox is the machine's outbox: the REST requests run on detached goroutines
// bounded by the actor's lifetime, the status and the resume points go to the
// producer manager, and what the client hears goes to the manager's stream and
// handles.

func (a *recoveryActor) request(r snapshotRequest) {
	a.remember(r.requestID)
	if a.isClosed() {
		return
	}
	// a request superseded by the next one for the producer is not retried beside it
	if cancel := a.snapshotCancels[r.producerID]; cancel != nil {
		cancel()
	}
	ctx, cancel := context.WithCancel(a.ctx)
	a.snapshotCancels[r.producerID] = cancel
	nodeID := a.cfg.SdkNodeID()
	a.detached.Add(1) // joined (bounded) by stopBounded
	go func() {
		defer a.detached.Done()
		defer cancel()
		success, err := a.api.PostRecovery(ctx, r.producer, r.requestID, nodeID, r.after)
		a.postEssential(factAnswered{requestID: r.requestID, err: answerOf(success, err)})
	}()
}

func (a *recoveryActor) eventRequest(r eventRequest) {
	a.remember(r.requestID)
	if a.isClosed() {
		return
	}
	ctx, cancel := context.WithCancel(a.ctx)
	a.eventCancels[r.requestID] = cancel
	nodeID := a.cfg.SdkNodeID()
	a.detached.Add(1) // joined (bounded) by stopBounded
	go func() {
		defer a.detached.Done()
		defer cancel()
		var (
			success bool
			err     error
		)
		if r.stateful {
			success, err = a.api.PostEventStatefulRecovery(ctx, r.producer, r.eventID, r.requestID, nodeID)
		} else {
			success, err = a.api.PostEventOddsRecovery(ctx, r.producer, r.eventID, r.requestID, nodeID)
		}
		a.postEssential(factAnswered{requestID: r.requestID, err: answerOf(success, err)})
	}()
}

func (a *recoveryActor) recoveryInfo(producerID int, info types.RecoveryInfo) {
	if err := a.pm.SetProducerRecoveryInfo(producerID, info); err != nil {
		a.logger.WithError(err).WithField("producer_id", producerID).
			WithField("request_id", info.RequestID()).
			Error("recovery: set producer recovery info")
	}
}

func (a *recoveryActor) resumePoint(producerID int, point time.Time) {
	if err := a.pm.SetProducerResumePoint(producerID, point); err != nil {
		a.logger.WithError(err).WithField("producer_id", producerID).
			Error("recovery: set producer resume point")
	}
}

func (a *recoveryActor) status(c statusChange) {
	if err := a.pm.SetProducerDown(c.producerID, c.down); err != nil {
		a.logger.WithError(err).WithField("producer_id", c.producerID).
			Error("recovery: set producer down")
		return
	}
	if !c.public {
		return
	}
	prod, err := a.pm.GetProducer(a.ctx, c.producerID)
	if err != nil {
		a.logger.WithError(err).WithField("producer_id", c.producerID).
			Warn("recovery: publish producer status")
		return
	}
	impl := &producerStatusImpl{
		producer: prod,
		timestamp: types.MessageTimestamp{
			Created: c.at, Sent: c.at, Received: c.at, Published: c.at,
		},
		isDown:               c.down,
		isDelayed:            c.delayed,
		producerStatusReason: c.reason(),
	}
	a.statusMu.Lock()
	a.statuses[c.producerID] = impl
	a.statusMu.Unlock()
	a.mgr.emitRecoveryMessage(types.RecoveryMessage{ProducerStatus: impl})
}

func (a *recoveryActor) eventEnded(o eventOutcome) {
	if cancel := a.eventCancels[o.requestID]; cancel != nil {
		// a recovery that ended, by expiry say, does not keep its request running
		cancel()
		delete(a.eventCancels, o.requestID)
	}
	err := o.err
	switch {
	case err == nil:
	case errors.Is(err, errFeedClosing):
		err = ErrManagerClosed
	default:
		err = fmt.Errorf("recovery: producer %d req=%d event recovery: %w", o.producerID, o.requestID, err)
	}
	if o.status == types.RecoveryStatusCompleted {
		a.emitEventRecovered(o)
	}
	// even if the stream drops the message, the handle ends and Done() unblocks
	a.mgr.completeHandle(o.requestID, o.status, err)
}

// emitEventRecovered publishes the event recovery's completion on the stream.
func (a *recoveryActor) emitEventRecovered(o eventOutcome) {
	prod, err := a.pm.GetProducer(a.ctx, o.producerID)
	if err != nil {
		a.logger.WithError(err).WithField("producer_id", o.producerID).
			WithField("request_id", o.requestID).
			Error("recovery: event recovery finished, but its producer is unavailable")
		return
	}
	now := a.clock.Now().wall
	a.mgr.emitRecoveryMessage(types.RecoveryMessage{
		EventRecoveryMessage: &eventRecoveryMessageImpl{
			eventID:   o.eventID,
			requestID: o.requestID,
			producer:  prod,
			timestamp: types.MessageTimestamp{
				Created: now, Sent: now, Received: now, Published: now,
			},
		},
	})
}

// currentStatus is the most recent status published for the producer, nil for
// none. Safe from any goroutine.
func (a *recoveryActor) currentStatus(producerID int) types.ProducerStatus {
	a.statusMu.RLock()
	defer a.statusMu.RUnlock()
	if s := a.statuses[producerID]; s != nil {
		return s
	}
	return nil
}
