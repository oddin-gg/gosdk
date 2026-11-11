package api

import (
	"sync"
	"time"
)

// breakerOpenFor is how long the breaker stays open after a failure. It
// matches the failure memory it replaces: long enough for a failing API to
// be left alone by builds that would only queue behind it, short enough that
// the first build after it is the probe of whether the API is back.
const breakerOpenFor = 10 * time.Second

// Breaker remembers, for the whole client, that the REST API failed moments
// ago. It is fed by every answer (and failure) of every call, getters
// included, so one healthy answer from any caller closes it. It is a plain
// time-based breaker without a half-open state: when the time ends, the next
// caller that needs the API simply tries, and that try either closes the
// breaker or opens it again. A half-open state would make every other caller
// give up during the probe for little gain.
//
// Only callers that choose to read it are affected; the client itself never
// refuses a request because of it.
type Breaker struct {
	mu      sync.Mutex
	now     func() time.Time
	openFor time.Duration
	// openUntil is the end of the open time; the zero value means closed.
	openUntil time.Time
	// opened is closed when the breaker opens and replaced when it closes,
	// so a waiter that took it while the breaker was closed is woken by the
	// next opening, and one that took it while open sees it closed at once.
	opened chan struct{}
}

func newBreaker(now func() time.Time, openFor time.Duration) *Breaker {
	return &Breaker{now: now, openFor: openFor, opened: make(chan struct{})}
}

// expireLocked closes the breaker once its time has passed. Every reader
// calls it under mu first, so the channel is replaced before a new waiter
// can take it: otherwise a waiter would get a channel that is already closed
// and give up at once while the breaker is closed.
func (b *Breaker) expireLocked() {
	if !b.openUntil.IsZero() && !b.now().Before(b.openUntil) {
		b.closeLocked()
	}
}

func (b *Breaker) closeLocked() {
	if b.openUntil.IsZero() {
		return
	}
	b.openUntil = time.Time{}
	b.opened = make(chan struct{})
}

// SetOpenFor changes how long the breaker stays open after a failure. For
// tests that need a short time; production uses the default.
func (b *Breaker) SetOpenFor(d time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.openFor = d
}

// Open reports whether the breaker is open.
func (b *Breaker) Open() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.expireLocked()
	return !b.openUntil.IsZero()
}

// Opened returns a channel that is closed once the breaker is open (at once
// if it already is). A breaker that closes and opens again hands out a new
// channel for the next opening, so call it again after each wake-up.
func (b *Breaker) Opened() <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.expireLocked()
	return b.opened
}

// Trip opens the breaker, or keeps it open for another full time. The
// message path uses it when a message has waited its whole time for the API:
// the API is then unavailable for it, whatever the reason.
func (b *Breaker) Trip() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.expireLocked()
	if b.openUntil.IsZero() {
		close(b.opened)
	}
	b.openUntil = b.now().Add(b.openFor)
}

// failure records a failed attempt that says the API is unavailable.
func (b *Breaker) failure() { b.Trip() }

// success records a healthy answer: it closes the breaker.
func (b *Breaker) success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closeLocked()
}
