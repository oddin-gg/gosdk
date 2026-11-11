package lru

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestLoadCoalesced_Basic exercises the happy path: fn runs once,
// returns a value, caller receives it.
func TestLoadCoalesced_Basic(t *testing.T) {
	var sf Flights
	got, err := LoadCoalesced(t.Context(), nil, &sf, "k", func(ctx context.Context) (int, error) {
		return 42, nil
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != 42 {
		t.Errorf("got = %d, want 42", got)
	}
}

// TestLoadCoalesced_ErrorPropagates verifies fn errors reach the caller.
func TestLoadCoalesced_ErrorPropagates(t *testing.T) {
	var sf Flights
	wantErr := errors.New("boom")
	_, err := LoadCoalesced(t.Context(), nil, &sf, "k", func(ctx context.Context) (int, error) {
		return 0, wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want %v", err, wantErr)
	}
}

// TestLoadCoalesced_AlreadyCancelledCtxNoFetch — already-cancelled ctx
// must not start a detached load.
func TestLoadCoalesced_AlreadyCancelledCtxNoFetch(t *testing.T) {
	var sf Flights
	var calls atomic.Int32
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := LoadCoalesced(ctx, nil, &sf, "k", func(ctx context.Context) (int, error) {
		calls.Add(1)
		return 1, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := calls.Load(); got != 0 {
		t.Errorf("fn invocations = %d, want 0", got)
	}
}

// TestLoadCoalesced_FirstCallerCancellationDoesNotKillSharedLoad —
// short-deadline first caller cannot cancel the load for later waiters.
func TestLoadCoalesced_FirstCallerCancellationDoesNotKillSharedLoad(t *testing.T) {
	var sf Flights
	var calls atomic.Int32
	// Handshake instead of sleeps: `started` proves caller A OWNS the
	// registered flight before B launches (a descheduled A would
	// previously let B start a fresh flight and the test pass without
	// exercising the regression); `release` holds the load open past
	// A's deadline deterministically.
	started := make(chan struct{})
	release := make(chan struct{})
	fn := func(ctx context.Context) (int, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
		case <-ctx.Done():
			// Pre-fix the first caller's ctx propagated here; firing
			// this branch would mean the bug is back.
			return 0, ctx.Err()
		}
		return 7, nil
	}

	type result struct {
		v   int
		err error
	}

	aDone := make(chan result, 1)
	go func() {
		ctxA, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
		defer cancel()
		v, err := LoadCoalesced(ctxA, nil, &sf, "k", fn)
		aDone <- result{v, err}
	}()

	<-started // A's flight is registered and its load is running

	bDone := make(chan result, 1)
	go func() {
		ctxB, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		v, err := LoadCoalesced(ctxB, nil, &sf, "k", fn)
		bDone <- result{v, err}
	}()

	a := <-aDone // A deadline-exceeds while the load is still held open
	if !errors.Is(a.err, context.DeadlineExceeded) {
		t.Errorf("caller A err = %v, want DeadlineExceeded", a.err)
	}

	close(release)
	b := <-bDone
	if b.err != nil {
		t.Fatalf("caller B failed: %v — A's cancellation killed the shared load", b.err)
	}
	if b.v != 7 {
		t.Errorf("caller B got = %d, want 7", b.v)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("fn invocations = %d, want 1 (singleflight should coalesce)", got)
	}
}

// TestLoadCoalesced_SecondCallerCtxBoundedWait — a slow load must not
// block a later caller past its own deadline.
func TestLoadCoalesced_SecondCallerCtxBoundedWait(t *testing.T) {
	var sf Flights
	released := make(chan struct{})
	defer close(released)
	started := make(chan struct{})
	var startOnce sync.Once
	fn := func(ctx context.Context) (int, error) {
		startOnce.Do(func() { close(started) })
		<-released
		return 1, nil
	}

	// Caller A starts the load and parks on `released`. Wait for the
	// loader to SIGNAL it is running (handshake, not a sleep) so A
	// provably owns the flight before B joins — a descheduled A could
	// otherwise let B become the owner and satisfy the assertions
	// without ever being a concurrent waiter.
	go func() {
		_, _ = LoadCoalesced(t.Context(), nil, &sf, "k", fn)
	}()
	<-started

	// Caller B has 50ms deadline. With the DoChan + per-caller select
	// pattern, B must return promptly with ctx.DeadlineExceeded — not
	// wait for A's load to finish.
	ctxB, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := LoadCoalesced(ctxB, nil, &sf, "k", fn)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("Get blocked for %v — caller ctx not honored independently", elapsed)
	}
}

// registrations counts the loaders that registered on a flight (starters and
// joiners alike), so a test can release a gated flight once its joiner is
// certainly parked on it instead of sleeping and hoping it got there.
type registrations struct{ n atomic.Int32 }

func trackRegistrations(t *testing.T) *registrations {
	t.Helper()
	r := &registrations{}
	t.Cleanup(SetFlightRegisteredHook(func() { r.n.Add(1) }))
	return r
}

// wait blocks until want loaders have registered.
func (r *registrations) wait(t *testing.T, want int32) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for r.n.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("only %d loaders registered on the flight, want %d", r.n.Load(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

// StartCoalesced returns at once, runs one flight for any number of starts
// and a LoadCoalesced caller that asks meanwhile shares it.
func TestStartCoalesced_OneFlightWithoutWaiting(t *testing.T) {
	var sf Flights
	var calls atomic.Int32
	release := make(chan struct{})
	fn := func(ctx context.Context) (int, error) {
		calls.Add(1)
		<-release
		return 7, nil
	}
	for range 5 {
		StartCoalesced(t.Context(), &sf, "k", fn)
	}
	got := make(chan int, 1)
	go func() {
		v, _ := LoadCoalesced(t.Context(), t.Context(), &sf, "k", fn)
		got <- v
	}()
	time.Sleep(50 * time.Millisecond)
	close(release)
	if v := <-got; v != 7 {
		t.Fatalf("waiter got %d, want the flight's 7", v)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("flights = %d, want 1", n)
	}
}

// The flight lives on the lifetime it was given: Close of the owner cancels
// it, and the caller that started it has no say.
func TestStartCoalesced_RunsOnTheLifetime(t *testing.T) {
	var sf Flights
	life, stop := context.WithCancel(t.Context())
	seen := make(chan error, 1)
	StartCoalesced(life, &sf, "k", func(ctx context.Context) (int, error) {
		<-ctx.Done()
		seen <- ctx.Err()
		return 0, ctx.Err()
	})
	stop()
	select {
	case err := <-seen:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("flight ended with %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the flight did not stop with its lifetime")
	}
}
