package cache

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// TestSportCache_RemovedSportReconciled pins the catalog refresh as a
// REPLACE: a sport absent from the fresh response must stop being
// served. Pre-fix the sports map was upsert-only — Sports() listed a
// removed sport for the process lifetime, and building it then failed
// on its tournament fetch.
func TestSportCache_RemovedSportReconciled(t *testing.T) {
	srv := newSportSrv(t, twoSportCatalog, noTournaments)
	sc := newSportDataCache(t.Context(), newAPIClientForTest(t, srv.Server), log.New(nil))
	sc.catalogTTL = 50 * time.Millisecond

	ids, err := sc.Sports(t.Context(), []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("initial Sports: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("Sports = %v, want 2 entries", ids)
	}

	srv.serveSports(oneSportCatalog) // sport 2 removed upstream
	time.Sleep(80 * time.Millisecond)

	ids, err = sc.Sports(t.Context(), []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("Sports after removal: %v", err)
	}
	if len(ids) != 1 || ids[0] != sportOne {
		t.Fatalf("Sports = %v, want only %s (removed sport must not linger)", ids, sportOne.ToString())
	}
	if _, err := sc.Sport(t.Context(), sportTwo, []types.Locale{types.EnLocale}); !errors.Is(err, ErrItemNotFoundInCache) {
		t.Fatalf("Sport(removed) err = %v, want ErrItemNotFoundInCache", err)
	}
}

// TestSportCache_EmptyResponseDoesNotWipeCatalog: mirrors the market
// cache's guard — an empty-but-successful catalog response must not be
// treated as "every sport was removed upstream" (pre-fix it wiped the
// sports map and marked the locale loaded, serving not-found for every
// sport for a full catalogTTL).
func TestSportCache_EmptyResponseDoesNotWipeCatalog(t *testing.T) {
	srv := newSportSrv(t, twoSportCatalog, noTournaments)
	sc := newSportDataCache(t.Context(), newAPIClientForTest(t, srv.Server), log.New(nil))
	sc.catalogTTL = 50 * time.Millisecond

	if _, err := sc.Sports(t.Context(), []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("seed Sports: %v", err)
	}

	srv.serveSports(`<?xml version="1.0"?><sports/>`)
	time.Sleep(80 * time.Millisecond)

	ids, err := sc.Sports(t.Context(), []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("Sports across empty response: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("Sports = %v, want 2 entries (empty response must not wipe the catalog)", ids)
	}

	// A real (non-empty) removal still reconciles.
	srv.serveSports(oneSportCatalog)
	time.Sleep(80 * time.Millisecond)
	ids, err = sc.Sports(t.Context(), []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("Sports after recovery: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("Sports = %v, want 1 (non-empty response keeps removal authority)", ids)
	}
}

// TestSportCache_StalePreClearFlightLoses mirrors the market cache's
// stale-flight regression (ragnar-cr run-3 F003): a pre-clear (gen-0)
// catalog flight finishing after a post-clear (gen-1) flight must not
// overwrite the fresh names or re-create sports the newer reconcile
// removed. Pre-fix only the old flight's reconcile and locale mark
// were suppressed — its row stores landed and, with the fresh mark
// standing, served stale/resurrected sports for up to catalogTTL.
func TestSportCache_StalePreClearFlightLoses(t *testing.T) {
	v1 := `<?xml version="1.0"?>
<sports><sport id="od:sport:1" name="Football" abbreviation="FB"/><sport id="od:sport:2" name="Tennis" abbreviation="TN"/><sport id="od:sport:3" name="Chess" abbreviation="CH"/></sports>`
	// Post-clear upstream state: sport 1 renamed, sport 2 removed
	// (sport 3 is the explicitly cleared one).
	v2 := `<?xml version="1.0"?>
<sports><sport id="od:sport:1" name="Footy" abbreviation="FB"/></sports>`

	var body atomic.Pointer[string]
	body.Store(&v1)
	var gateArmed atomic.Bool
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if gateArmed.CompareAndSwap(true, false) {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
			_, _ = io.WriteString(w, v1) // the pre-clear catalog view
			return
		}
		_, _ = io.WriteString(w, *body.Load())
	}))
	t.Cleanup(srv.Close)
	sc := newSportDataCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))
	sc.catalogTTL = 50 * time.Millisecond

	if _, err := sc.Sports(t.Context(), []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("seed Sports: %v", err)
	}
	body.Store(&v2)
	gateArmed.Store(true)
	time.Sleep(80 * time.Millisecond)

	// gen-0 flight starts and blocks inside the fixture.
	aDone := make(chan error, 1)
	go func() {
		_, err := sc.Sports(t.Context(), []types.Locale{types.EnLocale})
		aDone <- err
	}()
	<-entered

	// Clear mid-flight; the newer gen-1 flight commits v2 first.
	sc.Clear(types.URN{Prefix: "od", Type: "sport", ID: 3})
	ids, err := sc.Sports(t.Context(), []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("post-clear Sports: %v", err)
	}
	if len(ids) != 1 || ids[0] != sportOne {
		t.Fatalf("Sports = %v, want only %s from the fresh flight", ids, sportOne.ToString())
	}

	// The stale pre-clear flight finishes last: all its stores must lose.
	close(release)
	if err := <-aDone; err != nil {
		t.Fatalf("gen-0 caller: %v (a superseded flight must not fail the read)", err)
	}

	ids, err = sc.Sports(t.Context(), []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("final Sports: %v", err)
	}
	if len(ids) != 1 || ids[0] != sportOne {
		t.Fatalf("Sports = %v, want only %s (stale flight resurrected a reconciled-away sport)", ids, sportOne.ToString())
	}
	sc.mu.RLock()
	entry := sc.sports[sportOne]
	sc.mu.RUnlock()
	entry.mu.RLock()
	name := entry.name[types.EnLocale]
	entry.mu.RUnlock()
	if name != "Footy" {
		t.Fatalf("sport 1 name = %q, want %q (stale flight overwrote the fresh name)", name, "Footy")
	}
}

// TestSportCache_SupersededFlightJoinsWinner mirrors the market cache's
// join-the-winner regression (ragnar-cr run-5 F001): a superseded
// catalog flight must not answer its callers from the winner's
// mid-commit state — for a cleared sport the winner had not yet
// re-created, that read transiently dropped it from Sports().
func TestSportCache_SupersededFlightJoinsWinner(t *testing.T) {
	v1 := `<?xml version="1.0"?>
<sports><sport id="od:sport:1" name="Football" abbreviation="FB"/><sport id="od:sport:2" name="Tennis" abbreviation="TN"/></sports>`
	v2 := `<?xml version="1.0"?>
<sports><sport id="od:sport:1" name="Footy" abbreviation="FB"/><sport id="od:sport:2" name="Tennis" abbreviation="TN"/></sports>`

	var body atomic.Pointer[string]
	body.Store(&v1)
	var gateArmed atomic.Bool
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if gateArmed.CompareAndSwap(true, false) {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
			_, _ = io.WriteString(w, v1)
			return
		}
		_, _ = io.WriteString(w, *body.Load())
	}))
	t.Cleanup(srv.Close)

	var hookArmed atomic.Bool
	winnerEntered := make(chan struct{}, 1)
	winnerRelease := make(chan struct{})
	sportCommitGateHook = func(types.Locale) {
		if hookArmed.CompareAndSwap(true, false) {
			select {
			case winnerEntered <- struct{}{}:
			default:
			}
			<-winnerRelease
		}
	}
	t.Cleanup(func() { sportCommitGateHook = nil })

	sc := newSportDataCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))
	sc.catalogTTL = 50 * time.Millisecond

	if _, err := sc.Sports(t.Context(), []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("seed Sports: %v", err)
	}
	body.Store(&v2)
	gateArmed.Store(true)
	time.Sleep(80 * time.Millisecond)

	type result struct {
		ids []types.URN
		err error
	}
	aDone := make(chan result, 1)
	go func() {
		ids, err := sc.Sports(t.Context(), []types.Locale{types.EnLocale})
		aDone <- result{ids: ids, err: err}
	}()
	<-entered

	// Sport 1 is cleared mid-flight; the winner starts, wins the cursor,
	// and pauses before re-creating it.
	sc.Clear(sportOne)
	hookArmed.Store(true)
	bDone := make(chan error, 1)
	go func() {
		_, err := sc.Sports(t.Context(), []types.Locale{types.EnLocale})
		bDone <- err
	}()
	<-winnerEntered

	close(release)
	select {
	case r := <-aDone:
		t.Fatalf("superseded caller answered mid-commit: (%v, %v) — must wait for the winner", r.ids, r.err)
	case <-time.After(150 * time.Millisecond):
	}

	close(winnerRelease)
	if err := <-bDone; err != nil {
		t.Fatalf("winner caller: %v", err)
	}
	r := <-aDone
	if r.err != nil {
		t.Fatalf("superseded caller: %v", r.err)
	}
	if len(r.ids) != 2 {
		t.Fatalf("superseded caller saw Sports = %v, want both sports (pre-fix: the cleared sport transiently vanished)", r.ids)
	}
}

// TestSportCache_ReconcilePerLocale: the catalog response is
// authoritative for ITS locale only — a sport present in en but absent
// from the de catalog keeps its en data and stays reachable in en;
// multi-locale reads report the de gap as the typed incomplete.
func TestSportCache_ReconcilePerLocale(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if strings.Contains(r.URL.Path, "/de/") {
			_, _ = io.WriteString(w, oneSportCatalog) // de: sport 1 only
			return
		}
		_, _ = io.WriteString(w, twoSportCatalog) // en: sports 1 and 2
	}))
	t.Cleanup(srv.Close)
	sc := newSportDataCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))

	if _, err := sc.Sports(t.Context(), []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("en Sports: %v", err)
	}
	if _, err := sc.Sports(t.Context(), []types.Locale{types.DeLocale}); err != nil {
		t.Fatalf("de Sports: %v", err)
	}

	// The de reconcile must not evict sport 2's en data.
	if _, err := sc.Sport(t.Context(), sportTwo, []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("Sport(2, en) after de load: %v (another locale's reconcile must not evict en data)", err)
	}
	if _, err := sc.Sport(t.Context(), sportTwo, []types.Locale{types.EnLocale, types.DeLocale}); !errors.Is(err, ErrSportLocaleIncomplete) {
		t.Fatalf("Sport(2, [en,de]) err = %v, want ErrSportLocaleIncomplete", err)
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

// TestSportCache_StaleTournamentCommitRejected is the focused
// regression for the lost-update window that REPLACE + expiry open
// together (neither opens it alone).
//
// Post-expiry refreshes for one sport can run concurrently, and an
// earlier-STARTED fetch that finishes LAST would reinstall its older
// set over a newer one and stamp it fresh — serving a set already known
// to be superseded for up to a full catalogTTL. The commit is
// monotonic: a snapshot no newer than the committed one is rejected.
func TestSportCache_StaleTournamentCommitRejected(t *testing.T) {
	entry := &LocalizedSport{
		id:            sportOne,
		tournamentIDs: make(map[types.URN]struct{}),
		name:          make(map[types.Locale]string),
		abbreviation:  make(map[types.Locale]string),
	}
	older := time.Now()
	newer := older.Add(time.Second)
	tournamentEleven := types.URN{Prefix: "od", Type: "tournament", ID: 11}

	// The newer fetch commits first (it finished first).
	if !entry.replaceTournaments([]types.URN{tournamentEleven}, newer) {
		t.Fatal("newer snapshot was rejected")
	}
	// The older fetch, started earlier, finishes last.
	if entry.replaceTournaments([]types.URN{tournamentTen}, older) {
		t.Fatal("older snapshot was committed over a newer one")
	}

	got := entry.makeTournamentIDsList()
	if len(got) != 1 || got[0] != tournamentEleven {
		t.Fatalf("tournaments = %v, want only %s (newer snapshot must win)", got, tournamentEleven.ToString())
	}
	// The freshness stamp must not travel backwards either — an older
	// stamp would expire the entry early and re-open the window.
	entry.mu.RLock()
	stamp := entry.tournamentsLoadedAt
	entry.mu.RUnlock()
	if !stamp.Equal(newer) {
		t.Fatalf("tournamentsLoadedAt = %v, want %v (stamp must only advance)", stamp, newer)
	}
}

// TestSportCache_OutOfOrderRefreshKeepsNewest exercises the same race
// end to end. Two locales are two singleflight keys, so they are the
// mechanism for getting two concurrent in-flight refreshes of ONE
// sport's tournament list; the differing bodies stand in for an
// upstream change between the two fetches.
//
// The "en" fetch starts first and is held open until the "de" fetch has
// completed and committed, so the older fetch commits last.
func TestSportCache_OutOfOrderRefreshKeepsNewest(t *testing.T) {
	release := make(chan struct{})
	enStarted := make(chan struct{})
	var enOnce sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if !strings.Contains(r.URL.Path, "/tournaments") {
			_, _ = io.WriteString(w, oneSportCatalog)
			return
		}
		if strings.Contains(r.URL.Path, "/en/") {
			enOnce.Do(func() { close(enStarted) })
			<-release // hold the OLDER fetch open
			_, _ = io.WriteString(w, twoTournaments)
			return
		}
		_, _ = io.WriteString(w, oneTournament) // the NEWER snapshot
	}))
	defer srv.Close()

	sc := newSportDataCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := sc.SportTournaments(t.Context(), sportOne, types.EnLocale); err != nil {
			t.Errorf("en SportTournaments: %v", err)
		}
	}()

	<-enStarted // the older fetch is in flight
	if _, err := sc.SportTournaments(t.Context(), sportOne, types.Locale("de")); err != nil {
		t.Fatalf("de SportTournaments: %v", err)
	}
	close(release) // now let the older fetch finish and commit
	wg.Wait()

	sc.mu.RLock()
	entry := sc.sports[sportOne]
	sc.mu.RUnlock()
	got := entry.makeTournamentIDsList()
	if len(got) != 1 || got[0] != tournamentTen {
		t.Fatalf("cached tournaments = %v, want only %s (the newer snapshot must survive)", got, tournamentTen.ToString())
	}
}

// TestSportCache_TournamentCommitCannotResurrectRemovedSport pins the
// stub-creation guard (Fixpoint i11): a tournament fetch that STARTED
// before a catalog refresh removed its sport must not re-create the
// sport as a nameless stub when it commits afterwards — the stub
// pinned an obsolete tournament list and flipped Sport() from
// not-found to ErrSportLocaleIncomplete for an entity the fresh
// catalog says does not exist. The caller still receives its fetched
// list; only the cache commit is skipped.
func TestSportCache_TournamentCommitCannotResurrectRemovedSport(t *testing.T) {
	var gateTournaments atomic.Bool
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var sportsBody atomic.Pointer[string]
	body1 := twoSportCatalog
	sportsBody.Store(&body1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if strings.Contains(r.URL.Path, "/tournaments") {
			if gateTournaments.CompareAndSwap(true, false) {
				select {
				case entered <- struct{}{}:
				default:
				}
				<-release
			}
			_, _ = io.WriteString(w, `<?xml version="1.0"?>
<sport_tournaments><sport id="od:sport:2" name="Tennis"/><tournaments>`+
				`<tournament id="od:tournament:10" name="T10"><sport id="od:sport:2" name="Tennis"/></tournament>`+
				`</tournaments></sport_tournaments>`)
			return
		}
		_, _ = io.WriteString(w, *sportsBody.Load())
	}))
	t.Cleanup(srv.Close)
	sc := newSportDataCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))
	sc.catalogTTL = 50 * time.Millisecond

	if _, err := sc.Sports(t.Context(), []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("seed Sports: %v", err)
	}

	// The tournament fetch for sport 2 starts and blocks.
	gateTournaments.Store(true)
	tDone := make(chan error, 1)
	go func() {
		_, err := sc.SportTournaments(t.Context(), sportTwo, types.EnLocale)
		tDone <- err
	}()
	<-entered

	// Meanwhile the catalog refreshes without sport 2 (removed upstream).
	body2 := oneSportCatalog
	sportsBody.Store(&body2)
	time.Sleep(80 * time.Millisecond)
	if _, err := sc.Sports(t.Context(), []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("catalog refresh: %v", err)
	}

	// The older tournament fetch completes: its caller gets the list,
	// but the removed sport must not come back as a stub.
	close(release)
	if err := <-tDone; err != nil {
		t.Fatalf("SportTournaments: %v (the caller still gets its fetched list)", err)
	}
	ids, err := sc.Sports(t.Context(), []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("Sports after commit: %v", err)
	}
	if len(ids) != 1 || ids[0] != sportOne {
		t.Fatalf("Sports = %v, want only %s (stub resurrected a removed sport)", ids, sportOne.ToString())
	}
	if _, err := sc.Sport(t.Context(), sportTwo, []types.Locale{types.EnLocale}); !errors.Is(err, ErrItemNotFoundInCache) {
		t.Fatalf("Sport(removed) err = %v, want ErrItemNotFoundInCache (not a locale-gap error from a stub)", err)
	}
}

// TestSportCache_IconPathSurvivesIconlessLocale: icon_path is
// locale-independent and optional on the wire — a locale whose catalog
// row omits it must not erase the icon another locale supplied
// (pre-fix the unconditional assign let refresh ordering decide which
// value survived).
func TestSportCache_IconPathSurvivesIconlessLocale(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if strings.Contains(r.URL.Path, "/en/") {
			_, _ = io.WriteString(w, `<?xml version="1.0"?>
<sports><sport id="od:sport:1" name="Football" abbreviation="FB" icon_path="/icons/fb.png"/></sports>`)
			return
		}
		_, _ = io.WriteString(w, oneSportCatalog) // de: no icon_path attribute
	}))
	t.Cleanup(srv.Close)
	sc := newSportDataCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))

	if _, err := sc.Sports(t.Context(), []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("en Sports: %v", err)
	}
	if _, err := sc.Sports(t.Context(), []types.Locale{types.DeLocale}); err != nil {
		t.Fatalf("de Sports: %v", err)
	}

	entry, err := sc.Sport(t.Context(), sportOne, []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("Sport: %v", err)
	}
	snap := entry.summarySnapshot()
	if v, ok := snap.IconPath.Get(); !ok || v != "/icons/fb.png" {
		t.Fatalf("IconPath = %v, want /icons/fb.png (icon-less de row must not erase it)", snap.IconPath)
	}
}

// TestSportCache_ConcurrentRefreshCoalesced pins the herd control.
// Expiry turns a once-per-process fetch into a recurring one, so
// without coalescing every caller finding the list stale at the same
// moment issues its own FetchTournaments.
func TestSportCache_ConcurrentRefreshCoalesced(t *testing.T) {
	srv := newSportSrv(t, oneSportCatalog, oneTournament)
	sc := newSportDataCache(t.Context(), newAPIClientForTest(t, srv.Server), log.New(nil))

	const callers = 12
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := sc.SportTournaments(t.Context(), sportOne, types.EnLocale); err != nil {
				t.Errorf("SportTournaments: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := srv.tourHits.Load(); got != 1 {
		t.Fatalf("tournament endpoint hits = %d, want 1 (%d concurrent callers must coalesce)", got, callers)
	}
}
