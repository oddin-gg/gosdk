package cache

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

// recordingSportFactory is a minimal entityFactory that records the sport
// id BuildTournament resolves. Only BuildSport is exercised by
// BuildTournament; the rest satisfy the interface and must never be called
// on this path.
type recordingSportFactory struct {
	gotSportID types.URN
	called     bool
}

func (f *recordingSportFactory) BuildSport(_ context.Context, id types.URN, _ []types.Locale) (*types.Sport, error) {
	f.gotSportID = id
	f.called = true
	return &types.Sport{SportSummary: types.SportSummary{ID: id}}, nil
}

func (f *recordingSportFactory) BuildTournament(context.Context, types.URN, types.URN, []types.Locale) (*types.Tournament, error) {
	panic("unexpected BuildTournament call")
}
func (f *recordingSportFactory) BuildCompetitor(context.Context, types.URN, []types.Locale) (*types.Competitor, error) {
	panic("unexpected BuildCompetitor call")
}
func (f *recordingSportFactory) BuildTeamCompetitor(context.Context, types.URN, *string, []types.Locale) (*types.TeamCompetitor, error) {
	panic("unexpected BuildTeamCompetitor call")
}
func (f *recordingSportFactory) BuildPlayer(context.Context, types.URN, types.Locale) (*types.Player, error) {
	panic("unexpected BuildPlayer call")
}
func (f *recordingSportFactory) BuildFixture(context.Context, types.URN, types.Locale) (*types.Fixture, error) {
	panic("unexpected BuildFixture call")
}
func (f *recordingSportFactory) BuildMatchStatus(context.Context, types.URN, []types.Locale) (*types.MatchStatus, error) {
	panic("unexpected BuildMatchStatus call")
}

// TestBuildTournament_CachedSportOverridesRouteSport is the regression for
// the finding: the tournament's sport comes from the AUTHORITATIVE API
// summary, but BuildTournament built the public SportSummary from the
// routing-key sportID argument — so a mis-routed (or malicious) delivery
// could combine tournament A's API data with an unrelated route-selected
// sport B. It must now use the cached sport and ignore a conflicting route
// sport (mirrors the match path).
func TestBuildTournament_CachedSportOverridesRouteSport(t *testing.T) {
	// The API says tournament 7 belongs to sport 1.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<?xml version="1.0"?>
<tournament_info generated_at="2026-01-01T00:00:00Z">
  <tournament id="od:tournament:7" name="T" icon_path="/icons/t7.png">
    <sport id="od:sport:1" name="Football"/>
  </tournament>
  <competitors><competitor id="od:competitor:1"/></competitors>
</tournament_info>`)
	}))
	defer srv.Close()

	tc := newTournamentCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))
	id := types.URN{Prefix: "od", Type: "tournament", ID: 7}
	cachedSport := types.URN{Prefix: "od", Type: "sport", ID: 1}
	routeSport := types.URN{Prefix: "od", Type: "sport", ID: 99} // conflicting route

	fake := &recordingSportFactory{}
	out, err := BuildTournament(t.Context(), tc, fake, id, routeSport, []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("BuildTournament: %v", err)
	}
	if !fake.called {
		t.Fatal("BuildSport was never called")
	}
	if fake.gotSportID != cachedSport {
		t.Fatalf("resolved sport = %s, want cached %s (route %s must not win)", fake.gotSportID.ToString(), cachedSport.ToString(), routeSport.ToString())
	}
	if out.Sport.ID != cachedSport {
		t.Fatalf("tournament Sport.ID = %s, want cached %s", out.Sport.ID.ToString(), cachedSport.ToString())
	}
}
