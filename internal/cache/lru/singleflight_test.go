package lru

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/oddin-gg/gosdk/internal/apipolicy"
)

// TestLoadCoalesced_Basic exercises the happy path: fn runs once,
// returns a value, caller receives it.
func TestLoadCoalesced_Basic(t *testing.T) {
	var sf singleflight.Group
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
	var sf singleflight.Group
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
	var sf singleflight.Group
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
	var sf singleflight.Group
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
	var sf singleflight.Group
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

// TestLoadCoalesced_CarriesCallPolicyAndSeparatesFlights: the shared load is
// detached from the caller's cancellation but keeps the caller's call
// policy, and callers of different policies never share a flight.
func TestLoadCoalesced_CarriesCallPolicyAndSeparatesFlights(t *testing.T) {
	var sf singleflight.Group
	var mu sync.Mutex
	got := map[apipolicy.Mode]int{}
	release := make(chan struct{})
	fn := func(ctx context.Context) (int, error) {
		mu.Lock()
		got[apipolicy.ModeOf(ctx)]++
		mu.Unlock()
		<-release
		return 1, nil
	}
	var wg sync.WaitGroup
	for _, ctx := range []context.Context{t.Context(), apipolicy.With(t.Context(), apipolicy.Message)} {
		wg.Go(func() {
			if _, err := LoadCoalesced(ctx, t.Context(), &sf, "k", fn); err != nil {
				t.Error(err)
			}
		})
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		n := got[apipolicy.Default] + got[apipolicy.Message]
		mu.Unlock()
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("loads started = %d, want 2 (a flight per policy)", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	wg.Wait()
	if got[apipolicy.Default] != 1 || got[apipolicy.Message] != 1 {
		t.Fatalf("loader contexts carried %v, want one Default and one Message", got)
	}
}

// gatedLoader is a load whose first call blocks on gate and then fails with
// the budget of its starter running out; later calls succeed with 7.
type gatedLoader struct {
	calls   atomic.Int32
	entered chan struct{}
	gate    chan struct{}
}

func newGatedLoader() *gatedLoader {
	return &gatedLoader{entered: make(chan struct{}), gate: make(chan struct{})}
}

func (g *gatedLoader) fn(context.Context) (int, error) {
	if g.calls.Add(1) == 1 {
		close(g.entered)
		<-g.gate
		return 0, context.DeadlineExceeded
	}
	return 7, nil
}

type loadResult struct {
	v   int
	err error
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

// A message-path caller that joined a flight started by a message with
// little budget left gets that flight's deadline error without having used
// its own budget: it asks once more under its own. The starter keeps its own
// error and does not retry.
func TestLoadCoalesced_JoinerRetriesOnceWhenTheStartersBudgetRanOut(t *testing.T) {
	regs := trackRegistrations(t)
	var sf singleflight.Group
	g := newGatedLoader()
	starterCtx := apipolicy.WithBudget(t.Context(), apipolicy.Message, time.Millisecond)
	joinerCtx := apipolicy.With(t.Context(), apipolicy.Message)

	starter := make(chan loadResult, 1)
	go func() {
		v, err := LoadCoalesced(starterCtx, nil, &sf, "k", g.fn)
		starter <- loadResult{v, err}
	}()
	<-g.entered
	joiner := make(chan loadResult, 1)
	go func() {
		v, err := LoadCoalesced(joinerCtx, nil, &sf, "k", g.fn)
		joiner <- loadResult{v, err}
	}()
	regs.wait(t, 2)
	close(g.gate)

	if r := <-joiner; r.err != nil || r.v != 7 {
		t.Fatalf("joiner = %+v, want 7 from the retry under its own budget", r)
	}
	if r := <-starter; !errors.Is(r.err, context.DeadlineExceeded) {
		t.Fatalf("starter err = %v, want its own deadline error", r.err)
	}
	if got := g.calls.Load(); got != 2 {
		t.Fatalf("loads = %d, want 2 (the flight and one retry; the starter does not retry)", got)
	}
}

// A joiner whose own budget is spent has nothing to retry with, and a
// public getter's load (default policy) is left alone.
func TestLoadCoalesced_JoinerWithoutBudgetOrOnTheDefaultPolicyDoesNotRetry(t *testing.T) {
	cases := []struct {
		name string
		ctx  func(context.Context) context.Context
	}{
		{"own budget spent", func(c context.Context) context.Context {
			return apipolicy.WithBudget(c, apipolicy.Message, -time.Second)
		}},
		{"default policy", func(c context.Context) context.Context { return c }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			regs := trackRegistrations(t)
			var sf singleflight.Group
			g := newGatedLoader()
			// The starter shares the joiner's flight key (same mode).
			starterCtx := tc.ctx(t.Context())
			starter := make(chan loadResult, 1)
			go func() {
				v, err := LoadCoalesced(starterCtx, nil, &sf, "k", g.fn)
				starter <- loadResult{v, err}
			}()
			<-g.entered
			joiner := make(chan loadResult, 1)
			go func() {
				v, err := LoadCoalesced(tc.ctx(t.Context()), nil, &sf, "k", g.fn)
				joiner <- loadResult{v, err}
			}()
			regs.wait(t, 2)
			close(g.gate)

			if r := <-joiner; !errors.Is(r.err, context.DeadlineExceeded) {
				t.Fatalf("joiner = %+v, want the flight's deadline error", r)
			}
			<-starter
			if got := g.calls.Load(); got != 1 {
				t.Fatalf("loads = %d, want 1 (no retry)", got)
			}
		})
	}
}

// At most one retry per call: a second deadline error is returned.
func TestLoadCoalesced_JoinerRetriesAtMostOnce(t *testing.T) {
	regs := trackRegistrations(t)
	var sf singleflight.Group
	var calls atomic.Int32
	entered := make(chan struct{})
	gate := make(chan struct{})
	fn := func(context.Context) (int, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-gate
		}
		return 0, context.DeadlineExceeded
	}
	go func() { _, _ = LoadCoalesced(apipolicy.With(t.Context(), apipolicy.Message), nil, &sf, "k", fn) }()
	<-entered
	joiner := make(chan error, 1)
	go func() {
		_, err := LoadCoalesced(apipolicy.With(t.Context(), apipolicy.Message), nil, &sf, "k", fn)
		joiner <- err
	}()
	regs.wait(t, 2)
	close(gate)
	if err := <-joiner; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the deadline error after the one retry", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("loads = %d, want 2", got)
	}
}

func TestJoinerMayRetry(t *testing.T) {
	cancelled, cancel := context.WithCancel(apipolicy.With(context.Background(), apipolicy.Message))
	cancel()
	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{"retry case", apipolicy.With(context.Background(), apipolicy.Message), context.DeadlineExceeded, true},
		{"wrapped deadline error", apipolicy.With(context.Background(), apipolicy.Message), fmt.Errorf("fetch: %w", context.DeadlineExceeded), true},
		{"skip mode with budget", apipolicy.With(context.Background(), apipolicy.MessageSkip), context.DeadlineExceeded, true},
		{"not a deadline error", apipolicy.With(context.Background(), apipolicy.Message), errors.New("boom"), false},
		{"cancelled error", apipolicy.With(context.Background(), apipolicy.Message), context.Canceled, false},
		{"own context cancelled", cancelled, context.DeadlineExceeded, false},
		{"default mode", context.Background(), context.DeadlineExceeded, false},
		{"own budget spent", apipolicy.WithBudget(context.Background(), apipolicy.Message, -time.Second), context.DeadlineExceeded, false},
		{"no error", apipolicy.With(context.Background(), apipolicy.Message), nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := JoinerMayRetry(tc.ctx, tc.err); got != tc.want {
				t.Fatalf("JoinerMayRetry = %v, want %v", got, tc.want)
			}
		})
	}
}

// The starter of a flight owns its error: with budget left and a flight that
// ends in a deadline error (a slow API, not a short budget) it must not ask
// again, or one slow load would be sent twice.
func TestLoadCoalesced_StarterWithBudgetLeftDoesNotRetryItsOwnDeadline(t *testing.T) {
	var sf singleflight.Group
	var calls atomic.Int32
	_, err := LoadCoalesced(apipolicy.With(t.Context(), apipolicy.Message), nil, &sf, "k",
		func(context.Context) (int, error) {
			calls.Add(1)
			return 0, context.DeadlineExceeded
		})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the starter's own deadline error", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("loads = %d, want 1", got)
	}
}
