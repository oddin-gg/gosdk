package cache

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/internal/cache/lru"
	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

// TestMarketDescriptionCache_VariantCachedAcrossCalls is the regression
// for the CompositeKey pointer-identity bug: the key's Variant field was
// a *string, and Go compares pointer fields by identity, so a
// freshly-built lookup key could NEVER equal a stored one. Every variant
// lookup missed, refetched the variant from the API, and inserted a new
// entry under a new pointer key — per-ACCESS memory growth plus a
// redundant HTTP round-trip on every call.
func TestMarketDescriptionCache_VariantCachedAcrossCalls(t *testing.T) {
	const variant = "od:dynamic_outcomes:123" // IsMarketVariantWithDynamicOutcomes == true
	var variantHits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/variants/") {
			variantHits.Add(1)
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, fmt.Sprintf(`<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="7" name="Dynamic" variant="%s">
    <outcomes><outcome id="1" name="o1"/></outcomes>
  </market>
</market_descriptions>`, variant))
	}))
	defer srv.Close()

	mc := newMarketDescriptionCache(t.Context(), newAPIClientForTest(t, srv), nil)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	for i := 0; i < 3; i++ {
		entry, err := mc.MarketDescriptionByID(ctx, 7, types.Some(variant), []types.Locale{types.EnLocale})
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if v, ok := entry.Snapshot().Variant.Get(); !ok || v != variant {
			t.Fatalf("call %d: variant = %v", i, entry.Snapshot().Variant)
		}
	}
	if got := variantHits.Load(); got != 1 {
		t.Fatalf("variant endpoint hits = %d, want 1 (cache must hit on repeat calls)", got)
	}
}

// TestMarketDescriptionCache_ByIDMissSharesBulkLoad pins the load-
// granularity fix: a by-id miss for a non-variant id resolves through
// the BULK catalog endpoint, so it must share the bulk singleflight key
// and record the locale as loaded — a subsequent bulk read must not
// refetch. Pre-fix the by-id miss ran the full download under its own
// narrow key and never marked the locale, so the next bulk call
// downloaded the whole catalog again.
func TestMarketDescriptionCache_ByIDMissSharesBulkLoad(t *testing.T) {
	var bulkHits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/markets") && !strings.Contains(r.URL.Path, "/variants/") {
			bulkHits.Add(1)
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="1" name="One"><outcomes><outcome id="1" name="o1"/></outcomes></market>
  <market id="2" name="Two"><outcomes><outcome id="1" name="o1"/></outcomes></market>
</market_descriptions>`)
	}))
	defer srv.Close()

	mc := newMarketDescriptionCache(t.Context(), newAPIClientForTest(t, srv), nil)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	if _, err := mc.MarketDescriptionByID(ctx, 1, types.None[string](), []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("by-id: %v", err)
	}
	all, err := mc.LocalizedMarketDescriptions(ctx, types.EnLocale)
	if err != nil {
		t.Fatalf("bulk: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("bulk view = %d entries, want 2", len(all))
	}
	if got := bulkHits.Load(); got != 1 {
		t.Fatalf("bulk endpoint hits = %d, want 1 (by-id miss must mark the locale loaded)", got)
	}
}

// TestMarketDescriptionCache_MalformedRowSkipped pins the resilience
// fix: one catalog description without an <outcomes> block must not
// fail the whole locale load. Pre-fix the load aborted after partial
// upserts, the locale was never marked loaded, and every subsequent
// access refetched-and-failed — one bad row took down the entire
// market-description surface.
func TestMarketDescriptionCache_MalformedRowSkipped(t *testing.T) {
	var bulkHits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bulkHits.Add(1)
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="1" name="Good"><outcomes><outcome id="1" name="o1"/></outcomes></market>
  <market id="2" name="NoOutcomes"/>
  <market id="3" name="AlsoGood"><outcomes><outcome id="1" name="o1"/></outcomes></market>
</market_descriptions>`)
	}))
	defer srv.Close()

	mc := newMarketDescriptionCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	all, err := mc.LocalizedMarketDescriptions(ctx, types.EnLocale)
	if err != nil {
		t.Fatalf("load with malformed row: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("catalog = %d entries, want 2 (malformed row skipped, others kept)", len(all))
	}
	// Locale must be marked loaded: no refetch on the second read.
	if _, err := mc.LocalizedMarketDescriptions(ctx, types.EnLocale); err != nil {
		t.Fatalf("second read: %v", err)
	}
	if got := bulkHits.Load(); got != 1 {
		t.Fatalf("bulk hits = %d, want 1 (locale must be marked loaded despite the bad row)", got)
	}
}

// TestMatchStatusCache_Bounded pins the unbounded-growth fix: the
// status cache is fed by every OddsChange, and the old plain map kept
// one entry per match the process ever saw. It now lives in a bounded
// expirable LRU.
func TestMatchStatusCache_Bounded(t *testing.T) {
	// Construct directly: the package constructor subscribes to the API
	// client's observer hook, which this storage-only test doesn't need.
	c := &MatchStatusCache{
		logger: log.New(nil),
		entries: lru.NewTTL[types.URN, *LocalizedMatchStatus](
			lru.DefaultEventCacheSize, nil, lru.DefaultEventCacheTTL),
	}
	for i := 0; i < lru.DefaultEventCacheSize+50; i++ {
		id := types.URN{Prefix: "od", Type: "match", ID: i + 1}
		c.refreshOrInsertFeedItem(id, &feedXML.SportEventStatus{Status: ptrOf(1), MatchStatus: ptrOf(1)}, time.Time{})
	}
	if got := c.entries.Len(); got > lru.DefaultEventCacheSize {
		t.Fatalf("match-status entries = %d, want <= %d (LRU bound)", got, lru.DefaultEventCacheSize)
	}
}

// TestPlayersCache_Bounded pins the same class of fix for players.
func TestPlayersCache_Bounded(t *testing.T) {
	c := newPlayersCache(t.Context(), nil, log.New(nil))
	for i := 0; i < playersCacheSize+50; i++ {
		c.set(PlayerCacheKey{PlayerID: fmt.Sprintf("od:player:%d", i), Locale: types.EnLocale}, types.Player{})
	}
	if got := c.players.Len(); got > playersCacheSize {
		t.Fatalf("players entries = %d, want <= %d (LRU bound)", got, playersCacheSize)
	}
}

// ptrOf returns a pointer to v — test-literal convenience for the
// presence-pointer XML fields.
func ptrOf[T any](v T) *T { return &v }
