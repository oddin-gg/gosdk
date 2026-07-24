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
// the actorsMu RWMutex just to gate state-dependent operations.
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

const (
	initialDelay = 60 * time.Second
	tickPeriod   = 10 * time.Second
)

// HandleGCGracePeriod is the default time a completed *Handle stays
// queryable via LookupHandle before being garbage-collected. NEXT.md
// §0 left this as a guess; 5 minutes is generous enough for late
// pollers without growing the map indefinitely.
const HandleGCGracePeriod = 5 * time.Minute

// lifecycleSession bundles every per-Open allocation into a single
// pointer-sized atomic value. Open builds locally, then publishes the
// pointer atomically; Close.Swap(nil) atomically takes ownership for
// teardown. Concurrent readers (findOrSpawn, emitRecoveryMessage)
// observe a coherent snapshot or nil — never a half-built struct.
type lifecycleSession struct {
	ctx       context.Context
	cancelCtx context.CancelFunc
	out       chan types.RecoveryMessage
	closeCh   chan struct{}
	// tickDone closes when runTickLoop exits; cleanup joins on it
	// (ctx-bounded) so Close's nil return means the ticker goroutine
	// has actually exited, not merely been signalled.
	tickDone chan struct{}
}

// Manager is the dispatcher that owns the per-producer actor goroutines
// and the cross-actor singletons (output channel, request id generator,
// handles registry).
//
// Phase 5 v2 actor model (NEXT.md §11): per-producer state lives inside
// recoveryActor goroutines. Manager methods are thin — they look up
// the actor and push events to its inbox. The previous mutex-guarded
// producerRecoveryData + central manager-locks are gone.
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
	// recovery of a producer with no recovery cursor
	// (WithInitialSnapshotTime; Java/.NET parity). Zero means "no
	// window" — request the producer's full history. Passed as a
	// constructor arg rather than via config.Config, following the
	// api-timeout / feed-prefetch wiring precedent.
	initialSnapshotTime time.Duration

	// state is the explicit lifecycle gate. Only Open transitions
	// notOpened → open; only Close transitions any → closed. Once
	// closed, no actors are spawned, no recover-event commands are
	// accepted, and session callbacks no-op.
	state atomic.Int32

	// session bundles ctx, cancelCtx, out, closeCh. Atomic-pointer so
	// readers see a coherent snapshot or nil. Tickers are LOCAL to
	// runTickLoop (the goroutine spawned at Open publish) — no
	// manager field for them.
	session atomic.Pointer[lifecycleSession]

	// lifecycleMu serializes Open's publish step with Close's teardown
	// so the two never run concurrently. Readers don't take this
	// mutex — they Load the session pointer atomically.
	lifecycleMu sync.Mutex

	// actors holds one recoveryActor per known producer. Populated at
	// Open from ActiveProducers and lazily on first message for
	// previously-unknown producers.
	actorsMu sync.RWMutex
	actors   map[int]*recoveryActor

	// handles tracks outstanding *Handle objects keyed by request id.
	// Inserted by registerHandle, transitioned to terminal by
	// completeHandle, GC'd by gcCompletedHandles.
	handlesMu sync.RWMutex
	handles   map[int]*Handle

	// messageProcessingTimes maps sessionID → processing start time so
	// OnMessageProcessingEnded can warn on >1s processing.
	processingMu    sync.Mutex
	processingTimes map[uuid.UUID]time.Time

	// emitMu serializes drop-oldest writes to sess.out so concurrent
	// actor goroutines never interleave their try-send / drain / push
	// triples. Without this, two senders could observe a full channel,
	// each drain one value, and each push — net effect: the drained
	// values include one that was just successfully pushed, silently
	// losing a fresh message instead of an old one.
	emitMu sync.Mutex

	// tickDrops counts evTick events dropped because the actor's inbox
	// was full. Pre-v2.x this was silent — under sustained recovery
	// load the inactivity check could blind itself. The counter is
	// monotonic across the manager's lifetime; the warn log is rate-
	// limited per-producer to one entry per tickDropWarnInterval.
	tickDrops          atomic.Uint64
	tickDropWarnMu     sync.Mutex
	tickDropLastWarnAt map[int]time.Time

	// inboxDrops counts residual lossy inbox drops. NOTE the correctness
	// inputs no longer flow through the lossy path at all:
	// snapshot_complete uses reliable ctx-bounded admission (sendCtx),
	// alive uses the coalesced latest-alive mailbox, and processing
	// start/end call the thread-safe producer setters directly — so this
	// counter covers only best-effort notifications.
	inboxDrops atomic.Uint64
}

// tickDropWarnInterval bounds how often per-producer tick-drop warns are
// emitted. The tick period is 10s, so 60s allows up to 6 silent drops
// per producer before a fresh warn — enough to suppress one-off blips
// while still surfacing sustained backpressure within a minute.
const tickDropWarnInterval = 60 * time.Second

// NewManager constructs the recovery manager. Open(ctx) must be called
// before any actors are spawned. initialSnapshotTime is the lookback
// window for cursor-less first snapshot recoveries (zero disables).
func NewManager(cfg config.Config, producerManager *producer.Manager, apiClient *api.Client, logger *log.Logger, initialSnapshotTime time.Duration) *Manager {
	return &Manager{
		cfg:                 cfg,
		producerManager:     producerManager,
		apiClient:           apiClient,
		logger:              logger,
		initialSnapshotTime: initialSnapshotTime,
		sequence:            newGenerator(1),
		actors:              make(map[int]*recoveryActor),
		handles:             make(map[int]*Handle),
		processingTimes:     make(map[uuid.UUID]time.Time),
		tickDropLastWarnAt:  make(map[int]time.Time),
	}
}

// recordTickDrop is invoked by the tick fan-out loop when an actor's
// inbox is full. Bumps the monotonic counter and emits a per-producer
// rate-limited warn so operators can see sustained drops without
// log-spam from transient blips.
func (m *Manager) recordTickDrop(producerID int) {
	m.tickDrops.Add(1)
	now := time.Now()
	m.tickDropWarnMu.Lock()
	last, ok := m.tickDropLastWarnAt[producerID]
	emit := !ok || now.Sub(last) >= tickDropWarnInterval
	if emit {
		m.tickDropLastWarnAt[producerID] = now
	}
	m.tickDropWarnMu.Unlock()
	if emit && m.logger != nil {
		m.logger.WithField("producer_id", producerID).
			WithField("total_dropped", m.tickDrops.Load()).
			Warn("recovery: actor inbox full; tick dropped — inactivity check may be delayed")
	}
}

// TickDropCount returns the cumulative number of evTick events dropped
// because actor inboxes were full. Exposed for diagnostics / tests.
func (m *Manager) TickDropCount() uint64 {
	return m.tickDrops.Load()
}

// InboxDropCount returns the cumulative number of non-tick feed events
// dropped because actor inboxes were full. Retained for diagnostics /
// tests; since the correctness inputs left the lossy path
// (snapshot_complete → reliable admission, alive → coalesced mailbox,
// processing start/end → direct thread-safe setters) nothing increments
// it in production and it should stay at zero.
func (m *Manager) InboxDropCount() uint64 {
	return m.inboxDrops.Load()
}

// Open spawns one actor per active producer and starts the periodic
// inactivity tick. Returns the recovery-events channel.
//
// ctx bounds the bootstrap (active-producer fetch) and is also used
// to derive the actor-lifetime context internally via WithoutCancel
// — actor goroutines outlive the caller's Open ctx so a Subscribe
// timeout doesn't tear down the recovery state machine that's
// supposed to live for the Client's lifetime.
//
// Pre-fix the caller (ensureNormal) passed a WithoutCancel-derived
// ctx for both purposes, so the bootstrap producer-fetch never
// honored the user's Subscribe timeout. Now the user's ctx flows
// through; the lifecycle ctx is derived here.
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
	//   - bootCtx (== ctx): the caller's ctx, used to bound the
	//     ActiveProducers HTTP fetch below. Honors the user's
	//     Subscribe timeout.
	//   - mgrCtx: derived from WithoutCancel(ctx) so the actor
	//     lifetimes outlive the caller's Open call. Cancelled by
	//     Manager.Close (via cleanup → sess.cancelCtx).
	//
	// Pre-fix both purposes used the caller-provided ctx, but the
	// caller (ensureNormal) had already WithoutCancel'd it — making
	// the bootstrap fetch effectively un-cancellable from the user's
	// Subscribe ctx. The split here lets the user's timeout bound
	// only the boot work while the actors get a properly
	// caller-severed lifecycle ctx.
	mgrCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))

	activeProducers, err := m.producerManager.ActiveProducers(ctx)
	if err != nil {
		cancel()
		return nil, err
	}
	if len(activeProducers) == 0 {
		m.logger.Warn("no active producers")
	}

	out := make(chan types.RecoveryMessage, 1024)
	closeCh := make(chan struct{})
	tickDone := make(chan struct{})
	sess := &lifecycleSession{
		ctx:       mgrCtx,
		cancelCtx: cancel,
		out:       out,
		closeCh:   closeCh,
		tickDone:  tickDone,
	}

	// Build the actor map locally. We don't insert into m.actors yet —
	// a concurrent findOrSpawn would observe state==Opening and bail.
	localActors := make(map[int]*recoveryActor, len(activeProducers))
	for id := range activeProducers {
		a := newRecoveryActor(mgrCtx, id, m.cfg, m.apiClient, m.producerManager, m, m.logger, 256, m.initialSnapshotTime)
		localActors[id] = a
	}

	// Publish under lifecycleMu so Close's teardown either runs FIRST
	// (sees nil session, our CAS fails, we tear down locals) or runs
	// SECOND (sees the published session, tears it down). No
	// half-published window.
	m.lifecycleMu.Lock()
	if m.state.Load() == mgrStateClosed {
		// Close won the race. Release mu before tearing down locals.
		m.lifecycleMu.Unlock()
		cancel()
		close(closeCh)
		close(out)
		// localActors haven't started yet (no goroutines spawned) —
		// just discard the map.
		return nil, ErrManagerClosed
	}

	// Publish session pointer atomically; concurrent Loaders see the
	// fully-initialised struct or nil.
	m.session.Store(sess)
	// Move actors into the manager-visible map.
	m.actorsMu.Lock()
	for id, a := range localActors {
		m.actors[id] = a
	}
	m.actorsMu.Unlock()
	for _, a := range localActors {
		go a.run() //nolint:contextcheck // actor uses lifetime ctx (a.ctx) wired in newRecoveryActor; per-request ctx flows through evRecoverEvent.ctx and is composed with a.ctx via context.AfterFunc inside dispatch.
	}
	go func() {
		defer close(tickDone)
		m.runTickLoop(closeCh)
	}()

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
		// (unbounded is fine — the actors were allocated moments ago
		// and cannot be wedged yet).
		m.cleanup(context.WithoutCancel(ctx))
		m.lifecycleMu.Unlock()
		return nil, ErrManagerClosed
	}
	settled = true
	m.lifecycleMu.Unlock()
	return out, nil
}

// Close terminates all actors and the tick loop. Idempotent. Pending
// handles are failed so callers blocked on Done() unblock.
//
// Race ordering:
//   - Close marks state==Closed first so late session callbacks
//     observe Closed and no-op (avoids spawning actors during shutdown).
//   - Cleanup runs under lifecycleMu so a direct Open/Close race —
//     Close runs while Open is still allocating cancelCtx/out/actors —
//     resolves cleanly: Close's cleanup sees an empty manager, then
//     Open's CAS-Opening-to-Open fails and the same cleanup helper
//     tears down the resources Open just allocated.
func (m *Manager) Close() {
	m.CloseCtx(context.Background())
}

// CloseCtx is Close with the actor joins BOUNDED by ctx, so a wedged
// actor (stuck handler) can't pin the caller's shutdown budget. Reports
// whether every actor actually exited; on false the stragglers leak
// harmlessly — their lifetime ctx and shutdown channel were already
// signalled, so they exit as soon as whatever they're blocked on
// unwinds.
func (m *Manager) CloseCtx(ctx context.Context) bool {
	// Mark Closed *before* tearing down: late callbacks (session
	// shutdown racing with Close) observe state==Closed and no-op
	// instead of spawning new actors.
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
// The ticker is local to runTickLoop (not a manager field) and stops
// itself when its closeCh closes, so cleanup doesn't need to touch it.
//
// MUST be called with lifecycleMu held. The actor joins are bounded by
// ctx; the return reports whether every actor actually exited.
func (m *Manager) cleanup(ctx context.Context) bool {
	complete := true
	if sess := m.session.Swap(nil); sess != nil {
		if sess.cancelCtx != nil {
			sess.cancelCtx()
		}
		if sess.closeCh != nil {
			close(sess.closeCh)
		}
		// Join the ticker goroutine (ctx-bounded): cancel-without-join
		// let it briefly outlive Close's nil return, breaking shutdown
		// determinism. It exits promptly on closeCh; the bound is
		// belt-and-braces.
		if sess.tickDone != nil {
			select {
			case <-sess.tickDone:
			case <-ctx.Done():
				select {
				case <-sess.tickDone:
				default:
					complete = false
				}
			}
		}
		// out is closed last, after actors have been stopped (below).
		// The close runs under emitMu, paired with emitRecoveryMessage's
		// session re-check under the same lock: an emitter mid-push
		// delays the close until it finishes, and an emitter arriving
		// after the Swap(nil) above bails on the re-check. Actors are
		// the sole writers to sess.out (via emitRecoveryMessage).
		defer func() {
			if sess.out != nil {
				m.emitMu.Lock()
				close(sess.out)
				m.emitMu.Unlock()
			}
		}()
	}

	// Stop every actor (idempotent). Reset the map so a subsequent
	// cleanup() doesn't re-stop the same actors.
	m.actorsMu.Lock()
	actors := make([]*recoveryActor, 0, len(m.actors))
	for _, a := range m.actors {
		actors = append(actors, a)
	}
	m.actors = make(map[int]*recoveryActor)
	m.actorsMu.Unlock()
	for _, a := range actors {
		if !a.stopBounded(ctx) {
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

// runTickLoop drives the periodic inactivity check by fanning out
// evTick events to every actor. Lossy: if an actor's inbox is full
// (which would only happen if it's blocked on the API), the tick is
// dropped — the next tick arrives in tickPeriod.
//
// closeCh is captured at goroutine spawn time (passed by Open). The
// ticker is local — the loop owns it via defer. No manager fields
// shared with cleanup(), so the loop is race-isolated from teardown.
func (m *Manager) runTickLoop(closeCh <-chan struct{}) {
	// Start ticking immediately rather than after initialDelay: the tick
	// also drives MaxRecoveryExecution expiry (actor.expireStuckEventRecoveries),
	// and a full initialDelay before the FIRST scan would let a short
	// recovery overshoot its cap by up to that delay (~60s). The
	// producer-down inactivity check still needs the warm-up — no alive
	// baseline exists yet — so it stays gated by inactivityArmed until
	// initialDelay has elapsed. Net: expiry lag is bounded by tickPeriod,
	// producer-down timing is unchanged.
	ticker := time.NewTicker(tickPeriod)
	defer ticker.Stop()
	start := time.Now()
	for {
		select {
		case <-ticker.C:
			now := time.Now()
			inactivityArmed := now.Sub(start) >= initialDelay
			m.actorsMu.RLock()
			actors := make([]*recoveryActor, 0, len(m.actors))
			for _, a := range m.actors {
				actors = append(actors, a)
			}
			m.actorsMu.RUnlock()
			for _, a := range actors {
				if !a.send(evTick{now: now, inactivityArmed: inactivityArmed}) {
					// Inbox full — actor is wedged on a slow handler or
					// flooded with alives/recovery events. Ticks are
					// designed to be lossy (next tick arrives in 10s),
					// but a sustained drop blinds the inactivity check.
					// Surface a rate-limited warn + counter so operators
					// can see it, instead of dropping silently.
					m.recordTickDrop(a.producerID)
				}
			}
			m.gcCompletedHandles(now)
		case <-closeCh:
			return
		}
	}
}

// ProducerStatus returns the most recent producer-status snapshot
// recorded by the per-producer actor, or (nil, false) if no actor
// exists for the id or the actor has not yet emitted a status. The
// snapshot survives lossy-channel drops on RecoveryEvents.
func (m *Manager) ProducerStatus(producerID int) (types.ProducerStatus, bool) {
	m.actorsMu.RLock()
	a, ok := m.actors[producerID]
	m.actorsMu.RUnlock()
	if !ok {
		return nil, false
	}
	if s := a.currentStatus(); s != nil {
		return s, true
	}
	return nil, false
}

// findOrSpawn returns the actor for producerID, lazily spawning one
// if a message arrives for a producer not in the initial set. Returns
// nil when the manager has been Closed (or hasn't yet been Open'd) —
// callers must handle this explicitly to avoid spawning actors that
// will never be cleaned up.
func (m *Manager) findOrSpawn(producerID int) *recoveryActor {
	if m.state.Load() != mgrStateOpen {
		return nil
	}
	m.actorsMu.RLock()
	a, ok := m.actors[producerID]
	m.actorsMu.RUnlock()
	if ok {
		return a
	}
	m.actorsMu.Lock()
	defer m.actorsMu.Unlock()
	// Re-check state under the write lock to close the race with Close
	// observing actorsMu while iterating actors to stop them.
	if m.state.Load() != mgrStateOpen {
		return nil
	}
	if a, ok = m.actors[producerID]; ok {
		return a
	}
	// Snapshot the session pointer atomically. State==Open implies
	// session was published (Open assigns session before the final
	// CAS). A concurrent Close could Swap(nil) between our state
	// check and this Load, in which case we bail.
	sess := m.session.Load()
	if sess == nil {
		return nil
	}
	a = newRecoveryActor(sess.ctx, producerID, m.cfg, m.apiClient, m.producerManager, m, m.logger, 256, m.initialSnapshotTime)
	m.actors[producerID] = a
	go a.run()
	return a
}

// --- Inbound feed events (called from session.go on the AMQP path) ---

// OnMessageProcessingStarted records the per-session start time and
// dispatches to the actor. No-op when the manager has been Closed
// (callbacks may fire from sessions racing with shutdown).
func (m *Manager) OnMessageProcessingStarted(sessionID uuid.UUID, producerID int, timestamp time.Time) {
	m.processingMu.Lock()
	m.processingTimes[sessionID] = timestamp
	m.processingMu.Unlock()

	if a := m.findOrSpawn(producerID); a != nil {
		// Direct call, no inbox: the handler touches only thread-safe
		// producer.Manager setters, and routing it through the inbox let
		// a full inbox PERMANENTLY drop this correctness input — leaving
		// recovery cursors / liveness stale under pressure.
		a.onMessageProcessingStarted(timestamp)
	}
}

// OnMessageProcessingEnded warns on >1s processing and dispatches the
// gen timestamp to the actor.
func (m *Manager) OnMessageProcessingEnded(sessionID uuid.UUID, producerID int, timestamp time.Time) {
	if !timestamp.IsZero() {
		if a := m.findOrSpawn(producerID); a != nil {
			// Direct call — same rationale as OnMessageProcessingStarted.
			a.onMessageProcessingEnded(timestamp)
		}
	}

	m.processingMu.Lock()
	start, ok := m.processingTimes[sessionID]
	delete(m.processingTimes, sessionID)
	m.processingMu.Unlock()

	switch {
	case !ok || start.IsZero():
		m.logger.Warn("message processing ended, but was not started")
	case time.Since(start).Milliseconds() > 1000:
		m.logger.Warnf("processing message took more than 1s - %d ms", time.Since(start).Milliseconds())
	}
}

// OnAliveReceived dispatches to the producer's actor via the COALESCED
// latest-alive mailbox: a full inbox can only delay an alive, never
// permanently drop it (pre-fix a dropped alive left liveness stale, and
// a tick admitted after a slot opened could falsely flag the producer
// down before the fresh state arrived).
func (m *Manager) OnAliveReceived(producerID int, timestamp types.MessageTimestamp, isSubscribed bool, messageInterest types.MessageInterest) {
	if a := m.findOrSpawn(producerID); a != nil {
		a.enqueueAlive(evAlive{
			timestamp:       timestamp,
			isSubscribed:    isSubscribed,
			messageInterest: messageInterest,
		})
	}
}

// OnSnapshotCompleteReceived admits a snapshot-complete to the producer's
// actor with ctx-bounded backpressure — it is a correctness event, not a
// lossy observability one, so it must NOT be silently dropped when the
// inbox is full. Returns nil once the event is admitted (delivery may be
// acked), or a non-nil error when ctx cancels or the manager is shutting
// down (caller must leave the delivery unacked for redelivery).
//
// An unknown producer is a no-op success: there is no recovery to
// complete, so the delivery can be acked.
func (m *Manager) OnSnapshotCompleteReceived(ctx context.Context, producerID int, requestID int, messageInterest types.MessageInterest) error {
	m.actorsMu.RLock()
	a, ok := m.actors[producerID]
	m.actorsMu.RUnlock()
	if !ok {
		// Distinguish "genuinely unknown producer" (no recovery to
		// complete → safe to ack) from "actor map already reset by
		// shutdown" (completion unprocessable → must NOT be acked).
		// CloseCtx stores mgrStateClosed BEFORE cleanup resets the map
		// (and the map reset's actorsMu section orders that store before
		// our RLock read), so a lookup that misses because of teardown
		// always observes a non-Open state here; the delivery then stays
		// unacked and the broker redelivers it.
		if m.state.Load() != mgrStateOpen {
			return ErrManagerClosed
		}
		return nil // unknown producer; nothing to validate, safe to ack
	}
	return a.sendCtx(ctx, evSnapshotComplete{requestID: requestID, messageInterest: messageInterest})
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
		// Opening is "not yet ready" — fields the actor would touch
		// (m.ctx, m.out, …) might be half-initialized. Return
		// ErrManagerNotOpen consistently so callers retry rather than
		// fail terminally.
		return nil, ErrManagerNotOpen
	case mgrStateClosed:
		return nil, ErrManagerClosed
	}
	// Validate catalog membership BEFORE spawning: findOrSpawn
	// allocates a 256-slot inbox, registers the actor in the map, and
	// starts its goroutine — pre-fix an invalid producer id did all of
	// that, the recover command then failed with ErrProducerNotFound
	// inside the actor, and the dead-weight actor (map entry,
	// goroutine, tick work, post-warm-up error logging) lived until
	// Client shutdown. Repeated calls with distinct invalid ids grew
	// all of it without bound.
	if _, err := m.producerManager.GetProducer(ctx, producerID); err != nil {
		return nil, fmt.Errorf("recovery: validate producer %d: %w", producerID, err)
	}
	a := m.findOrSpawn(producerID) //nolint:contextcheck // findOrSpawn may spawn an actor; the actor uses its own lifetime ctx, not this caller's. Per-request ctx flows via evRecoverEvent.ctx below and is composed with the actor lifetime ctx inside dispatch.
	if a == nil {
		// Manager raced into Closed between the state check and
		// findOrSpawn — surface the same error.
		return nil, ErrManagerClosed
	}
	reply := make(chan recoverEventReply, 1)
	// sendCtxCommand, not sendCtx: a successful admission of the
	// recover COMMAND is acceptance — the reply/actor-done wait below
	// resolves whether it ran (handle) or was stranded by shutdown
	// (ErrManagerClosed, then truthfully nothing was issued). The
	// conservative post-admission shutdown re-check in sendCtx is for
	// idempotent redeliverable notifications only; applied here it
	// could report ErrManagerClosed for a command the shutdown drain
	// still dispatches — handle registered, POST potentially issued —
	// and a retrying caller would duplicate the recovery.
	if err := a.sendCtxCommand(ctx, evRecoverEvent{
		ctx:              ctx,
		eventID:          eventID,
		statefulRecovery: stateful,
		reply:            reply,
	}); err != nil {
		return nil, err
	}
	// Admission succeeded — from here the reply is GUARANTEED and prompt:
	// onRecoverEvent replies synchronously into the buffered chan before
	// dispatching the (detached) API call, every other handler is
	// short (no inline I/O), and run() drains admitted events even when
	// shutdown races the admission. Deliberately NO ctx.Done() arm here:
	// the caller's ctx bounds ADMISSION only. Honoring it in this wait
	// made "nil handle + ctx.Err()" a lie — the actor registered the
	// recovery and started the detached POST anyway (rooted at the actor
	// lifetime, by design), so a caller retrying after the cancellation
	// initiated a DUPLICATE recovery, contradicting the documented
	// "error ⇒ recovery was not accepted" contract. Post-admission
	// cancellation now returns the live handle instead.
	select {
	case r := <-reply:
		return r.handle, r.err
	case <-a.done:
		// Actor exited. Its shutdown drain dispatches admitted events
		// before run() returns, so the reply is normally already
		// buffered — drain it non-blocking; the handle (if any) has been
		// failed by failPendingHandles/cleanup. An empty reply chan means
		// the drain-vs-admission race was resolved by sendCtx reporting
		// ErrManagerClosed, so this arm is belt-and-braces for a caller
		// with a non-cancellable ctx racing client.Close.
		select {
		case r := <-reply:
			return r.handle, r.err
		default:
			return nil, ErrManagerClosed
		}
	}
}

// --- Handle registry (called by actors via actorManagerOps) ---

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
	h.complete(status, err, time.Now())
	return h
}

func (m *Manager) gcCompletedHandles(now time.Time) {
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
		h.complete(types.RecoveryStatusFailed, err, time.Now())
	}
}

// --- actorManagerOps ---

// nextRequestID is the actorManagerOps method backed by the shared
// generator. Generator is internally locked.
func (m *Manager) nextRequestID() int { return m.sequence.next() }

// emitRecoveryMessage is the actorManagerOps method to publish a
// RecoveryMessage on the public stream.
//
// Drop-oldest semantics, matching the public event-channel policy
// (NEXT.md §0.3): on full, drop the oldest queued message and push
// the new one. Blocking on a slow consumer would stall actors and
// risk shutdown deadlocks (an actor still holding a value while
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
	// mechanical invariant instead of an actors-stopped-first ordering
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
