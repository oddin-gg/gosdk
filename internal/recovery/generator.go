package recovery

import (
	"math"
	"math/rand"
	"sync"
)

// maxRequestID bounds generated recovery request IDs to the positive
// int32 range. request_id travels on the wire as a signed value and is
// correlated back from snapshot_complete; keeping it within int32 means
// it round-trips and stays positive on 32-bit architectures (where Go's
// int is 32-bit), not just on 64-bit. The SDK generates AND correlates
// its own IDs, so this bound is sufficient — no value outside it ever
// needs to be represented.
const maxRequestID = math.MaxInt32

type generator struct {
	value     int
	increment int
	mux       sync.Mutex
}

func (g *generator) next() int {
	g.mux.Lock()
	defer g.mux.Unlock()

	g.value += g.increment
	// Wrap within (0, maxRequestID] so the ID never goes negative or
	// exceeds int32 range on 32-bit builds. increment is small relative
	// to maxRequestID, so a single wrap per step suffices.
	if g.value > maxRequestID {
		g.value -= maxRequestID
	}
	if g.value <= 0 {
		g.value = 1
	}
	return g.value
}

func newGenerator(increment int) *generator {
	return &generator{
		// Seed randomly within the positive int32 range so a fresh SDK
		// instance is unlikely to reuse a previous instance's in-flight
		// request IDs. rand.Int31n keeps the seed non-negative and below
		// maxRequestID on every architecture (int(rand.Uint32()) went
		// negative on 32-bit for the upper half of the range).
		value:     int(rand.Int31n(maxRequestID)),
		increment: increment,
	}
}
