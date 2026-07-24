package cache

import (
	"cmp"
	"slices"

	"github.com/oddin-gg/gosdk/types"
)

// sortURNs orders a URN slice canonically (prefix, type, id). Public
// collections projected from maps (sports, tournament IDs, competitor
// IDs, …) previously surfaced Go's randomized map-iteration order — a
// first uncached call could retain upstream order while the next cached
// call reshuffled, churning UIs, serialized output, and diff-based
// consumers. Every map projection sorts through this before returning.
func sortURNs(urns []types.URN) {
	slices.SortFunc(urns, func(a, b types.URN) int {
		if c := cmp.Compare(a.Prefix, b.Prefix); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Type, b.Type); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
}
