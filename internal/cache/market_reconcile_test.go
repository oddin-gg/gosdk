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

// TestMarketDescriptionCache_EmptyResponseDoesNotWipeCatalog: an
// empty-but-successful bulk response (response_code=OK, zero <market>
// rows — an upstream deploy glitch or truncated-yet-well-formed body)
// must not be treated as a full upstream removal. Pre-fix the empty
// `seen` set made reconcileBulk strip the locale from every base entry
// and delete the emptied ones, and the locale was then marked loaded —
// every bulk read returned empty and every MarketDescriptionByID
// returned ErrItemNotFoundInCache for a full catalogTTL (12h): no
// odds-change market could be named.
func TestMarketDescriptionCache_EmptyResponseDoesNotWipeCatalog(t *testing.T) {
	srv := newMarketSrv(t, bulkCatalogWithStaticVariants)
	mc, ctx := newMarketCacheForTest(t, srv)
	mc.catalogTTL = 50 * time.Millisecond

	if _, err := mc.MarketDescriptionByID(ctx, 9, types.None[string](), []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("seed lookup: %v", err)
	}
	if got := mc.baseLen(); got != 3 {
		t.Fatalf("base = %d entries after seed, want 3", got)
	}

	// Upstream glitches: well-formed OK envelope with no rows.
	srv.serve(`<?xml version="1.0"?>
<market_descriptions response_code="OK"/>`)
	time.Sleep(80 * time.Millisecond)

	all, err := mc.LocalizedMarketDescriptions(ctx, types.EnLocale)
	if err != nil {
		t.Fatalf("bulk read across empty response: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("bulk view = %d entries, want 3 (empty response must not wipe the catalog)", len(all))
	}
	if _, err := mc.MarketDescriptionByID(ctx, 9, types.None[string](), []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("by-id across empty response: %v", err)
	}

	// Upstream recovers with a real removal: the reconcile still works.
	srv.serve(`<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="1" name="1x2" variant="way:three">
    <outcomes><outcome id="1" name="home"/><outcome id="2" name="draw"/><outcome id="3" name="away"/></outcomes>
  </market>
  <market id="9" name="Plain"><outcomes><outcome id="1" name="o1"/></outcomes></market>
</market_descriptions>`)
	time.Sleep(80 * time.Millisecond)
	all, err = mc.LocalizedMarketDescriptions(ctx, types.EnLocale)
	if err != nil {
		t.Fatalf("bulk read after recovery: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("bulk view = %d entries, want 2 (a non-empty response keeps its removal authority)", len(all))
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
// the monotonic gate on merge's CROSS-LOCALE mutations. Different
// locales load under different singleflight keys and can run
// concurrently: an earlier-STARTED response finishing LAST must not
// run the cross-locale stale sweep or reinstall older metadata. Its
// OWN locale stays fully authoritative — strings, removals, and
// outcome additions (see the completion-order test below for why adds
// are not gated): a re-added outcome carries only that locale's name,
// reading as a typed fresh disagreement rather than silent data.
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
	// last: everything about ITS locale lands — including re-adding
	// outcome 2 as de-only evidence — but metadata stays untouched.
	d.merge(row("Schlicht", "all|score",
		apiXML.MarketDescriptionOutcome{ID: "1", Name: "a1"},
		apiXML.MarketDescriptionOutcome{ID: "2", Name: "a2"},
	), types.DeLocale, olderStart, stale)

	snap := d.Snapshot()
	if len(snap.Outcomes) != 2 {
		t.Fatalf("outcomes = %+v, want 1 and 2 (own-locale membership always lands)", snap.Outcomes)
	}
	var o2 *types.OutcomeDescription
	for i := range snap.Outcomes {
		if snap.Outcomes[i].ID == "2" {
			o2 = &snap.Outcomes[i]
		}
	}
	if o2 == nil {
		t.Fatalf("outcome 2 missing: %+v", snap.Outcomes)
	}
	// The re-added outcome is de-only evidence — a typed disagreement,
	// never a silently complete en outcome.
	if _, ok := o2.Names[types.EnLocale]; ok {
		t.Fatalf("outcome 2 gained an en name from a de row: %+v", o2.Names)
	}
	if got := o2.Names[types.DeLocale]; got != "a2" {
		t.Fatalf("outcome 2 de name = %q, want %q", got, "a2")
	}
	if missing := d.missingLocales([]types.Locale{types.EnLocale}); len(missing) != 1 {
		t.Fatalf("missingLocales(en) = %v, want [en] (disagreement must surface, not read as complete)", missing)
	}
	if len(snap.Groups) != 1 || snap.Groups[0] != "handicap" {
		t.Fatalf("groups = %v, want [handicap] (stale-started row must not reinstall old metadata)", snap.Groups)
	}
	if got := snap.Names[types.DeLocale]; got != "Schlicht" {
		t.Fatalf("de market name = %q, want %q", got, "Schlicht")
	}

	// A genuinely newer row still advances everything: outcome 2's
	// de-only evidence is swept (de's mark is stale here) and metadata
	// follows the newest row.
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

// TestMarketDescriptionCache_InFlightLocaleNotSweptAsStale pins the
// loading-locale guard: a locale whose refresh is IN FLIGHT has an
// expired mark (the mark is republished only at flight completion),
// but its just-merged rows are the opposite of stale. Pre-fix an en
// merge interleaving with a de refresh — de merged market X's row,
// hasn't re-marked yet — swept X's fresh de outcome names; de then
// finished and marked itself fresh without revisiting X, and both
// locales served the silently truncated set for a full catalogTTL.
func TestMarketDescriptionCache_InFlightLocaleNotSweptAsStale(t *testing.T) {
	row := func(name string, outcomes ...apiXML.MarketDescriptionOutcome) apiXML.MarketDescription {
		return apiXML.MarketDescription{
			ID: 9, Name: name,
			Outcomes: &apiXML.OutcomesWrapper{Outcome: outcomes},
		}
	}
	mc := newMarketDescriptionCache(t.Context(), nil, log.New(nil))
	key := CompositeKey{MarketID: 9}
	start := time.Now()

	// The de refresh has merged market 9 (carrying outcome 2) but has
	// not completed — its mark is unset, its flight is registered.
	mc.mu.Lock()
	mc.loadingLocales[types.DeLocale] = 1
	mc.mu.Unlock()
	if err := mc.upsert(row("Schlicht",
		apiXML.MarketDescriptionOutcome{ID: "1", Name: "a1"},
		apiXML.MarketDescriptionOutcome{ID: "2", Name: "a2"},
	), types.DeLocale, start); err != nil {
		t.Fatalf("de upsert: %v", err)
	}

	// A concurrent en merge whose row omits outcome 2 must NOT sweep
	// the in-flight de names.
	if err := mc.upsert(row("Plain",
		apiXML.MarketDescriptionOutcome{ID: "1", Name: "o1"},
	), types.EnLocale, start.Add(time.Millisecond)); err != nil {
		t.Fatalf("en upsert: %v", err)
	}
	entry, ok := mc.lookup(key)
	if !ok {
		t.Fatal("entry missing")
	}
	snap := entry.Snapshot()
	if len(snap.Outcomes) != 2 {
		t.Fatalf("outcomes = %+v, want both (in-flight de rows must not be swept as stale)", snap.Outcomes)
	}

	// Once the de flight is gone AND its mark is expired, the same en
	// merge sweeps the now genuinely stale de evidence.
	mc.mu.Lock()
	delete(mc.loadingLocales, types.DeLocale)
	mc.mu.Unlock()
	if err := mc.upsert(row("Plain",
		apiXML.MarketDescriptionOutcome{ID: "1", Name: "o1"},
	), types.EnLocale, start.Add(2*time.Millisecond)); err != nil {
		t.Fatalf("en upsert after flight end: %v", err)
	}
	entry, _ = mc.lookup(key)
	snap = entry.Snapshot()
	if len(snap.Outcomes) != 1 || snap.Outcomes[0].ID != "1" {
		t.Fatalf("outcomes = %+v, want only outcome 1 (stale sweep must still fire once the flight is gone)", snap.Outcomes)
	}
}

// loadingLen reads the in-flight locale refcount map size under the
// cache lock (test helper).
func (m *MarketDescriptionCache) loadingLen() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.loadingLocales)
}

// TestMarketDescriptionCache_LoadingLocaleRefcountSettles drives the
// loadingLocales registration through REAL loadOne flights (the
// in-flight-locale test above pokes the map directly, which cannot
// catch broken production wiring). A leaked refcount is the dangerous
// direction: loadingLocales[l] > 0 forever makes staleLocale report
// that locale fresh for the process lifetime, so the cross-locale
// sweep never fires again and consumers get silently truncated
// outcome sets — the exact class this PR closes. The refcount must
// settle to zero after (a) a successful load, (b) a fetch error, and
// (c) a load raced by ClearCacheItem (tombstone-suppressed flight).
func TestMarketDescriptionCache_LoadingLocaleRefcountSettles(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	var gate, fail atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gate.Load() {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
		}
		w.Header().Set("Content-Type", "application/xml")
		if fail.Load() {
			_, _ = io.WriteString(w, `upstream exploded`)
			return
		}
		_, _ = io.WriteString(w, bulkCatalogWithStaticVariants)
	}))
	t.Cleanup(srv.Close)
	mc := newMarketDescriptionCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))
	mc.catalogTTL = 50 * time.Millisecond
	ctx := t.Context()

	// (a) successful load.
	if _, err := mc.MarketDescriptionByID(ctx, 9, types.None[string](), []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("seed lookup: %v", err)
	}
	if got := mc.loadingLen(); got != 0 {
		t.Fatalf("loadingLocales = %d after successful load, want 0", got)
	}

	// (b) fetch-error path.
	fail.Store(true)
	time.Sleep(80 * time.Millisecond) // expire the mark so the next read reloads
	if _, err := mc.MarketDescriptionByID(ctx, 999, types.None[string](), []types.Locale{types.DeLocale}); err == nil {
		t.Fatal("lookup during outage: want error")
	}
	if got := mc.loadingLen(); got != 0 {
		t.Fatalf("loadingLocales = %d after failed load, want 0", got)
	}
	fail.Store(false)

	// (c) ClearCacheItem racing an in-flight load: the flight's stores
	// are tombstone-suppressed, and its registration must still unwind.
	gate.Store(true)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = mc.MarketDescriptionByID(ctx, 9, types.None[string](), []types.Locale{types.RuLocale})
	}()
	<-entered // flight is blocked in the handler
	mc.ClearCacheItem(9, types.None[string]())
	gate.Store(false)
	close(release)
	<-done
	if got := mc.loadingLen(); got != 0 {
		t.Fatalf("loadingLocales = %d after clear-raced load, want 0 (a leak disables the stale sweep forever)", got)
	}
}

// TestMarketDescriptionCache_SweepSuppressedWhileLocaleLoadsEndToEnd is
// the production-wiring counterpart of the in-flight guard test: the
// de refresh is held open by the FIXTURE (a real loadOne flight, not a
// poked map) while an en refresh merges. While de is in flight the
// sweep must be suppressed — the en read reports the disagreement as
// the typed ErrMarketLocaleIncomplete (pre-fix wiring it would SUCCEED
// with a silently truncated outcome set). Once de completes, its own-
// locale removal drops the dead outcome and en reads converge to the
// truncated-but-correct set.
func TestMarketDescriptionCache_SweepSuppressedWhileLocaleLoadsEndToEnd(t *testing.T) {
	twoOutcomes := `<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="9" name="Plain"><outcomes><outcome id="1" name="o1"/><outcome id="2" name="o2"/></outcomes></market>
</market_descriptions>`
	oneOutcome := `<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="9" name="Plain"><outcomes><outcome id="1" name="o1"/></outcomes></market>
</market_descriptions>`

	var body atomic.Pointer[string]
	body.Store(&twoOutcomes)
	var blockDE atomic.Bool
	deEntered := make(chan struct{}, 1)
	deRelease := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/de/") && blockDE.Load() {
			select {
			case deEntered <- struct{}{}:
			default:
			}
			<-deRelease
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, *body.Load())
	}))
	t.Cleanup(srv.Close)
	mc := newMarketDescriptionCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))
	mc.catalogTTL = 50 * time.Millisecond
	ctx := t.Context()

	// Seed both locales with outcomes {1, 2}.
	if _, err := mc.MarketDescriptionByID(ctx, 9, types.None[string](), []types.Locale{types.EnLocale, types.DeLocale}); err != nil {
		t.Fatalf("seed lookup: %v", err)
	}

	// Outcome 2 is removed upstream; both marks expire; the de refresh
	// starts and blocks inside the fixture.
	body.Store(&oneOutcome)
	blockDE.Store(true)
	time.Sleep(80 * time.Millisecond)
	deDone := make(chan error, 1)
	go func() {
		_, err := mc.MarketDescriptionByID(ctx, 9, types.None[string](), []types.Locale{types.DeLocale})
		deDone <- err
	}()
	<-deEntered // the de flight is in the handler — registered as loading

	// An en refresh merges while de is in flight: outcome 2 loses its
	// en name (own-locale removal) but de's evidence must survive the
	// sweep — the read errors typed instead of silently truncating.
	if _, err := mc.MarketDescriptionByID(ctx, 9, types.None[string](), []types.Locale{types.EnLocale}); !errors.Is(err, ErrMarketLocaleIncomplete) {
		t.Fatalf("en read while de in flight: err = %v, want ErrMarketLocaleIncomplete (sweep must be suppressed for a loading locale)", err)
	}

	// de completes: its own row omits outcome 2, so the disagreement
	// resolves and en reads converge.
	blockDE.Store(false)
	close(deRelease)
	if err := <-deDone; err != nil {
		t.Fatalf("de read: %v", err)
	}
	entry, err := mc.MarketDescriptionByID(ctx, 9, types.None[string](), []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("en read after de completed: %v", err)
	}
	if snap := entry.Snapshot(); len(snap.Outcomes) != 1 || snap.Outcomes[0].ID != "1" {
		t.Fatalf("outcomes = %+v, want only outcome 1", snap.Outcomes)
	}
	if got := mc.loadingLen(); got != 0 {
		t.Fatalf("loadingLocales = %d after all loads settled, want 0", got)
	}
}

// TestMarketDescription_OutcomeSetIndependentOfCompletionOrder pins the
// round-3 regression: with en carrying {1,2} and de carrying {1}
// loaded concurrently, the outcome set must not depend on which
// response FINISHES first. Pre-fix the add-gate skipped outcome 2 when
// the en row (started first) finished last — both locales then passed
// coverage with silently truncated data — while the reverse order kept
// it and reported the disagreement. Same responses, same typed result,
// either order.
func TestMarketDescription_OutcomeSetIndependentOfCompletionOrder(t *testing.T) {
	enRow := apiXML.MarketDescription{
		ID: 9, Name: "Plain",
		Outcomes: &apiXML.OutcomesWrapper{Outcome: []apiXML.MarketDescriptionOutcome{
			{ID: "1", Name: "o1"}, {ID: "2", Name: "o2"},
		}},
	}
	deRow := apiXML.MarketDescription{
		ID: 9, Name: "Schlicht",
		Outcomes: &apiXML.OutcomesWrapper{Outcome: []apiXML.MarketDescriptionOutcome{
			{ID: "1", Name: "a1"},
		}},
	}
	newEntry := func() *LocalizedMarketDescription {
		return &LocalizedMarketDescription{
			id:       9,
			name:     make(map[types.Locale]string),
			outcomes: make(map[string]*LocalizedOutcomeDescription),
		}
	}
	enStart := time.Now()
	deStart := enStart.Add(10 * time.Millisecond)
	fresh := func(types.Locale) bool { return false } // both marks fresh

	// Order A: en finishes first, de second.
	a := newEntry()
	a.merge(enRow, types.EnLocale, enStart, fresh)
	a.merge(deRow, types.DeLocale, deStart, fresh)

	// Order B: de finishes first, the earlier-started en row last.
	b := newEntry()
	b.merge(deRow, types.DeLocale, deStart, fresh)
	b.merge(enRow, types.EnLocale, enStart, fresh)

	for name, d := range map[string]*LocalizedMarketDescription{"en-first": a, "de-first": b} {
		snap := d.Snapshot()
		if len(snap.Outcomes) != 2 {
			t.Fatalf("%s: outcomes = %+v, want both (completion order must not truncate the set)", name, snap.Outcomes)
		}
		missing := d.missingLocales([]types.Locale{types.EnLocale, types.DeLocale})
		if len(missing) != 1 || missing[0] != types.DeLocale {
			t.Fatalf("%s: missingLocales = %v, want [de] (the disagreement must surface as incomplete)", name, missing)
		}
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

// TestMarketDescriptionCache_NameOnlyEntryCannotCoverVacuously is the
// regression for ragnar-cr F003. Chain: (1) market 9 loads well-formed
// in de; (2) the en row arrives with NO <outcomes> block — pre-fix the
// existing-entry path skipped the malformed validation, deleted the
// whole malformed record, and merged the row's NAME; (3) the de
// catalog later drops market 9, reconciliation removes de from the
// entry — emptying the outcomes map — and the name-only entry
// survived. coversLocaleLocked(en) then passed VACUOUSLY (the outcome
// loop over an empty map), so by-id [en] served an outcome-less market
// as a valid description and the bulk view listed it. Post-fix a
// malformed row records evidence and contributes nothing, and an
// entry whose outcomes empty out is dropped — so the en read reports
// the typed ErrMarketLocaleIncomplete instead.
func TestMarketDescriptionCache_NameOnlyEntryCannotCoverVacuously(t *testing.T) {
	// Market 1 is present and well-formed everywhere so responses never
	// go empty (the empty-response guard must not mask the reconcile).
	deV1 := `<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="1" name="1x2"><outcomes><outcome id="1" name="home"/></outcomes></market>
  <market id="9" name="Schlicht"><outcomes><outcome id="1" name="a1"/></outcomes></market>
</market_descriptions>`
	deV2 := `<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="1" name="1x2"><outcomes><outcome id="1" name="home"/></outcomes></market>
</market_descriptions>`
	enMalformed := `<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="1" name="1x2"><outcomes><outcome id="1" name="home"/></outcomes></market>
  <market id="9" name="Plain"/>
</market_descriptions>`

	var deBody atomic.Pointer[string]
	deBody.Store(&deV1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if strings.Contains(r.URL.Path, "/de/") {
			_, _ = io.WriteString(w, *deBody.Load())
			return
		}
		_, _ = io.WriteString(w, enMalformed)
	}))
	t.Cleanup(srv.Close)
	mc := newMarketDescriptionCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))
	mc.catalogTTL = 50 * time.Millisecond
	ctx := t.Context()

	// (1) well-formed de row seeds the entry.
	if _, err := mc.MarketDescriptionByID(ctx, 9, types.None[string](), []types.Locale{types.DeLocale}); err != nil {
		t.Fatalf("de seed lookup: %v", err)
	}
	// (2) the malformed en row on the EXISTING entry: typed incomplete,
	// and the evidence must be recorded for the entry-missing path.
	if _, err := mc.MarketDescriptionByID(ctx, 9, types.None[string](), []types.Locale{types.EnLocale}); !errors.Is(err, ErrMarketLocaleIncomplete) {
		t.Fatalf("en lookup on malformed row: err = %v, want ErrMarketLocaleIncomplete", err)
	}
	mc.mu.RLock()
	_, recorded := mc.malformed[CompositeKey{MarketID: 9}][types.EnLocale]
	mc.mu.RUnlock()
	if !recorded {
		t.Fatal("malformed evidence for the en row not recorded on the existing-entry path")
	}

	// (3) de drops market 9; marks lapse; the de reconcile runs.
	deBody.Store(&deV2)
	time.Sleep(80 * time.Millisecond)
	if _, err := mc.MarketDescriptionByID(ctx, 9, types.None[string](), []types.Locale{types.DeLocale}); err == nil {
		t.Fatal("de lookup after upstream removal: want an error")
	}

	// The vacuous-coverage read: pre-fix this SUCCEEDED with an
	// outcome-less market.
	_, err := mc.MarketDescriptionByID(ctx, 9, types.None[string](), []types.Locale{types.EnLocale})
	if !errors.Is(err, ErrMarketLocaleIncomplete) {
		t.Fatalf("en lookup after de removal: err = %v, want ErrMarketLocaleIncomplete (pre-fix: valid zero-outcome market)", err)
	}
	if errors.Is(err, ErrItemNotFoundInCache) {
		t.Fatalf("err = %v, must not read as definitive not-found while the en catalog still carries the (broken) row", err)
	}

	// And the bulk view must not list the outcome-less market either.
	all, err := mc.LocalizedMarketDescriptions(ctx, types.EnLocale)
	if err != nil {
		t.Fatalf("bulk read: %v", err)
	}
	if _, ok := all[CompositeKey{MarketID: 9}]; ok {
		t.Fatal("bulk view lists the outcome-less market 9")
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
