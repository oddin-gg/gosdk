package api

import (
	"strings"
	"sync"
	"time"

	"github.com/oddin-gg/gosdk/internal/apipolicy"
)

// breakerOpenFor is how long a breaker stays open after a failure. It matches
// the per-key failure memory of the loaders: long enough for a failing
// endpoint to be left alone by builds that would only queue behind it, short
// enough that the first build after it is the probe of whether it is back.
const breakerOpenFor = 10 * time.Second

// Breaker remembers, per endpoint, that the REST API gave no answer moments
// ago: a transport error, a request that timed out, a rate limit, a body
// that the transport cut off. A response that arrived complete is an answer
// about ONE resource, whatever it says (a 500, 502, 503 or 504, a
// service-unavailable envelope, a body that cannot be decoded, a document
// about another resource, a 4xx): it never opens the breaker, since it says
// nothing about what other resources of the endpoint will do. The loaders
// remember such an answer for its key themselves. The message path also trips
// an endpoint when a message waited its whole time on it.
//
// The endpoints are the resource kinds of apipolicy.Endpoint. A healthy
// answer closes only its own endpoint's breaker, so one endpoint that hangs
// while the others answer stays open. It is a plain time-based breaker
// without a half-open state: when the time ends, the next caller that needs
// the endpoint simply tries, and that try either closes the breaker or opens
// it again. A half-open state would make every other caller give up during
// the probe for little gain.
//
// Only callers that choose to read it are affected; the client itself never
// refuses a request because of it.
type Breaker struct {
	eps [apipolicy.EndpointCount]endpointBreaker
}

// endpointBreaker is the breaker of one endpoint.
type endpointBreaker struct {
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
	b := &Breaker{}
	for i := range b.eps {
		b.eps[i] = endpointBreaker{now: now, openFor: openFor, opened: make(chan struct{})}
	}
	return b
}

func (b *Breaker) of(ep apipolicy.Endpoint) *endpointBreaker {
	if ep >= apipolicy.EndpointCount {
		ep = apipolicy.EndpointOther
	}
	return &b.eps[ep]
}

// expireLocked closes the breaker once its time has passed. Every reader
// calls it under mu first, so the channel is replaced before a new waiter
// can take it: otherwise a waiter would get a channel that is already closed
// and give up at once while the breaker is closed.
func (e *endpointBreaker) expireLocked() {
	if !e.openUntil.IsZero() && !e.now().Before(e.openUntil) {
		e.closeLocked()
	}
}

func (e *endpointBreaker) closeLocked() {
	if e.openUntil.IsZero() {
		return
	}
	e.openUntil = time.Time{}
	e.opened = make(chan struct{})
}

// SetOpenFor changes how long a breaker stays open after a failure. For
// tests that need a short time; production uses the default.
func (b *Breaker) SetOpenFor(d time.Duration) {
	for i := range b.eps {
		e := &b.eps[i]
		e.mu.Lock()
		e.openFor = d
		e.mu.Unlock()
	}
}

// Open reports whether the breaker of the endpoint is open.
func (b *Breaker) Open(ep apipolicy.Endpoint) bool {
	e := b.of(ep)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.expireLocked()
	return !e.openUntil.IsZero()
}

// Opened returns a channel that is closed once the endpoint's breaker is open
// (at once if it already is). A breaker that closes and opens again hands out
// a new channel for the next opening, so call it again after each wake-up.
func (b *Breaker) Opened(ep apipolicy.Endpoint) <-chan struct{} {
	e := b.of(ep)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.expireLocked()
	return e.opened
}

// Trip opens the endpoint's breaker, or keeps it open for another full time.
// The message path uses it when a message has waited its whole time on the
// endpoint: the endpoint is then unavailable for it, whatever the reason.
func (b *Breaker) Trip(ep apipolicy.Endpoint) {
	e := b.of(ep)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.expireLocked()
	if e.openUntil.IsZero() {
		close(e.opened)
	}
	e.openUntil = e.now().Add(e.openFor)
}

// failure records a failed attempt that says the endpoint is unavailable.
func (b *Breaker) failure(ep apipolicy.Endpoint) { b.Trip(ep) }

// success records a healthy answer: it closes the endpoint's breaker.
func (b *Breaker) success(ep apipolicy.Endpoint) {
	e := b.of(ep)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closeLocked()
}

// endpointOf maps a request path to the endpoint it belongs to, by the same
// kinds the cache loaders name when they gate a load.
func endpointOf(path string) apipolicy.Endpoint {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	p := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case len(p) == 5 && p[0] == "sports" && p[2] == "sport_events" && p[4] == "summary":
		return apipolicy.EndpointSummary
	case len(p) == 5 && p[0] == "sports" && p[2] == "sport_events" && p[4] == "fixture":
		return apipolicy.EndpointFixture
	case len(p) == 5 && p[0] == "sports" && p[2] == "competitors" && p[4] == "profile":
		return apipolicy.EndpointCompetitor
	case len(p) == 5 && p[0] == "sports" && p[2] == "players" && p[4] == "profile":
		return apipolicy.EndpointPlayer
	case len(p) == 5 && p[0] == "sports" && p[2] == "tournaments" && p[4] == "info":
		return apipolicy.EndpointTournament
	case len(p) == 3 && p[0] == "descriptions" && p[2] == "markets":
		return apipolicy.EndpointCatalog
	case len(p) == 6 && p[0] == "descriptions" && p[2] == "markets" && p[4] == "variants":
		return apipolicy.EndpointVariant
	case len(p) == 3 && p[0] == "descriptions" && p[2] == "match_status",
		len(p) == 2 && p[0] == "descriptions" && p[1] == "void_reasons",
		len(p) == 3 && p[0] == "sports" && p[2] == "sports",
		len(p) == 5 && p[0] == "sports" && p[2] == "sports" && p[4] == "tournaments":
		return apipolicy.EndpointStatic
	}
	return apipolicy.EndpointOther
}
