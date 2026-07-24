package recovery

import (
	"math"
	"testing"
)

// TestGenerator_SeedInPositiveInt32Range pins that a freshly seeded
// generator never starts negative or above int32 range — the pre-fix
// int(rand.Uint32()) went negative on 32-bit for the upper half of the
// uint32 range, corrupting request_id correlation there.
func TestGenerator_SeedInPositiveInt32Range(t *testing.T) {
	for i := 0; i < 10000; i++ {
		g := newGenerator(1)
		if g.value < 0 || g.value >= maxRequestID {
			t.Fatalf("seed %d out of [0, %d)", g.value, maxRequestID)
		}
	}
}

// TestGenerator_NextStaysPositiveAndInRange exercises next() across a
// wrap at maxRequestID and asserts every emitted id is a valid,
// positive int32-range request_id on any architecture.
func TestGenerator_NextStaysPositiveAndInRange(t *testing.T) {
	// Seed just below the ceiling with a large increment so the very
	// next step overflows int32 and must wrap.
	g := &generator{value: maxRequestID - 3, increment: 10}
	for i := 0; i < 100; i++ {
		id := g.next()
		if id <= 0 {
			t.Fatalf("next() returned non-positive id %d (would break 32-bit / API)", id)
		}
		if id > maxRequestID {
			t.Fatalf("next() returned %d > maxRequestID %d", id, maxRequestID)
		}
	}
}

// TestGenerator_Increments confirms the happy-path monotonic behavior is
// intact away from the wrap boundary.
func TestGenerator_Increments(t *testing.T) {
	g := &generator{value: 100, increment: 1}
	if got := g.next(); got != 101 {
		t.Fatalf("next() = %d, want 101", got)
	}
	if got := g.next(); got != 102 {
		t.Fatalf("next() = %d, want 102", got)
	}
}

// TestMaxRequestID_IsInt32Ceiling documents the bound so a future change
// to a wider type is a conscious decision.
func TestMaxRequestID_IsInt32Ceiling(t *testing.T) {
	if maxRequestID != math.MaxInt32 {
		t.Fatalf("maxRequestID = %d, want math.MaxInt32 (%d)", maxRequestID, math.MaxInt32)
	}
}
