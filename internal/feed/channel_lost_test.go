package feed

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/oddin-gg/gosdk/internal/factory"
	"github.com/oddin-gg/gosdk/types"
)

// recordingChannel is an amqpChannel double: Close records its ordinal
// against a shared counter (so a test can prove what ran before it), and
// the Notify* channels are handed back so a test can fire a loss the way
// the broker would.
type recordingChannel struct {
	seq       *atomic.Int32
	closedAt  atomic.Int32 // ordinal of Close (0 = not closed)
	watchedAt atomic.Int32 // ordinal of NotifyClose registration (0 = not yet)
	notifyMu  sync.Mutex
	closeCh   chan *amqp.Error
	cancelCh  chan string
	closeHang chan struct{} // when non-nil, Close blocks until it is closed
}

func (r *recordingChannel) Close() error {
	if r.closeHang != nil {
		<-r.closeHang
	}
	r.closedAt.Store(r.seq.Add(1))
	return nil
}

func (r *recordingChannel) NotifyClose(c chan *amqp.Error) chan *amqp.Error {
	r.notifyMu.Lock()
	defer r.notifyMu.Unlock()
	r.closeCh = c
	r.watchedAt.Store(r.seq.Add(1))
	return c
}

func (r *recordingChannel) NotifyCancel(c chan string) chan string {
	r.notifyMu.Lock()
	defer r.notifyMu.Unlock()
	r.cancelCh = c
	return c
}

// fireClose delivers a broker-side channel close to the watcher.
func (r *recordingChannel) fireClose(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		r.notifyMu.Lock()
		ch := r.closeCh
		r.notifyMu.Unlock()
		if ch != nil {
			ch <- &amqp.Error{Code: 320, Reason: "CONNECTION_FORCED"}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("watcher never registered NotifyClose")
		}
		time.Sleep(time.Millisecond)
	}
}

// sequencedOpener hands out one (deliveries, channel) pair per
// CreateChannel call so a test can lose the first and watch the reopen.
type sequencedOpener struct {
	chans    []chan amqp.Delivery
	channels []*recordingChannel
	seq      atomic.Int32
	calls    atomic.Int32
	openedAt []int32 // ordinal at which each CreateChannel ran
	mu       sync.Mutex
}

func newSequencedOpener(n int, hangClose bool) *sequencedOpener {
	o := &sequencedOpener{}
	for i := 0; i < n; i++ {
		o.chans = append(o.chans, make(chan amqp.Delivery))
		rc := &recordingChannel{seq: &o.seq}
		if hangClose && i == 0 {
			rc.closeHang = make(chan struct{})
		}
		o.channels = append(o.channels, rc)
	}
	return o
}

func (o *sequencedOpener) CreateChannel(context.Context, []string, string, int) (<-chan amqp.Delivery, amqpChannel, error) {
	n := int(o.calls.Add(1))
	o.mu.Lock()
	o.openedAt = append(o.openedAt, o.seq.Add(1))
	o.mu.Unlock()
	if n > len(o.chans) {
		return nil, nil, context.Canceled
	}
	return o.chans[n-1], o.channels[n-1], nil
}

func (o *sequencedOpener) waitCalls(t *testing.T, want int32) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for o.calls.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("CreateChannel calls = %d, want %d", o.calls.Load(), want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// lossRecorder captures every hook invocation with its ordinal in the
// shared sequence, so ordering against Close and CreateChannel is exact.
type lossRecorder struct {
	seq   *atomic.Int32
	mu    sync.Mutex
	calls []lossCall
}

type lossCall struct {
	interest types.MessageInterest
	lostAt   time.Time
	ordinal  int32
}

func (l *lossRecorder) hook(mi types.MessageInterest, lostAt time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, lossCall{mi, lostAt, l.seq.Add(1)})
}

func (l *lossRecorder) snapshot() []lossCall {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]lossCall(nil), l.calls...)
}

func shortDwell(t *testing.T) {
	t.Helper()
	oldDwell, oldBackoff := minChannelDwell, channelReopenBackoff
	minChannelDwell, channelReopenBackoff = 10*time.Second, 150*time.Millisecond
	t.Cleanup(func() { minChannelDwell, channelReopenBackoff = oldDwell, oldBackoff })
}

func closeConsumer(t *testing.T, c *ChannelConsumer) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = c.Close(ctx)
	})
}

// TestChannelConsumer_ChannelLost_HookBeforeCloseAndReopen pins the
// ordering the design rests on: when the deliveries channel closes, the
// hook runs exactly once for that channel, carrying the consumer's
// interest and a loss instant, BEFORE the old channel's Close (a real
// blocking RPC on a broker cancel) and BEFORE the replacement
// CreateChannel. A channel that died within its dwell is re-declared
// only after the reopen backoff.
func TestChannelConsumer_ChannelLost_HookBeforeCloseAndReopen(t *testing.T) {
	shortDwell(t)
	opener := newSequencedOpener(2, true)
	rec := &lossRecorder{seq: &opener.seq}
	c := NewChannelConsumer(opener, &factory.FeedMessageFactory{}, discardConsumerLogger(), "ex", "od:sport:", 0)
	c.SetChannelLostHook(rec.hook)
	var restoredAt, goneCalls atomic.Int32
	c.SetChannelRestoredHook(func() { restoredAt.Store(opener.seq.Add(1)) })
	c.SetConsumerGoneHook(func() { goneCalls.Add(1) })
	closeConsumer(t, c)
	if !c.ChannelLostHookInstalled() {
		t.Fatal("ChannelLostHookInstalled must report the installed hook")
	}
	if r, g := c.ChannelLifecycleHooksInstalled(); !r || !g {
		t.Fatalf("ChannelLifecycleHooksInstalled = %v/%v, want both", r, g)
	}

	mi := types.LiveOnlyMessageInterest
	if _, err := c.Open(context.Background(), []string{"k"}, &mi); err != nil {
		t.Fatalf("Open: %v", err)
	}

	lost := time.Now()
	close(opener.chans[0]) // the broker took the channel (and the queue) away

	// The hook must have run even though Close is still hanging.
	deadline := time.Now().Add(2 * time.Second)
	for len(rec.snapshot()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("hook did not run while the old channel's Close was blocked")
		}
		time.Sleep(time.Millisecond)
	}
	if got := opener.channels[0].closedAt.Load(); got != 0 {
		t.Fatalf("Close ran (ordinal %d) before the hook — must not", got)
	}
	close(opener.channels[0].closeHang) // let Close proceed
	opener.waitCalls(t, 2)

	calls := rec.snapshot()
	if len(calls) != 1 {
		t.Fatalf("hook calls = %d, want exactly 1", len(calls))
	}
	if calls[0].interest != types.LiveOnlyMessageInterest {
		t.Fatalf("hook interest = %v, want the consumer's LiveOnly", calls[0].interest)
	}
	if calls[0].lostAt.Before(lost.Add(-time.Second)) || calls[0].lostAt.After(time.Now()) {
		t.Fatalf("lostAt = %v, want ≈ the moment the channel closed (%v)", calls[0].lostAt, lost)
	}
	closedAt := opener.channels[0].closedAt.Load()
	opener.mu.Lock()
	reopenedAt := opener.openedAt[1]
	opener.mu.Unlock()
	if !(calls[0].ordinal < closedAt && closedAt < reopenedAt) {
		t.Fatalf("ordering hook(%d) < Close(%d) < CreateChannel#2(%d) violated", calls[0].ordinal, closedAt, reopenedAt)
	}
	// The restored hook fires once the replacement queue is bound, and
	// the gone hook not before the consumer is closed.
	deadline = time.Now().Add(2 * time.Second)
	for restoredAt.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("restored hook did not fire after the reopen")
		}
		time.Sleep(time.Millisecond)
	}
	if got := restoredAt.Load(); got < reopenedAt {
		t.Fatalf("restored hook ran at %d, before CreateChannel#2 at %d", got, reopenedAt)
	}
	// … and before the replacement's watcher is armed, so a replacement
	// that dies at once cannot have its loss reported ahead of this
	// restore and erased by it.
	deadline = time.Now().Add(2 * time.Second)
	for opener.channels[1].watchedAt.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("replacement channel's watcher never armed")
		}
		time.Sleep(time.Millisecond)
	}
	if w, r := opener.channels[1].watchedAt.Load(), restoredAt.Load(); w < r {
		t.Fatalf("replacement watcher armed at %d, before the restore at %d", w, r)
	}
	if goneCalls.Load() != 0 {
		t.Fatal("gone hook fired while the consumer is still running")
	}
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelShutdown()
	_ = c.Close(shutdownCtx)
	if got := goneCalls.Load(); got != 1 {
		t.Fatalf("gone hook calls after close = %d, want exactly 1", got)
	}
	if elapsed := time.Since(lost); elapsed < channelReopenBackoff-50*time.Millisecond {
		t.Fatalf("reopen after %v; want ≥ %v backoff for a channel that died within its dwell", elapsed, channelReopenBackoff)
	}
}

// TestChannelConsumer_ChannelLost_WatcherReportsWhileRunIsParked is the
// point of the watcher: run() can sit in admit behind a slow session for
// an unbounded time, but the loss still reaches the hook the instant the
// broker closes the channel — and exactly once, even when run() later
// notices the same loss.
func TestChannelConsumer_ChannelLost_WatcherReportsWhileRunIsParked(t *testing.T) {
	shortDwell(t)
	opener := newSequencedOpener(2, false)
	rec := &lossRecorder{seq: &opener.seq}
	c := NewChannelConsumer(opener, &factory.FeedMessageFactory{}, discardConsumerLogger(), "ex", "od:sport:", 0)
	c.SetChannelLostHook(rec.hook)
	closeConsumer(t, c)
	mi := types.AllMessageInterest
	if _, err := c.Open(context.Background(), []string{"k"}, &mi); err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Park run(): a delivery it cannot hand off (nobody reads outgoing).
	opener.chans[0] <- testDelivery(&countingAcknowledger{})
	time.Sleep(20 * time.Millisecond) // let run reach admit

	// The broker closes the channel; the deliveries channel stays open
	// (as it would while amqp091's buffer still holds deliveries).
	opener.channels[0].fireClose(t)

	deadline := time.Now().Add(2 * time.Second)
	for len(rec.snapshot()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("watcher did not report the loss while run() was parked in admit")
		}
		time.Sleep(time.Millisecond)
	}
	if got := opener.calls.Load(); got != 1 {
		t.Fatalf("CreateChannel calls = %d while run is parked, want 1 (no reopen yet)", got)
	}

	// Now let run() proceed and observe the same loss; the hook must
	// not fire a second time for the same channel.
	c.mu.Lock()
	out := c.outgoing
	c.mu.Unlock()
	<-out // unpark admit
	close(opener.chans[0])
	opener.waitCalls(t, 2)
	if got := len(rec.snapshot()); got != 1 {
		t.Fatalf("hook calls = %d after run() also saw the loss, want 1 (per-channel once)", got)
	}
}

// TestChannelConsumer_ChannelLost_SecondLossReportsAgain pins the two
// invariants the throttle must not break: the hook fires for EVERY lost
// channel (only the log is throttled — after the first loss the second
// falls inside channelLossWarnInterval and is counted as suppressed), and
// the dwell clock is reset per channel, so a second short-lived channel
// also waits the reopen backoff instead of spinning.
func TestChannelConsumer_ChannelLost_SecondLossReportsAgain(t *testing.T) {
	shortDwell(t)
	opener := newSequencedOpener(3, false)
	rec := &lossRecorder{seq: &opener.seq}
	c := NewChannelConsumer(opener, &factory.FeedMessageFactory{}, discardConsumerLogger(), "ex", "od:sport:", 0)
	c.SetChannelLostHook(rec.hook)
	closeConsumer(t, c)
	mi := types.AllMessageInterest
	if _, err := c.Open(context.Background(), []string{"k"}, &mi); err != nil {
		t.Fatalf("Open: %v", err)
	}

	close(opener.chans[0])
	opener.waitCalls(t, 2)
	secondLost := time.Now()
	close(opener.chans[1])
	opener.waitCalls(t, 3)

	calls := rec.snapshot()
	if len(calls) != 2 {
		t.Fatalf("hook calls = %d after two losses, want 2 (hook must not be throttled with the log)", len(calls))
	}
	c.lossMu.Lock()
	suppressed := c.suppressedLosses
	c.lossMu.Unlock()
	if suppressed != 1 {
		t.Fatalf("suppressed log lines = %d, want 1 (second loss inside the warn interval)", suppressed)
	}
	if elapsed := time.Since(secondLost); elapsed < channelReopenBackoff-50*time.Millisecond {
		t.Fatalf("second reopen after %v; want ≥ %v — the dwell clock must reset per channel", elapsed, channelReopenBackoff)
	}
}

// TestChannelConsumer_LongLivedChannelReopensImmediately: the backoff is
// for flapping channels only; an ordinary reconnect after a long-lived
// channel must not pay it.
func TestChannelConsumer_LongLivedChannelReopensImmediately(t *testing.T) {
	oldDwell := minChannelDwell
	minChannelDwell = 20 * time.Millisecond
	t.Cleanup(func() { minChannelDwell = oldDwell })
	opener := newSequencedOpener(2, false)
	c := NewChannelConsumer(opener, &factory.FeedMessageFactory{}, discardConsumerLogger(), "ex", "od:sport:", 0)
	c.SetChannelLostHook(func(types.MessageInterest, time.Time) {})
	closeConsumer(t, c)
	mi := types.AllMessageInterest
	if _, err := c.Open(context.Background(), []string{"k"}, &mi); err != nil {
		t.Fatalf("Open: %v", err)
	}
	time.Sleep(2 * minChannelDwell) // the channel outlives its dwell
	lost := time.Now()
	close(opener.chans[0])
	opener.waitCalls(t, 2)
	if elapsed := time.Since(lost); elapsed >= channelReopenBackoff {
		t.Fatalf("long-lived channel waited %v before reopening; want immediate (< %v)", elapsed, channelReopenBackoff)
	}
}

// TestChannelConsumer_GracefulCloseIsNotALoss pins the drain exit of run
// AND the watcher: a graceful close (what Subscription.Close takes) must
// not report a channel loss even though the channel is closed by us —
// it would flag every producer down and force a snapshot recovery on a
// normal shutdown.
func TestChannelConsumer_GracefulCloseIsNotALoss(t *testing.T) {
	opener := newSequencedOpener(2, false)
	rec := &lossRecorder{seq: &opener.seq}
	c := NewChannelConsumer(opener, &factory.FeedMessageFactory{}, discardConsumerLogger(), "ex", "od:sport:", 0)
	c.SetChannelLostHook(rec.hook)
	mi := types.AllMessageInterest
	if _, err := c.Open(context.Background(), []string{"k"}, &mi); err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c.CloseGraceful(ctx)
	// The graceful teardown closes the channel; the watcher must treat
	// the resulting NotifyClose as ours, not the broker's.
	opener.channels[0].fireClose(t)
	time.Sleep(30 * time.Millisecond)

	if got := len(rec.snapshot()); got != 0 {
		t.Fatalf("graceful close reported %d channel loss(es), want 0", got)
	}
	if got := opener.calls.Load(); got != 1 {
		t.Fatalf("graceful close reopened the channel: CreateChannel calls = %d, want 1", got)
	}
}

// TestChannelConsumer_AbruptCloseIsNotALoss: the ctx exit of run and the
// watcher are equally silent.
func TestChannelConsumer_AbruptCloseIsNotALoss(t *testing.T) {
	opener := newSequencedOpener(2, false)
	rec := &lossRecorder{seq: &opener.seq}
	c := NewChannelConsumer(opener, &factory.FeedMessageFactory{}, discardConsumerLogger(), "ex", "od:sport:", 0)
	c.SetChannelLostHook(rec.hook)
	mi := types.AllMessageInterest
	if _, err := c.Open(context.Background(), []string{"k"}, &mi); err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = c.Close(ctx)
	opener.channels[0].fireClose(t)
	time.Sleep(30 * time.Millisecond)
	if got := len(rec.snapshot()); got != 0 {
		t.Fatalf("abrupt close reported %d channel loss(es), want 0", got)
	}
}

// TestChannelConsumer_NoHookNoPanic: consumers without a hook (tests,
// replay sessions) reopen as before and run no watcher.
func TestChannelConsumer_NoHookNoPanic(t *testing.T) {
	shortDwell(t)
	opener := newSequencedOpener(2, false)
	c := NewChannelConsumer(opener, &factory.FeedMessageFactory{}, discardConsumerLogger(), "ex", "od:sport:", 0)
	closeConsumer(t, c)
	mi := types.AllMessageInterest
	if _, err := c.Open(context.Background(), []string{"k"}, &mi); err != nil {
		t.Fatalf("Open: %v", err)
	}
	close(opener.chans[0])
	opener.waitCalls(t, 2)
}

// failAfterOpener serves one good channel and fails every CreateChannel
// after it, so the reopen loop sits in its retry until closed.
type failAfterOpener struct {
	sequencedOpener
}

func (o *failAfterOpener) CreateChannel(ctx context.Context, keys []string, ex string, prefetch int) (<-chan amqp.Delivery, amqpChannel, error) {
	if o.calls.Load() >= 1 {
		o.calls.Add(1)
		return nil, nil, context.DeadlineExceeded
	}
	return o.sequencedOpener.CreateChannel(ctx, keys, ex, prefetch)
}

// TestChannelConsumer_CloseDuringReopenBackoffDoesNotPanic: a lost
// channel followed by Close while the reopen is waiting out the dwell
// backoff. The watcher was already stopped in-loop; the deferred stop on
// the ctx exit must not close it again (that was a process-killing panic
// on the consumer goroutine).
func TestChannelConsumer_CloseDuringReopenBackoffDoesNotPanic(t *testing.T) {
	oldDwell, oldBackoff := minChannelDwell, channelReopenBackoff
	minChannelDwell, channelReopenBackoff = 10*time.Second, 2*time.Second
	t.Cleanup(func() { minChannelDwell, channelReopenBackoff = oldDwell, oldBackoff })

	for _, graceful := range []bool{false, true} {
		opener := newSequencedOpener(2, false)
		c := NewChannelConsumer(opener, &factory.FeedMessageFactory{}, discardConsumerLogger(), "ex", "od:sport:", 0)
		c.SetChannelLostHook(func(types.MessageInterest, time.Time) {})
		mi := types.AllMessageInterest
		if _, err := c.Open(context.Background(), []string{"k"}, &mi); err != nil {
			t.Fatalf("Open: %v", err)
		}
		close(opener.chans[0])
		time.Sleep(50 * time.Millisecond) // run() is now inside the backoff select

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if graceful {
			c.CloseGraceful(ctx)
		} else {
			_ = c.Close(ctx)
		}
		cancel()
		done := make(chan struct{})
		go func() { c.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatalf("graceful=%v: consumer goroutines did not exit after close during backoff", graceful)
		}
		if got := opener.calls.Load(); got != 1 {
			t.Fatalf("graceful=%v: CreateChannel calls = %d, want 1 (closed before reopen)", graceful, got)
		}
	}
}

// TestChannelConsumer_CloseDuringFailingReopenDoesNotPanic: the other
// exit in the same window — the reopen keeps failing and Close lands in
// its retry loop.
func TestChannelConsumer_CloseDuringFailingReopenDoesNotPanic(t *testing.T) {
	oldDwell := minChannelDwell
	minChannelDwell = 0 // no backoff: go straight to the failing reopen
	t.Cleanup(func() { minChannelDwell = oldDwell })

	opener := &failAfterOpener{sequencedOpener: *newSequencedOpener(1, false)}
	c := NewChannelConsumer(opener, &factory.FeedMessageFactory{}, discardConsumerLogger(), "ex", "od:sport:", 0)
	c.SetChannelLostHook(func(types.MessageInterest, time.Time) {})
	var goneCalls atomic.Int32
	c.SetConsumerGoneHook(func() { goneCalls.Add(1) })
	mi := types.AllMessageInterest
	if _, err := c.Open(context.Background(), []string{"k"}, &mi); err != nil {
		t.Fatalf("Open: %v", err)
	}
	close(opener.chans[0])
	deadline := time.Now().Add(2 * time.Second)
	for opener.calls.Load() < 2 { // at least one failed reopen attempt
		if time.Now().After(deadline) {
			t.Fatal("reopen never attempted")
		}
		time.Sleep(2 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = c.Close(ctx)
	done := make(chan struct{})
	go func() { c.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("consumer goroutines did not exit after close during a failing reopen")
	}
	// A consumer that never got its queue back but is closed must report
	// gone, so it stops holding recoveries back.
	if got := goneCalls.Load(); got != 1 {
		t.Fatalf("gone hook calls = %d, want 1 after close during a failing reopen", got)
	}
}

// TestChannelConsumer_ReplacementLostAtOnce_ReportsLossAfterRestore: a
// replacement channel that the broker takes away immediately must be
// reported as lost AFTER its restore was reported, exactly once, however
// the watcher and run() interleave — otherwise the stale restore would
// erase the newer loss in a session-keyed ledger.
func TestChannelConsumer_ReplacementLostAtOnce_ReportsLossAfterRestore(t *testing.T) {
	shortDwell(t)
	opener := newSequencedOpener(3, false)
	rec := &lossRecorder{seq: &opener.seq}
	c := NewChannelConsumer(opener, &factory.FeedMessageFactory{}, discardConsumerLogger(), "ex", "od:sport:", 0)
	c.SetChannelLostHook(rec.hook)
	var restored []int32
	var restoredMu sync.Mutex
	c.SetChannelRestoredHook(func() {
		restoredMu.Lock()
		restored = append(restored, opener.seq.Add(1))
		restoredMu.Unlock()
	})
	closeConsumer(t, c)
	mi := types.AllMessageInterest
	if _, err := c.Open(context.Background(), []string{"k"}, &mi); err != nil {
		t.Fatalf("Open: %v", err)
	}

	close(opener.chans[0]) // first loss
	opener.waitCalls(t, 2)
	// Take the replacement away the instant it exists: fire its
	// NotifyClose (watcher path) and close its deliveries (run path).
	opener.channels[1].fireClose(t)
	close(opener.chans[1])
	opener.waitCalls(t, 3)

	deadline := time.Now().Add(2 * time.Second)
	for len(rec.snapshot()) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("second loss not reported: %d report(s)", len(rec.snapshot()))
		}
		time.Sleep(time.Millisecond)
	}
	calls := rec.snapshot()
	if len(calls) != 2 {
		t.Fatalf("loss reports = %d, want exactly 2", len(calls))
	}
	restoredMu.Lock()
	rs := append([]int32(nil), restored...)
	restoredMu.Unlock()
	if len(rs) < 1 {
		t.Fatal("no restore reported for the first reopen")
	}
	if !(calls[0].ordinal < rs[0] && rs[0] < calls[1].ordinal) {
		t.Fatalf("ordering loss#1(%d) < restore#1(%d) < loss#2(%d) violated", calls[0].ordinal, rs[0], calls[1].ordinal)
	}
}

// TestChannelConsumer_GoneIsOrderedAfterAnInFlightLoss: the watcher can
// be mid-report when the consumer is closed. The gone report — which
// settles the session in the recovery manager's session-keyed ledger —
// must not overtake it: a loss landing after gone would stay in that
// ledger forever and defer every later recovery for the producers the
// session served.
func TestChannelConsumer_GoneIsOrderedAfterAnInFlightLoss(t *testing.T) {
	opener := newSequencedOpener(2, false)
	c := NewChannelConsumer(opener, &factory.FeedMessageFactory{}, discardConsumerLogger(), "ex", "od:sport:", 0)

	inHook := make(chan struct{})
	release := make(chan struct{})
	var lossDone, goneAt atomic.Int32
	var hookOnce sync.Once
	c.SetChannelLostHook(func(types.MessageInterest, time.Time) {
		hookOnce.Do(func() { close(inHook) })
		<-release
		lossDone.Store(opener.seq.Add(1))
	})
	c.SetConsumerGoneHook(func() { goneAt.Store(opener.seq.Add(1)) })

	mi := types.AllMessageInterest
	if _, err := c.Open(context.Background(), []string{"k"}, &mi); err != nil {
		t.Fatalf("Open: %v", err)
	}

	opener.channels[0].fireClose(t) // the broker takes the channel away
	select {
	case <-inHook:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher never entered the loss hook")
	}

	// Close lands while the loss report is still running.
	closed := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.Close(ctx)
		close(closed)
	}()
	time.Sleep(50 * time.Millisecond)
	if goneAt.Load() != 0 {
		t.Fatal("gone reported while a loss report was still in flight")
	}

	close(release)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after the loss report completed")
	}
	l, g := lossDone.Load(), goneAt.Load()
	if l == 0 || g == 0 || g < l {
		t.Fatalf("ordering loss(%d) < gone(%d) violated", l, g)
	}
}
