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

// marketCatalogTTL bounds how long a locale stays flagged "loaded".
//
// Without it the flag was permanent for the process lifetime, so the
// bulk catalog was fetched exactly once and never again: markets added
// or renamed upstream stayed invisible until restart, and any entry
// lost from a bounded store could never be restored. It also gives the
// static-variant fix a second, independent safety net — even a key that
// somehow goes missing now refetches once the window lapses.
//
// 12h matches the entry TTL of the bounded caches and the expiry the
// legacy SDK applied to this catalog.
const marketCatalogTTL = 12 * time.Hour

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
	// marketCatalogTTL for why the mark is not permanent.
	loadedLocales map[types.Locale]time.Time
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

	// catalogTTL is marketCatalogTTL, held as a field so tests can
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

	// malformed records, per key, the validation cause of a catalog row
	// that was SKIPPED because it was malformed (e.g. a new market with no
	// <outcomes> block) so no entry was ever created for it. Without it, a
	// by-id lookup for such a market returned ErrItemNotFound when the
	// malformed row was the FIRST locale seen (no entry) but
	// ErrMarketLocaleIncomplete when another locale had created the entry
	// first — the SAME upstream defect classified two different ways
	// depending on load order. Consulted only on the entry-missing path of
	// MarketDescriptionByID; cleared when a good row later creates the
	// entry, and on ClearCacheItem/Purge. Guarded by mu.
	malformed map[CompositeKey]error

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
	if entry != nil {
		missing = entry.missingLocales(locales)
	}
	if len(missing) > 0 {
		if err := m.loadOne(ctx, &marketID, variant, missing); err != nil {
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
			cause := m.malformed[key]
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
	m.malformed = make(map[CompositeKey]error)
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
	loadedAt, ok := m.loadedLocales[locale]
	if !ok {
		return false
	}
	return m.catalogTTL <= 0 || time.Since(loadedAt) < m.catalogTTL
}

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

				for k := range descriptions {
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

				// Only the bulk fetch counts as fully loading the locale.
				// A single-id dynamic-variant fetch covers exactly that key.
				// Skipped when a Clear raced this load — marking the locale
				// loaded with pre-clear data would make bulk reads skip the
				// refetch the Clear asked for; the next read reloads fresh.
				if !isDynamicVariant {
					m.mu.Lock()
					if !m.lastClearAt.After(loadStarted) {
						// Timestamped with the fetch's START, not its
						// completion: the freshness window must cover the
						// age of the DATA, not of the transfer.
						m.loadedLocales[loc] = loadStarted
					}
					m.mu.Unlock()
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
	entry, ok := m.lookupLocked(key)
	if !ok {
		if description.Outcomes == nil {
			// Malformed FIRST-locale row: no entry gets created. Record
			// the cause so a subsequent by-id miss reports
			// ErrMarketLocaleIncomplete (upstream defect) rather than
			// ErrItemNotFound (genuine absence) — the same classification
			// the entry-exists path already yields, independent of which
			// locale was loaded first.
			err := fmt.Errorf("market description %s locale %s: payload missing outcomes block", key, locale)
			m.malformed[key] = err
			m.mu.Unlock()
			return err
		}
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
	// A well-formed row now backs this key — drop any malformed tombstone
	// so a later good load reclassifies the market as present.
	delete(m.malformed, key)
	m.mu.Unlock()

	entry.merge(description, locale)
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
		apiClient:     client,
		logger:        logger,
		lifetime:      lifeCtx,
		loadedLocales: make(map[types.Locale]time.Time),
		catalogTTL:    marketCatalogTTL,
		base:          make(map[CompositeKey]*LocalizedMarketDescription),
		clearedAt:     make(map[CompositeKey]time.Time),
		malformed:     make(map[CompositeKey]error),
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

// coversLocaleLocked reports full coverage of one locale: market name +
// every outcome name. Outcome DESCRIPTIONS are deliberately excluded —
// the upstream catalog marks them optional (data shape, not a coverage
// gap). Caller must hold d.mu; takes each outcome's lock in the same
// d.mu→outcome.mu order merge and Snapshot use.
func (d *LocalizedMarketDescription) coversLocaleLocked(locale types.Locale) bool {
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

func (d *LocalizedMarketDescription) merge(description data.MarketDescription, locale types.Locale) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if description.Outcomes != nil {
		for _, outcome := range description.Outcomes.Outcome {
			lo, ok := d.outcomes[outcome.ID]
			if !ok {
				// New outcome on a fresh fetch — add it.
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
			}
			lo.mu.Unlock()
		}
	}
	d.name[locale] = description.Name

	if description.Specifiers != nil {
		specifiers := make([]types.Specifier, 0, len(description.Specifiers.Specifier))
		for _, s := range description.Specifiers.Specifier {
			specifiers = append(specifiers, types.Specifier{Name: s.Name, Type: s.Type})
		}
		if len(specifiers) > 0 {
			d.specifiers = specifiers
		}
	}
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
