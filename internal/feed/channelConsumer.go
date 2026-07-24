package feed

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/oddin-gg/gosdk/internal/factory"
	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/internal/utils"
	"github.com/oddin-gg/gosdk/types"
)

const (
	emptyPosition = "-"

	// defaultPrefetch caps the unacked-deliveries window per consumer. With
	// auto-ack the broker considered every delivery acked instantly, so this
	// had no effect. With manual-ack (Phase 4 §0.6), this is the real
	// backpressure knob — beyond this, the broker stops delivering until the
	// consumer drains and acks.
	defaultPrefetch = 1000

	// oversizedRetainBytes bounds how much of a payload REJECTED for
	// exceeding feedXML.MaxFeedMessageBytes is retained on the resulting
	// UnparsableMessage — enough prefix for diagnosis; the full body is
	// deliberately not kept (see the decode-failure branch in
	// processDelivery).
	oversizedRetainBytes = 64 << 10
)

// QueueEnvelope pairs a decoded message with the ack for the AMQP
// delivery it came from. Ack ownership travels WITH the message down the
// pipeline (consumer → session → subscription pump): the broker is
// acknowledged only by whichever stage terminally handles the message —
// the pump, after the message lands in the public subscription buffer,
// or the session, when it intentionally consumes/drops it (alive
// handling, out-of-scope filtering). A delivery abandoned mid-pipeline
// by an abrupt shutdown is simply never acked; the broker releases
// unacked deliveries when the channel closes.
type QueueEnvelope struct {
	Msg *types.QueueMessage
	// Ack acknowledges the underlying delivery (idempotence is NOT
	// provided — call at most once). Errors are logged inside; nil only
	// in tests that fabricate envelopes.
	Ack func()
}

// channelOpener is the narrow surface ChannelConsumer needs from the
// feed Client: create an AMQP channel with the queue declared, routing
// keys bound, and consumption started. *Client satisfies it; the seam
// keeps the consumer unit-testable without a live broker.
type channelOpener interface {
	CreateChannel(ctx context.Context, routingKeys []string, exchangeName string, prefetch int) (<-chan amqp.Delivery, *amqp.Channel, error)
}

// amqpChannel is the narrow surface the consumer needs from an AMQP
// channel: closing it (which also deletes the exclusive autoDelete
// queue). *amqp.Channel satisfies it; the seam lets graceful-teardown
// tests observe the Close on the deadline/abandon path without standing
// up a live broker.
type amqpChannel interface {
	Close() error
}

// ChannelConsumer drains AMQP deliveries, decodes them, and admits decoded
// messages into an outgoing channel under ctx-cancellable backpressure.
// It does NOT ack on admission — the ack closure rides the envelope and
// fires only when the message reaches the public subscription buffer (or
// is intentionally dropped downstream). On connection drops the AMQP
// delivery channel closes; the consumer loop transparently waits for the
// Client to reconnect and re-creates its consumer channel.
type ChannelConsumer struct {
	client             channelOpener
	feedMessageFactory *factory.FeedMessageFactory
	logger             *log.Logger
	exchangeName       string
	sportIDPrefix      string

	prefetch int

	mu              sync.Mutex
	outgoing        chan QueueEnvelope
	closeFn         context.CancelFunc
	messageInterest *types.MessageInterest
	routingKeys     []string
	loopCtx         context.Context
	closeOnce       sync.Once
	closed          chan struct{}
	wg              sync.WaitGroup

	// drainCh implements graceful close (NEXT.md §8 Subscriptions):
	// closing it stops INTAKE — consume() returns at its next select
	// iteration, so the delivery currently being processed completes
	// its full decode + admit cycle first (processing is
	// synchronous within one loop iteration) — and run() exits instead
	// of re-opening the AMQP channel. Contrast with closeFn (abrupt):
	// cancelling the loop ctx aborts the in-flight admit and Nacks.
	drainOnce sync.Once
	drainCh   chan struct{}

	// Settlement accounting: unsettledN counts deliveries handed to the
	// session whose terminal disposition (ack on public-buffer
	// admission, drop-ack, or nack) has not yet fired. Graceful close
	// waits (bounded) for it to reach zero BEFORE the AMQP channel —
	// and with it the exclusive autoDelete queue — is torn down:
	// closing earlier forfeited redelivery for anything still unacked,
	// so a delivery stuck behind a full public buffer at drain time was
	// silently lost.
	//
	// Counter + broadcast channel rather than a sync.WaitGroup: a
	// WaitGroup can only be waited from a dedicated blocked goroutine,
	// which leaked forever whenever the pipeline abandoned a delivery
	// undelivered (its disposition then never fires — e.g. the public
	// pump exits on pumpStop while holding an envelope, or envelopes
	// remain queued between session and pump at teardown). The counter
	// lets CloseGraceful select on settledSignal() bounded by ctx and
	// report an honest not-settled result with nothing left behind.
	settleMu   sync.Mutex
	unsettledN int
	settleCh   chan struct{} // non-nil while a waiter is registered; closed + cleared when unsettledN hits 0

	// gracefulCh holds the AMQP channel across a graceful drain: run()
	// hands it over instead of closing it, and runShutdown closes it
	// after serializing against any in-flight Ack. Guarded by mu.
	gracefulCh amqpChannel

	// ackMu serializes a delivery's d.Ack against the graceful-teardown
	// gch.Close so the two never run concurrently on the same
	// *amqp.Channel (amqp091 does NOT mutually serialize Close and Ack —
	// Close's send path does not take the channel mutex that Ack takes).
	// chClosed, set under ackMu once the graceful channel is closed, tells
	// a late ack to skip d.Ack: the exclusive autoDelete queue is already
	// gone, so the ack would only return ErrClosed, and skipping keeps it
	// off a closed channel. This lets teardown key the close on "is an Ack
	// genuinely executing" (holds ackMu) rather than "have all deliveries
	// settled" — abandoned envelopes whose ack closure will NEVER run no
	// longer block the close, so the channel + queue can't leak.
	ackMu    sync.Mutex
	chClosed bool
}

// NewChannelConsumer constructs an unstarted consumer. Call Open to begin.
// prefetch ≤ 0 falls back to the package default.
func NewChannelConsumer(
	client channelOpener,
	feedMessageFactory *factory.FeedMessageFactory,
	logger *log.Logger,
	exchangeName string,
	sportIDPrefix string,
	prefetch int,
) *ChannelConsumer {
	if prefetch <= 0 {
		prefetch = defaultPrefetch
	}
	return &ChannelConsumer{
		client:             client,
		feedMessageFactory: feedMessageFactory,
		logger:             logger,
		exchangeName:       exchangeName,
		sportIDPrefix:      sportIDPrefix,
		prefetch:           prefetch,
		closed:             make(chan struct{}),
		drainCh:            make(chan struct{}),
	}
}

// Open subscribes to the configured routing keys and starts the consumer
// loop. The returned channel emits decoded messages until Close cancels.
//
// On AMQP-level reconnect (handled by Client) the consumer re-creates its
// channel transparently — callers don't need to react.
func (c *ChannelConsumer) Open(ctx context.Context, routingKeys []string, messageInterest *types.MessageInterest) (<-chan QueueEnvelope, error) {
	c.mu.Lock()
	if c.outgoing != nil {
		c.mu.Unlock()
		return nil, errors.New("feed: consumer already opened")
	}
	c.routingKeys = append([]string(nil), routingKeys...)
	c.messageInterest = messageInterest
	c.mu.Unlock()

	// Create the FIRST AMQP channel SYNCHRONOUSLY — declare the queue,
	// bind the routing keys, and start consuming BEFORE Open returns.
	// This makes the caller's ctx bound queue declaration and, crucially,
	// surfaces permanent exchange/permission/topology errors to the
	// caller (and, up the stack, to Connect / Subscribe / Subscription)
	// instead of retrying them forever inside run(). Nothing is committed
	// to the struct on failure, so a fresh Open can retry. Reconnect
	// after a LATER connection drop is still handled asynchronously by
	// run() (transient errors retried there).
	deliveries, ch, err := c.client.CreateChannel(ctx, c.routingKeys, c.exchangeName, c.prefetch)
	if err != nil {
		return nil, fmt.Errorf("feed: open consumer channel (interest=%s, keys=%d): %w", *messageInterest, len(routingKeys), err)
	}

	c.mu.Lock()
	// UNBUFFERED on purpose: elastic buffering lives solely in the
	// subscription's configured public channel — a hidden staging queue
	// here would hold messages that are invisible to the consumer and
	// dropped on abrupt close. Broker-side prefetch absorbs bursts;
	// deliveries stay UNACKED until the pump lands them in the public
	// buffer (the envelope carries the ack), so nothing parked in this
	// pipeline is ever falsely acknowledged.
	c.outgoing = make(chan QueueEnvelope)

	// Loop ctx must outlive the caller's Open ctx (which bounded the
	// first channel setup only). WithoutCancel propagates caller metadata
	// while severing the cancellation chain; closeFn cancels at Close() time.
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	c.loopCtx = loopCtx
	c.closeFn = cancel
	out := c.outgoing
	c.mu.Unlock()

	c.wg.Go(func() { c.run(loopCtx, deliveries, ch) })
	return out, nil
}

// CloseGraceful stops intake without aborting the in-flight delivery:
// the consumer loop finishes the current decode + admit cycle,
// stops pulling further deliveries, and the outgoing channel closes
// once every admitted message has been handed over. Waits for the loop
// to exit bounded by ctx; on deadline expiry it falls back to the
// abrupt path (loop-ctx cancel → Nack on the in-flight delivery).
// Idempotent, and composes with Close — whichever runs first wins.
//
// Returns whether the drain FULLY completed: the loop exited AND every
// handed-over delivery reached its terminal disposition within ctx.
// false means the deadline expired with work outstanding — the closing
// teardown then deletes the exclusive autoDelete queue and forfeits
// redelivery for anything still unacked, so callers MUST surface the
// deadline as the terminal cause rather than reporting a clean drain.
func (c *ChannelConsumer) CloseGraceful(ctx context.Context) bool {
	c.drainOnce.Do(func() { close(c.drainCh) })

	loopDone := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(loopDone)
	}()
	loopExited := false
	select {
	case <-loopDone:
		loopExited = true
	case <-ctx.Done():
		// Drain deadline expired — the loop is stuck (stalled consumer
		// chain or a broker wait). Fall through to the hard path.
	}
	settled := false
	if loopExited {
		// Per-delivery settlement: wait (bounded) until every handed-
		// over delivery reached its terminal disposition before Close
		// tears down the AMQP channel + exclusive autoDelete queue —
		// see the unsettledN field. Only after loop exit, so the wait
		// never races a consume-side addUnsettled. On deadline expiry
		// we close anyway: that is the documented discard path — but
		// it is reported to the caller, never masked.
		select {
		case <-c.settledSignal():
			settled = true
		case <-ctx.Done():
		}
	}
	_ = c.Close(ctx)
	return loopExited && settled
}

// addUnsettled records one delivery handed downstream whose terminal
// disposition is still pending. Paired with settleOne.
func (c *ChannelConsumer) addUnsettled() {
	c.settleMu.Lock()
	c.unsettledN++
	c.settleMu.Unlock()
}

// settleOne records a terminal disposition (ack, drop-ack, or nack) and
// wakes the graceful-close waiter when the last one lands.
func (c *ChannelConsumer) settleOne() {
	c.settleMu.Lock()
	c.unsettledN--
	if c.unsettledN == 0 && c.settleCh != nil {
		close(c.settleCh)
		c.settleCh = nil
	}
	c.settleMu.Unlock()
}

// settledSignal returns a channel that is closed once every handed-over
// delivery has settled — already closed if nothing is outstanding.
func (c *ChannelConsumer) settledSignal() <-chan struct{} {
	c.settleMu.Lock()
	defer c.settleMu.Unlock()
	if c.unsettledN == 0 {
		done := make(chan struct{})
		close(done)
		return done
	}
	if c.settleCh == nil {
		c.settleCh = make(chan struct{})
	}
	return c.settleCh
}

// Close terminates the consumer loop, waits for it to exit, and closes the
// outgoing channel. Idempotent. Cap the wait via the supplied ctx.
func (c *ChannelConsumer) Close(ctx context.Context) error {
	c.closeOnce.Do(func() { go c.runShutdown() })

	select {
	case <-c.closed:
		return nil
	default:
	}
	select {
	case <-c.closed:
		return nil
	case <-ctx.Done():
		select {
		case <-c.closed:
			return nil
		default:
			return ctx.Err()
		}
	}
}

func (c *ChannelConsumer) runShutdown() {
	c.mu.Lock()
	cancel := c.closeFn
	c.closeFn = nil
	out := c.outgoing
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	c.wg.Wait()
	// Close the channel a graceful drain stashed.
	c.mu.Lock()
	gch := c.gracefulCh
	c.gracefulCh = nil
	c.mu.Unlock()
	if gch != nil {
		c.closeGracefulChannel(gch)
	}
	if out != nil {
		close(out)
	}
	close(c.closed)
}

// closeGracefulChannel tears down the AMQP channel (and with it the
// exclusive autoDelete queue) that a graceful drain stashed, serialized
// against any in-flight delivery Ack so Close and Ack never run
// concurrently on the same non-thread-safe *amqp.Channel.
//
// The wait is keyed on "is an Ack GENUINELY executing" (an ack holds
// ackMu around its d.Ack), NOT on "has every handed-over delivery
// settled". The distinction is the whole fix: on the drain DEADLINE path
// envelopes are routinely abandoned mid-pipeline (session send aborted on
// ctx cancel, pump abandonment on pumpStop) — their ack closure will
// NEVER run, so unsettledN never returns to 0. Waiting for full
// settlement here left gch.Close blocked forever on a reaper; if only the
// subscription closed while the parent Client stayed alive, the exclusive
// queue was never deleted, stayed bound to its routing keys, and
// accumulated feed messages unboundedly (prefetch caps unacked
// deliveries, not queue depth), leaking one goroutine per timed-out close.
//
// TryLock succeeds whenever no ack is mid-flight — the common case, since
// abandoned envelopes never take ackMu — so the channel closes promptly.
// It only fails when an ack is genuinely wedged in a network write on a
// stalled transport; then a detached reaper waits for that single ack to
// finish (bounded by the connection teardown that fails a wedged write —
// the amqp091 heartbeat monitor tears the connection down after ~2 missed
// intervals even if the parent Client never closes) and closes afterward
// with no concurrent method in flight.
func (c *ChannelConsumer) closeGracefulChannel(gch amqpChannel) {
	if c.ackMu.TryLock() {
		c.chClosed = true
		_ = gch.Close()
		c.ackMu.Unlock()
		return
	}
	go func() {
		c.ackMu.Lock()
		c.chClosed = true
		_ = gch.Close()
		c.ackMu.Unlock()
	}()
}

// run is the single consumer goroutine. It consumes the initial channel
// (created synchronously in Open), and on delivery-channel close
// (connection drop) reopens a fresh channel — retrying transient errors —
// until ctx is cancelled or a graceful drain is requested. Permanent
// topology/permission errors are NOT retried here: they already surfaced
// synchronously from Open, so the initial channel is known-good.
func (c *ChannelConsumer) run(ctx context.Context, deliveries <-chan amqp.Delivery, ch *amqp.Channel) {
	for {
		c.consume(ctx, deliveries, ch)

		// consume returned: ctx cancelled, graceful drain, or delivery
		// channel closed.
		select {
		case <-c.drainCh:
			// GRACEFUL drain: hand the channel to shutdown WITHOUT
			// closing it. Closing here deleted the exclusive autoDelete
			// queue while admitted-but-unacked deliveries were still
			// being disposed downstream — a delivery stuck behind a full
			// public buffer became unacked AND unredeliverable (silent
			// loss). CloseGraceful waits for settlement; runShutdown
			// closes the stashed channel afterwards.
			//
			// Stash only a non-nil channel: assigning a nil *amqp.Channel
			// into the amqpChannel interface would yield a non-nil
			// interface wrapping a nil pointer, and runShutdown would then
			// call Close on it (panic). A nil ch (only in tests that drive
			// run without a real channel) leaves gracefulCh nil so teardown
			// skips the close.
			c.mu.Lock()
			if ch != nil {
				c.gracefulCh = ch
			}
			c.mu.Unlock()
			return
		default:
		}
		if ch != nil {
			_ = ch.Close()
		}
		if ctx.Err() != nil {
			return
		}

		// Connection dropped mid-consume — reopen. Transient failures
		// (brief disconnect window) are retried; the loop ctx bounds it.
		// EVERY stage of the reopen is drain-aware: pre-fix, drainCh was
		// checked exactly once above, so a graceful close arriving while
		// the reopen was parked (a CreateChannel wait or the retry
		// backoff) consumed the whole drain deadline and degraded to the
		// abrupt path. attemptCtx relays a drain request into the
		// in-flight CreateChannel, the backoff selects on drainCh, and a
		// channel that lands after drain was requested is closed and
		// discarded rather than consumed.
		attemptCtx, cancelAttempt := context.WithCancel(ctx)
		relayDone := make(chan struct{})
		go func() {
			defer close(relayDone)
			select {
			case <-c.drainCh:
				cancelAttempt()
			case <-attemptCtx.Done():
			}
		}()
		reopen := func() (reopened bool) {
			defer cancelAttempt()
			for {
				select {
				case <-c.drainCh:
					return false
				default:
				}
				var err error
				deliveries, ch, err = c.client.CreateChannel(attemptCtx, c.routingKeys, c.exchangeName, c.prefetch)
				if err == nil {
					select {
					case <-c.drainCh:
						// Drain raced the successful open — reject the
						// late channel instead of consuming from it.
						_ = ch.Close()
						return false
					default:
					}
					return true
				}
				if ctx.Err() != nil || attemptCtx.Err() != nil {
					return false
				}
				c.logger.WithError(err).Warn("feed: reopen consumer channel failed; retrying")
				select {
				case <-ctx.Done():
					return false
				case <-c.drainCh:
					return false
				case <-time.After(500 * time.Millisecond):
				}
			}
		}
		ok := reopen()
		<-relayDone // join the relay so it never outlives the loop
		if !ok {
			return
		}
	}
}

// consume drives a single AMQP-channel session. Returns when the delivery
// channel closes (typical reason: connection drop) or ctx cancels.
func (c *ChannelConsumer) consume(ctx context.Context, deliveries <-chan amqp.Delivery, ch *amqp.Channel) {
	for {
		// Hard bound on post-drain intake: in the main select below,
		// drainCh and deliveries can BOTH be ready, and Go picks
		// uniformly — under a continuous delivery stream the drain
		// signal could keep losing the race with no upper bound. This
		// non-blocking pre-check runs every iteration, so once drain is
		// signalled at most ONE further delivery (the one already
		// racing the signal inside the select) is processed.
		select {
		case <-c.drainCh:
			return
		default:
		}
		select {
		case <-ctx.Done():
			return
		case <-c.drainCh:
			// Graceful drain: stop pulling new deliveries. The
			// previously in-flight delivery already completed its
			// decode + admit — processing is synchronous within
			// one loop iteration, so reaching this select means the
			// cycle finished. No Nack (NEXT.md §8 graceful close).
			return
		case d, ok := <-deliveries:
			if !ok {
				// Channel closed (connection drop or broker close).
				return
			}
			// processDelivery always returns a non-nil *QueueMessage
			// — every branch (parse error, empty body, decode error,
			// successful decode) builds and returns one. It does NOT
			// touch the AMQP Acknowledger; ack ownership travels with
			// the envelope, nack on abort is admit's responsibility.
			qm := c.processDelivery(ctx, d)

			// Manual ack semantics (NEXT.md §0.6 / §8): the delivery is
			// acked only after the message is admitted to the PUBLIC
			// subscription buffer — the envelope carries the ack closure
			// downstream. If ctx cancels mid-handoff, Nack(requeue=false)
			// — recovery handles the gap.
			//
			// Settlement accounting: Add BEFORE the handoff (the ack can
			// fire the instant the session receives the envelope); the
			// not-admitted paths settle explicitly below since their ack
			// closure never fires.
			c.addUnsettled()
			if !c.admit(ctx, d, QueueEnvelope{Msg: qm, Ack: c.ackFunc(d)}) {
				c.settleOne()
				return
			}
		}
	}
}

// ackFunc builds the envelope's ack closure for one delivery. Ack errors
// are logged, not propagated — by the time the ack fires the message has
// already been handed to the consumer (or intentionally dropped), and
// the broker will simply redeliver on the next channel teardown.
//
// Late-ack after teardown: the ack and the graceful-teardown Close are
// serialized on ackMu (see closeGracefulChannel), so they never run
// concurrently on the same channel. In the normal path the ack fires
// before teardown; on the drain DEADLINE path teardown may win and set
// chClosed, in which case the ack skips d.Ack — the queue is exclusive +
// autoDelete on this channel and was deleted on close, so nothing is
// redelivered, and the message itself was already admitted to the public
// buffer. If instead the ack was already mid-flight when teardown ran,
// Close waited for it; a wedged write is failed with ErrClosed by the
// connection teardown, which is benign for the same reason. Logged at
// Debug (not Warn) so clean shutdowns stay quiet.
func (c *ChannelConsumer) ackFunc(d amqp.Delivery) func() {
	return func() {
		defer c.settleOne() // settle: terminal disposition reached
		// Serialize against the graceful-teardown Close (see ackMu). If
		// teardown already closed the channel, skip the Ack entirely: the
		// exclusive autoDelete queue is gone, the delivery was already
		// admitted to the public buffer, and issuing d.Ack now would both
		// return ErrClosed and touch a channel we must stay off.
		c.ackMu.Lock()
		defer c.ackMu.Unlock()
		if c.chClosed {
			return
		}
		err := d.Ack(false)
		if err == nil {
			return
		}
		log := c.logger.WithError(err).
			WithField("routing_key", d.RoutingKey).
			WithField("delivery_tag", d.DeliveryTag)
		if c.draining() && errors.Is(err, amqp.ErrClosed) {
			log.Debug("feed: ack after graceful drain raced channel close (benign; exclusive queue already deleted)")
			return
		}
		log.Warn("feed: ack failed")
	}
}

// draining reports whether graceful drain has been signalled.
func (c *ChannelConsumer) draining() bool {
	select {
	case <-c.drainCh:
		return true
	default:
		return false
	}
}

// admit blocks until env is handed to the session loop or ctx cancels
// (then Nacks the in-hand delivery). Returns false when the consume
// loop must exit. No ack happens here — a stalled downstream simply
// leaves the delivery unacked, and broker prefetch provides the
// backpressure; the slow-consumer warning is emitted by the
// subscription pump, which watches the actual public-buffer send.
func (c *ChannelConsumer) admit(ctx context.Context, d amqp.Delivery, env QueueEnvelope) bool {
	select {
	case c.outgoing <- env:
		return true
	case <-ctx.Done():
		if err := d.Nack(false, false); err != nil {
			c.logger.WithError(err).
				WithField("routing_key", d.RoutingKey).
				WithField("delivery_tag", d.DeliveryTag).
				Debug("feed: nack on shutdown")
		}
		return false
	}
}

// retainedBody returns the bytes to keep on an UnparsableMessage for a
// delivery that will NOT be decoded (malformed route, decode failure,
// route/payload identity or required-attribute rejection). It is the
// single body-retention boundary shared by every unparsable path.
//
// A body within feedXML.MaxFeedMessageBytes is returned as-is (the valid
// decode path keeps the full body too). A body OVER the cap is truncated
// to a COPIED oversizedRetainBytes prefix: the copy both bounds retention
// and severs the (possibly multi-megabyte, upstream-controlled) backing
// array so it stays garbage-collectable even though RawMessage() clones
// per call. The malformed-route path reaches this BEFORE feedXML.Decode's
// size gate, so without the explicit length check an oversized body there
// would bypass the 8 MiB robustness boundary entirely.
func retainedBody(body []byte) []byte {
	if len(body) > feedXML.MaxFeedMessageBytes {
		return append([]byte(nil), body[:min(len(body), oversizedRetainBytes)]...)
	}
	return body
}

// processDelivery decodes a single AMQP delivery into a *types.QueueMessage.
// On decode failure OR routing-key parse failure it returns an unparsable
// message — the caller admits it to the outgoing buffer, then acks. Acking
// is the caller's responsibility; processDelivery does NOT ack on error
// paths. Returns nil only when there's nothing meaningful to admit.
//
// Pre-v2.23 the routing-key parse failure branch acked-and-dropped the
// delivery instead of admitting it as UnparsableMessage. The function
// contract said "admitted on parse failure"; the implementation
// disagreed. v2.23 honors the contract: malformed routes are admitted
// with a minimal RoutingKeyInfo carrying just the raw route (EventID
// and SportID nil — BuildUnparsableMessage nil-checks both).
func (c *ChannelConsumer) processDelivery(ctx context.Context, d amqp.Delivery) *types.QueueMessage {
	// Timestamp semantics follow Java/.NET feature parity:
	//   - Created  = XML message timestamp (when the feed produced the
	//                event; populated below after a successful decode).
	//   - Sent     = AMQP delivery timestamp (when the broker dispatched).
	//   - Received = local time when this consumer dequeued.
	//   - Published= local time after successful decode (set later).
	//
	// On a successful decode, Created is set from message.Timestamp().
	// If AMQP delivery had no timestamp (d.Timestamp.IsZero()), Sent
	// falls back to Created — avoiding a zero "Sent" that downstream
	// consumers would mistake for "unknown".
	timestamp := types.MessageTimestamp{
		Sent:     d.Timestamp,
		Received: time.Now(),
	}

	routingKeyInfo, err := c.parseRoute(d.RoutingKey)
	if err != nil {
		c.logger.WithError(err).Errorf("failed to parse route %s", d.RoutingKey)
		// Admit as UnparsableMessage so consumers see the malformed
		// delivery. Minimal RoutingKeyInfo carries just the raw
		// route — EventID and SportID stay nil, and
		// BuildUnparsableMessage nil-checks both (v2.19 / v2.20 / v2.21
		// hardening) before dereferencing. Caller acks after admission.
		//
		// Bound the retained body: route parsing runs BEFORE feedXML.Decode,
		// so this path never passed the MaxFeedMessageBytes gate. Pre-fix a
		// malformed route carrying a multi-megabyte body retained the whole
		// thing on the envelope (and RawMessage() cloned it per call) — with
		// the default 256-slot buffer that let a misbehaving broker pin
		// gigabytes, defeating the 8 MiB robustness boundary the decode path
		// enforces. retainedBody truncates + copies an oversized body so the
		// large backing array stays garbage-collectable.
		return &types.QueueMessage{
			UnparsableMessage: c.feedMessageFactory.BuildUnparsableMessage(ctx, &types.FeedMessage{
				BasicFeedMessage: types.BasicFeedMessage{
					RawMessage: retainedBody(d.Body),
					RoutingKey: &types.RoutingKeyInfo{FullRoutingKey: d.RoutingKey},
					Timestamp:  timestamp,
				},
			}),
		}
	}

	queueMessage := &types.QueueMessage{}

	if len(d.Body) == 0 {
		c.logger.Warnf("received message without proper body from %s", d.RoutingKey)
		// No XML to read Created from; leave it zero. Sent stays as the
		// AMQP delivery timestamp (or zero if the broker didn't supply one).
		queueMessage.UnparsableMessage = c.feedMessageFactory.BuildUnparsableMessage(ctx, &types.FeedMessage{
			BasicFeedMessage: types.BasicFeedMessage{
				RawMessage: d.Body,
				RoutingKey: routingKeyInfo,
				Timestamp:  timestamp,
			},
		})
		return queueMessage
	}

	message, err := feedXML.Decode(d.Body)
	if err != nil {
		// Bounded diagnostics only — the body is untrusted and unbounded
		// (see utils.PayloadPreview); the full bytes stay available to
		// consumers on the UnparsableMessage below.
		switch {
		case errors.Is(err, feedXML.ErrUnknownMessage):
			c.logger.Errorf("unknown message - route %s %s", d.RoutingKey, utils.PayloadPreview(d.Body))
		default:
			c.logger.WithError(err).Errorf("failed to unmarshall route %s %s", d.RoutingKey, utils.PayloadPreview(d.Body))
		}
		// Decode failed — XML timestamp unavailable. Same shape as the
		// empty-body branch. retainedBody truncates + copies an oversized
		// body (ErrPayloadTooLarge is rejected before parsing) so the
		// multi-megabyte backing array is garbage-collectable rather than
		// pinned on the envelope and re-cloned by every RawMessage() call;
		// the diagnostic log above already records the full length.
		queueMessage.UnparsableMessage = c.feedMessageFactory.BuildUnparsableMessage(ctx, &types.FeedMessage{
			BasicFeedMessage: types.BasicFeedMessage{
				RawMessage: retainedBody(d.Body),
				RoutingKey: routingKeyInfo,
				Timestamp:  timestamp,
			},
		})
		return queueMessage
	}

	// Cross-check the two independent identities the delivery carries
	// BEFORE anything downstream consumes them: the routing key (used to
	// build the public event) and the XML payload (used for cache
	// invalidation). Pre-fix they were never compared — a payload for
	// event B on event A's route mutated B's caches while being
	// delivered as an event for A, and an event payload on a system
	// route could reach cache hooks before being reported unparsable.
	// Mismatches become unparsable envelopes with NO decoded Message
	// attached, so cache invalidation and event enrichment never run
	// under the wrong identity.
	if verr := validateRouteIdentity(routingKeyInfo, message); verr != nil {
		c.logger.WithError(verr).Errorf("route/payload identity mismatch - route %s %s", d.RoutingKey, utils.PayloadPreview(d.Body))
		queueMessage.UnparsableMessage = c.feedMessageFactory.BuildUnparsableMessage(ctx, &types.FeedMessage{
			BasicFeedMessage: types.BasicFeedMessage{
				RawMessage: retainedBody(d.Body),
				RoutingKey: routingKeyInfo,
				Timestamp:  timestamp,
			},
		})
		return queueMessage
	}

	// Required-attribute validation: XML decode only checks syntax, so a
	// MISSING attribute silently becomes a zero value that downstream
	// state/cache/recovery processing then acts on (Codex P2): a
	// zero-request_id snapshot_complete would be ACKed as an unknown
	// completion (real recovery stays pending), an absent `subscribed`
	// would read as "not subscribed" and could mark the producer down,
	// and an empty/unparseable event id would be delivered under routing
	// identity while cache invalidation silently skipped it. A message
	// that fails these checks becomes an UnparsableMessage — admitted +
	// ACKed (never redelivered) but carrying NO decoded Message, so none
	// of that processing runs under an unvalidated identity/payload.
	if verr := validateDecodedMessage(routingKeyInfo, message); verr != nil {
		c.logger.WithError(verr).Errorf("feed message failed required-attribute validation - route %s %s", d.RoutingKey, utils.PayloadPreview(d.Body))
		queueMessage.UnparsableMessage = c.feedMessageFactory.BuildUnparsableMessage(ctx, &types.FeedMessage{
			BasicFeedMessage: types.BasicFeedMessage{
				RawMessage: retainedBody(d.Body),
				RoutingKey: routingKeyInfo,
				Timestamp:  timestamp,
			},
		})
		return queueMessage
	}

	// XML decoded — populate Created from the message's own timestamp,
	// matching Java/.NET. If the broker omitted a delivery timestamp,
	// fall back so Sent is at least as authoritative as Created.
	timestamp.Created = message.Timestamp()
	if timestamp.Sent.IsZero() {
		timestamp.Sent = timestamp.Created
	}
	timestamp.Published = time.Now()
	basicMessage := types.BasicFeedMessage{
		RawMessage: d.Body,
		RoutingKey: routingKeyInfo,
		Timestamp:  timestamp,
	}

	queueMessage.RawFeedMessage = &types.RawFeedMessage{
		BasicFeedMessage: basicMessage,
		Message:          message,
		MessageInterest:  *c.messageInterest,
	}
	queueMessage.FeedMessage = &types.FeedMessage{
		BasicFeedMessage: basicMessage,
		Message:          message,
	}
	return queueMessage
}

// validateRouteIdentity cross-checks message kind and event identity
// between the parsed routing key and the decoded XML payload — see the
// call site in processDelivery for the failure modes this closes.
func validateRouteIdentity(rk *types.RoutingKeyInfo, msg types.BasicMessage) error {
	if rk == nil {
		return nil
	}
	_, isAlive := msg.(*feedXML.Alive)
	_, isSnapshot := msg.(*feedXML.SnapshotComplete)
	systemKind := isAlive || isSnapshot
	if rk.IsSystemRoutingKey != systemKind {
		if systemKind {
			return fmt.Errorf("system message %T arrived on an event route", msg)
		}
		return fmt.Errorf("event message %T arrived on a system route", msg)
	}
	if systemKind {
		return nil
	}
	// Event route + event payload: compare event ids when both sides
	// carry one (some message kinds have no event id attribute; an
	// empty XML id stays tolerated as absent).
	type eventIDer interface{ GetEventID() string }
	if em, ok := msg.(eventIDer); ok && rk.EventID != nil {
		if got := em.GetEventID(); got != "" {
			// Compare URN VALUES, not serialized text. ParseURN
			// canonicalizes (leading zeros: "od:match:007" → "od:match:7")
			// and the route id arrives already canonical, so a raw-string
			// compare spuriously rejected a payload that spelled a valid id
			// non-canonically. A payload id that does not parse cannot match
			// the parsed route id and is a genuine mismatch (validateDecoded-
			// Message rejects unparseable event ids on their own merits too).
			gotURN, perr := types.ParseURN(got)
			if perr != nil || gotURN.ToString() != rk.EventID.ToString() {
				return fmt.Errorf("payload event id %q does not match route event id %q", got, rk.EventID.ToString())
			}
		}
	}
	return nil
}

// validateDecodedMessage enforces required attributes that XML syntax
// validation alone cannot: an absent attribute decodes to a zero value
// that downstream processing would act on. See the call site for the
// consequences each check closes.
func validateDecodedMessage(rk *types.RoutingKeyInfo, msg types.BasicMessage) error {
	// Common required attributes on EVERY decoded feed message, system or
	// event. A missing product/timestamp XML attribute decodes to a zero
	// value downstream processing would act on: a zero product resolves to
	// the unknown-producer sentinel, and a zero timestamp yields a zero
	// Created that stalls monotonic processing/recovery cursors — which is
	// reachable for alive and snapshot_complete too, and they drive the
	// recovery state machine (a timestamp-less alive would record a zero
	// valid-alive generation / mis-compute the recovery range).
	if msg.Product() <= 0 {
		return fmt.Errorf("%T: non-positive product %d", msg, msg.Product())
	}
	if msg.Timestamp().IsZero() {
		return fmt.Errorf("%T: missing required timestamp", msg)
	}
	switch m := msg.(type) {
	case *feedXML.Alive:
		// subscribed must be EXPLICIT and boolean-valued: a missing
		// attribute (nil) read as false could mark the producer down.
		if m.Subscribed == nil {
			return fmt.Errorf("alive: missing required subscribed attribute")
		}
		if v := *m.Subscribed; v != 0 && v != 1 {
			return fmt.Errorf("alive: subscribed=%d out of range {0,1}", v)
		}
	case *feedXML.SnapshotComplete:
		// request_id identifies the recovery being completed; ids are
		// positive, so a zero (absent) id would falsely ACK an unknown
		// completion and strand the real recovery.
		if m.RequestID <= 0 {
			return fmt.Errorf("snapshot_complete: non-positive request_id %d", m.RequestID)
		}
	case *feedXML.OddsChange:
		if err := validateEventID(rk, m); err != nil {
			return err
		}
		return validateOutcomeMarkets("odds_change", m.Odds.Markets)
	case *feedXML.BetSettlement:
		if err := validateEventID(rk, m); err != nil {
			return err
		}
		return validateOutcomeMarkets("bet_settlement", m.Markets.Markets)
	case *feedXML.BetCancel:
		if err := validateEventID(rk, m); err != nil {
			return err
		}
		for _, mk := range m.Markets {
			if mk == nil {
				return fmt.Errorf("bet_cancel: nil market entry")
			}
			if mk.ID <= 0 {
				return fmt.Errorf("bet_cancel: non-positive market id %d", mk.ID)
			}
		}
	case *feedXML.RollbackBetSettlement:
		if err := validateEventID(rk, m); err != nil {
			return err
		}
		for _, mk := range m.Markets {
			if mk.ID <= 0 {
				return fmt.Errorf("rollback_bet_settlement: non-positive market id %d", mk.ID)
			}
		}
	case *feedXML.RollbackBetCancel:
		if err := validateEventID(rk, m); err != nil {
			return err
		}
		for _, mk := range m.Markets {
			if mk.ID <= 0 {
				return fmt.Errorf("rollback_bet_cancel: non-positive market id %d", mk.ID)
			}
		}
	case *feedXML.BetStop:
		return validateEventID(rk, m)
	case *feedXML.FixtureChange:
		return validateEventID(rk, m)
	default:
		// Fallback for any future event-addressed kind: it must carry a
		// nonempty, parseable event id — a missing/empty one is delivered
		// under routing identity while cache invalidation silently skips it
		// (ParseURN("") fails). Only checked for event routes (rk carries an
		// event id); system messages are handled by the cases above.
		type eventIDer interface{ GetEventID() string }
		if em, ok := msg.(eventIDer); ok && rk != nil && !rk.IsSystemRoutingKey {
			got := em.GetEventID()
			if got == "" {
				return fmt.Errorf("%T: missing required event_id", msg)
			}
			if _, err := types.ParseURN(got); err != nil {
				return fmt.Errorf("%T: unparseable event_id %q: %w", msg, got, err)
			}
		}
	}
	return nil
}

// eventBasicMessage is the shared surface of every event-addressed feed
// message: the BasicMessage attributes plus the payload event id.
type eventBasicMessage interface {
	types.BasicMessage
	GetEventID() string
}

// validateEventID enforces that an event-addressed message carries a
// nonempty, parseable event id. A missing/empty one is delivered under
// routing identity while cache invalidation silently skips it
// (ParseURN("") fails). Scoped to event routes (rk carries an event id);
// product and timestamp are validated commonly in validateDecodedMessage.
func validateEventID(rk *types.RoutingKeyInfo, msg eventBasicMessage) error {
	if rk != nil && !rk.IsSystemRoutingKey {
		got := msg.GetEventID()
		if got == "" {
			return fmt.Errorf("%T: missing required event_id", msg)
		}
		if _, err := types.ParseURN(got); err != nil {
			return fmt.Errorf("%T: unparseable event_id %q: %w", msg, got, err)
		}
	}
	return nil
}

// validateOutcomeMarkets checks per-market and per-outcome identities for
// the message kinds that carry outcomes (odds_change, bet_settlement):
// market ids are positive and outcome ids nonempty. A missing id decodes
// to 0 / "" — a betting identity consumers must never act on, and one
// description enrichment silently drops.
func validateOutcomeMarkets(kind string, markets []*feedXML.MarketWithOutcome) error {
	for _, mk := range markets {
		if mk == nil {
			return fmt.Errorf("%s: nil market entry", kind)
		}
		if mk.ID <= 0 {
			return fmt.Errorf("%s: non-positive market id %d", kind, mk.ID)
		}
		for i := range mk.Outcomes {
			if mk.Outcomes[i].ID == "" {
				return fmt.Errorf("%s: market %d has an outcome with an empty id", kind, mk.ID)
			}
		}
	}
	return nil
}

func (c *ChannelConsumer) parseRoute(route string) (*types.RoutingKeyInfo, error) {
	parts := strings.Split(route, ".")
	if len(parts) != 8 {
		return nil, fmt.Errorf("incorrect route %s", route)
	}

	sportID := parts[4]
	eventID := parts[6]
	hasID := sportID != emptyPosition || eventID != emptyPosition
	if !hasID {
		return &types.RoutingKeyInfo{
			FullRoutingKey:     route,
			IsSystemRoutingKey: true,
		}, nil
	}

	var (
		err      error
		sportURN *types.URN
		eventURN *types.URN
	)
	if sportID != emptyPosition {
		sportURN, err = types.ParseURN(c.sportIDPrefix + sportID)
		if err != nil {
			return nil, err
		}
	}

	eventType := parts[5]
	if eventType != emptyPosition && eventID != emptyPosition {
		eventURN, err = types.ParseURN(eventType + ":" + eventID)
		if err != nil {
			return nil, err
		}
	}

	return &types.RoutingKeyInfo{
		FullRoutingKey:     route,
		SportID:            sportURN,
		EventID:            eventURN,
		IsSystemRoutingKey: false,
	}, nil
}
