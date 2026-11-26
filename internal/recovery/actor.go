package recovery

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/config"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/internal/producer"
	"github.com/oddin-gg/gosdk/types"
)

// recoveryActor is the goroutine that owns the recovery machine, and so every
// producer's recovery state. Nobody else touches that state, so it needs no
// lock: whoever has something to tell puts a fact in a queue and returns, never
// waiting. One actor serves every producer.
//
// Three queues. The essential facts (sessions opening and closing, the start, the
// alives, snapshot_completes, lost and reopened channels, the API's answers, a
// recovery start the client sets) are never dropped, since losing one would
// leave the state wrong for good: an unsubscribed alive is the only word of a
// gap. They keep their order. That queue has no capacity of its own: what fills
// it is bounded by what the feed does and by the producers' own pace, since
// the alives of a producer wait in one fact until another essential fact is posted
// (so a producer costs one fact between two other essential facts, however many
// alives it sends) and a snapshot_complete of a request the feed never asked for is
// dropped as it is posted. The commands, the
// client's requests for an event recovery, are bounded, and one with no room is
// refused. The samples, the messages and alives a session finished, are many and
// each worth little: they are bounded too, and a full queue drops one. The actor
// takes the essential facts first, then the commands, then the samples, and the
// two lesser queues yield as soon as an essential fact waits, so nothing posted
// after an essential fact is handled before it, and a flood of samples holds up
// nothing else.
//
// The actor looks at the clock once a second, whatever else it is doing: the
// ticker counts on the monotonic clock, so a step of the wall clock neither
// stops the ticks nor makes them come in a burst.
//
// The REST requests run on goroutines of their own, bounded by the actor's
// lifetime, never here, and their outcomes come back as facts.
//
// Safe for concurrent use.
type recoveryActor struct {
	cfg    config.Config
	api    *api.Client
	pm     *producer.Manager
	mgr    actorManagerOps
	logger *log.Logger
	clock  Clock
	// ctx is the manager's lifetime: the REST requests are bounded by it.
	ctx context.Context

	machine *machine
	catalog *actorCatalog

	// essential is the queue of the facts that are never dropped, and closed says
	// the actor has taken what it holds for good: both guarded by essentialMu, so
	// a fact either is in the queue when the actor closes, to be handled, or
	// sees the close and is refused.
	essentialMu sync.Mutex
	essential   []fact
	closed      bool
	// stopRequested says the close has been signalled: the ordinary drain takes no
	// essential fact from then on, and what is queued is the close's. Set, together
	// with the close of shutdown, under essentialMu, so a take either happened
	// before the close was signalled or sees it. Guarded by essentialMu.
	stopRequested bool
	// wake nudges the actor's goroutine: one pending nudge says there is something to take.
	wake     chan struct{}
	commands chan *recoverEventCommand
	samples  chan fact

	// openAlives is, per producer, the last alives fact queued for it, and otherSeq
	// counts the essential facts that are not alives: an alive joins its producer's
	// fact while it waits in the queue and no other essential fact was posted after
	// it. Guarded by essentialMu.
	openAlives map[int]*factAlives
	otherSeq   uint64

	// issued are the request ids the actor has asked for: a snapshot_complete of
	// any other is of a request this feed never made (another instance's, which
	// the broker routes here too). The ids of the requests in flight are all
	// kept, so a snapshot_complete that comes hours after its request is not
	// taken for a stray; the ids of the requests that ended are a bounded
	// memory, endedOrder the oldest first, issuedKept of them at most.
	issuedMu   sync.Mutex
	issued     map[int]struct{}
	inFlight   map[int]struct{}
	endedOrder []int
	issuedKept int
	strayTold  atomic.Bool

	shutdown     chan struct{}
	shutdownOnce sync.Once
	done         chan struct{}
	// closing is the same as closed, read without the queue's lock.
	closing atomic.Bool
	// closeBy is the deadline of the close, by the wall clock: what the actor
	// spends on the facts queued when it closes ends there, or at finishWait.
	closeBy    atomic.Pointer[time.Time]
	finishWait time.Duration
	tickEvery  time.Duration
	// detached tracks the REST requests' goroutines. Adds happen on the actor's
	// goroutine only, so the join after it exits never races an Add.
	detached sync.WaitGroup

	// snapshotCancels and eventCancels stop a request that is superseded or has
	// ended. The actor's goroutine only.
	snapshotCancels map[int]context.CancelFunc
	eventCancels    map[int]context.CancelFunc

	statusMu sync.RWMutex
	statuses map[int]*producerStatusImpl

	counters actorCounters
	// failures bounds the log line of a fact the actor failed on.
	failures *log.Throttle

	// beforeHandle, when set, runs inside the handling of each fact, with the
	// fact. Tests only.
	beforeHandle atomic.Pointer[func(fact)]
	// beforeSample, when set, runs before each take from the samples, after the
	// look at the essential queue. Tests only; set before the actor runs.
	beforeSample func()
	// beforeCommand is beforeSample for the commands.
	beforeCommand func()
	// beforeEssential is the same before each take from the essential queue.
	beforeEssential func()
	// afterEssentialCheck runs in the ordinary drain between its look at the close
	// and its take from the essential queue. Tests only; set before the actor runs.
	afterEssentialCheck func()
}

// setBeforeHandle sets what runs inside the handling of each fact. Tests only.
func (a *recoveryActor) setBeforeHandle(fn func(fact)) { a.beforeHandle.Store(&fn) }

// actorCounters count what happened, for logs and tests.
type actorCounters struct {
	samplesDropped    atomic.Uint64
	commandsRefused   atomic.Uint64
	factsFailed       atomic.Uint64
	strayCompletions  atomic.Uint64
	unhandledAtClose  atomic.Uint64
	factsHandledTotal atomic.Uint64
}

const (
	// tickPeriod is how often the actor looks at the clock at least.
	tickPeriod = time.Second
	// sampleCapacity and commandCapacity bound the lesser queues.
	sampleCapacity  = 10_000
	commandCapacity = 10_000
	// samplesPerTurn and commandsPerTurn bound what the actor takes from a lesser
	// queue before it looks at the clock and the other queues again.
	samplesPerTurn  = 1_000
	commandsPerTurn = 1_000
	// issuedKept is how many ids of requests that ended the actor remembers, for
	// a snapshot_complete that is late. The ids of the requests in flight are
	// kept for as long as they are, and are not counted.
	issuedKept = 10_000
	// finishWait is how long the actor spends at most on the essential facts
	// queued when it closes.
	finishWait = 2 * time.Second
)

// maxPendingEventRecoveries bounds the event recoveries pending per producer:
// generous for a burst of requests after an outage, small enough that a caller
// flooding distinct events against a slow recovery API cannot grow handles,
// goroutines and map entries until the maximum recovery time. Identical requests
// (event and kind) coalesce onto one handle and take no extra slot.
const maxPendingEventRecoveries = 128

// ErrTooManyPendingEventRecoveries is returned by RecoverEventOdds /
// RecoverEventStateful when the producer already has
// maxPendingEventRecoveries recoveries awaiting snapshot_complete.
// Retryable once earlier recoveries complete or time out.
var ErrTooManyPendingEventRecoveries = errors.New("recovery: too many pending event recoveries for producer")

// errCommandsFull is why an event recovery is refused when the actor's queue of
// requests has no room: the call fails at once, and can be retried.
var errCommandsFull error = queueFullError{}

// queueFullError is errCommandsFull: its own text, since the queue is the actor's and
// no producer's cap, and it matches ErrTooManyPendingEventRecoveries inside the module
// (both are refusals that are retried once there is room).
type queueFullError struct{}

func (queueFullError) Error() string {
	return "recovery: the request queue of the recovery actor is full; the request was not accepted, retry it"
}

func (queueFullError) Is(target error) bool { return target == ErrTooManyPendingEventRecoveries }

// actorManagerOps is the narrow surface the actor needs back into the
// Manager. Limiting it makes the dependency explicit and keeps test
// doubles simple.
type actorManagerOps interface {
	registerHandle(*Handle)
	completeHandle(requestID int, status types.RecoveryRequestStatus, err error) *Handle
	LookupHandle(requestID int) (*Handle, bool)
	nextRequestID() int
	emitRecoveryMessage(types.RecoveryMessage)
	// processingSince is the sessions with a message of the client in processing
	// now, and since when, on the monotonic clock.
	processingSince() map[uuid.UUID]time.Duration
	// gcHandles forgets the handles that ended long enough ago.
	gcHandles(now time.Time)
	// resetterOf is the handle a session registered to reset its channel
	// through, nil for none.
	resetterOf(session uuid.UUID) Resetter
}

// newRecoveryActor builds the actor and its machine, over the producers the
// catalog has now. It runs nothing: run and start do.
func newRecoveryActor(
	ctx context.Context,
	cfg config.Config,
	apiClient *api.Client,
	pm *producer.Manager,
	mgr actorManagerOps,
	logger *log.Logger,
	clock Clock,
	producers []types.Producer,
	initialSnapshotTime time.Duration,
) *recoveryActor {
	if clock == nil {
		clock = SystemClock()
	}
	a := &recoveryActor{
		cfg:             cfg,
		api:             apiClient,
		pm:              pm,
		mgr:             mgr,
		logger:          logger,
		clock:           clock,
		ctx:             ctx,
		wake:            make(chan struct{}, 1),
		commands:        make(chan *recoverEventCommand, commandCapacity),
		samples:         make(chan fact, sampleCapacity),
		openAlives:      make(map[int]*factAlives),
		issued:          make(map[int]struct{}),
		inFlight:        make(map[int]struct{}),
		issuedKept:      issuedKept,
		shutdown:        make(chan struct{}),
		done:            make(chan struct{}),
		finishWait:      finishWait,
		tickEvery:       tickPeriod,
		snapshotCancels: make(map[int]context.CancelFunc),
		eventCancels:    make(map[int]context.CancelFunc),
		statuses:        make(map[int]*producerStatusImpl),
		failures:        log.NewThrottle(time.Minute, nil),
	}
	a.catalog = newActorCatalog(a, producers)
	a.machine = newMachine(a.catalog, settingsOf(cfg, initialSnapshotTime), a, clock, logger, mgr.nextRequestID)
	return a
}

// settingsOf is the client's settings the recovery rules use. The deadline for a
// snapshot_complete is never above the maximum recovery time, which it
// subdivides.
func settingsOf(cfg config.Config, initialSnapshotTime time.Duration) recoverySettings {
	timeout := cfg.SnapshotCompleteDeadline()
	if maxTime := cfg.MaxRecoveryExecution(); maxTime > 0 && (timeout <= 0 || timeout > maxTime) {
		timeout = maxTime
	}
	return recoverySettings{
		maxInactivity:           cfg.MaxInactivity(),
		maxRecoveryTime:         cfg.MaxRecoveryExecution(),
		snapshotCompleteTimeout: timeout,
		initialSnapshot:         initialSnapshotTime,
		nodeID:                  cfg.SdkNodeID(),
	}
}

// ---- posting

// postEssential queues a fact that is never dropped, and nudges the actor; false
// when the actor has closed, which is the only reason one is refused.
func (a *recoveryActor) postEssential(f fact) bool {
	a.essentialMu.Lock()
	if a.closed {
		a.essentialMu.Unlock()
		return false
	}
	a.essential = append(a.essential, f)
	a.otherSeq++
	a.essentialMu.Unlock()
	a.signal()
	return true
}

// postSample queues a sample, never waiting; one with no room is dropped and
// counted.
func (a *recoveryActor) postSample(f fact) {
	if a.closing.Load() {
		return
	}
	select {
	case a.samples <- f:
		a.signal()
	default:
		n := a.counters.samplesDropped.Add(1)
		// the first, then one in a thousand: a full queue is a wedged actor
		if n == 1 || n%1000 == 0 {
			a.logger.Warnf("The recovery actor's sample queue is full; %d dropped so far", n)
		}
	}
}

// postCommand queues an event recovery request, never waiting. A nil error says
// the actor will answer on the command's reply, or fail it at the close.
func (a *recoveryActor) postCommand(ctx context.Context, c *recoverEventCommand) error {
	// Fast-fail on an already-cancelled ctx before admission: a cancelled ctx
	// racing a free slot must not start a recovery the caller is told did not
	// happen.
	if err := ctx.Err(); err != nil {
		return err
	}
	a.essentialMu.Lock()
	defer a.essentialMu.Unlock()
	if a.closed {
		return ErrManagerClosed
	}
	select {
	case a.commands <- c:
		a.signal()
		return nil
	default:
		a.counters.commandsRefused.Add(1)
		return errCommandsFull
	}
}

// signal nudges the actor's goroutine; a nudge already pending is enough.
func (a *recoveryActor) signal() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// isClosed reports whether the actor has closed.
func (a *recoveryActor) isClosed() bool { return a.closing.Load() }

// ---- the facts of the feed, as the manager gets them

// start posts the opening of the feed.
func (a *recoveryActor) start() { a.postEssential(factStart{}) }

// onAlive posts an alive of the SDK's alive channel, never dropped. An alive joins
// the fact its producer's earlier alives wait in as long as no essential fact other
// than an alive was posted after that fact, so a flood of alives costs one fact per
// producer; an essential fact that is not an alive ends the sharing, so no alive is
// ever handled before such a fact posted before it. The alives of one producer keep
// their order; the alives of different producers, which touch no common state, may
// be handled in another order than they were posted.
func (a *recoveryActor) onAlive(producerID int, generatedAt time.Time, received Stamp, subscribed bool) {
	alive := aliveSeen{generatedAt: generatedAt, received: received, subscribed: subscribed}
	a.essentialMu.Lock()
	if a.closed {
		a.essentialMu.Unlock()
		return
	}
	if open := a.openAlives[producerID]; open != nil && !open.taken && open.after == a.otherSeq {
		open.slot.add(alive)
		a.essentialMu.Unlock()
		return
	}
	f := &factAlives{producer: producerID, after: a.otherSeq}
	f.slot.add(alive)
	a.openAlives[producerID] = f
	a.essential = append(a.essential, f)
	a.essentialMu.Unlock()
	a.signal()
}

// onSnapshotComplete posts a snapshot_complete a session took. A nil error says
// it was taken in, or is of a request this feed never asked for, which completes
// nothing; the delivery may be acked either way. ErrManagerClosed says it was
// not: the delivery must stay unacked for the broker to redeliver.
func (a *recoveryActor) onSnapshotComplete(session uuid.UUID, producerID, requestID int) error {
	if a.isClosed() {
		return ErrManagerClosed
	}
	if !a.wasIssued(requestID) {
		n := a.counters.strayCompletions.Add(1)
		a.logger.Debugf("A snapshot complete of request %d, which this feed did not ask for, is dropped, %d so far", requestID, n)
		if a.strayTold.CompareAndSwap(false, true) {
			a.logger.Infof("A snapshot complete of request %d arrived which this feed did not ask for: another instance "+
				"asked for it, and the broker routes it here too. Give each instance its own node id. "+
				"They are dropped and counted, and logged at debug", requestID)
		}
		return nil
	}
	if !a.postEssential(factSnapshotComplete{session: session, producer: producerID, requestID: requestID}) {
		return ErrManagerClosed
	}
	return nil
}

// remember notes a request id the actor asked for. It is kept until the request
// ends, and then as long as it is among the last issuedKept that ended.
func (a *recoveryActor) remember(requestID int) {
	a.issuedMu.Lock()
	defer a.issuedMu.Unlock()
	a.issued[requestID] = struct{}{}
	a.inFlight[requestID] = struct{}{}
}

// requestEnded says the request is no longer in flight: its id moves to the
// bounded memory of ended ones, and the oldest of those is forgotten past
// issuedKept. An id that was never remembered is ignored.
func (a *recoveryActor) requestEnded(requestID int) {
	a.issuedMu.Lock()
	defer a.issuedMu.Unlock()
	if _, ok := a.inFlight[requestID]; !ok {
		return
	}
	delete(a.inFlight, requestID)
	a.endedOrder = append(a.endedOrder, requestID)
	for len(a.endedOrder) > a.issuedKept {
		oldest := a.endedOrder[0]
		a.endedOrder = a.endedOrder[1:]
		// the machine asks for an id again only once it has forgotten it, so the
		// id may be in flight again by now: that one stays
		if _, live := a.inFlight[oldest]; !live {
			delete(a.issued, oldest)
		}
	}
}

func (a *recoveryActor) wasIssued(requestID int) bool {
	a.issuedMu.Lock()
	defer a.issuedMu.Unlock()
	_, ok := a.issued[requestID]
	return ok
}

// ---- the loop

// run is the actor's goroutine.
func (a *recoveryActor) run() {
	defer close(a.done)
	ticker := time.NewTicker(a.tickEvery)
	defer ticker.Stop()
	for {
		// a close takes precedence over whatever else is ready
		select {
		case <-a.shutdown:
			a.finish()
			return
		default:
		}
		select {
		case <-a.shutdown:
			a.finish()
			return
		case <-a.wake:
		case <-ticker.C:
			a.handle(factTick{})
		}
		a.drain()
	}
}

// drain takes what is queued, the essential facts first, then the commands, then
// the samples; a lesser queue gives up its turn as soon as an essential fact
// waits. A queue not emptied within its turn nudges the actor again, so the
// clock is looked at between the turns.
func (a *recoveryActor) drain() {
	a.drainEssential()
	more := a.drainCommands()
	more = a.drainSamples() || more
	if more || a.hasEssential() {
		a.signal()
	}
}

// takeEssential takes the next essential fact for the ordinary drain; none once
// the close is signalled, the check and the take being one hold of the lock.
func (a *recoveryActor) takeEssential() (fact, bool) {
	a.essentialMu.Lock()
	defer a.essentialMu.Unlock()
	if a.stopRequested || len(a.essential) == 0 {
		return nil, false
	}
	f := a.essential[0]
	if alives, ok := f.(*factAlives); ok {
		alives.taken = true
	}
	a.essential[0] = nil
	a.essential = a.essential[1:]
	if len(a.essential) == 0 {
		a.essential = nil
	}
	return f, true
}

func (a *recoveryActor) hasEssential() bool {
	a.essentialMu.Lock()
	defer a.essentialMu.Unlock()
	return len(a.essential) > 0
}

// signalStop signals the close: the flag and the close of shutdown are one hold
// of the lock, so no ordinary take follows the flag, and the channel is closed
// whenever the flag is seen.
func (a *recoveryActor) signalStop() {
	a.essentialMu.Lock()
	defer a.essentialMu.Unlock()
	a.stopRequested = true
	a.shutdownOnce.Do(func() { close(a.shutdown) })
}

// stopping reports whether the close has been signalled. Only an early exit for
// the drains of the commands and the samples: what the essential queue yields
// rests on the flag takeEssential reads.
func (a *recoveryActor) stopping() bool {
	select {
	case <-a.shutdown:
		return true
	default:
		return false
	}
}

// drainEssential handles the essential facts queued, until the close is
// signalled: what is left then is the close's to handle, once the machine knows
// the feed is closing. A session's closure handled before that would take the
// slowest session out and move a resume point forward, which the close cannot
// take back.
func (a *recoveryActor) drainEssential() {
	for {
		if a.beforeEssential != nil {
			a.beforeEssential()
		}
		if a.stopping() {
			return
		}
		if a.afterEssentialCheck != nil {
			a.afterEssentialCheck()
		}
		f, ok := a.takeEssential()
		if !ok {
			return
		}
		a.handle(f)
	}
}

// drainCommands takes up to commandsPerTurn requests, yielding to the essential
// facts; it reports whether it stopped with more waiting. Once the close is
// signalled it takes no more: the close refuses what is queued.
func (a *recoveryActor) drainCommands() bool {
	for n := 0; n < commandsPerTurn; n++ {
		if a.stopping() {
			return false
		}
		if a.hasEssential() {
			return true
		}
		if a.beforeCommand != nil {
			a.beforeCommand()
		}
		select {
		case c := <-a.commands:
			// what was posted before this request is handled before it
			a.drainEssential()
			if a.stopping() {
				// the facts before it are the close's now, so it is refused, as the
				// ones still queued are
				c.reply <- recoverEventReply{err: ErrManagerClosed}
				return false
			}
			a.handle(c)
		default:
			return false
		}
	}
	return len(a.commands) > 0
}

// drainSamples is drainCommands for the samples. The essential queue is looked at
// after a sample is taken, not only before: whoever posted an essential fact and
// then this sample put the essential fact in first, so once the sample is taken,
// that fact is there to see. A sample from a new channel cannot move a
// checkpoint before the loss of the old one is known.
func (a *recoveryActor) drainSamples() bool {
	for n := 0; n < samplesPerTurn; n++ {
		if a.stopping() {
			return false
		}
		if a.hasEssential() {
			return true
		}
		if a.beforeSample != nil {
			a.beforeSample()
		}
		select {
		case f := <-a.samples:
			a.drainEssential()
			if a.stopping() {
				// dropped, as the close drops the samples still queued
				return false
			}
			a.handle(f)
		default:
			return false
		}
	}
	return len(a.samples) > 0
}

// handle takes one fact to the end, and publishes the resume points it moved,
// even those of a fact that failed part way. A fact that fails, even with a panic,
// is counted and logged, and the actor goes on: one bad fact must not end the
// recovery of every producer.
func (a *recoveryActor) handle(f fact) {
	a.counters.factsHandledTotal.Add(1)
	defer a.publishResumePoints(f)
	defer func() {
		if r := recover(); r != nil {
			a.failed(f, r)
		}
	}()
	if hook := a.beforeHandle.Load(); hook != nil {
		(*hook)(f)
	}
	m := a.machine
	switch f := f.(type) {
	case factOpened:
		m.sessionOpening(f.session, f.interest, f.at)
	case factClosed:
		m.sessionClosed(f.session)
	case factStart:
		for _, p := range a.catalog.producers() {
			// every producer starts down, whatever an earlier feed left in the producer manager
			if err := a.pm.SetProducerDown(p.ID(), true); err != nil {
				a.logger.WithError(err).WithField("producer_id", p.ID()).Error("recovery: set producer down at start")
			}
		}
		m.start()
		// the transport reports no connection of its own: a session's queue is
		// bound or lost, and each says so
		m.connectionUp()
	case *factAlives:
		a.takeAlives(f)
	case factProcessed:
		m.processed(f.session, f.producer, f.generatedAt, m.markOf(f.producer, f.generatedAt, f.taken), f.taken, f.requestID)
	case factSessionAlive:
		m.sessionAlive(f.session, f.producer, f.generatedAt, m.markOf(f.producer, f.generatedAt, f.taken), f.taken, f.subscribed, f.held)
	case factSnapshotComplete:
		m.snapshotComplete(f.session, f.producer, f.requestID)
	case factChannelLost:
		m.channelLost(f.session)
	case factChannelReopened:
		m.channelReopened(f.session)
	case factResetDone:
		m.resetDone(f.session, f.number, f.replaced)
	case factAnswered:
		m.answeredAt(f.requestID, f.err, f.at.mono)
	case factRewind:
		switch {
		case f.to.IsZero():
			// the zero time does nothing: what was set before stays the seed
		case !m.started:
			// before the feed starts the last start the client set is the seed
			a.catalog.seeds[f.producer] = f.to
		default:
			m.rewind(f.producer, f.to)
		}
	case *recoverEventCommand:
		a.recoverEvent(f)
	case factTick:
		m.processing(a.mgr.processingSince())
		m.tick()
		a.mgr.gcHandles(a.clock.Now().wall)
	}
}

// takeAlives hands the machine the alives of the fact.
func (a *recoveryActor) takeAlives(f *factAlives) {
	for _, alive := range f.slot.inOrder() {
		a.machine.alive(f.producer, alive.generatedAt, aliveMark(alive.generatedAt, alive.received), alive.received, alive.subscribed)
	}
}

// recoverEvent asks the machine for one event recovery and answers the caller
// with its handle: registered before the answer, so whatever the recovery ends
// by finds it. The answer for a request already in flight is that one's handle.
func (a *recoveryActor) recoverEvent(c *recoverEventCommand) {
	id, err := a.machine.recoverEvent(c.producerID, c.eventID, c.stateful)
	if err != nil {
		if errors.Is(err, errFeedClosing) {
			err = ErrManagerClosed
		}
		c.reply <- recoverEventReply{err: err}
		return
	}
	h, ok := a.mgr.LookupHandle(id)
	if !ok {
		h = NewHandle(id, c.producerID, c.eventID, a.clock.Now().wall)
		a.mgr.registerHandle(h)
	}
	c.reply <- recoverEventReply{handle: h}
}

// failed counts a fact the actor failed on.
func (a *recoveryActor) failed(f fact, why any) {
	a.counters.factsFailed.Add(1)
	l := a.logger.WithField("fact", fmt.Sprintf("%T", f))
	if suppressed, ok := a.failures.Allow(); ok {
		if suppressed > 0 {
			l = l.WithField("suppressed", suppressed)
		}
		l.Errorf("recovery: the actor failed on a fact and goes on with the next: %v", why)
		return
	}
	l.Debugf("recovery: the actor failed on a fact and goes on with the next: %v", why)
}

// publishResumePoints publishes the points the fact moved.
func (a *recoveryActor) publishResumePoints(f fact) {
	defer func() {
		if r := recover(); r != nil {
			a.failed(f, r)
		}
	}()
	a.machine.publishResumePoints()
}

// ---- the end

// finish is the actor's end, on its goroutine: the machine is told the feed is
// closing, the essential facts queued when the close came are handled and the
// resume points they move published (a session's close among them moves none
// forward; one of them can open a gap that takes a point back, and the client
// reads the point at shutdown to resume from), within finishWait and the
// close's deadline. What is left takes the points back to where it could have
// taken them at the furthest. The requests nobody has answered yet are refused,
// the machine is closed, and nothing posted after is taken.
func (a *recoveryActor) finish() {
	a.essentialMu.Lock()
	a.closed = true
	pending := a.essential
	a.essential = nil
	a.essentialMu.Unlock()
	a.closing.Store(true)

	a.machine.beginClosing()
	if left := a.finishEssential(pending); len(left) > 0 {
		a.counters.unhandledAtClose.Add(1)
		backTo := make(map[int]time.Time)
		earliest := func(producer int, at time.Time) {
			if cur, set := backTo[producer]; !set || at.Before(cur) {
				backTo[producer] = at
			}
		}
		for _, f := range left {
			switch f := f.(type) {
			case factRewind:
				// the start the client set is the one thing the machine cannot guess
				if !f.to.IsZero() {
					earliest(f.producer, f.to)
				}
			case *factAlives:
				for _, alive := range f.slot.inOrder() {
					earliest(f.producer, aliveMark(alive.generatedAt, alive.received))
				}
			}
		}
		a.guard(func() {
			a.machine.unhandledAtClose(backTo)
			a.machine.publishResumePoints()
		})
	}
	a.closeMachine()
}

// finishEssential handles the queued essential facts until the deadline; it
// returns the ones it did not get to.
func (a *recoveryActor) finishEssential(pending []fact) []fact {
	deadline := time.Now().Add(a.finishWait)
	if by := a.closeBy.Load(); by != nil && by.Before(deadline) {
		deadline = *by
	}
	for i, f := range pending {
		if !time.Now().Before(deadline) {
			return pending[i:]
		}
		a.handle(f)
		pending[i] = nil
	}
	return nil
}

// closeMachine ends the machine: whoever waits for an event recovery is refused,
// the event recoveries in flight have failed, and what is queued is dropped.
func (a *recoveryActor) closeMachine() {
	for len(a.commands) > 0 {
		(<-a.commands).reply <- recoverEventReply{err: ErrManagerClosed}
	}
	for len(a.samples) > 0 {
		<-a.samples
	}
	a.essentialMu.Lock()
	clear(a.openAlives)
	a.essentialMu.Unlock()
	a.guard(a.machine.close)
	for _, cancel := range a.snapshotCancels {
		cancel()
	}
	for _, cancel := range a.eventCancels {
		cancel()
	}
}

// guard runs fn, counting a panic as a failed fact instead of ending the actor.
func (a *recoveryActor) guard(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			a.failed(nil, r)
		}
	}()
	fn()
}

// stopBounded signals shutdown and joins the actor's goroutine and its REST
// requests, BOUNDED by ctx; its deadline is also the close's: the facts queued are
// handled until then at most. Reports whether everything actually ended; on false
// the stragglers leak harmlessly until what they wait on unwinds, since the
// manager's lifetime context was cancelled before stop was called.
func (a *recoveryActor) stopBounded(ctx context.Context) bool {
	if deadline, ok := ctx.Deadline(); ok {
		a.closeBy.CompareAndSwap(nil, &deadline)
	}
	a.signalStop()
	select {
	case <-a.done:
	case <-ctx.Done():
		// Completion-first accounting: with both arms ready the select picks
		// uniformly, so an exited actor could be reported as a straggler.
		select {
		case <-a.done:
		default:
			return false
		}
	}
	// Join the REST requests' goroutines too (bounded). The API's in-flight slot
	// is released when the response body closes, before the goroutine's tail, so
	// without this join Close could return while one still runs. Adds happen only
	// on the actor goroutine, which has exited by this point, so this Wait cannot race an Add.
	detachedDone := make(chan struct{})
	go func() {
		a.detached.Wait()
		close(detachedDone)
	}()
	select {
	case <-detachedDone:
		return true
	case <-ctx.Done():
		select {
		case <-detachedDone:
			return true
		default:
			return false
		}
	}
}
