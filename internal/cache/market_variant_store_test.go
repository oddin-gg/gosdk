package cache

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

// The bulk catalog as upstream actually serves it: plain rows plus
// STATIC variants (way:*, best_of:*, gnr:*, …). The live test-env
// catalog carries 229 rows, 47 of them static variants and zero
// `od:dynamic_outcomes:` ones — dynamic variants come exclusively from
// the per-variant endpoint.
const bulkCatalogWithStaticVariants = `<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="1" name="1x2" variant="way:three">
    <outcomes><outcome id="1" name="home"/><outcome id="2" name="draw"/><outcome id="3" name="away"/></outcomes>
  </market>
  <market id="4" name="Correct score" variant="best_of:5">
    <outcomes><outcome id="1" name="3:0"/><outcome id="2" name="3:1"/></outcomes>
  </market>
  <market id="9" name="Plain">
    <outcomes><outcome id="1" name="o1"/></outcomes>
  </market>
</market_descriptions>`

// marketSrv serves body (swappable) and counts bulk vs per-variant hits.
type marketSrv struct {
	*httptest.Server
	body        atomic.Pointer[string]
	bulkHits    atomic.Int64
	variantHits atomic.Int64
}

func newMarketSrv(t *testing.T, body string) *marketSrv {
	t.Helper()
	s := &marketSrv{}
	s.body.Store(&body)
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/variants/") {
			s.variantHits.Add(1)
		} else if strings.Contains(r.URL.Path, "/markets") {
			s.bulkHits.Add(1)
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, *s.body.Load())
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *marketSrv) serve(body string) { s.body.Store(&body) }

func newMarketCacheForTest(t *testing.T, s *marketSrv) (*MarketDescriptionCache, context.Context) {
	t.Helper()
	mc := newMarketDescriptionCache(t.Context(), newAPIClientForTest(t, s.Server), log.New(nil))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	return mc, ctx
}

func (m *MarketDescriptionCache) baseLen() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.base)
}

func (m *MarketDescriptionCache) baseHas(key CompositeKey) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.base[key]
	return ok
}

// --- A: storage is split by provenance, not by key shape ---------------

// TestMarketDescriptionCache_StaticVariantStoredInBaseStore pins the
// routing rule: everything the BULK catalog returns lands in the
// permanent map, static variants included. Pre-fix they went to the
// bounded, TTL'd LRU purely because their key carried a variant string.
func TestMarketDescriptionCache_StaticVariantStoredInBaseStore(t *testing.T) {
	srv := newMarketSrv(t, bulkCatalogWithStaticVariants)
	mc, ctx := newMarketCacheForTest(t, srv)

	if _, err := mc.MarketDescriptionByID(ctx, 1, types.Some("way:three"), []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("initial lookup: %v", err)
	}
	if got := mc.variants.Len(); got != 0 {
		t.Fatalf("dynamic-variant LRU holds %d entries, want 0 (no dynamic variants in the bulk catalog)", got)
	}
	if got := mc.baseLen(); got != 3 {
		t.Fatalf("base store holds %d entries, want 3 (2 static variants + 1 plain row)", got)
	}
	for _, key := range []CompositeKey{
		{MarketID: 1, Variant: "way:three"},
		{MarketID: 4, Variant: "best_of:5"},
		{MarketID: 9},
	} {
		if !mc.baseHas(key) {
			t.Fatalf("base store missing %s", key)
		}
	}
}

// TestMarketDescriptionCache_StaticVariantSurvivesVariantStoreLoss is
// the regression for the reported production defect: kollector-mq
// dropped every odds change carrying markets 1/way:three, 4/best_of:*,
// 130/mr:12, 157/gnr:0to15, 163/st:2p2o … roughly 12h after each
// deploy.
//
// Pre-fix sequence: the bulk load put the static variant in the 12h-TTL
// LRU; the entry expired; the by-id miss called loadOne, which
// short-circuited on the already-flagged locale WITHOUT refetching; the
// entry stayed absent and MarketDescriptionByID returned
// ErrItemNotFoundInCache for the rest of the process lifetime.
//
// Purging the LRU models both triggers (TTL expiry and eviction
// pressure) — post-fix the entry is not in that store to begin with.
func TestMarketDescriptionCache_StaticVariantSurvivesVariantStoreLoss(t *testing.T) {
	srv := newMarketSrv(t, bulkCatalogWithStaticVariants)
	mc, ctx := newMarketCacheForTest(t, srv)

	if _, err := mc.MarketDescriptionByID(ctx, 1, types.Some("way:three"), []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("initial lookup: %v", err)
	}
	mc.variants.Purge()

	entry, err := mc.MarketDescriptionByID(ctx, 1, types.Some("way:three"), []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("lookup after variant-store loss: %v (pre-fix: permanent ErrItemNotFoundInCache)", err)
	}
	snap := entry.Snapshot()
	if v, ok := snap.Variant.Get(); !ok || v != "way:three" {
		t.Fatalf("variant = %v, want way:three", snap.Variant)
	}
	if len(snap.Outcomes) != 3 {
		t.Fatalf("outcomes = %d, want 3", len(snap.Outcomes))
	}
	if got := srv.bulkHits.Load(); got != 1 {
		t.Fatalf("bulk hits = %d, want 1 (entry must be served from the permanent store, not refetched)", got)
	}
}

// TestMarketDescriptionCache_BulkViewKeepsStaticVariants pins the
// quieter half of the same defect: the bulk views never errored, they
// just SHRANK. Once the LRU dropped the static variants, collect()
// stopped listing them and the public catalog silently lost those
// markets with no signal at all.
func TestMarketDescriptionCache_BulkViewKeepsStaticVariants(t *testing.T) {
	srv := newMarketSrv(t, bulkCatalogWithStaticVariants)
	mc, ctx := newMarketCacheForTest(t, srv)

	before, err := mc.LocalizedMarketDescriptions(ctx, types.EnLocale)
	if err != nil {
		t.Fatalf("initial bulk read: %v", err)
	}
	if len(before) != 3 {
		t.Fatalf("bulk view = %d entries, want 3", len(before))
	}
	mc.variants.Purge()

	after, err := mc.LocalizedMarketDescriptions(ctx, types.EnLocale)
	if err != nil {
		t.Fatalf("bulk read after variant-store loss: %v", err)
	}
	if len(after) != 3 {
		t.Fatalf("bulk view = %d entries, want 3 (pre-fix the static variants silently vanished)", len(after))
	}
}

// TestMarketDescriptionCache_ClearStaticVariantEvicts guards the third
// routing site: ClearCacheItem must delete from the same store upsert
// wrote to, or a public invalidation would silently no-op.
func TestMarketDescriptionCache_ClearStaticVariantEvicts(t *testing.T) {
	srv := newMarketSrv(t, bulkCatalogWithStaticVariants)
	mc, ctx := newMarketCacheForTest(t, srv)

	if _, err := mc.MarketDescriptionByID(ctx, 1, types.Some("way:three"), []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("initial lookup: %v", err)
	}
	if !mc.baseHas(CompositeKey{MarketID: 1, Variant: "way:three"}) {
		t.Fatal("static variant not in the base store before clear")
	}
	mc.ClearCacheItem(1, types.Some("way:three"))
	if mc.baseHas(CompositeKey{MarketID: 1, Variant: "way:three"}) {
		t.Fatal("cleared static variant still present in the base store")
	}
	// Clear resets the loaded-locale marks, so the next read refetches.
	if _, err := mc.MarketDescriptionByID(ctx, 1, types.Some("way:three"), []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("lookup after clear: %v", err)
	}
	if got := srv.bulkHits.Load(); got != 2 {
		t.Fatalf("bulk hits = %d, want 2 (clear must force a refetch)", got)
	}
}

// TestMarketDescriptionCache_DynamicVariantStaysInBoundedLRU is the
// over-correction guard: the unbounded `od:dynamic_outcomes:` tail must
// NOT migrate into the permanent map, or the cache grows without limit.
func TestMarketDescriptionCache_DynamicVariantStaysInBoundedLRU(t *testing.T) {
	const variant = "od:dynamic_outcomes:123"
	srv := newMarketSrv(t, `<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="7" name="Dynamic" variant="`+variant+`">
    <outcomes><outcome id="1" name="o1"/></outcomes>
  </market>
</market_descriptions>`)
	mc, ctx := newMarketCacheForTest(t, srv)

	if _, err := mc.MarketDescriptionByID(ctx, 7, types.Some(variant), []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("dynamic lookup: %v", err)
	}
	if got := mc.variants.Len(); got != 1 {
		t.Fatalf("dynamic-variant LRU holds %d entries, want 1", got)
	}
	if got := mc.baseLen(); got != 0 {
		t.Fatalf("base store holds %d entries, want 0 (dynamic tail must stay bounded)", got)
	}
}

// TestMarketDescriptionCache_DynamicVariantRefetchedAfterEviction pins
// why the LRU is safe for the dynamic family specifically: its by-id
// miss re-fetches from the per-variant endpoint and is not gated on the
// loaded-locale flag, so eviction costs one round-trip, not the entry.
func TestMarketDescriptionCache_DynamicVariantRefetchedAfterEviction(t *testing.T) {
	const variant = "od:dynamic_outcomes:123"
	srv := newMarketSrv(t, `<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="7" name="Dynamic" variant="`+variant+`">
    <outcomes><outcome id="1" name="o1"/></outcomes>
  </market>
</market_descriptions>`)
	mc, ctx := newMarketCacheForTest(t, srv)

	if _, err := mc.MarketDescriptionByID(ctx, 7, types.Some(variant), []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("dynamic lookup: %v", err)
	}
	mc.variants.Purge()

	if _, err := mc.MarketDescriptionByID(ctx, 7, types.Some(variant), []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("dynamic lookup after eviction: %v", err)
	}
	if got := srv.variantHits.Load(); got != 2 {
		t.Fatalf("per-variant endpoint hits = %d, want 2 (eviction must trigger a refetch)", got)
	}
}

// TestMarketDescriptionCache_AbsentMarketStillNotFound checks the fix
// did not make genuine absence undetectable, AND that a repeated miss
// does not re-download the catalog on every call — the refetch-storm
// hazard that ruled out "just refetch on any miss" as the fix.
func TestMarketDescriptionCache_AbsentMarketStillNotFound(t *testing.T) {
	srv := newMarketSrv(t, bulkCatalogWithStaticVariants)
	mc, ctx := newMarketCacheForTest(t, srv)

	for i := range 3 {
		_, err := mc.MarketDescriptionByID(ctx, 999, types.Some("way:three"), []types.Locale{types.EnLocale})
		if !errors.Is(err, ErrItemNotFoundInCache) {
			t.Fatalf("call %d: err = %v, want ErrItemNotFoundInCache", i, err)
		}
	}
	if got := srv.bulkHits.Load(); got != 1 {
		t.Fatalf("bulk hits = %d, want 1 (a repeated miss must not re-download the catalog)", got)
	}
}

// --- C: the loaded-locale mark expires ---------------------------------

// TestMarketDescriptionCache_CatalogRefetchedAfterTTL pins the catalog
// refresh. Pre-fix loadedLocales was permanent for the process
// lifetime: the bulk catalog was downloaded exactly once, so markets
// added or renamed upstream stayed invisible until restart.
func TestMarketDescriptionCache_CatalogRefetchedAfterTTL(t *testing.T) {
	srv := newMarketSrv(t, bulkCatalogWithStaticVariants)
	mc, ctx := newMarketCacheForTest(t, srv)
	mc.catalogTTL = 50 * time.Millisecond

	if _, err := mc.LocalizedMarketDescriptions(ctx, types.EnLocale); err != nil {
		t.Fatalf("initial bulk read: %v", err)
	}

	// Upstream renames a market and adds a new one.
	srv.serve(`<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="1" name="Match winner" variant="way:three">
    <outcomes><outcome id="1" name="home"/><outcome id="2" name="draw"/><outcome id="3" name="away"/></outcomes>
  </market>
  <market id="4" name="Correct score" variant="best_of:5">
    <outcomes><outcome id="1" name="3:0"/><outcome id="2" name="3:1"/></outcomes>
  </market>
  <market id="9" name="Plain"><outcomes><outcome id="1" name="o1"/></outcomes></market>
  <market id="12" name="Brand new"><outcomes><outcome id="1" name="o1"/></outcomes></market>
</market_descriptions>`)
	time.Sleep(80 * time.Millisecond)

	all, err := mc.LocalizedMarketDescriptions(ctx, types.EnLocale)
	if err != nil {
		t.Fatalf("bulk read after catalog TTL: %v", err)
	}
	if got := srv.bulkHits.Load(); got != 2 {
		t.Fatalf("bulk hits = %d, want 2 (the loaded-locale mark must expire)", got)
	}
	if len(all) != 4 {
		t.Fatalf("bulk view = %d entries, want 4 (upstream addition must land)", len(all))
	}
	if got := all[CompositeKey{MarketID: 1, Variant: "way:three"}].Snapshot().Names[types.EnLocale]; got != "Match winner" {
		t.Fatalf("market 1 name = %q, want %q (upstream rename must land)", got, "Match winner")
	}
}

// TestMarketDescriptionCache_CatalogNotRefetchedWithinTTL is the other
// half of the contract: inside the window the catalog is served from
// cache, so the expiring mark costs one download per TTL, not per read.
func TestMarketDescriptionCache_CatalogNotRefetchedWithinTTL(t *testing.T) {
	srv := newMarketSrv(t, bulkCatalogWithStaticVariants)
	mc, ctx := newMarketCacheForTest(t, srv)
	mc.catalogTTL = time.Hour

	for i := range 5 {
		if _, err := mc.LocalizedMarketDescriptions(ctx, types.EnLocale); err != nil {
			t.Fatalf("bulk read %d: %v", i, err)
		}
	}
	if got := srv.bulkHits.Load(); got != 1 {
		t.Fatalf("bulk hits = %d, want 1 (no refetch inside the TTL window)", got)
	}
}

// TestMarketDescriptionCache_WarmByIDRefetchesAfterCatalogTTL pins the
// warm-hit half of the catalog expiry: a by-id lookup whose entry
// already covers every requested locale must STILL refresh once the
// locale's catalog mark lapses. Pre-fix only bulk reads and by-id
// MISSES consulted the mark, so a feed-only consumer — which resolves
// markets exclusively by id on the odds-change hot path — served
// renamed markets and outcomes for the process lifetime.
func TestMarketDescriptionCache_WarmByIDRefetchesAfterCatalogTTL(t *testing.T) {
	srv := newMarketSrv(t, bulkCatalogWithStaticVariants)
	mc, ctx := newMarketCacheForTest(t, srv)
	mc.catalogTTL = 50 * time.Millisecond

	if _, err := mc.MarketDescriptionByID(ctx, 1, types.Some("way:three"), []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("initial lookup: %v", err)
	}

	// Upstream renames market 1 and one of its outcomes.
	srv.serve(`<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="1" name="Match winner" variant="way:three">
    <outcomes><outcome id="1" name="1"/><outcome id="2" name="X"/><outcome id="3" name="2"/></outcomes>
  </market>
  <market id="4" name="Correct score" variant="best_of:5">
    <outcomes><outcome id="1" name="3:0"/><outcome id="2" name="3:1"/></outcomes>
  </market>
  <market id="9" name="Plain"><outcomes><outcome id="1" name="o1"/></outcomes></market>
</market_descriptions>`)
	time.Sleep(80 * time.Millisecond)

	entry, err := mc.MarketDescriptionByID(ctx, 1, types.Some("way:three"), []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("warm lookup after catalog TTL: %v", err)
	}
	if got := srv.bulkHits.Load(); got != 2 {
		t.Fatalf("bulk hits = %d, want 2 (warm hit must refetch past the TTL window)", got)
	}
	snap := entry.Snapshot()
	if got := snap.Names[types.EnLocale]; got != "Match winner" {
		t.Fatalf("market 1 name = %q, want %q (rename must reach warm by-id readers)", got, "Match winner")
	}
}

// TestMarketDescriptionCache_WarmByIDNotRefetchedWithinTTL is the other
// half: inside the window warm hits stay free — the expiring mark costs
// one download per TTL, not one per odds change.
func TestMarketDescriptionCache_WarmByIDNotRefetchedWithinTTL(t *testing.T) {
	srv := newMarketSrv(t, bulkCatalogWithStaticVariants)
	mc, ctx := newMarketCacheForTest(t, srv)
	mc.catalogTTL = time.Hour

	for i := range 5 {
		if _, err := mc.MarketDescriptionByID(ctx, 1, types.Some("way:three"), []types.Locale{types.EnLocale}); err != nil {
			t.Fatalf("lookup %d: %v", i, err)
		}
	}
	if got := srv.bulkHits.Load(); got != 1 {
		t.Fatalf("bulk hits = %d, want 1 (no warm refetch inside the TTL window)", got)
	}
}

// TestMarketDescriptionCache_WarmByIDServesStaleOnRefreshFailure pins
// the degradation contract: when the refresh is purely freshness-driven
// (the entry still covers every requested locale) and the upstream is
// down, the stale-but-complete entry is served rather than failing the
// odds-change hot path. The next call retries (the mark stays expired).
func TestMarketDescriptionCache_WarmByIDServesStaleOnRefreshFailure(t *testing.T) {
	srv := newMarketSrv(t, bulkCatalogWithStaticVariants)
	mc, ctx := newMarketCacheForTest(t, srv)
	mc.catalogTTL = 50 * time.Millisecond

	if _, err := mc.MarketDescriptionByID(ctx, 1, types.Some("way:three"), []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("initial lookup: %v", err)
	}

	srv.serve(`upstream exploded`) // the refresh fetch will fail to decode
	time.Sleep(80 * time.Millisecond)

	entry, err := mc.MarketDescriptionByID(ctx, 1, types.Some("way:three"), []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("warm lookup during outage: %v (stale-but-complete entry must be served)", err)
	}
	if got := entry.Snapshot().Names[types.EnLocale]; got != "1x2" {
		t.Fatalf("name = %q, want stale %q", got, "1x2")
	}

	// Upstream recovers: the still-expired mark retries and the fresh
	// catalog lands.
	srv.serve(bulkCatalogWithStaticVariants)
	if _, err := mc.MarketDescriptionByID(ctx, 1, types.Some("way:three"), []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("warm lookup after recovery: %v", err)
	}
	if got := srv.bulkHits.Load(); got < 3 {
		t.Fatalf("bulk hits = %d, want >= 3 (failed refresh must not mark the locale loaded)", got)
	}
}

// TestMarketDescriptionCache_ByIDMissRefetchesAfterCatalogTTL pins the
// expiring mark as an INDEPENDENT safety net for the production defect:
// even if an entry goes missing from a store for some other reason, the
// by-id path recovers on its own once the window lapses, instead of
// failing for the rest of the process lifetime.
func TestMarketDescriptionCache_ByIDMissRefetchesAfterCatalogTTL(t *testing.T) {
	srv := newMarketSrv(t, bulkCatalogWithStaticVariants)
	mc, ctx := newMarketCacheForTest(t, srv)
	mc.catalogTTL = 50 * time.Millisecond

	if _, err := mc.MarketDescriptionByID(ctx, 1, types.Some("way:three"), []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("initial lookup: %v", err)
	}
	// Model an arbitrary loss from the permanent store.
	mc.mu.Lock()
	delete(mc.base, CompositeKey{MarketID: 1, Variant: "way:three"})
	mc.mu.Unlock()

	time.Sleep(80 * time.Millisecond)

	if _, err := mc.MarketDescriptionByID(ctx, 1, types.Some("way:three"), []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("lookup after catalog TTL: %v (the expired mark must force a refetch)", err)
	}
	if got := srv.bulkHits.Load(); got != 2 {
		t.Fatalf("bulk hits = %d, want 2", got)
	}
}
