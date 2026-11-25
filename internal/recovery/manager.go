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
	"github.com/oddin-gg/gosdk/internal/sdkerr"
	"github.com/oddin-gg/gosdk/types"
)

// Manager lifecycle states. Stored atomically so observers don't need
// a lock just to gate state-dependent operations.
//
// The Opening state separates "Open started" from "Open finished
// initialising" — without it, Open's CAS NotOpened → Open would
// publish state==Open before the per-Open lifecycleSession is stored,
// letting a concurrent dispatchRecoverEvent observe the gate as Open
// and reach into a nil session pointer.
const (
	mgrStateNotOpened int32 = 0
	mgrStateOpening   int32 = 1
	mgrStateOpen      int32 = 2
	mgrStateClosed    int32 = 3
)

// ErrManagerClosed is returned by recovery-API entry points after the
// manager has been Closed. Callers (gosdk.Client) wrap and surface to
// users as gosdk.ErrAlreadyClosed-equivalent.
// ErrManagerClosed wraps the shared closed-client sentinel
// (sdkerr.ErrClosed == gosdk.ErrAlreadyClosed) so recovery errors —
// including failed-at-shutdown handle results — classify under the
// public sentinel via errors.Is.
var ErrManagerClosed = fmt.Errorf("recovery: manager closed: %w", sdkerr.ErrClosed)

// ErrManagerNotOpen is returned by recovery-API entry points before
// Open has run (e.g., Connect path was never hit).
var ErrManagerNotOpen = errors.New("recovery: manager not open")

// HandleGCGracePeriod is the default time a completed *Handle stays
// queryable via LookupHandle before being garbage-collected. NEXT.md
// §0 left this as a guess; 5 minutes is generous enough for late
// pollers without growing the map indefinitely.
const HandleGCGracePeriod = 5 * time.Minute

// lifecycleSession bundles every per-Open allocation into a single
// pointer-sized atomic value. Open builds locally, then publishes the
// pointer atomically; Close.Swap(nil) atomically takes ownership for
// teardown. Concurrent readers (emitRecoveryMessage) observe a coherent
// snapshot or nil, never a half-built struct.
type lifecycleSession struct {
	ctx       context.Context
	cancelCtx context.CancelFunc
	out       chan types.RecoveryMessage
	// actor runs the recovery state machine of every producer.
	actor *recoveryActor
	// releaseHook stops the producer manager telling the actor of a recovery
	// start the client sets.
	releaseHook func()
}

// startRelay carries the recovery starts the client sets to the actor, from before the
// actor exists: a start set while the manager opens is held, in order, and posted
// when the actor is attached.
type startRelay struct {
	mu      sync.Mutex
	actor   *recoveryActor
	pending []factRewind
}

// tell is the producer manager's hook.
func (r *startRelay) tell(id int, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := factRewind{producer: id, to: at}
	if r.actor == nil {
		r.pending = append(r.pending, f)
		return
	}
	r.actor.postEssential(f)
}

// attach hands the actor the starts held, and what comes later.
func (r *startRelay) attach(a *recoveryActor) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.actor = a
	for _, f := range r.pending {
		a.postEssential(f)
	}
	r.pending = nil
}

// Manager is the recovery component: the one recovery actor that runs every
// producer's recovery state machine, the handles of the event recoveries, and
// the stream of what the client hears. Its methods are thin: they turn what the
// feed reports into facts for the actor and return, never waiting for it.
//
// Lifecycle (state machine in `state`):
//
//	NotOpened → Opening → Open → Closed
//
// All per-Open allocations live behind `session` (atomic.Pointer):
// Open builds locally and publishes once; Close.Swap(nil) takes
// ownership for teardown. lifecycleMu serializes Open's publish step
// with Close's teardown so a Close that lands during Open's init
// either (a) finds nothing to tear down + Open's CAS-fail tears down
// what it allocated, or (b) finds the published session and tears it
// down. There is no half-published window observable to readers.
type Manager struct {
	cfg             config.Config
	producerManager *producer.Manager
	apiClient       *api.Client
	logger          *log.Logger
	sequence        *generator

	// initialSnapshotTime is the lookback window for the FIRST snapshot
	// recovery of a producer with no recovery start
	// (WithInitialSnapshotTime). Zero means "no window": request the
	// producer's full history.
	initialSnapshotTime time.Duration

	// state is the explicit lifecycle gate. Only Open transitions
	// notOpened → open; only Close transitions any → closed. Once
	// closed, no recover-event commands are accepted, and session
	// callbacks no-op.
	state atomic.Int32

	// session bundles ctx, cancelCtx, out and the actor. Atomic-pointer so
	// readers see a coherent snapshot or nil.
	session atomic.Pointer[lifecycleSession]

	// lifecycleMu serializes Open's publish step with Close's teardown
	// so the two never run concurrently. Readers don't take this
	// mutex — they Load the session pointer atomically.
	lifecycleMu sync.Mutex

	// handles tracks outstanding *Handle objects keyed by request id.
	// Inserted by registerHandle, transitioned to terminal by
	// completeHandle, GC'd by gcHandles.
	handlesMu sync.RWMutex
	handles   map[int]*Handle

	// processing holds, per session of the client, when the message it is
	// processing began: the slow-processing warning needs it, and the actor
	// reads which sessions have been in one for long.
	processingMu sync.Mutex
	processing   map[uuid.UUID]Stamp

	// emitMu serializes drop-oldest writes to sess.out so concurrent
	// writers never interleave their try-send / drain / push
	// triples. Without this, two senders could observe a full channel,
	// each drain one value, and each push — net effect: the drained
	// values include one that was just successfully pushed, silently
	// losing a fresh message instead of an old one.
	emitMu sync.Mutex

	// slowProcessingThrottle bounds the "processing message took more
	// than 1s" warn: a stalled consumer or a slow dependency makes every
	// message slow, which used to mean one line per message.
	slowProcessingThrottle *log.Throttle

	// afterSeedsRead, when set, runs in Open once the actor has read the seeds and
	// before it is attached to the starts the client sets. Tests only.
	afterSeedsRead func()

	// clk stamps the facts the manager itself takes; nil reads the system's
	// clock, and a test sets another.
	clk Clock
}

// slowProcessingWarnInterval bounds the slow-message-processing warn to
// one line per interval; the line carries the number of warns held back.
const slowProcessingWarnInterval = time.Minute

// NewManager constructs the recovery manager. Open(ctx) must be called
// before anything is reported to it. initialSnapshotTime is the lookback
// window for a first snapshot recovery with no recovery start (zero disables).
func NewManager(cfg config.Config, producerManager *producer.Manager, apiClient *api.Client, logger *log.Logger, initialSnapshotTime time.Duration) *Manager {
	return &Manager{
		cfg:                 cfg,
		producerManager:     producerManager,
		apiClient:           apiClient,
		logger:              logger,
		initialSnapshotTime: initialSnapshotTime,
		sequence:            newGenerator(1),
		handles:             make(map[int]*Handle),
		processing:          make(map[uuid.UUID]Stamp),

		slowProcessingThrottle: log.NewThrottle(slowProcessingWarnInterval, nil),
	}
}

// Open starts the recovery actor over the active producers and has it start the
// feed. Returns the recovery-events channel.
//
// ctx bounds the bootstrap (active-producer fetch) and is also used
// to derive the actor-lifetime context internally via WithoutCancel
// — the actor outlives the caller's Open ctx so a Subscribe
// timeout doesn't tear down the recovery state machine that's
// supposed to live for the Client's lifetime.
//
// Open is one-shot: once a manager has been Open'd (success or failure)
// and Close'd, the same instance can't be reopened. The gosdk.Client
// constructs a fresh recovery.Manager on retry — see
// Client.resetConnectionLayer.
func (m *Manager) Open(ctx context.Context) (<-chan types.RecoveryMessage, error) {
	// CAS NotOpened → Opening. Concurrent observers see "Opening" until
	// init completes; only after the second CAS to Open do recover-event
	// entry points and session callbacks accept work.
	if !m.state.CompareAndSwap(mgrStateNotOpened, mgrStateOpening) {
		switch m.state.Load() {
		case mgrStateOpening, mgrStateOpen:
			return nil, errors.New("recovery: already opened")
		case mgrStateClosed:
			return nil, ErrManagerClosed
		}
	}

	// On any error path during init, revert Opening → NotOpened so a
	// retry-friendly caller can try again. (resetConnectionLayer
	// already constructs a fresh Manager, but this keeps the state
	// machine self-consistent if Open is called twice on the same
	// instance — a test harness, for example.)
	settled := false
	defer func() {
		if !settled {
			m.state.CompareAndSwap(mgrStateOpening, mgrStateNotOpened)
		}
	}()

	// Two ctxs in play:
	//
	//   - ctx: the caller's, used to bound the ActiveProducers HTTP fetch
	//     below. Honors the user's Subscribe timeout.
	//   - mgrCtx: derived from WithoutCancel(ctx) so the actor lives past
	//     the caller's Open call. Cancelled by Manager.Close (via cleanup →
	//     sess.cancelCtx).
	mgrCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))

	activeProducers, err := m.producerManager.ActiveProducers(ctx)
	if err != nil {
		cancel()
		return nil, err
	}
	if len(activeProducers) == 0 {
		m.logger.Warn("no active producers")
	}
	producers := make([]types.Producer, 0, len(activeProducers))
	for _, p := range activeProducers {
		producers = append(producers, p)
	}

	out := make(chan types.RecoveryMessage, 1024)
	// A start the client sets from here on reaches the actor: it is told before the
	// seeds are read, held until the actor exists, and handled before the feed starts,
	// where the last of them is the seed.
	relay := &startRelay{}
	release := m.producerManager.SetRecoveryFromHook(relay.tell)
	actor := newRecoveryActor(mgrCtx, m.cfg, m.apiClient, m.producerManager, m, m.logger, m.clk, producers, m.initialSnapshotTime)
	if m.afterSeedsRead != nil {
		m.afterSeedsRead()
	}
	relay.attach(actor)
	sess := &lifecycleSession{
		ctx:         mgrCtx,
		cancelCtx:   cancel,
		out:         out,
		actor:       actor,
		releaseHook: release,
	}

	// Publish under lifecycleMu so Close's teardown either runs FIRST
	// (sees nil session, our CAS fails, we tear down locals) or runs
	// SECOND (sees the published session, tears it down). No
	// half-published window.
	m.lifecycleMu.Lock()
	if m.state.Load() == mgrStateClosed {
		// Close won the race. Release mu before tearing down locals.
		m.lifecycleMu.Unlock()
		release()
		cancel()
		close(out)
		// the actor has not started (no goroutine spawned): just discard it
		return nil, ErrManagerClosed
	}

	// Publish session pointer atomically; concurrent Loaders see the
	// fully-initialised struct or nil.
	m.session.Store(sess)
	go actor.run()
	actor.start()

	// Final CAS Opening → Open under mu. Guaranteed to succeed since
	// state==Opening when we entered the critical section and Close
	// can only transition state under the same mu (Close stores
	// Closed before taking mu, but here we already verified state
	// wasn't Closed under mu, and Close can't change state again
	// while we hold mu).
	if !m.state.CompareAndSwap(mgrStateOpening, mgrStateOpen) {
		// Defensive — shouldn't be reachable under mu. Tear down via
		// the same cleanup path Close would use. WithoutCancel: the
		// teardown must run even if the caller's ctx already fired
		// (unbounded is fine — the actor was started moments ago
		// and cannot be wedged yet).
		m.cleanup(context.WithoutCancel(ctx))
		m.lifecycleMu.Unlock()
		return nil, ErrManagerClosed
	}
	settled = true
	m.lifecycleMu.Unlock()
	return out, nil
}

// Close terminates the actor. Idempotent. Pending handles are failed so callers
// blocked on Done() unblock.
//
// Race ordering:
//   - Close marks state==Closed first so late session callbacks
//     observe Closed and no-op.
//   - Cleanup runs under lifecycleMu so a direct Open/Close race —
//     Close runs while Open is still allocating — resolves cleanly:
//     Close's cleanup sees an empty manager, then
//     Open's CAS-Opening-to-Open fails and the same cleanup helper
//     tears down the resources Open just allocated.
func (m *Manager) Close() {
	m.CloseCtx(context.Background())
}

// CloseCtx is Close with the actor join BOUNDED by ctx, so a wedged
// actor (stuck handler) can't pin the caller's shutdown budget. Reports
// whether the actor actually exited; on false it leaks
// harmlessly — its lifetime ctx and shutdown channel were already
// signalled, so it exits as soon as whatever it's blocked on
// unwinds. The deadline of ctx is also the actor's own for what it still
// handles as it closes: the resume points are final when this returns.
func (m *Manager) CloseCtx(ctx context.Context) bool {
	// Mark Closed *before* tearing down: late callbacks
	// (session shutdown racing with Close) observe state==Closed and no-op.
	m.state.Store(mgrStateClosed)
	m.lifecycleMu.Lock()
	complete := m.cleanup(ctx)
	m.lifecycleMu.Unlock()
	return complete
}

// cleanup tears down whatever resources the manager currently owns.
// Atomically *takes* the session pointer (Swap(nil)) so a subsequent
// cleanup call sees nil and no-ops. Resources Open allocated AFTER
// the previous cleanup ran are still cleaned up by the second call —
// because the second call sees the new session pointer Open published.
//
// The feed is told it is closing before anything else closes: the actor handles
// the facts it holds with the resume points only going back, then closes the
// machine. A session that closes after this is no fact of the actor's, so the
// point a client reads once Close returns does not depend on the order the
// sessions close in.
//
// MUST be called with lifecycleMu held. The actor join is bounded by ctx; the
// return reports whether the actor actually exited.
func (m *Manager) cleanup(ctx context.Context) bool {
	complete := true
	if sess := m.session.Swap(nil); sess != nil {
		if sess.releaseHook != nil {
			sess.releaseHook()
		}
		// cancelled before the actor is told to stop: its requests abort at once
		if sess.cancelCtx != nil {
			sess.cancelCtx()
		}
		// out is closed last, after the actor has stopped (below).
		// The close runs under emitMu, paired with emitRecoveryMessage's
		// session re-check under the same lock: an emitter mid-push
		// delays the close until it finishes, and an emitter arriving
		// after the Swap(nil) above bails on the re-check. The actor is
		// the sole writer to sess.out (via emitRecoveryMessage).
		defer func() {
			if sess.out != nil {
				m.emitMu.Lock()
				close(sess.out)
				m.emitMu.Unlock()
			}
		}()
		if sess.actor != nil && !sess.actor.stopBounded(ctx) {
			complete = false
		}
	}

	// Unblock any pending handle waiters. failPendingHandles is
	// idempotent — it walks m.handles and only acts on non-terminal
	// entries; after the first sweep handles are terminal so a second
	// sweep is a fast no-op.
	// ErrManagerClosed (not an ad-hoc errors.New): handle.Result().Err
	// must classify via errors.Is against gosdk.ErrAlreadyClosed — the
	// sentinel wraps the shared closed-client error.
	m.failPendingHandles(ErrManagerClosed)
	return complete
}

// actor is the running actor, nil before Open and once the manager has closed.
func (m *Manager) actor() *recoveryActor {
	if m.state.Load() != mgrStateOpen {
		return nil
	}
	if sess := m.session.Load(); sess != nil {
		return sess.actor
	}
	return nil
}

// ProducerStatus returns the most recent producer-status snapshot
// the actor published, or (nil, false) if it has published none for the
// id. The snapshot survives lossy-channel drops on RecoveryEvents.
func (m *Manager) ProducerStatus(producerID int) (types.ProducerStatus, bool) {
	sess := m.session.Load()
	if sess == nil || sess.actor == nil {
		return nil, false
	}
	if s := sess.actor.currentStatus(producerID); s != nil {
		return s, true
	}
	return nil, false
}

// --- Inbound feed events (called from session.go on the AMQP path) ---

// OnMessageProcessingStarted records when the session began a message of the
// client and moves the producer's last-message timestamp. Called for the
// sessions of the client only: the SDK's own alive-only session is not the
// client's processing and reports nothing. No-op when the manager is not open
// (callbacks may fire from sessions racing with shutdown).
func (m *Manager) OnMessageProcessingStarted(sessionID uuid.UUID, producerID int, taken Stamp) {
	m.processingMu.Lock()
	m.processing[sessionID] = taken
	m.processingMu.Unlock()

	if m.actor() == nil {
		return
	}
	if taken.Wall().IsZero() {
		m.logger.WithField("producer_id", producerID).Warn("recovery: processing started with zero timestamp")
		return
	}
	if err := m.producerManager.SetProducerLastMessageTimestamp(producerID, taken.Wall()); err != nil {
		m.logger.WithError(err).WithField("producer_id", producerID).Error("recovery: set producer last message timestamp")
	}
}

// OnMessageProcessingEnded warns on >1s processing and moves the producer's
// last-processed timestamp to the message's gen timestamp; a zero timestamp (a
// message whose build or send failed) moves nothing, but the message is out of
// processing all the same.
func (m *Manager) OnMessageProcessingEnded(sessionID uuid.UUID, producerID int, timestamp time.Time) {
	m.processingMu.Lock()
	started, ok := m.processing[sessionID]
	delete(m.processing, sessionID)
	m.processingMu.Unlock()

	if m.actor() != nil && !timestamp.IsZero() {
		if err := m.producerManager.SetLastProcessedMessageGenTimestamp(producerID, timestamp); err != nil {
			m.logger.WithError(err).WithField("producer_id", producerID).Error("recovery: set last processed gen timestamp")
		}
	}

	elapsed := m.clock().Now().Mono() - started.Mono()
	switch {
	case !ok:
		m.logger.Warn("message processing ended, but was not started")
	case elapsed.Milliseconds() > 1000:
		if suppressed, ok := m.slowProcessingThrottle.Allow(); ok {
			l := m.logger
			if suppressed > 0 {
				l = l.WithField("suppressed", suppressed)
			}
			l.Warnf("processing message took more than 1s - %d ms", elapsed.Milliseconds())
		}
	}
}

// processingSince is the sessions that have a message of the client in
// processing, and since when on the monotonic clock.
func (m *Manager) processingSince() map[uuid.UUID]time.Duration {
	m.processingMu.Lock()
	defer m.processingMu.Unlock()
	out := make(map[uuid.UUID]time.Duration, len(m.processing))
	for id, taken := range m.processing {
		out[id] = taken.Mono()
	}
	return out
}

// OnClientSessionOpened registers a session of the client with its interest,
// before its consumer starts and so before its first delivery. openedAt is a
// reading of the clock taken before the session's queue is bound: no message in
// the queue is older than it. The session awaits its queue until
// OnFeedChannelRestored says it is bound; nothing is asked for its producers
// before.
//
// A session with the alive-only interest is never registered: it has no
// lane, receives no replay and takes no snapshot_complete, so it holds no
// recovery and loses nothing. That is the SDK's own alive session and a
// client's alive-only subscription alike.
func (m *Manager) OnClientSessionOpened(session uuid.UUID, messageInterest types.MessageInterest, openedAt Stamp) {
	if !hasLane(messageInterest) {
		return
	}
	if a := m.actor(); a != nil {
		a.postEssential(factOpened{session: session, interest: messageInterest, at: openedAt})
	}
}

// OnFeedChannelLost reports that a session's consumer channel, and with it its
// exclusive, auto-delete queue, was lost, whether with the whole AMQP
// connection or alone on a channel-level exception. Everything published until
// the consumer re-binds is gone from the broker: the producers the session
// receives are down until a recovery brings it back. Runs on the consumer's
// goroutine between losing the queue and re-binding it, so it only posts. The
// SDK's own alive channel is no session of the actor's: losing it alone opens no
// gap.
func (m *Manager) OnFeedChannelLost(session uuid.UUID) {
	if a := m.actor(); a != nil {
		a.postEssential(factChannelLost{session: session})
	}
}

// OnFeedChannelRestored reports that a session's queue is bound: after its
// first open, and after a loss, when its consumer has re-declared and re-bound
// it. What the session's producers miss is asked for from now on.
func (m *Manager) OnFeedChannelRestored(session uuid.UUID) {
	if a := m.actor(); a != nil {
		a.postEssential(factChannelReopened{session: session})
	}
}

// OnFeedSessionGone reports that a session's consumer exited for good, or never
// opened. It needs no queue, so it must not hold recoveries back.
func (m *Manager) OnFeedSessionGone(session uuid.UUID) {
	if a := m.actor(); a != nil {
		a.postEssential(factClosed{session: session})
	}
}

// OnMessageProcessed reports that the session has finished with a message of the
// producer: the client holds it and its ack has run (an ack that failed
// is logged, and the fact is posted all the same). It is a sample: posted when
// the delivery is settled, not when the session has merely read it, and dropped
// when the actor's sample queue is full.
//
// generatedAt is the message's timestamp by the producer's clock, taken is
// when the session began processing it, and requestID the recovery it
// belongs to (0 for a live message).
func (m *Manager) OnMessageProcessed(session uuid.UUID, producerID int, generatedAt time.Time, taken Stamp, requestID int) {
	if a := m.actor(); a != nil {
		a.postSample(factProcessed{session: session, producer: producerID, generatedAt: generatedAt, taken: taken, requestID: requestID})
	}
}

// OnSessionAlive reports an alive of the producer that a session of the client
// has finished, with the instant it was taken: the sign the client side is
// getting messages. The alive-only session reports its alives through
// OnAliveReceived, which is the alive channel of the SDK and bound to no
// session. A sample, like OnMessageProcessed.
func (m *Manager) OnSessionAlive(session uuid.UUID, producerID int, generatedAt time.Time, taken Stamp, isSubscribed bool) {
	if a := m.actor(); a != nil {
		a.postSample(factSessionAlive{session: session, producer: producerID, generatedAt: generatedAt, taken: taken, subscribed: isSubscribed})
	}
}

// OnAliveReceived reports an alive of the SDK's own alive channel: the producer
// is there. It joins the fact its producer's earlier alives wait in while no other
// essential fact was posted after it, which the actor takes once, so a full
// queue can only delay an alive, never drop it. received is when the session
// took it; the producer's last-message timestamp moves to the one the feed
// stamped on it.
func (m *Manager) OnAliveReceived(producerID int, timestamp types.MessageTimestamp, received Stamp, isSubscribed bool) {
	a := m.actor()
	if a == nil {
		return
	}
	if enabled, err := m.producerManager.IsProducerEnabled(a.ctx, producerID); err == nil && enabled {
		if err := m.producerManager.SetProducerLastMessageTimestamp(producerID, timestamp.Received); err != nil {
			m.logger.WithError(err).WithField("producer_id", producerID).Error("recovery: set producer last message timestamp")
		}
	}
	a.onAlive(producerID, timestamp.Created, received, isSubscribed)
}

// OnSnapshotCompleteReceived admits a snapshot_complete a session took to the
// actor. It is a correctness event, never dropped: it returns nil once it is in
// (or when it is of a request this feed never asked for, which completes nothing),
// and the delivery may be acked; a non-nil error says the manager is closed, and
// the caller must leave the delivery unacked for the broker to redeliver.
func (m *Manager) OnSnapshotCompleteReceived(_ context.Context, session uuid.UUID, producerID int, requestID int) error {
	a := m.actor()
	if a == nil {
		return ErrManagerClosed
	}
	return a.onSnapshotComplete(session, producerID, requestID)
}

// --- Synchronous commands ---

// InitiateEventOddsRecoveryHandle is the handle-returning variant.
// Sends a recoverEvent command to the actor and waits for the reply.
func (m *Manager) InitiateEventOddsRecoveryHandle(ctx context.Context, producerID int, eventID types.URN) (*Handle, error) {
	return m.dispatchRecoverEvent(ctx, producerID, eventID, false)
}

// InitiateEventStatefulRecoveryHandle is the handle-returning variant.
func (m *Manager) InitiateEventStatefulRecoveryHandle(ctx context.Context, producerID int, eventID types.URN) (*Handle, error) {
	return m.dispatchRecoverEvent(ctx, producerID, eventID, true)
}

func (m *Manager) dispatchRecoverEvent(ctx context.Context, producerID int, eventID types.URN, stateful bool) (*Handle, error) {
	switch m.state.Load() {
	case mgrStateNotOpened, mgrStateOpening:
		// Opening is "not yet ready": return ErrManagerNotOpen
		// consistently so callers retry rather than fail terminally.
		return nil, ErrManagerNotOpen
	case mgrStateClosed:
		return nil, ErrManagerClosed
	}
	// Validate catalog membership BEFORE queueing: an invalid producer id must
	// not cost the actor a command.
	if _, err := m.producerManager.GetProducer(ctx, producerID); err != nil {
		return nil, fmt.Errorf("recovery: validate producer %d: %w", producerID, err)
	}
	a := m.actor()
	if a == nil {
		// Manager raced into Closed between the state check and the actor lookup.
		return nil, ErrManagerClosed
	}
	cmd := &recoverEventCommand{
		producerID: producerID,
		eventID:    eventID,
		stateful:   stateful,
		reply:      make(chan recoverEventReply, 1),
	}
	// Admission: a nil error is acceptance, and the wait below resolves whether the
	// command ran (a handle) or was refused by the close (ErrManagerClosed,
	// truthfully nothing was issued).
	if err := a.postCommand(ctx, cmd); err != nil {
		return nil, err
	}
	// Admission succeeded: from here the reply is GUARANTEED and prompt: the
	// actor answers a command synchronously, before the REST request goes out,
	// and its close answers the ones it did not run. Deliberately NO ctx.Done()
	// arm here: the caller's ctx bounds ADMISSION only. Honoring it in this wait
	// would make "nil handle + ctx.Err()" a lie, since the actor registers the
	// recovery and starts the request anyway, and a caller retrying after the
	// cancellation would start a DUPLICATE recovery, contradicting the documented
	// "error means the recovery was not accepted" contract.
	select {
	case r := <-cmd.reply:
		return r.handle, r.err
	case <-a.done:
		// The actor exited. Its close answers every command it took, so the
		// reply is normally already buffered: drain it non-blocking.
		select {
		case r := <-cmd.reply:
			return r.handle, r.err
		default:
			return nil, ErrManagerClosed
		}
	}
}

// --- Handle registry (called by the actor via actorManagerOps) ---

// LookupHandle returns the tracked handle for a request id. The second
// return value is false when the id is unknown (never registered) or
// has been GC'd after the grace period.
func (m *Manager) LookupHandle(requestID int) (*Handle, bool) {
	m.handlesMu.RLock()
	defer m.handlesMu.RUnlock()
	h, ok := m.handles[requestID]
	return h, ok
}

func (m *Manager) registerHandle(h *Handle) {
	m.handlesMu.Lock()
	if m.handles == nil {
		m.handles = make(map[int]*Handle)
	}
	m.handles[h.requestID] = h
	m.handlesMu.Unlock()
}

func (m *Manager) completeHandle(requestID int, status types.RecoveryRequestStatus, err error) *Handle {
	m.handlesMu.RLock()
	h := m.handles[requestID]
	m.handlesMu.RUnlock()
	if h == nil {
		return nil
	}
	h.complete(status, err, m.clock().Now().Wall())
	return h
}

func (m *Manager) gcHandles(now time.Time) {
	m.handlesMu.Lock()
	defer m.handlesMu.Unlock()
	for id, h := range m.handles {
		if !h.IsTerminal() {
			continue
		}
		if now.Sub(h.endedAt) > HandleGCGracePeriod {
			delete(m.handles, id)
		}
	}
}

func (m *Manager) failPendingHandles(err error) {
	m.handlesMu.RLock()
	pending := make([]*Handle, 0, len(m.handles))
	for _, h := range m.handles {
		if !h.IsTerminal() {
			pending = append(pending, h)
		}
	}
	m.handlesMu.RUnlock()
	for _, h := range pending {
		h.complete(types.RecoveryStatusFailed, err, m.clock().Now().Wall())
	}
}

// --- what the actor uses of the manager ---

// nextRequestID is the actorManagerOps method backed by the shared
// generator. Generator is internally locked.
func (m *Manager) nextRequestID() int { return m.sequence.next() }

// emitRecoveryMessage is the actorManagerOps method to publish a
// RecoveryMessage on the public stream.
//
// Drop-oldest semantics, matching the public event-channel policy
// (NEXT.md §0.3): on full, drop the oldest queued message and push
// the new one. Blocking on a slow consumer would stall the actor and
// risk shutdown deadlocks (the actor still holding a value while
// runShutdown waits on its done channel).
//
// Snapshots the session pointer atomically so a concurrent Close that
// runs Swap(nil) doesn't race with the channel sends — we either get
// a coherent (still-open) channel or nil (close already took it).
//
// emitMu serializes the try-send / drain / push triple so concurrent
// actor goroutines can't interleave drains and pushes. Critical
// section is microseconds (three non-blocking selects).
func (m *Manager) emitRecoveryMessage(msg types.RecoveryMessage) {
	if m.state.Load() != mgrStateOpen {
		return
	}
	sess := m.session.Load()
	if sess == nil || sess.out == nil {
		return
	}
	out := sess.out

	m.emitMu.Lock()
	defer m.emitMu.Unlock()

	// Re-check under emitMu: cleanup() Swap(nil)s the session BEFORE it
	// closes sess.out (also under emitMu). An emitter that passed the
	// state/session gates above but acquired emitMu after the swap must
	// bail here — otherwise it could push onto a channel the deferred
	// close is about to (or already did) close. Together with the
	// emitMu-guarded close, this makes "no send on closed out" a
	// mechanical invariant instead of an actor-stopped-first ordering
	// convention.
	if m.session.Load() != sess {
		return
	}

	select {
	case out <- msg:
		return
	default:
	}
	// Channel full — drain one to make room, then push.
	select {
	case <-out:
	default:
	}
	select {
	case out <- msg:
	default:
	}
}

// eventRecoveryMessageImpl satisfies types.EventRecoveryMessage —
// the per-event recovery completion event delivered on the recovery
// stream.
type eventRecoveryMessageImpl struct {
	eventID   types.URN
	requestID int
	producer  types.Producer
	timestamp types.MessageTimestamp
}

func (e eventRecoveryMessageImpl) Producer() types.Producer          { return e.producer }
func (e eventRecoveryMessageImpl) Timestamp() types.MessageTimestamp { return e.timestamp }
func (e eventRecoveryMessageImpl) EventID() types.URN                { return e.eventID }
func (e eventRecoveryMessageImpl) RequestID() int                    { return e.requestID }
