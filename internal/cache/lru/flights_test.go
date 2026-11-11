package lru

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/internal/apipolicy"
)

// messageWait is the message timeout of the tests below.
const messageWait = 100 * time.Millisecond

// scheduleSlack is how late a goroutine may be on a loaded machine before a
// timing assertion means something. It stays far below the ten seconds a
// message would wait if the age of the flight did not end its wait.
const scheduleSlack = time.Second

// A message that needs a key whose flight is hung waits until the flight is
// as old as the message timeout, not a timeout of its own: with several
// messages needing the same key, each leaves at most one timeout after the
// flight's start. The flight goes on, fills the cache when it ends, and a
// getter that waited on it gets its answer.
func TestEventCache_MessagesDoNotWaitOnAFlightOlderThanTheirTimeout(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	var inner, started atomic.Int32
	base := recordingLoader(&inner, false)
	c := NewEventCache[string, string, *localizedFoo](Config{}, func(ctx context.Context, key string, locales []string, existing *localizedFoo, has bool) (*localizedFoo, error) {
		started.Add(1)
		once.Do(func() { close(entered) })
		<-release
		return base(ctx, key, locales, existing, has)
	})
	// The gate stays closed: a hung endpoint does not open it while other
	// endpoints answer.
	gate := newTestGate()

	getterErr := make(chan error, 1)
	getterStart := time.Now()
	go func() {
		_, _, err := c.Get(t.Context(), "m", []string{"en"})
		getterErr <- err
	}()
	<-entered
	flightStart := time.Now()

	// Messages arrive 20 ms apart. The context deadline is far away: only
	// the age of the flight may end their wait.
	const n = 8
	var wg sync.WaitGroup
	late := make(chan time.Duration, n)
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(i) * 20 * time.Millisecond)
			ctx, cancel := context.WithTimeout(apipolicy.WithMessage(t.Context(), gate, messageWait), 10*time.Second)
			defer cancel()
			_, _, err := c.Get(ctx, "m", []string{"en"})
			errs <- err
			late <- time.Since(flightStart)
		}()
	}
	wg.Wait()
	close(errs)
	close(late)
	for err := range errs {
		if !errors.Is(err, ErrFlightTooOld) {
			t.Fatalf("err = %v, want ErrFlightTooOld", err)
		}
	}
	for d := range late {
		if d > messageWait+scheduleSlack {
			t.Fatalf("a message left %v after the flight started, want about %v", d, messageWait)
		}
	}
	// A message arriving once the flight is older than its timeout does not
	// wait at all.
	start := time.Now()
	ctx, cancel := context.WithTimeout(apipolicy.WithMessage(t.Context(), gate, messageWait), 10*time.Second)
	_, _, err := c.Get(ctx, "m", []string{"en"})
	cancel()
	if !errors.Is(err, ErrFlightTooOld) || time.Since(start) > scheduleSlack {
		t.Fatalf("err = %v after %v, want ErrFlightTooOld at once", err, time.Since(start))
	}
	if started.Load() != 1 {
		t.Fatalf("loader runs = %d after the late message, want 1", started.Load())
	}

	// The getter waits for the whole flight, however old it is.
	select {
	case err := <-getterErr:
		t.Fatalf("the getter returned before the flight ended: %v", err)
	default:
	}
	close(release)
	if err := <-getterErr; err != nil {
		t.Fatalf("getter: %v", err)
	}
	if time.Since(getterStart) < messageWait {
		t.Fatal("the getter did not wait for the flight")
	}
	if _, ok := c.Peek("m"); !ok {
		t.Fatal("the flight did not fill the cache")
	}
	if inner.Load() != 1 {
		t.Fatalf("loader calls = %d, want 1", inner.Load())
	}
}

func TestLoadCoalesced_MessagesDoNotWaitOnAFlightOlderThanTheirTimeout(t *testing.T) {
	var sf Flights
	release := make(chan struct{})
	var calls atomic.Int32
	fn := func(context.Context) (int, error) {
		calls.Add(1)
		<-release
		return 7, nil
	}
	gate := newTestGate()

	getter := make(chan int, 1)
	go func() {
		v, _ := LoadCoalesced(t.Context(), t.Context(), &sf, "k", fn)
		getter <- v
	}()
	waitFor(t, "the flight to start", func() bool { return calls.Load() == 1 })

	start := time.Now()
	for range 10 {
		ctx, cancel := context.WithTimeout(apipolicy.WithMessage(t.Context(), gate, messageWait), 10*time.Second)
		_, err := LoadCoalesced(ctx, t.Context(), &sf, "k", fn)
		cancel()
		if !errors.Is(err, ErrFlightTooOld) {
			t.Fatalf("err = %v, want ErrFlightTooOld", err)
		}
	}
	// One timeout for the first message, next to nothing for the others;
	// ten timeouts would be a second.
	if d := time.Since(start); d > 4*messageWait {
		t.Fatalf("ten messages waited %v in total, want about %v", d, messageWait)
	}

	close(release)
	if v := <-getter; v != 7 {
		t.Fatalf("getter = %d, want 7", v)
	}
	if calls.Load() != 1 {
		t.Fatalf("loads = %d, want 1", calls.Load())
	}
}

// A message that joins a young flight waits for its answer.
func TestFlight_AwaitReturnsTheResultOfAYoungFlight(t *testing.T) {
	var sf Flights
	f := sf.Go("k", func() (any, error) {
		time.Sleep(20 * time.Millisecond)
		return 3, nil
	})
	ctx := apipolicy.WithMessage(t.Context(), newTestGate(), time.Second)
	if v, err := f.Await(ctx); err != nil || v != 3 {
		t.Fatalf("v=%v err=%v, want 3", v, err)
	}
}

// When the flight is older than the message timeout and the message's own
// context has ended as well, the context's error is the answer: it is the
// message's own time that ran out, and the two ends can fire together.
func TestFlight_AwaitPrefersTheCallersOwnExpiry(t *testing.T) {
	var sf Flights
	release := make(chan struct{})
	defer close(release)
	f := sf.Go("k", func() (any, error) { <-release; return 1, nil })
	time.Sleep(2 * messageWait)

	ctx, cancel := context.WithTimeout(apipolicy.WithMessage(t.Context(), newTestGate(), messageWait), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	if _, err := f.Await(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
}

// laggingCtx is a context whose deadline has passed but whose error is not
// set yet, as a context is for the instant between its deadline and its timer
// running.
type laggingCtx struct {
	context.Context
	deadline time.Time
}

func (c laggingCtx) Deadline() (time.Time, bool) { return c.deadline, true }

func TestFlight_AwaitCountsAPassedDeadlineAsTheCallersOwnExpiry(t *testing.T) {
	var sf Flights
	release := make(chan struct{})
	defer close(release)
	f := sf.Go("k", func() (any, error) { <-release; return 1, nil })
	time.Sleep(2 * messageWait)

	base := apipolicy.WithMessage(t.Context(), newTestGate(), messageWait)
	ctx := laggingCtx{Context: base, deadline: time.Now().Add(-time.Millisecond)}
	if _, err := f.Await(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
}

func TestFlights_ForgetDetachesARunningFlight(t *testing.T) {
	var sf Flights
	release := make(chan struct{})
	first := sf.Go("k", func() (any, error) { <-release; return 1, nil })
	sf.Forget("k")
	second := sf.Go("k", func() (any, error) { return 2, nil })
	if first == second {
		t.Fatal("a call after Forget joined the forgotten flight")
	}
	if v, _ := second.Await(t.Context()); v != 2 {
		t.Fatalf("second = %v, want 2", v)
	}
	close(release)
	if v, _ := first.Await(t.Context()); v != 1 {
		t.Fatalf("the forgotten flight lost its result: %v", v)
	}
	// The ended flight of the forgotten key must not remove the new one's slot.
	third := sf.Go("k", func() (any, error) { return 3, nil })
	if v, _ := third.Await(t.Context()); v != 3 {
		t.Fatalf("third = %v, want 3", v)
	}
}

func TestFlights_SharedResultReachesEveryWaiter(t *testing.T) {
	var sf Flights
	release := make(chan struct{})
	var runs atomic.Int32
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := sf.Do("k", func() (any, error) { runs.Add(1); <-release; return 9, nil })
			if v != 9 || err != nil {
				t.Errorf("v=%v err=%v", v, err)
			}
		}()
	}
	waitFor(t, "one flight", func() bool { return runs.Load() == 1 })
	time.Sleep(10 * time.Millisecond)
	close(release)
	wg.Wait()
	if runs.Load() != 1 {
		t.Fatalf("runs = %d, want 1", runs.Load())
	}
}
