package feed

import (
	"context"
	"errors"
	"math/rand/v2"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/oddin-gg/gosdk/internal/factory"
	"github.com/oddin-gg/gosdk/types"
)

// gateOpener parks CreateChannel until released, so a test can act while
// Open is inside it.
type gateOpener struct {
	entered chan struct{}
	release chan struct{}
	del     chan amqp.Delivery
	ch      *recordingChannel
	calls   atomic.Int32
}

func newGateOpener() *gateOpener {
	return &gateOpener{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		del:     make(chan amqp.Delivery),
		ch:      &recordingChannel{seq: new(atomic.Int32)},
	}
}

func (o *gateOpener) CreateChannel(context.Context, []string, string, int) (<-chan amqp.Delivery, amqpChannel, error) {
	if o.calls.Add(1) == 1 {
		close(o.entered)
		<-o.release
	}
	return o.del, o.ch, nil
}

// lifecycleHooks counts every lifecycle hook invocation.
type lifecycleHooks struct {
	lost, restored, gone atomic.Int32
}

func (h *lifecycleHooks) install(c *ChannelConsumer) {
	c.SetChannelLostHook(func(types.MessageInterest, time.Time) { h.lost.Add(1) })
	c.SetChannelRestoredHook(func() { h.restored.Add(1) })
	c.SetConsumerGoneHook(func() { h.gone.Add(1) })
}

// assertTornDown reports the final state of a consumer whose Open and Close both
// returned: the channel is closed, the loop and watcher are gone, the
// outgoing channel is closed, gone fired once, and no loss was reported.
func assertTornDown(t *testing.T, c *ChannelConsumer, ch *recordingChannel, h *lifecycleHooks, out <-chan QueueEnvelope) {
	t.Helper()
	if ch.closedAt.Load() == 0 {
		t.Error("the AMQP channel was never closed")
	}
	done := make(chan struct{})
	go func() { c.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("the consumer loop or watcher is still running")
	}
	select {
	case _, ok := <-out:
		if ok {
			t.Error("outgoing channel delivered a message")
		}
	case <-time.After(2 * time.Second):
		t.Error("outgoing channel was never closed")
	}
	if got := h.gone.Load(); got != 1 {
		t.Errorf("gone hook fired %d times, want 1", got)
	}
	if h.lost.Load() != 0 || h.restored.Load() != 0 {
		t.Errorf("lost=%d restored=%d, want 0 and 0", h.lost.Load(), h.restored.Load())
	}
}

type gateOpenResult struct {
	out <-chan QueueEnvelope
	err error
}

func startOpen(c *ChannelConsumer) <-chan gateOpenResult {
	res := make(chan gateOpenResult, 1)
	go func() {
		mi := types.LiveOnlyMessageInterest
		out, err := c.Open(context.Background(), []string{"k"}, &mi)
		res <- gateOpenResult{out, err}
	}()
	return res
}

// waitClosedOrSettle returns once the consumer's shutdown finished, or
// after a short settle when it is (correctly) still waiting on Open.
func waitClosedOrSettle(c *ChannelConsumer) {
	select {
	case <-c.closed:
	case <-time.After(200 * time.Millisecond):
	}
}

// Close wins while Open is inside CreateChannel: the shutdown waits for
// Open, then closes the channel, the loop and the watcher it started.
func TestChannelConsumer_CloseDuringOpen_TearsDownWhatOpenCreated(t *testing.T) {
	o := newGateOpener()
	c := NewChannelConsumer(o, &factory.FeedMessageFactory{}, discardConsumerLogger(), "ex", "od:sport:", 0)
	h := &lifecycleHooks{}
	h.install(c)

	opened := startOpen(c)
	<-o.entered

	closeErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		closeErr <- c.Close(ctx)
	}()
	waitClosedOrSettle(c)
	close(o.release)

	res := <-opened
	if res.err != nil {
		t.Fatalf("Open: %v", res.err)
	}
	if err := <-closeErr; err != nil {
		t.Fatalf("Close: %v", err)
	}
	assertTornDown(t, c, o.ch, h, res.out)
}

// Close called while Open holds the state lock right after CreateChannel
// returned (before the loop starts): either order of the two ends in the
// same torn-down state.
func TestChannelConsumer_CloseDuringOpen_AfterCreateChannel(t *testing.T) {
	o := newGateOpener()
	c := NewChannelConsumer(o, &factory.FeedMessageFactory{}, discardConsumerLogger(), "ex", "od:sport:", 0)
	h := &lifecycleHooks{}
	h.install(c)

	opened := startOpen(c)
	<-o.entered
	c.mu.Lock()
	close(o.release)
	closeErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		closeErr <- c.Close(ctx)
	}()
	time.Sleep(20 * time.Millisecond)
	c.mu.Unlock()

	res := <-opened
	if res.err != nil {
		t.Fatalf("Open: %v", res.err)
	}
	if err := <-closeErr; err != nil {
		t.Fatalf("Close: %v", err)
	}
	assertTornDown(t, c, o.ch, h, res.out)
}

// Close after Open returned (the loop is running) is the normal path.
func TestChannelConsumer_CloseAfterOpen_TearsDown(t *testing.T) {
	o := newGateOpener()
	close(o.release)
	c := NewChannelConsumer(o, &factory.FeedMessageFactory{}, discardConsumerLogger(), "ex", "od:sport:", 0)
	h := &lifecycleHooks{}
	h.install(c)

	res := <-startOpen(c)
	if res.err != nil {
		t.Fatalf("Open: %v", res.err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	assertTornDown(t, c, o.ch, h, res.out)
}

// Open after a finished Close creates no channel, starts nothing and
// reports nothing, whatever the number of Closes before it.
func TestChannelConsumer_OpenAfterClose_CreatesNothing(t *testing.T) {
	o := newGateOpener()
	close(o.release)
	c := NewChannelConsumer(o, &factory.FeedMessageFactory{}, discardConsumerLogger(), "ex", "od:sport:", 0)
	h := &lifecycleHooks{}
	h.install(c)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for range 2 {
		if err := c.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
	res := <-startOpen(c)
	if !errors.Is(res.err, errConsumerClosed) || res.out != nil {
		t.Fatalf("Open = (%v, %v), want (nil, errConsumerClosed)", res.out, res.err)
	}
	if got := o.calls.Load(); got != 0 {
		t.Errorf("CreateChannel called %d times, want 0", got)
	}
	c.wg.Wait()
	if h.gone.Load() != 0 || h.lost.Load() != 0 || h.restored.Load() != 0 {
		t.Errorf("hooks fired: gone=%d lost=%d restored=%d", h.gone.Load(), h.lost.Load(), h.restored.Load())
	}
}

// Close before Open and a second Close return nil and report nothing.
func TestChannelConsumer_CloseBeforeOpenAndTwice(t *testing.T) {
	o := newGateOpener()
	c := NewChannelConsumer(o, &factory.FeedMessageFactory{}, discardConsumerLogger(), "ex", "od:sport:", 0)
	h := &lifecycleHooks{}
	h.install(c)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for range 2 {
		if err := c.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
	if h.gone.Load() != 0 || h.lost.Load() != 0 || h.restored.Load() != 0 {
		t.Errorf("hooks fired for a consumer that never opened: gone=%d lost=%d restored=%d", h.gone.Load(), h.lost.Load(), h.restored.Load())
	}
}

// One Open against Closes started at random points before, during and
// after it: either Open fails with errConsumerClosed having created
// nothing, or it succeeds and is torn down; gone fires once in the second
// case and never in the first.
func TestChannelConsumer_OpenCloseRace(t *testing.T) {
	for i := range 200 {
		o := newGateOpener()
		close(o.release)
		c := NewChannelConsumer(o, &factory.FeedMessageFactory{}, discardConsumerLogger(), "ex", "od:sport:", 0)
		h := &lifecycleHooks{}
		h.install(c)

		mi := types.LiveOnlyMessageInterest
		start := make(chan struct{})
		opened := make(chan gateOpenResult, 1)
		closers := 1 + rand.IntN(3)
		closeDone := make(chan struct{}, closers)
		go func() {
			<-start
			for range rand.IntN(50) {
				runtime.Gosched()
			}
			out, err := c.Open(context.Background(), []string{"k"}, &mi)
			opened <- gateOpenResult{out, err}
		}()
		for range closers {
			delay := rand.IntN(50)
			go func() {
				<-start
				for range delay {
					runtime.Gosched()
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = c.Close(ctx)
				closeDone <- struct{}{}
			}()
		}
		close(start)
		res := <-opened
		for range closers {
			<-closeDone
		}

		if res.err != nil {
			if !errors.Is(res.err, errConsumerClosed) {
				t.Fatalf("iteration %d: Open: %v", i, res.err)
			}
			if o.calls.Load() != 0 || h.gone.Load() != 0 {
				t.Fatalf("iteration %d: refused Open created %d channels, gone=%d", i, o.calls.Load(), h.gone.Load())
			}
			c.wg.Wait()
			continue
		}
		assertTornDown(t, c, o.ch, h, res.out)
	}
}
