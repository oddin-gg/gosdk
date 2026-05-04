package recovery

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/producer"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/protocols"
)

// recoveryActor owns all state for a single producer's recovery state
// machine. A single goroutine processes events from inbox; nothing
// else mutates the actor's fields. No mutex needed on the per-producer
// state. Cross-actor and cross-package mutations (handles map, msgCh,
// producer.Manager) go through thread-safe abstractions.
//
// Phase 5 v2 architecture: replaces the previous mutex-guarded
// producerRecoveryData + central manager-locks with a single-threaded
// owner per producer (NEXT.md §11). State-machine semantics are
// preserved exactly — Java/Kotlin and .NET reference SDKs match.
type recoveryActor struct {
	// Immutable after construction.
	producerID uint
	cfg        protocols.OddsFeedConfiguration
	api        *api.Client
	pm         *producer.Manager
	mgr        actorManagerOps // narrow interface back to the Manager
	logger     *log.Logger

	// Inbox + lifecycle.
	inbox    chan actorEvent
	shutdown chan struct{}
	done     chan struct{}

	// Manager-lifetime ctx, used for API calls. Cancelled at shutdown.
	ctx context.Context

	// Per-producer state. Only the actor goroutine touches these.
	recoveryState          protocols.RecoveryState
	currentRecovery        *recoveryData
	eventRecoveries        map[uint]*eventRecovery
	lastUserSessionAlive   time.Time
	lastValidAliveGen      time.Time  // gen-timestamp captured during recovery
	lastSystemAlive        *time.Time // pointer: nil = never seen
	firstRecoveryCompleted bool
	downReason             protocols.ProducerDownReason
	statusReason           protocols.ProducerStatusReason
}

// actorManagerOps is the narrow surface the actor needs back into the
// Manager. Limiting it makes the dependency explicit and keeps test
// doubles simple.
type actorManagerOps interface {
	registerHandle(*Handle)
	completeHandle(requestID uint, status protocols.RecoveryRequestStatus, err error) *Handle
	nextRequestID() uint
	emitRecoveryMessage(protocols.RecoveryMessage)
}

func newRecoveryActor(
	ctx context.Context,
	producerID uint,
	cfg protocols.OddsFeedConfiguration,
	apiClient *api.Client,
	pm *producer.Manager,
	mgr actorManagerOps,
	logger *log.Logger,
	inboxSize int,
) *recoveryActor {
	if inboxSize <= 0 {
		inboxSize = 256
	}
	return &recoveryActor{
		producerID:      producerID,
		cfg:             cfg,
		api:             apiClient,
		pm:              pm,
		mgr:             mgr,
		logger:          logger,
		ctx:             ctx,
		inbox:           make(chan actorEvent, inboxSize),
		shutdown:        make(chan struct{}),
		done:            make(chan struct{}),
		eventRecoveries: make(map[uint]*eventRecovery),
	}
}

// run is the actor's main loop.
func (a *recoveryActor) run() {
	defer close(a.done)
	for {
		select {
		case ev := <-a.inbox:
			a.dispatch(ev)
		case <-a.shutdown:
			return
		}
	}
}

// send pushes an event to the inbox. Lossy: returns false when full
// (callers may log/drop). Tick events are tolerated to drop because
// the next tick arrives in 10s.
func (a *recoveryActor) send(ev actorEvent) bool {
	select {
	case a.inbox <- ev:
		return true
	default:
		return false
	}
}

// sendBlocking pushes an event, blocking if the inbox is full.
// Used for synchronous commands (recoverEvent) where the caller
// must reach the actor.
func (a *recoveryActor) sendBlocking(ev actorEvent) {
	a.inbox <- ev
}

// stop terminates the actor. Idempotent.
func (a *recoveryActor) stop() {
	select {
	case <-a.shutdown:
		// already stopping
	default:
		close(a.shutdown)
	}
	<-a.done
}

// dispatch routes an event to the matching handler. New event types
// must be added here.
func (a *recoveryActor) dispatch(ev actorEvent) {
	switch e := ev.(type) {
	case evMsgProcessingStarted:
		a.onMessageProcessingStarted(e.timestamp)
	case evMsgProcessingEnded:
		a.onMessageProcessingEnded(e.timestamp)
	case evAlive:
		a.onAlive(e)
	case evSnapshotComplete:
		a.onSnapshotComplete(e)
	case evRecoverEvent:
		a.onRecoverEvent(e)
	case evTick:
		a.onTick(e.now)
	default:
		a.logger.Warnf("recovery actor: unknown event type %T", ev)
	}
}

// --- Producer-manager state queries (delegate to thread-safe pm) ---

func (a *recoveryActor) isDisabled() bool {
	enabled, err := a.pm.IsProducerEnabled(a.ctx, a.producerID)
	if err != nil {
		return true
	}
	return !enabled
}

func (a *recoveryActor) isFlaggedDown() bool {
	down, err := a.pm.IsProducerDown(a.ctx, a.producerID)
	if err != nil {
		return true
	}
	return down
}

func (a *recoveryActor) producerName() (string, error) {
	prod, err := a.pm.GetProducer(a.ctx, a.producerID)
	if err != nil {
		return "", err
	}
	return prod.Name(), nil
}

func (a *recoveryActor) timestampForRecovery() (time.Time, error) {
	prod, err := a.pm.GetProducer(a.ctx, a.producerID)
	if err != nil {
		return time.Time{}, err
	}
	return prod.TimestampForRecovery(), nil
}

func (a *recoveryActor) lastProcessedMessageGenTimestamp() (time.Time, error) {
	prod, err := a.pm.GetProducer(a.ctx, a.producerID)
	if err != nil {
		return time.Time{}, err
	}
	return prod.LastProcessedMessageGenTimestamp(), nil
}

// --- Per-producer state queries (no mutex — actor goroutine only) ---

func (a *recoveryActor) isPerformingRecovery() bool {
	return a.recoveryState == protocols.StartedRecoveryState ||
		a.recoveryState == protocols.InterruptedRecoveryState
}

func (a *recoveryActor) isKnownRecovery(requestID uint) bool {
	if a.currentRecovery != nil && a.currentRecovery.recoveryID == requestID {
		return true
	}
	_, ok := a.eventRecoveries[requestID]
	return ok
}

func (a *recoveryActor) snapshotValidationNeeded(interest protocols.MessageInterest) bool {
	return interest == protocols.LiveOnlyMessageInterest ||
		interest == protocols.PrematchOnlyMessageInterest
}

// validateSnapshotComplete checks whether a SnapshotComplete with this
// requestID + interest finishes the current full snapshot recovery.
//
// Logic preserved exactly from the pre-actor implementation; matches
// Java/Kotlin and .NET reference SDKs (all three accept Started OR
// Interrupted state via the !isPerformingRecovery gate).
func (a *recoveryActor) validateSnapshotComplete(requestID uint, interest protocols.MessageInterest) bool {
	if !a.isPerformingRecovery() {
		return false
	}
	if a.currentRecovery == nil || a.currentRecovery.recoveryID != requestID {
		return false
	}
	if !a.snapshotValidationNeeded(interest) {
		return true
	}
	res, err := a.validateProducerSnapshotCompletes(a.currentRecovery.snapshotComplete(interest))
	if err != nil {
		return false
	}
	return res
}

func (a *recoveryActor) validateEventSnapshotComplete(requestID uint, interest protocols.MessageInterest) bool {
	er, ok := a.eventRecoveries[requestID]
	if !ok {
		return false
	}
	if !a.snapshotValidationNeeded(interest) {
		return true
	}
	res, err := a.validateProducerSnapshotCompletes(er.snapshotComplete(interest))
	if err != nil {
		return false
	}
	return res
}

// validateProducerSnapshotCompletes checks each producer scope has had
// its corresponding SnapshotComplete reported.
func (a *recoveryActor) validateProducerSnapshotCompletes(received []protocols.MessageInterest) (bool, error) {
	prod, err := a.pm.GetProducer(a.ctx, a.producerID)
	if err != nil {
		return false, err
	}
	finished := make([]bool, len(prod.ProducerScopes()))
	for i, scope := range prod.ProducerScopes() {
		for _, interest := range received {
			switch scope {
			case protocols.LiveProducerScope:
				finished[i] = interest == protocols.LiveOnlyMessageInterest
			case protocols.PrematchProducerScope:
				finished[i] = interest == protocols.PrematchOnlyMessageInterest
			default:
				return false, errors.New("unknown producer scope")
			}
		}
	}
	for _, v := range finished {
		if !v {
			return false, nil
		}
	}
	return true, nil
}

// --- Event handlers ---

func (a *recoveryActor) onMessageProcessingStarted(t time.Time) {
	if t.IsZero() {
		a.logger.Warn("processing started with zero timestamp")
		return
	}
	if err := a.pm.SetProducerLastMessageTimestamp(a.producerID, t); err != nil {
		a.logger.WithError(err).Error("set producer last message timestamp")
	}
}

func (a *recoveryActor) onMessageProcessingEnded(t time.Time) {
	if t.IsZero() {
		return
	}
	if err := a.pm.SetLastProcessedMessageGenTimestamp(a.producerID, t); err != nil {
		a.logger.WithError(err).Error("set last processed gen timestamp")
	}
}

func (a *recoveryActor) onAlive(e evAlive) {
	if a.isDisabled() {
		return
	}
	if e.messageInterest == protocols.SystemAliveOnly {
		if err := a.systemAliveReceived(e.timestamp, e.isSubscribed); err != nil {
			a.logger.WithError(err).Error("failed to process alive")
		}
		return
	}
	a.lastUserSessionAlive = e.timestamp.Created
}

func (a *recoveryActor) onSnapshotComplete(e evSnapshotComplete) {
	switch {
	case a.isDisabled():
		a.logger.Infof("received snapshot recovery complete for disabled producer %d", a.producerID)
	case !a.isKnownRecovery(e.requestID):
		a.logger.Infof("unknown snapshot recovery complete received for request %d and producer %d", e.requestID, a.producerID)
	case a.validateEventSnapshotComplete(e.requestID, e.messageInterest):
		if err := a.eventRecoveryFinished(e.requestID); err != nil {
			a.logger.WithError(err).Error("event recovery failed")
		}
	case a.validateSnapshotComplete(e.requestID, e.messageInterest):
		if err := a.snapshotRecoveryFinished(e.requestID); err != nil {
			a.logger.WithError(err).Error("snapshot recovery finished failed")
		}
	}
}

func (a *recoveryActor) onTick(now time.Time) {
	if a.isDisabled() {
		return
	}
	var lastTimestamp time.Time
	if a.lastSystemAlive != nil {
		lastTimestamp = *a.lastSystemAlive
	}
	aliveInterval := now.Sub(lastTimestamp)
	var err error
	switch {
	case aliveInterval.Seconds() > float64(a.cfg.MaxInactivitySeconds()):
		err = a.producerDown(protocols.AliveInternalViolationProducerDownReason)
	case !a.calculateTiming(now):
		err = a.producerDown(protocols.ProcessingQueueDelayViolationProducerDownReason)
	}
	if err != nil {
		a.logger.WithError(err).Errorf("failed to check recovery")
	}
}

func (a *recoveryActor) onRecoverEvent(e evRecoverEvent) {
	now := time.Now()
	producerName, err := a.producerName()
	if err != nil {
		e.reply <- recoverEventReply{err: err}
		return
	}
	requestID := a.mgr.nextRequestID()
	handle := NewHandle(requestID, a.producerID, e.eventID, now)
	a.mgr.registerHandle(handle)
	a.eventRecoveries[requestID] = newEventRecovery(e.eventID, requestID, now)

	var success bool
	if e.statefulRecovery {
		success, err = a.api.PostEventStatefulRecovery(e.ctx, producerName, e.eventID, requestID, a.cfg.SdkNodeID())
	} else {
		success, err = a.api.PostEventOddsRecovery(e.ctx, producerName, e.eventID, requestID, a.cfg.SdkNodeID())
	}
	if !success {
		delete(a.eventRecoveries, requestID)
	}
	if err != nil {
		a.logger.WithError(err).Error("event recovery failed")
		a.mgr.completeHandle(requestID, protocols.RecoveryStatusFailed, err)
		e.reply <- recoverEventReply{err: err}
		return
	}
	e.reply <- recoverEventReply{handle: handle}
}

// --- State-machine helpers (preserved verbatim from manager.go) ---

func (a *recoveryActor) systemAliveReceived(timestamp protocols.MessageTimestamp, subscribed bool) error {
	if err := a.pm.SetProducerLastMessageTimestamp(a.producerID, timestamp.Received); err != nil {
		return err
	}

	recoveryTimestamp, err := a.timestampForRecovery()
	if err != nil {
		return err
	}

	if !subscribed {
		if !a.isFlaggedDown() {
			if err := a.producerDown(protocols.OtherProducerDownReason); err != nil {
				return err
			}
		}
		return a.makeSnapshotRecovery(recoveryTimestamp)
	}

	now := time.Now()
	state := a.recoveryState
	downReason := a.downReason
	isBackFromInactivity := a.isFlaggedDown() &&
		!a.isPerformingRecovery() &&
		downReason == protocols.ProcessingQueueDelayViolationProducerDownReason &&
		a.calculateTiming(now)
	isInRecovery := state != protocols.NotStartedRecoveryState &&
		state != protocols.ErrorRecoveryState &&
		state != protocols.InterruptedRecoveryState

	switch {
	case isBackFromInactivity:
		err = a.producerUp(protocols.ReturnedFromInactivityProducerUpReason)
	case isInRecovery:
		if a.isFlaggedDown() && !a.isPerformingRecovery() && a.downReason != protocols.ProcessingQueueDelayViolationProducerDownReason {
			if err := a.makeSnapshotRecovery(recoveryTimestamp); err != nil {
				return err
			}
		}
		recoveryTiming := now.Sub(a.lastRecoveryStartedAt())
		maxInterval := float64(a.cfg.MaxRecoveryExecutionMinutes())
		if a.isPerformingRecovery() && recoveryTiming.Minutes() > maxInterval {
			a.recoveryState = protocols.ErrorRecoveryState
			a.currentRecovery = nil
			if err := a.makeSnapshotRecovery(recoveryTimestamp); err != nil {
				return err
			}
		}
	default:
		err = a.makeSnapshotRecovery(recoveryTimestamp)
	}
	if err != nil {
		return err
	}

	// Per-producer state mutation (was: data.systemAliveReceived).
	t := timestamp.Received
	a.lastSystemAlive = &t
	if a.recoveryState == protocols.StartedRecoveryState {
		a.lastValidAliveGen = timestamp.Created
	}
	if !a.isFlaggedDown() {
		if err := a.pm.SetLastAliveReceivedGenTimestamp(a.producerID, timestamp.Created); err != nil {
			return err
		}
	}
	return nil
}

func (a *recoveryActor) lastRecoveryStartedAt() time.Time {
	if a.currentRecovery != nil {
		return a.currentRecovery.recoveryStartedAt
	}
	return time.Time{}
}

func (a *recoveryActor) calculateTiming(now time.Time) bool {
	maxInactivity := float64(a.cfg.MaxInactivitySeconds())
	lastProcessed, err := a.lastProcessedMessageGenTimestamp()
	if err != nil {
		a.logger.WithError(err).Warn("failed to get last processed message gen timestamp")
		return false
	}
	messageProcessingDelay := now.Sub(lastProcessed)
	userAliveDelay := now.Sub(a.lastUserSessionAlive)
	return math.Abs(messageProcessingDelay.Seconds()) < maxInactivity &&
		math.Abs(userAliveDelay.Seconds()) < maxInactivity
}

// producerDown matches manager.producerDown exactly. Pre-actor:
// data.setProducerDown crossed into producer.Manager (mutex-protected),
// then notifyProducerChangedState emitted on msgCh. The actor flow is
// the same — only the actor's own state mutates without locks.
func (a *recoveryActor) producerDown(reason protocols.ProducerDownReason) error {
	if a.isDisabled() {
		return nil
	}

	if a.isFlaggedDown() && a.downReason != reason {
		name, err := a.producerName()
		if err != nil {
			return err
		}
		a.logger.Infof("changing producer %s down reason from %d to %d", name, a.downReason, reason)
		if err := a.pm.SetProducerDown(a.producerID, true); err != nil {
			return err
		}
		a.downReason = reason
		a.eventRecoveries = make(map[uint]*eventRecovery)
	}

	if a.recoveryState == protocols.StartedRecoveryState && reason != protocols.ProcessingQueueDelayViolationProducerDownReason {
		a.recoveryState = protocols.InterruptedRecoveryState
	}

	if !a.isFlaggedDown() {
		if err := a.pm.SetProducerDown(a.producerID, true); err != nil {
			return err
		}
		a.downReason = reason
		a.eventRecoveries = make(map[uint]*eventRecovery)
	}

	return a.notifyProducerChangedState(reason.ToProducerStatusReason())
}

func (a *recoveryActor) producerUp(reason protocols.ProducerUpReason) error {
	if a.isDisabled() {
		return nil
	}
	if a.isFlaggedDown() {
		if err := a.pm.SetProducerDown(a.producerID, false); err != nil {
			return err
		}
		a.downReason = protocols.DefaultProducerDownReason
	}
	return a.notifyProducerChangedState(reason.ToProducerStatusReason())
}

func (a *recoveryActor) notifyProducerChangedState(reason protocols.ProducerStatusReason) error {
	if a.statusReason == reason {
		return nil
	}
	a.statusReason = reason

	producerData, err := a.pm.GetProducer(a.ctx, a.producerID)
	if err != nil {
		return err
	}
	now := time.Now()
	delayed := !a.calculateTiming(now)
	msg := newProducerStatusImpl(
		producerData,
		protocols.MessageTimestamp{
			Created: now, Sent: now, Received: now, Published: now,
		},
		a.isFlaggedDown(),
		delayed,
		reason,
	)
	a.mgr.emitRecoveryMessage(protocols.RecoveryMessage{ProducerStatus: msg})
	return nil
}

func (a *recoveryActor) makeSnapshotRecovery(timestamp time.Time) error {
	now := time.Now()
	recoverFrom := timestamp
	if !timestamp.IsZero() {
		recoveryTime := now.Sub(recoverFrom)
		if recoveryTime.Minutes() > float64(a.cfg.MaxRecoveryExecutionMinutes()) {
			recoverFrom = now.Add(-time.Duration(a.cfg.MaxRecoveryExecutionMinutes()) * time.Minute)
		}
	}

	requestID := a.mgr.nextRequestID()
	producerName, err := a.producerName()
	if err != nil {
		return err
	}

	a.currentRecovery = newRecoveryData(requestID, now)
	a.recoveryState = protocols.StartedRecoveryState

	a.logger.Infof("recovery started for request %d", requestID)

	success, err := a.api.PostRecovery(a.ctx, producerName, requestID, a.cfg.SdkNodeID(), recoverFrom)
	if err != nil {
		return err
	}
	recoveryInfo := newRecoveryInfoImpl(recoverFrom, now, requestID, success, a.cfg.SdkNodeID())
	return a.pm.SetProducerRecoveryInfo(a.producerID, recoveryInfo)
}

func (a *recoveryActor) snapshotRecoveryFinished(requestID uint) error {
	started := a.lastRecoveryStartedAt()
	if started.IsZero() {
		return errors.New("inconsistent recovery state")
	}
	finished := time.Now()
	a.logger.Infof("recovery finished for request %d in %d ms", requestID, finished.Sub(started).Milliseconds())

	if a.recoveryState == protocols.InterruptedRecoveryState {
		if err := a.makeSnapshotRecovery(a.lastValidAliveGen); err != nil {
			return err
		}
	}

	var reason protocols.ProducerUpReason
	if a.firstRecoveryCompleted {
		reason = protocols.ReturnedFromInactivityProducerUpReason
	} else {
		reason = protocols.FirstRecoveryCompletedProducerUpReason
		a.firstRecoveryCompleted = true
	}

	a.currentRecovery = newRecoveryData(requestID, started)
	a.recoveryState = protocols.CompletedRecoveryState
	return a.producerUp(reason)
}

func (a *recoveryActor) eventRecoveryFinished(id uint) error {
	er, ok := a.eventRecoveries[id]
	if !ok {
		return errors.New("inconsistent event recovery state")
	}
	started := er.recoveryStartedAt
	finished := time.Now()
	a.logger.Infof("event %s recovery finished for request %d in %d ms", er.eventID.ToString(), id, finished.Sub(started).Milliseconds())

	producerData, err := a.pm.GetProducer(a.ctx, a.producerID)
	if err != nil {
		return err
	}
	a.mgr.emitRecoveryMessage(protocols.RecoveryMessage{
		EventRecoveryMessage: &eventRecoveryMessageImpl{
			eventID:   er.eventID,
			requestID: id,
			producer:  producerData,
			timestamp: protocols.MessageTimestamp{
				Created: finished, Sent: finished, Received: finished, Published: finished,
			},
		},
	})

	// Reliable per-request completion (NEXT.md §11): even if the
	// channel send above is dropped (lossy + slow consumer), the
	// handle is updated and unblocks any caller blocked on Done().
	a.mgr.completeHandle(id, protocols.RecoveryStatusCompleted, nil)

	delete(a.eventRecoveries, id)
	return nil
}

