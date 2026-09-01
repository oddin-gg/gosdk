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
// Small-catalog data (sports, market descriptions, void reasons,
// match-status descriptions) is NOT cached through this package: those
// caches live in internal/cache with their own per-locale maps, expiring
// catalog marks, and clear-tombstone machinery.
package lru

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

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
	lru    *TTL[K, T]
	sf     singleflight.Group
	loader Loader[K, L, T]

	// clearMu + clearedAt implement the Clear-vs-in-flight-load guard.
	// A loader closure snapshots time.Now() before reading the existing
	// entry; before re-admitting its merged result it checks — under
	// clearMu, mutually exclusive with Clear's tombstone+Remove — that
	// no Clear landed after that snapshot. Without this, a Clear (public
	// ClearMatch etc., or FixtureChange auto-invalidation) racing a slow
	// load was silently undone: the loader re-Added the pre-clear entry
	// with a fresh TTL. Tombstones are pruned once older than any
	// possible in-flight load (LoadTimeout-bounded), so the map cannot
	// grow past the churn of a single load window.
	clearMu   sync.Mutex
	clearedAt map[K]time.Time

	// purgedAt (guarded by clearMu) + purgeGen extend the same
	// invariant to Purge: purgedAt suppresses admission (and StoreSide
	// commits) for any flight that started before the purge, and
	// purgeGen is folded into the singleflight key so a caller arriving
	// AFTER a purge never joins a pre-purge flight (the per-key
	// equivalent — sf.Forget — can't enumerate keys on a global purge).
	// Without this fence a loader started before Purge could finish
	// afterward and repopulate the "emptied" cache, and Purge landing
	// between lru.Add and OnAdmit could orphan freshly-committed side
	// state.
	purgedAt time.Time
	purgeGen atomic.Uint64

	// lifetime is Config.Lifetime (may be nil); see that field's doc.
	lifetime context.Context

	// onAdmit is Config.OnAdmit (may be nil); see that field's doc.
	onAdmit func(key any, value any)
}

// errClearedDuringLoad signals that a Clear invalidated the entry while
// a load for it was in flight; the merged result was discarded and the
// caller should retry from the now-empty entry.
var errClearedDuringLoad = errors.New("cache entry cleared during load")

// maxClearRetries bounds how many times Get restarts a load whose result
// was discarded by a racing Clear. Each retry starts from an empty entry
// and completes unless ANOTHER Clear lands within the same load window —
// sustained sub-LoadTimeout clear cadence on one key is pathological.
const maxClearRetries = 3

// clearTombstoneSweepLen is the map size beyond which Clear amortizes a
// prune of expired tombstones.
const clearTombstoneSweepLen = 1024

// Config tunes the cache behavior.
type Config struct {
	// Size is the maximum number of entries before LRU eviction kicks in.
	// Zero falls back to DefaultEventCacheSize.
	Size int

	// TTL is the per-entry expiration. Zero falls back to DefaultEventCacheTTL.
	TTL time.Duration

	// OnEvict is called when an entry is evicted. Optional.
	OnEvict func(key any, value any)

	// OnAdmit is called under the clear-tombstone lock immediately
	// after a loaded entry is admitted to the cache — atomic with
	// Clear, exactly like StoreSide. Owners of SIDE state gathered
	// during a load (e.g. icon paths carried by profile payloads)
	// commit it here, so nothing is committed for a load whose entry
	// was never admitted (later-locale fetch failure, coverage
	// validation failure, racing Clear): a commit made earlier, from
	// inside the loader, would orphan the side value — no parent
	// entry means no eviction hook ever cleans it up. Must be brief
	// and must not call back into this cache. Optional.
	OnAdmit func(key any, value any)

	// Lifetime, when non-nil, is the detach root for singleflight
	// loads: a load survives its first caller's cancellation but NOT
	// the owning component's shutdown (cancel Lifetime on Close and
	// every in-flight loader's ctx cancels with it). Nil falls back to
	// context.WithoutCancel(callerCtx) — fully detached.
	Lifetime context.Context
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
	var onEvict EvictCallback[K, T]
	if cfg.OnEvict != nil {
		onEvict = func(k K, v T) { cfg.OnEvict(k, v) }
	}
	return &EventCache[K, L, T]{
		lru:       NewTTL[K, T](size, onEvict, ttl),
		loader:    loader,
		clearedAt: make(map[K]time.Time),
		lifetime:  cfg.Lifetime,
		onAdmit:   cfg.OnAdmit,
	}
}

// sfKey renders the singleflight key for an entity key. Shared by Get
// (flight creation) and Clear (Forget) so an invalidation always
// detaches future callers from a pre-clear in-flight load. The purge
// generation prefix detaches post-Purge callers from every pre-purge
// flight the same way.
func (c *EventCache[K, L, T]) sfKey(key K) string {
	return fmt.Sprintf("%d|%v", c.purgeGen.Load(), key)
}

// Get returns the cached entry for key, ensuring it is populated for every
// locale in `locales`. If any locale is missing, the loader is invoked
// (deduplicated across concurrent callers via singleflight) and merged into
// the existing entry.
//
// Concurrency: at most one loader per key runs at a time. Each caller
// selects on its own ctx.Done() (via DoChan), so a slow loader doesn't
// block later callers past their own deadlines. The shared load runs
// under context.WithoutCancel(ctx) — a short-deadline first caller
// cannot cancel the load for later waiters; the loader's HTTP client
// still bounds the request.
//
// Mixed-locale singleflight coalescing: the singleflight is keyed on
// the entity key alone, so caller A asking for [en] and caller B
// asking for [ru] share one in-flight load. The shared body uses A's
// `locales` (closure capture) — so B's locales might not be in the
// returned entry. After receiving the result we re-check coverage
// for the CURRENT caller's locales, and recurse to load any still-
// missing ones. The recursion is bounded: by the time the singleflight
// completes its result, the entity is in the cache; the next call
// (if any) starts a fresh singleflight that loads only the gap.
//
// Returns (zero, false, err) only on loader error or ctx cancellation
// of the calling goroutine. On success, returns (entry, found-or-loaded, nil).
func (c *EventCache[K, L, T]) Get(ctx context.Context, key K, locales []L) (T, bool, error) {
	return c.get(ctx, key, locales, 0, 0)
}

func (c *EventCache[K, L, T]) get(ctx context.Context, key K, locales []L, clearRetries, coverageRetries int) (T, bool, error) {
	var zero T

	// Fast path: cached entry already covers all requested locales.
	if v, ok := c.lru.Get(key); ok && coversAll(v.Locales(), locales) {
		return v, true, nil
	}

	// If the caller's ctx is already done, fail fast without kicking off
	// a detached loader the caller will never wait for. Without this
	// guard, an expired ctx still triggers an upstream load through
	// WithoutCancel below.
	if err := ctx.Err(); err != nil {
		return zero, false, err
	}

	// Detach the shared loader from this caller's cancellation so a
	// short-deadline first caller can't kill the load for later
	// waiters; per-caller cancellation is honored at the select below.
	// The detach root is the cache's lifetime ctx (when configured), so
	// the owner's Close cancels in-flight loads — a detached fetch must
	// not outlive the component that started it. Bounded by LoadTimeout
	// so a stuck upstream / misconfigured HTTP client cannot leave the
	// load running indefinitely (see LoadCoalesced for the same defense).
	base := c.lifetime //nolint:contextcheck // deliberate detach: the owning cache's lifetime ctx, not the caller's, is the correct cancellation root for the shared load
	if base == nil {
		base = context.WithoutCancel(ctx)
	}

	// Slow path under singleflight: at most one loader per key in flight.
	// The LoadTimeout ctx is created INSIDE the flight body — only the
	// winning caller's closure runs, so building it out here would leak
	// one live timer per coalesced waiter until the 60s timeout fired.
	ch := c.sf.DoChan(c.sfKey(key), func() (any, error) {
		loadCtx, loadCancel := context.WithTimeout(base, LoadTimeout)
		defer loadCancel()
		// Snapshot the flight's start BEFORE reading the existing entry:
		// the pre-Add check below discards this flight's merge if a
		// Clear lands after this point (see clearedAt field doc).
		started := time.Now()
		// Re-check inside the singleflight critical region in case another
		// goroutine populated the entry while we were waiting.
		existing, hadExisting := c.lru.Get(key)
		if hadExisting && coversAll(existing.Locales(), locales) {
			return existing, nil
		}

		missing := missingLocales(existingLocales(existing, hadExisting), locales)
		if len(missing) == 0 {
			if hadExisting {
				return existing, nil
			}
			// Cold key + no requested locales: nothing cached and
			// nothing to load. Returning the zero T here would box a
			// typed-nil into the flight result (`r.Val == nil` is false
			// for a nil *T in an interface) and panic on the coverage
			// re-check's Locales() call. Surface it as an error instead.
			return zero, fmt.Errorf("no cached entry for key %v and no locales requested: %w", key, ErrEntryNotPopulated)
		}

		updated, lerr := c.loader(loadCtx, key, missing, existing, hadExisting)
		if lerr != nil {
			return zero, lerr
		}

		// Sanity check: every requested locale should now be present.
		if !coversAll(updated.Locales(), locales) {
			return zero, fmt.Errorf("loader for key %v returned without populating all requested locales: %w", key, ErrEntryNotPopulated)
		}

		// Admit-or-discard, mutually exclusive with Clear: if a Clear
		// landed after `started`, our merge base (and thus `updated`)
		// may contain invalidated data — discard and let the caller
		// retry from the now-empty entry.
		//
		// Close-gate: a flight whose fetch completed just before the
		// owner's lifetime was cancelled could otherwise pause here,
		// survive a successful client Close, then resume and mutate the
		// cache — contradicting Close's quiescence guarantee. The
		// re-check immediately before admission shrinks that window to
		// a few instructions. (base is the lifetime ctx when the owner
		// configured one; the WithoutCancel fallback never fires.)
		c.clearMu.Lock()
		cleared := c.clearedAt[key].After(started) || c.purgedAt.After(started) || base.Err() != nil
		if !cleared {
			c.lru.Add(key, updated)
			if c.onAdmit != nil {
				c.onAdmit(key, updated)
			}
		}
		c.clearMu.Unlock()
		if cleared {
			return zero, errClearedDuringLoad
		}
		return updated, nil
	})

	var v T
	select {
	case r := <-ch:
		if errors.Is(r.Err, errClearedDuringLoad) {
			if clearRetries >= maxClearRetries {
				// Only reachable under a sustained storm of Clears for
				// this key, each landing within one load window. Bail
				// with a transient error rather than livelock.
				return zero, false, fmt.Errorf("cache load for key %v: %w", key, errClearedDuringLoad)
			}
			return c.get(ctx, key, locales, clearRetries+1, coverageRetries)
		}
		if r.Err != nil {
			return zero, false, r.Err
		}
		var ok bool
		if v, ok = r.Val.(T); !ok {
			return zero, false, nil
		}
	case <-ctx.Done():
		return zero, false, ctx.Err()
	}

	// Re-check coverage for THIS caller. If we coalesced onto another
	// caller's in-flight load that asked for a different locale set,
	// our locales may not be populated. Recurse — the next call sees
	// the partially-populated cache entry and singleflights only the
	// gap. Normally this converges in one extra round-trip because the
	// cached entry monotonically gains locales — but that only holds
	// while the entry stays resident, so a hard depth cap (one attempt
	// per requested locale, plus one) backstops TTL-expiry/eviction
	// races where the entry vanishes between the coalesced return and
	// the recursive call.
	if !coversAll(v.Locales(), locales) {
		if coverageRetries > len(locales) {
			return zero, false, fmt.Errorf("coverage retries exhausted for key %v (locales=%v): %w", key, locales, ErrEntryNotPopulated)
		}
		return c.get(ctx, key, locales, clearRetries, coverageRetries+1)
	}
	return v, true, nil
}

// Peek returns a cached entry without triggering a load. The boolean is
// false if the key is not in the cache.
func (c *EventCache[K, L, T]) Peek(key K) (T, bool) {
	return c.lru.Peek(key)
}

// Clear removes a single key from the cache. It also (a) records a
// tombstone so an in-flight load cannot re-admit its (pre-clear) merge
// — see the clearedAt field doc — and (b) Forgets the singleflight so
// callers arriving after the Clear start a fresh load instead of
// joining a pre-clear flight and receiving the entry this Clear just
// discarded.
//
// The flight key is captured UNDER clearMu so its purge-generation
// prefix is read atomically with the tombstone. Otherwise a Purge
// slipping between the unlock and a later sfKey() call would advance
// the generation, and Clear would Forget a newer, legitimate
// post-Purge flight instead of the pre-clear one it means to discard —
// splitting concurrent post-Purge callers across two loads whose
// empty-parent admissions overwrite (rather than merge) each other,
// plus a duplicate upstream request.
func (c *EventCache[K, L, T]) Clear(key K) {
	c.clearMu.Lock()
	now := time.Now()
	c.clearedAt[key] = now
	if len(c.clearedAt) > clearTombstoneSweepLen {
		// Amortized prune: a tombstone older than the longest possible
		// in-flight load can no longer affect any flight (loads are
		// LoadTimeout-bounded); one extra minute of slack for scheduling.
		cutoff := now.Add(-(LoadTimeout + time.Minute))
		for k, t := range c.clearedAt {
			if t.Before(cutoff) {
				delete(c.clearedAt, k)
			}
		}
	}
	c.lru.Remove(key)
	flightKey := c.sfKey(key)
	c.clearMu.Unlock()
	if clearForgetHook != nil {
		clearForgetHook()
	}
	// Forgetting the captured key is correct even if a Purge advanced
	// the generation after the unlock: post-Purge callers use a
	// different key, so this only detaches the pre-clear flight it is
	// meant to discard.
	c.sf.Forget(flightKey)
}

// clearForgetHook is a test-only seam invoked in Clear after the flight
// key is captured (under clearMu) and the lock is released, but before
// the singleflight Forget. Production builds leave it nil.
var clearForgetHook func()

// StoreSide runs `store` under the clear-tombstone lock iff key was NOT
// Cleared after `since`, returning whether the store ran. For owners of
// SIDE state keyed alongside cache entries (e.g. the tournament /
// competitor icon maps): their stores must be suppressed by exactly the
// invalidations that suppress the parent entry. Executing the store
// INSIDE clearMu makes it atomic with Clear's tombstone + Remove (whose
// OnEvict deletes side state under the same lock): either the store
// lands first and the racing Clear's eviction removes it, or the Clear's
// tombstone lands first and the store is suppressed. A bare
// check-then-store on the tombstone would leave a window between the
// check and the write for a Clear to slip through — the side value would
// then survive an invalidation that removed the parent entry.
//
// `store` must be brief and must not call back into this cache's Clear /
// Get / Purge (clearMu is held).
func (c *EventCache[K, L, T]) StoreSide(key K, since time.Time, store func()) bool {
	c.clearMu.Lock()
	defer c.clearMu.Unlock()
	if c.clearedAt[key].After(since) || c.purgedAt.After(since) {
		return false
	}
	store()
	return true
}

// Purge removes everything from the cache. Used on Close.
//
// Concurrency: fenced exactly like Clear, but globally — the purge
// timestamp suppresses admission of any flight that started before it
// (an in-flight loader can no longer finish afterward and silently
// repopulate the cache), the generation bump detaches later callers
// from every pre-purge flight, and running the LRU purge under clearMu
// keeps it atomic with the Add+OnAdmit admission pair (a purge slipping
// between them would otherwise orphan just-committed side state).
func (c *EventCache[K, L, T]) Purge() {
	c.clearMu.Lock()
	c.purgedAt = time.Now()
	c.purgeGen.Add(1)
	c.lru.Purge()
	c.clearMu.Unlock()
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
