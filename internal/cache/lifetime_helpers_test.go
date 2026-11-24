package cache

import (
	"testing"

	"github.com/oddin-gg/gosdk/internal/cache/lru"
)

// testLife is the lifetime of a cache built by a test; it ends with the test.
func testLife(t testing.TB) *lru.Lifetime {
	t.Helper()
	life := lru.NewLifetime(t.Context())
	t.Cleanup(life.End)
	return life
}
