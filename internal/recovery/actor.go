package recovery

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/config"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/internal/producer"
	"github.com/oddin-gg/gosdk/types"
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
	producerID int
	cfg        config.Config
	api        *api.Client
	pm         *producer.Manager
	mgr        actorManagerOps // narrow interface back to the Manager
	logger     *log.Logger

	// initialSnapshotTime is the lookback window applied to the first
	// snapshot recovery when the producer has no recovery cursor.
	// Zero = full history (pre-wiring behaviour).
	initialSnapshotTime time.Duration

	// Inbox + lifecycle.
	inbox    chan actorEvent
	shutdown chan struct{}
	// detached tracks the actor's detached API-call wrappers (snapshot
	// and event-recovery POSTs). Adds happen on the actor goroutine
	// only, so stopBounded's post-join wait never races an Add.
	detached sync.WaitGroup

	// pendingSystemAlive / pendingUserAlive coalesce alive events:
	// OnAliveReceived stores the LATEST alive of its interest class here
	// and best-effort nudges the inbox. dispatch (on the nudge) and onTick
	// drain both — so inbox pressure can only delay an alive, never
	// permanently drop it, and a tick can never evaluate producer
	// staleness while a fresher alive sits unprocessed.
	//
	// Two slots, NOT one: user sessions deliberately bind the system-alive
	// routing key, so every broker alive fans out to the system session
	// AND all user sessions near-simultaneously. Only SystemAliveOnly
	// alives update lastSystemAlive and drive the recovery state machine.
	// A single shared slot let a user-session alive, stored before the
	// actor drained, permanently overwrite an unprocessed system alive —
	// repeated losses pushed aliveInterval past MaxInactivity and produced
	// a false producer-down plus a spurious full snapshot recovery.
	// Coalescing is now scoped WITHIN each interest class only.
	pendingSystemAlive atomic.Pointer[evAlive]
	pendingUserAlive   atomic.Pointer[evAlive]

	// pendingLossAt holds the EARLIEST unapplied channel-loss instant as
	// unix nanoseconds (0 = none), set by enqueueChannelLost; drained
	// (and acted on) before any alive or tick is evaluated, so the
	// producer is already flagged down when the first alive after the
	// rebind arrives.
	pendingLossAt atomic.Int64

	// recoveryFloor is the earliest instant a snapshot recovery must
	// reach back to: the earliest channel loss not yet covered by a
	// completed recovery (anchored at the pre-loss recovery cursor, see
	// enqueueChannelLost), and the cursor of any recovery a loss
	// interrupted (its replay was lost with the queue). makeSnapshotRecovery
	// never starts later than it, and snapshotRecoveryFinished clears it
	// only when the completed recovery started at or before it. This is
	// what makes the ordering of a loss notice and the alives around it
	// irrelevant: an alive that overtakes the notice may advance the
	// producer's cursor, but not past the floor. Actor goroutine only.
	recoveryFloor time.Time

	// deferredRecovery is set when a snapshot recovery was due but a
	// lost session in this producer's scope had not re-bound its queue
	// yet; resumeDeferredRecovery starts it once the rebind is reported
	// (evChannelRebound) or, as fallback, on the next tick or alive.
	deferredRecovery bool
	done             chan struct{}

	// Manager-lifetime ctx, used for API calls. Cancelled at shutdown.
	ctx context.Context

	// Per-producer state. Only the actor goroutine touches these.
	recoveryState          types.RecoveryState
	currentRecovery        *recoveryData
	eventRecoveries        map[int]*eventRecovery
	lastUserSessionAlive   time.Time
	lastValidAliveGen      time.Time  // gen-timestamp captured during recovery
	lastSystemAlive        *time.Time // pointer: nil = never seen
	firstRecoveryCompleted bool
	downReason             types.ProducerDownReason
	statusReason           types.ProducerStatusReason

	// statusSnapshot is the most recent ProducerStatus emitted by this
	// actor. Read by external callers (Client.ProducerStatus) under the
	// atomic-pointer pattern — the actor goroutine is the only writer.
	statusSnapshot atomic.Pointer[producerStatusImpl]
}

// actorManagerOps is the narrow surface the actor needs back into the
// Manager. Limiting it makes the dependency explicit and keeps test
// doubles simple.
type actorManagerOps interface {
	registerHandle(*Handle)
	completeHandle(requestID int, status types.RecoveryRequestStatus, err error) *Handle
	LookupHandle(requestID int) (*Handle, bool)
	nextRequestID() int
	emitRecoveryMessage(types.RecoveryMessage)
	// rebindPending reports whether a lost session in the producer's
	// scope still has no queue; see Manager.rebindPending.
	rebindPending(producerID int) bool
}

// maxPendingEventRecoveries bounds concurrent pending event recoveries
// PER PRODUCER — generous for legitimate post-outage re-request bursts,
// small enough that a caller flooding distinct events against a slow
// recovery API cannot grow handles/goroutines/map entries until the
// MaxRecoveryExecution sweep (~hours). Identical (event, kind) requests
// coalesce onto one handle and don't consume extra slots.
const maxPendingEventRecoveries = 128

// ErrTooManyPendingEventRecoveries is returned by RecoverEventOdds /
// RecoverEventStateful when the producer already has
// maxPendingEventRecoveries recoveries awaiting snapshot_complete.
// Retryable once earlier recoveries complete or time out.
var ErrTooManyPendingEventRecoveries = errors.New("recovery: too many pending event recoveries for producer")

func newRecoveryActor(
	ctx context.Context,
	producerID int,
	cfg config.Config,
	apiClient *api.Client,
	pm *producer.Manager,
	mgr actorManagerOps,
	logger *log.Logger,
	inboxSize int,
	initialSnapshotTime time.Duration,
) *recoveryActor {
	if inboxSize <= 0 {
		inboxSize = 256
	}
	return &recoveryActor{
		producerID:          producerID,
		cfg:                 cfg,
		api:                 apiClient,
		pm:                  pm,
		mgr:                 mgr,
		logger:              logger,
		initialSnapshotTime: initialSnapshotTime,
		ctx:                 ctx,
		inbox:               make(chan actorEvent, inboxSize),
		shutdown:            make(chan struct{}),
		done:                make(chan struct{}),
		eventRecoveries:     make(map[int]*eventRecovery),
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
			// Drain-then-exit: sendCtx's admission races close(shutdown)
			// (both select arms ready → the runtime picks either), so a
			// correctness event (snapshot_complete) can be admitted-and-
			// acked a moment before shutdown and still sit in the buffered
			// inbox. Exiting without dispatching it would silently drop an
			// event whose AMQP delivery was already acked on the strength
			// of the admission. Handlers are shutdown-safe: emits re-check
			// the session under emitMu, API calls are bounded by the
			// already-cancelled a.ctx, handle completions are idempotent.
			for {
				select {
				case ev := <-a.inbox:
					a.dispatch(ev)
				default:
					return
				}
			}
		}
	}
}

// enqueueAlive stores ev as the pending (latest) alive and nudges the
// actor. The nudge send is best-effort: if the inbox is full, evTick —
// and any later nudge — drains pendingAlive anyway.
func (a *recoveryActor) enqueueAlive(ev evAlive) {
	if ev.messageInterest == types.SystemAliveOnly {
		a.pendingSystemAlive.Store(&ev)
	} else {
		a.pendingUserAlive.Store(&ev)
	}
	a.send(evAliveNudge{})
}

// enqueueChannelLost records that a consumer channel — and with it its
// exclusive queue — was lost at lostAt, and nudges the actor. Coalesced
// like alive, keeping the EARLIEST instant: the flag is what carries the
// fact, the nudge may be dropped by a full inbox.
//
// The instant recorded is the earlier of lostAt and the producer's loss
// anchor (producer.Manager.LossAnchor): the recovery cursor as it stood
// BEFORE the most recent alive, or an explicit rewind. The most recent
// alive is not trusted because it arrives on the alive session's own
// channel and may have been generated after the broker deleted the
// consumer's queue yet processed — concurrently with this call, on the
// actor goroutine — before the loss was seen locally; sampling the
// cursor here on the reporting goroutine cannot order itself against
// that. The alive before it is a full alive interval older, safely
// pre-loss. In the producer's clock domain, the anchor also covers
// messages the dead queue still held undelivered — a lagging consumer's
// queue may hold a span timestamped well before the loss, and those die
// with it too. lostAt alone (client wall clock) is the fallback when the
// producer has no cursor yet.
func (a *recoveryActor) enqueueChannelLost(lostAt time.Time) {
	anchor := lostAt
	if cursor, err := a.pm.LossAnchor(a.producerID); err == nil && !cursor.IsZero() && cursor.Before(anchor) {
		anchor = cursor
	}
	a.notePendingLoss(anchor)
	a.send(evChannelLossNudge{})
}

// notePendingLoss folds lostAt into pendingLossAt, keeping the earliest.
func (a *recoveryActor) notePendingLoss(lostAt time.Time) {
	ns := lostAt.UnixNano()
	for {
		cur := a.pendingLossAt.Load()
		if cur != 0 && cur <= ns {
			return
		}
		if a.pendingLossAt.CompareAndSwap(cur, ns) {
			return
		}
	}
}

// lowerRecoveryFloor moves the recovery floor to t if t is earlier (or
// no floor is set). Actor goroutine only.
func (a *recoveryActor) lowerRecoveryFloor(t time.Time) {
	if t.IsZero() {
		return
	}
	if a.recoveryFloor.IsZero() || t.Before(a.recoveryFloor) {
		a.recoveryFloor = t
	}
}

// drainPendingChannelLoss acts on a pending channel loss, if any, and
// reports whether nothing is left pending. The flag is cleared only once
// the reaction took effect: when the producer manager cannot answer (a
// network fault that dropped AMQP may well be failing the producer
// catalog too) the flag is put back so the next nudge or tick retries —
// unlike alive, nothing upstream repeats this notice. While it is
// pending the caller must NOT process alives: an alive handled with the
// producer still up would advance the recovery cursor past the gap the
// deferred reaction is meant to recover. Actor-goroutine only.
func (a *recoveryActor) drainPendingChannelLoss() (settled bool) {
	ns := a.pendingLossAt.Swap(0)
	if ns == 0 {
		return true
	}
	lostAt := time.Unix(0, ns)
	// Floor FIRST, before the reaction that may fail: from here on no
	// alive — however it interleaves — can move the recovery past the
	// loss.
	a.lowerRecoveryFloor(lostAt)
	if !a.onChannelLost() {
		a.notePendingLoss(lostAt)
		return false
	}
	return true
}

// onChannelLost closes the gap a lost consumer channel opens. Every
// subscription consumes from an exclusive, auto-delete queue: when its
// channel goes — with the whole connection, or alone on a channel-level
// exception — the broker deletes the queue, and everything published to
// the exchange until the consumer re-binds is lost; nothing is
// "released" or redelivered. The alive-based checks only notice a
// connection drop longer than MaxInactivity and never notice a single
// channel's loss while alives keep flowing on another, so both used to
// pass silently. Flagging the producer down makes the next system alive
// issue a snapshot recovery from the last alive gen timestamp before the
// loss (systemAliveReceived: flagged down + not performing recovery →
// makeSnapshotRecovery); an in-flight recovery is interrupted by
// producerDown and restarted the same way, since its replayed messages
// were lost along with the rest.
//
// The notice is raised at the moment of loss, before the consumer starts
// re-binding; and independently of timing, drainPendingChannelLoss has
// already lowered the recovery floor to the loss instant, so an alive
// that reaches the actor first cannot move the recovery past the gap.
//
// Returns false when the reaction could not be applied because the
// producer manager errored; the caller then keeps the notice pending.
func (a *recoveryActor) onChannelLost() bool {
	enabled, err := a.pm.IsProducerEnabled(a.ctx, a.producerID)
	if err != nil {
		a.logger.WithError(err).WithField("producer_id", a.producerID).
			Warn("recovery: channel lost but producer state unavailable; will retry")
		return false
	}
	if !enabled {
		return true
	}
	a.logger.WithField("producer_id", a.producerID).
		Warn("recovery: consumer channel lost; messages published until the queue is re-bound are lost with it — flagging producer down to force a snapshot recovery")
	if err := a.producerDown(types.ConnectionDownProducerDownReason); err != nil {
		a.logger.WithError(err).WithField("producer_id", a.producerID).Error("recovery: producer-down after channel loss; will retry")
		return false
	}
	return true
}

// drainPendingAlive processes the latest coalesced alive of each interest
// class, if any. Actor-goroutine only.
//
// User-session alive first: a system alive processed afterwards then
// observes the freshest lastUserSessionAlive when systemAliveReceived
// evaluates producer timing, matching the arrival order of a normal feed.
func (a *recoveryActor) drainPendingAlive() {
	if ev := a.pendingUserAlive.Swap(nil); ev != nil {
		a.onAlive(*ev)
	}
	if ev := a.pendingSystemAlive.Swap(nil); ev != nil {
		a.onAlive(*ev)
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

// sendCtx pushes an event with ctx-bounded backpressure. Returns
// ctx.Err() if ctx fires first or ErrManagerClosed if the actor's
// shutdown channel closes (manager Close is racing with us).
//
// A nil return GUARANTEES the event will be dispatched: either the main
// loop picks it up, or run()'s drain-on-shutdown does. The pre-check
// keeps an already-stopping actor from admitting new events at random
// (the select picks uniformly among ready arms); the post-admission
// re-check closes the remaining race — if close(shutdown) overlapped
// the send, run()'s final drain poll may already have run, so the
// admission is conservatively reported as ErrManagerClosed. The caller
// then leaves the delivery unacked and the broker redelivers; if the
// drain DID also dispatch the event, the redelivered copy is dropped by
// the requestID/state guards (snapshot-complete handling is idempotent).
//
// This conservative shape is ONLY for idempotent, redeliverable
// notifications (snapshot-complete, detached API results). Commands
// with a reply channel must use sendCtxCommand — reporting
// "not admitted" for a command the shutdown drain still dispatches
// makes the caller's error a lie (the actor registers the handle and
// starts the POST anyway).
func (a *recoveryActor) sendCtx(ctx context.Context, ev actorEvent) error {
	// Fast-fail on an already-cancelled ctx BEFORE attempting admission:
	// the select below picks uniformly among ready arms, so a cancelled
	// ctx racing a free inbox slot could otherwise admit the event and
	// return nil — for evRecoverEvent that means a recovery the caller
	// was told (via a later ctx.Err()) never happened.
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-a.shutdown:
		return ErrManagerClosed
	default:
	}
	select {
	case a.inbox <- ev:
		select {
		case <-a.shutdown:
			// Shutdown raced the admission — see doc comment: report
			// not-admitted so the delivery stays unacked.
			return ErrManagerClosed
		default:
			// Shutdown had NOT begun when the send completed, so any
			// later drain necessarily observes the buffered event.
			return nil
		}
	case <-ctx.Done():
		return ctx.Err()
	case <-a.shutdown:
		return ErrManagerClosed
	}
}

// sendCtxCommand admits a reply-carrying command (evRecoverEvent): a
// successful send IS acceptance, with NO post-admission shutdown
// re-check. The caller resolves the actual outcome by awaiting the
// reply (or actor-done): if the shutdown drain dispatched the admitted
// command, the reply carries the registered handle — the caller gets
// the truth instead of a fabricated ErrManagerClosed while the actor
// registered the recovery and (racing a.ctx's cancellation relay,
// context.AfterFunc runs asynchronously on an already-cancelled ctx)
// could even issue the external POST; a caller retrying on that lie
// would start a DUPLICATE recovery under a fresh request id. If the
// drain instead missed the admission (send completed after the final
// drain poll), no reply ever comes — the caller's actor-done arm
// reports ErrManagerClosed, and then it is genuinely true that nothing
// ran.
func (a *recoveryActor) sendCtxCommand(ctx context.Context, ev actorEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-a.shutdown:
		return ErrManagerClosed
	default:
	}
	select {
	case a.inbox <- ev:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-a.shutdown:
		return ErrManagerClosed
	}
}

// stop terminates the actor. Idempotent. Unbounded join — use
// stopBounded from paths that carry a shutdown budget.
func (a *recoveryActor) stop() {
	a.stopBounded(context.Background())
}

// stopBounded signals shutdown and joins the actor goroutine BOUNDED by
// ctx. Reports whether the goroutine actually exited; on false it leaks
// harmlessly until its current handler unwinds (everything it waits on
// was already signalled — a.ctx is cancelled before stop is called on
// the Manager.Close path).
func (a *recoveryActor) stopBounded(ctx context.Context) bool {
	select {
	case <-a.shutdown:
		// already stopping
	default:
		close(a.shutdown)
	}
	joined := func() bool {
		select {
		case <-a.done:
			return true
		case <-ctx.Done():
			// Completion-first accounting: with both arms ready the
			// select picks uniformly, so an exited actor could be
			// reported as a straggler (feeding a false "work still
			// pending" terminal cause). Re-check completion before
			// reporting failure.
			select {
			case <-a.done:
				return true
			default:
				return false
			}
		}
	}()
	if !joined {
		return false
	}
	// Join the detached API wrappers too (bounded). The API in-flight
	// slot is released when the response body closes — BEFORE the
	// wrapper's logging / result-delivery / handle-completion tail — so
	// without this join Client.Close could return while a wrapper still
	// runs. Adds happen only on the (now exited) actor goroutine, so
	// this Wait cannot race an Add.
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
	case evAliveNudge:
		// A channel loss that preceded this alive must be applied first,
		// so the alive sees the producer flagged down and starts recovery.
		// If it could not be applied yet, the alive stays coalesced for
		// the retry — processing it now would advance the cursor past
		// the gap.
		if a.drainPendingChannelLoss() {
			a.drainPendingAlive()
		}
	case evChannelLossNudge:
		a.drainPendingChannelLoss()
	case evChannelRebound:
		a.resumeDeferredRecovery()
	case evSnapshotComplete:
		a.onSnapshotComplete(e)
	case evRecoverEvent:
		a.onRecoverEvent(e)
	case evRecoverEventCompleted:
		a.onRecoverEventCompleted(e)
	case evSnapshotRecoveryAPICompleted:
		a.onSnapshotRecoveryAPICompleted(e)
	case evTick:
		// Drain any coalesced channel loss and alive FIRST: the tick must
		// never evaluate producer staleness while a fresher alive sits
		// unprocessed, and the tick is the fallback that applies a
		// channel loss whose nudge was dropped by a full inbox. An alive
		// stays coalesced while a loss is still pending (see
		// drainPendingChannelLoss).
		if a.drainPendingChannelLoss() {
			a.drainPendingAlive()
		}
		a.resumeDeferredRecovery() // fallback for a dropped evChannelRebound
		a.onTick(e.now, e.inactivityArmed)
	default:
		a.logger.Warnf("recovery actor: unknown event type %T", ev)
	}
}

// --- Producer-manager state queries (delegate to thread-safe pm) ---

// isDisabled and isFlaggedDown coerce ANY producer-manager error to
// the conservative "disabled"/"down" answer. Pre-v2.31 the error was
// silently dropped — a transient producer-catalog fetch failure made
// every producer look administratively disabled with zero diagnostic.
// Now we log WithError so the underlying cause is recoverable.
func (a *recoveryActor) isDisabled() bool {
	enabled, err := a.pm.IsProducerEnabled(a.ctx, a.producerID)
	if err != nil {
		a.logger.WithError(err).WithField("producer_id", a.producerID).Warn("recovery: isDisabled fallback to true (producer-manager error)")
		return true
	}
	return !enabled
}

func (a *recoveryActor) isFlaggedDown() bool {
	down, err := a.pm.IsProducerDown(a.ctx, a.producerID)
	if err != nil {
		a.logger.WithError(err).WithField("producer_id", a.producerID).Warn("recovery: isFlaggedDown fallback to true (producer-manager error)")
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
	return a.recoveryState == types.StartedRecoveryState ||
		a.recoveryState == types.InterruptedRecoveryState
}

func (a *recoveryActor) isKnownRecovery(requestID int) bool {
	if a.currentRecovery != nil && a.currentRecovery.recoveryID == requestID {
		return true
	}
	_, ok := a.eventRecoveries[requestID]
	return ok
}

func (a *recoveryActor) snapshotValidationNeeded(interest types.MessageInterest) bool {
	return interest == types.LiveOnlyMessageInterest ||
		interest == types.PrematchOnlyMessageInterest
}

// validateSnapshotComplete checks whether a SnapshotComplete with this
// requestID + interest finishes the current full snapshot recovery.
//
// Logic preserved exactly from the pre-actor implementation; matches
// Java/Kotlin and .NET reference SDKs (all three accept Started OR
// Interrupted state via the !isPerformingRecovery gate).
func (a *recoveryActor) validateSnapshotComplete(requestID int, interest types.MessageInterest) bool {
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

func (a *recoveryActor) validateEventSnapshotComplete(requestID int, interest types.MessageInterest) bool {
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
//
// Pre-fix the inner loop overwrote finished[i] on every iteration of
// `received`, so for a mixed-scope producer (live|prematch) receiving
// both interests the second iteration always cleared the first scope's
// match — validation could never succeed. Java/.NET use set membership
// here; we mirror that with slices.Contains so a scope is "finished"
// iff its corresponding MessageInterest appears anywhere in `received`.
func (a *recoveryActor) validateProducerSnapshotCompletes(received []types.MessageInterest) (bool, error) {
	prod, err := a.pm.GetProducer(a.ctx, a.producerID)
	if err != nil {
		return false, err
	}
	scopes := prod.ProducerScopes()
	finished := make([]bool, len(scopes))
	for i, scope := range scopes {
		var want types.MessageInterest
		switch scope {
		case types.LiveProducerScope:
			want = types.LiveOnlyMessageInterest
		case types.PrematchProducerScope:
			want = types.PrematchOnlyMessageInterest
		default:
			return false, errors.New("unknown producer scope")
		}
		finished[i] = slices.Contains(received, want)
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
		a.logger.WithField("producer_id", a.producerID).Warn("recovery: processing started with zero timestamp")
		return
	}
	if err := a.pm.SetProducerLastMessageTimestamp(a.producerID, t); err != nil {
		a.logger.WithError(err).WithField("producer_id", a.producerID).Error("recovery: set producer last message timestamp")
	}
}

func (a *recoveryActor) onMessageProcessingEnded(t time.Time) {
	if t.IsZero() {
		return
	}
	if err := a.pm.SetLastProcessedMessageGenTimestamp(a.producerID, t); err != nil {
		a.logger.WithError(err).WithField("producer_id", a.producerID).Error("recovery: set last processed gen timestamp")
	}
}

func (a *recoveryActor) onAlive(e evAlive) {
	if a.isDisabled() {
		return
	}
	if e.messageInterest == types.SystemAliveOnly {
		if err := a.systemAliveReceived(e.timestamp, e.isSubscribed); err != nil {
			a.logger.WithError(err).WithField("producer_id", a.producerID).Error("recovery: process system alive")
		}
		return
	}
	a.lastUserSessionAlive = e.timestamp.Created
}

func (a *recoveryActor) onSnapshotComplete(e evSnapshotComplete) {
	switch {
	case a.isDisabled():
		a.logger.WithField("producer_id", a.producerID).WithField("request_id", e.requestID).Info("recovery: snapshot complete for disabled producer")
	case !a.isKnownRecovery(e.requestID):
		a.logger.WithField("producer_id", a.producerID).WithField("request_id", e.requestID).Info("recovery: unknown snapshot complete")
	case a.validateEventSnapshotComplete(e.requestID, e.messageInterest):
		if err := a.eventRecoveryFinished(e.requestID); err != nil {
			a.logger.WithError(err).WithField("producer_id", a.producerID).WithField("request_id", e.requestID).Error("recovery: event recovery finished")
		}
	case a.validateSnapshotComplete(e.requestID, e.messageInterest):
		if err := a.snapshotRecoveryFinished(e.requestID); err != nil {
			a.logger.WithError(err).WithField("producer_id", a.producerID).WithField("request_id", e.requestID).Error("recovery: snapshot recovery finished")
		}
	}
}

func (a *recoveryActor) onTick(now time.Time, inactivityArmed bool) {
	// Timeout scan for in-flight event recoveries: per NEXT.md the
	// configured MaxRecoveryExecution caps the wall time a single
	// recovery may take. Without this scan, an event recovery whose
	// SnapshotComplete never arrives would hang the handle forever
	// (RecoveryStatusTimedOut would remain unreachable). This runs on
	// EVERY tick — including during the manager warm-up window — so the
	// enforcement lag is bounded by tickPeriod, not initialDelay+tickPeriod.
	a.expireStuckEventRecoveries(now)
	a.expireStuckSnapshotRecovery(now)

	// Suppress the producer-down inactivity check until the warm-up window
	// has elapsed: with no alive baseline yet, aliveInterval would be huge
	// and flag a healthy producer down at the first tick.
	if !inactivityArmed {
		return
	}

	if a.isDisabled() {
		return
	}
	var lastTimestamp time.Time
	if a.lastSystemAlive != nil {
		lastTimestamp = *a.lastSystemAlive
	}
	aliveInterval := now.Sub(lastTimestamp)
	var err error
	var downReason types.ProducerDownReason
	switch {
	case aliveInterval > a.cfg.MaxInactivity():
		downReason = types.AliveInternalViolationProducerDownReason
		err = a.producerDown(downReason)
	case !a.calculateTiming(now):
		downReason = types.ProcessingQueueDelayViolationProducerDownReason
		err = a.producerDown(downReason)
	}
	if err != nil {
		a.logger.WithError(err).
			WithField("producer_id", a.producerID).
			WithField("down_reason", downReason).
			Error("recovery: tick producer-down transition")
	}
}

// expireStuckEventRecoveries scans active event recoveries for
// requests whose start time is older than MaxRecoveryExecution and
// transitions them to RecoveryStatusTimedOut. Pure-state in the actor
// goroutine — no locks needed.
// expireStuckSnapshotRecovery sweeps the FULL snapshot recovery
// (currentRecovery) against MaxRecoveryExecution from the tick loop.
// Pre-fix only per-event recoveries expired here — currentRecovery's
// age was checked ONLY by the next system alive, so with the
// SnapshotComplete lost AND alives absent (producer gone quiet), the
// recovery sat Started/Interrupted indefinitely. Expiring to Error
// mirrors the late-API-failure rollback: the next alive re-issues a
// fresh snapshot recovery.
func (a *recoveryActor) expireStuckSnapshotRecovery(now time.Time) {
	if !a.isPerformingRecovery() || a.currentRecovery == nil {
		return
	}
	maxAge := a.cfg.MaxRecoveryExecution()
	if maxAge <= 0 {
		return // defense-in-depth; New rejects non-positive values
	}
	started := a.lastRecoveryStartedAt()
	if started.IsZero() || now.Sub(started) <= maxAge {
		return
	}
	a.logger.
		WithField("producer_id", a.producerID).
		WithField("request_id", a.currentRecovery.recoveryID).
		WithField("started_at", started).
		Error("recovery: snapshot recovery exceeded MaxRecoveryExecution; transitioning to Error")
	a.currentRecovery = nil
	a.recoveryState = types.ErrorRecoveryState
}

func (a *recoveryActor) expireStuckEventRecoveries(now time.Time) {
	if len(a.eventRecoveries) == 0 {
		return
	}
	maxAge := a.cfg.MaxRecoveryExecution()
	if maxAge <= 0 {
		// Defense-in-depth only: gosdk.New rejects a non-positive
		// WithMaxRecoveryExecution as ErrInvalidConfig
		// (validateConfigBounds), so this branch is unreachable via
		// public construction — it guards internal/test wiring that
		// bypasses New.
		return
	}
	var expired []int
	for id, er := range a.eventRecoveries {
		if now.Sub(er.recoveryStartedAt) > maxAge {
			expired = append(expired, id)
		}
	}
	for _, id := range expired {
		if er := a.eventRecoveries[id]; er != nil && er.cancelAPI != nil {
			// Abort a POST still in flight — a timed-out recovery must
			// not keep its HTTP request (and detached goroutine)
			// running for the remainder of the API timeout.
			er.cancelAPI()
		}
		delete(a.eventRecoveries, id)
		a.mgr.completeHandle(id, types.RecoveryStatusTimedOut, errors.New("recovery: event recovery exceeded MaxRecoveryExecution"))
	}
}

// onRecoverEvent registers the recovery (creates the handle, populates
// eventRecoveries) on the actor goroutine, replies to the caller with
// the handle SYNCHRONOUSLY, then dispatches the API call to a detached
// goroutine. The API result feeds back as evRecoverEventCompleted so
// state mutation stays single-threaded.
//
// Pre-fix this handler ran the API call inline (~30 s with retries),
// blocking the actor for the duration — every alive/tick/snapshot for
// this producer queued behind it; under load the 256-slot inbox
// dropped alives and triggered false producer-down. Pre-fix, callers
// whose ctx fired during the inline API call also orphaned the
// resulting handle (caller returned ctx.Err() with no reference; the
// handle survived until 5-min GC). Both classes of bug are fixed by
// the early-reply restructure.
func (a *recoveryActor) onRecoverEvent(e evRecoverEvent) {
	now := time.Now()
	// Coalesce identical pending requests: a caller re-requesting the
	// same (event, kind) while one is already in flight receives the
	// EXISTING handle. Pre-fix every call against a slow recovery API
	// fanned out its own handle + map entry + detached POST goroutine —
	// the 256-slot inbox bounded nothing because the actor dequeues
	// commands quickly.
	for id, er := range a.eventRecoveries {
		if er.eventID == e.eventID && er.stateful == e.statefulRecovery {
			if h, ok := a.mgr.LookupHandle(id); ok {
				e.reply <- recoverEventReply{handle: h}
				return
			}
		}
	}
	// Bounded pending admission per producer: distinct-event floods are
	// rejected instead of growing handles/goroutines/map entries until
	// MaxRecoveryExecution (~hours) sweeps them.
	if len(a.eventRecoveries) >= maxPendingEventRecoveries {
		e.reply <- recoverEventReply{err: fmt.Errorf("recovery: producer %d has %d pending event recoveries: %w",
			a.producerID, len(a.eventRecoveries), ErrTooManyPendingEventRecoveries)}
		return
	}
	producerName, err := a.producerName()
	if err != nil {
		e.reply <- recoverEventReply{err: err}
		return
	}
	requestID := a.mgr.nextRequestID()
	handle := NewHandle(requestID, a.producerID, e.eventID, now)
	a.mgr.registerHandle(handle)
	er := newEventRecovery(e.eventID, requestID, now)
	er.stateful = e.statefulRecovery
	a.eventRecoveries[requestID] = er

	// Reply immediately so the caller has a Handle even if its ctx
	// fires during the API call.
	e.reply <- recoverEventReply{handle: handle}

	// Root the detached API call at the ACTOR lifetime, NOT the caller's
	// ctx (e.ctx). The caller's ctx bounded ADMISSION only — the send into
	// the inbox and the wait for this reply (both in dispatchRecoverEvent).
	// Once the handle is returned, the recovery is a long-running operation
	// owned by the actor/handle lifecycle, and callers routinely
	// `defer cancel()` their request ctx the instant they hold the handle.
	// Rooting apiCtx at e.ctx let that deferred cancel abort the in-flight
	// recovery POST, tearing down a recovery the caller believed was
	// running. Background() + AfterFunc(a.ctx, ...) makes the API call
	// cancellable ONLY by actor shutdown: Manager.cleanup cancels a.ctx
	// before actor.stop(), so a Close racing a slow POST still aborts it
	// (no permanently-blocked goroutine), while an ordinary caller-side
	// cancel no longer touches it.
	apiCtx, apiCancel := context.WithCancel(context.Background())
	stopAfter := context.AfterFunc(a.ctx, apiCancel)
	// Retained so the expiry sweep can abort a POST still in flight
	// when the recovery times out (actor-goroutine-only access).
	er.cancelAPI = apiCancel

	stateful := e.statefulRecovery
	eventID := e.eventID
	nodeID := a.cfg.SdkNodeID()
	a.detached.Add(1) // joined (bounded) by stopBounded
	go func() {
		defer a.detached.Done()
		defer stopAfter()
		defer apiCancel()
		var (
			success bool
			apiErr  error
		)
		if stateful {
			success, apiErr = a.api.PostEventStatefulRecovery(apiCtx, producerName, eventID, requestID, nodeID)
		} else {
			success, apiErr = a.api.PostEventOddsRecovery(apiCtx, producerName, eventID, requestID, nodeID)
		}
		// Best-effort send back to the actor. If the inbox is full
		// (other events backing up) we still need the result delivered
		// so the handle can transition to terminal — use sendCtx with
		// a fallback to a.ctx so we don't leak this goroutine when
		// shutdown beats the actor draining its inbox.
		//
		// contextcheck: a.ctx is the correct ctx here, NOT apiCtx.
		// apiCtx is the per-request ctx that may have been cancelled
		// (by AfterFunc(a.ctx, apiCancel) on actor shutdown) — that's
		// exactly the cancellation we're trying to deliver via
		// evRecoverEventCompleted{err: apiErr}, so we must not use a
		// cancelled ctx for the delivery itself. The
		// actor lifetime ctx (a.ctx) is the right scope: it's only
		// cancelled when the actor is shutting down, in which case
		// we fall back to completing the handle directly below.
		if err := a.sendCtx(a.ctx, evRecoverEventCompleted{ //nolint:contextcheck // see comment above — using a.ctx is intentional for actor-inbox delivery
			requestID: requestID,
			success:   success,
			err:       apiErr,
		}); err != nil {
			// Actor has shut down: complete the handle directly so
			// callers blocked on Done() unblock. Preserve the
			// original API error if any — pre-fix `err` (the
			// shutdown ErrManagerClosed) replaced apiErr entirely
			// and the actual API failure was lost.
			cause := err
			if apiErr != nil {
				cause = errors.Join(apiErr, err)
			}
			a.mgr.completeHandle(requestID, types.RecoveryStatusFailed, cause)
		}
	}()
}

// onRecoverEventCompleted runs on the actor goroutine when the
// detached API goroutine reports its outcome. Mirrors the pre-restructure
// post-API tail of onRecoverEvent (delete-on-failure, log + complete on
// error) but executes single-threaded with the rest of the actor's state.
func (a *recoveryActor) onRecoverEventCompleted(e evRecoverEventCompleted) {
	if !e.success {
		delete(a.eventRecoveries, e.requestID)
	}
	if e.err != nil {
		a.logger.WithError(e.err).
			WithField("producer_id", a.producerID).
			WithField("request_id", e.requestID).
			Error("recovery: event recovery API failed")
		a.mgr.completeHandle(e.requestID, types.RecoveryStatusFailed,
			fmt.Errorf("recovery: producer %d req=%d event recovery: %w", a.producerID, e.requestID, e.err))
	}
	// success && err == nil: the recovery is in flight; the eventual
	// SnapshotComplete handler transitions the handle to terminal.
}

// --- State-machine helpers (preserved verbatim from manager.go) ---

func (a *recoveryActor) systemAliveReceived(timestamp types.MessageTimestamp, subscribed bool) error {
	if err := a.pm.SetProducerLastMessageTimestamp(a.producerID, timestamp.Received); err != nil {
		return err
	}

	recoveryTimestamp, err := a.timestampForRecovery()
	if err != nil {
		return err
	}

	if !subscribed {
		if !a.isFlaggedDown() {
			if err := a.producerDown(types.OtherProducerDownReason); err != nil {
				return err
			}
		}
		// Don't stomp an in-flight snapshot recovery: makeSnapshotRecovery
		// overwrites currentRecovery + resets recoveryState, which would
		// orphan any pending handle. If a recovery is already running,
		// transition it to Interrupted so snapshotRecoveryFinished
		// re-issues a fresh recovery on completion (mirrors Java/.NET).
		if a.isPerformingRecovery() {
			a.recoveryState = types.InterruptedRecoveryState
			return nil
		}
		return a.startSnapshotRecovery(recoveryTimestamp)
	}

	now := time.Now()
	state := a.recoveryState
	downReason := a.downReason
	isBackFromInactivity := a.isFlaggedDown() &&
		!a.isPerformingRecovery() &&
		downReason == types.ProcessingQueueDelayViolationProducerDownReason &&
		a.calculateTiming(now)
	isInRecovery := state != types.NotStartedRecoveryState &&
		state != types.ErrorRecoveryState &&
		state != types.InterruptedRecoveryState

	switch {
	case isBackFromInactivity:
		err = a.producerUp(types.ReturnedFromInactivityProducerUpReason)
	case isInRecovery:
		if a.isFlaggedDown() && !a.isPerformingRecovery() && a.downReason != types.ProcessingQueueDelayViolationProducerDownReason {
			if err := a.startSnapshotRecovery(recoveryTimestamp); err != nil {
				return err
			}
		}
		recoveryTiming := now.Sub(a.lastRecoveryStartedAt())
		maxInterval := a.cfg.MaxRecoveryExecution()
		if a.isPerformingRecovery() && recoveryTiming > maxInterval {
			a.recoveryState = types.ErrorRecoveryState
			a.currentRecovery = nil
			if err := a.startSnapshotRecovery(recoveryTimestamp); err != nil {
				return err
			}
		}
	default:
		err = a.startSnapshotRecovery(recoveryTimestamp)
	}
	if err != nil {
		return err
	}

	// Per-producer state mutation (was: data.systemAliveReceived).
	t := timestamp.Received
	a.lastSystemAlive = &t
	if a.recoveryState == types.StartedRecoveryState {
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

// calculateTiming reports whether message processing and user-session
// alives are both fresh enough (within MaxInactivity).
//
// Each half applies only once a baseline EXISTS (non-zero timestamp;
// same IsZero discipline as expireStuckSnapshotRecovery). This matters
// most for lastUserSessionAlive: it is written only by user-session
// alives (onAlive's non-SystemAliveOnly branch), and the internal
// alive session ensureNormal opens is SystemAliveOnly — so a client
// that never Subscribes (Connect-only, e.g. the recovery example) or
// subscribes with SystemAliveOnly never sets it. Pre-guard,
// now.Sub(zero) dwarfed MaxInactivity, so once the tick loop armed the
// inactivity check every tick flagged the producer down with
// ProcessingQueueDelayViolation — and PERMANENTLY: the only up-path
// for that reason (isBackFromInactivity) requires this predicate to
// turn true, which the zero field made impossible. A missing baseline
// therefore counts as fresh; monitoring of each half begins with its
// first observation. Genuine alive silence is still caught by the
// aliveInterval check in onTick, which fires before this one.
func (a *recoveryActor) calculateTiming(now time.Time) bool {
	maxInactivity := a.cfg.MaxInactivity()
	lastProcessed, err := a.lastProcessedMessageGenTimestamp()
	if err != nil {
		a.logger.WithError(err).Warn("failed to get last processed message gen timestamp")
		return false
	}
	processedFresh := lastProcessed.IsZero() ||
		now.Sub(lastProcessed).Abs() < maxInactivity
	userAliveFresh := a.lastUserSessionAlive.IsZero() ||
		now.Sub(a.lastUserSessionAlive).Abs() < maxInactivity
	return processedFresh && userAliveFresh
}

// producerDown matches manager.producerDown exactly. Pre-actor:
// data.setProducerDown crossed into producer.Manager (mutex-protected),
// then notifyProducerChangedState emitted on msgCh. The actor flow is
// the same — only the actor's own state mutates without locks.
func (a *recoveryActor) producerDown(reason types.ProducerDownReason) error {
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
		a.failAllEventRecoveries(fmt.Errorf("recovery: producer %d down (reason=%d)", a.producerID, reason))
	}

	if a.recoveryState == types.StartedRecoveryState && reason != types.ProcessingQueueDelayViolationProducerDownReason {
		// The interrupted recovery's replay is lost or about to be
		// superseded: whatever restarts it must reach back at least as
		// far as it did, so its cursor becomes a floor — a one-shot
		// explicit rewind it consumed, or an initial-snapshot window
		// measured from an earlier "now", is otherwise gone by the time
		// the restart recomputes its cursor.
		if a.currentRecovery != nil {
			a.lowerRecoveryFloor(a.currentRecovery.recoverFrom)
		}
		a.recoveryState = types.InterruptedRecoveryState
	}

	if !a.isFlaggedDown() {
		if err := a.pm.SetProducerDown(a.producerID, true); err != nil {
			return err
		}
		a.downReason = reason
		a.failAllEventRecoveries(fmt.Errorf("recovery: producer %d down (reason=%d)", a.producerID, reason))
	}

	return a.notifyProducerChangedState(reason.ToProducerStatusReason())
}

// failAllEventRecoveries fails every in-flight event-recovery handle and
// clears the per-actor map. Pre-fix, the producerDown path discarded the
// map without completing the handles — callers blocked on Handle.Done()
// hung until manager Close, and producer-flap loops accumulated
// permanently-pending handles.
//
// It also CANCELS each entry's detached recovery POST (cancelAPI). Pre-
// fix the POST goroutines kept running after the map was cleared, so
// they no longer counted against the per-producer admission cap
// (maxPendingEventRecoveries): a producer-down transition freed all 128
// map slots while 128 POSTs were still in flight, letting another 128
// pass admission — repeated down-reason changes / flap cycles then
// accumulated HTTP calls and goroutines until API timeout. Cancelling
// the POST context (mirrors the expiry sweep) terminates the request
// before the next batch can overlap it.
func (a *recoveryActor) failAllEventRecoveries(err error) {
	for id, er := range a.eventRecoveries {
		if er != nil && er.cancelAPI != nil {
			er.cancelAPI()
		}
		a.mgr.completeHandle(id, types.RecoveryStatusFailed, err)
	}
	a.eventRecoveries = make(map[int]*eventRecovery)
}

func (a *recoveryActor) producerUp(reason types.ProducerUpReason) error {
	if a.isDisabled() {
		return nil
	}
	if a.isFlaggedDown() {
		if err := a.pm.SetProducerDown(a.producerID, false); err != nil {
			return err
		}
		a.downReason = types.DefaultProducerDownReason
	}
	return a.notifyProducerChangedState(reason.ToProducerStatusReason())
}

func (a *recoveryActor) notifyProducerChangedState(reason types.ProducerStatusReason) error {
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
	impl := &producerStatusImpl{
		producer: producerData,
		timestamp: types.MessageTimestamp{
			Created: now, Sent: now, Received: now, Published: now,
		},
		isDown:               a.isFlaggedDown(),
		isDelayed:            delayed,
		producerStatusReason: reason,
	}
	// Snapshot for Client.ProducerStatus(producerID) — survives even
	// when the lossy RecoveryEvents channel drops the corresponding
	// event.
	a.statusSnapshot.Store(impl)
	a.mgr.emitRecoveryMessage(types.RecoveryMessage{ProducerStatus: impl})
	return nil
}

// currentStatus returns the most recent ProducerStatus this actor
// emitted, or nil if none has been emitted yet. Safe to call from any
// goroutine.
func (a *recoveryActor) currentStatus() types.ProducerStatus {
	if s := a.statusSnapshot.Load(); s != nil {
		return s
	}
	return nil
}

// makeSnapshotRecovery initiates a snapshot recovery for this producer.
//
// State mutation (currentRecovery + recoveryState=Started) and the
// request-id allocation happen inline on the actor goroutine — fast,
// single-threaded. The actual PostRecovery HTTP call runs in a
// detached goroutine bounded by the actor lifetime ctx; its outcome
// feeds back as evSnapshotRecoveryAPICompleted, where
// SetProducerRecoveryInfo runs single-threaded.
//
// Pre-fix the PostRecovery call ran inline (~30s with retries),
// blocking the actor for the duration. Every alive/tick/snapshot-
// complete/recover-event for this producer queued behind it; under
// load the 256-slot inbox dropped alives and triggered false
// producer-down. Mirrors the v2.24 detach-event-recovery restructure.
// startSnapshotRecovery is the gate in front of makeSnapshotRecovery: a
// snapshot recovery must not be requested while a lost session in this
// producer's scope has no queue yet — the replay would be published
// into nothing, and its snapshot_complete with it (a recovery stalled
// until MaxRecoveryExecution, or completed with its head lost). The
// request is deferred instead and issued by resumeDeferredRecovery once
// the rebind is reported; the producer stays flagged down meanwhile and
// the recovery floor keeps the eventual request reaching back to the
// loss.
func (a *recoveryActor) startSnapshotRecovery(timestamp time.Time) error {
	if a.mgr.rebindPending(a.producerID) {
		if !a.deferredRecovery {
			a.logger.WithField("producer_id", a.producerID).
				Info("recovery: snapshot recovery deferred until the lost subscription queue is re-bound")
		}
		a.deferredRecovery = true
		return nil
	}
	a.deferredRecovery = false
	return a.makeSnapshotRecovery(timestamp)
}

// resumeDeferredRecovery issues a snapshot recovery that startSnapshotRecovery
// deferred, once no lost session in scope is pending any more. The
// cursor is recomputed: the floor makes it reach back to the loss.
func (a *recoveryActor) resumeDeferredRecovery() {
	if !a.deferredRecovery || a.mgr.rebindPending(a.producerID) {
		return
	}
	ts, err := a.timestampForRecovery()
	if err != nil {
		a.logger.WithError(err).WithField("producer_id", a.producerID).Warn("recovery: resume deferred snapshot recovery: recovery timestamp")
		return
	}
	if err := a.startSnapshotRecovery(ts); err != nil {
		a.logger.WithError(err).WithField("producer_id", a.producerID).Error("recovery: resume deferred snapshot recovery")
	}
}

func (a *recoveryActor) makeSnapshotRecovery(timestamp time.Time) error {
	now := time.Now()
	recoverFrom := timestamp
	// The floor wins over a later cursor: a channel loss (or a recovery
	// it interrupted) must be reached back to even if alives processed
	// since moved the producer's cursor forward. A zero cursor means
	// "everything the producer has" and is left alone — the floor only
	// ever pulls a later cursor back.
	if !a.recoveryFloor.IsZero() && !recoverFrom.IsZero() && a.recoveryFloor.Before(recoverFrom) {
		recoverFrom = a.recoveryFloor
	}
	if !recoverFrom.IsZero() {
		maxRecovery := a.cfg.MaxRecoveryExecution()
		if now.Sub(recoverFrom) > maxRecovery {
			recoverFrom = now.Add(-maxRecovery)
		}
		// Defensive clamp (the public setter rejects future cursors, but
		// message-timestamp-derived cursors flow in unvalidated): asking
		// the recovery service to start AFTER a future instant silently
		// omits every currently-missing message.
		if recoverFrom.After(now) {
			recoverFrom = now
		}
	} else if a.initialSnapshotTime > 0 {
		// No recovery cursor for this producer (first snapshot after
		// connect, or post-error reset before any message landed):
		// look back the configured initial-snapshot window instead of
		// requesting the producer's full history. Wires
		// WithInitialSnapshotTime (Java/.NET parity), which was
		// previously stored on Config but never read.
		recoverFrom = now.Add(-a.initialSnapshotTime)
	}

	requestID := a.mgr.nextRequestID()
	producerName, err := a.producerName()
	if err != nil {
		return fmt.Errorf("recovery: producer %d snapshot req=%d: producer name: %w", a.producerID, requestID, err)
	}

	// A superseded snapshot request must not keep retrying its POST
	// alongside the replacement (a flapping channel would otherwise stack
	// one detached retry loop per loss); event recoveries already carry
	// this cancel.
	if a.currentRecovery != nil && a.currentRecovery.cancelAPI != nil {
		a.currentRecovery.cancelAPI()
	}
	apiCtx, cancelAPI := context.WithCancel(a.ctx)
	a.currentRecovery = newRecoveryData(requestID, now)
	a.currentRecovery.recoverFrom = recoverFrom
	a.currentRecovery.cancelAPI = cancelAPI
	a.recoveryState = types.StartedRecoveryState

	a.logger.WithField("producer_id", a.producerID).WithField("request_id", requestID).WithField("recover_from", recoverFrom).Info("recovery: snapshot recovery started")

	// Detach the API call from the actor goroutine. Use a.ctx for the
	// request so a manager Close cancels in-flight recoveries; the
	// inbox-send fallback uses sendCtx with a.ctx so a shutdown that
	// races the API result still delivers (or returns ErrManagerClosed
	// quickly, in which case we do nothing — currentRecovery survives
	// in the actor's state and the snapshot-complete event handler
	// will reconcile when the actor next runs).
	a.detached.Add(1) // joined (bounded) by stopBounded
	go func() {
		defer a.detached.Done()
		defer cancelAPI()
		success, apiErr := a.api.PostRecovery(apiCtx, producerName, requestID, a.cfg.SdkNodeID(), recoverFrom)
		// Inbox delivery deliberately uses a.ctx, not apiCtx: a superseded
		// request's cancel must not also drop its (now late) result on
		// the floor — the actor still wants to log/ignore it by request
		// id, and only actor shutdown may cancel the delivery.
		//nolint:contextcheck // see above
		// a.ctx is the right context for inbox delivery; it's only
		// cancelled at actor shutdown, in which case sendCtx returns
		// ErrManagerClosed. Log the drop so an operator correlating
		// a hung handle to a shutdown race has a breadcrumb.
		if err := a.sendCtx(a.ctx, evSnapshotRecoveryAPICompleted{
			requestID:    requestID,
			recoverFrom:  recoverFrom,
			startedAt:    now,
			success:      success,
			err:          apiErr,
			producerName: producerName,
		}); err != nil {
			a.logger.WithError(err).
				WithField("producer_id", a.producerID).
				WithField("request_id", requestID).
				Warn("recovery: snapshot recovery API result lost (actor shutdown raced API completion)")
		}
	}()
	return nil
}

// onSnapshotRecoveryAPICompleted runs on the actor goroutine when the
// detached PostRecovery call returns. Mirrors the post-API tail of the
// pre-restructure makeSnapshotRecovery: persist recovery info, log
// errors. On API failure, transitions to ErrorRecoveryState and
// clears currentRecovery — without this rollback, the actor would
// stay in StartedRecoveryState forever waiting for a snapshot_complete
// that never arrives (the recovery never started on the upstream side).
// The next tick / alive will then be free to re-issue.
func (a *recoveryActor) onSnapshotRecoveryAPICompleted(e evSnapshotRecoveryAPICompleted) {
	// Stale-event guard: if currentRecovery has rotated (e.g.,
	// makeSnapshotRecovery was called again with a fresh requestID
	// between our send and arrival), this completion no longer
	// matches what's in flight. Drop silently — the state machine
	// is already tracking the newer recovery.
	if a.currentRecovery == nil || a.currentRecovery.recoveryID != e.requestID {
		return
	}
	if e.err != nil {
		// Rollback applies only while the recovery is still in flight.
		// PostRecovery is idempotent, so a retried call can lose the
		// race against the AMQP snapshot the server already accepted
		// for attempt 1: snapshot_complete lands, snapshotRecoveryFinished
		// re-creates currentRecovery with the SAME requestID and sets
		// CompletedRecoveryState, and only then does the detached call
		// return its (late, now meaningless) transport error. Rolling
		// back here would flip a healthy completed producer to Error
		// and force a redundant full re-recovery on the next alive.
		if a.recoveryState != types.StartedRecoveryState && a.recoveryState != types.InterruptedRecoveryState {
			a.logger.WithError(e.err).
				WithField("producer_id", a.producerID).
				WithField("request_id", e.requestID).
				WithField("recovery_state", a.recoveryState).
				Warn("recovery: late PostRecovery API error after recovery already settled; dropping")
			return
		}
		a.logger.WithError(e.err).
			WithField("producer_id", a.producerID).
			WithField("request_id", e.requestID).
			Error("recovery: PostRecovery API failed; transitioning to Error")
		a.currentRecovery = nil
		a.recoveryState = types.ErrorRecoveryState
		return
	}
	recoveryInfo := newRecoveryInfoImpl(e.recoverFrom, e.startedAt, e.requestID, e.success, a.cfg.SdkNodeID())
	if err := a.pm.SetProducerRecoveryInfo(a.producerID, recoveryInfo); err != nil {
		a.logger.WithError(err).
			WithField("producer_id", a.producerID).
			WithField("request_id", e.requestID).
			Error("recovery: SetProducerRecoveryInfo failed after snapshot recovery API")
	}
}

func (a *recoveryActor) snapshotRecoveryFinished(requestID int) error {
	started := a.lastRecoveryStartedAt()
	if started.IsZero() {
		return fmt.Errorf("recovery: producer %d req=%d: inconsistent recovery state (currentRecovery=%v, recoveryState=%v)", a.producerID, requestID, a.currentRecovery, a.recoveryState)
	}
	finished := time.Now()
	a.logger.WithField("producer_id", a.producerID).WithField("request_id", requestID).WithField("elapsed_ms", finished.Sub(started).Milliseconds()).Info("recovery: snapshot recovery finished")

	// Interrupted re-issue: an alive with subscribed=false arrived
	// while the just-completed recovery was in flight (see H1 fix in
	// systemAliveReceived). The original recovery is done on the wire,
	// but we need a fresh snapshot from the post-interruption point.
	// makeSnapshotRecovery sets currentRecovery to the NEW recovery
	// and recoveryState to Started; we MUST return early here so the
	// completion-path overwrites below don't clobber the freshly-issued
	// recovery's state. Producer stays "down" (no producerUp emit) —
	// the new snapshot's snapshot_complete will drive the next
	// completion when it arrives.
	//
	// Pre-fix this function continued past the if-block, overwriting
	// currentRecovery to the OLD requestID and recoveryState to
	// Completed, then emitted producerUp. The re-issued recovery's
	// snapshot_complete would then arrive with a requestID that no
	// longer matched currentRecovery.recoveryID and be silently
	// ignored — the new recovery hung forever.
	if a.recoveryState == types.InterruptedRecoveryState {
		return a.startSnapshotRecovery(a.lastValidAliveGen)
	}

	var reason types.ProducerUpReason
	if a.firstRecoveryCompleted {
		reason = types.ReturnedFromInactivityProducerUpReason
	} else {
		reason = types.FirstRecoveryCompletedProducerUpReason
		a.firstRecoveryCompleted = true
	}

	// The floor is discharged only by a recovery that reached back to it
	// — a zero cursor means "everything the producer has" and covers any
	// floor. A recovery that started from a later cursor (a loss whose
	// reaction is still pending lowered the floor after this one began)
	// leaves the floor in place for the recovery that follows.
	if a.currentRecovery != nil && !a.recoveryFloor.IsZero() &&
		(a.currentRecovery.recoverFrom.IsZero() || !a.currentRecovery.recoverFrom.After(a.recoveryFloor)) {
		a.recoveryFloor = time.Time{}
	}
	a.currentRecovery = newRecoveryData(requestID, started)
	a.recoveryState = types.CompletedRecoveryState
	return a.producerUp(reason)
}

func (a *recoveryActor) eventRecoveryFinished(id int) error {
	er, ok := a.eventRecoveries[id]
	if !ok {
		return fmt.Errorf("recovery: producer %d req=%d: event recovery state missing", a.producerID, id)
	}
	started := er.recoveryStartedAt
	finished := time.Now()
	a.logger.WithField("producer_id", a.producerID).
		WithField("request_id", id).
		WithField("event_id", er.eventID.ToString()).
		WithField("elapsed_ms", finished.Sub(started).Milliseconds()).
		Info("recovery: event recovery finished")

	producerData, err := a.pm.GetProducer(a.ctx, a.producerID)
	if err != nil {
		return fmt.Errorf("recovery: producer %d req=%d event recovery finished: GetProducer: %w", a.producerID, id, err)
	}
	a.mgr.emitRecoveryMessage(types.RecoveryMessage{
		EventRecoveryMessage: &eventRecoveryMessageImpl{
			eventID:   er.eventID,
			requestID: id,
			producer:  producerData,
			timestamp: types.MessageTimestamp{
				Created: finished, Sent: finished, Received: finished, Published: finished,
			},
		},
	})

	// Reliable per-request completion (NEXT.md §11): even if the
	// channel send above is dropped (lossy + slow consumer), the
	// handle is updated and unblocks any caller blocked on Done().
	a.mgr.completeHandle(id, types.RecoveryStatusCompleted, nil)

	delete(a.eventRecoveries, id)
	return nil
}
