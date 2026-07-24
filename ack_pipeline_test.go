package gosdk

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/oddin-gg/gosdk/internal/feed"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

// pumpFakeSession is a minimal sdkOddsFeedSession whose RespCh stays
// open so tests can feed envelopes through a live pumpSubscription.
type pumpFakeSession struct {
	respCh chan sessionEnvelope
}

func (f *pumpFakeSession) ID() uuid.UUID                      { return uuid.UUID{} }
func (f *pumpFakeSession) RespCh() <-chan sessionEnvelope     { return f.respCh }
func (f *pumpFakeSession) IsReplay() bool                     { return false }
func (f *pumpFakeSession) Err() error                         { return nil }
func (f *pumpFakeSession) Close(context.Context)              {}
func (f *pumpFakeSession) CloseGraceful(context.Context) bool { return true }
func (f *pumpFakeSession) Open(context.Context, []string, *types.MessageInterest, bool) error {
	return nil
}

func newPumpHarness(t *testing.T, buffer int, handler slog.Handler) (*Client, *pumpFakeSession, *Subscription) {
	t.Helper()
	if handler == nil {
		handler = slog.NewTextHandler(discardWriter{}, nil)
	}
	sess := &pumpFakeSession{respCh: make(chan sessionEnvelope)}
	c := &Client{subs: map[uuid.UUID]*Subscription{}, logger: log.New(slog.New(handler))}
	s := &Subscription{
		id:              uuid.New(),
		messages:        make(chan types.SessionMessage, buffer),
		closed:          make(chan struct{}),
		underlying:      sess,
		client:          c,
		pumpDone:        make(chan struct{}),
		pumpStop:        make(chan struct{}),
		shutdownTimeout: time.Second,
	}
	c.subs[s.id] = s
	c.wg.Add(1)
	go c.pumpSubscription(s)
	t.Cleanup(func() {
		select {
		case <-s.pumpDone:
		default:
			close(sess.respCh)
			<-s.pumpDone
		}
	})
	return c, sess, s
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// TestPump_AcksOnlyAfterPublicBufferAdmission pins the seventh-pass P1:
// the broker ack fires at the pump's successful send into the PUBLIC
// subscription buffer — NEXT.md §0.6 "acked only after admitted to the
// subscription buffer" — not at any earlier pipeline hop. With the
// buffer full, the envelope's ack must NOT fire; it fires only once the
// consumer frees a slot and the held message lands.
func TestPump_AcksOnlyAfterPublicBufferAdmission(t *testing.T) {
	_, sess, s := newPumpHarness(t, 1, nil)

	var ack1, ack2 atomic.Int32
	// First envelope: buffer has room — delivered and acked.
	sess.respCh <- sessionEnvelope{ack: func() { ack1.Add(1) }}
	waitFor(t, func() bool { return ack1.Load() == 1 }, "first envelope acked on admission")

	// Second envelope: buffer is FULL (nobody consumed msg1). The pump
	// holds it; the ack must not fire while the message is undeliverable.
	sess.respCh <- sessionEnvelope{ack: func() { ack2.Add(1) }}
	time.Sleep(100 * time.Millisecond)
	if got := ack2.Load(); got != 0 {
		t.Fatalf("ack fired while the subscription buffer was full (acks=%d) — delivery acked before public admission", got)
	}

	// Consumer drains one slot: the held message lands, THEN acks.
	<-s.Messages()
	waitFor(t, func() bool { return ack2.Load() == 1 }, "held envelope acked after buffer freed")
}

// TestPump_AbortedEnvelopeStaysUnacked pins the abrupt half: an envelope
// the pump holds when pumpStop closes is dropped UNACKED — the broker
// releases it on channel close; nothing is acked-then-lost.
func TestPump_AbortedEnvelopeStaysUnacked(t *testing.T) {
	_, sess, s := newPumpHarness(t, 1, nil)

	var acks atomic.Int32
	sess.respCh <- sessionEnvelope{ack: func() { acks.Add(1) }} // fills buffer, acked
	waitFor(t, func() bool { return acks.Load() == 1 }, "first envelope acked")
	sess.respCh <- sessionEnvelope{ack: func() { acks.Add(1) }} // pump blocks holding it

	time.Sleep(50 * time.Millisecond)
	close(s.pumpStop)
	<-s.pumpDone
	if got := acks.Load(); got != 1 {
		t.Fatalf("acks = %d, want 1 — the aborted in-hand envelope must stay unacked", got)
	}
}

// TestPump_SlowConsumerWarnsAtPublicBuffer pins the seventh-pass P2: the
// slow-consumer warning is measured at the REAL blockage point — the
// pump's send into Messages() — once per detection window while the
// public buffer stays full, reporting that channel's capacity. (It can
// no longer fire while Messages() has room, because this is the only
// place it exists.)
func TestPump_SlowConsumerWarnsAtPublicBuffer(t *testing.T) {
	oldInterval := slowConsumerWarnInterval
	slowConsumerWarnInterval = 30 * time.Millisecond
	defer func() { slowConsumerWarnInterval = oldInterval }()

	rec := &recordingHandler{}
	_, sess, s := newPumpHarness(t, 1, rec)

	sess.respCh <- sessionEnvelope{} // fills the buffer
	sess.respCh <- sessionEnvelope{} // pump blocks; warn windows elapse

	deadline := time.Now().Add(2 * time.Second)
	for count(rec.messages(), "subscription buffer full") < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("slow-consumer warnings = %d, want >= 2 (one per window); msgs=%v",
				count(rec.messages(), "subscription buffer full"), rec.messages())
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Unblock and verify the message still arrives (warn is observability,
	// not action).
	<-s.Messages()
	select {
	case <-s.Messages():
	case <-time.After(time.Second):
		t.Fatal("held message not delivered after consumer resumed")
	}
}

// TestSession_TerminalDrops_AckAtDropPoint pins the session-side half of
// the pipeline-ack contract: a delivery the session terminally consumes
// or drops (nil payload, alive bookkeeping) is acked at that drop point,
// while a StrategyThrow termination leaves the delivery unacked.
func TestSession_TerminalDrops_AckAtDropPoint(t *testing.T) {
	var acks atomic.Int32
	o := &oddsFeedSessionImpl{
		exceptionStrategy: StrategyCatch,
		msgCh:             make(chan sessionEnvelope, 4),
		logger:            discardLogger(),
	}

	// Nil message == nothing to deliver == intentional drop: acked.
	mi := types.AllMessageInterest
	o.processMessage(t.Context(), feed.QueueEnvelope{Msg: nil, Ack: func() { acks.Add(1) }}, &mi, false)
	if got := acks.Load(); got != 1 {
		t.Fatalf("acks after nil-payload drop = %d, want 1", got)
	}

	// Unparsable under Catch: the ack must RIDE the emitted envelope
	// (fired later by the pump), not fire inside the session.
	var rideAck atomic.Int32
	o.emitUnparsable(t.Context(), nil, errFeedMessageDecode, func() { rideAck.Add(1) }, false, nil)
	env := <-o.msgCh
	if rideAck.Load() != 0 {
		t.Fatal("unparsable ack fired inside the session — must ride the envelope to the pump")
	}
	if env.ack == nil {
		t.Fatal("unparsable envelope carries no ack")
	}
	env.ack()
	if rideAck.Load() != 1 {
		t.Fatal("unparsable envelope ack did not fire the delivery ack")
	}

	// StrategyThrow: session terminates without handling the delivery —
	// the ack must NOT fire (broker redelivers/releases).
	var throwAck atomic.Int32
	thrower := &oddsFeedSessionImpl{
		exceptionStrategy: StrategyThrow,
		msgCh:             make(chan sessionEnvelope, 1),
		closeFn:           func() {},
		logger:            discardLogger(),
	}
	thrower.emitUnparsable(t.Context(), nil, errFeedMessageDecode, func() { throwAck.Add(1) }, false, nil)
	if got := throwAck.Load(); got != 0 {
		t.Fatalf("acks after StrategyThrow termination = %d, want 0 (unhandled delivery stays unacked)", got)
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func count(msgs []string, substr string) int {
	n := 0
	for _, m := range msgs {
		if strings.Contains(m, substr) {
			n++
		}
	}
	return n
}
