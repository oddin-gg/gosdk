package cache

import (
	"errors"
	"net/http"

	"github.com/oddin-gg/gosdk/internal/api"
)

// notFoundIfAbsent maps a DEFINITIVE upstream 404 into the
// ErrItemNotFoundInCache classification while preserving the original
// APIError in the chain (errors.Join), so both errors.Is(err,
// ErrItemNotFound) and errors.As(err, **api.Error) hold. Single-entity
// reads (match, tournament, player, competitor, match status, fixture)
// resolve via a by-id API fetch whose absence is a 404 — unlike the
// bulk-catalog reads (sport, market description) where "loaded but id
// absent" already yields the sentinel. The exported ErrItemNotFound
// documentation names all of these; without this the by-id-fetch
// entities propagated a bare APIError and errors.Is(err,
// ErrItemNotFound) silently failed for them.
// Non-404 errors (transport, 5xx, validation) pass through unchanged so
// callers still distinguish "definitely absent" from "try again".
func notFoundIfAbsent(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *api.Error
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		return errors.Join(err, ErrItemNotFoundInCache)
	}
	return err
}

// ErrItemNotFoundInCache is returned (wrapped) when a cache lookup
// completes successfully (no fetch error) but the requested item is
// not present in the cache. Consumers can errors.Is to distinguish
// "API failure" from "API said no such item".
var ErrItemNotFoundInCache = errors.New("item not found in cache")

// ErrLocaleNotLoaded is returned (wrapped) when a localized accessor
// is asked for a locale that wasn't preloaded. Distinguishes from
// "the entity itself is missing" (which uses ErrItemNotFoundInCache).
var ErrLocaleNotLoaded = errors.New("locale not loaded")

// ErrMarketLocaleIncomplete is returned (wrapped) when a market
// description cannot satisfy every requested locale even AFTER the
// catalog for those locales was loaded — the upstream catalog has no
// (or a malformed, skipped-at-decode) entry for this market in the
// missing locale(s). Distinct from ErrLocaleNotLoaded: retrying will
// not help until the upstream catalog itself changes.
var ErrMarketLocaleIncomplete = errors.New("market description missing requested locale data")

// ErrSportLocaleIncomplete is the sport-catalog analogue of
// ErrMarketLocaleIncomplete: a by-id sport lookup found the sport, and
// every requested locale's catalog was loaded, but this sport carries no
// name/abbreviation in some requested locale (the upstream catalog omits
// it there). The sport-list catalog is per-locale global, so loading two
// asymmetric locales marks both loaded while a sport present in only one
// keeps a locale gap. Distinct from ErrLocaleNotLoaded (locale never
// requested) and ErrItemNotFoundInCache (sport absent entirely): retrying
// will not help until the upstream catalog itself changes.
var ErrSportLocaleIncomplete = errors.New("sport missing requested locale data")

// errClearedDuringLoad signals that a ClearCacheItem/Purge landed while
// a singleflight load was in flight, so the observer-driven store was
// suppressed by the clear tombstone and the loader's final lookup came
// back empty. A transient invalidation race — NOT definitive upstream
// absence — so it must never wrap ErrItemNotFoundInCache: callers retry
// from the now-empty entry (the cache-package analogue of
// lru.errClearedDuringLoad). Escapes only wrapped, on retry exhaustion
// under a sustained clear storm, where it still reads as retryable.
var errClearedDuringLoad = errors.New("cache: entry cleared during load")

// errStaleFlight aborts a singleflight load whose generation advanced
// between key construction and closure entry. The window: a getter reads
// flightGen, a Clear increments it (recording its tombstone) and THEN
// the getter registers the old-generation flight — its load would start
// after the clear (so time-based tombstone admission considers it fresh)
// while new-generation callers register a SEPARATE flight: coalescing
// splits, the API is hit twice, and the two loads race their stores.
// Every loader closure re-checks the generation on entry and returns
// this sentinel; callers retry with the fresh generation, re-joining the
// surviving flight. Never escapes the cache package.
var errStaleFlight = errors.New("cache: flight generation invalidated during registration")
