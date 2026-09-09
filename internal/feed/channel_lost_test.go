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
// connection going away), the hook runs exactly once and BEFORE the
// consumer asks for a new channel — the recovery layer must learn about
// the lost queue before any delivery on the replacement can be processed.
func TestChannelConsumer_ChannelLostHookFiresBeforeReopen(t *testing.T) {
	opener := &sequencedOpener{chans: []chan amqp.Delivery{make(chan amqp.Delivery), make(chan amqp.Delivery)}}
	c := NewChannelConsumer(opener, &factory.FeedMessageFactory{}, discardConsumerLogger(), "ex", "od:sport:", 0)

	var hookCalls atomic.Int32
	var openerCallsAtHook atomic.Int32
	c.SetChannelLostHook(func() {
		hookCalls.Add(1)
		openerCallsAtHook.Store(opener.calls.Load())
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mi := types.AllMessageInterest
	if _, err := c.Open(ctx, []string{"k"}, &mi); err != nil {
		t.Fatalf("Open: %v", err)
	}

	close(opener.chans[0]) // the broker took the channel (and the queue) away

	deadline := time.Now().Add(2 * time.Second)
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

	// A drain/close is not a loss: no further hook call.
	cancel()
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), time.Second)
	defer cancelShutdown()
	_ = c.Close(shutdownCtx)
	if got := hookCalls.Load(); got != 1 {
		t.Fatalf("hook calls after close = %d, want still 1", got)
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
