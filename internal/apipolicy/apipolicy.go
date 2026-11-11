// Package apipolicy carries the REST call policy of the feed message path
// through a context, so the layers between the message factory and the HTTP
// client (entity caches, singleflight loaders) do not need a parameter for
// it.
//
// Two callers ask the API for the same entity: a public getter
// (Client.Match, ...), which waits for the answer and retries transient
// failures, and the message path, which builds the event of every incoming
// feed message on the session goroutine. A getter that waits for an
// unavailable API only delays that caller; the message path doing the same
// delays every message behind it. The message path therefore marks its
// context: the API client makes one attempt with a short timeout and no
// retries, and, for an event the factory recently failed to build, makes no
// request at all.
//
// The policy holds only while every loader that coalesces concurrent calls
// onto one flight does three things. It detaches the flight's context from
// the caller's cancellation but keeps the caller's policy: Carry. It keeps
// flights of different policies apart in the flight key: Suffix, so a getter
// that waits through retries does not hold a message-path caller behind it
// and a single-attempt failure does not become a getter's answer. And a
// loader that discards flights by key (a Clear that calls Forget) discards
// the keys of every policy: the Suffixes loop (loaders that detach callers by
// a generation number in the key do not forget keys and need no loop). A
// loader that misses one of these either lets a message wait out a getter's
// retries or lets a getter share the failure of a message's one attempt.
// lru.LoadCoalesced does the first two for its callers, and EventCache all
// three; a new loader should use one of them rather than call singleflight
// directly.
package apipolicy

import (
	"context"
	"errors"
	"time"
)

// Mode is the call policy a context carries. The zero value is the
// default policy of every ordinary caller.
type Mode uint8

const (
	// Default: retries with backoff, the client's normal timeouts.
	Default Mode = iota
	// Message: a single attempt bounded by MessageTimeout, no retries.
	Message
	// MessageSkip: no request is made; a load that needs the API fails at
	// once with ErrSkipped, one that is served from cache succeeds.
	MessageSkip
)

// MessageTimeout is the budget of ONE message-path build: every request the
// build makes (the match summary, the tournament, the competitors, the
// fixture, further locales) shares it and inherits what is left of it, so
// a build is bounded as a whole, not per request. Headers and body of each
// request count against it.
const MessageTimeout = 2 * time.Second

// ErrSkipped is returned for a load that was skipped because the REST API
// failed moments ago (see Gate), and for a call made under MessageSkip.
var ErrSkipped = errors.New("api: skipped, the REST API failed moments ago")

// ErrBudgetSpent is returned for a call made under a message mode whose
// shared budget (MessageTimeout) was already spent when the call began, so
// nothing was sent. It also satisfies errors.Is(err, context.DeadlineExceeded).
// A request that was sent and then ran out of time is a real failure and
// does not carry it.
var ErrBudgetSpent = errors.New("api: message-path budget spent before the request was sent")

type ctxKey struct{}

// policy is what a context carries: the mode and, for the message modes,
// the instant the whole build's budget runs out.
type policy struct {
	mode     Mode
	deadline time.Time
}

// With returns ctx carrying the policy m. For the message modes the budget
// (MessageTimeout) starts now and covers everything done under the returned
// context.
func With(ctx context.Context, m Mode) context.Context {
	return WithBudget(ctx, m, MessageTimeout)
}

// WithBudget is With for a budget other than MessageTimeout (tests that need
// a short or an already spent one). The Default mode carries none.
func WithBudget(ctx context.Context, m Mode, budget time.Duration) context.Context {
	p := policy{mode: m}
	if m != Default {
		p.deadline = time.Now().Add(budget)
	}
	return context.WithValue(ctx, ctxKey{}, p)
}

func policyOf(ctx context.Context) policy {
	p, _ := ctx.Value(ctxKey{}).(policy)
	return p
}

// ModeOf returns the policy ctx carries (Default when none).
func ModeOf(ctx context.Context) Mode { return policyOf(ctx).mode }

// Remaining returns what is left of the message budget under ctx, and
// whether ctx carries one (only the message modes do). A non-positive
// duration means the budget is spent.
func Remaining(ctx context.Context) (time.Duration, bool) {
	p := policyOf(ctx)
	if p.deadline.IsZero() {
		return 0, false
	}
	return time.Until(p.deadline), true
}

// Carry returns dst carrying the policy of src. Loaders that detach their
// context from the caller's (shared singleflight loads rooted at a cache
// lifetime) use it to keep the policy of the caller whose load it is.
func Carry(dst, src context.Context) context.Context {
	if p := policyOf(src); p.mode != Default {
		return context.WithValue(dst, ctxKey{}, p)
	}
	return dst
}

// Suffix returns a singleflight key suffix that keeps flights of
// different policies apart: a getter that waits through retries must not
// hold a message-path caller behind it, and a message-path caller's
// single-attempt failure must not become the getter's answer. Empty for
// the default policy, so default keys are unchanged.
func Suffix(ctx context.Context) string {
	switch ModeOf(ctx) {
	case Message:
		return "|msg"
	case MessageSkip:
		return "|skip"
	default:
		return ""
	}
}

// Suffixes lists every suffix Suffix can return, for invalidation that
// must reach flights of all policies.
var Suffixes = [...]string{"", "|msg", "|skip"}

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

type gateKey struct{}

// WithMessage marks ctx as belonging to the message path, whose loads honour
// gate: a coalescing loader called with the mark skips (ErrSkipped) instead
// of starting or joining a flight while the gate is open, and stops waiting
// on a flight once it opens. A context without the mark (every getter,
// every flight) ignores the gate.
func WithMessage(ctx context.Context, gate Gate) context.Context {
	return context.WithValue(ctx, gateKey{}, gate)
}

// WithoutGate returns ctx without the mark of WithMessage. A flight that is
// rooted in a caller's context (and not in its owner's lifetime) uses it, so
// the loads the flight makes in turn are not the message path's.
func WithoutGate(ctx context.Context) context.Context {
	return context.WithValue(ctx, gateKey{}, Gate(nil))
}

// GateOf returns the gate of the message mark ctx carries, or nil.
func GateOf(ctx context.Context) Gate {
	g, _ := ctx.Value(gateKey{}).(Gate)
	return g
}
