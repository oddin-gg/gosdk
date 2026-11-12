package lru

import (
	"context"

	"github.com/oddin-gg/gosdk/internal/apipolicy"
)

// SkippedByGate is what every coalescing loader asks first, after its cache
// fast path: for a caller on the message path (see apipolicy.WithMessage)
// while the gate is open it returns apipolicy.ErrSkipped, and the loader
// then neither starts nor joins a flight. It returns nil for every other
// caller, whatever the gate says.
func SkippedByGate(ctx context.Context) error {
	if g := apipolicy.GateOf(ctx); g != nil && g.Open() {
		return apipolicy.ErrSkipped
	}
	return nil
}

// GateOpened returns the channel a loader adds to the select in which it
// waits on its flight: it is closed once the gate opens, and the loader then
// returns apipolicy.ErrSkipped while the flight goes on and fills the cache
// for later callers. It is nil, which blocks forever in a select, for a
// caller without the message mark.
func GateOpened(ctx context.Context) <-chan struct{} {
	if g := apipolicy.GateOf(ctx); g != nil {
		return g.Opened()
	}
	return nil
}
