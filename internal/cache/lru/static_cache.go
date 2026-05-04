package lru

import (
	"context"
	"sync"
)

// StaticLoader fetches a full snapshot of catalog data for a single locale.
// Returned data replaces whatever was previously cached for that locale.
type StaticLoader[K comparable, L comparable, V any] func(ctx context.Context, locale L) (map[K]V, error)

// StaticCache is a small-catalog cache keyed by (locale, K). It loads each
// locale lazily on first access via the supplied loader, and a load failure
// does NOT poison the cache — the next access for that locale retries.
//
// Suitable for catalogs that fit comfortably in memory (sports, base market
// descriptions, market void reasons, match-status descriptions). For
// unbounded long-tail data (variant market descriptions, per-event entities)
// use EventCache.
type StaticCache[K comparable, L comparable, V any] struct {
	mu        sync.RWMutex
	perLocale map[L]*staticEntry[K, V]
	loader    StaticLoader[K, L, V]
}

type staticEntry[K comparable, V any] struct {
	mu      sync.Mutex // serializes load attempts for this locale
	loaded  bool
	data    map[K]V
	lastErr error
}

// NewStaticCache constructs a static catalog cache.
func NewStaticCache[K comparable, L comparable, V any](loader StaticLoader[K, L, V]) *StaticCache[K, L, V] {
	return &StaticCache[K, L, V]{
		perLocale: make(map[L]*staticEntry[K, V]),
		loader:    loader,
	}
}

// All returns the full catalog for the given locale, fetching once on first
// access and caching the result. Subsequent calls hit the in-memory map.
//
// On loader error the cached state is left "not loaded" so the next call
// retries — there's no poisoning of the cache by a transient failure.
func (c *StaticCache[K, L, V]) All(ctx context.Context, locale L) (map[K]V, error) {
	entry := c.entryFor(locale)

	// Fast path: already loaded.
	entry.mu.Lock()
	if entry.loaded {
		data := entry.data
		entry.mu.Unlock()
		return data, nil
	}
	defer entry.mu.Unlock()

	// Re-check under the lock (another goroutine may have just loaded).
	if entry.loaded {
		return entry.data, nil
	}

	data, err := c.loader(ctx, locale)
	if err != nil {
		entry.lastErr = err
		// loaded stays false; next call retries.
		return nil, err
	}
	entry.data = data
	entry.loaded = true
	entry.lastErr = nil
	return entry.data, nil
}

// Get returns a single entry by key+locale, loading the locale's catalog
// once on first access. Returns (zero, false, nil) for unknown keys.
func (c *StaticCache[K, L, V]) Get(ctx context.Context, locale L, key K) (V, bool, error) {
	var zero V
	all, err := c.All(ctx, locale)
	if err != nil {
		return zero, false, err
	}
	v, ok := all[key]
	return v, ok, nil
}

// Clear forces a refresh of the given locale on next access.
func (c *StaticCache[K, L, V]) Clear(locale L) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.perLocale, locale)
}

// Purge clears every locale.
func (c *StaticCache[K, L, V]) Purge() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.perLocale = make(map[L]*staticEntry[K, V])
}

// entryFor returns the (memoized) staticEntry for locale, creating it lazily.
func (c *StaticCache[K, L, V]) entryFor(locale L) *staticEntry[K, V] {
	c.mu.RLock()
	if e, ok := c.perLocale[locale]; ok {
		c.mu.RUnlock()
		return e
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.perLocale[locale]; ok {
		return e
	}
	e := &staticEntry[K, V]{}
	c.perLocale[locale] = e
	return e
}
