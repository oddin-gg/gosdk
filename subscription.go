package gosdk

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/oddin-gg/gosdk/types"
)

// SubscribeOption tunes a Subscribe call.
type SubscribeOption func(*subscribeConfig)

type subscribeConfig struct {
	messageInterest types.MessageInterest
	// messageInterestSet tracks whether messageInterest was explicitly
	// chosen by the caller (via WithMessageInterest or implied by
	// WithSpecificEvents). Required because
	// types.SpecifiedMatchesOnlyMessageInterest is "" — i.e. the
	// "specified events" interest collides with the zero value, so an
	// "is the interest the empty default" check cannot distinguish
	// "unset" from "specified". Without this, a Subscribe pre-default
	// of AllMessageInterest stomps WithSpecificEvents back to All and
	// the user silently subscribes to everything.
	messageInterestSet bool
	specificEvents     map[types.URN]struct{}
	replay             bool
}

// WithMessageInterest selects which messages the subscription receives.
// Default (no option, no WithSpecificEvents): types.AllMessageInterest.
func WithMessageInterest(m types.MessageInterest) SubscribeOption {
	return func(c *subscribeConfig) {
		c.messageInterest = m
		c.messageInterestSet = true
	}
}

// WithSpecificEvents narrows the subscription to a fixed set of event URNs.
// If the caller has not also called WithMessageInterest, this implies
// SpecifiedMatchesOnlyMessageInterest (event-specific routing keys).
// If the caller called WithMessageInterest explicitly (e.g. AllMessageInterest
// to receive every message and filter client-side), that choice is
// preserved — option order does not matter.
func WithSpecificEvents(events ...types.URN) SubscribeOption {
	return func(c *subscribeConfig) {
		c.specificEvents = make(map[types.URN]struct{}, len(events))
		for _, e := range events {
			c.specificEvents[e] = struct{}{}
		}
		if !c.messageInterestSet {
			c.messageInterest = types.SpecifiedMatchesOnlyMessageInterest
			c.messageInterestSet = true
		}
	}
}

// WithReplay marks the subscription as replay-mode (uses the replay
// exchange and the dummy recovery manager). Equivalent to
// SessionBuilder.BuildReplay() in the legacy API.
func WithReplay() SubscribeOption { return func(c *subscribeConfig) { c.replay = true } }

// Subscription is the v1.0.0 replacement for OddsFeedSession + the
// channel split (session/global). See NEXT.md §4 / §8 Subscriptions.
//
// Lifecycle:
//   - Messages() returns the message stream; the channel closes after a
//     graceful drain or abrupt termination.
//   - Close(ctx) requests a graceful drain; ctx bounds the caller's
//     wait, the drain itself runs on the WithShutdownTimeout budget.
//   - Done() closes when the subscription terminates (any reason).
//   - Err() returns the cause: nil for graceful close, non-nil otherwise.
type Subscription struct {
	id       uuid.UUID
	messages chan types.SessionMessage

	closeOnce sync.Once
	closed    chan struct{}
	closeErr  error

	// underlying is the legacy session this subscription wraps. The
	// adapter goroutine pumps the legacy SessionMessage channel into our
	// outgoing channel.
	underlying sdkOddsFeedSession
	// client is the parent — used by runShutdown to remove this
	// subscription from c.subs once it terminates, so a long-lived
	// client with many short-lived subscriptions doesn't leak entries.
	client          *Client
	pumpDone        chan struct{}
	shutdownTimeout time.Duration

	// pumpStop is the pump's private stop signal, deliberately SEPARATE
	// from `closed` (the public Done). runShutdown must be able to stop
	// and join the pump BEFORE it publishes Done — if the pump selected
	// on `closed` instead, unblocking a stuck send would also return
	// Close() to the caller, letting the consumer race the deadline
	// discard and receive a message after Done() closed.
	pumpStop chan struct{}
}

// Messages returns the message stream. Closes after termination.
//
// SessionMessage is a tagged union: consumers branch on which field is
// set — the embedded EventMessage variants (OddsChange, BetStop,
// BetSettlement, …) for parsed payloads, RawFeedMessage for the
// extended-data side-channel (WithExtendedDataReporting), or
// UnparsableMessage for an in-band decode/build failure.
func (s *Subscription) Messages() <-chan types.SessionMessage { return s.messages }

// Done closes when the subscription terminates for any reason.
func (s *Subscription) Done() <-chan struct{} { return s.closed }

// Err returns the cause of termination. Nil on graceful close.
// Non-nil only after Done() is closed.
func (s *Subscription) Err() error {
	select {
	case <-s.closed:
		return s.closeErr
	default:
		return nil
	}
}

// Close requests a graceful drain. Idempotent. Like Client.Close, the
// supplied ctx bounds the CALLER'S WAIT, not the drain work itself:
//   - On a nil return, shutdown has completed — Done() is closed and
//     Err() reflects the result.
//   - If ctx expires first, Close returns ctx.Err() while the drain
//     CONTINUES in the background; Done() is not yet closed. Call Close
//     again with a fresh context to wait for completion (it joins the
//     same in-flight shutdown).
//
// The drain itself is bounded by ONE ceiling: WithShutdownTimeout
// (NEXT.md §8 Subscriptions — session close AND buffer drain share
// it). The session finishes its in-flight delivery (decode + admit +
// ack — no Nack), stops intake, and every admitted message is drained
// to the consumer; only if that budget expires before the consumer
// read everything are the remaining buffered messages discarded, with
// Err() returning the deadline error. The first caller's ctx VALUES
// propagate into the drain, but its cancellation deliberately does
// not: buffered messages were already ACKed on admission (the queue is
// exclusive+autoDelete, so they exist nowhere else), and a drain
// aborted by a short-lived caller ctx would silently discard them —
// the opposite of the background-continuation contract above, and of
// Client.Close, which roots its shutdown the same way.
func (s *Subscription) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.budget())
		go func() {
			defer cancel()
			s.runShutdown(nil, drainCtx)
		}()
	})

	// Fast path: already done. Completed shutdown always wins over ctx.
	select {
	case <-s.closed:
		return s.closeErr
	default:
	}
	select {
	case <-s.closed:
		return s.closeErr
	case <-ctx.Done():
		select {
		case <-s.closed:
			return s.closeErr
		default:
			return ctx.Err()
		}
	}
}

// abortWithErr is called by the parent Client on abrupt shutdown
// (ctx-cancel / client.Close / terminal error). The subscription terminates
// without draining; the legacy session does its own Nack-on-cancel.
func (s *Subscription) abortWithErr(err error) {
	s.closeOnce.Do(func() {
		// No caller ctx to propagate — Background root capped at the
		// shutdown budget (the documented Background-rooted shutdown
		// chain, NEXT.md §8 Close).
		drainCtx, cancel := context.WithTimeout(context.Background(), s.budget())
		go func() {
			defer cancel()
			s.runShutdown(err, drainCtx)
		}()
	})
}

// budget returns the configured graceful-shutdown ceiling.
func (s *Subscription) budget() time.Duration {
	if s.shutdownTimeout > 0 {
		return s.shutdownTimeout
	}
	return defaultShutdownTimeout
}

// runShutdown drives both termination paths. workCtx is the single
// shared ceiling for every stage — session close, pump wait, and (on
// the graceful path) the consumer-drain wait. Pre-fix each stage got
// its own full budget (session close bounded by shutdownTimeout, then
// the pump wait ANOTHER shutdownTimeout), where NEXT.md §8 "Shutdown
// work budget" requires one shared cap.
func (s *Subscription) runShutdown(terminalErr error, workCtx context.Context) {
	graceful := terminalErr == nil
	closeErr := terminalErr

	// sessionSettled is the feed/session layer's own drain verdict:
	// every handed-over delivery reached its terminal disposition and
	// the session loop exited within workCtx. Defaults true so the
	// abrupt path (which has no drain contract) and nil-session test
	// subscriptions skip the fold below.
	sessionSettled := true
	if s.underlying != nil {
		if graceful {
			// Drain-first: intake stops, the in-flight delivery
			// completes decode + admit + ack, and admitted messages
			// flow through the pump into s.messages.
			sessionSettled = s.underlying.CloseGraceful(workCtx)
		} else {
			// Abrupt: cancel the session loop; the feed layer Nacks
			// the in-flight delivery (recovery covers the gap).
			s.underlying.Close(workCtx)
		}
	}
	if s.pumpDone != nil {
		// Wait for the pump goroutine to finish observing the session's
		// closed message channel.
		//
		// Pump owns close(s.messages) (deferred on its exit), so by the
		// time pumpDone closes, s.messages is already closed. On timeout
		// we deliberately do NOT close s.messages — pump may still be
		// blocked on send, and closing the channel under it would panic.
		// stopPump below unblocks it.
		select {
		case <-s.pumpDone:
		case <-workCtx.Done():
		}
	}
	if graceful {
		// Hold Done() open until the consumer has read every admitted
		// message, or the drain deadline expires — in which case the
		// remainder is discarded and the deadline becomes the terminal
		// cause (NEXT.md §8: "remaining buffered messages are discarded
		// and Err() returns ctx.Err()").
		s.waitBufferDrained(workCtx)
		if workCtx.Err() != nil {
			// Deadline path — taken even when the final poll saw an
			// EMPTY buffer: STOP AND JOIN THE SENDER BEFORE the final
			// drain/discard decision, and only publish Done() after it
			// completed. The pump may still be live (its wait above was
			// abandoned on the same deadline) and holding a delivery:
			// its send-select can beat pumpStop and slip one more
			// already-ACKed message into the buffer AFTER an
			// empty-buffer observation ended the wait — deciding
			// before the join would then close Done() with Err()==nil
			// while an ACKed message sits unread (acked-then-lost,
			// violating the drain-or-discard contract). Equally,
			// discarding concurrently with a live pump could observe a
			// momentarily empty buffer, exit, and have the pump slip
			// one more message into the freed slot. And Done() must
			// close LAST: Close() returns to the caller the moment
			// s.closed closes, so anything after that races the
			// consumer. pumpStop (not s.closed) unblocks the pump
			// precisely so this ordering is possible.
			//
			// After the join the buffer is settled: non-empty means the
			// consumer did NOT read everything — record the deadline and
			// discard; empty means every admitted message was read (a
			// genuine drain completion despite the deadline) — closeErr
			// stays nil.
			joined := s.stopPump()
			if len(s.messages) > 0 {
				closeErr = workCtx.Err()
				s.discardBuffered()
			}
			if closeErr == nil && !joined {
				// Pump parked in broker I/O (blocked Ack) — its work is
				// NOT complete regardless of what the buffer shows.
				closeErr = workCtx.Err()
			}
		}
		// An empty (or fully-read) buffer is NOT sufficient proof of a
		// clean drain: the pump can abandon an undelivered — still
		// unacked — envelope when pumpStop wins its send-select after
		// the deadline (the consumer freeing a slot at that exact
		// moment makes both cases ready), and envelopes can remain
		// queued between session and pump. The session layer tracks
		// per-delivery settlement and reports it; teardown of the
		// exclusive autoDelete queue forfeits redelivery for anything
		// unsettled, so a not-settled drain must surface the deadline
		// as the terminal cause, never nil.
		if closeErr == nil && !sessionSettled {
			// The real session reports not-settled only via ctx expiry,
			// so workCtx.Err() is the cause; the fallback keeps the
			// never-nil invariant against any other implementation.
			if closeErr = workCtx.Err(); closeErr == nil {
				closeErr = context.DeadlineExceeded
			}
		}
	}

	// Stop the pump on every remaining path too (abrupt termination,
	// graceful with a completed drain but a session stuck open) so the
	// goroutine never outlives the subscription. A failed join here
	// (pump parked in a blocked broker Ack) must not report a clean
	// close — the transport teardown finishes the pump later.
	if !s.stopPump() && closeErr == nil {
		closeErr = context.DeadlineExceeded
	}
	// Fold a session terminal error (StrategyThrow) into the result:
	// a graceful Close can win the closeOnce race against the pump's
	// abortWithErr for the FINAL message's decode failure — the abort
	// then no-ops and a nil closeErr would mask the terminal error from
	// both Close's return and Err().
	if closeErr == nil && s.underlying != nil {
		if serr := s.underlying.Err(); serr != nil {
			closeErr = serr
		}
	}
	s.closeErr = closeErr
	s.removeFromClient()
	close(s.closed)
}

// pumpJoinGrace bounds stopPump's join. Generous for a responsive pump
// (which needs only scheduling time past the pumpStop signal), short
// enough that a transport-blocked pump cannot dominate the shutdown
// budget. A fixed grace rather than the caller's workCtx because
// stopPump usually runs AFTER that ctx expired (the deadline-discard
// path) — a pure ctx-bounded select would refuse to wait at all and the
// drain/discard decision would race the live pump, the exact bug the
// join exists to prevent.
const pumpJoinGrace = time.Second

// stopPump signals the pump's private stop channel and joins the pump,
// bounded by pumpJoinGrace. The pump's send-select and receive-select
// both carry the pumpStop case, so it normally exits promptly — but the
// pump is NOT I/O-free past the signal: deliverToSubscription runs the
// broker Ack synchronously after a successful buffer send, and on a
// stalled transport that Ack parks in a network write the pump cannot
// observe pumpStop from. An unbounded join then held runShutdown (and
// with it Done()) hostage to the transport. On a timed-out join the
// pump unwinds later, when the heartbeat/connection teardown fails the
// blocked Ack — until then s.messages stays open (the pump owns its
// close); the deadline error is still reported through the settlement
// fold, never a clean close. Returns whether the pump was actually
// joined. Safe to call multiple times within the single runShutdown
// invocation and with partially constructed test Subscriptions.
func (s *Subscription) stopPump() bool {
	if s.pumpStop != nil {
		select {
		case <-s.pumpStop:
			// already closed
		default:
			close(s.pumpStop)
		}
	}
	if s.pumpDone != nil {
		select {
		case <-s.pumpDone:
		default:
			t := time.NewTimer(pumpJoinGrace)
			defer t.Stop()
			select {
			case <-s.pumpDone:
			case <-t.C:
				return false
			}
		}
	}
	return true
}

// removeFromClient drops this subscription from the client's registry
// so a long-lived Client doesn't accumulate terminated *Subscription
// pointers across many short-lived subscriptions. Safe even if
// Client.runShutdown already snapshotted us — abortWithErr was
// idempotent and the snapshot loop drives the SAME runShutdown.
func (s *Subscription) removeFromClient() {
	if s.client == nil {
		return
	}
	s.client.subsMu.Lock()
	delete(s.client.subs, s.id)
	s.client.subsMu.Unlock()
}

// waitBufferDrained polls until the consumer has emptied s.messages or
// ctx expires. Returns true when the buffer is empty. Polling (10ms) is
// deliberate: the alternative — a consumption-acknowledgement channel —
// would add a synchronization step to every message read for the sole
// benefit of the close path.
func (s *Subscription) waitBufferDrained(ctx context.Context) bool {
	if len(s.messages) == 0 {
		return true
	}
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return len(s.messages) == 0
		case <-tick.C:
			if len(s.messages) == 0 {
				return true
			}
		}
	}
}

// discardBuffered empties whatever is currently buffered on s.messages
// without blocking. Racing consumer reads are fine — either side
// consuming a message counts toward "discarded or read". Safe on both
// an open and a closed channel.
func (s *Subscription) discardBuffered() {
	for {
		select {
		case _, ok := <-s.messages:
			if !ok {
				return
			}
		default:
			return
		}
	}
}
