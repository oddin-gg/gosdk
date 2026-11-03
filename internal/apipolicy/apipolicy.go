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

// ErrSkipped is returned for a call made under MessageSkip.
var ErrSkipped = errors.New("api: request skipped, the API recently failed for this event")

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
	p := policy{mode: m}
	if m != Default {
		p.deadline = time.Now().Add(MessageTimeout)
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
