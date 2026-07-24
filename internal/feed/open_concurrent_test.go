package feed

import (
	"context"
	"errors"
	"testing"
	"time"
)

// stageInFlightOpen puts a Client into the "attempt in flight" state the
// way Open's owner path does, returning the attempt record the owner
// will settle. Tests drive the REAL waiter path (Client.Open) against it.
func stageInFlightOpen() (*Client, *openResult) {
	res := &openResult{done: make(chan struct{})}
	return &Client{
		opening:     true,
		openAttempt: res,
		closed:      make(chan struct{}),
	}, res
}

// settleOpen publishes an attempt outcome exactly like Open's defer:
// fields first, close(done) last, under mu.
func settleOpen(c *Client, res *openResult, opened bool, err error) {
	c.mu.Lock()
	c.opening = false
	c.opened = opened
	res.err = err
	res.opened = opened
	close(res.done)
	c.mu.Unlock()
}

// TestClient_Open_ConcurrentWaiterSeesError verifies the v2.5 review
// fix through the REAL waiter path: a second concurrent Open(ctx) waits
// on the in-flight attempt and observes that attempt's error.
func TestClient_Open_ConcurrentWaiterSeesError(t *testing.T) {
	c, res := stageInFlightOpen()
	want := errors.New("dial failed")

	doneB := make(chan error, 1)
	go func() { doneB <- c.Open(t.Context()) }()

	time.Sleep(20 * time.Millisecond) // let B enter the waiter branch
	settleOpen(c, res, false, want)

	select {
	case got := <-doneB:
		if !errors.Is(got, want) {
			t.Errorf("waiter saw err = %v, want %v", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter never returned")
	}
}

// TestClient_Open_ConcurrentWaiterSeesSuccess: same setup, success
// outcome — waiters return nil.
func TestClient_Open_ConcurrentWaiterSeesSuccess(t *testing.T) {
	c, res := stageInFlightOpen()

	doneB := make(chan error, 1)
	go func() { doneB <- c.Open(t.Context()) }()

	time.Sleep(20 * time.Millisecond)
	settleOpen(c, res, true, nil)

	select {
	case got := <-doneB:
		if got != nil {
			t.Errorf("waiter saw err = %v, want nil", got)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter never returned")
	}
}

// TestClient_Open_WaiterCtxCancelled: waiter's ctx expires before the
// in-flight Open settles → ctx.Err() returned.
func TestClient_Open_WaiterCtxCancelled(t *testing.T) {
	c, _ := stageInFlightOpen()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := c.Open(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("waiter saw err = %v, want context.Canceled", err)
	}
}

// TestClient_Open_WaiterImmuneToAttemptRollover is the regression for
// the failure/retry rollover (Codex P2): waiter B captured attempt A's
// done channel, A failed with E1 and closed it, and — before B woke — a
// fresh attempt C cleared the Client's mutable openErr/opened and
// replaced openDone. B then read C's state: a generic "did not yield a
// connection" (or C's eventual success) instead of E1, losing errors.Is
// classification of the real failure. Post-fix each attempt's outcome
// lives on an immutable per-attempt record, so B reports E1 no matter
// what later attempts do.
func TestClient_Open_WaiterImmuneToAttemptRollover(t *testing.T) {
	c, resA := stageInFlightOpen()
	e1 := errors.New("attempt A: broker unreachable")

	doneB := make(chan error, 1)
	go func() { doneB <- c.Open(t.Context()) }()
	time.Sleep(20 * time.Millisecond) // let B capture attempt A's record

	// A fails with E1 — and attempt C starts IMMEDIATELY, replacing all
	// mutable state before B is scheduled again.
	settleOpen(c, resA, false, e1)
	c.mu.Lock()
	c.opening = true
	c.openAttempt = &openResult{done: make(chan struct{})} // attempt C, unsettled
	c.mu.Unlock()

	select {
	case got := <-doneB:
		if !errors.Is(got, e1) {
			t.Fatalf("waiter saw %v, want attempt A's error %v (rollover leaked attempt C's state)", got, e1)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter never returned (blocked on attempt C's unsettled record?)")
	}
}
