package factory

import (
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/cache"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

// Canned XML for the per-entity endpoints exercised by EntityFactory.
const (
	tournamentInfoXML = `<?xml version="1.0"?>
<tournament_info generated_at="2026-01-01T00:00:00">
  <tournament id="od:tournament:7" name="Cup" abbreviation="CUP" risk_tier="2">
    <sport id="od:sport:1" name="Soccer" abbreviation="SOC"/>
  </tournament>
</tournament_info>`

	sportsXML = `<?xml version="1.0"?>
<sports generated_at="2026-01-01T00:00:00">
  <sport id="od:sport:1" name="Soccer" abbreviation="SOC"/>
</sports>`

	// %s: the fixture echoes the REQUESTED competitor id — the API
	// client validates response identity and rejects a static
	// one-body-for-all-ids reply as misrouted.
	competitorProfileXMLTmpl = `<?xml version="1.0"?>
<competitor_profile>
  <competitor id="%s" name="Home" abbreviation="HOM" country="GB" country_code="GB"/>
  <players>
    <player id="od:player:300" name="Striker" full_name="Striker McName" sport="od:sport:1"/>
  </players>
</competitor_profile>`

	playerProfileXML = `<?xml version="1.0"?>
<player_profile>
  <player id="od:player:300" name="Striker" full_name="Striker McName" sport="od:sport:1"/>
</player_profile>`

	fixtureXML = `<?xml version="1.0"?>
<fixtures_fixture generated_at="2026-01-01T00:00:00">
  <fixture id="od:match:99" name="Home vs Away" scheduled="2026-01-01T20:00:00" start_time="2026-01-01T20:00:00">
    <tournament id="od:tournament:7" name="Cup" abbreviation="CUP" risk_tier="2">
      <sport id="od:sport:1" name="Soccer" abbreviation="SOC"/>
    </tournament>
    <competitors>
      <competitor id="od:competitor:50" name="Home" abbreviation="HOM" qualifier="home"/>
      <competitor id="od:competitor:51" name="Away" abbreviation="AWY" qualifier="away"/>
    </competitors>
  </fixture>
</fixtures_fixture>`

	matchSummaryXML = `<?xml version="1.0"?>
<match_summary generated_at="2026-01-01T00:00:00">
  <sport_event id="od:match:99" name="Home vs Away" scheduled="2026-01-01T20:00:00" liveodds="booked">
    <tournament id="od:tournament:7" name="Cup" abbreviation="CUP" risk_tier="2">
      <sport id="od:sport:1" name="Soccer" abbreviation="SOC"/>
    </tournament>
    <competitors>
      <competitor id="od:competitor:50" name="Home" abbreviation="HOM" qualifier="home"/>
      <competitor id="od:competitor:51" name="Away" abbreviation="AWY" qualifier="away"/>
    </competitors>
  </sport_event>
  <sport_event_status status="live" match_status_code="6" home_score="1" away_score="0" scoreboard_available="false"/>
</match_summary>`

	matchStatusDescXML = `<?xml version="1.0"?>
<match_status_descriptions response_code="OK">
  <match_status id="6" description="1st period"/>
</match_status_descriptions>`

	tournamentSportsXML = `<?xml version="1.0"?>
<sport_tournaments generated_at="2026-01-01T00:00:00">
  <sport id="od:sport:1" name="Soccer" abbreviation="SOC"/>
  <tournaments>
    <tournament id="od:tournament:7" name="Cup" abbreviation="CUP" risk_tier="2">
      <sport id="od:sport:1" name="Soccer" abbreviation="SOC"/>
    </tournament>
  </tournaments>
</sport_tournaments>`
)

// entityFixtureServer routes every per-entity endpoint to canned XML.
func entityFixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/sports") && !strings.Contains(path, "/sport_events/"):
			if strings.HasSuffix(path, "/tournaments") {
				_, _ = io.WriteString(w, tournamentSportsXML)
				return
			}
			_, _ = io.WriteString(w, sportsXML)
		case strings.HasSuffix(path, "/tournaments") && strings.Contains(path, "/sports/od:sport:"):
			_, _ = io.WriteString(w, tournamentSportsXML)
		case strings.HasSuffix(path, "/info") && strings.Contains(path, "/tournaments/"):
			_, _ = io.WriteString(w, tournamentInfoXML)
		case strings.HasSuffix(path, "/profile") && strings.Contains(path, "/competitors/"):
			parts := strings.Split(strings.TrimSuffix(path, "/profile"), "/")
			_, _ = fmt.Fprintf(w, competitorProfileXMLTmpl, parts[len(parts)-1])
		case strings.HasSuffix(path, "/profile") && strings.Contains(path, "/players/"):
			_, _ = io.WriteString(w, playerProfileXML)
		case strings.HasSuffix(path, "/fixture") && strings.Contains(path, "/sport_events/"):
			_, _ = io.WriteString(w, fixtureXML)
		case strings.HasSuffix(path, "/summary") && strings.Contains(path, "/sport_events/"):
			_, _ = io.WriteString(w, matchSummaryXML)
		case strings.Contains(path, "/match_status"):
			_, _ = io.WriteString(w, matchStatusDescXML)
		default:
			t.Logf("entityFixtureServer: unhandled %s", path)
			http.NotFound(w, r)
		}
	}))
}

// rewriteHTTPClient routes outbound requests to a test server's host.
type rewriteHTTPClient struct {
	target string
	base   http.RoundTripper
}

func (rt *rewriteHTTPClient) RoundTrip(req *http.Request) (*http.Response, error) {
	t, _ := url.Parse(rt.target)
	req.URL.Scheme = t.Scheme
	req.URL.Host = t.Host
	base := rt.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// newEntityFactory wires up a real api.Client + cache.Manager + EntityFactory
// pointed at the supplied test server.
func newEntityFactory(t *testing.T, srv *httptest.Server) *EntityFactory {
	t.Helper()
	cfg := minimalCfg{}
	apiClient := api.New(cfg)
	apiClient.SetHTTPClient(&http.Client{
		Transport: &rewriteHTTPClient{
			target: srv.URL,
			base:   &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		},
		Timeout: 2 * time.Second,
	})
	cm := cache.NewManager(t.Context(), apiClient, cfg, log.New(nil), nil)
	return NewEntityFactory(cm)
}

// --- BuildSport / BuildSports ---

func TestEntityFactory_BuildSport(t *testing.T) {
	srv := entityFixtureServer(t)
	defer srv.Close()
	f := newEntityFactory(t, srv)

	urn, _ := types.ParseURN("od:sport:1")
	sport, err := f.BuildSport(t.Context(), *urn, []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("BuildSport: %v", err)
	}
	if sport == nil || sport.ID != *urn {
		t.Fatalf("BuildSport returned %+v", sport)
	}
}

func TestEntityFactory_BuildSports(t *testing.T) {
	srv := entityFixtureServer(t)
	defer srv.Close()
	f := newEntityFactory(t, srv)

	sports, err := f.BuildSports(t.Context(), []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("BuildSports: %v", err)
	}
	if len(sports) == 0 {
		t.Errorf("BuildSports returned empty slice")
	}
}

// --- BuildTournament / BuildTournaments ---

func TestEntityFactory_BuildTournament(t *testing.T) {
	srv := entityFixtureServer(t)
	defer srv.Close()
	f := newEntityFactory(t, srv)

	tournURN, _ := types.ParseURN("od:tournament:7")
	sportURN, _ := types.ParseURN("od:sport:1")
	tour, err := f.BuildTournament(t.Context(), *tournURN, *sportURN, []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("BuildTournament: %v", err)
	}
	if tour == nil || tour.ID != *tournURN {
		t.Fatalf("BuildTournament returned %+v", tour)
	}
}

func TestEntityFactory_BuildTournaments(t *testing.T) {
	srv := entityFixtureServer(t)
	defer srv.Close()
	f := newEntityFactory(t, srv)

	tournURN, _ := types.ParseURN("od:tournament:7")
	sportURN, _ := types.ParseURN("od:sport:1")
	tours, err := f.BuildTournaments(t.Context(), []types.URN{*tournURN}, *sportURN, []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("BuildTournaments: %v", err)
	}
	if len(tours) != 1 {
		t.Errorf("BuildTournaments len = %d, want 1", len(tours))
	}
}

// --- BuildCompetitor / BuildCompetitors / BuildTeamCompetitor ---

func TestEntityFactory_BuildCompetitor(t *testing.T) {
	srv := entityFixtureServer(t)
	defer srv.Close()
	f := newEntityFactory(t, srv)

	urn, _ := types.ParseURN("od:competitor:50")
	c, err := f.BuildCompetitor(t.Context(), *urn, []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("BuildCompetitor: %v", err)
	}
	if c == nil || c.ID != *urn {
		t.Fatalf("BuildCompetitor returned %+v", c)
	}
}

func TestEntityFactory_BuildCompetitors(t *testing.T) {
	srv := entityFixtureServer(t)
	defer srv.Close()
	f := newEntityFactory(t, srv)

	a, _ := types.ParseURN("od:competitor:50")
	b, _ := types.ParseURN("od:competitor:51")
	cs, err := f.BuildCompetitors(t.Context(), []types.URN{*a, *b}, []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("BuildCompetitors: %v", err)
	}
	if len(cs) != 2 {
		t.Errorf("BuildCompetitors len = %d, want 2", len(cs))
	}
}

func TestEntityFactory_BuildTeamCompetitor(t *testing.T) {
	srv := entityFixtureServer(t)
	defer srv.Close()
	f := newEntityFactory(t, srv)

	urn, _ := types.ParseURN("od:competitor:50")
	q := "home"
	tc, err := f.BuildTeamCompetitor(t.Context(), *urn, &q, []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("BuildTeamCompetitor: %v", err)
	}
	if tc == nil || tc.Qualifier.ValueOr("") != q {
		t.Fatalf("BuildTeamCompetitor returned %+v", tc)
	}
}

// --- BuildPlayer ---

func TestEntityFactory_BuildPlayer(t *testing.T) {
	srv := entityFixtureServer(t)
	defer srv.Close()
	f := newEntityFactory(t, srv)

	urn, _ := types.ParseURN("od:player:300")
	p, err := f.BuildPlayer(t.Context(), *urn, types.EnLocale)
	if err != nil {
		t.Fatalf("BuildPlayer: %v", err)
	}
	if p == nil || p.ID != urn.ToString() {
		t.Fatalf("BuildPlayer returned %+v", p)
	}
}

// --- BuildFixture ---

func TestEntityFactory_BuildFixture(t *testing.T) {
	srv := entityFixtureServer(t)
	defer srv.Close()
	f := newEntityFactory(t, srv)

	urn, _ := types.ParseURN("od:match:99")
	fix, err := f.BuildFixture(t.Context(), *urn, types.EnLocale)
	if err != nil {
		t.Fatalf("BuildFixture: %v", err)
	}
	if fix == nil {
		t.Fatalf("BuildFixture returned nil")
	}
	if fix.Locale != types.EnLocale {
		t.Errorf("BuildFixture.Locale = %v, want en", fix.Locale)
	}
}

// --- BuildMatchStatus ---

func TestEntityFactory_BuildMatchStatus(t *testing.T) {
	srv := entityFixtureServer(t)
	defer srv.Close()
	f := newEntityFactory(t, srv)

	urn, _ := types.ParseURN("od:match:99")
	ms, err := f.BuildMatchStatus(t.Context(), *urn, []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("BuildMatchStatus: %v", err)
	}
	if ms == nil {
		t.Fatalf("BuildMatchStatus returned nil")
	}
}

// --- BuildMatch / BuildMatches (the eager fan-out path) ---

func TestEntityFactory_BuildMatch(t *testing.T) {
	srv := entityFixtureServer(t)
	defer srv.Close()
	f := newEntityFactory(t, srv)

	urn, _ := types.ParseURN("od:match:99")
	m, err := f.BuildMatch(t.Context(), *urn, []types.Locale{types.EnLocale}, nil)
	if err != nil {
		t.Fatalf("BuildMatch: %v", err)
	}
	if m == nil || m.ID != *urn {
		t.Fatalf("BuildMatch returned %+v", m)
	}
	if m.Tournament.ID.ToString() != "od:tournament:7" {
		t.Errorf("Match.Tournament.ID = %v", m.Tournament.ID)
	}
	if len(m.Competitors) != 2 {
		t.Errorf("Match competitors = %d, want 2", len(m.Competitors))
	}
}

func TestEntityFactory_BuildMatches(t *testing.T) {
	srv := entityFixtureServer(t)
	defer srv.Close()
	f := newEntityFactory(t, srv)

	urn, _ := types.ParseURN("od:match:99")
	matches, err := f.BuildMatches(t.Context(), []types.URN{*urn}, []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("BuildMatches: %v", err)
	}
	if len(matches) != 1 {
		t.Errorf("BuildMatches len = %d, want 1", len(matches))
	}
}

// --- NewEntityFactory ---

func TestNewEntityFactory(t *testing.T) {
	srv := entityFixtureServer(t)
	defer srv.Close()
	if f := newEntityFactory(t, srv); f == nil {
		t.Fatal("NewEntityFactory returned nil")
	}
}
