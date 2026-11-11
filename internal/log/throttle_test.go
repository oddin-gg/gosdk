package log

import (
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestThrottle_FirstThroughThenOnePerInterval(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	th := NewThrottle(time.Minute, clk.Now)

	if n, ok := th.Allow(); !ok || n != 0 {
		t.Fatalf("first Allow = (%d, %v), want (0, true)", n, ok)
	}
	for range 5 {
		clk.Advance(time.Second)
		if _, ok := th.Allow(); ok {
			t.Fatal("Allow inside the interval returned true")
		}
	}
	clk.Advance(time.Minute)
	if n, ok := th.Allow(); !ok || n != 5 {
		t.Fatalf("Allow after the interval = (%d, %v), want (5, true)", n, ok)
	}
	// The count restarts after a line went out.
	clk.Advance(time.Second)
	if _, ok := th.Allow(); ok {
		t.Fatal("Allow right after an emitted line returned true")
	}
	clk.Advance(time.Minute)
	if n, ok := th.Allow(); !ok || n != 1 {
		t.Fatalf("Allow = (%d, %v), want (1, true)", n, ok)
	}
}

func TestThrottle_ConcurrentCountsEveryCall(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	th := NewThrottle(time.Minute, clk.Now)

	const callers = 64
	var wg sync.WaitGroup
	var mu sync.Mutex
	emitted := 0
	for range callers {
		wg.Go(func() {
			if _, ok := th.Allow(); ok {
				mu.Lock()
				emitted++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if emitted != 1 {
		t.Fatalf("emitted = %d, want exactly 1 inside one interval", emitted)
	}
	clk.Advance(time.Minute)
	if n, ok := th.Allow(); !ok || n != callers-1 {
		t.Fatalf("Allow = (%d, %v), want (%d, true)", n, ok, callers-1)
	}
}
