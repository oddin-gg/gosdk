package feed

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/oddin-gg/gosdk/internal/factory"
	"github.com/oddin-gg/gosdk/types"
)

// sequencedOpener hands out one deliveries channel per CreateChannel
// call, so a test can close the first and watch the consumer reopen.
type sequencedOpener struct {
	chans []chan amqp.Delivery
	calls atomic.Int32
}

func (o *sequencedOpener) CreateChannel(context.Context, []string, string, int) (<-chan amqp.Delivery, *amqp.Channel, error) {
	n := int(o.calls.Add(1))
	if n > len(o.chans) {
		return nil, nil, context.Canceled
	}
	return o.chans[n-1], nil, nil
}

// TestChannelConsumer_ChannelLostHookFiresBeforeReopen: when the
// deliveries channel closes (broker cancel, channel exception, or the
// connection going away), the hook runs exactly once, carries the
// consumer's interest, and runs BEFORE the consumer asks for a new
// channel — the recovery layer must learn about the lost queue before any
// delivery on the replacement can be processed. A channel that died
// within its dwell is re-declared only after the reopen backoff.
func TestChannelConsumer_ChannelLostHookFiresBeforeReopen(t *testing.T) {
	opener := &sequencedOpener{chans: []chan amqp.Delivery{make(chan amqp.Delivery), make(chan amqp.Delivery)}}
	c := NewChannelConsumer(opener, &factory.FeedMessageFactory{}, discardConsumerLogger(), "ex", "od:sport:", 0)

	var hookCalls atomic.Int32
	var openerCallsAtHook atomic.Int32
	var hookInterest atomic.Value
	c.SetChannelLostHook(func(mi types.MessageInterest) {
		hookCalls.Add(1)
		openerCallsAtHook.Store(opener.calls.Load())
		hookInterest.Store(mi)
	})
	if !c.ChannelLostHookInstalled() {
		t.Fatal("ChannelLostHookInstalled must report the installed hook")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mi := types.LiveOnlyMessageInterest
	if _, err := c.Open(ctx, []string{"k"}, &mi); err != nil {
		t.Fatalf("Open: %v", err)
	}

	lost := time.Now()
	close(opener.chans[0]) // the broker took the channel (and the queue) away

	deadline := time.Now().Add(3 * time.Second)
	for opener.calls.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("consumer did not reopen: CreateChannel calls = %d", opener.calls.Load())
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := hookCalls.Load(); got != 1 {
		t.Fatalf("hook calls = %d, want exactly 1", got)
	}
	if got := openerCallsAtHook.Load(); got != 1 {
		t.Fatalf("hook ran after CreateChannel #%d; it must run before the reopen (#2)", got)
	}
	if got, _ := hookInterest.Load().(types.MessageInterest); got != types.LiveOnlyMessageInterest {
		t.Fatalf("hook interest = %v, want the consumer's LiveOnly", got)
	}
	// The channel lived well under minChannelDwell, so the reopen must
	// have waited channelReopenBackoff instead of spinning.
	if elapsed := time.Since(lost); elapsed < channelReopenBackoff-100*time.Millisecond {
		t.Fatalf("reopen after %v; want ≥ %v backoff for a channel that died within its dwell", elapsed, channelReopenBackoff)
	}

	// An abrupt close is not a loss: no further hook call. (Open builds
	// the loop ctx with WithoutCancel, so it is Close, not cancel(), that
	// stops the loop — via the ctx.Err() exit in run.)
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), time.Second)
	defer cancelShutdown()
	_ = c.Close(shutdownCtx)
	if got := hookCalls.Load(); got != 1 {
		t.Fatalf("hook calls after abrupt close = %d, want still 1", got)
	}
}

// TestChannelConsumer_GracefulCloseIsNotALoss pins the drain exit of run:
// a graceful close (what Subscription.Close takes) must not report a
// channel loss — it would flag every producer down and force a snapshot
// recovery on a normal shutdown.
func TestChannelConsumer_GracefulCloseIsNotALoss(t *testing.T) {
	opener := &sequencedOpener{chans: []chan amqp.Delivery{make(chan amqp.Delivery), make(chan amqp.Delivery)}}
	c := NewChannelConsumer(opener, &factory.FeedMessageFactory{}, discardConsumerLogger(), "ex", "od:sport:", 0)
	var hookCalls atomic.Int32
	c.SetChannelLostHook(func(types.MessageInterest) { hookCalls.Add(1) })

	mi := types.AllMessageInterest
	if _, err := c.Open(context.Background(), []string{"k"}, &mi); err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c.CloseGraceful(ctx)

	if got := hookCalls.Load(); got != 0 {
		t.Fatalf("graceful close reported %d channel loss(es), want 0", got)
	}
	if got := opener.calls.Load(); got != 1 {
		t.Fatalf("graceful close reopened the channel: CreateChannel calls = %d, want 1", got)
	}
}

// TestChannelConsumer_NoHookNoPanic: consumers without a hook (tests,
// replay sessions) reopen as before.
func TestChannelConsumer_NoHookNoPanic(t *testing.T) {
	opener := &sequencedOpener{chans: []chan amqp.Delivery{make(chan amqp.Delivery), make(chan amqp.Delivery)}}
	c := NewChannelConsumer(opener, &factory.FeedMessageFactory{}, discardConsumerLogger(), "ex", "od:sport:", 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mi := types.AllMessageInterest
	if _, err := c.Open(ctx, []string{"k"}, &mi); err != nil {
		t.Fatalf("Open: %v", err)
	}
	close(opener.chans[0])
	deadline := time.Now().Add(2 * time.Second)
	for opener.calls.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("consumer did not reopen: calls = %d", opener.calls.Load())
		}
		time.Sleep(2 * time.Millisecond)
	}
}
