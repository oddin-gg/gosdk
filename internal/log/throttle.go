package log

import (
	"sync"
	"time"
)

// Throttle rate-limits a repeating log line: the first occurrence goes
// out at once, later ones are counted and suppressed until the interval
// has passed since the last line that went out; that next line reports
// how many were suppressed in between. The zero value is not usable;
// build one with NewThrottle. Safe for concurrent use.
type Throttle struct {
	interval time.Duration
	now      func() time.Time

	mu         sync.Mutex
	lastAt     time.Time
	emitted    bool
	suppressed int
}

// NewThrottle returns a Throttle that lets one line through per
// interval. A nil now uses time.Now.
func NewThrottle(interval time.Duration, now func() time.Time) *Throttle {
	if now == nil {
		now = time.Now
	}
	return &Throttle{interval: interval, now: now}
}

// Allow reports whether the caller should log now. When it does,
// suppressed is the number of calls that were swallowed since the
// previous line (0 for the first line). A nil Throttle never suppresses.
func (t *Throttle) Allow() (suppressed int, ok bool) {
	if t == nil {
		return 0, true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if t.emitted && now.Sub(t.lastAt) < t.interval {
		t.suppressed++
		return 0, false
	}
	suppressed = t.suppressed
	t.suppressed = 0
	t.lastAt = now
	t.emitted = true
	return suppressed, true
}
