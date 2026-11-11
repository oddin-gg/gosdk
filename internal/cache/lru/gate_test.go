package lru

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/oddin-gg/gosdk/internal/apipolicy"
)

// testGate is a gate the tests open and close by hand.
type testGate struct {
	mu     sync.Mutex
	open   bool
	opened chan struct{}
}

func newTestGate() *testGate { return &testGate{opened: make(chan struct{})} }

func (g *testGate) Open() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.open
}

func (g *testGate) Opened() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.opened
}

func (g *testGate) setOpen() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.open {
		g.open = true
		close(g.opened)
	}
}

// countFlights counts the registrations on a flight (starter or joiner)
// until the returned function is called.
func countFlights(t *testing.T) (count *atomic.Int32, restore func()) {
	t.Helper()
	count = new(atomic.Int32)
	restore = SetFlightRegisteredHook(func() { count.Add(1) })
	t.Cleanup(restore)
	return count, restore
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// With the gate open, a marked caller neither starts nor joins a flight.
func TestLoadCoalesced_GateOpenSkipsWithoutStartingOrJoiningAFlight(t *testing.T) {
	var sf singleflight.Group
	gate := newTestGate()
	flights, _ := countFlights(t)

	// An unmarked caller's flight is running; the marked one must not join it.
	release := make(chan struct{})
	var loads atomic.Int32
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = LoadCoalesced(t.Context(), t.Context(), &sf, "k", func(context.Context) (int, error) {
			loads.Add(1)
			close(started)
			<-release
			return 1, nil
		})
	}()
	<-started
	defer func() { close(release); <-done }()

	gate.setOpen()
	marked := apipolicy.WithMessage(t.Context(), gate)
	_, err := LoadCoalesced(marked, t.Context(), &sf, "k", func(context.Context) (int, error) {
		loads.Add(1)
		return 2, nil
	})
	if !errors.Is(err, apipolicy.ErrSkipped) {
		t.Fatalf("err = %v, want ErrSkipped", err)
	}
	if got := flights.Load(); got != 1 {
		t.Fatalf("flight registrations = %d, want 1 (the marked caller joined or started one)", got)
	}
	if got := loads.Load(); got != 1 {
		t.Fatalf("loads = %d, want 1", got)
	}

	// A key nobody flies on: skipped without a load as well.
	_, err = LoadCoalesced(marked, t.Context(), &sf, "other", func(context.Context) (int, error) {
		loads.Add(1)
		return 3, nil
	})
	if !errors.Is(err, apipolicy.ErrSkipped) || loads.Load() != 1 {
		t.Fatalf("err = %v loads = %d, want ErrSkipped and no load", err, loads.Load())
	}
}

// A caller that waits on a flight is released the moment the gate opens, and
// the flight goes on: the next caller finds what it loaded.
func TestLoadCoalesced_GateOpenedReleasesAWaiterAndTheFlightGoesOn(t *testing.T) {
	var sf singleflight.Group
	gate := newTestGate()
	marked := apipolicy.WithMessage(t.Context(), gate)

	release := make(chan struct{})
	var stored atomic.Int32
	entered := make(chan struct{})
	load := func(context.Context) (int, error) {
		close(entered)
		<-release
		stored.Store(7)
		return 7, nil
	}

	errc := make(chan error, 1)
	go func() {
		_, err := LoadCoalesced(marked, t.Context(), &sf, "k", load)
		errc <- err
	}()
	<-entered
	select {
	case err := <-errc:
		t.Fatalf("returned before the gate opened: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	gate.setOpen()
	select {
	case err := <-errc:
		if !errors.Is(err, apipolicy.ErrSkipped) {
			t.Fatalf("err = %v, want ErrSkipped", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter was not released by the gate opening")
	}

	// The flight still runs to its end after its waiter left.
	close(release)
	waitFor(t, "the flight to complete", func() bool { return stored.Load() == 7 })
	got, err := LoadCoalesced(t.Context(), t.Context(), &sf, "k", func(context.Context) (int, error) { return 7, nil })
	if err != nil || got != 7 {
		t.Fatalf("got=%d err=%v, want 7", got, err)
	}
}

// A caller without the mark ignores the gate, open or not; so does a marked
// caller while the gate is closed.
func TestLoadCoalesced_UnmarkedCallerIgnoresTheGate(t *testing.T) {
	var sf singleflight.Group
	gate := newTestGate()
	gate.setOpen()

	got, err := LoadCoalesced(t.Context(), t.Context(), &sf, "k", func(context.Context) (int, error) { return 5, nil })
	if err != nil || got != 5 {
		t.Fatalf("unmarked: got=%d err=%v", got, err)
	}

	closed := newTestGate()
	got, err = LoadCoalesced(apipolicy.WithMessage(t.Context(), closed), t.Context(), &sf, "k2", func(context.Context) (int, error) { return 6, nil })
	if err != nil || got != 6 {
		t.Fatalf("marked, gate closed: got=%d err=%v", got, err)
	}
}

// The flight itself is not the message path's: what it loads in turn must
// not see the caller's mark, even when it is rooted in the caller's context.
func TestLoadCoalesced_TheFlightDoesNotCarryTheGate(t *testing.T) {
	var sf singleflight.Group
	gate := newTestGate()
	marked := apipolicy.WithMessage(t.Context(), gate)
	var sawGate atomic.Bool
	_, err := LoadCoalesced(marked, nil, &sf, "k", func(loadCtx context.Context) (int, error) {
		sawGate.Store(apipolicy.GateOf(loadCtx) != nil)
		return 1, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if sawGate.Load() {
		t.Fatal("the flight's context carried the caller's message mark")
	}
}

func TestEventCache_GateOpenSkipsWithoutStartingOrJoiningAFlight(t *testing.T) {
	var calls atomic.Int32
	c := NewEventCache[string, string, *localizedFoo](Config{}, recordingLoader(&calls, false))
	gate := newTestGate()
	gate.setOpen()
	flights, _ := countFlights(t)

	_, _, err := c.Get(apipolicy.WithMessage(t.Context(), gate), "m", []string{"en"})
	if !errors.Is(err, apipolicy.ErrSkipped) {
		t.Fatalf("err = %v, want ErrSkipped", err)
	}
	if calls.Load() != 0 || flights.Load() != 0 {
		t.Fatalf("loads=%d flights=%d, want none", calls.Load(), flights.Load())
	}

	// Cached entries are still served: the gate only stops loads.
	if _, _, err := c.Get(t.Context(), "m", []string{"en"}); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := c.Get(apipolicy.WithMessage(t.Context(), gate), "m", []string{"en"}); err != nil || !ok || v == nil {
		t.Fatalf("a cached entry was not served with the gate open: ok=%v err=%v", ok, err)
	}
}

func TestEventCache_GateOpenedReleasesAWaiterAndTheFlightFillsTheCache(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	var inner atomic.Int32
	base := recordingLoader(&inner, false)
	c := NewEventCache[string, string, *localizedFoo](Config{}, func(ctx context.Context, key string, locales []string, existing *localizedFoo, has bool) (*localizedFoo, error) {
		once.Do(func() { close(entered) })
		<-release
		return base(ctx, key, locales, existing, has)
	})
	gate := newTestGate()
	marked := apipolicy.WithMessage(t.Context(), gate)

	errc := make(chan error, 1)
	go func() {
		_, _, err := c.Get(marked, "m", []string{"en"})
		errc <- err
	}()
	<-entered
	gate.setOpen()
	select {
	case err := <-errc:
		if !errors.Is(err, apipolicy.ErrSkipped) {
			t.Fatalf("err = %v, want ErrSkipped", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter was not released by the gate opening")
	}

	close(release)
	waitFor(t, "the flight to fill the cache", func() bool { _, ok := c.Peek("m"); return ok })
	if inner.Load() != 1 {
		t.Fatalf("loader calls = %d, want 1", inner.Load())
	}
}

func TestEventCache_UnmarkedCallerIgnoresTheGate(t *testing.T) {
	var calls atomic.Int32
	c := NewEventCache[string, string, *localizedFoo](Config{}, recordingLoader(&calls, false))
	if _, ok, err := c.Get(t.Context(), "m", []string{"en"}); err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("loader calls = %d, want 1", calls.Load())
	}
}

func TestEventCache_TheFlightDoesNotCarryTheGate(t *testing.T) {
	var sawGate atomic.Bool
	c := NewEventCache[string, string, *localizedFoo](Config{}, func(ctx context.Context, key string, locales []string, existing *localizedFoo, has bool) (*localizedFoo, error) {
		sawGate.Store(apipolicy.GateOf(ctx) != nil)
		var n atomic.Int32
		return recordingLoader(&n, false)(ctx, key, locales, existing, has)
	})
	if _, _, err := c.Get(apipolicy.WithMessage(t.Context(), newTestGate()), "m", []string{"en"}); err != nil {
		t.Fatal(err)
	}
	if sawGate.Load() {
		t.Fatal("the flight's context carried the caller's message mark")
	}
}
