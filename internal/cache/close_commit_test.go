package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/apipolicy"
	"github.com/oddin-gg/gosdk/internal/cache/lru"
	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

// Every store of every cache is a commit of the manager's lifetime: Close
// returns only after the commits that were running, and a flight that ends
// later stores nothing. Each case holds one flight inside a commit (the
// n-th one of the flight, the first that changes what the cache holds),
// closes the manager and releases the flight; the cache must be what it was
// when CloseCtx returned.

const (
	closeCommitVoidXML = `<?xml version="1.0"?>
<void_reasons response_code="OK"><void_reason id="1" name="stale"/></void_reasons>`
	closeCommitStatusXML = `<?xml version="1.0"?>
<match_status_descriptions response_code="OK"><match_status id="1" description="live"/></match_status_descriptions>`
	closeCommitPlayerXML = `<?xml version="1.0"?>
<player_profile><player id="od:player:300" name="Striker" full_name="Striker McName" sport="od:sport:1"/></player_profile>`
	closeCommitFixtureXML = `<?xml version="1.0"?>
<fixtures_fixture generated_at="2026-01-01T00:00:00">
  <fixture id="od:match:99" name="Home vs Away" scheduled="2026-01-01T20:00:00" start_time="2026-01-01T20:00:00">
    <tournament id="od:tournament:7" name="Cup" abbreviation="CUP" risk_tier="2">
      <sport id="od:sport:1" name="Soccer" abbreviation="SOC"/>
    </tournament>
  </fixture>
</fixtures_fixture>`
	closeCommitRefreshedCatalog = `<?xml version="1.0"?><market_descriptions response_code="OK">` +
		`<market id="9" name="Renamed"><outcomes><outcome id="1" name="renamed-o1"/><outcome id="2" name="o2"/></outcomes></market>` +
		`</market_descriptions>`
)

// closeCommitServer answers every endpoint the caches load from at once.
func closeCommitServer(t *testing.T, catalog *atomic.Pointer[string]) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		var body string
		switch {
		case strings.HasSuffix(p, "/info"):
			body = iconTournamentXML
		case strings.Contains(p, "/competitors/"):
			body = iconCompetitorXML
		case strings.Contains(p, "/players/"):
			body = closeCommitPlayerXML
		case strings.HasSuffix(p, "/fixture"):
			body = closeCommitFixtureXML
		case strings.HasSuffix(p, "/summary"):
			body = matchSummaryBody("od:match:42", "en")
		case strings.HasSuffix(p, "/markets"):
			body = *catalog.Load()
		case strings.HasSuffix(p, "/match_status"):
			body = closeCommitStatusXML
		case strings.HasSuffix(p, "/void_reasons"):
			body = closeCommitVoidXML
		case strings.HasSuffix(p, "/tournaments"):
			body = oneTournament
		case strings.HasSuffix(p, "/sports"):
			body = oneSportCatalog
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

type closeCommitCase struct {
	name string
	// nth is the commit of the flight the case holds.
	nth int
	// commitOnly leaves the case out of the test that holds the download.
	commitOnly bool
	// setup runs before the hold is armed.
	setup func(t *testing.T, m *Manager)
	// run starts the load that opens the flight; its return ends with the
	// flight unless done says otherwise.
	run func(ctx context.Context, m *Manager)
	// done reports that a flight that run did not wait for has ended.
	done func(m *Manager) bool
	// state describes what the cache holds.
	state func(m *Manager) string
}

// readLocked reads under mu. A lock that stays taken is a commit that holds
// it, as a flight that is still inside its commit when Close returns would;
// the test must not block on it, so that is reported instead. Reads of the
// flight itself release the lock again at once.
func readLocked(mu *sync.RWMutex, read func() string) string {
	for deadline := time.Now().Add(500 * time.Millisecond); !mu.TryRLock(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			return "locked by a commit"
		}
	}
	defer mu.RUnlock()
	return read()
}

func closeCommitCases() []closeCommitCase {
	matchID := types.URN{Prefix: "od", Type: "match", ID: 42}
	fixtureID := types.URN{Prefix: "od", Type: "match", ID: 99}
	tournamentID := types.URN{Prefix: "od", Type: "tournament", ID: 7}
	competitorID := types.URN{Prefix: "od", Type: "competitor", ID: 3}
	en := []types.Locale{types.EnLocale}

	marketState := func(m *Manager) string {
		mc := m.MarketDescriptionCache
		return readLocked(&mc.mu, func() string {
			return fmt.Sprintf("base=%d loaded=%d", len(mc.base), len(mc.loadedLocales))
		})
	}
	sportState := func(m *Manager) string {
		sc := m.SportDataCache
		return readLocked(&sc.mu, func() string {
			tournaments := 0
			for _, e := range sc.sports {
				e.mu.RLock()
				tournaments += len(e.tournamentIDs)
				e.mu.RUnlock()
			}
			return fmt.Sprintf("sports=%d tournaments=%d loaded=%d", len(sc.sports), tournaments, len(sc.loadedLocales))
		})
	}
	iconState := func(m *Manager, tournament bool) string {
		if tournament {
			_, ok := m.TournamentCache.loadIcon(tournamentID)
			return fmt.Sprintf("entries=%d icon=%v", m.TournamentCache.lru.Len(), ok)
		}
		_, ok := m.CompetitorCache.loadIcon(competitorID)
		return fmt.Sprintf("entries=%d icon=%v", m.CompetitorCache.lru.Len(), ok)
	}

	return []closeCommitCase{
		{
			// The summary reaches the status cache first (1st commit).
			name: "match admission", nth: 2,
			run:   func(ctx context.Context, m *Manager) { _, _ = m.MatchCache.Match(ctx, matchID, en) },
			state: func(m *Manager) string { return fmt.Sprint(m.MatchCache.lru.Len()) },
		},
		{
			name: "fixture admission", nth: 1,
			run:   func(ctx context.Context, m *Manager) { _, _ = m.FixtureCache.Fixture(ctx, fixtureID, en) },
			state: func(m *Manager) string { return fmt.Sprint(m.FixtureCache.lru.Len()) },
		},
		{
			name: "tournament admission", nth: 1,
			run:   func(ctx context.Context, m *Manager) { _, _ = m.TournamentCache.Tournament(ctx, tournamentID, en) },
			state: func(m *Manager) string { return iconState(m, true) },
		},
		{
			// The icon of the profile is committed by OnAdmit with the entry.
			name: "competitor admission with its icon", nth: 1,
			run:   func(ctx context.Context, m *Manager) { _, _ = m.CompetitorCache.Competitor(ctx, competitorID, en) },
			state: func(m *Manager) string { return iconState(m, false) },
		},
		{
			name: "competitor icon flight", nth: 1,
			run: func(ctx context.Context, m *Manager) {
				_, _ = m.CompetitorCache.CompetitorIcon(ctx, competitorID, types.EnLocale)
			},
			state: func(m *Manager) string { return iconState(m, false) },
		},
		{
			name: "tournament icon flight", nth: 1,
			run: func(ctx context.Context, m *Manager) {
				_, _ = m.TournamentCache.TournamentIcon(ctx, tournamentID, types.EnLocale)
			},
			state: func(m *Manager) string { return iconState(m, true) },
		},
		{
			// 1st commit of the flight advances the cursor, the 2nd stores a row.
			name: "market catalog", nth: 2,
			run: func(ctx context.Context, m *Manager) {
				_, _ = m.MarketDescriptionCache.MarketDescriptionByID(ctx, 9, types.None[string](), en)
			},
			state: marketState,
		},
		{
			name: "market catalog background refresh", nth: 2,
			setup: func(t *testing.T, m *Manager) {
				mc := m.MarketDescriptionCache
				mc.catalogTTL = 50 * time.Millisecond
				if _, err := mc.MarketDescriptionByID(t.Context(), 9, types.None[string](), en); err != nil {
					t.Fatalf("seed lookup: %v", err)
				}
				time.Sleep(80 * time.Millisecond)
			},
			run: func(ctx context.Context, m *Manager) {
				msg := apipolicy.WithMessage(ctx, newGateStub(), time.Minute)
				_, _ = m.MarketDescriptionCache.MarketDescriptionByID(msg, 9, types.None[string](), en)
			},
			done: func(m *Manager) bool {
				mc := m.MarketDescriptionCache
				mc.mu.RLock()
				defer mc.mu.RUnlock()
				return len(mc.loadingLocales) == 0
			},
			state: func(m *Manager) string {
				mc := m.MarketDescriptionCache
				if st := marketState(m); strings.HasPrefix(st, "locked") {
					return st
				}
				e, ok := mc.lookup(variantKey(9, types.None[string]()))
				if !ok {
					return marketState(m) + " market 9 missing"
				}
				name, _ := e.Name(types.EnLocale)
				return marketState(m) + " name=" + name
			},
		},
		{
			name: "localized static data", nth: 1,
			run: func(ctx context.Context, m *Manager) {
				_, _ = m.LocalizedStaticMatchStatus.LocalizedItem(ctx, 1, en)
			},
			state: func(m *Manager) string {
				c := m.LocalizedStaticMatchStatus
				return readLocked(&c.mux, func() string {
					return fmt.Sprintf("ids=%d loaded=%d", len(c.internalCache), len(c.loadedLocales))
				})
			},
		},
		{
			name: "player", nth: 1,
			run: func(ctx context.Context, m *Manager) {
				_, _ = m.PlayersCache.GetPlayer(ctx, PlayerCacheKey{PlayerID: "od:player:300", Locale: types.EnLocale})
			},
			state: func(m *Manager) string { return fmt.Sprint(m.PlayersCache.players.Len()) },
		},
		{
			// The summary reaches the status cache as an API observer, which
			// the cache registers itself, so the second test cannot hold the
			// response in front of it: this cache is covered by the first.
			name: "match status", nth: 1, commitOnly: true,
			run:   func(ctx context.Context, m *Manager) { _, _ = m.MatchStatusCache.MatchStatus(ctx, matchID) },
			state: func(m *Manager) string { return fmt.Sprint(m.MatchStatusCache.entries.Len()) },
		},
		{
			name: "sport catalog", nth: 2,
			run:   func(ctx context.Context, m *Manager) { _, _ = m.SportDataCache.Sports(ctx, en) },
			state: sportState,
		},
		{
			name: "sport tournaments", nth: 1,
			run: func(ctx context.Context, m *Manager) {
				_, _ = m.SportDataCache.SportTournaments(ctx, sportOne, types.EnLocale)
			},
			state: sportState,
		},
		{
			name: "market void reasons", nth: 1,
			run: func(ctx context.Context, m *Manager) { _, _ = m.MarketVoidReasonsCache.MarketVoidReasons(ctx) },
			state: func(m *Manager) string {
				v := m.MarketVoidReasonsCache
				return readLocked(&v.mu, func() string {
					return fmt.Sprintf("loaded=%v n=%d", v.loaded, len(v.void))
				})
			},
		},
	}
}

func TestClose_AFlightHeldInsideItsCommitLeavesTheCacheUnchanged(t *testing.T) {
	for _, tc := range closeCommitCases() {
		t.Run(tc.name, func(t *testing.T) {
			var catalog atomic.Pointer[string]
			seed := bulkCatalogWithStaticVariants
			catalog.Store(&seed)
			srv := closeCommitServer(t, &catalog)
			client := newAPIClientForTest(t, srv)
			m := NewManager(t.Context(), client, &fakeCacheCfg{}, log.New(nil), nil)
			t.Cleanup(func() { m.CloseCtx(context.Background()) })
			if tc.setup != nil {
				tc.setup(t, m)
				refreshed := closeCommitRefreshedCatalog
				catalog.Store(&refreshed)
			}

			initial := tc.state(m)

			// Hold the flight inside its nth commit.
			var commits atomic.Int32
			entered := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			unhold := func() { releaseOnce.Do(func() { close(release) }) }
			restore := lru.SetCommitHook(func() {
				if int(commits.Add(1)) == tc.nth {
					close(entered)
					<-release
				}
			})
			t.Cleanup(func() { unhold(); restore() })

			flightDone := make(chan struct{})
			go func() {
				defer close(flightDone)
				tc.run(t.Context(), m)
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatalf("the flight never reached commit %d (%d commits seen)", tc.nth, commits.Load())
			}

			closeCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			closeDone := make(chan bool, 1)
			go func() { closeDone <- m.CloseCtx(closeCtx) }()

			// CloseCtx waits for the running commit; a Close that returns
			// while the flight is inside its commit has lost that guarantee.
			var atClose string
			select {
			case <-closeDone:
				atClose = tc.state(m)
				unhold()
			case <-time.After(150 * time.Millisecond):
				unhold()
				select {
				case ok := <-closeDone:
					if !ok {
						t.Fatal("CloseCtx did not complete")
					}
				case <-time.After(5 * time.Second):
					t.Fatal("CloseCtx did not return after the commit was released")
				}
				atClose = tc.state(m)
			}

			select {
			case <-flightDone:
			case <-time.After(5 * time.Second):
				t.Fatal("the flight did not end")
			}
			if tc.done != nil {
				waitFor(t, "the background flight to end", func() bool { return tc.done(m) })
			}
			if after := tc.state(m); after != atClose {
				t.Fatalf("the cache changed after CloseCtx returned: %q at return, %q later", atClose, after)
			}
			if atClose == initial {
				t.Fatalf("the held commit stored nothing (%q): the case checks nothing", initial)
			}
		})
	}
}

// A flight that reaches its commits after Close stores nothing at all.
func TestClose_AFlightThatEndsAfterCloseStoresNothing(t *testing.T) {
	for _, tc := range closeCommitCases() {
		if tc.commitOnly {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			var catalog atomic.Pointer[string]
			seed := bulkCatalogWithStaticVariants
			catalog.Store(&seed)
			srv := closeCommitServer(t, &catalog)
			client := newAPIClientForTest(t, srv)
			m := NewManager(t.Context(), client, &fakeCacheCfg{}, log.New(nil), nil)
			t.Cleanup(func() { m.CloseCtx(context.Background()) })
			if tc.setup != nil {
				tc.setup(t, m)
				refreshed := closeCommitRefreshedCatalog
				catalog.Store(&refreshed)
			}
			before := tc.state(m)

			// The flight is held after its download (an API observer) and
			// before any commit; the manager is closed meanwhile.
			entered := make(chan struct{})
			release := make(chan struct{})
			var enterOnce, releaseOnce sync.Once
			unhold := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(unhold)
			client.SubscribeWithAPIObserver(holdObserver(func() {
				enterOnce.Do(func() { close(entered) })
				<-release
			}))

			flightDone := make(chan struct{})
			go func() {
				defer close(flightDone)
				tc.run(t.Context(), m)
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("the flight never downloaded")
			}
			if !m.CloseCtx(t.Context()) {
				t.Fatal("CloseCtx did not complete")
			}
			unhold()
			select {
			case <-flightDone:
			case <-time.After(5 * time.Second):
				t.Fatal("the flight did not end")
			}
			if tc.done != nil {
				waitFor(t, "the background flight to end", func() bool { return tc.done(m) })
			}
			if after := tc.state(m); after != before {
				t.Fatalf("a flight that ended after CloseCtx changed the cache: %q before, %q after", before, after)
			}
		})
	}
}

type holdObserver func()

func (h holdObserver) OnAPIResponse(api.Response) { h() }

// A catalog flight that is closed between its first commit (the cursor) and
// the stores that follow stores no row and no mark, and its callers get the
// closed error instead of a partial catalog.
func TestClose_ACatalogFlightClosedBetweenItsCommitsStoresNothing(t *testing.T) {
	en := []types.Locale{types.EnLocale}
	cases := []struct {
		name  string
		arm   func(closeManager func())
		run   func(ctx context.Context, m *Manager) error
		state func(m *Manager) string
	}{
		{
			name: "market catalog",
			arm: func(closeManager func()) {
				marketCommitGateHook = func(types.Locale) { closeManager() }
				t.Cleanup(func() { marketCommitGateHook = nil })
			},
			run: func(ctx context.Context, m *Manager) error {
				_, err := m.MarketDescriptionCache.MarketDescriptionByID(ctx, 9, types.None[string](), en)
				return err
			},
			state: func(m *Manager) string {
				mc := m.MarketDescriptionCache
				mc.mu.RLock()
				defer mc.mu.RUnlock()
				return fmt.Sprintf("base=%d loaded=%d", len(mc.base), len(mc.loadedLocales))
			},
		},
		{
			name: "sport catalog",
			arm: func(closeManager func()) {
				sportCommitGateHook = func(types.Locale) { closeManager() }
				t.Cleanup(func() { sportCommitGateHook = nil })
			},
			run: func(ctx context.Context, m *Manager) error {
				_, err := m.SportDataCache.Sports(ctx, en)
				return err
			},
			state: func(m *Manager) string {
				sc := m.SportDataCache
				sc.mu.RLock()
				defer sc.mu.RUnlock()
				return fmt.Sprintf("sports=%d loaded=%d", len(sc.sports), len(sc.loadedLocales))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var catalog atomic.Pointer[string]
			seed := bulkCatalogWithStaticVariants
			catalog.Store(&seed)
			srv := closeCommitServer(t, &catalog)
			m := NewManager(t.Context(), newAPIClientForTest(t, srv), &fakeCacheCfg{}, log.New(nil), nil)
			t.Cleanup(func() { m.CloseCtx(context.Background()) })
			before := tc.state(m)

			tc.arm(func() { m.CloseCtx(context.Background()) })
			if err := tc.run(t.Context(), m); err == nil {
				t.Fatal("the flight reported success although the cache was closed before its stores")
			}
			if after := tc.state(m); after != before {
				t.Fatalf("a flight closed between its commits changed the cache: %q before, %q after", before, after)
			}
		})
	}
}

// The feed path of the match status is a second writer of its cache: after
// Close it stores nothing.
func TestClose_AFeedMessageAfterCloseStoresNoMatchStatus(t *testing.T) {
	var catalog atomic.Pointer[string]
	seed := bulkCatalogWithStaticVariants
	catalog.Store(&seed)
	m := NewManager(t.Context(), newAPIClientForTest(t, closeCommitServer(t, &catalog)), &fakeCacheCfg{}, log.New(nil), nil)
	t.Cleanup(func() { m.CloseCtx(context.Background()) })
	id := types.URN{Prefix: "od", Type: "match", ID: 42}

	if !m.CloseCtx(t.Context()) {
		t.Fatal("CloseCtx did not complete")
	}
	m.MatchStatusCache.refreshOrInsertFeedItem(id, &feedXML.SportEventStatus{Status: ptrOf(1), MatchStatus: ptrOf(1)}, time.Time{})
	if n := m.MatchStatusCache.entries.Len(); n != 0 {
		t.Fatalf("a feed message after Close stored %d entries", n)
	}
}

// The periodic refresh of the localized static data is a second writer: a
// refresh whose fetch finishes across Close leaves what the cache held.
func TestClose_ARefreshThatEndsAfterCloseKeepsTheStaticData(t *testing.T) {
	life := testLife(t)
	var closeDuringFetch atomic.Bool
	fetcher := func(_ context.Context, _ types.Locale) ([]types.StaticData, error) {
		if closeDuringFetch.Load() {
			life.End()
			return []types.StaticData{{ID: 1, Description: types.Some("changed")}}, nil
		}
		return []types.StaticData{{ID: 1, Description: types.Some("live")}}, nil
	}
	c := newLocalizedStaticDataCache(life, &fakeCacheCfg{}, log.New(nil), nil, fetcher)
	t.Cleanup(c.Close)
	en := []types.Locale{types.EnLocale}
	if _, err := c.LocalizedItem(t.Context(), 1, en); err != nil {
		t.Fatalf("first load: %v", err)
	}
	read := func() string {
		c.mux.RLock()
		defer c.mux.RUnlock()
		return c.internalCache[1][types.EnLocale]
	}

	closeDuringFetch.Store(true)
	c.timerTick(t.Context())
	if got := read(); got != "live" {
		t.Fatalf("a refresh that finished after Close stored %q", got)
	}
}

// A status fetch whose response arrives after Close is refused by the
// observer's store; the caller gets the closed error, not a cache miss that
// reads as an absent match.
func TestClose_AStatusFetchClosedMidFlightReportsClosed(t *testing.T) {
	var catalog atomic.Pointer[string]
	seed := bulkCatalogWithStaticVariants
	catalog.Store(&seed)
	client := newAPIClientForTest(t, closeCommitServer(t, &catalog))
	life := testLife(t)
	// Subscribed before the cache, so it sees the response first: the
	// lifetime ends after the download and before the store.
	client.SubscribeWithAPIObserver(holdObserver(life.End))
	c := newMatchStatusCache(life, client, &fakeCacheCfg{}, log.New(nil))

	_, err := c.MatchStatus(t.Context(), types.URN{Prefix: "od", Type: "match", ID: 42})
	if err == nil || errors.Is(err, ErrItemNotFoundInCache) {
		t.Fatalf("got %v, want the closed error", err)
	}
}
