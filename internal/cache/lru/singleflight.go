package lru

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/oddin-gg/gosdk/internal/apipolicy"
)

// LoadTimeout is the soft outer bound applied to every detached
// singleflight load. The detach (`context.WithoutCancel`) means the
// caller's deadline does not cancel the shared load — without an
// independent bound, a misconfigured *http.Client (e.g. one passed via
// WithHTTPClient with `Timeout == 0`) could leave a stuck TCP connect
// blocking the singleflight slot indefinitely, jamming every other
// caller of the same key.
//
// 60s is generous for typical SDK loads (catalog / per-event XML
// fetches finish in milliseconds on the happy path) and only fires on
// genuine network hangs. Test packages that need a different bound
// (deliberate slow-loader stress tests) override this directly.
var LoadTimeout = 60 * time.Second

// LoadCoalesced runs `fn` under singleflight keyed on `sfKey`, coalescing
// concurrent callers with identical keys into a single in-flight load.
//
// Cancellation semantics:
//   - If the caller's ctx is already done, returns ctx.Err() immediately
//     without starting any load (no detached fetch on cold-cancelled ctx).
//   - The shared load is detached from the CALLER's cancellation — a
//     short-deadline first caller cannot cancel the load for later
//     waiters. It runs under `lifetime` (the owning cache's lifecycle
//     ctx, cancelled on Close), so a torn-down owner DOES cancel it:
//     no detached fetch outlives the component that started it. A nil
//     lifetime falls back to context.WithoutCancel(ctx) — fully
//     detached (standalone/test use).
//   - The shared load is additionally bounded by LoadTimeout so it
//     cannot outlive the caller's expectations indefinitely on a
//     misconfigured HTTP client. Per-call HTTP / loader timeouts still
//     bound the actual fetch on the happy path.
//   - Each caller independently selects on its own ctx, so a slow load
//     won't block past the caller's deadline.
//
// This is the simple "single key, return value" primitive. EventCache.Get
// implements a richer multi-locale-merge variant inline; for cache loaders
// that just want consistent ctx semantics across the codebase, use this.
func LoadCoalesced[T any](
	ctx context.Context,
	lifetime context.Context,
	sf *singleflight.Group,
	sfKey string,
	fn func(context.Context) (T, error),
) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	// base carries no timer/cancel of its own — building it out here is
	// free even for coalesced callers. The LoadTimeout ctx is created
	// INSIDE the closure: only the winning singleflight body runs (and
	// defers its cancel); building it per-caller would leak one live
	// timer per coalesced waiter until the 60s timeout fired.
	base := lifetime
	if base == nil {
		base = context.WithoutCancel(ctx)
	}
	// The call policy of the caller rides along (see apipolicy): flights of
	// different policies are separate, and the load keeps its caller's.
	//
	// A flight runs on its STARTER's budget, and the key only tells the
	// mode, so a caller that joined a flight can get the starter's budget
	// running out (a deadline error) while its own budget is untouched. It
	// then asks again once, under its own budget: it starts a new flight or
	// joins a newer one. Only the message modes retry (a public getter's
	// load has no per-message budget), and only a joiner: the starter's
	// error is its own.
	for attempt := 0; ; attempt++ {
		var started atomic.Bool
		ch := sf.DoChan(sfKey+apipolicy.Suffix(ctx), func() (any, error) {
			started.Store(true)
			loadCtx, cancel := context.WithTimeout(base, LoadTimeout)
			defer cancel()
			return fn(apipolicy.Carry(loadCtx, ctx))
		})
		FlightRegistered()
		select {
		case r := <-ch:
			if r.Err != nil {
				if attempt == 0 && !started.Load() && JoinerMayRetry(ctx, r.Err) {
					continue
				}
				return zero, r.Err
			}
			if r.Val == nil {
				return zero, nil
			}
			return r.Val.(T), nil
		case <-ctx.Done():
			return zero, ctx.Err()
		}
	}
}

// JoinerMayRetry reports whether a caller that joined a flight should ask
// again: the flight ended on a deadline (the starter's budget), while this
// caller's own context is alive and, under a message mode, its own budget is
// not spent. Every loader that coalesces on a flight and serves the message
// path uses it, so the rule stays in one place.
func JoinerMayRetry(ctx context.Context, err error) bool {
	if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return false
	}
	if apipolicy.ModeOf(ctx) == apipolicy.Default {
		return false
	}
	remaining, ok := apipolicy.Remaining(ctx)
	return ok && remaining > 0
}

// flightHook is a test-only seam: see SetFlightRegisteredHook.
var flightHook atomic.Pointer[func()]

// FlightRegistered is called by every coalescing loader right after it
// registered on a flight (as its starter or as a joiner), before it waits.
// It does nothing in production.
func FlightRegistered() {
	if h := flightHook.Load(); h != nil {
		(*h)()
	}
}

// SetFlightRegisteredHook installs fn to be called by FlightRegistered and
// returns the function that removes it. Tests use it to release a gated
// flight only once a joiner is certainly parked on it, instead of sleeping
// and hoping it got there.
func SetFlightRegisteredHook(fn func()) (restore func()) {
	flightHook.Store(&fn)
	return func() { flightHook.Store(nil) }
}
