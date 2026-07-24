package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

// gatedServer serves body, blocking each request on gate after
// signalling entered (buffered, so multiple requests don't deadlock the
// signal). Used to hold a load in flight while a Clear lands.
func gatedServer(t *testing.T, body func(r *http.Request) string, entered chan<- struct{}, gate <-chan struct{}) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-gate
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, body(r))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// TestPlayersCache_ClearDuringLoad_NotResurrected pins the Codex P1:
// a Clear landing while a player fetch is in flight must not be undone
// by the fetch's store — the next read refetches.
func TestPlayersCache_ClearDuringLoad_NotResurrected(t *testing.T) {
	entered := make(chan struct{}, 1)
	gate := make(chan struct{})
	srv, hits := gatedServer(t, func(*http.Request) string {
		return `<?xml version="1.0"?>
<player_profile><player id="od:player:1" name="P" full_name="PF"/></player_profile>`
	}, entered, gate)

	c := newPlayersCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))
	key := PlayerCacheKey{PlayerID: "od:player:1", Locale: types.EnLocale}

	done := make(chan error, 1)
	go func() {
		_, err := c.GetPlayer(context.Background(), key)
		done <- err
	}()
	<-entered
	c.Clear(key) // invalidate while the fetch is in flight
	close(gate)

	if err := <-done; err != nil {
		t.Fatalf("GetPlayer: %v", err)
	}
	// The in-flight load's store must have been suppressed.
	if _, ok := c.lookup(key); ok {
		t.Fatal("cleared player was resurrected by the in-flight load")
	}
	// Next read refetches.
	if _, err := c.GetPlayer(context.Background(), key); err != nil {
		t.Fatalf("refetch: %v", err)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("server hits = %d, want 2 (clear must force a refetch)", got)
	}
}

// TestMarketDescriptionCache_ClearDuringLoad_NotResurrected pins the
// same invariant for market descriptions: a ClearCacheItem racing an
// in-flight bulk load must suppress both the row store AND the
// loadedLocales mark, so the next read refetches instead of serving
// (and trusting) pre-clear data.
func TestMarketDescriptionCache_ClearDuringLoad_NotResurrected(t *testing.T) {
	entered := make(chan struct{}, 1)
	gate := make(chan struct{})
	srv, hits := gatedServer(t, func(*http.Request) string {
		return `<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="1" name="One"><outcomes><outcome id="1" name="o1"/></outcomes></market>
</market_descriptions>`
	}, entered, gate)

	mc := newMarketDescriptionCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))

	done := make(chan error, 1)
	go func() {
		_, err := mc.MarketDescriptionByID(context.Background(), 1, types.None[string](), []types.Locale{types.EnLocale})
		done <- err
	}()
	<-entered
	mc.ClearCacheItem(1, types.None[string]()) // clear mid-flight
	close(gate)

	// The in-flight call itself may report not-found (its store was
	// suppressed) — that's the documented transient for a clear race.
	if err := <-done; err != nil && !errors.Is(err, ErrItemNotFoundInCache) {
		t.Fatalf("in-flight call: %v", err)
	}
	if mc.localeLoaded(types.EnLocale) {
		t.Fatal("locale marked loaded by pre-clear load — bulk reads would skip the refetch")
	}
	// Next read must hit the API again and succeed.
	entry, err := mc.MarketDescriptionByID(context.Background(), 1, types.None[string](), []types.Locale{types.EnLocale})
	if err != nil || entry == nil {
		t.Fatalf("refetch: %v", err)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("server hits = %d, want 2 (clear must force a refetch)", got)
	}
}

// TestMatchStatusCache_ClearDuringLoad_NotResurrected pins the same
// invariant for match statuses: a ClearCacheItem racing an in-flight
// MatchStatus() summary fetch must suppress the observer-driven store.
// The suppressed flight is NOT the caller's answer: MatchStatus retries
// from the now-empty entry (post-clear fetch, second server hit) and
// succeeds — coalesced waiters never see a not-found for a live match.
func TestMatchStatusCache_ClearDuringLoad_NotResurrected(t *testing.T) {
	const urn = "od:match:9"
	entered := make(chan struct{}, 1)
	gate := make(chan struct{})
	srv, hits := gatedServer(t, func(*http.Request) string {
		return fmt.Sprintf(`<?xml version="1.0"?>
<match_summary generated_at="2026-01-01T00:00:00Z">
  <sport_event id="%s" name="X" scheduled="2026-01-01T12:00:00Z">
    <tournament id="od:tournament:1"><sport id="od:sport:1"/></tournament>
  </sport_event>
  <sport_event_status status="live" match_status_code="6" home_score="1.0" away_score="0.0"/>
</match_summary>`, urn)
	}, entered, gate)

	msc := newMatchStatusCache(t.Context(), newAPIClientForTest(t, srv), &fakeCacheCfg{}, log.New(nil))
	id := types.URN{Prefix: "od", Type: "match", ID: 9}

	done := make(chan error, 1)
	go func() {
		_, err := msc.MatchStatus(context.Background(), id)
		done <- err
	}()
	<-entered
	msc.ClearCacheItem(id) // clear mid-flight
	close(gate)

	// The in-flight call retries past the suppressed store and succeeds.
	if err := <-done; err != nil {
		t.Fatalf("in-flight call: %v", err)
	}
	// The entry present is the RETRY's post-clear fetch, not the
	// suppressed pre-clear store: exactly two server hits, and the next
	// read serves from cache without a third.
	if _, ok := msc.lookup(id); !ok {
		t.Fatal("post-clear retry did not repopulate the cache")
	}
	if _, err := msc.MatchStatus(context.Background(), id); err != nil {
		t.Fatalf("cached read: %v", err)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("server hits = %d, want 2 (suppressed pre-clear store + one post-clear refetch)", got)
	}
}

// TestMatchStatusCache_ClearDuringLoad_TransientNotNotFound pins the
// council P1: when a clear suppresses the in-flight store AND the
// post-clear retry cannot recover (server now errors), the surfaced
// error must be a transient fetch/clear classification — NOT the
// definitive ErrItemNotFoundInCache, which the exported contract
// documents as "the entity does not exist upstream". Pre-fix every
// coalesced waiter got the not-found sentinel for a live match.
func TestMatchStatusCache_ClearDuringLoad_TransientNotNotFound(t *testing.T) {
	const urn = "od:match:13"
	entered := make(chan struct{}, 1)
	gate := make(chan struct{})
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) > 1 {
			// 400 is terminal for the api client (no attempt-level
			// retries) and is NOT mapped to the not-found sentinel.
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		select {
		case entered <- struct{}{}:
		default:
		}
		<-gate
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, fmt.Sprintf(`<?xml version="1.0"?>
<match_summary generated_at="2026-01-01T00:00:00Z">
  <sport_event id="%s" name="X" scheduled="2026-01-01T12:00:00Z">
    <tournament id="od:tournament:1"><sport id="od:sport:1"/></tournament>
  </sport_event>
  <sport_event_status status="live" match_status_code="6" home_score="1.0" away_score="0.0"/>
</match_summary>`, urn))
	}))
	t.Cleanup(srv.Close)

	msc := newMatchStatusCache(t.Context(), newAPIClientForTest(t, srv), &fakeCacheCfg{}, log.New(nil))
	id := types.URN{Prefix: "od", Type: "match", ID: 13}

	done := make(chan error, 1)
	go func() {
		_, err := msc.MatchStatus(context.Background(), id)
		done <- err
	}()
	<-entered
	msc.ClearCacheItem(id) // suppress the in-flight store
	close(gate)

	err := <-done
	if err == nil {
		t.Fatal("expected the post-clear retry to fail (server 400s)")
	}
	if errors.Is(err, ErrItemNotFoundInCache) {
		t.Fatalf("clear-during-load race classified as definitive absence: %v", err)
	}
	if _, ok := msc.lookup(id); ok {
		t.Fatal("cleared match status was resurrected by the suppressed pre-clear store")
	}
}

// fakeCacheCfg is the minimal config.Config for the match-status test.
type fakeCacheCfg struct{}

func (f *fakeCacheCfg) AccessToken() *string                    { s := "tok"; return &s }
func (f *fakeCacheCfg) DefaultLocale() types.Locale             { return types.EnLocale }
func (f *fakeCacheCfg) MaxInactivity() time.Duration            { return 20 * time.Second }
func (f *fakeCacheCfg) MaxRecoveryExecution() time.Duration     { return 6 * time.Hour }
func (f *fakeCacheCfg) MessagingPort() int                      { return 5672 }
func (f *fakeCacheCfg) SdkNodeID() *int                         { return nil }
func (f *fakeCacheCfg) SelectedEnvironment() *types.Environment { return nil }
func (f *fakeCacheCfg) SelectedRegion() types.Region            { return types.RegionDefault }
func (f *fakeCacheCfg) ExchangeName() string                    { return "oddinfeed" }
func (f *fakeCacheCfg) ReplayExchangeName() string              { return "oddinreplay" }
func (f *fakeCacheCfg) ReportExtendedData() bool                { return false }
func (f *fakeCacheCfg) APIURL() (string, error)                 { return "x", nil }
func (f *fakeCacheCfg) MQURL() (string, error)                  { return "x", nil }
func (f *fakeCacheCfg) SportIDPrefix() string                   { return "od:sport:" }

// TestMatchStatusCache_ForeignFetch_ClearNotBypassed pins the third-pass
// P1: a summary fetch initiated OUTSIDE MatchStatus() (e.g. the match
// cache's loader) also drives the status observer. The fetch start now
// travels on api.Response.StartedAt, so ClearCacheItem suppresses the
// stale store no matter which code path fetched. Pre-fix the observer
// fell back to time.Now() and the clear could never win.
func TestMatchStatusCache_ForeignFetch_ClearNotBypassed(t *testing.T) {
	const urn = "od:match:11"
	entered := make(chan struct{}, 1)
	gate := make(chan struct{})
	srv, _ := gatedServer(t, func(*http.Request) string {
		return fmt.Sprintf(`<?xml version="1.0"?>
<match_summary generated_at="2026-01-01T00:00:00Z">
  <sport_event id="%s" name="X" scheduled="2026-01-01T12:00:00Z">
    <tournament id="od:tournament:1"><sport id="od:sport:1"/></tournament>
  </sport_event>
  <sport_event_status status="live" match_status_code="6" home_score="1.0" away_score="0.0"/>
</match_summary>`, urn)
	}, entered, gate)

	apiClient := newAPIClientForTest(t, srv)
	msc := newMatchStatusCache(t.Context(), apiClient, &fakeCacheCfg{}, log.New(nil))
	id := types.URN{Prefix: "od", Type: "match", ID: 11}

	// Foreign fetch: call the API client directly (as the match cache
	// does) — NOT msc.MatchStatus(), so the status cache has no local
	// bookkeeping for this flight.
	done := make(chan error, 1)
	go func() {
		_, err := apiClient.FetchMatchSummary(context.Background(), id, types.EnLocale)
		done <- err
	}()
	<-entered
	msc.ClearCacheItem(id) // clear while the FOREIGN fetch is in flight
	close(gate)

	if err := <-done; err != nil {
		t.Fatalf("foreign fetch: %v", err)
	}
	if _, ok := msc.lookup(id); ok {
		t.Fatal("cleared status was resurrected by a foreign summary fetch")
	}
}

// TestMatchStatusCache_ClearIsolatedPerKey pins the per-key granularity:
// clearing match B must not suppress the store of an in-flight fetch
// for match A.
func TestMatchStatusCache_ClearIsolatedPerKey(t *testing.T) {
	const urn = "od:match:21"
	entered := make(chan struct{}, 1)
	gate := make(chan struct{})
	srv, _ := gatedServer(t, func(*http.Request) string {
		return fmt.Sprintf(`<?xml version="1.0"?>
<match_summary generated_at="2026-01-01T00:00:00Z">
  <sport_event id="%s" name="X" scheduled="2026-01-01T12:00:00Z">
    <tournament id="od:tournament:1"><sport id="od:sport:1"/></tournament>
  </sport_event>
  <sport_event_status status="live" match_status_code="6" home_score="1.0" away_score="0.0"/>
</match_summary>`, urn)
	}, entered, gate)

	msc := newMatchStatusCache(t.Context(), newAPIClientForTest(t, srv), &fakeCacheCfg{}, log.New(nil))
	idA := types.URN{Prefix: "od", Type: "match", ID: 21}
	idB := types.URN{Prefix: "od", Type: "match", ID: 999}

	done := make(chan error, 1)
	go func() {
		_, err := msc.MatchStatus(context.Background(), idA)
		done <- err
	}()
	<-entered
	msc.ClearCacheItem(idB) // unrelated key
	close(gate)

	if err := <-done; err != nil {
		t.Fatalf("MatchStatus(A): %v", err)
	}
	if _, ok := msc.lookup(idA); !ok {
		t.Fatal("clearing B suppressed the in-flight store for A (cache-wide tombstone regression)")
	}
}

// TestMarketDescriptionCache_ClearIsolatedPerRow pins the per-key
// granularity for the bulk catalog: clearing market 1 mid-load must
// suppress ONLY market 1's row — the rest of the response is stored and
// returned, so a cold MarketDescriptions call can never come back as an
// empty catalog with no error.
func TestMarketDescriptionCache_ClearIsolatedPerRow(t *testing.T) {
	entered := make(chan struct{}, 1)
	gate := make(chan struct{})
	srv, hits := gatedServer(t, func(*http.Request) string {
		return `<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="1" name="One"><outcomes><outcome id="1" name="o1"/></outcomes></market>
  <market id="2" name="Two"><outcomes><outcome id="1" name="o1"/></outcomes></market>
  <market id="3" name="Three"><outcomes><outcome id="1" name="o1"/></outcomes></market>
</market_descriptions>`
	}, entered, gate)

	mc := newMarketDescriptionCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))

	type res struct {
		out map[CompositeKey]*LocalizedMarketDescription
		err error
	}
	done := make(chan res, 1)
	go func() {
		out, err := mc.LocalizedMarketDescriptions(context.Background(), types.EnLocale)
		done <- res{out, err}
	}()
	<-entered
	mc.ClearCacheItem(1, types.None[string]()) // clear ONE market mid-load
	close(gate)

	r := <-done
	if r.err != nil {
		t.Fatalf("bulk load: %v", r.err)
	}
	// Markets 2 and 3 survived; only the cleared row was suppressed.
	if len(r.out) != 2 {
		t.Fatalf("bulk view = %d entries, want 2 (unrelated rows must be stored)", len(r.out))
	}
	if _, ok := r.out[CompositeKey{MarketID: 1}]; ok {
		t.Fatal("cleared market 1 present in the pre-clear load's view")
	}
	// Locale must stay unmarked so the next bulk read refetches and
	// restores market 1.
	all, err := mc.LocalizedMarketDescriptions(context.Background(), types.EnLocale)
	if err != nil {
		t.Fatalf("refetch: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("post-refetch view = %d entries, want 3", len(all))
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("server hits = %d, want 2", got)
	}
}

// TestPlayersCache_DuplicateKeys pins the third-pass P2: duplicate keys
// in the GetPlayers input must not be reported as not-found after they
// resolved successfully (the old check compared map size against input
// length).
func TestPlayersCache_DuplicateKeys(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<?xml version="1.0"?>
<player_profile><player id="od:player:5" name="P" full_name="PF"/></player_profile>`)
	}))
	defer srv.Close()

	c := newPlayersCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))
	key := PlayerCacheKey{PlayerID: "od:player:5", Locale: types.EnLocale}

	out, err := c.GetPlayers(context.Background(), []PlayerCacheKey{key, key})
	if err != nil {
		t.Fatalf("GetPlayers with duplicate keys: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("result len = %d, want 1", len(out))
	}
	if _, ok := out[key]; !ok {
		t.Fatal("resolved key missing from result")
	}
}

// TestPlayersCache_PostClearCallerDoesNotJoinOldFlight pins the fourth-
// pass P1: tombstones stop a pre-clear flight from repopulating storage,
// but a caller arriving AFTER the clear must not JOIN that flight and be
// handed its pre-clear result. The clear advances the flight generation,
// so the post-clear caller starts a fresh fetch — asserted here while
// the pre-clear flight is still blocked on the gate.
func TestPlayersCache_PostClearCallerDoesNotJoinOldFlight(t *testing.T) {
	entered := make(chan struct{}, 1)
	gate := make(chan struct{})
	srv, hits := gatedServer(t, func(*http.Request) string {
		return `<?xml version="1.0"?>
<player_profile><player id="od:player:2" name="P" full_name="PF"/></player_profile>`
	}, entered, gate)

	c := newPlayersCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))
	key := PlayerCacheKey{PlayerID: "od:player:2", Locale: types.EnLocale}

	pre := make(chan error, 1)
	go func() {
		_, err := c.GetPlayer(context.Background(), key)
		pre <- err
	}()
	<-entered
	c.Clear(key)

	// Post-clear caller, while the pre-clear flight is STILL blocked:
	// must run its own fetch (second server hit), not join the old one.
	if _, err := c.GetPlayer(context.Background(), key); err != nil {
		t.Fatalf("post-clear GetPlayer: %v", err)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("server hits = %d, want 2 (post-clear caller joined the pre-clear flight)", got)
	}
	close(gate)
	if err := <-pre; err != nil {
		t.Fatalf("pre-clear GetPlayer: %v", err)
	}
}

// TestMatchStatusCache_PostClearCallerDoesNotJoinOldFlight pins the same
// invariant for match statuses: pre-fix the post-clear caller joined the
// old flight and received its transient not-found instead of fetching.
func TestMatchStatusCache_PostClearCallerDoesNotJoinOldFlight(t *testing.T) {
	const urn = "od:match:31"
	entered := make(chan struct{}, 1)
	gate := make(chan struct{})
	srv, hits := gatedServer(t, func(*http.Request) string {
		return fmt.Sprintf(`<?xml version="1.0"?>
<match_summary generated_at="2026-01-01T00:00:00Z">
  <sport_event id="%s" name="X" scheduled="2026-01-01T12:00:00Z">
    <tournament id="od:tournament:1"><sport id="od:sport:1"/></tournament>
  </sport_event>
  <sport_event_status status="live" match_status_code="6" home_score="1.0" away_score="0.0"/>
</match_summary>`, urn)
	}, entered, gate)

	msc := newMatchStatusCache(t.Context(), newAPIClientForTest(t, srv), &fakeCacheCfg{}, log.New(nil))
	id := types.URN{Prefix: "od", Type: "match", ID: 31}

	pre := make(chan error, 1)
	go func() {
		_, err := msc.MatchStatus(context.Background(), id)
		pre <- err
	}()
	<-entered
	msc.ClearCacheItem(id)

	// Post-clear caller must succeed via a fresh flight — not inherit
	// the old flight's suppressed-store transient error.
	if _, err := msc.MatchStatus(context.Background(), id); err != nil {
		t.Fatalf("post-clear MatchStatus: %v", err)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("server hits = %d, want 2 (post-clear caller joined the pre-clear flight)", got)
	}
	close(gate)
	<-pre // pre-clear caller outcome is not contractual here; just join it
}

// TestMarketDescriptionCache_PostClearCallerDoesNotJoinOldFlight pins
// the same invariant for the bulk catalog: a post-clear bulk read must
// not join the pre-clear flight (whose cleared row is suppressed) and
// come back with a partial catalog.
func TestMarketDescriptionCache_PostClearCallerDoesNotJoinOldFlight(t *testing.T) {
	entered := make(chan struct{}, 1)
	gate := make(chan struct{})
	srv, hits := gatedServer(t, func(*http.Request) string {
		return `<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="1" name="One"><outcomes><outcome id="1" name="o1"/></outcomes></market>
  <market id="2" name="Two"><outcomes><outcome id="1" name="o1"/></outcomes></market>
</market_descriptions>`
	}, entered, gate)

	mc := newMarketDescriptionCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))

	pre := make(chan error, 1)
	go func() {
		_, err := mc.LocalizedMarketDescriptions(context.Background(), types.EnLocale)
		pre <- err
	}()
	<-entered
	mc.ClearCacheItem(1, types.None[string]())

	// Post-clear bulk read: fresh flight, complete catalog.
	all, err := mc.LocalizedMarketDescriptions(context.Background(), types.EnLocale)
	if err != nil {
		t.Fatalf("post-clear bulk read: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("post-clear catalog = %d entries, want 2 (joined the pre-clear flight?)", len(all))
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("server hits = %d, want 2 (post-clear caller joined the pre-clear flight)", got)
	}
	close(gate)
	<-pre
}

// TestSportCache_ClearDuringLoad_NotResurrected pins the fifth-pass P1
// for the sport catalog: a Clear racing an in-flight FetchSports must
// suppress ONLY the cleared sport's row (per-key isolation — the rest
// of the catalog is stored, so Sports() never comes back empty) and
// keep the locale unmarked so the next read refetches.
func TestSportCache_ClearDuringLoad_NotResurrected(t *testing.T) {
	entered := make(chan struct{}, 1)
	gate := make(chan struct{})
	srv, hits := gatedServer(t, func(*http.Request) string {
		return `<?xml version="1.0"?>
<sports>
  <sport id="od:sport:1" name="Football" abbreviation="FB"/>
  <sport id="od:sport:2" name="Basketball" abbreviation="BB"/>
</sports>`
	}, entered, gate)

	sc := newSportDataCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))
	sport1 := types.URN{Prefix: "od", Type: "sport", ID: 1}
	sport2 := types.URN{Prefix: "od", Type: "sport", ID: 2}

	type res struct {
		ids []types.URN
		err error
	}
	done := make(chan res, 1)
	go func() {
		ids, err := sc.Sports(context.Background(), []types.Locale{types.EnLocale})
		done <- res{ids, err}
	}()
	<-entered
	sc.Clear(sport1) // invalidate ONE sport while the catalog load is in flight
	close(gate)

	r := <-done
	if r.err != nil {
		t.Fatalf("Sports: %v", r.err)
	}
	got := map[types.URN]bool{}
	for _, id := range r.ids {
		got[id] = true
	}
	if got[sport1] {
		t.Fatal("cleared sport present in the pre-clear load's view")
	}
	if !got[sport2] {
		t.Fatal("unrelated sport suppressed — per-key isolation regression")
	}
	// Locale must stay unmarked: the next read refetches and restores
	// the cleared sport.
	if _, err := sc.Sport(context.Background(), sport1, []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("refetch: %v", err)
	}
	if gotHits := hits.Load(); gotHits != 2 {
		t.Fatalf("server hits = %d, want 2 (clear must force a refetch)", gotHits)
	}
}

// TestSportCache_PostClearCallerDoesNotJoinOldFlight pins the flight-
// generation half for sports: a caller arriving after Clear must start
// a fresh catalog fetch, not join the pre-clear flight.
func TestSportCache_PostClearCallerDoesNotJoinOldFlight(t *testing.T) {
	entered := make(chan struct{}, 1)
	gate := make(chan struct{})
	srv, hits := gatedServer(t, func(*http.Request) string {
		return `<?xml version="1.0"?>
<sports><sport id="od:sport:1" name="Football" abbreviation="FB"/></sports>`
	}, entered, gate)

	sc := newSportDataCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))
	sport1 := types.URN{Prefix: "od", Type: "sport", ID: 1}

	pre := make(chan error, 1)
	go func() {
		_, err := sc.Sports(context.Background(), []types.Locale{types.EnLocale})
		pre <- err
	}()
	<-entered
	sc.Clear(sport1)

	// Post-clear caller while the pre-clear flight is still blocked:
	// fresh flight, second server hit, complete catalog.
	if _, err := sc.Sport(context.Background(), sport1, []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("post-clear Sport: %v", err)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("server hits = %d, want 2 (post-clear caller joined the pre-clear flight)", got)
	}
	close(gate)
	<-pre
}

// TestSportCache_TournamentFetchDoesNotResurrectClearedSport pins the
// ensureSportEntry/recordTournament path: an in-flight SportTournaments
// call must not recreate a sport cleared while its fetch was running.
func TestSportCache_TournamentFetchDoesNotResurrectClearedSport(t *testing.T) {
	entered := make(chan struct{}, 1)
	gate := make(chan struct{})
	srv, _ := gatedServer(t, func(r *http.Request) string {
		return `<?xml version="1.0"?>
<sport_tournaments><sport id="od:sport:1" name="Football"/><tournaments><tournament id="od:tournament:5" name="T"><sport id="od:sport:1" name="Football"/></tournament></tournaments></sport_tournaments>`
	}, entered, gate)

	sc := newSportDataCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))
	sport1 := types.URN{Prefix: "od", Type: "sport", ID: 1}

	done := make(chan error, 1)
	go func() {
		_, err := sc.SportTournaments(context.Background(), sport1, types.EnLocale)
		done <- err
	}()
	<-entered
	sc.Clear(sport1) // clear while the tournament fetch is in flight
	close(gate)

	if err := <-done; err != nil {
		t.Fatalf("SportTournaments: %v", err)
	}
	sc.mu.RLock()
	_, resurrected := sc.sports[sport1]
	sc.mu.RUnlock()
	if resurrected {
		t.Fatal("cleared sport recreated by the in-flight tournament fetch")
	}
}

// TestLocalizedStaticDataCache_RefreshDeletesAbsentEntries pins the
// fifth-pass P2: the periodic refresh must replace the locale snapshot
// atomically — an id that disappeared upstream previously stayed cached
// forever because timerTick only upserted.
func TestLocalizedStaticDataCache_RefreshDeletesAbsentEntries(t *testing.T) {
	var call atomic.Int32
	fetcher := func(ctx context.Context, locale types.Locale) ([]types.StaticData, error) {
		if call.Add(1) == 1 {
			return []types.StaticData{
				{ID: 1, Description: types.Some("one")},
				{ID: 2, Description: types.Some("two")},
			}, nil
		}
		return []types.StaticData{{ID: 1, Description: types.Some("one-v2")}}, nil
	}
	c := newLocalizedStaticDataCache(t.Context(), &fakeCacheCfg{}, log.New(nil), nil, fetcher)
	defer c.Close()

	// Initial load: both ids present.
	if _, err := c.Item(t.Context(), 2); err != nil {
		t.Fatalf("initial Item: %v", err)
	}

	// Simulate the periodic refresh, whose response no longer has id 2.
	c.timerTick(t.Context())

	one, err := c.LocalizedItem(t.Context(), 1, []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("LocalizedItem(1): %v", err)
	}
	if got := one.Descriptions[types.EnLocale]; got != "one-v2" {
		t.Fatalf("id 1 description = %q, want refreshed one-v2", got)
	}
	two, err := c.LocalizedItem(t.Context(), 2, []types.Locale{types.EnLocale})
	if !errors.Is(err, ErrItemNotFoundInCache) {
		t.Fatalf("LocalizedItem(2) after refresh drop = %v, want ErrItemNotFoundInCache (unknown ids are typed errors, not empty successes)", err)
	}
	if len(two.Descriptions) != 0 {
		t.Fatalf("id 2 still cached after refresh dropped it: %v", two.Descriptions)
	}
}

// TestTournamentCache_IconNotResurrectedByInFlightFetch pins the
// sixth-pass P2: the icon side-map must honour the same clear tombstone
// as its parent entry — a TournamentIcon fetch that began before
// ClearCacheItem must not restore the deleted icon afterwards.
func TestTournamentCache_IconNotResurrectedByInFlightFetch(t *testing.T) {
	entered := make(chan struct{}, 1)
	gate := make(chan struct{})
	srv, _ := gatedServer(t, func(*http.Request) string {
		return `<?xml version="1.0"?>
<tournament_info generated_at="2026-01-01T00:00:00Z">
  <tournament id="od:tournament:7" name="T" icon_path="/icons/t7.png">
    <sport id="od:sport:1" name="Football"/>
  </tournament>
</tournament_info>`
	}, entered, gate)

	tc := newTournamentCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))
	id := types.URN{Prefix: "od", Type: "tournament", ID: 7}

	done := make(chan error, 1)
	go func() {
		_, err := tc.TournamentIcon(context.Background(), id, types.EnLocale)
		done <- err
	}()
	<-entered
	tc.ClearCacheItem(id) // clear while the icon fetch is in flight
	close(gate)

	if err := <-done; err != nil {
		t.Fatalf("TournamentIcon: %v", err)
	}
	tc.iconMu.RLock()
	_, resurrected := tc.icons[id]
	tc.iconMu.RUnlock()
	if resurrected {
		t.Fatal("cleared tournament icon was resurrected by the in-flight fetch")
	}
}

// TestCompetitorCache_IconNotResurrectedByInFlightFetch pins the same
// invariant for competitor icons — including the loader-internal icon
// write, which previously ran BEFORE the EventCache's own tombstone
// admission check and so survived a clear that discarded the entry.
//
// The EventCache retries a clear-discarded load with a fresh post-clear
// flight — which would legitimately store a fresh icon — so the server
// fails every request after the first: the only icon write attempted is
// the pre-clear flight's, and it must be suppressed.
func TestCompetitorCache_IconNotResurrectedByInFlightFetch(t *testing.T) {
	entered := make(chan struct{}, 1)
	gate := make(chan struct{})
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) > 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		select {
		case entered <- struct{}{}:
		default:
		}
		<-gate
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<?xml version="1.0"?>
<competitor_profile generated_at="2026-01-01T00:00:00Z">
  <competitor id="od:competitor:3" name="Team" abbreviation="TM" icon_path="/icons/c3.png"/>
</competitor_profile>`)
	}))
	t.Cleanup(srv.Close)

	cc := newCompetitorCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))
	id := types.URN{Prefix: "od", Type: "competitor", ID: 3}

	// The loader path: Competitor() fetches the profile and writes the
	// icon side-map from inside the EventCache loader.
	done := make(chan error, 1)
	go func() {
		_, err := cc.Competitor(context.Background(), id, []types.Locale{types.EnLocale})
		done <- err
	}()
	<-entered
	cc.ClearCacheItem(id) // clear while the loader fetch is in flight
	close(gate)

	if err := <-done; err == nil {
		t.Fatal("expected the post-clear retry to fail (server 500s)")
	}
	if _, ok := cc.loadIcon(id); ok {
		t.Fatal("cleared competitor icon was resurrected by the pre-clear loader write")
	}
}
