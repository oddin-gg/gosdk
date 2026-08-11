package cache

import (
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

// sportSrv serves a swappable sport catalog and per-sport tournament
// list, counting hits on each endpoint.
type sportSrv struct {
	*httptest.Server
	sports      atomic.Pointer[string]
	tournaments atomic.Pointer[string]
	sportHits   atomic.Int64
	tourHits    atomic.Int64
}

func newSportSrv(t *testing.T, sports, tournaments string) *sportSrv {
	t.Helper()
	s := &sportSrv{}
	s.sports.Store(&sports)
	s.tournaments.Store(&tournaments)
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if strings.Contains(r.URL.Path, "/tournaments") {
			s.tourHits.Add(1)
			_, _ = io.WriteString(w, *s.tournaments.Load())
			return
		}
		s.sportHits.Add(1)
		_, _ = io.WriteString(w, *s.sports.Load())
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *sportSrv) serveSports(body string)      { s.sports.Store(&body) }
func (s *sportSrv) serveTournaments(body string) { s.tournaments.Store(&body) }

const (
	oneSportCatalog = `<?xml version="1.0"?>
<sports><sport id="od:sport:1" name="Football" abbreviation="FB"/></sports>`
	twoSportCatalog = `<?xml version="1.0"?>
<sports><sport id="od:sport:1" name="Football" abbreviation="FB"/><sport id="od:sport:2" name="Tennis" abbreviation="TN"/></sports>`

	// Every <tournament> carries the nested <sport> the API client's
	// per-row identity check requires.
	oneTournament = `<?xml version="1.0"?>
<sport_tournaments><sport id="od:sport:1" name="Football"/><tournaments>` +
		`<tournament id="od:tournament:10" name="T10"><sport id="od:sport:1" name="Football"/></tournament>` +
		`</tournaments></sport_tournaments>`
	twoTournaments = `<?xml version="1.0"?>
<sport_tournaments><sport id="od:sport:1" name="Football"/><tournaments>` +
		`<tournament id="od:tournament:10" name="T10"><sport id="od:sport:1" name="Football"/></tournament>` +
		`<tournament id="od:tournament:11" name="T11"><sport id="od:sport:1" name="Football"/></tournament>` +
		`</tournaments></sport_tournaments>`
	noTournaments = `<?xml version="1.0"?>
<sport_tournaments><sport id="od:sport:1" name="Football"/></sport_tournaments>`
)

var (
	sportOne      = types.URN{Prefix: "od", Type: "sport", ID: 1}
	sportTwo      = types.URN{Prefix: "od", Type: "sport", ID: 2}
	tournamentTen = types.URN{Prefix: "od", Type: "tournament", ID: 10}
)

// TestSportCache_CatalogRefetchedAfterTTL pins the sport-catalog
// refresh. Pre-fix loadedLocales was permanent for the process
// lifetime, so FetchSports ran exactly once and a sport added upstream
// stayed invisible to a long-running consumer until it restarted.
func TestSportCache_CatalogRefetchedAfterTTL(t *testing.T) {
	srv := newSportSrv(t, oneSportCatalog, noTournaments)
	sc := newSportDataCache(t.Context(), newAPIClientForTest(t, srv.Server), log.New(nil))
	sc.catalogTTL = 50 * time.Millisecond

	ids, err := sc.Sports(t.Context(), []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("initial Sports: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("Sports = %v, want 1 entry", ids)
	}

	srv.serveSports(twoSportCatalog) // a new sport appears upstream
	time.Sleep(80 * time.Millisecond)

	ids, err = sc.Sports(t.Context(), []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("Sports after TTL: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("Sports = %v, want 2 entries (upstream addition must land)", ids)
	}
	if got := srv.sportHits.Load(); got != 2 {
		t.Fatalf("catalog hits = %d, want 2 (the loaded-locale mark must expire)", got)
	}
}

// TestSportCache_CatalogNotRefetchedWithinTTL is the other half: inside
// the window the catalog is served from cache, so the expiring mark
// costs one download per TTL, not one per read.
func TestSportCache_CatalogNotRefetchedWithinTTL(t *testing.T) {
	srv := newSportSrv(t, oneSportCatalog, noTournaments)
	sc := newSportDataCache(t.Context(), newAPIClientForTest(t, srv.Server), log.New(nil))
	sc.catalogTTL = time.Hour

	for i := range 5 {
		if _, err := sc.Sports(t.Context(), []types.Locale{types.EnLocale}); err != nil {
			t.Fatalf("Sports %d: %v", i, err)
		}
	}
	if got := srv.sportHits.Load(); got != 1 {
		t.Fatalf("catalog hits = %d, want 1 (no refetch inside the TTL window)", got)
	}
}

// TestSportCache_TournamentsRefetchedAfterTTL pins the per-sport
// tournament refresh. tournamentsLoaded was a permanent bool, so a
// sport's tournament list was fetched once per process — a tournament
// added upstream (a new weekly league) never appeared.
func TestSportCache_TournamentsRefetchedAfterTTL(t *testing.T) {
	srv := newSportSrv(t, oneSportCatalog, oneTournament)
	sc := newSportDataCache(t.Context(), newAPIClientForTest(t, srv.Server), log.New(nil))
	sc.catalogTTL = 50 * time.Millisecond

	got, err := sc.SportTournaments(t.Context(), sportOne, types.EnLocale)
	if err != nil {
		t.Fatalf("initial SportTournaments: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("tournaments = %v, want 1", got)
	}

	srv.serveTournaments(twoTournaments)
	time.Sleep(80 * time.Millisecond)

	got, err = sc.SportTournaments(t.Context(), sportOne, types.EnLocale)
	if err != nil {
		t.Fatalf("SportTournaments after TTL: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("tournaments = %v, want 2 (upstream addition must land)", got)
	}
	if got := srv.tourHits.Load(); got != 2 {
		t.Fatalf("tournament hits = %d, want 2", got)
	}
}

// TestSportCache_TournamentsNotRefetchedWithinTTL keeps the
// empty-result contract intact: a sport with genuinely zero
// tournaments must still be served from cache inside the window rather
// than re-fetched on every call.
func TestSportCache_TournamentsNotRefetchedWithinTTL(t *testing.T) {
	srv := newSportSrv(t, oneSportCatalog, noTournaments)
	sc := newSportDataCache(t.Context(), newAPIClientForTest(t, srv.Server), log.New(nil))
	sc.catalogTTL = time.Hour

	for i := range 5 {
		got, err := sc.SportTournaments(t.Context(), sportOne, types.EnLocale)
		if err != nil {
			t.Fatalf("SportTournaments %d: %v", i, err)
		}
		if len(got) != 0 {
			t.Fatalf("tournaments = %v, want empty", got)
		}
	}
	if got := srv.tourHits.Load(); got != 1 {
		t.Fatalf("tournament hits = %d, want 1 (empty list must stay cached inside the TTL)", got)
	}
}

// TestSportCache_TournamentsReplacedNotMerged pins the refresh as a
// REPLACE. The commit path used to merge each fetched id into the
// existing set, which was harmless while the list was loaded exactly
// once — but with a refreshing list a merge accumulates every
// tournament the sport has ever had, so one removed upstream would
// linger in SportTournaments/Sports views forever.
func TestSportCache_TournamentsReplacedNotMerged(t *testing.T) {
	srv := newSportSrv(t, oneSportCatalog, twoTournaments)
	sc := newSportDataCache(t.Context(), newAPIClientForTest(t, srv.Server), log.New(nil))
	sc.catalogTTL = 50 * time.Millisecond

	got, err := sc.SportTournaments(t.Context(), sportOne, types.EnLocale)
	if err != nil {
		t.Fatalf("initial SportTournaments: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("tournaments = %v, want 2", got)
	}

	srv.serveTournaments(oneTournament) // tournament 11 removed upstream
	time.Sleep(80 * time.Millisecond)

	got, err = sc.SportTournaments(t.Context(), sportOne, types.EnLocale)
	if err != nil {
		t.Fatalf("SportTournaments after TTL: %v", err)
	}
	if len(got) != 1 || got[0] != tournamentTen {
		t.Fatalf("tournaments = %v, want only %s (removal must land)", got, tournamentTen.ToString())
	}

	// And the cached view agrees — not just the returned slice.
	sc.mu.RLock()
	entry := sc.sports[sportOne]
	sc.mu.RUnlock()
	if cached := entry.makeTournamentIDsList(); len(cached) != 1 {
		t.Fatalf("cached tournament list = %v, want 1 (stale id must not linger)", cached)
	}
}

// TestSportCache_ClearStillForcesRefetch guards the invalidation path
// against the timestamp change: Clear must still reset the locale
// marks, independent of the TTL window.
func TestSportCache_ClearStillForcesRefetch(t *testing.T) {
	srv := newSportSrv(t, twoSportCatalog, noTournaments)
	sc := newSportDataCache(t.Context(), newAPIClientForTest(t, srv.Server), log.New(nil))
	sc.catalogTTL = time.Hour // clear must win regardless of freshness

	if _, err := sc.Sports(t.Context(), []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("initial Sports: %v", err)
	}
	sc.Clear(sportTwo)

	ids, err := sc.Sports(t.Context(), []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("Sports after clear: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("Sports = %v, want 2 (clear must refetch the catalog)", ids)
	}
	if got := srv.sportHits.Load(); got != 2 {
		t.Fatalf("catalog hits = %d, want 2", got)
	}
}
