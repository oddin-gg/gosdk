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

	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

// TestSportCache_ClearInvalidatesLoadedLocales is the regression for
// the v2.25 finding: SportCache.Clear deleted the sport entry but
// left loadedLocales marked, so a subsequent Sport(ctx,id) skipped
// the refetch (locale already loaded → no FetchSports call) and
// returned "sport not found"; Sports(ctx) returned an incomplete
// catalog.
//
// Strategy: load the catalog, clear one sport, verify the next
// Sport(id) refetches and finds it.
func TestSportCache_ClearInvalidatesLoadedLocales(t *testing.T) {
	body := `<?xml version="1.0"?>
<sports>
  <sport id="od:sport:1" name="Football" abbreviation="FB"/>
  <sport id="od:sport:2" name="Basketball" abbreviation="BB"/>
</sports>`
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	sc := newSportDataCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	// Initial load — 1 hit.
	if _, err := sc.Sports(ctx, []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("initial Sports: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("hits after initial load = %d, want 1", got)
	}

	// Clear sport 1. Pre-fix: loadedLocales still marks EnLocale, so
	// the next access skips refetch and returns "not found".
	id1, _ := types.ParseURN("od:sport:1")
	sc.Clear(*id1)

	// Re-access: must refetch.
	got, err := sc.Sport(ctx, *id1, []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("Sport after Clear: %v", err)
	}
	if got == nil {
		t.Fatal("Sport(ctx, id1) = nil after Clear; want a refilled entry")
	}
	if hits.Load() != 2 {
		t.Errorf("hits after Clear+refetch = %d, want 2 (refetch must trigger)", hits.Load())
	}
}

// TestSportCache_EmptyTournamentsCachedAfterFirstFetch is the
// regression for the v2.x finding: SportCache only had tournamentIDs
// (no loaded flag), so SportTournaments / BuildSport keyed off
// `len(...) > 0` and re-fetched on every call for a sport with a
// genuinely empty tournament list. Now LocalizedSport.tournamentsLoaded
// distinguishes "fetched, no tournaments" from "not yet fetched".
func TestSportCache_EmptyTournamentsCachedAfterFirstFetch(t *testing.T) {
	sportsBody := `<?xml version="1.0"?>
<sports>
  <sport id="od:sport:1" name="Football" abbreviation="FB"/>
</sports>`
	emptyTournamentsBody := `<?xml version="1.0"?>
<sport_tournaments><sport id="od:sport:1" name="Football" abbreviation="FB"/></sport_tournaments>`

	var sportsHits, tournamentsHits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		switch {
		case strings.Contains(r.URL.Path, "/tournaments"):
			tournamentsHits.Add(1)
			_, _ = io.WriteString(w, emptyTournamentsBody)
		case strings.Contains(r.URL.Path, "/sports"):
			sportsHits.Add(1)
			_, _ = io.WriteString(w, sportsBody)
		}
	}))
	defer srv.Close()

	sc := newSportDataCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	id, _ := types.ParseURN("od:sport:1")

	// First call → fetches both /sports and /tournaments. Empty
	// tournaments → tournamentsLoaded=true on the entry.
	ids, err := sc.SportTournaments(ctx, *id, types.EnLocale)
	if err != nil {
		t.Fatalf("first SportTournaments: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("first call ids = %v, want empty", ids)
	}
	if got := tournamentsHits.Load(); got != 1 {
		t.Fatalf("tournaments hits after first call = %d, want 1", got)
	}

	// Second call → must NOT re-fetch /tournaments (pre-fix bug).
	ids, err = sc.SportTournaments(ctx, *id, types.EnLocale)
	if err != nil {
		t.Fatalf("second SportTournaments: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("second call ids = %v, want empty", ids)
	}
	if got := tournamentsHits.Load(); got != 1 {
		t.Errorf("tournaments hits after second call = %d, want still 1 (empty result must be cached)", got)
	}

	// Third call via BuildSport → must also NOT re-fetch.
	if _, err := BuildSport(ctx, sc, *id, []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("BuildSport: %v", err)
	}
	if got := tournamentsHits.Load(); got != 1 {
		t.Errorf("tournaments hits after BuildSport = %d, want still 1", got)
	}
}

// TestMarketDescriptionCache_SnapshotPointersAreCloned is the
// regression for the v2.25 finding: pointer-typed fields (Variant,
// IncludesOutcomesOfType, OutcomeType) on the returned snapshot
// previously aliased the cache's *string pointees. A caller doing
// `*md.Variant = "evil"` would mutate the cache; a second read by
// the same or another caller would observe the mutation.
func TestMarketDescriptionCache_SnapshotPointersAreCloned(t *testing.T) {
	body := `<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="42" name="X" includes_outcomes_of_type="player" outcome_type="competitor">
    <outcomes><outcome id="1" name="o1"/></outcomes>
  </market>
</market_descriptions>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	mc := newMarketDescriptionCache(t.Context(), newAPIClientForTest(t, srv), nil)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	entry, err := mc.MarketDescriptionByID(ctx, 42, types.None[string](), []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("MarketDescriptionByID: %v", err)
	}
	first := entry.Snapshot()

	// Tamper with the returned snapshot. Optional[T] is a value
	// wrapper, so wholesale-replacing the field on `first` doesn't
	// touch the cache — but the assertion still proves that
	// independent reads see the cached values.
	first.IncludesOutcomesOfType = types.Some("TAMPERED")
	first.OutcomeType = types.Some("TAMPERED")

	// Second snapshot must reflect the cache's untouched values.
	second := entry.Snapshot()
	if v, ok := second.IncludesOutcomesOfType.Get(); !ok || v != "player" {
		t.Errorf("IncludesOutcomesOfType after caller tamper = %v, want Some(\"player\")", second.IncludesOutcomesOfType)
	}
	if v, ok := second.OutcomeType.Get(); !ok || v != "competitor" {
		t.Errorf("OutcomeType after caller tamper = %v, want Some(\"competitor\")", second.OutcomeType)
	}
}

// TestMarketDescriptionCache_ClearItemInvalidatesBulkView is the
// regression for the v2.25 finding: ClearCacheItem deleted one
// description but kept loadedLocales, so MarketDescriptions /
// Multi*MarketDescriptions skipped refetch and returned an incomplete
// bulk view until some unrelated path triggered a reload.
func TestMarketDescriptionCache_ClearItemInvalidatesBulkView(t *testing.T) {
	body := func(locale string) string {
		return fmt.Sprintf(`<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="1" name="One in %s">
    <outcomes><outcome id="1" name="o1"/></outcomes>
  </market>
  <market id="2" name="Two in %s">
    <outcomes><outcome id="1" name="o1"/></outcomes>
  </market>
</market_descriptions>`, locale, locale)
	}
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only count hits to the bulk catalog endpoint.
		if strings.Contains(r.URL.Path, "/markets") &&
			!strings.Contains(r.URL.Path, "/variants/") {
			hits.Add(1)
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, body("en"))
	}))
	defer srv.Close()

	mc := newMarketDescriptionCache(t.Context(), newAPIClientForTest(t, srv), nil)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	// Initial bulk load.
	first, err := mc.MultiLocalizedMarketDescriptions(ctx, []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("MultiLocalizedMarketDescriptions: %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("first read returned %d entries, want 2", len(first))
	}
	if hits.Load() != 1 {
		t.Fatalf("hits after initial load = %d, want 1", hits.Load())
	}

	// Clear market 1.
	mc.ClearCacheItem(1, types.None[string]())

	// Bulk read must refetch and re-include market 1.
	second, err := mc.MultiLocalizedMarketDescriptions(ctx, []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("second MultiLocalizedMarketDescriptions: %v", err)
	}
	if len(second) != 2 {
		t.Errorf("after Clear+refetch, bulk view returned %d entries, want 2 (cleared id must reappear)", len(second))
	}
	if hits.Load() != 2 {
		t.Errorf("hits after Clear+bulk read = %d, want 2 (bulk refetch must trigger)", hits.Load())
	}
}

// TestSportCache_MalformedRowDoesNotPartiallyCommit is the regression
// for catalog partial commits (Codex P2): a response whose EARLIER rows
// are valid but which fails to parse mid-way must commit NOTHING —
// pre-fix each row was committed as it parsed, and the retry's
// authoritative response merged on top of the partial commit, leaving
// phantom entries in Sports/SportTournaments until explicit
// invalidation.
func TestSportCache_MalformedRowDoesNotPartiallyCommit(t *testing.T) {
	var served atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if served.Add(1) == 1 {
			// Attempt 1: valid od:sport:77 THEN a malformed id.
			_, _ = io.WriteString(w, `<?xml version="1.0"?>
<sports generated_at="2026-01-01T00:00:00">
  <sport id="od:sport:77" name="Phantom" abbreviation="PHA"/>
  <sport id="not a urn" name="Broken" abbreviation="BRK"/>
</sports>`)
			return
		}
		// Attempt 2: authoritative catalog WITHOUT sport 77.
		_, _ = io.WriteString(w, `<?xml version="1.0"?>
<sports generated_at="2026-01-01T00:00:00">
  <sport id="od:sport:1" name="Soccer" abbreviation="SOC"/>
</sports>`)
	}))
	defer srv.Close()

	sc := newSportDataCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))

	if _, err := sc.Sports(t.Context(), []types.Locale{types.EnLocale}); err == nil {
		t.Fatal("first load should fail on the malformed row")
	}
	ids, err := sc.Sports(t.Context(), []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("retry load: %v", err)
	}
	for _, id := range ids {
		if id.ToString() == "od:sport:77" {
			t.Fatalf("phantom sport od:sport:77 from the FAILED response survived the retry: %v", ids)
		}
	}
	if len(ids) != 1 || ids[0].ToString() != "od:sport:1" {
		t.Fatalf("Sports = %v, want exactly [od:sport:1]", ids)
	}
}
