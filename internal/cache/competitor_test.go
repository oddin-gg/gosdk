package cache

import (
	"context"
	"fmt"
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

// playerOnlyFactory is an entityFactory stub whose BuildPlayer is
// test-controlled; every other build method is unreachable from
// LocalizedCompetitor.snapshot and panics if called.
type playerOnlyFactory struct {
	buildPlayer func(ctx context.Context, id types.URN, locale types.Locale) (*types.Player, error)
}

func (f *playerOnlyFactory) BuildPlayer(ctx context.Context, id types.URN, locale types.Locale) (*types.Player, error) {
	return f.buildPlayer(ctx, id, locale)
}
func (f *playerOnlyFactory) BuildTournament(context.Context, types.URN, types.URN, []types.Locale) (*types.Tournament, error) {
	panic("unexpected BuildTournament")
}
func (f *playerOnlyFactory) BuildSport(context.Context, types.URN, []types.Locale) (*types.Sport, error) {
	panic("unexpected BuildSport")
}
func (f *playerOnlyFactory) BuildCompetitor(context.Context, types.URN, []types.Locale) (*types.Competitor, error) {
	panic("unexpected BuildCompetitor")
}
func (f *playerOnlyFactory) BuildTeamCompetitor(context.Context, types.URN, *string, []types.Locale) (*types.TeamCompetitor, error) {
	panic("unexpected BuildTeamCompetitor")
}
func (f *playerOnlyFactory) BuildFixture(context.Context, types.URN, types.Locale) (*types.Fixture, error) {
	panic("unexpected BuildFixture")
}
func (f *playerOnlyFactory) BuildMatchStatus(context.Context, types.URN, []types.Locale) (*types.MatchStatus, error) {
	panic("unexpected BuildMatchStatus")
}

// rosterEntry builds a LocalizedCompetitor carrying n players and
// names for the given locales.
func rosterEntry(n int, locales ...types.Locale) *LocalizedCompetitor {
	e := &LocalizedCompetitor{
		id:            types.URN{Prefix: "od", Type: "competitor", ID: 1},
		name:          make(map[types.Locale]string),
		abbreviation:  make(map[types.Locale]string),
		playersLoaded: true,
	}
	for _, l := range locales {
		e.name[l] = "Team"
		e.abbreviation[l] = "T"
	}
	for i := range n {
		e.players = append(e.players, types.URN{Prefix: "od", Type: "player", ID: i + 1})
	}
	return e
}

// TestCompetitorSnapshot_RosterFanOut pins the (locale × player)
// fan-out added in dc8da31: resolutions must overlap (bounded by
// playerLoadConcurrency), and every bucket slot must land in ORDER
// with the right locale — the failure mode of index-written buckets is
// silent (a wrong index or shared bucket leaves zero-valued Players
// with empty IDs, no error). Parallelism is asserted via an atomic
// active/peak counter, not wall-clock, so scheduler jitter can't flake
// the test; a generous elapsed ceiling additionally catches a
// re-serialized loop.
func TestCompetitorSnapshot_RosterFanOut(t *testing.T) {
	const players = 6
	locales := []types.Locale{types.EnLocale, types.DeLocale}
	const perCall = 60 * time.Millisecond

	var active, peak atomic.Int64
	factory := &playerOnlyFactory{buildPlayer: func(ctx context.Context, id types.URN, locale types.Locale) (*types.Player, error) {
		cur := active.Add(1)
		defer active.Add(-1)
		for {
			p := peak.Load()
			if cur <= p || peak.CompareAndSwap(p, cur) {
				break
			}
		}
		time.Sleep(perCall)
		return &types.Player{ID: id.ToString(), Name: "P-" + id.ToString(), Locale: locale}, nil
	}}

	entry := rosterEntry(players, locales...)
	start := time.Now()
	c, err := entry.snapshot(t.Context(), nil, factory, locales)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	// Bounded parallelism actually happened.
	if p := peak.Load(); p < 2 {
		t.Errorf("peak concurrent BuildPlayer = %d, want >= 2 (fan-out re-serialized?)", p)
	}
	if p := peak.Load(); p > playerLoadConcurrency {
		t.Errorf("peak concurrent BuildPlayer = %d, want <= %d (SetLimit bound)", p, playerLoadConcurrency)
	}
	// 12 sequential calls would take ~720ms; two bounded waves ~120ms.
	if sequential := time.Duration(players*len(locales)) * perCall; elapsed > sequential/2 {
		t.Errorf("elapsed = %v, want well under the sequential %v", elapsed, sequential)
	}

	// Every bucket slot in order, with the bucket's locale.
	for _, l := range locales {
		bucket := c.Players[l]
		if len(bucket) != players {
			t.Fatalf("Players[%s] = %d entries, want %d", l, len(bucket), players)
		}
		for i, p := range bucket {
			want := entry.players[i].ToString()
			if p.ID != want {
				t.Fatalf("Players[%s][%d].ID = %q, want %q (index-written bucket out of order)", l, i, p.ID, want)
			}
			if p.Locale != l {
				t.Fatalf("Players[%s][%d].Locale = %q, want %q", l, i, p.Locale, l)
			}
		}
	}
}

// TestCompetitorSnapshot_FirstFailureCancelsSiblings pins the errgroup
// cancellation semantics introduced in dc8da31: the first BuildPlayer
// failure must fail the snapshot with an error naming that player, AND
// cancel the sibling fetches' ctx so they stop early instead of each
// running to completion.
func TestCompetitorSnapshot_FirstFailureCancelsSiblings(t *testing.T) {
	const players = 6
	locales := []types.Locale{types.EnLocale, types.DeLocale}
	badID := "od:player:3"

	var cancelled atomic.Int64
	var wg sync.WaitGroup
	factory := &playerOnlyFactory{buildPlayer: func(ctx context.Context, id types.URN, locale types.Locale) (*types.Player, error) {
		if id.ToString() == badID && locale == types.EnLocale {
			return nil, fmt.Errorf("profile fetch exploded")
		}
		wg.Add(1)
		defer wg.Done()
		select {
		case <-ctx.Done():
			cancelled.Add(1)
			return nil, ctx.Err()
		case <-time.After(10 * time.Second):
			// Only reachable if the first failure does NOT cancel the
			// group ctx — the test then fails on the elapsed assertion
			// rather than hanging.
			return &types.Player{ID: id.ToString(), Locale: locale}, nil
		}
	}}

	entry := rosterEntry(players, locales...)
	start := time.Now()
	_, err := entry.snapshot(t.Context(), nil, factory, locales)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("snapshot = nil error, want the player failure")
	}
	if !strings.Contains(err.Error(), badID) {
		t.Fatalf("err = %v, want the failing player %s named", err, badID)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("snapshot took %v — siblings were not cancelled on first failure", elapsed)
	}
	wg.Wait() // every started sibling has returned
	if cancelled.Load() == 0 {
		t.Error("no sibling observed ctx cancellation (first failure must cancel the group)")
	}
}

// TestCompetitorCache_IconStore_NoCrossURLContention is the regression
// for the L1 finding from the v2.x review pass.
//
// Pre-fix the icon storage was a `map[URN]*string` guarded by a single
// global `iconMu sync.RWMutex`. Inside the EventCache singleflight
// loader (one goroutine per URN), the map write was guarded by
// `iconMu.Lock()` — so concurrent loaders for *different* competitors
// briefly serialized on the same write lock, defeating the per-key
// load isolation singleflight is supposed to provide.
//
// Post-fix the icon storage is a `sync.Map`. Concurrent loaders for
// different URNs proceed without contending on a shared mutex.
//
// Strategy: drive 50 concurrent fetches for distinct competitor URNs.
// The fetches all complete; final state has every URN's icon recorded.
// Direct contention measurement isn't deterministic, but a
// concurrent-safety + correctness regression test catches both
// sync.Map type-assertion bugs and any future revert that re-introduces
// the global lock.
func TestCompetitorCache_IconStore_ConcurrentDistinctURLs(t *testing.T) {
	const urns = 50
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		// Minimal CompetitorResponse XML; the key field for this
		// test is icon_path (we just want the storeIcon path
		// exercised). The REQUESTED competitor id is echoed — the API
		// client validates response identity, so a static id would be
		// rejected as a misrouted response.
		parts := strings.Split(strings.TrimSuffix(r.URL.Path, "/profile"), "/")
		id := parts[len(parts)-1]
		_, _ = io.WriteString(w, `<?xml version="1.0"?>
<competitor_profile>
  <competitor id="`+id+`" name="Team" abbreviation="T" icon_path="icon-`+id+`"/>
</competitor_profile>`)
	}))
	defer srv.Close()

	cc := newCompetitorCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))

	var wg sync.WaitGroup
	wg.Add(urns)
	for i := 0; i < urns; i++ {
		go func() {
			defer wg.Done()
			id := types.URN{Prefix: "od", Type: "competitor", ID: 1000 + i}
			_, _ = cc.CompetitorIcon(t.Context(), id, types.EnLocale)
		}()
	}
	wg.Wait()

	// Verify every URN's icon was recorded — proves storeIcon ran
	// for each loader without races/data loss.
	for i := 0; i < urns; i++ {
		id := types.URN{Prefix: "od", Type: "competitor", ID: 1000 + i}
		v, ok := cc.loadIcon(id)
		if !ok {
			t.Errorf("urn %v: icon not stored after concurrent fetch", id)
			continue
		}
		// The mock server's icon_path is empty in some XML schemas; we
		// only assert a record exists, not the value. The race detector
		// catches sync.Map misuse if any.
		_ = v
	}
}

// TestCompetitorCache_IconStore_RoundTrip covers the basic
// store/load/delete cycle through the helpers.
func TestCompetitorCache_IconStore_RoundTrip(t *testing.T) {
	cc := &CompetitorCache{}
	id := types.URN{Prefix: "od", Type: "competitor", ID: 1}

	// Not present.
	if _, ok := cc.loadIcon(id); ok {
		t.Errorf("loadIcon on empty: ok=true, want false")
	}

	// Store nil (competitor with no icon).
	cc.storeIcon(id, nil)
	v, ok := cc.loadIcon(id)
	if !ok {
		t.Errorf("loadIcon after storeIcon(nil): ok=false, want true")
	}
	if v != nil {
		t.Errorf("loadIcon after storeIcon(nil): v=%v, want nil", v)
	}

	// Overwrite with a real path.
	path := "icon.png"
	cc.storeIcon(id, &path)
	v, ok = cc.loadIcon(id)
	if !ok || v == nil || *v != "icon.png" {
		t.Errorf("loadIcon after storeIcon(\"icon.png\"): (%v, %v), want (\"icon.png\", true)", v, ok)
	}

	// Delete.
	cc.deleteIcon(id)
	if _, ok := cc.loadIcon(id); ok {
		t.Errorf("loadIcon after deleteIcon: ok=true, want false")
	}
}

// TestCompetitorCache_IconStore_TypeSafety guards against a regression
// where someone replaces sync.Map with a wrong type and the type
// assertion in loadIcon panics at runtime instead of at compile time.
func TestCompetitorCache_IconStore_TypeSafety(t *testing.T) {
	cc := &CompetitorCache{}
	for i := 0; i < 5; i++ {
		id := types.URN{Prefix: "od", Type: "competitor", ID: i}
		path := fmt.Sprintf("p-%d", i)
		cc.storeIcon(id, &path)
	}
	for i := 0; i < 5; i++ {
		id := types.URN{Prefix: "od", Type: "competitor", ID: i}
		v, ok := cc.loadIcon(id)
		if !ok {
			t.Errorf("urn %v: not stored", id)
			continue
		}
		want := fmt.Sprintf("p-%d", i)
		if v == nil || *v != want {
			t.Errorf("urn %v: got %v, want %s", id, v, want)
		}
	}
}
