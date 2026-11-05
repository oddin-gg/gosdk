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
// background goroutine: expiry is enforced lazily — on access, and by a
// small bounded sweep of the oldest end on every Add.
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
// stays bounded by the size cap regardless of expiry timing. Access alone
// does not give a useful memory bound, though: entries that are never read
// again (an event that finished) would sit until the size cap pushed them
// out, so an idle cache that keeps seeing new keys grows to the cap and
// stays there. Every Add therefore also drops expired entries from the
// least recently used end, at most expirySweepPerAdd of them, so the work
// per Add is constant and a growing cache sheds what has outlived its TTL
// at the rate it takes in new entries. The sweep stops at the first live
// entry (the recency order is not the expiry order, so an expired entry
// behind a live one is shed once it reaches the end or is read). ttl <= 0
// disables expiry entirely.
type TTL[K comparable, V any] struct {
	mu      sync.Mutex
	ttl     time.Duration
	onEvict EvictCallback[K, V]
	lru     *simplelru.LRU[K, ttlEntry[V]]
	// now is the clock; time.Now except in tests.
	now func() time.Time
}

type ttlEntry[V any] struct {
	value   V
	expires time.Time // zero when the cache has no TTL
}

// NewTTL constructs the cache. size must be positive (construction
// sites pass compile-time constants; a non-positive size panics, same
// class as a nil-map write).
func NewTTL[K comparable, V any](size int, onEvict EvictCallback[K, V], ttl time.Duration) *TTL[K, V] {
	t := &TTL[K, V]{ttl: ttl, onEvict: onEvict, now: time.Now}
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

// expirySweepPerAdd bounds how many expired entries one Add drops from
// the oldest end.
const expirySweepPerAdd = 4

// sweepExpiredLocked drops up to expirySweepPerAdd expired entries from
// the least recently used end, firing the evict callback for each, and
// stops at the first live one.
func (t *TTL[K, V]) sweepExpiredLocked(now time.Time) {
	if t.ttl <= 0 {
		return
	}
	for range expirySweepPerAdd {
		_, e, ok := t.lru.GetOldest()
		if !ok || !t.expiredLocked(e, now) {
			return
		}
		t.lru.RemoveOldest()
	}
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
	if t.expiredLocked(e, t.now()) {
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
	if t.expiredLocked(e, t.now()) {
		t.lru.Remove(key)
		var zero V
		return zero, false
	}
	return e.value, true
}

// Add inserts/refreshes the entry with a fresh TTL, first dropping a
// bounded number of expired entries from the oldest end (see the type
// comment). Reports whether the size cap displaced the oldest entry; an
// entry dropped for being expired does not count.
func (t *TTL[K, V]) Add(key K, value V) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.sweepExpiredLocked(now)
	var exp time.Time
	if t.ttl > 0 {
		exp = now.Add(t.ttl)
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
// but not yet been swept or touched still count — the size cap bounds
// them, and no consumer semantics depend on an exact live count.
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
	now := t.now()
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
