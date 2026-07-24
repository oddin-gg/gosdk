package gosdk

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/oddin-gg/gosdk/internal/cache"
	"github.com/oddin-gg/gosdk/internal/factory"
	"github.com/oddin-gg/gosdk/internal/feed"
	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/internal/producer"
	"github.com/oddin-gg/gosdk/internal/utils"
	"github.com/oddin-gg/gosdk/types"
)

// errFeedMessageDecode is the cause reported to a Subscription terminated
// under StrategyThrow when the in-band failure was an XML/decode error
// surfaced by the channel consumer (the underlying parser's error stays
// in the logs).
var errFeedMessageDecode = errors.New("gosdk: feed message decode failed")

// sdkOddsFeedSession is the internal interface the legacy session impl
// satisfies. It used to embed a public types.OddsFeedSession; that
// public interface was retired alongside the manager-of-managers shape.
// sessionEnvelope pairs a public SessionMessage with the broker ack of
// the AMQP delivery it was built from. The ack is non-nil only on the
// FINAL envelope produced for a delivery (an extended-data RawFeedMessage
// preceding a parsed message rides with a nil ack); the subscription pump
// fires it after the message lands in the public buffer — completing the
// NEXT.md §0.6 contract that deliveries are acked only after admission to
// the subscription buffer.
type sessionEnvelope struct {
	msg types.SessionMessage
	ack func()
}

// runAck fires a possibly-nil ack closure. Session/pump code paths call
// this at every point that terminally handles a delivery.
func runAck(ack func()) {
	if ack != nil {
		ack()
	}
}

type sdkOddsFeedSession interface {
	ID() uuid.UUID
	RespCh() <-chan sessionEnvelope
	Open(
		ctx context.Context,
		routingKeys []string,
		messageInterest *types.MessageInterest,
		reportExtendedData bool,
	) error
	Close(ctx context.Context)
	// CloseGraceful stops AMQP intake, lets the in-flight delivery
	// finish its decode+admit+ack cycle, and drains already-admitted
	// messages through to RespCh before closing it. ctx is the drain
	// deadline; on expiry it falls back to the abrupt path. Shares
	// idempotence with Close — whichever runs first wins.
	//
	// Returns whether the drain FULLY completed: every delivery the
	// feed layer handed downstream reached its terminal disposition
	// (acked on public-buffer admission, drop-acked, or nacked) and the
	// session loop exited, all within ctx. false means at least one
	// delivery was abandoned undelivered — teardown of the exclusive
	// autoDelete queue forfeits its redelivery — so the caller must
	// report the drain deadline, never a clean close. An empty public
	// buffer is NOT sufficient proof of a clean drain.
	CloseGraceful(ctx context.Context) bool
	IsReplay() bool
	// Err returns a terminal error captured by the session — non-nil only
	// when the session terminated under StrategyThrow due to a build/decode
	// failure. nil for graceful close or under StrategyCatch.
	Err() error
}

// cacheNotifier is the slice of *cache.Manager that the session uses to
// auto-invalidate cached fixture/match/tournament/status entries when
// matching feed messages arrive. Extracted as an interface so the
// replay-isolation gate (see processFeedMessage) is unit-testable
// without spinning up a full cache.Manager.
type cacheNotifier interface {
	OnFeedMessageReceived(feedMessage *types.FeedMessage)
}

// messageBuilder is the slice of *factory.FeedMessageFactory the session
// uses to translate raw feed messages into typed public messages.
// Extracted as an interface so processFeedMessage's recovery-cursor
// dispatch is unit-testable with canned messages.
type messageBuilder interface {
	BuildMessage(ctx context.Context, feedMessage *types.FeedMessage) (any, error)
	BuildUnparsableMessage(ctx context.Context, feedMessage *types.FeedMessage) types.UnparsableMessage
}

// recoveryMessageProcessor is the session→recovery seam: the hooks the
// message pump drives on the recovery manager (a *recovery.Manager in
// production, a dummy for replay sessions). Internal wiring only — no
// consumer implements or receives it, which is why it lives unexported
// here rather than in the public types/ package (v1.0.0 surface pass).
type recoveryMessageProcessor interface {
	OnMessageProcessingStarted(sessionID uuid.UUID, producerID int, timestamp time.Time)
	OnMessageProcessingEnded(sessionID uuid.UUID, producerID int, timestamp time.Time)
	OnAliveReceived(producerID int, timestamp types.MessageTimestamp, isSubscribed bool, messageInterest types.MessageInterest)
	// OnSnapshotCompleteReceived admits a snapshot-complete to the
	// producer's recovery actor. Unlike the lossy observability events
	// above, this is a CORRECTNESS event — a lost snapshot-complete
	// strands recovery until MaxRecoveryExecution — so admission is
	// ctx-bounded backpressure rather than best-effort. It returns a
	// non-nil error when the completion could NOT be admitted (ctx
	// cancelled or the recovery manager is shutting down); the caller
	// must then leave the AMQP delivery UNACKED so the broker redelivers.
	// A nil error means the transition was admitted and the delivery may
	// be acked.
	OnSnapshotCompleteReceived(ctx context.Context, producerID int, requestID int, messageInterest types.MessageInterest) error
}

type oddsFeedSessionImpl struct {
	channelConsumer          *feed.ChannelConsumer
	producerManager          *producer.Manager
	cacheManager             cacheNotifier
	feedMessageFactory       messageBuilder
	recoveryMessageProcessor recoveryMessageProcessor
	exchangeName             string
	sportIDPrefix            string
	sessionID                uuid.UUID
	logger                   *log.Logger
	exceptionStrategy        ExceptionStrategy

	closeFn   context.CancelFunc
	msgCh     chan sessionEnvelope
	done      chan struct{} // closed by goroutine after it exits
	closeOnce sync.Once
	isReplay  bool
	// gracefulSettled records CloseGraceful's result; written once
	// inside closeOnce, read by every CloseGraceful caller (the Once
	// provides the happens-before). Stays false when the abrupt Close
	// consumed the Once first — an abrupt close is never a settled drain.
	gracefulSettled bool

	errMu sync.RWMutex
	err   error
}

func (o *oddsFeedSessionImpl) RespCh() <-chan sessionEnvelope {
	return o.msgCh
}

func (o *oddsFeedSessionImpl) IsReplay() bool {
	return o.isReplay
}

func (o *oddsFeedSessionImpl) Err() error {
	o.errMu.RLock()
	defer o.errMu.RUnlock()
	return o.err
}

func (o *oddsFeedSessionImpl) setErr(err error) {
	o.errMu.Lock()
	if o.err == nil {
		o.err = err
	}
	o.errMu.Unlock()
}

func (o *oddsFeedSessionImpl) Open(
	ctx context.Context,
	routingKeys []string,
	messageInterest *types.MessageInterest,
	reportExtendedData bool) error {
	if o.closeFn != nil {
		return errors.New("gosdk: session: open called twice")
	}

	ch, err := o.channelConsumer.Open(ctx, routingKeys, messageInterest)
	if err != nil {
		return fmt.Errorf("gosdk: session: channel consumer open (interest=%s, keys=%d): %w", *messageInterest, len(routingKeys), err)
	}

	// Loop ctx must outlive the caller's Open ctx (which only bounds the
	// consumer's queue declaration). WithoutCancel propagates caller
	// metadata while severing the cancellation chain; closeFn cancels at
	// Close() time.
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	o.closeFn = cancel
	o.done = make(chan struct{})

	go func(messageInterest *types.MessageInterest) {
		// The goroutine owns msgCh: it's the sole sender and the sole
		// closer. Close() signals exit via loopCtx; this defer guarantees
		// msgCh is closed exactly once after the last send completes.
		defer func() {
			close(o.msgCh)
			close(o.done)
		}()
		for {
			// Non-blocking cancellation pre-check: the select below picks
			// uniformly among READY arms, so after StrategyThrow's
			// terminal error (emitUnparsable cancels loopCtx and returns)
			// a continuously-ready delivery channel could keep winning —
			// the session would keep processing and ACKING deliveries
			// AFTER its terminal error. Checking ctx first makes the
			// Throw exit deterministic: at most the envelope already in
			// flight when the error fired is processed, never a
			// successor.
			if loopCtx.Err() != nil {
				return
			}
			select {
			case <-loopCtx.Done():
				return
			case env, ok := <-ch:
				if !ok {
					// Channel consumer closed its outgoing channel — final
					// shutdown signal. Exit; defer closes msgCh + done.
					return
				}
				o.processMessage(loopCtx, env, messageInterest, reportExtendedData)
			}
		}
	}(messageInterest)

	return nil
}

// Close tears down the session in the correct order to avoid
// send-on-closed-channel and nil-deref panics:
//  1. Cancel loopCtx — signals the goroutine to exit; in-flight sends
//     observe ctx.Done() and bail.
//  2. Wait for the goroutine to confirm exit (bounded by ctx).
//  3. Close the channel consumer (idempotent; safe regardless).
//
// NOTE: cache manager lifecycle is owned by *Client* (one cache shared
// across every session). Sessions deliberately do NOT close it here —
// closing would tear down the cache for every other in-flight
// subscription on the same Client.
//
// The goroutine itself owns msgCh and closes it on exit — never Close().
// Idempotent via sync.Once.
func (o *oddsFeedSessionImpl) Close(ctx context.Context) {
	o.closeOnce.Do(func() {
		if o.closeFn != nil {
			o.closeFn()
		}
		if o.done != nil {
			select {
			case <-o.done:
			case <-ctx.Done():
				// ctx expired; goroutine still draining. Continue with
				// downstream close — channelConsumer.Close also bounds
				// itself by ctx.
			}
		}
		_ = o.channelConsumer.Close(ctx)
		// NOTE: cacheManager is OWNED BY *Client* and shared across
		// every session. Do NOT close it here — Subscription.Close
		// would otherwise tear the cache down for the whole client
		// (and every other in-flight subscription). Cache lifecycle
		// is solely Client.Close's responsibility.
	})
}

// CloseGraceful is the drain-first variant of Close (NEXT.md §8
// Subscriptions, graceful path). Ordering:
//  1. Gracefully close the channel consumer — intake stops, the
//     in-flight delivery completes decode + admit + ack, and the
//     consumer's outgoing channel closes once every admitted message
//     was handed to this session's loop.
//  2. Wait (bounded by ctx) for the loop goroutine to pump the
//     remaining consumer messages through msgCh and exit via its
//     channel-closed branch — it closes msgCh + done on the way out.
//  3. Release the loop ctx: pure hygiene after a completed drain; on
//     deadline expiry it aborts the in-flight send so the goroutine
//     can't stay blocked forever.
func (o *oddsFeedSessionImpl) CloseGraceful(ctx context.Context) bool {
	o.closeOnce.Do(func() {
		settled := o.channelConsumer.CloseGraceful(ctx)
		loopDrained := true
		if o.done != nil {
			select {
			case <-o.done:
			case <-ctx.Done():
				loopDrained = false
			}
		}
		if o.closeFn != nil {
			o.closeFn()
		}
		// Settled only when the feed layer saw every delivery reach a
		// terminal disposition AND the loop pumped everything through.
		// A loop that missed the deadline still holds envelopes in
		// msgCh whose acks will never fire — that is a discard, not a
		// drain, and the caller must report it as such.
		o.gracefulSettled = settled && loopDrained
	})
	return o.gracefulSettled
}

func (o *oddsFeedSessionImpl) ID() uuid.UUID {
	return o.sessionID
}

// processMessage terminally handles ONE AMQP delivery: exactly one of the
// following happens to env.Ack — it rides the final sessionEnvelope sent
// to msgCh (the pump acks after public-buffer admission), it fires here
// at an intentional-drop point (alive/snapshot handling, disabled or
// out-of-scope producer, nil payload), or it is deliberately NOT fired
// (StrategyThrow termination, ctx-cancelled send) so the broker
// redelivers/releases the unacked delivery.
func (o *oddsFeedSessionImpl) processMessage(ctx context.Context, env feed.QueueEnvelope, messageInterest *types.MessageInterest, reportExtendedData bool) {
	msg := env.Msg
	// Defensive: msg should never be nil here (loop already filtered ok),
	// but a future channel-shape change shouldn't panic the goroutine.
	// Nothing to deliver == intentional drop: ack.
	if msg == nil {
		runAck(env.Ack)
		return
	}

	emitRaw := reportExtendedData && msg.RawFeedMessage != nil

	if msg.UnparsableMessage != nil {
		o.emitUnparsable(ctx, msg.UnparsableMessage, errFeedMessageDecode, env.Ack, emitRaw, msg.RawFeedMessage)
		return
	}

	if msg.FeedMessage == nil {
		// No parsed message will be built, so there is no shared-state
		// race with a downstream parse. The raw side-channel, if any, is
		// the delivery's only output and carries the ack.
		o.dropDelivery(ctx, emitRaw, msg.RawFeedMessage, env.Ack)
		return
	}

	// FeedMessage != nil: the raw and parsed envelopes share the decoded
	// message, the routing-key pointer, and the byte slice — all publicly
	// mutable. So the raw side-channel is NOT published here: publishing
	// it before the SDK finishes reading that shared state (producer
	// checks, cache invalidation, OnAlive/OnSnapshot, BuildMessage) would
	// let a consumer mutate it mid-parse and corrupt the built event.
	// Instead each disposition below publishes the raw only AFTER its
	// reads complete and BEFORE the envelope that carries the ack (via
	// sendRawSideChannel / dropDelivery / emitUnparsable) — so, on the
	// FIFO o.msgCh, the raw is admitted to the public buffer before the
	// ack fires: never mutated mid-parse, never acked-then-lost.
	producerID := msg.FeedMessage.Message.Product()
	// Producers map is populated at SDK startup; these are in-memory
	// cache reads after init, but ctx is still propagated so any future
	// I/O fallback or instrumentation hooks observe the loop ctx.
	//
	// On any producer-lookup failure, we MUST stop processing this
	// message — pre-fix, the session continued with a zero-value
	// producerData, which silently mis-filtered messages under the
	// scope check below. The error is also surfaced via the
	// UnparsableMessage envelope so the consumer can react (Throw
	// strategy will set Subscription.Err; Catch strategy delivers
	// the unparsable as a normal message).
	producerData, err := o.producerManager.GetProducer(ctx, producerID)
	if err != nil {
		o.logger.WithError(err).WithField("producer_id", producerID).Error("session: get producer failed; dropping message")
		unparsableMsg := o.feedMessageFactory.BuildUnparsableMessage(ctx, msg.FeedMessage)
		o.emitUnparsable(ctx, unparsableMsg, fmt.Errorf("gosdk: session: get producer %d: %w", producerID, err), env.Ack, emitRaw, msg.RawFeedMessage)
		return
	}

	isProducerEnabled, err := o.producerManager.IsProducerEnabled(ctx, producerID)
	switch {
	case err != nil:
		o.logger.WithError(err).WithField("producer_id", producerID).Error("session: is-producer-enabled failed; dropping message")
		unparsableMsg := o.feedMessageFactory.BuildUnparsableMessage(ctx, msg.FeedMessage)
		o.emitUnparsable(ctx, unparsableMsg, fmt.Errorf("gosdk: session: is producer enabled %d: %w", producerID, err), env.Ack, emitRaw, msg.RawFeedMessage)
		return
	case !isProducerEnabled:
		o.dropDelivery(ctx, emitRaw, msg.RawFeedMessage, env.Ack)
		return
	case !messageInterest.IsProducerInScope(producerData):
		o.dropDelivery(ctx, emitRaw, msg.RawFeedMessage, env.Ack)
		return
	}

	o.processFeedMessage(ctx, msg.FeedMessage, *messageInterest, env.Ack, emitRaw, msg.RawFeedMessage)
}

func (o *oddsFeedSessionImpl) processFeedMessage(ctx context.Context, feedMessage *types.FeedMessage, messageInterest types.MessageInterest, ack func(), emitRaw bool, rawMsg *types.RawFeedMessage) {
	producerID := feedMessage.Message.Product()
	o.recoveryMessageProcessor.OnMessageProcessingStarted(o.sessionID, producerID, time.Now())

	// Pair every OnMessageProcessingStarted with exactly one
	// OnMessageProcessingEnded. Pre-v2.24 the BuildMessage failure
	// path and the unrecognized-built-type default branch returned
	// without calling End — the recovery manager tracked active
	// processing per session and never cleared those entries, so
	// each failed message leaked one stale "in-flight" record.
	// endProcessing is idempotent so success paths still call it
	// explicitly with the real message-gen timestamp; the deferred
	// fallback fires only when none of the success branches did,
	// using time.Time{} (no cursor advance — same convention the
	// SnapshotComplete branch uses).
	processingEnded := false
	endProcessing := func(messageGenTs time.Time) {
		if processingEnded {
			return
		}
		processingEnded = true
		o.recoveryMessageProcessor.OnMessageProcessingEnded(o.sessionID, producerID, messageGenTs)
	}
	defer endProcessing(time.Time{})

	// Replay sessions share the live Client's cache manager (one cache
	// per Client across all sessions, by design — see Client.Replay()).
	// We deliberately skip cache invalidation for replay traffic so a
	// historical fixture_change / bet_settlement / odds_change can't
	// invalidate or mutate live cache entries that real-time consumers
	// rely on. Mirrors the .NET ReplayFeed isolation pattern. Replay
	// consumers still receive every message via msgCh — only the
	// cache-side bookkeeping is gated.
	if !o.isReplay {
		o.cacheManager.OnFeedMessageReceived(feedMessage)
	}

	switch msg := feedMessage.Message.(type) {
	case *feedXML.Alive:
		o.recoveryMessageProcessor.OnAliveReceived(producerID, feedMessage.Timestamp, msg.Subscribed != nil && *msg.Subscribed == 1, messageInterest)
		endProcessing(feedMessage.Timestamp.Created)
		// Terminal handling: consumed by the recovery machinery, never
		// forwarded — intentional drop. The reads of feedMessage are done,
		// so the raw side-channel (if any) can now carry the ack.
		o.dropDelivery(ctx, emitRaw, rawMsg, ack)
		return
	case *feedXML.SnapshotComplete:
		// snapshot_complete is a correctness event: admit it to the
		// recovery actor with backpressure and ack ONLY if admitted. A
		// dropped completion strands recovery until MaxRecoveryExecution
		// (default 6h), so if admission fails (ctx cancelled / recovery
		// manager shutting down) we leave the delivery unacked and let
		// the broker redeliver.
		if err := o.recoveryMessageProcessor.OnSnapshotCompleteReceived(ctx, producerID, msg.RequestID, messageInterest); err != nil {
			o.logger.WithError(err).
				WithField("producer_id", producerID).
				WithField("request_id", msg.RequestID).
				Warn("session: snapshot_complete not admitted to recovery; leaving delivery unacked for redelivery")
			endProcessing(time.Time{})
			return
		}
		endProcessing(time.Time{})
		// Reads of feedMessage are done; raw side-channel may carry the ack.
		o.dropDelivery(ctx, emitRaw, rawMsg, ack)
		return
	}

	message, err := o.feedMessageFactory.BuildMessage(ctx, feedMessage)
	if err != nil {
		// Bounded diagnostics only: %v of the FeedMessage expanded the
		// embedded RawMessage byte slice wholesale (unbounded, upstream-
		// controlled). Log route + bounded payload preview instead.
		route := ""
		if feedMessage.RoutingKey != nil {
			route = feedMessage.RoutingKey.FullRoutingKey
		}
		o.logger.WithError(err).Errorf("failed to build message from feed message route %s %s", route, utils.PayloadPreview(feedMessage.RawMessage))
		unparsableMsg := o.feedMessageFactory.BuildUnparsableMessage(ctx, feedMessage)
		o.emitUnparsable(ctx, unparsableMsg, fmt.Errorf("gosdk: build message: %w", err), ack, emitRaw, rawMsg)
		return // deferred endProcessing fires
	}

	// BuildMessage is the SDK's last read of the shared decoded message —
	// the built message (and any unparsable below) is an independent value.
	// Publish the raw side-channel now (nil ack) so it is enqueued on the
	// FIFO o.msgCh BEFORE the parsed/unparsable envelope that carries the
	// ack; the ack, fired on that later envelope's admission, can never
	// precede the raw. Mark it consumed so no disposition below re-emits.
	// If raw admission FAILED (ctx cancelled mid-shutdown), abort without
	// sending the ack-bearing parsed envelope — a racing receiver could
	// still admit it and ack a delivery whose required raw was dropped.
	// Unacked, the broker redelivers the whole delivery.
	if emitRaw {
		if !o.sendRawSideChannel(ctx, rawMsg) {
			return // deferred endProcessing fires; delivery stays unacked
		}
		emitRaw = false
	}

	// Recovery cursor: advance from the upstream gen-timestamp so any
	// successfully-built message updates LastProcessedMessageGenTimestamp
	// for this producer. Pre-fix only OddsChange/BetStop carried a
	// non-zero timestamp through (the actor ignores zero), so a stream
	// dominated by BetCancel / BetSettlement / FixtureChange / Rollback*
	// could leave the cursor stale and drive false producer-down /
	// processing-queue-delay decisions.
	var admitted bool
	switch msg := message.(type) {
	case types.OddsChange:
		admitted = o.send(ctx, sessionEnvelope{msg: types.SessionMessage{EventMessage: types.EventMessage{OddsChange: msg}}, ack: ack})
	case types.BetStop:
		admitted = o.send(ctx, sessionEnvelope{msg: types.SessionMessage{EventMessage: types.EventMessage{BetStop: msg}}, ack: ack})
	case types.BetCancel:
		admitted = o.send(ctx, sessionEnvelope{msg: types.SessionMessage{EventMessage: types.EventMessage{BetCancel: msg}}, ack: ack})
	case types.BetSettlement:
		admitted = o.send(ctx, sessionEnvelope{msg: types.SessionMessage{EventMessage: types.EventMessage{BetSettlement: msg}}, ack: ack})
	case types.FixtureChangeMessage:
		admitted = o.send(ctx, sessionEnvelope{msg: types.SessionMessage{EventMessage: types.EventMessage{FixtureChange: msg}}, ack: ack})
	case types.RollbackBetSettlement:
		admitted = o.send(ctx, sessionEnvelope{msg: types.SessionMessage{EventMessage: types.EventMessage{RollbackBetSettlement: msg}}, ack: ack})
	case types.RollbackBetCancel:
		admitted = o.send(ctx, sessionEnvelope{msg: types.SessionMessage{EventMessage: types.EventMessage{RollbackBetCancel: msg}}, ack: ack})
	default:
		// Unrecognized built type: treat as unparsable and DO NOT
		// advance the cursor — we can't claim we processed a message
		// we can't categorize. Deferred endProcessing fires with
		// time.Time{}, matching the BuildMessage-failure shape.
		unparsableMsg := o.feedMessageFactory.BuildUnparsableMessage(ctx, feedMessage)
		o.emitUnparsable(ctx, unparsableMsg, errFeedMessageDecode, ack, emitRaw, rawMsg)
		return
	}

	// Advance LastProcessedMessageGenTimestamp ONLY when the message was
	// actually admitted downstream. A ctx-cancelled send (graceful-close
	// deadline, shutdown) delivered NOTHING and acked NOTHING, so claiming
	// its gen-timestamp as "processed" would falsely advance the cursor —
	// calculateTiming would then see a spuriously recent processed cursor
	// and could delay or suppress processing-queue-delay recovery. On a
	// failed send the deferred endProcessing(time.Time{}) fires instead
	// (no cursor advance), matching the BuildMessage-failure convention.
	if admitted {
		endProcessing(feedMessage.Timestamp.Created)
	}
}

// send pushes a sessionEnvelope to the output channel under ctx-bounded
// backpressure. Replaces the bare `o.msgCh <- ...` pattern that risks
// panicking on a closed channel during shutdown. On ctx cancellation the
// envelope — and its ack — is dropped undelivered: the broker releases
// the unacked delivery when the AMQP channel closes. Reports whether the
// envelope was admitted; callers whose ack rides THIS envelope may
// ignore the result (a failed send simply never acks), but callers that
// send a FOLLOW-UP ack-bearing envelope must abort when an earlier
// required envelope (the raw side-channel) failed admission.
func (o *oddsFeedSessionImpl) send(ctx context.Context, env sessionEnvelope) bool {
	select {
	case o.msgCh <- env:
		return true
	case <-ctx.Done():
		return false
	}
}

// sendRawSideChannel publishes the extended-data raw envelope with a nil
// ack, reporting whether it was admitted. Callers invoke it AFTER the SDK
// has finished reading the shared decoded message (BuildMessage /
// recovery handlers) — the raw envelope exposes that same memory to
// consumers — and BEFORE sending the parsed or unparsable envelope that
// carries the delivery ack. o.msgCh is FIFO, so the raw is admitted to
// the public buffer first and the ack (fired on the later envelope's
// admission) can never precede it. On a false return the caller MUST NOT
// send the ack-bearing envelope: acking a delivery whose required raw
// was never admitted would break the raw-before-parsed half of the
// NEXT.md §0.6 contract — leaving the delivery unacked hands it back to
// the broker for redelivery instead.
func (o *oddsFeedSessionImpl) sendRawSideChannel(ctx context.Context, rawMsg *types.RawFeedMessage) bool {
	return o.send(ctx, sessionEnvelope{msg: types.SessionMessage{RawFeedMessage: cloneRawEnvelope(rawMsg)}})
}

// cloneRawEnvelope decouples the raw envelope handed to consumers from
// the SDK's backing payload buffer. types.RawFeedMessage exposes the
// bytes as a public FIELD (BasicFeedMessage.RawMessage), while the
// parsed/unparsable message built from the same delivery retains the
// original slice — its RawMessage() accessor clones on READ, which
// protects nothing when the backing store itself is mutable through the
// raw envelope. Without this copy, a consumer writing to
// raw.RawMessage would tamper with what the later parsed message's
// RawMessage() returns, and could race its readers. One copy per
// delivery, paid only when extended-data reporting is enabled. The
// embedded RoutingKey pointer and decoded Message stay shared — those
// are read-only by contract; the finding is the byte buffer.
func cloneRawEnvelope(rawMsg *types.RawFeedMessage) *types.RawFeedMessage {
	cp := *rawMsg
	cp.RawMessage = append([]byte(nil), rawMsg.RawMessage...)
	return &cp
}

// dropDelivery terminally handles a delivery that yields NO parsed
// envelope (nil FeedMessage, alive, disabled/out-of-scope producer,
// admitted snapshot_complete). With extended-data reporting on, the raw
// envelope is the delivery's only output and carries the ack, so it fires
// only once the raw is admitted to the public buffer — never
// acked-then-lost; otherwise it is a bare ack.
func (o *oddsFeedSessionImpl) dropDelivery(ctx context.Context, emitRaw bool, rawMsg *types.RawFeedMessage, ack func()) {
	if emitRaw {
		o.send(ctx, sessionEnvelope{msg: types.SessionMessage{RawFeedMessage: cloneRawEnvelope(rawMsg)}, ack: ack})
		return
	}
	runAck(ack)
}

// emitUnparsable handles in-band parse/build failures per the configured
// ExceptionStrategy. Under StrategyCatch (default) it emits an
// UnparsableMessage to the consumer, the delivery's ack riding the
// envelope; when extended-data reporting is on it publishes the raw
// side-channel FIRST (nil ack) so the raw is admitted before the ack
// fires. Under StrategyThrow it records the cause and triggers shutdown
// (the goroutine exits, msgCh closes, the pump observes Err() and aborts
// the subscription) — the delivery stays unacked on purpose (the session
// terminated without handling it) and the raw is deliberately NOT emitted,
// since the unacked delivery will be redelivered and re-reported.
func (o *oddsFeedSessionImpl) emitUnparsable(ctx context.Context, unparsable types.UnparsableMessage, cause error, ack func(), emitRaw bool, rawMsg *types.RawFeedMessage) {
	if o.exceptionStrategy == StrategyThrow {
		o.setErr(cause)
		if o.closeFn != nil {
			o.closeFn()
		}
		return
	}
	if emitRaw {
		if !o.sendRawSideChannel(ctx, rawMsg) {
			// Raw admission failed (shutdown): do NOT send the
			// ack-bearing unparsable envelope — see sendRawSideChannel.
			return
		}
	}
	o.send(ctx, sessionEnvelope{msg: types.SessionMessage{UnparsableMessage: unparsable}, ack: ack})
}

func newSession(
	rabbitMQClient *feed.Client,
	producerManager *producer.Manager,
	cacheManager *cache.Manager,
	feedMessageFactory *factory.FeedMessageFactory,
	recoverMessageProcessor recoveryMessageProcessor,
	exchangeName string,
	sportIDPrefix string,
	isReplay bool,
	logger *log.Logger,
	exceptionStrategy ExceptionStrategy,
	amqpPrefetch int,
) sdkOddsFeedSession {
	return &oddsFeedSessionImpl{
		channelConsumer: feed.NewChannelConsumer(
			rabbitMQClient,
			feedMessageFactory,
			logger,
			exchangeName,
			sportIDPrefix,
			amqpPrefetch,
		),
		producerManager:          producerManager,
		cacheManager:             cacheManager,
		feedMessageFactory:       feedMessageFactory,
		recoveryMessageProcessor: recoverMessageProcessor,
		exchangeName:             exchangeName,
		sportIDPrefix:            sportIDPrefix,
		sessionID:                uuid.New(),
		isReplay:                 isReplay,
		logger:                   logger,
		exceptionStrategy:        exceptionStrategy,
		msgCh:                    make(chan sessionEnvelope),
	}
}
