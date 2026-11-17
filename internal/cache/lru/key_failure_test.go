package lru

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/internal/apipolicy"
)

// answeredErr is an error that says the API answered for the key and failed,
// as the API client's errors do (see apipolicy.KeyFailure).
type answeredErr struct{}

func (answeredErr) Error() string    { return "answered: 500" }
func (answeredErr) KeyFailure() bool { return true }

func failingLoad(err error) func(context.Context) (int, error) {
	return func(context.Context) (int, error) { return 0, err }
}

// A key whose flight failed with an answer of the API is remembered: a
// message-path caller for it is skipped without starting or joining a flight.
// A getter is not, and a success forgets the key.
func TestFailureMemory_AMessageIsSkippedForAKeyThatFailedMomentsAgo(t *testing.T) {
	var sf Flights
	gate := newTestGate()
	marked := apipolicy.WithMessage(t.Context(), gate, apipolicy.MessageTimeout)
	flights, _ := countFlights(t)

	_, err := LoadCoalesced(marked, t.Context(), &sf, apipolicy.EndpointSummary, "k", failingLoad(fmt.Errorf("wrapped: %w", answeredErr{})))
	if !errors.Is(err, answeredErr{}) {
		t.Fatalf("first load: err = %v, want the answer's error", err)
	}
	// The flight ends after its waiter was handed the result; wait until the
	// group has noted the outcome.
	waitFor(t, "the failure to be remembered", func() bool { return sf.Remembered("k") })

	before := flights.Load()
	var loads int
	_, err = LoadCoalesced(marked, t.Context(), &sf, apipolicy.EndpointSummary, "k", func(context.Context) (int, error) {
		loads++
		return 1, nil
	})
	if !errors.Is(err, apipolicy.ErrSkipped) {
		t.Fatalf("err = %v, want ErrSkipped for a key that failed moments ago", err)
	}
	if flights.Load() != before || loads != 0 {
		t.Fatalf("the skipped caller started or joined a flight (registrations +%d, loads %d)", flights.Load()-before, loads)
	}

	// Another key is not affected.
	if v, err := LoadCoalesced(marked, t.Context(), &sf, apipolicy.EndpointSummary, "other", func(context.Context) (int, error) { return 2, nil }); err != nil || v != 2 {
		t.Fatalf("other key: %v, %v", v, err)
	}

	// A getter ignores the memory, and its success forgets the key.
	if v, err := LoadCoalesced(t.Context(), t.Context(), &sf, apipolicy.EndpointSummary, "k", func(context.Context) (int, error) { return 3, nil }); err != nil || v != 3 {
		t.Fatalf("getter: %v, %v", v, err)
	}
	waitFor(t, "the success to forget the key", func() bool { return !sf.Remembered("k") })
	if v, err := LoadCoalesced(marked, t.Context(), &sf, apipolicy.EndpointSummary, "k", func(context.Context) (int, error) { return 4, nil }); err != nil || v != 4 {
		t.Fatalf("after the success: %v, %v", v, err)
	}
}

// The memory lasts for its time only.
func TestFailureMemory_Expires(t *testing.T) {
	defer SetFailureMemory(80 * time.Millisecond)()
	var sf Flights
	marked := apipolicy.WithMessage(t.Context(), newTestGate(), apipolicy.MessageTimeout)
	_, _ = LoadCoalesced(marked, t.Context(), &sf, apipolicy.EndpointSummary, "k", failingLoad(answeredErr{}))
	waitFor(t, "the failure to be remembered", func() bool { return sf.Remembered("k") })
	waitFor(t, "the memory to expire", func() bool { return !sf.Remembered("k") })
}

// Only an answer of the API for the key is remembered: a cancellation, a
// timeout or any other error says nothing about the key.
func TestFailureMemory_OnlyAnswersOfTheAPIAreRemembered(t *testing.T) {
	var sf Flights
	marked := apipolicy.WithMessage(t.Context(), newTestGate(), apipolicy.MessageTimeout)
	for name, err := range map[string]error{
		"plain":    errors.New("connection refused"),
		"canceled": context.Canceled,
		"deadline": context.DeadlineExceeded,
	} {
		_, got := LoadCoalesced(marked, t.Context(), &sf, apipolicy.EndpointSummary, name, failingLoad(err))
		if got == nil {
			t.Fatalf("%s: no error", name)
		}
		// Let the flight finish; a getter's follow-up proves it is gone.
		if _, err := LoadCoalesced(t.Context(), t.Context(), &sf, apipolicy.EndpointSummary, name, func(context.Context) (int, error) { return 1, nil }); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if sf.Remembered(name) {
			t.Fatalf("%s: remembered", name)
		}
	}
}

// The memory is bounded.
func TestFailureMemory_IsBounded(t *testing.T) {
	var sf Flights
	for i := range maxRememberedFailures + 500 {
		sf.mu.Lock()
		sf.noteOutcomeLocked(fmt.Sprintf("k%d", i), answeredErr{})
		sf.mu.Unlock()
	}
	sf.mu.Lock()
	n := len(sf.failed)
	sf.mu.Unlock()
	if n > maxRememberedFailures {
		t.Fatalf("remembered %d keys, bound is %d", n, maxRememberedFailures)
	}
	if !sf.Remembered(fmt.Sprintf("k%d", maxRememberedFailures+499)) {
		t.Fatal("the newest failure was dropped")
	}
}

// A message waiter is released at the first failed attempt of the flight's
// request, not at the end of its retries; the flight goes on and its result
// still reaches a waiter that is not a message.
func TestFlight_AMessageIsReleasedAtTheFirstFailedAttemptAndTheFlightGoesOn(t *testing.T) {
	var sf Flights
	marked := apipolicy.WithMessage(t.Context(), newTestGate(), time.Minute)
	attempted := make(chan struct{})
	release := make(chan struct{})
	load := func(ctx context.Context) (int, error) {
		apipolicy.AttemptFailed(ctx) // the first attempt failed, a retry follows
		close(attempted)
		<-release
		return 9, nil
	}

	var wg sync.WaitGroup
	var getterVal int
	var getterErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		getterVal, getterErr = LoadCoalesced(t.Context(), t.Context(), &sf, apipolicy.EndpointSummary, "k", load)
	}()
	<-attempted

	start := time.Now()
	_, err := LoadCoalesced(marked, t.Context(), &sf, apipolicy.EndpointSummary, "k", load)
	if !errors.Is(err, apipolicy.ErrSkipped) {
		t.Fatalf("message err = %v, want ErrSkipped at the first failed attempt", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("the message waited %v", time.Since(start))
	}
	// The flight is still running; the getter still gets its result.
	close(release)
	wg.Wait()
	if getterErr != nil || getterVal != 9 {
		t.Fatalf("getter = %v, %v, want 9", getterVal, getterErr)
	}
}

// A message whose time runs out while it waits on a flight records the
// endpoint of that flight, and no other.
func TestFlight_ATimedOutWaitNamesItsEndpoint(t *testing.T) {
	var sf Flights
	release := make(chan struct{})
	defer close(release)
	go func() {
		_, _ = LoadCoalesced(t.Context(), t.Context(), &sf, apipolicy.EndpointPlayer, "p", func(context.Context) (int, error) {
			<-release
			return 1, nil
		})
	}()
	waitFor(t, "the flight to start", func() bool {
		sf.mu.Lock()
		defer sf.mu.Unlock()
		return len(sf.m) == 1
	})

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	marked := apipolicy.WithMessage(ctx, newTestGate(), time.Minute)
	_, err := LoadCoalesced(marked, t.Context(), &sf, apipolicy.EndpointPlayer, "p", failingLoad(nil))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the message's deadline", err)
	}
	got := apipolicy.TimedOut(marked)
	if len(got) != 1 || got[0] != apipolicy.EndpointPlayer {
		t.Fatalf("timed out endpoints = %v, want only the player profile", got)
	}

	// A wait that ended some other way records nothing.
	other := apipolicy.WithMessage(t.Context(), newTestGate(), time.Minute)
	if _, err := LoadCoalesced(other, t.Context(), &sf, apipolicy.EndpointSummary, "s", func(context.Context) (int, error) { return 1, nil }); err != nil {
		t.Fatal(err)
	}
	if got := apipolicy.TimedOut(other); len(got) != 0 {
		t.Fatalf("a healthy load recorded %v", got)
	}
}

// The gate of one endpoint does not skip a load of another.
func TestSkippedByGate_IsPerEndpoint(t *testing.T) {
	var sf Flights
	g := &endpointGate{open: map[apipolicy.Endpoint]bool{apipolicy.EndpointPlayer: true}}
	marked := apipolicy.WithMessage(t.Context(), g, time.Minute)
	if err := SkippedByGate(marked, &sf, "k", apipolicy.EndpointPlayer); !errors.Is(err, apipolicy.ErrSkipped) {
		t.Fatalf("player: %v, want ErrSkipped", err)
	}
	if err := SkippedByGate(marked, &sf, "k", apipolicy.EndpointSummary); err != nil {
		t.Fatalf("summary: %v, want no skip while only the player profile is open", err)
	}
}

type endpointGate struct{ open map[apipolicy.Endpoint]bool }

func (g *endpointGate) Open(ep apipolicy.Endpoint) bool           { return g.open[ep] }
func (g *endpointGate) Opened(apipolicy.Endpoint) <-chan struct{} { return nil }

// The check of the memory and the start of a flight are one step: a caller
// that looked at the memory before the flight of its key ended must not start
// the next flight after it. GoFor is that step; without the mark it starts a
// flight as GoWith does.
func TestFlights_GoForRefusesAMarkedCallerForARememberedKeyAtTheStart(t *testing.T) {
	var sf Flights
	marked := apipolicy.WithMessage(t.Context(), newTestGate(), apipolicy.MessageTimeout)
	f := sf.GoWith("k", func(*Flight) (any, error) { return nil, answeredErr{} })
	<-f.Done()
	waitFor(t, "the failure to be remembered", func() bool { return sf.Remembered("k") })

	started := make(chan struct{}, 2)
	start := func(*Flight) (any, error) { started <- struct{}{}; return 1, nil }
	if f, err := sf.GoFor(marked, "k", start); !errors.Is(err, apipolicy.ErrSkipped) || f != nil {
		t.Fatalf("GoFor = %v, %v, want ErrSkipped and no flight", f, err)
	}
	select {
	case <-started:
		t.Fatal("a flight was started for a remembered key")
	default:
	}
	f, err := sf.GoFor(t.Context(), "k", start)
	if err != nil {
		t.Fatal(err)
	}
	<-f.Done()
	if v, _ := f.Result(); v != 1 {
		t.Fatalf("getter flight result = %v", v)
	}
}
