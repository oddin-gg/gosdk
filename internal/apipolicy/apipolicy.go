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
// breaker of the API client: while the breaker is open it asks the caches
// only.
//
// The mark does not change the request. Every load, whoever asked for it,
// runs as a flight with the getters' retries and timeouts; the flight
// carries no mark and goes on when a message stops waiting for it, and what
// it loads is there for later messages and getters. The mark only decides
// what the caller does before and while it waits:
//
//   - A coalescing loader called with the mark neither starts nor joins a
//     flight while the gate is open: it returns ErrSkipped.
//   - It stops waiting on a flight, with ErrSkipped, once the gate opens.
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
	"time"
)

// MessageTimeout is how long one message-path build waits for everything it
// needs (the match summary, the tournament, the competitors, the fixture,
// further locales, the market and player names) before it goes on with what
// it has.
const MessageTimeout = 2 * time.Second

// ErrSkipped is returned for a load that was skipped because the REST API
// failed moments ago (see Gate).
var ErrSkipped = errors.New("api: skipped, the REST API failed moments ago")

// Gate tells the loaders whether the REST API failed moments ago. The API
// client's breaker implements it; the interface keeps the cache packages
// from importing the client.
type Gate interface {
	// Open reports whether callers that honour the gate must not load.
	Open() bool
	// Opened returns a channel that is closed once the gate is open (at
	// once if it already is). Take a new one after each wake-up.
	Opened() <-chan struct{}
}

type markKey struct{}

type mark struct {
	gate    Gate
	timeout time.Duration
}

// WithMessage marks ctx as belonging to the message path, whose loads honour
// gate: a coalescing loader called with the mark skips (ErrSkipped) instead
// of starting or joining a flight while the gate is open, stops waiting on a
// flight once it opens, and stops waiting on a flight that has run for
// timeout (the message's own, see MessageTimeout) whenever it joined. A
// context without the mark (every getter, every flight) ignores all three.
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
