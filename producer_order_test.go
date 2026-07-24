package gosdk

import (
	"testing"

	"github.com/oddin-gg/gosdk/types"
)

// idOnlyProducer implements just ID() — the only method mapToSlice reads —
// by embedding a nil types.Producer for the rest.
type idOnlyProducer struct {
	types.Producer
	id int
}

func (p idOnlyProducer) ID() int { return p.id }

// TestMapToSlice_DeterministicByID pins that the public producer-list
// accessors (Producers / ActiveProducers / ProducersInScope, all built on
// mapToSlice) return a stable, ID-ascending order rather than Go's
// randomized map-iteration order — so a caller's prods[0] is deterministic.
func TestMapToSlice_DeterministicByID(t *testing.T) {
	m := map[int]types.Producer{
		4: idOnlyProducer{id: 4},
		1: idOnlyProducer{id: 1},
		3: idOnlyProducer{id: 3},
		2: idOnlyProducer{id: 2},
	}
	want := []int{1, 2, 3, 4}

	// Run repeatedly: a randomized order would eventually diverge.
	for iter := 0; iter < 50; iter++ {
		got := mapToSlice(m)
		if len(got) != len(want) {
			t.Fatalf("len = %d, want %d", len(got), len(want))
		}
		for i, p := range got {
			if p.ID() != want[i] {
				t.Fatalf("iter %d: mapToSlice not ascending by ID: got %d at index %d, want %d", iter, p.ID(), i, want[i])
			}
		}
	}
}
