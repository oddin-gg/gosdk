package lru

import (
	"context"

	"github.com/oddin-gg/gosdk/internal/apipolicy"
)

// SkippedByGate is what every coalescing loader asks first, after its cache
// fast path: for a caller on the message path (see apipolicy.WithMessage) it
// returns apipolicy.ErrSkipped while the breaker of the loader's endpoint ep
// is open, and while the last flight of key (in sf) failed with an answer of
// the API moments ago. The loader then neither starts nor joins a flight. It
// returns nil for every other caller, whatever the gate says.
func SkippedByGate(ctx context.Context, sf *Flights, key string, ep apipolicy.Endpoint) error {
	g := apipolicy.GateOf(ctx)
	if g == nil {
		return nil
	}
	if g.Open(ep) || sf.Remembered(key) {
		return apipolicy.ErrSkipped
	}
	return nil
}

// GateOpened returns the channel a loader adds to the select in which it
// waits on its flight: it is closed once the gate of its endpoint opens, and
// the loader then returns apipolicy.ErrSkipped while the flight goes on and
// fills the cache for later callers. It is nil, which blocks forever in a
// select, for a caller without the message mark.
func GateOpened(ctx context.Context, ep apipolicy.Endpoint) <-chan struct{} {
	if g := apipolicy.GateOf(ctx); g != nil {
		return g.Opened(ep)
	}
	return nil
}
