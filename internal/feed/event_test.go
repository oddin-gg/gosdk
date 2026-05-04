package feed

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

// TestEvent_Emitter_NilSafe verifies emit is a no-op when no emitter is
// installed.
func TestEvent_Emitter_NilSafe(t *testing.T) {
	c := &Client{}
	// Should not panic.
	c.emit(EventConnected, nil)
	c.emit(EventDisconnected, errors.New("boom"))
	c.emit(EventReconnecting, nil)
}

// TestEvent_Emitter_DispatchesAllKinds verifies SetEventEmitter installs
// the callback and emit fires it with the right payload.
func TestEvent_Emitter_DispatchesAllKinds(t *testing.T) {
	c := &Client{}
	var seen []Event
	var mu sync.Mutex
	c.SetEventEmitter(func(ev Event) {
		mu.Lock()
		seen = append(seen, ev)
		mu.Unlock()
	})

	bang := errors.New("boom")
	c.emit(EventConnected, nil)
	c.emit(EventDisconnected, bang)
	c.emit(EventReconnecting, nil)

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 3 {
		t.Fatalf("got %d events, want 3", len(seen))
	}
	if seen[0].Kind != EventConnected || seen[0].Err != nil {
		t.Errorf("seen[0] = %+v", seen[0])
	}
	if seen[1].Kind != EventDisconnected || seen[1].Err != bang {
		t.Errorf("seen[1] = %+v", seen[1])
	}
	if seen[2].Kind != EventReconnecting || seen[2].Err != nil {
		t.Errorf("seen[2] = %+v", seen[2])
	}
}

// TestEvent_Emitter_ConcurrentSetAndEmit confirms swapping the emitter
// while emit fires from another goroutine is race-safe.
func TestEvent_Emitter_ConcurrentSetAndEmit(t *testing.T) {
	c := &Client{}

	var calls atomic.Int64
	emitter := func(Event) { calls.Add(1) }

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				c.SetEventEmitter(emitter)
				c.SetEventEmitter(nil)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				c.emit(EventConnected, nil)
			}
		}
	}()

	// Let the race run briefly.
	for i := 0; i < 1000; i++ {
		c.emit(EventDisconnected, nil)
	}
	close(stop)
	wg.Wait()

	// Just want to confirm we got SOME calls and the race detector didn't trip.
	if calls.Load() == 0 {
		t.Fatal("expected at least one emitter call")
	}
}

// TestEvent_Emitter_NilArgIsNoOp verifies SetEventEmitter(nil) clears
// the callback and subsequent emit calls are no-ops.
func TestEvent_Emitter_NilArgIsNoOp(t *testing.T) {
	c := &Client{}
	var fired atomic.Bool
	c.SetEventEmitter(func(Event) { fired.Store(true) })
	c.SetEventEmitter(nil)
	c.emit(EventConnected, nil)
	if fired.Load() {
		t.Fatal("emitter fired after SetEventEmitter(nil)")
	}
}
