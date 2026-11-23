package recovery

import (
	"sync"
	"testing"
	"time"
)

// fakeClock moves its two readings independently, the way a stepped wall
// clock does: the wall side jumps, the monotonic side keeps counting. They
// start at unrelated numbers, so nothing can pass by treating them as one.
type fakeClock struct {
	mu   sync.Mutex
	wall time.Time
	mono time.Duration
}

func newFakeClock() *fakeClock {
	return &fakeClock{
		wall: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
		mono: 7*time.Hour + 123*time.Millisecond,
	}
}

func (c *fakeClock) Now() Stamp {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Stamp{wall: c.wall, mono: c.mono}
}

// advance moves both readings by d, as time passing does.
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.wall = c.wall.Add(d)
	c.mono += d
}

// stepWall moves the wall reading alone, forward or back.
func (c *fakeClock) stepWall(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.wall = c.wall.Add(d)
}

// advanceMono moves the monotonic reading alone.
func (c *fakeClock) advanceMono(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mono += d
}

var _ Clock = (*fakeClock)(nil)
var _ Clock = (*fakeClock)(nil)
var _ Clock = systemClock{}

func TestFakeClock_ReadingsMoveIndependently(t *testing.T) {
	c := newFakeClock()
	start := c.Now()

	c.stepWall(30 * time.Second)
	got := c.Now()
	if got.mono != start.mono || !got.wall.Equal(start.wall.Add(30*time.Second)) {
		t.Fatalf("a wall step moved the monotonic side: %+v -> %+v", start, got)
	}

	c.advanceMono(5 * time.Second)
	after := c.Now()
	if after.mono-got.mono != 5*time.Second || !after.wall.Equal(got.wall) {
		t.Fatalf("a monotonic move moved the wall side: %+v -> %+v", got, after)
	}

	c.advance(time.Second)
	last := c.Now()
	if last.mono-after.mono != time.Second || last.wall.Sub(after.wall) != time.Second {
		t.Fatalf("advance must move both: %+v -> %+v", after, last)
	}
}

func TestSystemClock_MonotonicSideCountsFromProcessStartAndNeverGoesBack(t *testing.T) {
	c := SystemClock()
	a := c.Now()
	time.Sleep(2 * time.Millisecond)
	b := c.Now()
	if a.Mono() < 0 {
		t.Fatalf("mono before the process start: %v", a.Mono())
	}
	if b.Mono() <= a.Mono() {
		t.Fatalf("mono did not advance: %v then %v", a.Mono(), b.Mono())
	}
	if b.Wall().Before(a.Wall()) {
		t.Fatalf("wall went back without a step: %v then %v", a.Wall(), b.Wall())
	}
	// Both readings come from one call, so the wall reading is the
	// process start plus the monotonic one.
	if d := b.Wall().Sub(processStart) - b.Mono(); d != 0 {
		t.Fatalf("readings are not of one moment: wall-processStart-mono = %v", d)
	}
}

// measured at the feed, producer's clock 3 s behind the SDK's.
func measuredOn(c *fakeClock, behind time.Duration) clockOffset {
	r := c.Now()
	return measureOffset(r.wall.Add(-behind), r)
}

func TestClockOffset_Measured(t *testing.T) {
	c := newFakeClock()
	r := c.Now()
	o := measureOffset(r.wall.Add(-3*time.Second), r)
	if o.offset != 3*time.Second {
		t.Fatalf("offset = %v, want 3s", o.offset)
	}
	if want := time.Duration(r.wall.UnixNano()) - r.mono; o.skew != want {
		t.Fatalf("skew = %v, want %v", o.skew, want)
	}
	// A producer ahead of the SDK has a negative offset.
	ahead := measureOffset(r.wall.Add(2*time.Second), r)
	if ahead.offset != -2*time.Second {
		t.Fatalf("offset of a producer ahead = %v, want -2s", ahead.offset)
	}
}

// Time passing on both clocks is not a step, whatever the offset is.
func TestClockOffset_BothClocksAdvancingLeavesTheOffset(t *testing.T) {
	c := newFakeClock()
	o := measuredOn(c, 3*time.Second)
	c.advance(10 * time.Minute)
	if got := o.at(c.Now()); got != 3*time.Second {
		t.Fatalf("offset after time passed = %v, want 3s", got)
	}
}

// A step of the wall clock after the offset was measured moves the offset by
// the step: the producer's clock did not move with it, so its now is the
// SDK's now less the step.
func TestClockOffset_StepOfTheWallClockIsCorrectedOut(t *testing.T) {
	for _, step := range []time.Duration{30 * time.Second, -30 * time.Second, 10 * time.Minute, -10 * time.Minute} {
		c := newFakeClock()
		o := measuredOn(c, 3*time.Second)
		before := c.Now()
		producerNowBefore := o.producerNow(before)

		c.stepWall(step)
		after := c.Now()

		if got, want := o.at(after), 3*time.Second+step; got != want {
			t.Errorf("step %v: offset = %v, want %v", step, got, want)
		}
		if got := o.producerNow(after); !got.Equal(producerNowBefore) {
			t.Errorf("step %v: producer now = %v, want it unmoved at %v", step, got, producerNowBefore)
		}
	}
}

// The correction goes on after time has passed on both clocks as well.
func TestClockOffset_StepThenTimePassing(t *testing.T) {
	c := newFakeClock()
	o := measuredOn(c, 3*time.Second)
	c.stepWall(-30 * time.Second)
	c.advance(2 * time.Minute)
	if got, want := o.at(c.Now()), 3*time.Second-30*time.Second; got != want {
		t.Fatalf("offset = %v, want %v", got, want)
	}
}

// The limit: up to a second is the jitter of reading two clocks, or a slew,
// and is no step; over it is.
func TestClockOffset_AMovementUnderASecondIsNotAStepAndOneOverItIs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		move    time.Duration
		stepped bool
	}{
		{"900ms forward", 900 * time.Millisecond, false},
		{"900ms back", -900 * time.Millisecond, false},
		{"exactly the limit", time.Second, false},
		{"1100ms forward", 1100 * time.Millisecond, true},
		{"1100ms back", -1100 * time.Millisecond, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newFakeClock()
			o := measuredOn(c, 3*time.Second)
			c.stepWall(tc.move)
			want := 3 * time.Second
			if tc.stepped {
				want += tc.move
			}
			if got := o.at(c.Now()); got != want {
				t.Fatalf("offset = %v, want %v", got, want)
			}
		})
	}
}

// The monotonic clock moving with the wall one standing still is the wall
// clock stepping back: a deadline on the monotonic side fires, and the
// producer's now does not follow the SDK's wall clock backwards.
func TestClockOffset_MonotonicMovingAloneReadsAsAWallStepBack(t *testing.T) {
	c := newFakeClock()
	o := measuredOn(c, 3*time.Second)
	c.advanceMono(5 * time.Second)
	if got, want := o.at(c.Now()), 3*time.Second-5*time.Second; got != want {
		t.Fatalf("offset = %v, want %v", got, want)
	}
}

// The correction is a function of one stamp's two readings and the stamp the
// offset was measured on: the order readings are taken in does not matter.
func TestClockOffset_AtIsStateless(t *testing.T) {
	c := newFakeClock()
	o := measuredOn(c, 3*time.Second)
	c.stepWall(30 * time.Second)
	stepped := c.Now()
	c.stepWall(-30 * time.Second)
	back := c.Now()

	if got := o.at(stepped); got != 33*time.Second {
		t.Fatalf("stepped reading: offset = %v, want 33s", got)
	}
	if got := o.at(back); got != 3*time.Second {
		t.Fatalf("a step undone: offset = %v, want 3s", got)
	}
	if got := o.at(stepped); got != 33*time.Second {
		t.Fatalf("reading again gave %v, want 33s", got)
	}
}
