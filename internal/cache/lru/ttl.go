package lru

import (
	"sync"
	"time"

	"github.com/hashicorp/golang-lru/v2/simplelru"
)

// EvictCallback is invoked with the evicted key/value — same shape the
// hashicorp caches use.
type EvictCallback[K comparable, V any] func(key K, value V)

// TTL is a size-bounded, thread-safe LRU with per-entry TTL and NO
// background goroutine: expiry is enforced lazily on access.
//
// It replaces hashicorp's expirable.LRU (v2.0.7), which starts a
// cleanup goroutine per cache that can never exit — upstream's own
// source says "done channel is never closed, so deleteExpired()
// goroutine will never exit" and ships Close commented out. Every
// constructed Client held seven such caches, so repeated New/Close
// grew goroutine and retained-memory counts linearly and made
// Client.Close's full-join guarantee untrue.
//
// Lazy expiry needs no janitor for correctness: an expired entry is
// never returned (evicted on the access that discovers it), and memory
// stays bounded by the size cap regardless of expiry timing. ttl <= 0
// disables expiry entirely.
type TTL[K comparable, V any] struct {
	mu      sync.Mutex
	ttl     time.Duration
	onEvict EvictCallback[K, V]
	lru     *simplelru.LRU[K, ttlEntry[V]]
}

type ttlEntry[V any] struct {
	value   V
	expires time.Time // zero when the cache has no TTL
}

// NewTTL constructs the cache. size must be positive (construction
// sites pass compile-time constants; a non-positive size panics, same
// class as a nil-map write).
func NewTTL[K comparable, V any](size int, onEvict EvictCallback[K, V], ttl time.Duration) *TTL[K, V] {
	t := &TTL[K, V]{ttl: ttl, onEvict: onEvict}
	var inner func(K, ttlEntry[V])
	if onEvict != nil {
		inner = func(k K, e ttlEntry[V]) { onEvict(k, e.value) }
	}
	l, err := simplelru.NewLRU[K, ttlEntry[V]](size, inner)
	if err != nil {
		panic(err) // size <= 0 — programmer error at the construction site
	}
	t.lru = l
	return t
}

func (t *TTL[K, V]) expiredLocked(e ttlEntry[V], now time.Time) bool {
	return t.ttl > 0 && now.After(e.expires)
}

// Get returns the live value and marks it recently used. An expired
// entry is a miss and is evicted (firing the evict callback, matching
// the expirable.LRU behavior it replaces).
func (t *TTL[K, V]) Get(key K) (V, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.lru.Get(key)
	if !ok {
		var zero V
		return zero, false
	}
	if t.expiredLocked(e, time.Now()) {
		t.lru.Remove(key)
		var zero V
		return zero, false
	}
	return e.value, true
}

// Peek returns the live value without recency update.
func (t *TTL[K, V]) Peek(key K) (V, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.lru.Peek(key)
	if !ok {
		var zero V
		return zero, false
	}
	if t.expiredLocked(e, time.Now()) {
		t.lru.Remove(key)
		var zero V
		return zero, false
	}
	return e.value, true
}

// Add inserts/refreshes the entry with a fresh TTL. Reports whether an
// eviction displaced the oldest entry.
func (t *TTL[K, V]) Add(key K, value V) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	var exp time.Time
	if t.ttl > 0 {
		exp = time.Now().Add(t.ttl)
	}
	return t.lru.Add(key, ttlEntry[V]{value: value, expires: exp})
}

// Remove deletes the entry (firing the evict callback when present).
func (t *TTL[K, V]) Remove(key K) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lru.Remove(key)
}

// Purge empties the cache, firing the evict callback per entry.
func (t *TTL[K, V]) Purge() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lru.Purge()
}

// Len reports the number of stored entries. Entries that have expired
// but not yet been touched still count — the size cap bounds them, and
// no consumer semantics depend on an exact live count.
func (t *TTL[K, V]) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lru.Len()
}

// Keys returns the LIVE keys, oldest to newest, evicting any expired
// entries it encounters.
func (t *TTL[K, V]) Keys() []K {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	keys := t.lru.Keys()
	out := make([]K, 0, len(keys))
	for _, k := range keys {
		e, ok := t.lru.Peek(k)
		if !ok {
			continue
		}
		if t.expiredLocked(e, now) {
			t.lru.Remove(k)
			continue
		}
		out = append(out, k)
	}
	return out
}
