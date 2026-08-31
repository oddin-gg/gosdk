package cache

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apiXML "github.com/oddin-gg/gosdk/internal/api/xml"
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

// TestMarketDescriptionCache_ZombieOutcomeDroppedAcrossLocales: an
// outcome removed upstream must not be kept alive by a stale locale
// that is never requested again. The first cut of the reconcile
// removed only the refreshed locale from an obsolete outcome — one
// still named by (say) a de load from before the removal survived,
// permanently failed en coverage, and made the market unavailable in
// en until the de locale happened to reload. Cross-locale removal is
// freshness-scoped: the sweep fires here because de's catalog mark has
// expired by the time en refreshes (see
// TestMarketDescriptionCache_FreshLocaleDisagreementPreserved for the
// both-marks-fresh case, which must NOT delete).
func TestMarketDescriptionCache_ZombieOutcomeDroppedAcrossLocales(t *testing.T) {
	catalog := func(outcomes string) string {
		return `<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="9" name="Plain"><outcomes>` + outcomes + `</outcomes></market>
</market_descriptions>`
	}
	srv := newMarketSrv(t, catalog(`<outcome id="1" name="o1"/><outcome id="2" name="o2"/>`))
	mc, ctx := newMarketCacheForTest(t, srv)
	mc.catalogTTL = 50 * time.Millisecond

	// Seed both locales: outcome 2 gains en AND de names.
	if _, err := mc.MarketDescriptionByID(ctx, 9, types.None[string](), []types.Locale{types.EnLocale, types.DeLocale}); err != nil {
		t.Fatalf("seed lookup: %v", err)
	}

	// Outcome 2 is removed upstream; only en is ever requested again.
	srv.serve(catalog(`<outcome id="1" name="o1"/>`))
	time.Sleep(80 * time.Millisecond)

	entry, err := mc.MarketDescriptionByID(ctx, 9, types.None[string](), []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("en lookup after outcome removal: %v (zombie outcome must not poison en coverage)", err)
	}
	snap := entry.Snapshot()
	if len(snap.Outcomes) != 1 || snap.Outcomes[0].ID != "1" {
		t.Fatalf("outcomes = %+v, want only outcome 1 (stale de name must not keep outcome 2 alive)", snap.Outcomes)
	}
	// The surviving outcome keeps its other locales' names.
	if got := snap.Outcomes[0].Names[types.DeLocale]; got == "" {
		t.Fatalf("outcome 1 de name lost by the en refresh: %+v", snap.Outcomes[0].Names)
	}
}

// TestMarketDescriptionCache_FreshLocaleDisagreementPreserved: a locale
// whose catalog TEMPORARILY omits an outcome must not delete other
// locales' still-fresh data. Pre-fix (last-row-wins outcome set) a de
// row carrying {1} while en carried {1,2} deleted outcome 2 from en
// globally — and the multi-locale request then PASSED coverage with
// silently truncated outcomes. Fresh disagreement must keep both
// sides' data and surface as the documented ErrMarketLocaleIncomplete.
func TestMarketDescriptionCache_FreshLocaleDisagreementPreserved(t *testing.T) {
	enCatalog := `<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="9" name="Plain"><outcomes><outcome id="1" name="o1"/><outcome id="2" name="o2"/></outcomes></market>
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

	// Both locales load fresh, back to back: en lists {1,2}, de {1}.
	// The asymmetry must be a typed incomplete, not a silent success.
	if _, err := mc.MarketDescriptionByID(ctx, 9, types.None[string](), []types.Locale{types.EnLocale, types.DeLocale}); !errors.Is(err, ErrMarketLocaleIncomplete) {
		t.Fatalf("err = %v, want ErrMarketLocaleIncomplete for a fresh outcome-set disagreement", err)
	}

	// And outcome 2's fresh en data survived the de load.
	entry, ok := mc.lookup(CompositeKey{MarketID: 9})
	if !ok {
		t.Fatal("entry missing after loads")
	}
	snap := entry.Snapshot()
	if len(snap.Outcomes) != 2 {
		t.Fatalf("outcomes = %+v, want both (fresh de row must not delete en's outcome 2)", snap.Outcomes)
	}
	// The en-only view still works: coverage per locale is intact.
	if _, err := mc.MarketDescriptionByID(ctx, 9, types.None[string](), []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("en-only lookup: %v", err)
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

// TestMarketDescriptionCache_StaleFallbackRevalidatesCoverage: the
// serve-stale-on-refresh-failure path must re-validate coverage on the
// CURRENT entry. loadOne commits per locale sequentially and mutates
// the entry in place, so a multi-locale freshness refresh can succeed
// for the first locale (reconciling removed data away) and then fail
// for the second — pre-fix the pointer captured before the load was
// returned as a complete success even though the surviving entry no
// longer covered the requested locales.
func TestMarketDescriptionCache_StaleFallbackRevalidatesCoverage(t *testing.T) {
	full := `<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="9" name="Plain"><outcomes><outcome id="1" name="o1"/></outcomes></market>
</market_descriptions>`
	removed := `<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="1" name="1x2"><outcomes><outcome id="1" name="home"/></outcomes></market>
</market_descriptions>`

	var failDE, dropEN atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		switch {
		case strings.Contains(r.URL.Path, "/de/") && failDE.Load():
			_, _ = io.WriteString(w, `upstream exploded`)
		case strings.Contains(r.URL.Path, "/en/") && dropEN.Load():
			_, _ = io.WriteString(w, removed) // market 9 removed upstream
		default:
			_, _ = io.WriteString(w, full)
		}
	}))
	t.Cleanup(srv.Close)

	mc := newMarketDescriptionCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))
	mc.catalogTTL = 50 * time.Millisecond
	ctx := t.Context()

	// Seed: market 9 covered in both locales.
	if _, err := mc.MarketDescriptionByID(ctx, 9, types.None[string](), []types.Locale{types.EnLocale, types.DeLocale}); err != nil {
		t.Fatalf("seed lookup: %v", err)
	}

	// Upstream removes market 9; the de catalog endpoint goes down.
	dropEN.Store(true)
	failDE.Store(true)
	time.Sleep(80 * time.Millisecond)

	// The freshness refresh loads en first (reconciling market 9's en
	// data away), then fails on de. The pre-refresh entry no longer
	// covers [en, de]; serving it as a clean success would hand back
	// data the refresh itself just invalidated.
	if _, err := mc.MarketDescriptionByID(ctx, 9, types.None[string](), []types.Locale{types.EnLocale, types.DeLocale}); err == nil {
		t.Fatal("partially-refreshed entry served as success; want the fetch error")
	}
}

// TestMarketDescription_StaleStartedRowHasNoCrossLocaleAuthority pins
// the monotonic gate on merge's cross-locale mutations. Different
// locales load under different singleflight keys and can run
// concurrently: an earlier-STARTED response finishing LAST could
// otherwise resurrect an outcome the newer row removed, or reinstall
// older metadata — and with both locale marks then fresh, the rollback
// stood for a full catalogTTL. A stale-started row may still apply its
// own locale's strings (same-locale flights are serialized), but must
// not add outcomes, sweep other locales, or touch metadata.
func TestMarketDescription_StaleStartedRowHasNoCrossLocaleAuthority(t *testing.T) {
	row := func(name, groups string, outcomes ...apiXML.MarketDescriptionOutcome) apiXML.MarketDescription {
		return apiXML.MarketDescription{
			ID:       9,
			Name:     name,
			Groups:   groups,
			Outcomes: &apiXML.OutcomesWrapper{Outcome: outcomes},
		}
	}
	d := &LocalizedMarketDescription{
		id:       9,
		name:     make(map[types.Locale]string),
		outcomes: make(map[string]*LocalizedOutcomeDescription),
	}

	olderStart := time.Now()
	newerStart := olderStart.Add(50 * time.Millisecond)
	stale := func(types.Locale) bool { return true } // everything expired

	// The NEWER en row (outcome 2 removed upstream, regrouped) applies
	// first — it finished first.
	d.merge(row("Plain", "handicap", apiXML.MarketDescriptionOutcome{ID: "1", Name: "o1"}), types.EnLocale, newerStart, stale)

	// The OLDER de row (still carrying outcome 2, old groups) finishes
	// last: its own locale's strings land, nothing else.
	d.merge(row("Schlicht", "all|score",
		apiXML.MarketDescriptionOutcome{ID: "1", Name: "a1"},
		apiXML.MarketDescriptionOutcome{ID: "2", Name: "a2"},
	), types.DeLocale, olderStart, stale)

	snap := d.Snapshot()
	if len(snap.Outcomes) != 1 || snap.Outcomes[0].ID != "1" {
		t.Fatalf("outcomes = %+v, want only outcome 1 (stale-started row must not resurrect outcome 2)", snap.Outcomes)
	}
	if got := snap.Outcomes[0].Names[types.DeLocale]; got != "a1" {
		t.Fatalf("outcome 1 de name = %q, want %q (own-locale strings still apply)", got, "a1")
	}
	if len(snap.Groups) != 1 || snap.Groups[0] != "handicap" {
		t.Fatalf("groups = %v, want [handicap] (stale-started row must not reinstall old metadata)", snap.Groups)
	}
	if got := snap.Names[types.DeLocale]; got != "Schlicht" {
		t.Fatalf("de market name = %q, want %q", got, "Schlicht")
	}

	// A genuinely newer row still advances everything.
	newest := newerStart.Add(50 * time.Millisecond)
	d.merge(row("Plain", "score",
		apiXML.MarketDescriptionOutcome{ID: "1", Name: "o1"},
		apiXML.MarketDescriptionOutcome{ID: "3", Name: "o3"},
	), types.EnLocale, newest, stale)
	snap = d.Snapshot()
	if len(snap.Outcomes) != 2 {
		t.Fatalf("outcomes = %+v, want 1 and 3 (newest row is authoritative)", snap.Outcomes)
	}
	if len(snap.Groups) != 1 || snap.Groups[0] != "score" {
		t.Fatalf("groups = %v, want [score]", snap.Groups)
	}
}

// TestMarketDescriptionCache_MalformedTombstoneReconciled: a market
// whose only trace is a malformed-row record (no entry was ever
// created) and that the fresh catalog no longer carries must classify
// as ErrItemNotFound. Pre-fix reconcileBulk pruned only m.base, so the
// stale malformed record kept the removed market classified as
// ErrMarketLocaleIncomplete forever.
func TestMarketDescriptionCache_MalformedTombstoneReconciled(t *testing.T) {
	srv := newMarketSrv(t, `<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="9" name="Plain"><outcomes><outcome id="1" name="o1"/></outcomes></market>
  <market id="5" name="Broken"/>
</market_descriptions>`)
	mc, ctx := newMarketCacheForTest(t, srv)
	mc.catalogTTL = 50 * time.Millisecond

	// The malformed row (no outcomes block) classifies as incomplete.
	if _, err := mc.MarketDescriptionByID(ctx, 5, types.None[string](), []types.Locale{types.EnLocale}); !errors.Is(err, ErrMarketLocaleIncomplete) {
		t.Fatalf("err = %v, want ErrMarketLocaleIncomplete for the malformed row", err)
	}

	// The broken market is removed upstream entirely.
	srv.serve(`<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="9" name="Plain"><outcomes><outcome id="1" name="o1"/></outcomes></market>
</market_descriptions>`)
	time.Sleep(80 * time.Millisecond)

	_, err := mc.MarketDescriptionByID(ctx, 5, types.None[string](), []types.Locale{types.EnLocale})
	if !errors.Is(err, ErrItemNotFoundInCache) {
		t.Fatalf("err = %v, want ErrItemNotFoundInCache after upstream removal", err)
	}
	if errors.Is(err, ErrMarketLocaleIncomplete) {
		t.Fatalf("err = %v, stale malformed record must not classify a removed market as incomplete", err)
	}
}

// TestMarketDescriptionCache_MalformedRecordSurvivesOtherLocale: a
// locale whose catalog simply omits a key must not erase ANOTHER
// locale's malformed evidence for it. Pre-fix reconcileBulk deleted
// the whole per-key record: with the en row malformed and the de
// catalog omitting the market, loading de flipped the multi-locale
// classification from ErrMarketLocaleIncomplete (market exists but is
// broken in en) to a definitive ErrItemNotFound.
func TestMarketDescriptionCache_MalformedRecordSurvivesOtherLocale(t *testing.T) {
	enCatalog := `<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="9" name="Plain"><outcomes><outcome id="1" name="o1"/></outcomes></market>
  <market id="5" name="Broken"/>
</market_descriptions>`
	deCatalog := `<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="9" name="Schlicht"><outcomes><outcome id="1" name="a1"/></outcomes></market>
</market_descriptions>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if strings.Contains(r.URL.Path, "/de/") {
			_, _ = io.WriteString(w, deCatalog) // de omits market 5 entirely
			return
		}
		_, _ = io.WriteString(w, enCatalog) // en carries market 5, malformed
	}))
	t.Cleanup(srv.Close)

	mc := newMarketDescriptionCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))
	ctx := t.Context()

	// Loading BOTH locales runs de's reconcile after en recorded the
	// malformed row; en's evidence must survive it.
	_, err := mc.MarketDescriptionByID(ctx, 5, types.None[string](), []types.Locale{types.EnLocale, types.DeLocale})
	if !errors.Is(err, ErrMarketLocaleIncomplete) {
		t.Fatalf("err = %v, want ErrMarketLocaleIncomplete (en's malformed evidence must survive de's reconcile)", err)
	}
	if errors.Is(err, ErrItemNotFoundInCache) {
		t.Fatalf("err = %v, must not classify as definitive not-found while en still carries the row", err)
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
