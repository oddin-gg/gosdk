package cache

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

// These tests pin the bulk catalog as a REPLACE, not an accumulate.
// Pre-fix the cache was a historical union: markets removed upstream
// were served for the process lifetime, removed outcomes lingered (and
// poisoned locale coverage for every locale loaded after the removal),
// and groups / outcome types / specifiers were frozen at entry creation.

// TestMarketDescriptionCache_RemovedMarketReconciled: a market absent
// from the fresh bulk response must stop being served — by the bulk
// views and by id.
func TestMarketDescriptionCache_RemovedMarketReconciled(t *testing.T) {
	srv := newMarketSrv(t, bulkCatalogWithStaticVariants)
	mc, ctx := newMarketCacheForTest(t, srv)
	mc.catalogTTL = 50 * time.Millisecond

	if _, err := mc.MarketDescriptionByID(ctx, 4, types.Some("best_of:5"), []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("initial lookup: %v", err)
	}

	// Market 4 is removed upstream.
	srv.serve(`<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="1" name="1x2" variant="way:three">
    <outcomes><outcome id="1" name="home"/><outcome id="2" name="draw"/><outcome id="3" name="away"/></outcomes>
  </market>
  <market id="9" name="Plain"><outcomes><outcome id="1" name="o1"/></outcomes></market>
</market_descriptions>`)
	time.Sleep(80 * time.Millisecond)

	all, err := mc.LocalizedMarketDescriptions(ctx, types.EnLocale)
	if err != nil {
		t.Fatalf("bulk read after removal: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("bulk view = %d entries, want 2 (removed market must not linger)", len(all))
	}
	if mc.baseHas(CompositeKey{MarketID: 4, Variant: "best_of:5"}) {
		t.Fatal("removed market still present in the base store")
	}
	if _, err := mc.MarketDescriptionByID(ctx, 4, types.Some("best_of:5"), []types.Locale{types.EnLocale}); !errors.Is(err, ErrItemNotFoundInCache) {
		t.Fatalf("by-id err = %v, want ErrItemNotFoundInCache after upstream removal", err)
	}
}

// TestMarketDescriptionCache_RemovedOutcomeReconciled: an outcome the
// fresh row no longer carries must leave the entry — pre-fix it not
// only lingered in snapshots, it broke coverage validation for every
// locale loaded after the removal (the dead outcome could never gain
// the new locale's name), turning the market permanently unavailable
// via ErrMarketLocaleIncomplete.
func TestMarketDescriptionCache_RemovedOutcomeReconciled(t *testing.T) {
	srv := newMarketSrv(t, `<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="9" name="Plain">
    <outcomes><outcome id="1" name="o1"/><outcome id="2" name="o2"/></outcomes>
  </market>
</market_descriptions>`)
	mc, ctx := newMarketCacheForTest(t, srv)
	mc.catalogTTL = 50 * time.Millisecond

	if _, err := mc.MarketDescriptionByID(ctx, 9, types.None[string](), []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("initial lookup: %v", err)
	}

	// Outcome 2 is removed upstream.
	srv.serve(`<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="9" name="Plain">
    <outcomes><outcome id="1" name="o1"/></outcomes>
  </market>
</market_descriptions>`)
	time.Sleep(80 * time.Millisecond)

	entry, err := mc.MarketDescriptionByID(ctx, 9, types.None[string](), []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("lookup after outcome removal: %v", err)
	}
	snap := entry.Snapshot()
	if len(snap.Outcomes) != 1 || snap.Outcomes[0].ID != "1" {
		t.Fatalf("outcomes = %+v, want only outcome 1 (removed outcome must not linger)", snap.Outcomes)
	}

	// The coverage-poison regression: a locale loaded AFTER the removal
	// must validate — pre-fix the lingering outcome 2 lacked the new
	// locale's name forever, so this call failed with
	// ErrMarketLocaleIncomplete for the rest of the process lifetime.
	if _, err := mc.MarketDescriptionByID(ctx, 9, types.None[string](), []types.Locale{types.EnLocale, types.DeLocale}); err != nil {
		t.Fatalf("multi-locale lookup after outcome removal: %v (dead outcome must not poison new-locale coverage)", err)
	}
}

// TestMarketDescriptionCache_MetadataRefreshedOnReload: groups, outcome
// types, and specifiers must track the fresh row — pre-fix groups and
// outcome types were set only at entry creation and specifiers only
// ever grew, so upstream changes to any of them never landed.
func TestMarketDescriptionCache_MetadataRefreshedOnReload(t *testing.T) {
	srv := newMarketSrv(t, `<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="9" name="Plain" groups="all|score" outcome_type="player">
    <outcomes><outcome id="1" name="o1" description="first"/></outcomes>
    <specifiers><specifier name="total" type="decimal"/></specifiers>
  </market>
</market_descriptions>`)
	mc, ctx := newMarketCacheForTest(t, srv)
	mc.catalogTTL = 50 * time.Millisecond

	entry, err := mc.MarketDescriptionByID(ctx, 9, types.None[string](), []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("initial lookup: %v", err)
	}
	snap := entry.Snapshot()
	if len(snap.Groups) != 2 || len(snap.Specifiers) != 1 {
		t.Fatalf("initial snapshot groups=%v specifiers=%v", snap.Groups, snap.Specifiers)
	}

	// Upstream regroups the market, drops the outcome type and the
	// specifier, and removes the outcome description.
	srv.serve(`<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="9" name="Plain" groups="handicap">
    <outcomes><outcome id="1" name="o1"/></outcomes>
  </market>
</market_descriptions>`)
	time.Sleep(80 * time.Millisecond)

	entry, err = mc.MarketDescriptionByID(ctx, 9, types.None[string](), []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("lookup after metadata change: %v", err)
	}
	snap = entry.Snapshot()
	if len(snap.Groups) != 1 || snap.Groups[0] != "handicap" {
		t.Fatalf("groups = %v, want [handicap] (regroup must land)", snap.Groups)
	}
	if _, ok := snap.OutcomeType.Get(); ok {
		t.Fatalf("outcomeType = %v, want None (dropped upstream)", snap.OutcomeType)
	}
	if len(snap.Specifiers) != 0 {
		t.Fatalf("specifiers = %v, want none (emptied upstream)", snap.Specifiers)
	}
	if len(snap.Outcomes[0].Descriptions) != 0 {
		t.Fatalf("outcome descriptions = %v, want none (removed upstream)", snap.Outcomes[0].Descriptions)
	}
}

// TestMarketDescriptionCache_ReconcilePerLocale: the bulk response is
// authoritative for ITS locale only. A market present in en but absent
// from the de catalog keeps its en data (and stays reachable by id in
// en); only the all-locales views and multi-locale by-id reads report
// the de gap.
func TestMarketDescriptionCache_ReconcilePerLocale(t *testing.T) {
	enCatalog := `<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="1" name="1x2"><outcomes><outcome id="1" name="home"/></outcomes></market>
  <market id="9" name="Plain"><outcomes><outcome id="1" name="o1"/></outcomes></market>
</market_descriptions>`
	deCatalog := `<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="9" name="Schlicht"><outcomes><outcome id="1" name="a1"/></outcomes></market>
</market_descriptions>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if strings.Contains(r.URL.Path, "/de/") {
			_, _ = io.WriteString(w, deCatalog)
			return
		}
		_, _ = io.WriteString(w, enCatalog)
	}))
	t.Cleanup(srv.Close)

	mc := newMarketDescriptionCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))
	ctx := t.Context()

	if _, err := mc.LocalizedMarketDescriptions(ctx, types.EnLocale); err != nil {
		t.Fatalf("en bulk read: %v", err)
	}
	if _, err := mc.LocalizedMarketDescriptions(ctx, types.DeLocale); err != nil {
		t.Fatalf("de bulk read: %v", err)
	}

	// The de reconcile must not evict market 1's en data.
	if !mc.baseHas(CompositeKey{MarketID: 1}) {
		t.Fatal("market 1 evicted by another locale's reconcile")
	}
	if _, err := mc.MarketDescriptionByID(ctx, 1, types.None[string](), []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("by-id en lookup: %v", err)
	}
	// The de gap stays a typed incomplete for multi-locale readers.
	if _, err := mc.MarketDescriptionByID(ctx, 1, types.None[string](), []types.Locale{types.EnLocale, types.DeLocale}); !errors.Is(err, ErrMarketLocaleIncomplete) {
		t.Fatalf("by-id [en,de] err = %v, want ErrMarketLocaleIncomplete", err)
	}
}
