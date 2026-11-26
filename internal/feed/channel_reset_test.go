package feed

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/oddin-gg/gosdk/internal/factory"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

// resetChannel is an amqpChannel that behaves like a real one on Close: the
// delivery stream and the notification channels close with it. A broker-side
// close is fireBrokerClose.
type resetChannel struct {
	mu         sync.Mutex
	closed     bool
	deliveries chan amqp.Delivery
	closeCh    chan *amqp.Error
	cancelCh   chan string
	// closes counts the calls of Close, effective the ones that closed it.
	closes    atomic.Int32
	effective atomic.Int32
	// hang, when non-nil, holds Close until it is closed.
	hang chan struct{}
}

func newResetChannel() *resetChannel {
	return &resetChannel{deliveries: make(chan amqp.Delivery)}
}

func (r *resetChannel) shut() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.effective.Add(1)
	r.closed = true
	close(r.deliveries)
	if r.closeCh != nil {
		close(r.closeCh)
	}
	if r.cancelCh != nil {
		close(r.cancelCh)
	}
}

func (r *resetChannel) Close() error {
	if r.hang != nil {
		<-r.hang
	}
	r.closes.Add(1)
	r.shut()
	return nil
}

func (r *resetChannel) NotifyClose(c chan *amqp.Error) chan *amqp.Error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeCh = c
	if r.closed {
		close(c)
	}
	return c
}

func (r *resetChannel) NotifyCancel(c chan string) chan string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cancelCh = c
	if r.closed {
		close(c)
	}
	return c
}

// fireBrokerClose is the broker closing the channel: the watcher is told, and
// the delivery stream ends.
func (r *resetChannel) fireBrokerClose() {
	r.mu.Lock()
	if !r.closed && r.closeCh != nil {
		r.closeCh <- &amqp.Error{Code: 320, Reason: "CONNECTION_FORCED"}
	}
	r.mu.Unlock()
	r.shut()
}

// resetOpener hands out a fresh resetChannel per CreateChannel. After the
// first call, the next `fail` calls fail, and calls wait for `gate` when it is
// set.
type resetOpener struct {
	// seq orders the opens against the reports; openedAt is where each
	// successful open fell.
	seq      atomic.Int32
	openedAt atomic.Int32
	mu       sync.Mutex
	channels []*resetChannel
	calls    atomic.Int32
	fail     atomic.Int32
	gate     chan struct{}
	entered  chan struct{}
	// afterOpen, when set, runs on the consumer's goroutine for every
	// successful open after the first, with the channel about to be returned.
	afterOpen func(call int32, rc *resetChannel)
}

func newResetOpener() *resetOpener {
	return &resetOpener{entered: make(chan struct{}, 32)}
}

func (o *resetOpener) CreateChannel(ctx context.Context, _ []string, _ string, _ int) (<-chan amqp.Delivery, amqpChannel, error) {
	n := o.calls.Add(1)
	if n > 1 {
		o.entered <- struct{}{}
		if o.gate != nil {
			select {
			case <-o.gate:
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
		}
		if o.fail.Add(-1) >= 0 {
			return nil, nil, errors.New("broker unavailable")
		}
	}
	rc := newResetChannel()
	o.openedAt.Store(o.seq.Add(1))
	o.mu.Lock()
	o.channels = append(o.channels, rc)
	o.mu.Unlock()
	if n > 1 && o.afterOpen != nil {
		o.afterOpen(n, rc)
	}
	return rc.deliveries, rc, nil
}

func (o *resetOpener) channel(i int) *resetChannel {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.channels[i]
}

func (o *resetOpener) created() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.channels)
}

// waitEntered waits for a reopen attempt to start.
func (o *resetOpener) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-o.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("no reopen attempt started")
	}
}

type resetDone struct {
	number   uint64
	replaced bool
}

// resetProbe records the hooks of a consumer.
type resetProbe struct {
	lost, restored, gone atomic.Int32
	done                 chan resetDone
	// seq is shared with the opener, doneAt is where the last report fell.
	seq    *atomic.Int32
	doneAt atomic.Int32
}

func newResetProbe(seq *atomic.Int32) *resetProbe {
	return &resetProbe{done: make(chan resetDone, 16), seq: seq}
}

func (p *resetProbe) install(c *ChannelConsumer) {
	c.SetChannelLostHook(func(types.MessageInterest, time.Time) { p.lost.Add(1) })
	c.SetChannelRestoredHook(func() { p.restored.Add(1) })
	c.SetConsumerGoneHook(func() { p.gone.Add(1) })
	c.SetChannelResetDoneHook(func(number uint64, replaced bool) {
		p.doneAt.Store(p.seq.Add(1))
		p.done <- resetDone{number, replaced}
	})
}

func (p *resetProbe) waitDone(t *testing.T) resetDone {
	t.Helper()
	select {
	case d := <-p.done:
		return d
	case <-time.After(3 * time.Second):
		t.Fatal("the reset was not reported done")
		return resetDone{}
	}
}

// noMoreDone fails when another done-report arrives within a short while.
func (p *resetProbe) noMoreDone(t *testing.T) {
	t.Helper()
	select {
	case d := <-p.done:
		t.Fatalf("unexpected extra reset-done report: %+v", d)
	case <-time.After(60 * time.Millisecond):
	}
}

func newResetConsumer(t *testing.T, o *resetOpener, logger *log.Logger) (*ChannelConsumer, *resetProbe) {
	t.Helper()
	// a long dwell backoff, so a reset that did not skip it is caught
	oldDwell, oldBackoff, oldRetry := minChannelDwell, channelReopenBackoff, channelReopenRetry
	minChannelDwell, channelReopenBackoff, channelReopenRetry = 10*time.Second, 5*time.Second, 20*time.Millisecond
	t.Cleanup(func() { minChannelDwell, channelReopenBackoff, channelReopenRetry = oldDwell, oldBackoff, oldRetry })
	if logger == nil {
		logger = discardConsumerLogger()
	}
	c := NewChannelConsumer(o, &factory.FeedMessageFactory{}, logger, "ex", "od:sport:", 0)
	p := newResetProbe(&o.seq)
	p.install(c)
	closeConsumer(t, c)
	return c, p
}

func openResetConsumer(t *testing.T, c *ChannelConsumer) <-chan QueueEnvelope {
	t.Helper()
	mi := types.AllMessageInterest
	out, err := c.Open(context.Background(), []string{"k"}, &mi)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return out
}

func waitWaitGroup(t *testing.T, c *ChannelConsumer) {
	t.Helper()
	done := make(chan struct{})
	go func() { c.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the consumer's goroutines are still running")
	}
}

func closeNow(t *testing.T, c *ChannelConsumer) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// sendDelivery hands a delivery to the consumer on the channel and returns the
// envelope that comes out.
func sendDelivery(t *testing.T, ch *resetChannel, out <-chan QueueEnvelope, ack amqp.Acknowledger) QueueEnvelope {
	t.Helper()
	select {
	case ch.deliveries <- testDelivery(ack):
	case <-time.After(3 * time.Second):
		t.Fatal("the consumer takes no delivery")
	}
	select {
	case env := <-out:
		return env
	case <-time.After(3 * time.Second):
		t.Fatal("no envelope came out")
		return QueueEnvelope{}
	}
}

func TestChannelReset_ReplacesTheChannelWithoutALoss(t *testing.T) {
	o := newResetOpener()
	c, p := newResetConsumer(t, o, nil)
	out := openResetConsumer(t, c)
	old := o.channel(0)

	start := time.Now()
	c.Reset(7)
	if got := p.waitDone(t); got != (resetDone{7, true}) {
		t.Fatalf("done = %+v, want number 7 replaced", got)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the reset took %v: the dwell backoff must be skipped", elapsed)
	}
	if got := o.calls.Load(); got != 2 {
		t.Fatalf("CreateChannel calls = %d, want 2", got)
	}
	if old.effective.Load() != 1 {
		t.Fatalf("old channel closed %d times, want 1", old.effective.Load())
	}
	p.noMoreDone(t)
	if p.lost.Load() != 0 {
		t.Fatalf("a reset was reported as a loss %d times", p.lost.Load())
	}
	if p.restored.Load() != 0 {
		t.Fatalf("restored fired %d times for a reset that lost nothing", p.restored.Load())
	}
	// deliveries of the new channel flow
	env := sendDelivery(t, o.channel(1), out, &countingAcknowledger{})
	if env.Msg == nil {
		t.Fatal("no message from the new channel")
	}
	env.Ack()
}

func TestChannelReset_DoneComesAfterTheNewQueueIsBound(t *testing.T) {
	o := newResetOpener()
	o.fail.Store(3)
	c, p := newResetConsumer(t, o, nil)
	openResetConsumer(t, c)

	c.Reset(1)
	if got := p.waitDone(t); got != (resetDone{1, true}) {
		t.Fatalf("done = %+v", got)
	}
	// the initial open, three failed attempts, and the one that succeeded
	if got := o.calls.Load(); got != 5 {
		t.Fatalf("CreateChannel calls = %d, want 5", got)
	}
	if done, bound := p.doneAt.Load(), o.openedAt.Load(); done < bound {
		t.Fatalf("done reported at %d, before the queue was bound at %d", done, bound)
	}
	if o.created() != 2 {
		t.Fatalf("channels created = %d, want 2", o.created())
	}
	if p.lost.Load() != 0 {
		t.Fatal("a failed reopen during a reset was reported as a loss")
	}
}

func TestChannelReset_RacingABrokerClose_IsOneReset(t *testing.T) {
	o := newResetOpener()
	c, p := newResetConsumer(t, o, nil)
	openResetConsumer(t, c)
	old := o.channel(0)
	old.hang = make(chan struct{}) // the close the reset makes is slow

	resetReturned := make(chan struct{})
	go func() { c.Reset(3); close(resetReturned) }()
	// the reset is marked and parked in the channel's close
	deadline := time.Now().Add(2 * time.Second)
	for !c.isResetting() {
		if time.Now().After(deadline) {
			t.Fatal("the reset was never marked")
		}
		time.Sleep(time.Millisecond)
	}
	old.fireBrokerClose() // the broker takes the channel in the same moment
	close(old.hang)
	<-resetReturned

	if got := p.waitDone(t); got != (resetDone{3, true}) {
		t.Fatalf("done = %+v", got)
	}
	p.noMoreDone(t)
	if p.lost.Load() != 0 {
		t.Fatalf("a real loss during a reset was reported %d times", p.lost.Load())
	}
	if got := o.calls.Load(); got != 2 {
		t.Fatalf("CreateChannel calls = %d, want 2", got)
	}
}

func TestChannelReset_NotReplacedWhenThereIsNoChannelToClose(t *testing.T) {
	t.Run("before Open", func(t *testing.T) {
		o := newResetOpener()
		c, p := newResetConsumer(t, o, nil)
		c.Reset(1)
		if got := p.waitDone(t); got != (resetDone{1, false}) {
			t.Fatalf("done = %+v", got)
		}
		if o.calls.Load() != 0 {
			t.Fatal("a channel was created for a reset before Open")
		}
		if c.isResetting() {
			t.Fatal("a reset that closed nothing left the consumer marked")
		}
		waitWaitGroup(t, c)
	})
	t.Run("after Close", func(t *testing.T) {
		o := newResetOpener()
		c, p := newResetConsumer(t, o, nil)
		openResetConsumer(t, c)
		closeNow(t, c)
		c.Reset(2)
		if got := p.waitDone(t); got != (resetDone{2, false}) {
			t.Fatalf("done = %+v", got)
		}
		if o.calls.Load() != 1 || o.channel(0).closes.Load() != 1 {
			t.Fatalf("calls=%d closes=%d: nothing may be reopened or closed again", o.calls.Load(), o.channel(0).closes.Load())
		}
		waitWaitGroup(t, c)
		if p.gone.Load() != 1 {
			t.Fatalf("gone fired %d times", p.gone.Load())
		}
	})
	t.Run("while Close runs", func(t *testing.T) {
		o := newResetOpener()
		c, p := newResetConsumer(t, o, nil)
		openResetConsumer(t, c)
		old := o.channel(0)
		old.hang = make(chan struct{}) // the loop is held in the channel's close
		closed := make(chan struct{})
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = c.Close(ctx)
			close(closed)
		}()
		deadline := time.Now().Add(2 * time.Second)
		for {
			c.mu.Lock()
			shutdown := c.shutdown
			c.mu.Unlock()
			if shutdown {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the shutdown did not begin")
			}
			time.Sleep(time.Millisecond)
		}
		c.Reset(6)
		if got := p.waitDone(t); got != (resetDone{6, false}) {
			t.Fatalf("done = %+v", got)
		}
		if c.isResetting() {
			t.Fatal("a reset during Close left the consumer marked")
		}
		close(old.hang)
		<-closed
		if o.calls.Load() != 1 || old.closes.Load() != 1 {
			t.Fatalf("calls=%d closes=%d: a reset during Close must reopen and close nothing", o.calls.Load(), old.closes.Load())
		}
		waitWaitGroup(t, c)
		p.noMoreDone(t)
	})
	t.Run("during a graceful drain", func(t *testing.T) {
		o := newResetOpener()
		c, p := newResetConsumer(t, o, nil)
		out := openResetConsumer(t, c)
		// a delivery in the pipeline that is not settled keeps the drain open
		env := sendDelivery(t, o.channel(0), out, &countingAcknowledger{})
		drained := make(chan bool, 1)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			drained <- c.CloseGraceful(ctx)
		}()
		deadline := time.Now().Add(2 * time.Second)
		for !c.draining() {
			if time.Now().After(deadline) {
				t.Fatal("drain did not begin")
			}
			time.Sleep(time.Millisecond)
		}
		c.Reset(4)
		if got := p.waitDone(t); got != (resetDone{4, false}) {
			t.Fatalf("done = %+v", got)
		}
		if o.channel(0).closes.Load() != 0 {
			t.Fatal("the reset closed the channel of a draining consumer")
		}
		env.Ack() // settles: the drain completes
		if !<-drained {
			t.Fatal("the drain did not complete")
		}
		if o.calls.Load() != 1 {
			t.Fatalf("CreateChannel calls = %d, want 1", o.calls.Load())
		}
		waitWaitGroup(t, c)
		p.noMoreDone(t)
	})
}

func TestChannelReset_LateAcksOfTheClosedChannelAreHarmless(t *testing.T) {
	var buf bytes.Buffer
	var bufMu sync.Mutex
	logger := log.New(slog.New(slog.NewTextHandler(lockedWriter{&bufMu, &buf}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	o := newResetOpener()
	c, p := newResetConsumer(t, o, logger)
	out := openResetConsumer(t, c)

	env := sendDelivery(t, o.channel(0), out, closedChannelAcknowledger{})
	c.Reset(1)
	p.waitDone(t)
	env.Ack() // the channel the delivery came from is gone

	bufMu.Lock()
	text := buf.String()
	bufMu.Unlock()
	if !strings.Contains(text, "ack after a channel reset") {
		t.Fatalf("the late ack left no debug line:\n%s", text)
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "feed: ack") && (strings.Contains(line, "level=WARN") || strings.Contains(line, "level=ERROR")) {
			t.Fatalf("a late ack of the closed channel surfaced above debug: %s", line)
		}
	}
	// the consumer settles the delivery all the same
	if c.settledSignal() == nil {
		t.Fatal("no settle signal")
	}
	select {
	case <-c.settledSignal():
	case <-time.After(time.Second):
		t.Fatal("the late ack did not settle the delivery")
	}
}

// closedChannelAcknowledger fails every ack as a closed channel does.
type closedChannelAcknowledger struct{}

func (closedChannelAcknowledger) Ack(uint64, bool) error        { return amqp.ErrClosed }
func (closedChannelAcknowledger) Nack(uint64, bool, bool) error { return amqp.ErrClosed }
func (closedChannelAcknowledger) Reject(uint64, bool) error     { return amqp.ErrClosed }

// lockedWriter serializes writes to a shared buffer.
type lockedWriter struct {
	mu *sync.Mutex
	b  *bytes.Buffer
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func TestChannelReset_CloseDuringTheReopenIsAClose(t *testing.T) {
	for _, mode := range []string{"parked in CreateChannel", "failing and retrying"} {
		t.Run(mode, func(t *testing.T) {
			o := newResetOpener()
			if mode == "failing and retrying" {
				o.fail.Store(1 << 20)
			} else {
				o.gate = make(chan struct{}) // never opened
			}
			c, p := newResetConsumer(t, o, nil)
			out := openResetConsumer(t, c)

			c.Reset(9)
			o.waitEntered(t)

			start := time.Now()
			closeNow(t, c)
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Fatalf("Close took %v during a reset", elapsed)
			}
			waitWaitGroup(t, c)
			if p.gone.Load() != 1 {
				t.Fatalf("gone fired %d times, want 1", p.gone.Load())
			}
			p.noMoreDone(t)
			if p.lost.Load() != 0 {
				t.Fatalf("lost fired %d times", p.lost.Load())
			}
			if o.created() != 1 {
				t.Fatalf("channels created = %d: none may survive a Close", o.created())
			}
			select {
			case _, ok := <-out:
				if ok {
					t.Fatal("unexpected envelope")
				}
			case <-time.After(time.Second):
				t.Fatal("the outgoing channel was not closed")
			}
		})
	}
}

func TestChannelReset_ALossAfterAFinishedResetIsReported(t *testing.T) {
	o := newResetOpener()
	c, p := newResetConsumer(t, o, nil)
	// newResetConsumer set a long backoff: the second reopen needs a short one
	channelReopenBackoff = 20 * time.Millisecond
	openResetConsumer(t, c)

	c.Reset(1)
	p.waitDone(t)
	if c.isResetting() {
		t.Fatal("the mark outlived the reset")
	}
	o.channel(1).fireBrokerClose()

	deadline := time.Now().Add(3 * time.Second)
	for p.lost.Load() == 0 || p.restored.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("lost=%d restored=%d: a real loss after a reset must be reported", p.lost.Load(), p.restored.Load())
		}
		time.Sleep(time.Millisecond)
	}
	if p.lost.Load() != 1 || p.restored.Load() != 1 {
		t.Fatalf("lost=%d restored=%d, want 1 and 1", p.lost.Load(), p.restored.Load())
	}
	p.noMoreDone(t)
}

func TestChannelReset_ASecondResetJoinsTheOneUnderWay(t *testing.T) {
	o := newResetOpener()
	o.gate = make(chan struct{})
	c, p := newResetConsumer(t, o, nil)
	openResetConsumer(t, c)

	c.Reset(1)
	o.waitEntered(t) // the reopen is parked
	c.Reset(2)
	close(o.gate)

	first, second := p.waitDone(t), p.waitDone(t)
	if first != (resetDone{1, true}) || second != (resetDone{2, true}) {
		t.Fatalf("done = %+v then %+v, want 1 and 2 replaced", first, second)
	}
	p.noMoreDone(t)
	if o.created() != 2 || o.channel(0).effective.Load() != 1 {
		t.Fatalf("channels=%d closes of the first=%d: a joined reset replaces the channel once", o.created(), o.channel(0).effective.Load())
	}
	if p.lost.Load() != 0 {
		t.Fatal("a reset was reported as a loss")
	}
}

func TestChannelReset_AResetAfterARealLossRestoresAndReportsDone(t *testing.T) {
	o := newResetOpener()
	o.gate = make(chan struct{})
	c, p := newResetConsumer(t, o, nil)
	channelReopenBackoff = 20 * time.Millisecond
	openResetConsumer(t, c)

	o.channel(0).fireBrokerClose() // a real loss; the reopen parks at the gate
	o.waitEntered(t)
	if p.lost.Load() != 1 {
		t.Fatalf("lost = %d, want 1", p.lost.Load())
	}
	c.Reset(5)
	close(o.gate)
	if got := p.waitDone(t); got != (resetDone{5, true}) {
		t.Fatalf("done = %+v", got)
	}
	if p.restored.Load() != 1 {
		t.Fatalf("restored = %d: the loss that was reported must be restored", p.restored.Load())
	}
	if p.lost.Load() != 1 {
		t.Fatalf("lost = %d, want 1", p.lost.Load())
	}
}

// waitFor polls until cond holds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// A Close that has begun, but has not cancelled the loop yet, when the
// replacement arrives: the reset is not a success.
func TestChannelReset_ReplacementLandingDuringShutdownIsNotASuccess(t *testing.T) {
	o := newResetOpener()
	c, p := newResetConsumer(t, o, nil)
	o.afterOpen = func(int32, *resetChannel) {
		c.mu.Lock()
		c.shutdown = true // runShutdown's first step; the loop is not cancelled yet
		c.mu.Unlock()
	}
	openResetConsumer(t, c)

	c.Reset(1)
	waitFor(t, "the consumer to exit", func() bool { return p.gone.Load() == 1 })
	waitFor(t, "the late channel to be closed", func() bool { return o.created() == 2 && o.channel(1).effective.Load() == 1 })
	p.noMoreDone(t)
	if p.lost.Load() != 0 || p.restored.Load() != 0 {
		t.Fatalf("lost=%d restored=%d, want none", p.lost.Load(), p.restored.Load())
	}
}

// A Close that cancels the loop after the reopen succeeded: no done-report,
// gone once, and the new channel is closed, not left open.
func TestChannelReset_CloseOvertakingASuccessfulReopenReportsNoDone(t *testing.T) {
	o := newResetOpener()
	c, p := newResetConsumer(t, o, nil)
	o.afterOpen = func(int32, *resetChannel) {
		c.mu.Lock()
		cancel := c.closeFn
		c.mu.Unlock()
		cancel()
	}
	openResetConsumer(t, c)

	c.Reset(1)
	waitFor(t, "the consumer to exit", func() bool { return p.gone.Load() == 1 })
	waitFor(t, "the new channel to be closed", func() bool { return o.created() == 2 && o.channel(1).effective.Load() == 1 })
	closeNow(t, c)
	waitWaitGroup(t, c)
	p.noMoreDone(t)
	if p.gone.Load() != 1 {
		t.Fatalf("gone fired %d times, want 1", p.gone.Load())
	}
}
