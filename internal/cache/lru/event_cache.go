// Package lru provides bounded, ctx-aware cache primitives used across the
// SDK's per-event and per-variant caches.
//
// EventCache wraps hashicorp/golang-lru/v2/expirable and adds:
//   - ctx propagation through the loader on cache misses
//   - singleflight deduplication so concurrent loaders for the same key share
//     a single in-flight request
//   - per-locale "merge into existing entry" semantics: callers describe the
//     locales they need, and the loader is asked only for the missing ones
//
// StaticCache (in static_cache.go) is the small-catalog variant with no
// bounded size and a simple per-locale RWMutex map.
package lru

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	hashilru "github.com/hashicorp/golang-lru/v2/expirable"
	"golang.org/x/sync/singleflight"
)

// LocaleKey is the constraint for cache keys that carry localized state.
type LocaleKey interface{ comparable }

// LocalizedEntry is implemented by cache values that carry per-locale fields.
// EventCache calls Locales() to determine which locales an entry already
// covers and asks the Loader only for the missing ones.
//
// Implementations MUST be safe for concurrent reads after the EventCache
// has admitted the entry. Locales() is called under the cache's read path
// and must not block.
type LocalizedEntry[L comparable] interface {
	// Locales returns the set of locales currently populated on this entry.
	Locales() []L
}

// Loader fetches data for a single key in the requested locales. The returned
// entry is merged into any existing cache entry for the same key — the cache
// passes the existing entry (or the zero value of T if none) for the loader
// to update in place, and the loader returns the (possibly same) value to
// store. The entry must be safe for concurrent reads after return.
type Loader[K comparable, L comparable, T any] func(
	ctx context.Context,
	key K,
	locales []L,
	existing T,
	hasExisting bool,
) (T, error)

// EventCache is a per-event LRU cache with TTL eviction and singleflight
// dedup of loader calls. Type T must implement LocalizedEntry[L].
type EventCache[K comparable, L comparable, T LocalizedEntry[L]] struct {
	lru    *hashilru.LRU[K, T]
	sf     singleflight.Group
	loader Loader[K, L, T]
}

// Config tunes the cache behavior.
type Config struct {
	// Size is the maximum number of entries before LRU eviction kicks in.
	// Zero falls back to DefaultEventCacheSize.
	Size int

	// TTL is the per-entry expiration. Zero falls back to DefaultEventCacheTTL.
	TTL time.Duration

	// OnEvict is called when an entry is evicted. Optional.
	OnEvict func(key any, value any)
}

// Defaults — sized for typical SDK consumers (thousands of active events).
const (
	DefaultEventCacheSize = 5000
	DefaultEventCacheTTL  = 12 * time.Hour
)

// ErrEntryNotPopulated is returned when the loader returned no error but
// the resulting entry still lacks at least one of the requested locales.
// This usually indicates a bug in the loader.
var ErrEntryNotPopulated = errors.New("cache entry missing requested locale after load")

// NewEventCache constructs a bounded, expiring, singleflight-protected
// per-event cache.
func NewEventCache[K comparable, L comparable, T LocalizedEntry[L]](
	cfg Config,
	loader Loader[K, L, T],
) *EventCache[K, L, T] {
	size := cfg.Size
	if size <= 0 {
		size = DefaultEventCacheSize
	}
	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = DefaultEventCacheTTL
	}
	var onEvict hashilru.EvictCallback[K, T]
	if cfg.OnEvict != nil {
		onEvict = func(k K, v T) { cfg.OnEvict(k, v) }
	}
	return &EventCache[K, L, T]{
		lru:    hashilru.NewLRU[K, T](size, onEvict, ttl),
		loader: loader,
	}
}

// Get returns the cached entry for key, ensuring it is populated for every
// locale in `locales`. If any locale is missing, the loader is invoked
// (deduplicated across concurrent callers via singleflight) and merged into
// the existing entry.
//
// Returns (zero, false, err) only on loader error. On success, returns
// (entry, found-or-loaded, nil).
func (c *EventCache[K, L, T]) Get(ctx context.Context, key K, locales []L) (T, bool, error) {
	var zero T

	// Fast path: cached entry already covers all requested locales.
	if v, ok := c.lru.Get(key); ok && coversAll(v.Locales(), locales) {
		return v, true, nil
	}

	// Slow path under singleflight: at most one loader per key in flight.
	sfKey := fmt.Sprintf("%v", key)
	v, err, _ := c.sf.Do(sfKey, func() (any, error) {
		// Re-check inside the singleflight critical region in case another
		// goroutine populated the entry while we were waiting.
		existing, hadExisting := c.lru.Get(key)
		if hadExisting && coversAll(existing.Locales(), locales) {
			return existing, nil
		}

		missing := missingLocales(existingLocales(existing, hadExisting), locales)
		if len(missing) == 0 {
			// Defensive: we got here with no missing locales; just return.
			return existing, nil
		}

		updated, lerr := c.loader(ctx, key, missing, existing, hadExisting)
		if lerr != nil {
			return zero, lerr
		}

		// Sanity check: every requested locale should now be present.
		if !coversAll(updated.Locales(), locales) {
			return zero, fmt.Errorf("loader for key %v returned without populating all requested locales: %w", key, ErrEntryNotPopulated)
		}

		c.lru.Add(key, updated)
		return updated, nil
	})

	if err != nil {
		return zero, false, err
	}
	if v == nil {
		return zero, false, nil
	}
	return v.(T), true, nil
}

// Peek returns a cached entry without triggering a load. The boolean is
// false if the key is not in the cache.
func (c *EventCache[K, L, T]) Peek(key K) (T, bool) {
	return c.lru.Peek(key)
}

// Clear removes a single key from the cache.
func (c *EventCache[K, L, T]) Clear(key K) {
	c.lru.Remove(key)
}

// Purge removes everything from the cache. Used on Close.
func (c *EventCache[K, L, T]) Purge() {
	c.lru.Purge()
}

// Len returns the current number of entries.
func (c *EventCache[K, L, T]) Len() int {
	return c.lru.Len()
}

// internal helpers

func existingLocales[L comparable, T LocalizedEntry[L]](v T, has bool) []L {
	if !has {
		return nil
	}
	return v.Locales()
}

func coversAll[L comparable](have, want []L) bool {
	if len(want) == 0 {
		return true
	}
	set := make(map[L]struct{}, len(have))
	for _, l := range have {
		set[l] = struct{}{}
	}
	for _, l := range want {
		if _, ok := set[l]; !ok {
			return false
		}
	}
	return true
}

func missingLocales[L comparable](have, want []L) []L {
	if len(want) == 0 {
		return nil
	}
	set := make(map[L]struct{}, len(have))
	for _, l := range have {
		set[l] = struct{}{}
	}
	missing := make([]L, 0, len(want))
	for _, l := range want {
		if _, ok := set[l]; !ok {
			missing = append(missing, l)
		}
	}
	return missing
}

// pkgGuard prevents an unused-import for sync when no method uses it directly
// in the public surface; kept for future extensions to this file.
var _ sync.Mutex
