package feed

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/oddin-gg/gosdk/internal/factory"
	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

// fakeChannelOpener is a test double for the feed Client's CreateChannel,
// so the consumer's synchronous first-channel handshake can be exercised
// without a live broker.
type fakeChannelOpener struct {
	deliveries <-chan amqp.Delivery
	ch         *amqp.Channel
	err        error
	calls      atomic.Int32
}

func (f *fakeChannelOpener) CreateChannel(context.Context, []string, string, int) (<-chan amqp.Delivery, *amqp.Channel, error) {
	f.calls.Add(1)
	return f.deliveries, f.ch, f.err
}

func discardConsumerLogger() *log.Logger {
	return log.New(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// countingAcknowledger records acks/nacks without failing them.
type countingAcknowledger struct {
	acks  atomic.Int32
	nacks atomic.Int32
}

func (c *countingAcknowledger) Ack(uint64, bool) error        { c.acks.Add(1); return nil }
func (c *countingAcknowledger) Nack(uint64, bool, bool) error { c.nacks.Add(1); return nil }
func (c *countingAcknowledger) Reject(uint64, bool) error     { return nil }

func newDrainTestConsumer() *ChannelConsumer {
	logger := log.New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	mi := types.AllMessageInterest
	return &ChannelConsumer{
		feedMessageFactory: &factory.FeedMessageFactory{},
		logger:             logger,
		messageInterest:    &mi,
		outgoing:           make(chan QueueEnvelope, 4),
		drainCh:            make(chan struct{}),
		closed:             make(chan struct{}),
	}
}

func testDelivery(ack amqp.Acknowledger) amqp.Delivery {
	return amqp.Delivery{
		Acknowledger: ack,
		RoutingKey:   "hi.pre.live.odds_change.1.match.123.-",
		Body:         []byte("garbage-not-xml"), // decodes as Unparsable — fine for admission
		Timestamp:    time.Unix(1_700_000_000, 0).UTC(),
	}
}

// TestConsume_GracefulDrain_AdmitsInFlightNoAckNoNack pins the graceful-
// close contract under pipeline ack ownership (NEXT.md §0.6 / §8): the
// delivery being processed completes its decode + admit cycle — no Nack —
// and the CONSUMER never acks; the envelope carries the ack downstream
// and the broker is acknowledged only after the message reaches the
// public subscription buffer. Invoking the admitted envelope's Ack fires
// the underlying delivery's Ack exactly once.
func TestConsume_GracefulDrain_AdmitsInFlightNoAckNoNack(t *testing.T) {
	c := newDrainTestConsumer()
	ack := &countingAcknowledger{}

	deliveries := make(chan amqp.Delivery, 1)
	deliveries <- testDelivery(ack)

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.consume(context.Background(), deliveries, nil)
	}()

	// Wait for the delivery to be admitted.
	var env QueueEnvelope
	select {
	case env = <-c.outgoing:
		if env.Msg == nil {
			t.Fatal("admitted message is nil")
		}
		if env.Ack == nil {
			t.Fatal("admitted envelope carries no Ack closure")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("delivery was not admitted")
	}

	// Drain with no further deliveries queued: consume must exit
	// promptly rather than keep blocking on the delivery channel.
	close(c.drainCh)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("consume did not exit after drain signal")
	}
	// The consumer must NOT have acked — ack ownership moved downstream.
	if a := ack.acks.Load(); a != 0 {
		t.Fatalf("acks = %d, want 0 (consumer must not ack on admission)", a)
	}
	if n := ack.nacks.Load(); n != 0 {
		t.Fatalf("nacks = %d, want 0 (graceful close must not Nack)", n)
	}
	// Firing the envelope's ack acknowledges the underlying delivery.
	env.Ack()
	if a := ack.acks.Load(); a != 1 {
		t.Fatalf("acks after env.Ack() = %d, want 1", a)
	}
}

// TestConsume_AbruptCancel_NacksInFlight pins the abrupt path: a ctx
// cancellation while the admit-send is blocked (stalled session) Nacks
// the in-hand delivery (requeue=false — recovery covers the gap).
func TestConsume_AbruptCancel_NacksInFlight(t *testing.T) {
	c := newDrainTestConsumer()
	c.outgoing = make(chan QueueEnvelope) // unbuffered, nobody reads
	ack := &countingAcknowledger{}

	deliveries := make(chan amqp.Delivery, 1)
	deliveries <- testDelivery(ack)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.consume(ctx, deliveries, nil)
	}()

	// Give the loop a moment to pull the delivery and block on the
	// admit-send, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("consume did not exit on ctx cancel")
	}
	if n := ack.nacks.Load(); n != 1 {
		t.Fatalf("nacks = %d, want 1 (abrupt cancel Nacks the in-flight delivery)", n)
	}
	if a := ack.acks.Load(); a != 0 {
		t.Fatalf("acks = %d, want 0", a)
	}
}

// TestBetCancelFixture_StartTimeIsInt64Millis pins the *int64 widening
// of bet_cancel start/end times against the repo's own fixture (13-digit
// millisecond values that overflow int32 and don't fit int on 32-bit).
func TestBetCancelFixture_StartTimeIsInt64Millis(t *testing.T) {
	raw, err := os.ReadFile("xml/testdata/bet_cancel.xml")
	if err != nil {
		// The fixture is TRACKED — a missing file means the checkout or
		// a rename broke the test's subject, and t.Skip would silently
		// retire the int64-widening regression.
		t.Fatalf("tracked fixture missing: %v", err)
	}
	msg, err := feedXML.Decode(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	bc, ok := msg.(*feedXML.BetCancel)
	if !ok {
		t.Fatalf("decoded %T, want *feedXML.BetCancel", msg)
	}
	if bc.StartTime == nil {
		t.Fatal("StartTime is nil")
	}
	if *bc.StartTime != 1777800000000 {
		t.Fatalf("StartTime = %d, want 1777800000000", *bc.StartTime)
	}
}

// TestConsume_DrainAlreadySignalled_NoIntake pins the hard intake bound
// (Codex P2): when the drain signal is already set, consume must exit
// WITHOUT pulling queued deliveries — the per-iteration non-blocking
// pre-check removes the select race that previously let a continuous
// delivery stream keep winning against drainCh with no upper bound.
func TestConsume_DrainAlreadySignalled_NoIntake(t *testing.T) {
	c := newDrainTestConsumer()
	ack := &countingAcknowledger{}

	deliveries := make(chan amqp.Delivery, 4)
	for i := 0; i < 4; i++ {
		deliveries <- testDelivery(ack)
	}
	close(c.drainCh) // drain requested before the loop even starts

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.consume(context.Background(), deliveries, nil)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("consume did not exit")
	}
	if got := len(deliveries); got != 4 {
		t.Fatalf("deliveries consumed after drain: %d left, want 4 (zero intake)", got)
	}
	if a, n := ack.acks.Load(), ack.nacks.Load(); a != 0 || n != 0 {
		t.Fatalf("acks=%d nacks=%d, want 0/0", a, n)
	}
}

// TestOpen_StagingChannelUnbuffered pins the pipeline shape: the
// consumer's staging channel must be UNBUFFERED so the subscription's
// configured channel is the only elastic stage in the pipeline — a
// hidden buffered staging queue would park messages invisibly, and
// under the ack-at-pump contract would inflate the unacked in-pipeline
// window past the documented in-hop holds.
func TestOpen_StagingChannelUnbuffered(t *testing.T) {
	// Synchronous first-channel handshake succeeds; run() then consumes
	// the (empty) deliveries channel until Close cancels the loop ctx.
	opener := &fakeChannelOpener{deliveries: make(chan amqp.Delivery)}
	c := NewChannelConsumer(opener, &factory.FeedMessageFactory{},
		discardConsumerLogger(), "ex", "od:sport:", 0)
	mi := types.AllMessageInterest
	out, err := c.Open(context.Background(), []string{"k"}, &mi)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = c.Close(ctx)
	}()
	if got := cap(out); got != 0 {
		t.Fatalf("staging channel cap = %d, want 0 (unbuffered)", got)
	}
	if got := opener.calls.Load(); got != 1 {
		t.Fatalf("CreateChannel calls = %d, want 1 (first channel created synchronously in Open)", got)
	}
}

// TestOpen_SurfacesChannelSetupError pins the P1 fix: a permanent channel
// setup error (exchange/permission/topology) surfaces synchronously from
// Open instead of being retried forever inside run() — and leaves nothing
// committed, so a fresh Open can retry.
func TestOpen_SurfacesChannelSetupError(t *testing.T) {
	wantErr := errors.New("declare queue: ACCESS_REFUSED")
	opener := &fakeChannelOpener{err: wantErr}
	c := NewChannelConsumer(opener, &factory.FeedMessageFactory{},
		discardConsumerLogger(), "ex", "od:sport:", 0)
	mi := types.AllMessageInterest

	out, err := c.Open(context.Background(), []string{"k"}, &mi)
	if err == nil {
		t.Fatal("Open returned nil despite the first channel setup failing")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("Open err = %v, want it to wrap %v", err, wantErr)
	}
	if out != nil {
		t.Fatal("Open returned a non-nil channel on failure")
	}
	if got := opener.calls.Load(); got != 1 {
		t.Fatalf("CreateChannel calls = %d, want 1 (no forever-retry)", got)
	}

	// Nothing was committed on failure — a fresh Open retries and, on a
	// now-successful setup, succeeds.
	opener.err = nil
	opener.deliveries = make(chan amqp.Delivery)
	if _, err := c.Open(context.Background(), []string{"k"}, &mi); err != nil {
		t.Fatalf("second Open after the transient failure cleared: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = c.Close(ctx)
}

// TestCloseGraceful_WaitsForDeliverySettlement is the regression for
// the lost-final-delivery P1: on graceful drain, run() used to close
// the AMQP channel (deleting the exclusive autoDelete queue) as soon as
// consume returned — while the last handed-over delivery's ack fires
// only on PUBLIC-buffer admission, which can lag behind a full buffer.
// The delivery then became unacked AND unredeliverable. CloseGraceful
// must not tear the channel down until every handed-over delivery
// reached its terminal disposition (or the drain deadline expired).
func TestCloseGraceful_WaitsForDeliverySettlement(t *testing.T) {
	c := newDrainTestConsumer()
	c.client = &fakeChannelOpener{}
	ack := &countingAcknowledger{}

	deliveries := make(chan amqp.Delivery, 1)
	deliveries <- testDelivery(ack)

	loopCtx, cancelLoop := context.WithCancel(context.Background())
	defer cancelLoop()
	c.loopCtx = loopCtx
	c.closeFn = cancelLoop
	c.wg.Go(func() { c.run(loopCtx, deliveries, nil) })

	// Receive the envelope like the session would — but do NOT fire its
	// ack yet (a full public buffer downstream).
	env := <-c.outgoing

	// Graceful close on another goroutine: it must BLOCK on settlement.
	done := make(chan bool, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done <- c.CloseGraceful(ctx)
	}()

	select {
	case <-done:
		t.Fatal("CloseGraceful returned before the outstanding delivery settled")
	case <-time.After(100 * time.Millisecond):
		// Correct: still waiting on settlement.
	}

	env.Ack() // terminal disposition — settlement complete
	select {
	case settled := <-done:
		if !settled {
			t.Fatal("CloseGraceful reported not-settled after full settlement")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CloseGraceful did not return after settlement")
	}
	if ack.acks.Load() != 1 {
		t.Fatalf("acks = %d, want 1", ack.acks.Load())
	}
}

// TestCloseGraceful_ReportsUnsettledOnDeadline pins the honest-status
// half of the settlement contract (Codex P1 follow-up): when the drain
// deadline expires with a handed-over delivery still unsettled,
// CloseGraceful must return false — the closing teardown deletes the
// exclusive autoDelete queue and forfeits redelivery, so callers need to
// surface the deadline instead of reporting a clean drain. Pre-fix the
// settlement result was discarded (and a waiter goroutine leaked on a
// WaitGroup that could never complete).
func TestCloseGraceful_ReportsUnsettledOnDeadline(t *testing.T) {
	c := newDrainTestConsumer()
	c.client = &fakeChannelOpener{}
	ack := &countingAcknowledger{}

	deliveries := make(chan amqp.Delivery, 1)
	deliveries <- testDelivery(ack)

	loopCtx, cancelLoop := context.WithCancel(context.Background())
	defer cancelLoop()
	c.loopCtx = loopCtx
	c.closeFn = cancelLoop
	c.wg.Go(func() { c.run(loopCtx, deliveries, nil) })

	// Receive the envelope like the session would — its ack NEVER fires
	// (the downstream pump abandoned it).
	env := <-c.outgoing
	_ = env

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if settled := c.CloseGraceful(ctx); settled {
		t.Fatal("CloseGraceful reported settled=true with an unsettled delivery outstanding")
	}
	if ack.acks.Load() != 0 {
		t.Fatalf("acks = %d, want 0 (nothing settled)", ack.acks.Load())
	}
}

// fakeAMQPChannel is an amqpChannel test double that records Close.
type fakeAMQPChannel struct{ closed atomic.Int32 }

func (f *fakeAMQPChannel) Close() error { f.closed.Add(1); return nil }

// TestCloseGracefulChannel_ClosesDespiteAbandonedDelivery is the
// regression for the graceful-drain leak (3/3-reviewer Require Change):
// on the drain DEADLINE path a delivery handed downstream is routinely
// ABANDONED — its ack closure never runs, so unsettledN never returns to
// 0. Pre-fix runShutdown handed the channel to a reaper that waited on
// full settlement forever, leaking the exclusive autoDelete queue and a
// goroutine whenever only the subscription (not the parent Client)
// closed. The close must now key on "is an Ack executing", not "is
// everything settled": with no ack in flight, the abandoned delivery must
// NOT block the channel Close.
func TestCloseGracefulChannel_ClosesDespiteAbandonedDelivery(t *testing.T) {
	c := newDrainTestConsumer()
	// Simulate one delivery handed downstream whose disposition will never
	// fire (session send aborted / pump abandonment): unsettledN stays 1.
	c.addUnsettled()

	ch := &fakeAMQPChannel{}
	c.closeGracefulChannel(ch)

	if ch.closed.Load() != 1 {
		t.Fatalf("graceful channel Close calls = %d, want 1 (abandoned delivery must not block the close)", ch.closed.Load())
	}
	if !c.chClosed {
		t.Fatal("chClosed not set after graceful channel close")
	}
	// A late ack now skips d.Ack (channel/queue gone) instead of touching
	// the closed channel.
	ack := &countingAcknowledger{}
	c.ackFunc(testDelivery(ack))()
	if ack.acks.Load() != 0 {
		t.Fatalf("late ack after chClosed: acks = %d, want 0 (must skip d.Ack)", ack.acks.Load())
	}
}

// TestCloseGracefulChannel_WaitsForInFlightAck pins the other half: an Ack
// GENUINELY executing (holding ackMu, possibly wedged on a stalled write)
// must serialize the Close so the two never run concurrently on the same
// non-thread-safe *amqp.Channel. The Close is deferred until the ack
// releases ackMu.
func TestCloseGracefulChannel_WaitsForInFlightAck(t *testing.T) {
	c := newDrainTestConsumer()
	c.ackMu.Lock() // stand in for an ack mid-d.Ack

	ch := &fakeAMQPChannel{}
	c.closeGracefulChannel(ch) // TryLock fails → detached reaper waits

	// Give the reaper a moment; it must NOT close while the ack holds ackMu.
	time.Sleep(50 * time.Millisecond)
	if ch.closed.Load() != 0 {
		t.Fatalf("channel closed while an ack was in flight (concurrent Close+Ack): closed=%d", ch.closed.Load())
	}

	c.ackMu.Unlock() // ack completes → reaper may now close

	deadline := time.Now().Add(2 * time.Second)
	for ch.closed.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("channel not closed after the in-flight ack completed")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
