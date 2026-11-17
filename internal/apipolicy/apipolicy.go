// Package apipolicy marks the context of the feed message path, so the layers
// between the message factory and the HTTP client (entity caches, coalescing
// loaders) know that a message waits for the load and may be turned away,
// without a parameter for it.
//
// Two callers ask the API for the same entity: a public getter
// (Client.Match, ...), which waits for the answer and retries transient
// failures, and the message path, which builds the event of every incoming
// feed message on the session goroutine. A getter that waits for an
// unavailable API only delays that caller; the message path doing the same
// delays every message behind it. So the message path waits under one
// timeout (MessageTimeout) that covers the whole build, and under the
// breaker of the API client for the endpoint it is about to ask: while that
// breaker is open it asks the caches only for what the endpoint serves.
//
// The mark does not change the request. Every load, whoever asked for it,
// runs as a flight with the getters' retries and timeouts; the flight
// carries no mark and goes on when a message stops waiting for it, and what
// it loads is there for later messages and getters. The mark only decides
// what the caller does before and while it waits:
//
//   - A coalescing loader called with the mark neither starts nor joins a
//     flight while the gate of the loader's endpoint is open, or while the
//     key failed moments ago: it returns ErrSkipped.
//   - It stops waiting on a flight, with ErrSkipped, once that gate opens or
//     once the first attempt of the flight's request failed (the flight goes
//     on retrying for whoever else needs it).
//   - It stops waiting on a flight that has run longer than the message's
//     timeout, however late the message joined it (see TimeoutOf).
//   - A context without the mark (every getter, every flight, preloads and
//     ticker refreshes) ignores the gate.
//
// A loader that waits on a flight should use lru.LoadCoalesced or
// lru.EventCache, which do this, rather than wait on lru.Flights directly, and
// a flight must not run under the caller's context but under its owner's
// lifetime, so that the mark does not reach the requests it makes.
package apipolicy

import (
	"context"
	"errors"
	"sync/atomic"
	"time"
)

// MessageTimeout is how long one message-path build waits for everything it
// needs (the match summary, the tournament, the competitors, the fixture,
// further locales, the market and player names) before it goes on with what
// it has.
const MessageTimeout = 2 * time.Second

// ErrSkipped is returned for a load that was skipped because the REST API
// failed moments ago, for the endpoint or for the key (see Gate).
var ErrSkipped = errors.New("api: skipped, the REST API failed moments ago")

// Endpoint is one kind of REST resource the message path loads. The API
// client keeps one breaker per endpoint: an endpoint that hangs or is
// unavailable does not hold back loads from the others, and a healthy answer
// of one endpoint does not close the breaker of another.
type Endpoint uint8

// The endpoints of the loaders. EndpointOther is everything no message-path
// loader asks for (producers, schedules, recovery, replay).
const (
	EndpointOther Endpoint = iota
	EndpointSummary
	EndpointFixture
	EndpointCompetitor
	EndpointPlayer
	EndpointTournament
	EndpointCatalog
	EndpointVariant
	EndpointStatic
	// EndpointCount is the number of endpoints; it is not an endpoint.
	EndpointCount
)

var endpointNames = [EndpointCount]string{
	"other", "match summary", "fixture", "competitor profile", "player profile",
	"tournament info", "market descriptions", "variant descriptions", "static descriptions",
}

func (e Endpoint) String() string {
	if e < EndpointCount {
		return endpointNames[e]
	}
	return "unknown"
}

// Gate tells the loaders whether an endpoint of the REST API failed moments
// ago. The API client's breaker implements it; the interface keeps the cache
// packages from importing the client.
type Gate interface {
	// Open reports whether callers that honour the gate must not load from
	// the endpoint.
	Open(Endpoint) bool
	// Opened returns a channel that is closed once the gate of the endpoint
	// is open (at once if it already is). Take a new one after each wake-up.
	Opened(Endpoint) <-chan struct{}
}

type markKey struct{}

type mark struct {
	gate    Gate
	timeout time.Duration
	// timedOut is the set of endpoints whose wait ended by the message's own
	// time running out (bit i is Endpoint i).
	timedOut atomic.Uint32
}

// WithMessage marks ctx as belonging to the message path, whose loads honour
// gate: a coalescing loader called with the mark skips (ErrSkipped) instead
// of starting or joining a flight while the gate of its endpoint is open (or
// the key failed moments ago), stops waiting on a flight once it opens or the
// flight's first attempt failed, and stops waiting on a flight that has run
// for timeout (the message's own, see MessageTimeout) whenever it joined. A
// context without the mark (every getter, every flight) ignores all of it.
func WithMessage(ctx context.Context, gate Gate, timeout time.Duration) context.Context {
	return context.WithValue(ctx, markKey{}, &mark{gate: gate, timeout: timeout})
}

// WithoutGate returns ctx without the mark of WithMessage. A flight that is
// rooted in a caller's context (and not in its owner's lifetime) uses it, so
// the loads the flight makes in turn are not the message path's.
func WithoutGate(ctx context.Context) context.Context {
	return context.WithValue(ctx, markKey{}, (*mark)(nil))
}

// GateOf returns the gate of the message mark ctx carries, or nil.
func GateOf(ctx context.Context) Gate {
	if m, _ := ctx.Value(markKey{}).(*mark); m != nil {
		return m.gate
	}
	return nil
}

// TimeoutOf returns the timeout the message mark ctx carries: how long a
// flight may have run before a message stops waiting on it. It reports false
// for a context without the mark.
func TimeoutOf(ctx context.Context) (time.Duration, bool) {
	if m, _ := ctx.Value(markKey{}).(*mark); m != nil {
		return m.timeout, true
	}
	return 0, false
}

// NoteTimedOut records that a wait of the message ended because the
// message's own time ran out while it waited on endpoint. The factory trips
// the breakers of exactly these endpoints, so a message that waited on a hung
// player profile does not open the breaker of the match summary. It does
// nothing for a context without the mark.
func NoteTimedOut(ctx context.Context, ep Endpoint) {
	if m, _ := ctx.Value(markKey{}).(*mark); m != nil && ep < EndpointCount {
		for {
			old := m.timedOut.Load()
			if m.timedOut.CompareAndSwap(old, old|1<<ep) {
				return
			}
		}
	}
}

// TimedOut returns the endpoints NoteTimedOut recorded for ctx's message.
func TimedOut(ctx context.Context) []Endpoint {
	m, _ := ctx.Value(markKey{}).(*mark)
	if m == nil {
		return nil
	}
	bits := m.timedOut.Load()
	var eps []Endpoint
	for ep := Endpoint(0); ep < EndpointCount; ep++ {
		if bits&(1<<ep) != 0 {
			eps = append(eps, ep)
		}
	}
	return eps
}

type attemptKey struct{}

// WithAttemptFailed returns a context whose requests call fn each time an
// attempt of a retried request fails without ending the request (a server
// error, a rate limit, a transport error). A flight uses it to release the
// messages that wait on it at the first failure instead of at the end of the
// retries. fn must be cheap and safe to call repeatedly.
func WithAttemptFailed(ctx context.Context, fn func()) context.Context {
	return context.WithValue(ctx, attemptKey{}, fn)
}

// AttemptFailed reports a failed attempt to the function WithAttemptFailed
// put in ctx, if any. The API client calls it.
func AttemptFailed(ctx context.Context) {
	if fn, _ := ctx.Value(attemptKey{}).(func()); fn != nil {
		fn()
	}
}

// KeyFailure reports whether err says that the API answered for the key and
// the answer was a failure, which makes the key not worth asking again for a
// moment: a server error, an unreadable or misdirected body, a rejection.
// A transport error, a cancellation or a timeout is not one (it says nothing
// about the key, and the endpoint's breaker covers an outage), nor is a
// resource that does not exist (it may appear). The error types of the API
// client implement the method it looks for.
func KeyFailure(err error) bool {
	var kf interface{ KeyFailure() bool }
	return errors.As(err, &kf) && kf.KeyFailure()
}
