package lru

import (
	"context"
	"sync/atomic"
	"time"

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

// LoadCoalesced runs `fn` as a flight (see Flights) keyed on `sfKey`,
// coalescing concurrent callers with identical keys into a single in-flight
// load.
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
	sf *Flights,
	sfKey string,
	fn func(context.Context) (T, error),
) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if err := SkippedByGate(ctx); err != nil {
		return zero, err
	}
	// base carries no timer/cancel of its own — building it out here is
	// free even for coalesced callers. The LoadTimeout ctx is created
	// INSIDE the closure: only the winning singleflight body runs (and
	// defers its cancel); building it per-caller would leak one live
	// timer per coalesced waiter until the 60s timeout fired.
	base := lifetime
	if base == nil {
		// Not the caller's message mark either: what the flight loads is
		// not the message path's.
		base = apipolicy.WithoutGate(context.WithoutCancel(ctx))
	}
	// The flight does not carry the caller's message mark: it runs for
	// whoever asks, with the getters' retries and timeouts, and each
	// caller bounds only its own wait. A caller on the message path also
	// stops waiting once the gate opens; the flight goes on and fills the
	// cache for later callers.
	f := sf.Go(sfKey, func() (any, error) {
		loadCtx, cancel := context.WithTimeout(base, LoadTimeout)
		defer cancel()
		return fn(loadCtx)
	})
	FlightRegistered()
	val, err := f.Await(ctx)
	if err != nil {
		return zero, err
	}
	if val == nil {
		return zero, nil
	}
	return val.(T), nil
}

// StartCoalesced starts the flight of key as a flight without waiting
// for it, or does nothing more than join the one that is running. It is the
// background refresh of a value the caller can serve stale: the flight runs
// under `lifetime` (the owning cache's, so Close cancels it) with the same
// LoadTimeout as LoadCoalesced, carries no message mark, and its result is
// dropped: fn stores what it loads. No goroutine waits for it.
func StartCoalesced[T any](
	lifetime context.Context,
	sf *Flights,
	sfKey string,
	fn func(context.Context) (T, error),
) {
	base := lifetime
	if base == nil {
		// Standalone/test use: no owner to cancel it, and no caller to inherit from.
		base = context.Background() //nolint:contextcheck // deliberate: a background refresh has no caller context
	}
	sf.Go(sfKey, func() (any, error) {
		loadCtx, cancel := context.WithTimeout(base, LoadTimeout)
		defer cancel()
		return fn(loadCtx)
	})
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
