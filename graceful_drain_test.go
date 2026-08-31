package gosdk

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/oddin-gg/gosdk/types"
)

// drainFakeSession is a minimal sdkOddsFeedSession for Subscription
// shutdown tests. Its message channel is pre-closed (the pump has
// nothing to forward), so tests exercise the drain logic in isolation.
type drainFakeSession struct {
	respCh          chan sessionEnvelope
	gracefulCalls   int
	abruptCalls     int
	lastGracefulCtx context.Context
}

func newDrainFakeSession() *drainFakeSession {
	ch := make(chan sessionEnvelope)
	close(ch)
	return &drainFakeSession{respCh: ch}
}

func (f *drainFakeSession) ID() uuid.UUID                  { return uuid.UUID{} }
func (f *drainFakeSession) RespCh() <-chan sessionEnvelope { return f.respCh }
func (f *drainFakeSession) IsReplay() bool                 { return false }
func (f *drainFakeSession) Err() error                     { return nil }
func (f *drainFakeSession) Close(ctx context.Context)      { f.abruptCalls++ }
func (f *drainFakeSession) CloseGraceful(ctx context.Context) bool {
	f.gracefulCalls++
	f.lastGracefulCtx = ctx
	return true // pre-closed RespCh: nothing outstanding, drain trivially settles
}
func (f *drainFakeSession) Open(context.Context, []string, *types.MessageInterest, bool) error {
	return nil
}

func newDrainTestSubscription(fake *drainFakeSession, buffered int, budget time.Duration) *Subscription {
	pumpDone := make(chan struct{})
	close(pumpDone) // pump already exited; messages buffer is the drain surface
	s := &Subscription{
		id:              uuid.New(),
		messages:        make(chan types.SessionMessage, buffered+1),
		closed:          make(chan struct{}),
		underlying:      fake,
		pumpDone:        pumpDone,
		shutdownTimeout: budget,
	}
	for i := 0; i < buffered; i++ {
		s.messages <- types.SessionMessage{}
	}
	return s
}

// TestSubscriptionClose_GracefulDrainsToConsumer pins the NEXT.md §8
// graceful contract (Codex P1): Close(ctx) holds Done() open until the
// consumer has read every admitted message; Err() is nil; the session
// was closed via the GRACEFUL path.
func TestSubscriptionClose_GracefulDrainsToConsumer(t *testing.T) {
	fake := newDrainFakeSession()
	s := newDrainTestSubscription(fake, 3, 2*time.Second)

	got := make(chan int, 1)
	go func() {
		n := 0
		for range s.messages {
			n++
		}
		got <- n
	}()

	// The consumer above drains the buffer; messages closes only after
	// we close it — the pump normally owns that, so emulate it once the
	// subscription reports Done.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	close(s.messages)

	if err := s.Err(); err != nil {
		t.Fatalf("Err() = %v, want nil after graceful drain", err)
	}
	if fake.gracefulCalls != 1 || fake.abruptCalls != 0 {
		t.Fatalf("session close calls: graceful=%d abrupt=%d, want 1/0", fake.gracefulCalls, fake.abruptCalls)
	}
	if n := <-got; n != 3 {
		t.Fatalf("consumer read %d messages, want all 3", n)
	}
}

// TestSubscriptionClose_DeadlineDiscardsAndReportsCtxErr pins the
// deadline half of the contract: with no consumer reading, the drain
// deadline expires, the buffered remainder is discarded, and Err()
// returns the ctx error. It also pins the SINGLE shared budget: session
// close + pump wait + drain previously each got a full shutdownTimeout;
// now the whole sequence must finish within roughly one budget.
func TestSubscriptionClose_DeadlineDiscardsAndReportsCtxErr(t *testing.T) {
	const budget = 200 * time.Millisecond
	fake := newDrainFakeSession()
	s := newDrainTestSubscription(fake, 3, budget)

	start := time.Now()
	err := s.Close(context.Background()) // no caller deadline: budget rules
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close = %v, want DeadlineExceeded", err)
	}
	if !errors.Is(s.Err(), context.DeadlineExceeded) {
		t.Fatalf("Err() = %v, want DeadlineExceeded", s.Err())
	}
	if len(s.messages) != 0 {
		t.Fatalf("%d messages still buffered, want 0 (discarded on deadline)", len(s.messages))
	}
	// One shared ceiling: well under 2× the budget even though session
	// close, pump wait, and drain all ran.
	if elapsed > 3*budget {
		t.Fatalf("Close took %v — stages are not sharing the %v budget", elapsed, budget)
	}
}

// TestSubscriptionClose_CallerCtxBoundsOnlyTheWait pins the
// background-continuation half of the Close contract: a short-lived
// caller ctx ends the WAIT (Close returns ctx.Err()) but must not
// abort the drain — the buffered messages were already ACKed on
// admission and exist nowhere else, so a drain cancelled by the
// caller's ctx silently discarded them (pre-fix the drain ctx was a
// CHILD of the caller's). The drain finishes under its own budget, a
// consumer can still read everything, and a second Close with a fresh
// ctx joins the same shutdown and reports the graceful result.
func TestSubscriptionClose_CallerCtxBoundsOnlyTheWait(t *testing.T) {
	fake := newDrainFakeSession()
	s := newDrainTestSubscription(fake, 3, 2*time.Second)

	shortCtx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := s.Close(shortCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close(shortCtx) = %v, want DeadlineExceeded (the wait is bounded)", err)
	}
	select {
	case <-s.Done():
		t.Fatal("Done() closed while the consumer had read nothing — drain aborted by the caller ctx")
	default:
	}

	// The consumer shows up late; the still-running drain must deliver
	// every buffered message.
	got := make(chan int, 1)
	go func() {
		n := 0
		for range s.messages {
			n++
		}
		got <- n
	}()

	ctx, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	if err := s.Close(ctx); err != nil {
		t.Fatalf("second Close: %v (must join the in-flight shutdown and report its result)", err)
	}
	close(s.messages)
	if err := s.Err(); err != nil {
		t.Fatalf("Err() = %v, want nil after graceful drain", err)
	}
	if n := <-got; n != 3 {
		t.Fatalf("consumer read %d messages, want all 3 (drain must survive the first caller's ctx)", n)
	}
}

// TestSubscriptionAbort_KeepsBufferReadable pins the abrupt path: Done()
// closes with the terminal cause, and already-admitted messages stay
// READABLE (abort discards nothing — only graceful-deadline does).
func TestSubscriptionAbort_KeepsBufferReadable(t *testing.T) {
	fake := newDrainFakeSession()
	s := newDrainTestSubscription(fake, 2, time.Second)

	sentinel := errors.New("boom")
	s.abortWithErr(sentinel)
	select {
	case <-s.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done() not closed after abort")
	}
	if !errors.Is(s.Err(), sentinel) {
		t.Fatalf("Err() = %v, want sentinel", s.Err())
	}
	if fake.abruptCalls != 1 || fake.gracefulCalls != 0 {
		t.Fatalf("session close calls: graceful=%d abrupt=%d, want 0/1", fake.gracefulCalls, fake.abruptCalls)
	}
	if got := len(s.messages); got != 2 {
		t.Fatalf("buffered after abort = %d, want 2 (still readable)", got)
	}
}

// TestNew_EmptyToken_ErrInvalidConfig pins the exported sentinel and
// up-front validation: no HTTP is attempted for a config that cannot
// work (NEXT.md §10/§12).
func TestNew_EmptyToken_ErrInvalidConfig(t *testing.T) {
	_, err := New(t.Context(), NewConfig("", types.IntegrationEnvironment))
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New = %v, want ErrInvalidConfig", err)
	}
}

// TestSubscribeAfterClose_ErrAlreadyClosed pins the exported
// ErrAlreadyClosed sentinel on the closed-client path.
func TestSubscribeAfterClose_ErrAlreadyClosed(t *testing.T) {
	c := newCatalogClient(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := c.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := c.Subscribe(ctx); !errors.Is(err, ErrAlreadyClosed) {
		t.Fatalf("Subscribe after Close = %v, want ErrAlreadyClosed", err)
	}
	if err := c.Connect(ctx); !errors.Is(err, ErrAlreadyClosed) {
		t.Fatalf("Connect after Close = %v, want ErrAlreadyClosed", err)
	}
}

// stuckSession simulates a session whose loop cannot finish draining:
// RespCh stays open and CloseGraceful/Close return without closing it,
// leaving the pump alive — the shape behind the Codex P1 deadline race.
type stuckSession struct {
	respCh chan sessionEnvelope
}

func (f *stuckSession) ID() uuid.UUID                      { return uuid.UUID{} }
func (f *stuckSession) RespCh() <-chan sessionEnvelope     { return f.respCh }
func (f *stuckSession) IsReplay() bool                     { return false }
func (f *stuckSession) Err() error                         { return nil }
func (f *stuckSession) Close(context.Context)              {}
func (f *stuckSession) CloseGraceful(context.Context) bool { return false } // stuck: never settles
func (f *stuckSession) Open(context.Context, []string, *types.MessageInterest, bool) error {
	return nil
}

// TestSubscriptionClose_Deadline_NoDeliveryAfterDone pins the Codex P1
// deadline race: with the pump still LIVE and blocked in a buffer send,
// the deadline path must stop and join the pump BEFORE discarding —
// otherwise the pump could slip its in-flight message into the slot
// discard just freed, delivering it AFTER Done() closed. Post-fix,
// once Close returns on the deadline path, Messages() is closed and
// empty: nothing can surface after Done().
func TestSubscriptionClose_Deadline_NoDeliveryAfterDone(t *testing.T) {
	sess := &stuckSession{respCh: make(chan sessionEnvelope, 2)}
	sess.respCh <- sessionEnvelope{} // msg1: fills the buffer
	sess.respCh <- sessionEnvelope{} // msg2: pump blocks sending it

	c := &Client{subs: map[uuid.UUID]*Subscription{}}
	s := &Subscription{
		id:              uuid.New(),
		messages:        make(chan types.SessionMessage, 1),
		closed:          make(chan struct{}),
		underlying:      sess,
		client:          c,
		pumpDone:        make(chan struct{}),
		pumpStop:        make(chan struct{}),
		shutdownTimeout: 200 * time.Millisecond,
	}
	c.subs[s.id] = s
	c.wg.Add(1)
	go c.pumpSubscription(s)

	err := s.Close(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close = %v, want DeadlineExceeded", err)
	}
	select {
	case <-s.Done():
	default:
		t.Fatal("Done() not closed after Close returned")
	}
	// The load-bearing assertion: nothing is deliverable after Done().
	// The pump was joined and the buffer discarded, so Messages() is
	// closed and empty — a receive must fail immediately.
	select {
	case msg, ok := <-s.Messages():
		if ok {
			t.Fatalf("received a message AFTER Done() closed: %+v", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("Messages() still open after deadline Close")
	}
}

// TestSubscription_GracefulDeadline_NeverNilErrWithUnreadAckedMessage is
// the regression for the deadline/empty-buffer race: waitBufferDrained
// treated an empty buffer as drain success even when workCtx had already
// expired and the pump wait had been abandoned with the pump still live.
// runShutdown then stopped the pump WITHOUT re-deciding — and the pump's
// send-select (real deliverToSubscription) can beat pumpStop, enqueue and
// ACK one final message, and exit. Done() closed with Err()==nil while an
// ACKed message sat unread in the closed channel — acked-then-lost under
// a nil-error drain report. Post-fix, the deadline path joins the pump
// FIRST and decides drain/discard on the settled buffer, so nil Err
// implies an empty buffer. The select race is probabilistic — iterate.
func TestSubscription_GracefulDeadline_NeverNilErrWithUnreadAckedMessage(t *testing.T) {
	for i := 0; i < 40; i++ {
		s := &Subscription{
			id:       uuid.New(),
			messages: make(chan types.SessionMessage, 4),
			closed:   make(chan struct{}),
			pumpStop: make(chan struct{}),
			pumpDone: make(chan struct{}),
		}
		c := &Client{logger: discardLogger()}

		var acked atomic.Int32
		env := sessionEnvelope{
			msg: types.SessionMessage{},
			ack: func() { acked.Add(1) },
		}
		// Live pump holding one delivery: uses the REAL send/ack logic.
		go func() {
			defer close(s.pumpDone)
			defer close(s.messages)
			c.deliverToSubscription(s, env)
		}()

		// Drain budget already expired when the graceful path runs.
		expired, cancel := context.WithCancel(context.Background())
		cancel()
		s.runShutdown(nil, expired)

		if s.closeErr == nil && len(s.messages) > 0 {
			t.Fatalf("iteration %d: Done() with Err()==nil while %d ACKed message(s) sit unread (acked=%d)",
				i, len(s.messages), acked.Load())
		}
	}
}

// TestSubscription_GracefulClose_UnsettledDeliveryNeverNilErr is the
// regression for the empty-buffer half of the deadline race (Codex P1):
// at pump-join time the public buffer can be EMPTY — the consumer read
// everything that was admitted — while the feed/session layer still
// holds an UNSETTLED delivery (the pump abandoned an undelivered,
// unacked envelope when pumpStop won its send-select, or envelopes
// remained queued between session and pump). Pre-fix, runShutdown judged
// the drain by final buffer length alone and reported Err()==nil for a
// close that silently dropped a message whose redelivery the queue
// teardown forfeited. Post-fix the session's settlement verdict is
// plumbed through and the deadline becomes the terminal cause.
func TestSubscription_GracefulClose_UnsettledDeliveryNeverNilErr(t *testing.T) {
	sess := &stuckSession{respCh: make(chan sessionEnvelope)} // open + empty: pump idles; CloseGraceful reports not-settled
	c := &Client{subs: map[uuid.UUID]*Subscription{}, logger: discardLogger()}
	s := &Subscription{
		id:              uuid.New(),
		messages:        make(chan types.SessionMessage, 4), // stays empty — nothing is ever admitted
		closed:          make(chan struct{}),
		underlying:      sess,
		client:          c,
		pumpDone:        make(chan struct{}),
		pumpStop:        make(chan struct{}),
		shutdownTimeout: 200 * time.Millisecond,
	}
	c.subs[s.id] = s
	c.wg.Add(1)
	go c.pumpSubscription(s)

	err := s.Close(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close = %v, want DeadlineExceeded (unsettled delivery must not report a clean drain)", err)
	}
	if !errors.Is(s.Err(), context.DeadlineExceeded) {
		t.Fatalf("Err() = %v, want DeadlineExceeded", s.Err())
	}
	if n := len(s.messages); n != 0 {
		t.Fatalf("buffer holds %d messages, want 0 — the point is err despite an EMPTY buffer", n)
	}
}

// TestSubscriptionClose_BlockedAckDoesNotHangShutdown is the regression
// for the shutdown half of the blocking-transport P1: the pump runs the
// broker Ack synchronously after a successful buffer send, and on a
// stalled transport that Ack parks in a network write. stopPump's join
// was UNBOUNDED — Close never returned, Done() never closed, and the
// public lifecycle guarantee failed. Post-fix the join is bounded by a
// short grace, the deadline is reported (never a clean close), and the
// pump is left to unwind when the transport teardown fails the Ack.
func TestSubscriptionClose_BlockedAckDoesNotHangShutdown(t *testing.T) {
	sess := &stuckSession{respCh: make(chan sessionEnvelope, 1)}
	c := &Client{subs: map[uuid.UUID]*Subscription{}, logger: discardLogger()}
	s := &Subscription{
		id:              uuid.New(),
		messages:        make(chan types.SessionMessage, 1),
		closed:          make(chan struct{}),
		underlying:      sess,
		client:          c,
		pumpDone:        make(chan struct{}),
		pumpStop:        make(chan struct{}),
		shutdownTimeout: 200 * time.Millisecond,
	}
	c.subs[s.id] = s
	c.wg.Add(1)
	go c.pumpSubscription(s)

	ackBlocked := make(chan struct{})
	ackRelease := make(chan struct{})
	sess.respCh <- sessionEnvelope{ack: func() {
		close(ackBlocked)
		<-ackRelease // simulates an Ack parked in a stalled network write
	}}
	<-ackBlocked // pump is now inside the blocked Ack, past its send

	start := time.Now()
	err := s.Close(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Close returned nil while the pump is parked in a blocked Ack")
	}
	select {
	case <-s.Done():
	default:
		t.Fatal("Done() not closed after Close returned")
	}
	// Budget (200ms) + bounded pump-join grace (1s) + slack — the
	// pre-fix unbounded join never returned at all.
	if elapsed > 5*time.Second {
		t.Fatalf("Close took %v — pump join is not bounded", elapsed)
	}

	// Restored Done ⇒ Messages-closed ordering: the ACK is decoupled from
	// the pump (settleAck runs it on its own goroutine), so the pump
	// abandons the wedged ACK on pumpStop, returns, and closes
	// s.messages — WITHOUT the ACK ever being released. Pre-decoupling
	// the pump was pinned inside the synchronous ACK and Messages() stayed
	// open forever while Done() was closed.
	select {
	case <-s.pumpDone:
	case <-time.After(2 * time.Second):
		t.Fatal("pump did not close messages while the Ack was still wedged — Done/Messages ordering broken")
	}
	if _, open := <-s.Messages(); open {
		t.Fatal("Messages() delivered after Done() with a wedged Ack; want closed")
	}

	close(ackRelease) // release the detached ACK goroutine (cleanup)
}
