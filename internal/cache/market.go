package cache

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/oddin-gg/gosdk/internal/api"
	data "github.com/oddin-gg/gosdk/internal/api/xml"
	"github.com/oddin-gg/gosdk/internal/cache/lru"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/internal/utils"
	"github.com/oddin-gg/gosdk/types"
)

// CompositeKey identifies a market description: marketID + optional variant.
//
// Variant is a plain string ("" = base description, no variant) so the
// key compares by VALUE. It was previously *string — and Go compares
// pointer fields by identity, so a freshly-built lookup key could never
// equal a stored one: every variant lookup missed, every load inserted
// a new entry under a new pointer key, and the map grew per ACCESS
// (not per variant) while the same variant re-downloaded every time.
type CompositeKey struct {
	MarketID int
	Variant  string
}

// String renders the key for logs/diagnostics.
func (k CompositeKey) String() string {
	v := k.Variant
	if v == "" {
		v = "*"
	}
	return fmt.Sprintf("%d-%s", k.MarketID, v)
}

// isDynamicVariant reports whether the key addresses the DYNAMIC
// variant long tail — the `od:dynamic_outcomes:` family, fetched one
// key at a time from the per-variant endpoint.
//
// This, and NOT "the key has a variant string", decides which store
// backs the key: storage is split by PROVENANCE, not by key shape.
// Everything the BULK catalog returns — base rows and static variants
// alike (`way:three`, `best_of:5`, `gnr:0to15`, `mr:12`, `st:2p2o`) —
// belongs in the permanent map, because a bulk load is the only thing
// that can repopulate it, and loadOne skips the bulk load whenever the
// locale is already flagged loaded.
//
// Routing static variants into the bounded, TTL'd LRU made them
// permanently unrecoverable: ~12h after the first catalog load every
// one of them expired, the by-id miss short-circuited on the
// loaded-locale flag without refetching, and MarketDescriptionByID
// returned ErrItemNotFoundInCache for the rest of the process
// lifetime — consumers dropped every odds change carrying such a
// market. Only the dynamic family is safe in the LRU, because its
// by-id miss re-fetches from the per-variant endpoint and is not
// gated on loadedLocales at all.
func (k CompositeKey) isDynamicVariant() bool {
	return utils.IsMarketVariantWithDynamicOutcomes(k.Variant)
}

// variantCacheSize bounds the DYNAMIC-variant LRU. Dynamic variants
// form an unbounded long tail — one entry per (marketID, variant)
// tuple, where the variant may encode map/set numbers etc. (NEXT.md §6
// prescribes a bounded LRU, default 5000, for exactly this data).
// Static variants are NOT counted here; see isDynamicVariant.
const variantCacheSize = 5000

// defaultCatalogTTL bounds how long a locale stays flagged "loaded" in
// the bulk-catalog caches — MarketDescriptionCache here, SportCache in
// sportCache.go (which also uses it for its per-sport tournament
// lists).
//
// Without it the flag was permanent for the process lifetime, so each
// catalog was fetched exactly once and never again: entities added or
// renamed upstream stayed invisible until the process restarted, and
// any entry lost from a bounded store could never be restored. For the
// market cache it also gives the static-variant fix a second,
// independent safety net — even a key that somehow goes missing now
// refetches once the window lapses.
//
// 12h matches the entry TTL of the bounded caches and the expiry the
// legacy SDK applied to these catalogs. LocalizedStaticDataCache is
// deliberately NOT in this list: it already refreshes every loaded
// locale from a 24h background ticker, which solves the same staleness
// problem by a different mechanism.
const defaultCatalogTTL = 12 * time.Hour

// variantKey builds the cache key from (marketID, variant). An
// empty-string variant is normalised to "no variant" — NEXT.md §0
// rejects `Some("")` as a meaningful variant, and treating it as
// distinct from `None` here would silently create an unfetchable
// (id, "") cache entry that the API can never populate.
func variantKey(marketID int, variant types.Optional[string]) CompositeKey {
	if v, ok := variant.Get(); ok && v != "" {
		return CompositeKey{MarketID: marketID, Variant: v}
	}
	return CompositeKey{MarketID: marketID}
}

// MarketDescriptionCache stores market descriptions per (marketID, variant)
// composite key. Each entry holds per-locale name/outcome data.
//
// Storage is split by PROVENANCE, not by key shape. Everything the bulk
// catalog returns — plain rows AND static variants — lives in a plain
// map, bounded by the upstream catalog size (hundreds of entries) and
// restorable only by another bulk load. Only the `od:dynamic_outcomes:`
// family, which is an unbounded long tail fetched one key at a time from
// the per-variant endpoint, lives in a bounded expirable LRU
// (variantCacheSize). See CompositeKey.isDynamicVariant for what went
// wrong when the split keyed on "does this key have a variant string".
//
// Concurrent-load safety (v2.24): replaces a single global loadMu —
// which serialized every concurrent loader regardless of key/locale —
// with a singleflight.Group. Bulk catalog loads (including by-id misses,
// which resolve through the bulk endpoint) share the "*|<locale>" key;
// dynamic-variant loads get a per-(id,variant,locale) key. Different
// keys load in parallel; concurrent callers for the same key share a
// single API round-trip.
type MarketDescriptionCache struct {
	apiClient *api.Client
	logger    *log.Logger
	// lifetime is the detach root for singleflight catalog loads —
	// cancelled by Manager.Close so no fetch outlives the owner.
	lifetime context.Context

	mu sync.RWMutex
	// loadedLocales records WHEN each locale's bulk catalog was last
	// fetched (the fetch's start instant, so the age measures data
	// freshness rather than transfer time). A locale counts as loaded
	// only while that timestamp is within catalogTTL — see
	// defaultCatalogTTL for why the mark is not permanent.
	loadedLocales map[types.Locale]time.Time
	// loadingLocales counts bulk-catalog flights currently IN FLIGHT
	// per locale (guarded by mu; a refcount because a generation bump
	// can briefly leave an old-gen and a new-gen flight for one locale
	// alive together). The freshness mark is republished only when a
	// flight COMPLETES, so mid-refresh a locale's mark is expired even
	// though its rows are being merged right now — merge's stale-locale
	// sweep must treat such a locale as fresh, or a concurrent
	// other-locale merge deletes outcome names the in-flight refresh
	// just wrote (see upsert's staleLocale).
	loadingLocales map[types.Locale]int
	// fetchCursor records, per locale, the fetch-START of the newest
	// bulk flight that entered its STORE phase — the flight-level
	// monotonic cursor (guarded by mu; same only-advance discipline as
	// lastRowAt / replaceTournaments / apiStartedAt). Every store a
	// bulk flight makes (rows, reconcile, locale mark) is rejected once
	// a newer-started flight has begun committing.
	//
	// Why it exists: the generation guard runs ONCE at flight-closure
	// entry, and a ClearCacheItem bumps the generation — so a pre-clear
	// (old-gen) flight and a post-clear (new-gen) flight for the SAME
	// locale can be alive together under different singleflight keys.
	// Same-locale serialization — the premise merge's ungated own-locale
	// writes rest on — is false in exactly that window: without the
	// cursor, the older flight finishing LAST overwrote the newer
	// flight's fresh names with stale pre-clear values and re-created
	// markets the newer reconcile had just removed (their keys carry no
	// clear tombstone), all under a fresh locale mark, serving the
	// stale/resurrected rows for up to a catalogTTL.
	//
	// Unlike loadedLocales, Clear/Purge deliberately do NOT reset it:
	// the cursor orders flights, the mark gates read validity. Keeping
	// it across invalidations also keeps merge's cross-locale sweep
	// honest after a Clear (a wiped mark made every locale look stale,
	// briefly re-arming the global sweep the freshness-scoped model
	// removed).
	fetchCursor map[types.Locale]time.Time
	// base holds every description the BULK catalog returns: plain
	// market rows AND static variants. Bounded by the upstream catalog
	// (hundreds of entries) and never expired, because a bulk load is
	// the only thing that can restore an entry here.
	base map[CompositeKey]*LocalizedMarketDescription
	// variants holds ONLY the `od:dynamic_outcomes:` long tail, which is
	// unbounded and individually re-fetchable from the per-variant
	// endpoint — the two properties that make a bounded, expiring store
	// safe for it. See CompositeKey.isDynamicVariant.
	variants *lru.TTL[CompositeKey, *LocalizedMarketDescription]

	// catalogTTL is defaultCatalogTTL, held as a field so tests can
	// compress the window (same pattern as the static-data cache's
	// refresh timings).
	catalogTTL time.Duration

	// Clear-vs-in-flight-load tombstones (same invariant as
	// lru.EventCache), guarded by mu like the stores they gate.
	// Without them, a pre-clear catalog fetch could re-admit the
	// cleared description AND re-mark the locale loaded, silently
	// undoing the public invalidation.
	//
	// Granularity matters: row suppression is PER KEY (clearedAt) plus
	// a purge generation (purgedAt) — a ClearCacheItem for market A
	// must not suppress the rows of every other market in an in-flight
	// bulk response, which would return an empty catalog with no
	// error. Only the loadedLocales mark uses the coarse lastClearAt
	// (max of any clear/purge): the cleared key's row WAS suppressed,
	// so the locale must stay unmarked to force the bulk refetch that
	// restores it.
	clearedAt   map[CompositeKey]time.Time
	lastClearAt time.Time
	purgedAt    time.Time

	// malformed records, per key and per LOCALE, the validation cause of
	// a catalog row that was SKIPPED because it was malformed (e.g. a new
	// market with no <outcomes> block) so no entry was ever created for
	// it. Without it, a by-id lookup for such a market returned
	// ErrItemNotFound when the malformed row was the FIRST locale seen
	// (no entry) but ErrMarketLocaleIncomplete when another locale had
	// created the entry first — the SAME upstream defect classified two
	// different ways depending on load order. Locale granularity lets
	// reconcileBulk prune safely: one locale's catalog omitting the key
	// retracts only THAT locale's evidence, not another locale's live
	// malformed classification. Consulted only on the entry-missing path
	// of MarketDescriptionByID; cleared when a good row later creates the
	// entry, and on ClearCacheItem/Purge. Guarded by mu.
	malformed map[CompositeKey]map[types.Locale]error

	// flightGen is folded into every singleflight key. Tombstones stop
	// a pre-clear flight from repopulating the cache, but without a
	// generation a caller arriving AFTER the clear could still JOIN
	// that flight — and receive a partial catalog or a transient
	// not-found instead of initiating a fresh fetch. Advancing on
	// every ClearCacheItem/Purge detaches future callers from all
	// in-flight loads.
	flightGen atomic.Uint64

	sf singleflight.Group
}

// marketTombstonePruneLen is the clearedAt size beyond which
// ClearCacheItem amortizes a prune; marketTombstoneMaxAge bounds how
// long a tombstone can matter (no in-flight fetch outlives the HTTP
// client timeout; extra slack for scheduling).
const (
	marketTombstonePruneLen = 1024
	marketTombstoneMaxAge   = 2 * time.Minute
)

// lookupLocked returns the entry for key from the appropriate store.
// Caller must hold m.mu (read or write) for the base map; the variant
// LRU is internally synchronized but kept under the same discipline for
// consistency with the upsert path.
func (m *MarketDescriptionCache) lookupLocked(key CompositeKey) (*LocalizedMarketDescription, bool) {
	if key.isDynamicVariant() {
		return m.variants.Get(key)
	}
	entry, ok := m.base[key]
	return entry, ok
}

func (m *MarketDescriptionCache) lookup(key CompositeKey) (*LocalizedMarketDescription, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.lookupLocked(key)
}

// LocalizedMarketDescriptions returns every cached description that contains
// data for the given locale, fetching the locale's full catalog if not yet
// loaded.
func (m *MarketDescriptionCache) LocalizedMarketDescriptions(ctx context.Context, locale types.Locale) (map[CompositeKey]*LocalizedMarketDescription, error) {
	if !m.localeLoaded(locale) {
		if err := m.loadAll(ctx, []types.Locale{locale}); err != nil {
			return nil, err
		}
	}
	return m.collect(func(v *LocalizedMarketDescription) bool { return v.hasLocale(locale) }), nil
}

// MultiLocalizedMarketDescriptions loads every supplied locale into
// the cache (those not already loaded) and returns the catalog,
// keeping only entries that contain EVERY supplied locale.
// Description entries are shared with single-locale calls — each
// returned entry's Names + outcome Names/Descriptions maps include
// every supplied locale, so a Snapshot() emits the full preloaded
// shape. Markets the upstream catalog omits (or that were skipped as
// malformed) in SOME requested locale are filtered out rather than
// returned with partial locale coverage — the pre-fix primary-locale
// gate let such entries through, silently violating the all-locales
// contract; fetch them individually via MarketDescriptionByID, which
// reports the gap as ErrMarketLocaleIncomplete.
func (m *MarketDescriptionCache) MultiLocalizedMarketDescriptions(ctx context.Context, locales []types.Locale) (map[CompositeKey]*LocalizedMarketDescription, error) {
	if len(locales) == 0 {
		return nil, nil
	}
	missing := make([]types.Locale, 0, len(locales))
	for _, l := range locales {
		if !m.localeLoaded(l) {
			missing = append(missing, l)
		}
	}
	if len(missing) > 0 {
		if err := m.loadAll(ctx, missing); err != nil {
			return nil, err
		}
	}
	return m.collect(func(v *LocalizedMarketDescription) bool { return len(v.missingLocales(locales)) == 0 }), nil
}

// collect snapshots both stores into one view map, filtered by keep.
func (m *MarketDescriptionCache) collect(keep func(*LocalizedMarketDescription) bool) map[CompositeKey]*LocalizedMarketDescription {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[CompositeKey]*LocalizedMarketDescription, len(m.base))
	for k, v := range m.base {
		if keep(v) {
			out[k] = v
		}
	}
	for _, k := range m.variants.Keys() {
		if v, ok := m.variants.Peek(k); ok && keep(v) {
			out[k] = v
		}
	}
	return out
}

// MarketDescriptionByID returns the description for (marketID, variant),
// loading missing locales as needed.
func (m *MarketDescriptionCache) MarketDescriptionByID(
	ctx context.Context,
	marketID int,
	variant types.Optional[string],
	locales []types.Locale,
) (*LocalizedMarketDescription, error) {
	key := variantKey(marketID, variant)

	entry, _ := m.lookup(key)

	missing := locales
	staleOnly := false
	if entry != nil {
		missing = entry.missingLocales(locales)
		if len(missing) == 0 && !key.isDynamicVariant() {
			// A bulk-provenance entry is only as fresh as its locale's
			// catalog mark: rows in the permanent base map never expire on
			// their own, so a warm hit that skipped this check served
			// renamed markets/outcomes for the process lifetime to any
			// consumer that only reads by id (the odds-change hot path) —
			// the catalogTTL refresh fired solely on bulk reads and by-id
			// MISSES. Requested locales whose mark expired are treated as
			// missing; loadOne coalesces them onto the shared bulk flight
			// and the refreshed catalog re-marks the locale. Dynamic
			// variants are excluded: their store already expires per entry
			// and their reload path is the per-variant endpoint.
			missing = m.expiredLocales(locales)
			staleOnly = len(missing) > 0
		}
	}
	if len(missing) > 0 {
		if err := m.loadOne(ctx, &marketID, variant, missing); err != nil {
			if staleOnly {
				// The refresh was purely freshness-driven — failing the
				// read here would turn any upstream outage past the TTL
				// window into a hard failure on the odds-change hot path,
				// so prefer serving the stale entry and let the next call
				// retry the refresh (the mark stays expired).
				//
				// But only when the entry is STILL complete: loadOne
				// commits per locale sequentially, so an earlier locale's
				// successful refresh may have reconciled data away (a
				// market or outcome removed upstream) before a later
				// locale's fetch failed — and merge/reconcile mutate the
				// entry IN PLACE, so the pointer captured above reflects
				// those removals. Re-look-up and re-validate coverage;
				// a partially-refreshed entry must surface the fetch
				// error, not masquerade as a complete success.
				if cur, ok := m.lookup(key); ok && len(cur.missingLocales(locales)) == 0 {
					if m.logger != nil {
						m.logger.WithError(err).
							WithField("market", key.String()).
							Warn("cache: catalog refresh failed, serving stale market description")
					}
					return cur, nil
				}
			}
			return nil, err
		}
		entry, _ = m.lookup(key)
		if entry == nil {
			// Loader succeeded but no entry exists for this (id, variant).
			// Distinguish a genuine upstream absence (ErrItemNotFound) from
			// a row that DID arrive but was skipped as malformed
			// (ErrMarketLocaleIncomplete) — the latter must classify the
			// same way regardless of which locale was loaded first.
			m.mu.RLock()
			var cause error
			if perLocale := m.malformed[key]; len(perLocale) > 0 {
				// Prefer a requested locale's cause; any locale's record
				// still proves the market exists-but-broken upstream.
				for _, l := range locales {
					if c, ok := perLocale[l]; ok {
						cause = c
						break
					}
				}
				if cause == nil {
					for _, c := range perLocale {
						cause = c
						break
					}
				}
			}
			m.mu.RUnlock()
			if cause != nil {
				return nil, fmt.Errorf("market description %s malformed in upstream catalog (locales=%v): %w: %w", key, locales, ErrMarketLocaleIncomplete, cause)
			}
			// Wrap ErrItemNotFoundInCache so consumers can errors.Is to
			// distinguish this from a fetch / upsert failure.
			return nil, fmt.Errorf("market description %s missing after load (locales=%v): %w", key, locales, ErrItemNotFoundInCache)
		}
	}
	// Revalidate FULL locale coverage on the returned entry. loadOne
	// skips locales whose catalog is already flagged loaded — correct
	// globally, but this particular market may have been absent (or
	// skipped as malformed) in that locale's catalog, in which case the
	// load "succeeds" without closing this entry's gap. Returning the
	// entry anyway would silently violate the all-requested-locales
	// contract; surface the gap as a typed error instead.
	if still := entry.missingLocales(locales); len(still) > 0 {
		return nil, fmt.Errorf("market description %s: locales %v unavailable in upstream catalog: %w", key, still, ErrMarketLocaleIncomplete)
	}
	return entry, nil
}

// ClearCacheItem evicts a single description AND invalidates every
// loaded-locale marker. Without the locale invalidation, a subsequent
// MarketDescriptions / LocalizedMarketDescriptions / Multi* bulk read
// would see the locale flagged loaded and skip the refetch — so the
// just-cleared description would silently disappear from the bulk view
// until some unrelated path triggered a reload.
//
// Direct by-id reads via MarketDescriptionByID happen to refill on
// their own (the entry-level missingLocales check fires when the entry
// itself is missing), but the bulk-view inconsistency is the bug.
func (m *MarketDescriptionCache) ClearCacheItem(marketID int, variant types.Optional[string]) {
	key := variantKey(marketID, variant)
	m.mu.Lock()
	m.flightGen.Add(1) // detach future callers from in-flight loads
	now := time.Now()
	m.clearedAt[key] = now
	m.lastClearAt = now
	if len(m.clearedAt) > marketTombstonePruneLen {
		cutoff := now.Add(-marketTombstoneMaxAge)
		for k, t := range m.clearedAt {
			if t.Before(cutoff) {
				delete(m.clearedAt, k)
			}
		}
	}
	if key.isDynamicVariant() {
		m.variants.Remove(key)
	} else {
		delete(m.base, key)
	}
	delete(m.malformed, key)
	m.loadedLocales = make(map[types.Locale]time.Time)
	m.mu.Unlock()
}

// Purge clears the entire cache.
func (m *MarketDescriptionCache) Purge() {
	m.mu.Lock()
	m.flightGen.Add(1) // detach future callers from in-flight loads
	now := time.Now()
	m.purgedAt = now
	m.lastClearAt = now
	m.clearedAt = make(map[CompositeKey]time.Time)
	m.malformed = make(map[CompositeKey]map[types.Locale]error)
	m.base = make(map[CompositeKey]*LocalizedMarketDescription)
	m.variants.Purge()
	m.loadedLocales = make(map[types.Locale]time.Time)
	m.mu.Unlock()
}

// localeLoaded reports whether the locale's bulk catalog was fetched
// recently enough to skip a refetch. The mark EXPIRES (catalogTTL): a
// permanent flag meant the catalog was downloaded once per process and
// never refreshed, so upstream additions and renames never landed.
func (m *MarketDescriptionCache) localeLoaded(locale types.Locale) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.localeLoadedLocked(locale)
}

// localeLoadedLocked is localeLoaded for callers already holding m.mu
// (read or write).
func (m *MarketDescriptionCache) localeLoadedLocked(locale types.Locale) bool {
	loadedAt, ok := m.loadedLocales[locale]
	if !ok {
		return false
	}
	return m.catalogTTL <= 0 || time.Since(loadedAt) < m.catalogTTL
}

// localeDataFreshLocked reports whether the locale's newest COMMITTED
// bulk data (fetchCursor) is within catalogTTL. Distinct from
// localeLoadedLocked: the mark gates read validity and is wiped by
// Clear/Purge to force refetches, while the cursor tracks data age and
// survives invalidations — merge's cross-locale sweep judges staleness
// by data age (see upsert's staleLocale). Caller must hold m.mu.
func (m *MarketDescriptionCache) localeDataFreshLocked(locale types.Locale) bool {
	at, ok := m.fetchCursor[locale]
	if !ok {
		return false
	}
	return m.catalogTTL <= 0 || time.Since(at) < m.catalogTTL
}

// expiredLocales returns the requested locales whose catalog mark is
// absent or older than catalogTTL. Used by the warm by-id path so a
// bulk-provenance hit re-loads once its locale's catalog window lapses
// (see MarketDescriptionByID).
func (m *MarketDescriptionCache) expiredLocales(requested []types.Locale) []types.Locale {
	var expired []types.Locale
	for _, l := range requested {
		if !m.localeLoaded(l) {
			expired = append(expired, l)
		}
	}
	return expired
}

// marketCommitGateHook is a test-only seam invoked by a bulk flight
// after it has advanced the fetchCursor (winning the locale) but
// before any store — the window a superseded flight's callers must
// never read across. Production builds leave it nil (same pattern as
// lru.clearForgetHook).
var marketCommitGateHook func(types.Locale)

func (m *MarketDescriptionCache) loadAll(ctx context.Context, locales []types.Locale) error {
	return m.loadOne(ctx, nil, types.None[string](), locales)
}

func (m *MarketDescriptionCache) loadOne(ctx context.Context, marketID *int, variant types.Optional[string], locales []types.Locale) error {
	for _, locale := range locales {
		// Re-check ctx between locales so a multi-locale call cancelled
		// mid-iteration doesn't keep starting detached fetches.
		if err := ctx.Err(); err != nil {
			return err
		}
		// Normalise empty-string variant to "no variant" so the
		// singleflight key matches variantKey()'s normalisation —
		// otherwise concurrent (id, None) and (id, Some("")) callers
		// would race against different sf entries for the same
		// effective load.
		variantStr := ""
		if v, ok := variant.Get(); ok && v != "" {
			variantStr = v
		}

		// singleflight key encodes the load granularity:
		//   "*|<locale>"                — bulk catalog load
		//   "<id>|<variant>|<locale>"   — single-variant dynamic-outcome load
		//
		// Everything that is NOT a dynamic-variant fetch resolves via
		// the BULK catalog endpoint — including by-id misses — so it
		// must share the bulk key and record the locale as loaded.
		// Pre-fix a by-id miss ran the full catalog download under its
		// own narrow "<id>|*|<locale>" key: concurrent misses for
		// different ids duplicated the full download in parallel, and
		// the completed work was never recorded in loadedLocales, so
		// the next bulk call downloaded the catalog yet again.
		isDynamicVariant := marketID != nil && variantStr != "" && utils.IsMarketVariantWithDynamicOutcomes(variantStr)
		loc := locale
		for {
			gen := m.flightGen.Load()
			var sfKey string
			if isDynamicVariant {
				sfKey = fmt.Sprintf("%d|%d|%s|%s", gen, *marketID, variantStr, locale)
			} else {
				sfKey = fmt.Sprintf("%d|*|%s", gen, locale)
			}

			_, err := lru.LoadCoalesced(ctx, m.lifetime, &m.sf, sfKey, func(loadCtx context.Context) (struct{}, error) {
				// Stale-generation guard — see errStaleFlight for the
				// register-vs-clear window this closes.
				if m.flightGen.Load() != gen {
					return struct{}{}, errStaleFlight
				}
				// Snapshot the flight's start BEFORE fetching: upsert and
				// the loadedLocales mark below are suppressed if a Clear
				// lands after this point (see lastClearAt).
				loadStarted := time.Now()
				// Double-check inside the critical region.
				if !isDynamicVariant && m.localeLoaded(loc) {
					return struct{}{}, nil
				}
				if !isDynamicVariant {
					// Register this locale as in flight for the duration of
					// the fetch+merge, so concurrent other-locale merges
					// don't mistake its expired mark for staleness and
					// sweep the rows this flight is writing (see
					// loadingLocales).
					m.mu.Lock()
					m.loadingLocales[loc]++
					m.mu.Unlock()
					defer func() {
						m.mu.Lock()
						if m.loadingLocales[loc]--; m.loadingLocales[loc] <= 0 {
							delete(m.loadingLocales, loc)
						}
						m.mu.Unlock()
					}()
				}
				var (
					descriptions []data.MarketDescription
					err          error
				)
				if isDynamicVariant {
					descriptions, err = m.apiClient.FetchMarketDescriptionsWithDynamicOutcomes(loadCtx, *marketID, variantStr, loc)
				} else {
					descriptions, err = m.apiClient.FetchMarketDescriptions(loadCtx, loc)
				}
				if err != nil {
					return struct{}{}, fmt.Errorf("fetch market description %s locale %s: %w", sfKey, loc, err)
				}

				// Flight-level monotonic gate (see fetchCursor): enter the
				// store phase only if no newer-started flight for this
				// locale has begun committing; advancing the cursor here
				// makes every store of any OLDER flight — including one
				// already mid-commit — reject from now on. A superseded
				// flight returns errStaleFlight so its callers re-register
				// under the current generation and JOIN the winning flight
				// (singleflight completes only after the winner's full
				// commit, mark included). Returning success here instead
				// let the superseded caller re-read MID-commit state — for
				// a cleared key the winner had not yet re-created, that
				// read reported a wrong transient ErrItemNotFound for a
				// market that exists upstream.
				if !isDynamicVariant {
					m.mu.Lock()
					superseded := m.fetchCursor[loc].After(loadStarted)
					if !superseded && loadStarted.After(m.fetchCursor[loc]) {
						m.fetchCursor[loc] = loadStarted
					}
					m.mu.Unlock()
					if superseded {
						return struct{}{}, errStaleFlight
					}
					if marketCommitGateHook != nil {
						marketCommitGateHook(loc)
					}
				}

				// Track every key the response carries (malformed rows
				// included — a broken row still proves the market exists
				// upstream) so the bulk reconcile below only removes
				// markets genuinely absent from the fresh catalog.
				var seen map[CompositeKey]struct{}
				if !isDynamicVariant {
					seen = make(map[CompositeKey]struct{}, len(descriptions))
				}
				for k := range descriptions {
					if seen != nil {
						seen[variantKey(descriptions[k].ID, types.FromPtr(descriptions[k].Variant))] = struct{}{}
					}
					if err := m.upsert(descriptions[k], loc, loadStarted); err != nil {
						// A malformed catalog row must not abort the whole
						// locale load: pre-fix, one description without an
						// <outcomes> block failed the load AFTER partial
						// upserts, the locale was never marked loaded, and
						// every subsequent access refetched the catalog and
						// failed again — the entire market-description
						// surface went unavailable over one bad row.
						if m.logger != nil {
							m.logger.WithError(err).
								WithField("market_id", descriptions[k].ID).
								WithField("locale", string(loc)).
								Warn("cache: skipping malformed market description")
						}
						continue
					}
				}
				if !isDynamicVariant {
					// The bulk response is the complete catalog for this
					// locale: markets it no longer carries must stop being
					// served (the base map never expires on its own, so
					// without this an upstream removal lingered for the
					// process lifetime).
					m.reconcileBulk(loc, seen, loadStarted)
				}

				// Only the bulk fetch counts as fully loading the locale.
				// A single-id dynamic-variant fetch covers exactly that key.
				// Skipped when a Clear raced this load — marking the locale
				// loaded with pre-clear data would make bulk reads skip the
				// refetch the Clear asked for; the next read reloads fresh.
				if !isDynamicVariant {
					m.mu.Lock()
					if !m.lastClearAt.After(loadStarted) && !m.fetchCursor[loc].After(loadStarted) {
						// Timestamped with the fetch's START, not its
						// completion: the freshness window must cover the
						// age of the DATA, not of the transfer. Also
						// suppressed when a newer-started flight owns the
						// locale (fetchCursor) — an older mark must not
						// regress the newer flight's.
						m.loadedLocales[loc] = loadStarted
					}
					// A flight superseded MID-commit must not report
					// success either — its remaining stores were rejected
					// per row, so its callers would re-read the winner's
					// partial state exactly like the store-phase-entry case
					// above. Re-check and hand them to the winner.
					lost := m.fetchCursor[loc].After(loadStarted)
					m.mu.Unlock()
					if lost {
						return struct{}{}, errStaleFlight
					}
				}
				return struct{}{}, nil
			})
			if errors.Is(err, errStaleFlight) {
				continue // re-register under the fresh generation
			}
			if err != nil {
				return err
			}
			break
		}
	}
	return nil
}

// reconcileBulk removes, for one locale, every base entry the fresh
// bulk response no longer carries — the response is the complete
// catalog for that locale, so absence is authoritative removal, not a
// gap. Entries left with no locale at all are dropped from the map
// (mirrors LocalizedStaticDataCache.timerTick's atomic per-locale
// replace; the base map has no TTL of its own, so without this an
// upstream removal was served for the process lifetime — the same
// retention class the tournament-list REPLACE fixed).
//
// Only the base map is touched: the dynamic-variant LRU is fed by the
// per-variant endpoint, whose responses carry no catalog authority.
//
// Suppressed entirely when any invalidation landed after the load
// began — this response's view may predate the clear, and removals
// driven by it are as suspect as its rows; the invalidation already
// reset the locale marks, so the next load reconciles from fresh data.
// Runs under m.mu, atomic with upsert's create+merge (which holds m.mu
// through the merge), so it can never observe — and prune — an entry
// whose first locale is still being populated.
func (m *MarketDescriptionCache) reconcileBulk(locale types.Locale, seen map[CompositeKey]struct{}, loadStarted time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// An EMPTY response carries no removal authority. response_code=OK
	// with zero <market> rows decodes successfully (nil slice, nil
	// error) — an upstream deploy glitch or truncated-but-well-formed
	// body is indistinguishable from a genuinely empty catalog, and a
	// bookmaker catalog with zero markets does not exist in practice.
	// Pruning on it would wipe the ENTIRE base map and, with the locale
	// then marked loaded, serve not-found for every market for a full
	// catalogTTL. Refusing to prune costs at most one catalogTTL of
	// staleness if the catalog ever really emptied. (Pre-replace, the
	// accumulate semantics made an empty response a harmless no-op —
	// this guard restores that property for the pathological case.)
	if len(seen) == 0 {
		if len(m.base) > 0 && m.logger != nil {
			m.logger.WithField("locale", string(locale)).
				Warn("cache: empty market catalog response; keeping cached descriptions (reconcile skipped)")
		}
		return
	}
	if m.lastClearAt.After(loadStarted) || m.purgedAt.After(loadStarted) {
		return
	}
	if m.fetchCursor[locale].After(loadStarted) {
		// A newer-started flight owns this locale now (see fetchCursor);
		// removals driven by this flight's older view are as superseded
		// as its rows.
		return
	}
	for key, entry := range m.base {
		if _, ok := seen[key]; ok {
			continue
		}
		if entry.removeLocale(locale) {
			delete(m.base, key)
		}
	}
	// Malformed records are reconciled the same way, at LOCALE
	// granularity: a market whose only trace was a malformed row (no
	// entry was ever created) and that the fresh catalog no longer
	// carries must classify as ErrItemNotFound, not stay
	// ErrMarketLocaleIncomplete forever — but this locale's catalog
	// omitting the key retracts only THIS locale's evidence. A row
	// malformed in a DIFFERENT locale's catalog keeps its record (and
	// its incomplete classification) until that locale's own refresh
	// says otherwise; a whole-key delete here let an unrelated locale
	// erase live evidence and flip the classification to a definitive
	// not-found.
	for key, perLocale := range m.malformed {
		if _, ok := seen[key]; ok {
			continue
		}
		delete(perLocale, locale)
		if len(perLocale) == 0 {
			delete(m.malformed, key)
		}
	}
}

func (m *MarketDescriptionCache) upsert(description data.MarketDescription, locale types.Locale, loadStarted time.Time) error {
	key := variantKey(description.ID, types.FromPtr(description.Variant))

	m.mu.Lock()
	if m.clearedAt[key].After(loadStarted) || m.purgedAt.After(loadStarted) {
		// A Clear for THIS key (or a Purge) landed after the load
		// started: its data may predate the invalidation. Skip only
		// this row — unrelated markets in the same bulk response are
		// stored normally, so a clear can never empty the catalog.
		m.mu.Unlock()
		return nil
	}
	if !key.isDynamicVariant() && m.fetchCursor[locale].After(loadStarted) {
		// A newer-started bulk flight for this locale has begun
		// committing (see fetchCursor): this row is superseded catalog
		// content. Checked per row — the gate at store-phase entry can't
		// stop a flight that was already mid-commit when the newer one
		// arrived. Dynamic-variant rows are exempt: they come from the
		// per-variant endpoint and the bulk cursor says nothing about
		// them.
		m.mu.Unlock()
		return nil
	}
	if description.Outcomes == nil {
		// Malformed row (no <outcomes> block): record the cause per
		// (key, locale), contribute nothing, and RETRACT this locale's
		// previous contribution from any existing entry. A subsequent
		// read for this locale then classifies as
		// ErrMarketLocaleIncomplete (upstream defect) — via coverage
		// revalidation while the entry survives on other locales, or via
		// the kept evidence once it empties — never ErrItemNotFound
		// (genuine absence) and never a silently partial or stale entry.
		//
		// The retraction is what keeps classification independent of
		// prior cache state: the row IS its locale's newest catalog
		// content (same-locale flights are serialized), and without it a
		// previously well-formed locale whose refreshed row lost its
		// outcomes block kept serving the pre-defect name/outcomes with a
		// NIL error indefinitely — the key stays in the reconcile's
		// `seen` set and the completed load renews the freshness mark, so
		// nothing ever consulted the recorded evidence. The serve-stale
		// fallback deliberately does NOT apply here: that path is for
		// fetch FAILURES, logs at read time, and leaves the mark expired
		// to retry — a malformed row arrives on a SUCCESSFUL load that
		// re-marks the locale, so serving stale would be silent and
		// permanent.
		err := fmt.Errorf("market description %s locale %s: payload missing outcomes block", key, locale)
		perLocale := m.malformed[key]
		if perLocale == nil {
			perLocale = make(map[types.Locale]error, 1)
			m.malformed[key] = perLocale
		}
		perLocale[locale] = err
		if entry, ok := m.lookupLocked(key); ok {
			if entry.removeLocale(locale) {
				// No usable data left in any locale — drop the entry;
				// the by-id entry-missing path serves the kept evidence.
				if key.isDynamicVariant() {
					m.variants.Remove(key)
				} else {
					delete(m.base, key)
				}
			}
		}
		m.mu.Unlock()
		return err
	}
	entry, ok := m.lookupLocked(key)
	if !ok {
		outcomes := make(map[string]*LocalizedOutcomeDescription, len(description.Outcomes.Outcome))
		for _, o := range description.Outcomes.Outcome {
			outcomes[o.ID] = &LocalizedOutcomeDescription{
				name:        make(map[types.Locale]string),
				description: make(map[types.Locale]string),
			}
		}
		entry = &LocalizedMarketDescription{
			id:                     description.ID,
			variant:                description.Variant,
			IncludesOutcomesOfType: description.IncludesOutcomesOfType,
			OutcomeType:            description.OutcomeType,
			outcomes:               outcomes,
			name:                   make(map[types.Locale]string),
			groups:                 splitGroups(description.Groups),
		}
		if key.isDynamicVariant() {
			m.variants.Add(key, entry)
		} else {
			m.base[key] = entry
		}
	}
	// A well-formed row backs this key IN THIS LOCALE — drop this
	// locale's malformed record so a later good load reclassifies the
	// market as present. Only this locale's: another locale's row may
	// still be malformed upstream, and a whole-key delete here let a
	// valid de row erase en's live malformed evidence — after which a
	// de-side removal reconciled the entry away and en reads reported a
	// definitive ErrItemNotFound for a market the en catalog still
	// carries (broken).
	if perLocale := m.malformed[key]; perLocale != nil {
		delete(perLocale, locale)
		if len(perLocale) == 0 {
			delete(m.malformed, key)
		}
	}
	// staleLocale scopes merge's cross-locale outcome removal to locales
	// whose bulk-catalog mark has expired. Only bulk-provenance rows
	// consult the marks: dynamic-variant entries live in a TTL'd store
	// whose whole-entry expiry already bounds their staleness, and the
	// bulk marks say nothing about per-variant data.
	//
	// A locale with a flight currently IN PROGRESS counts as fresh even
	// though its mark is expired — the mark is republished only when the
	// flight completes, so mid-refresh its rows are the very opposite of
	// stale. Without this, an en merge interleaving with a de refresh
	// (de merged market X, hasn't re-marked yet) swept X's just-written
	// de outcome names; de then finished and marked itself fresh without
	// revisiting X, and both locales served the silently truncated set
	// for a full catalogTTL.
	//
	// Freshness is judged by the fetchCursor, not loadedLocales:
	// Clear/Purge wipe the marks (to force refetches) but not the
	// cursor, and a wiped mark made every locale look stale to the very
	// next single-locale load — briefly re-arming the global
	// last-row-wins sweep the freshness-scoped model removed. The
	// cursor records exactly what the sweep needs: how old each
	// locale's newest committed data is.
	var staleLocale func(types.Locale) bool
	if !key.isDynamicVariant() {
		staleLocale = func(l types.Locale) bool {
			return m.loadingLocales[l] == 0 && !m.localeDataFreshLocked(l)
		}
	}
	// Merge UNDER m.mu (it takes the entry's own lock; the m.mu →
	// entry.mu order matches collect/reconcileBulk — and staleLocale
	// reads loadedLocales, which m.mu guards). Merging after the
	// unlock — the previous shape — left a window where a freshly
	// created entry sat in the map with zero locales; reconcileBulk
	// (which prunes empty entries atomically under m.mu) could then
	// delete it while this row's merge was still pending, silently
	// losing the row. Bulk loads are rare (once per catalogTTL), so the
	// added hold time is irrelevant.
	entry.merge(description, locale, loadStarted, staleLocale)
	m.mu.Unlock()
	return nil
}

// splitGroups parses the pipe-separated groups attribute. An omitted or
// empty attribute yields an empty result — strings.Split("", "|")
// returns [""], which fabricated a phantom empty group on every market
// without groups (visible in MarketDescription.Groups and in any
// group-membership check against ""). Empty components from stray
// separators are dropped for the same reason.
func splitGroups(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, "|")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func newMarketDescriptionCache(lifeCtx context.Context, client *api.Client, logger *log.Logger) *MarketDescriptionCache {
	return &MarketDescriptionCache{
		apiClient:      client,
		logger:         logger,
		lifetime:       lifeCtx,
		loadedLocales:  make(map[types.Locale]time.Time),
		loadingLocales: make(map[types.Locale]int),
		fetchCursor:    make(map[types.Locale]time.Time),
		catalogTTL:     defaultCatalogTTL,
		base:           make(map[CompositeKey]*LocalizedMarketDescription),
		clearedAt:      make(map[CompositeKey]time.Time),
		malformed:      make(map[CompositeKey]map[types.Locale]error),
		variants: lru.NewTTL[CompositeKey, *LocalizedMarketDescription](
			variantCacheSize, nil, lru.DefaultEventCacheTTL),
	}
}

// LocalizedMarketDescription stores per-(market, variant) description data
// across multiple locales. mu guards all fields.
type LocalizedMarketDescription struct {
	mu sync.RWMutex

	id                     int
	variant                *string
	IncludesOutcomesOfType *string
	OutcomeType            *string
	outcomes               map[string]*LocalizedOutcomeDescription
	specifiers             []types.Specifier
	name                   map[types.Locale]string
	groups                 []string

	// lastRowAt is the fetch-start instant of the newest APPLIED
	// well-formed row — the monotonic cursor for merge's cross-locale
	// mutations (outcome-set additions, the stale-locale sweep, and the
	// locale-independent metadata). Same only-advance discipline as
	// LocalizedSport.replaceTournaments and the match-status cache's
	// apiStartedAt; see merge for the race it closes.
	lastRowAt time.Time
}

func (d *LocalizedMarketDescription) hasLocale(locale types.Locale) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.coversLocaleLocked(locale)
}

// missingLocales reports which of the requested locales this entry does
// NOT fully cover. Coverage means the market name AND every known
// outcome's name — a name-only check let outcome-level gaps through
// (locale A's payload carrying an outcome that locale B's omits), so an
// entry could pass validation while one of its outcomes silently lacked
// a requested locale, violating the all-requested-locales contract on
// MarketDescription.Outcomes. Like a market-level gap, an upstream
// catalog that genuinely omits an outcome translation surfaces as
// ErrMarketLocaleIncomplete after one refetch (by-id path) or filters
// the entry from bulk views — never a partially-localized result.
func (d *LocalizedMarketDescription) missingLocales(locales []types.Locale) []types.Locale {
	d.mu.RLock()
	defer d.mu.RUnlock()
	var missing []types.Locale
	for _, l := range locales {
		if !d.coversLocaleLocked(l) {
			missing = append(missing, l)
		}
	}
	return missing
}

// removeLocale drops every trace of one locale from the entry (market
// name, outcome names and descriptions), deleting outcomes left with no
// locale names at all. Reports whether the entry is now unusable and
// should be dropped by the caller: no market name in any locale, OR no
// outcomes left. The outcome condition matters — coversLocaleLocked
// iterates the outcome map, so an entry whose outcomes this removal
// just emptied (names in other locales only ever came from rows that
// carried no outcome data) would otherwise cover every named locale
// VACUOUSLY, and by-id reads would serve an outcome-less market as a
// valid description. Used by reconcileBulk for markets absent from a
// fresh bulk response; a market genuinely still present upstream is
// re-created by that locale's next load.
func (d *LocalizedMarketDescription) removeLocale(locale types.Locale) (empty bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.name, locale)
	for id, lo := range d.outcomes {
		lo.mu.Lock()
		delete(lo.name, locale)
		delete(lo.description, locale)
		gone := len(lo.name) == 0
		lo.mu.Unlock()
		if gone {
			delete(d.outcomes, id)
		}
	}
	return len(d.name) == 0 || len(d.outcomes) == 0
}

// coversLocaleLocked reports full coverage of one locale: market name +
// every outcome name. Outcome DESCRIPTIONS are deliberately excluded —
// the upstream catalog marks them optional (data shape, not a coverage
// gap). Caller must hold d.mu; takes each outcome's lock in the same
// d.mu→outcome.mu order merge and Snapshot use.
//
// An entry with NO outcomes covers nothing: the loop below would pass
// vacuously, serving an outcome-less market as a valid description.
// The zero-outcome shape is reachable without reconciliation (whose
// removeLocale applies the same emptiness test and drops the entry) —
// a well-formed row whose <outcomes> container carries zero <outcome>
// children clears the nil-block malformed guard, and merge's sweep
// then empties a single-locale entry's outcome map while its name
// survives. Such a market is unusable for odds resolution either way;
// reads classify it as ErrMarketLocaleIncomplete.
func (d *LocalizedMarketDescription) coversLocaleLocked(locale types.Locale) bool {
	if len(d.outcomes) == 0 {
		return false
	}
	if _, ok := d.name[locale]; !ok {
		return false
	}
	for _, o := range d.outcomes {
		o.mu.RLock()
		_, ok := o.name[locale]
		o.mu.RUnlock()
		if !ok {
			return false
		}
	}
	return true
}

// merge folds one freshly fetched catalog row into the entry.
//
// REPLACE, not accumulate (mirrors LocalizedSport.replaceTournaments
// and LocalizedStaticDataCache.timerTick): the row is authoritative for
// its OWN locale — its strings, and which outcomes exist in that locale
// — and for the locale-independent metadata it carries.
//
// For OTHER locales the row's outcome-set authority is FRESHNESS-
// SCOPED: an outcome the row omits additionally loses every locale
// whose catalog mark has expired (staleLocale), because that contrary
// evidence is older than the freshness horizon; an outcome left with no
// locale names is dropped. This sits between two wrong extremes, both
// previously shipped:
//
//   - pure accumulate: outcomes removed upstream lingered forever and
//     poisoned coverage for every locale loaded after the removal
//     (by-id returned ErrMarketLocaleIncomplete for the rest of the
//     process lifetime);
//   - last-row-wins set: a locale whose catalog TEMPORARILY omitted an
//     outcome deleted other locales' still-fresh data globally, and a
//     multi-locale request then passed coverage with silently
//     truncated outcomes instead of reporting the documented
//     ErrMarketLocaleIncomplete.
//
// With freshness-scoped removal, fresh disagreement between locales
// keeps both sides' data and surfaces as ErrMarketLocaleIncomplete; a
// zombie outcome kept alive only by locales nobody refreshes is swept
// as soon as their marks lapse — bounded by one catalogTTL.
//
// A malformed row (no <outcomes> block) never reaches merge — upsert
// records it per (key, locale) and contributes nothing (the earlier
// name-only merge left entries that could outlive their outcomes and
// then cover locales vacuously); the Outcomes guard below is defensive.
//
// Cross-locale mutations are additionally MONOTONIC on loadStarted
// (the fetch-start instant of the row's flight, tracked in lastRowAt):
// different locales load under different singleflight keys and can run
// concurrently, so an earlier-STARTED response finishing LAST could
// otherwise sweep other locales' newer data or reinstall older
// metadata — and with both locale marks then fresh, the rollback stood
// for a full catalogTTL. A stale-started row still applies EVERYTHING
// about its own locale — strings, own-locale removals, AND own-locale
// outcome additions (same-locale flights are serialized by
// singleflight, so per-locale data is always that locale's newest) —
// but it cannot run the cross-locale sweep or touch metadata.
//
// Own-locale additions are deliberately NOT gated: gating them made
// the outcome set depend on completion ORDER — with en carrying {1,2}
// and de carrying {1}, en-finishes-last silently dropped outcome 2
// (both locales then passed coverage with truncated data), while
// en-finishes-first kept it and correctly reported the disagreement as
// ErrMarketLocaleIncomplete. Recording own-locale membership always is
// faithful to that locale's newest row, so the same responses now
// yield the same typed outcome in either completion order. The cost is
// that a stale-started row can re-add, WITH ONLY ITS OWN locale's
// name, an outcome a newer row removed — which reads as a fresh
// disagreement (typed error, never silent truncation) and is swept by
// the freshness sweep once that locale's mark lapses.
func (d *LocalizedMarketDescription) merge(description data.MarketDescription, locale types.Locale, loadStarted time.Time, staleLocale func(types.Locale) bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if description.Outcomes != nil {
		newest := loadStarted.After(d.lastRowAt)
		fresh := make(map[string]struct{}, len(description.Outcomes.Outcome))
		for _, outcome := range description.Outcomes.Outcome {
			fresh[outcome.ID] = struct{}{}
			lo, ok := d.outcomes[outcome.ID]
			if !ok {
				// Add unconditionally — own-locale set membership is not
				// gated on newest; see the completion-order note above.
				lo = &LocalizedOutcomeDescription{
					name:        make(map[types.Locale]string),
					description: make(map[types.Locale]string),
				}
				d.outcomes[outcome.ID] = lo
			}
			lo.mu.Lock()
			lo.name[locale] = outcome.Name
			if outcome.Description != nil {
				lo.description[locale] = *outcome.Description
			} else {
				// Description dropped upstream: an absent attribute must
				// not leave the old localized text behind.
				delete(lo.description, locale)
			}
			lo.mu.Unlock()
		}
		// Outcomes the fresh row no longer carries: always drop this
		// row's own locale; drop other locales only when their catalog
		// mark is stale (see the freshness-scoped contract above) AND
		// this row is the newest applied — a stale-started row has no
		// cross-locale authority.
		for id, lo := range d.outcomes {
			if _, ok := fresh[id]; ok {
				continue
			}
			lo.mu.Lock()
			delete(lo.name, locale)
			delete(lo.description, locale)
			if newest && staleLocale != nil {
				for l := range lo.name {
					if staleLocale(l) {
						delete(lo.name, l)
						delete(lo.description, l)
					}
				}
			}
			gone := len(lo.name) == 0
			lo.mu.Unlock()
			if gone {
				delete(d.outcomes, id)
			}
		}

		if newest {
			// Locale-independent metadata: the newest row is authoritative.
			d.groups = splitGroups(description.Groups)
			d.IncludesOutcomesOfType = description.IncludesOutcomesOfType
			d.OutcomeType = description.OutcomeType
			var specifiers []types.Specifier
			if description.Specifiers != nil && len(description.Specifiers.Specifier) > 0 {
				specifiers = make([]types.Specifier, 0, len(description.Specifiers.Specifier))
				for _, s := range description.Specifiers.Specifier {
					specifiers = append(specifiers, types.Specifier{Name: s.Name, Type: s.Type})
				}
			}
			d.specifiers = specifiers // nil clears a set emptied upstream
			d.lastRowAt = loadStarted
		}
	}
	d.name[locale] = description.Name
}

// Snapshot projects the cached entry into a types.MarketDescription
// value (data-copy under the entry's read lock).
func (d *LocalizedMarketDescription) Snapshot() types.MarketDescription {
	d.mu.RLock()
	defer d.mu.RUnlock()

	names := make(map[types.Locale]string, len(d.name))
	for k, v := range d.name {
		names[k] = v
	}

	outcomes := make([]types.OutcomeDescription, 0, len(d.outcomes))
	for id, oc := range d.outcomes {
		oc.mu.RLock()
		ocNames := make(map[types.Locale]string, len(oc.name))
		for k, v := range oc.name {
			ocNames[k] = v
		}
		ocDesc := make(map[types.Locale]string, len(oc.description))
		for k, v := range oc.description {
			ocDesc[k] = v
		}
		oc.mu.RUnlock()
		outcomes = append(outcomes, types.OutcomeDescription{
			ID:           id,
			Names:        ocNames,
			Descriptions: ocDesc,
		})
	}
	// Deterministic public ordering: d.outcomes is a map, so the
	// projection otherwise reshuffles between identical calls (see
	// sortURNs for the class rationale).
	slices.SortFunc(outcomes, func(a, b types.OutcomeDescription) int {
		return cmp.Compare(a.ID, b.ID)
	})

	specifiers := make([]types.Specifier, len(d.specifiers))
	copy(specifiers, d.specifiers)

	groups := make([]string, len(d.groups))
	copy(groups, d.groups)

	// Pointer-typed string fields (Variant, IncludesOutcomesOfType,
	// OutcomeType) migrated to Optional[string] in v2.28 — value
	// semantics replace the v2.25 clonePtr workaround.
	return types.MarketDescription{
		ID:                     d.id,
		Names:                  names,
		Variant:                types.FromPtr(d.variant),
		IncludesOutcomesOfType: types.FromPtr(d.IncludesOutcomesOfType),
		OutcomeType:            types.FromPtr(d.OutcomeType),
		Outcomes:               outcomes,
		Specifiers:             specifiers,
		Groups:                 groups,
	}
}

// LocalizedOutcomeDescription holds per-locale outcome data.
type LocalizedOutcomeDescription struct {
	mu          sync.RWMutex
	name        map[types.Locale]string
	description map[types.Locale]string
}

// LocalizedName returns the cached outcome name for a locale.
func (l *LocalizedOutcomeDescription) LocalizedName(locale types.Locale) *string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	v, ok := l.name[locale]
	if !ok {
		return nil
	}
	return &v
}
