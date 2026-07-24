package sport

import (
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/cache"
	"github.com/oddin-gg/gosdk/internal/factory"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

// minimalCfg satisfies config.Config.
type minimalCfg struct {
	apiURL string
	token  string
}

func (c *minimalCfg) AccessToken() *string                    { return &c.token }
func (c *minimalCfg) DefaultLocale() types.Locale             { return types.EnLocale }
func (c *minimalCfg) MaxInactivity() time.Duration            { return 20 * time.Second }
func (c *minimalCfg) MaxRecoveryExecution() time.Duration     { return 360 * time.Minute }
func (c *minimalCfg) MessagingPort() int                      { return 5672 }
func (c *minimalCfg) SdkNodeID() *int                         { return nil }
func (c *minimalCfg) SelectedEnvironment() *types.Environment { return nil }
func (c *minimalCfg) SelectedRegion() types.Region            { return types.RegionDefault }
func (c *minimalCfg) ExchangeName() string                    { return "oddinfeed" }
func (c *minimalCfg) ReplayExchangeName() string              { return "oddinreplay" }
func (c *minimalCfg) ReportExtendedData() bool                { return false }
func (c *minimalCfg) APIURL() (string, error)                 { return c.apiURL, nil }
func (c *minimalCfg) MQURL() (string, error)                  { return "", nil }
func (c *minimalCfg) SportIDPrefix() string                   { return "od:sport:" }

type rewriteTransport struct {
	target string
	base   http.RoundTripper
}

func (rt *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t, _ := url.Parse(rt.target)
	req.URL.Scheme = t.Scheme
	req.URL.Host = t.Host
	base := rt.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// fixtureServer dispatches each Oddin sport-domain endpoint to a body.
// Patterns match by HasSuffix; longest pattern wins.
type fixtureServer struct {
	t      *testing.T
	bodies map[string]string
}

func (f *fixtureServer) handler() http.HandlerFunc {
	keys := make([]string, 0, len(f.bodies))
	for k := range f.bodies {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && len(keys[j]) > len(keys[j-1]); j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		for _, k := range keys {
			if matchPath(r.URL.Path, k) {
				_, _ = io.WriteString(w, f.bodies[k])
				return
			}
		}
		f.t.Logf("unhandled path: %s", r.URL.Path)
		http.NotFound(w, r)
	}
}

// matchPath returns true when the request path ends with the pattern,
// or contains the pattern when it ends in "/".
func matchPath(path, pattern string) bool {
	if strings.HasSuffix(pattern, "/") {
		return strings.Contains(path, pattern)
	}
	return strings.HasSuffix(path, pattern)
}

func newSportManager(t *testing.T, srv *httptest.Server) *Manager {
	t.Helper()
	u, _ := url.Parse(srv.URL)
	cfg := &minimalCfg{apiURL: u.Host, token: "tok"}
	apiClient := api.New(cfg)
	apiClient.SetHTTPClient(&http.Client{
		Transport: &rewriteTransport{
			target: srv.URL,
			base:   &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		},
		Timeout: 2 * time.Second,
	})
	cm := cache.NewManager(t.Context(), apiClient, cfg, log.New(nil), nil)
	ef := factory.NewEntityFactory(cm)
	return NewManager(ef, apiClient, cm, cfg)
}

const sportsBody = `<?xml version="1.0"?>
<sports generated_at="2026-01-01T00:00:00">
  <sport id="od:sport:1" name="Soccer" abbreviation="SOC"/>
  <sport id="od:sport:2" name="Basketball" abbreviation="BSK"/>
</sports>`

const sportTournamentsListBody = `<?xml version="1.0"?>
<sport_tournaments generated_at="2026-01-01T00:00:00">
  <sport id="od:sport:1" name="Soccer" abbreviation="SOC"/>
  <tournaments>
    <tournament id="od:tournament:1" name="Premier League" abbreviation="PL" risk_tier="1">
      <sport id="od:sport:1" name="Soccer" abbreviation="SOC"/>
    </tournament>
  </tournaments>
</sport_tournaments>`

const fixtureChangesBody = `<?xml version="1.0"?>
<fixture_changes generated_at="2026-01-01T00:00:00">
  <fixture_change sport_event_id="od:match:1" update_time="2026-01-01T10:00:00"/>
  <fixture_change sport_event_id="od:match:2" update_time="2026-01-01T11:00:00"/>
</fixture_changes>`

const tournamentInfoBody = `<?xml version="1.0"?>
<tournament_info generated_at="2026-01-01T00:00:00">
  <tournament id="od:tournament:1" name="Premier League" abbreviation="PL" risk_tier="1">
    <sport id="od:sport:1" name="Soccer" abbreviation="SOC"/>
  </tournament>
</tournament_info>`

// emptyTournamentListBody returns an empty tournament list whose
// top-level <sport> echoes the REQUESTED sport id — FetchTournaments now
// validates that identity (an empty list carries no nested tournaments to
// check), so the per-route body must match the route's sport.
func emptyTournamentListBody(sportID string) string {
	return `<?xml version="1.0"?>
<sport_tournaments generated_at="2026-01-01T00:00:00">
  <sport id="` + sportID + `" name="Empty" abbreviation="E"/>
</sport_tournaments>`
}

// --- tests ---

// sportsRoutesWithEmptyTournaments returns the body map for tests that
// only need Sports() to work — provides empty tournament lists for both
// sport URNs in sportsBody so BuildSport's eager fan-out succeeds.
func sportsRoutesWithEmptyTournaments() map[string]string {
	return map[string]string{
		"/sports/en/sports":                        sportsBody,
		"/sports/en/sports/od:sport:1/tournaments": emptyTournamentListBody("od:sport:1"),
		"/sports/en/sports/od:sport:2/tournaments": emptyTournamentListBody("od:sport:2"),
	}
}

func TestSport_LocalizedSports(t *testing.T) {
	srv := httptest.NewServer((&fixtureServer{
		t:      t,
		bodies: sportsRoutesWithEmptyTournaments(),
	}).handler())
	defer srv.Close()

	mgr := newSportManager(t, srv)
	got, err := mgr.LocalizedSports(t.Context(), types.EnLocale)
	if err != nil {
		t.Fatalf("LocalizedSports: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %d sports, want 2", len(got))
	}
}

func TestSport_Sports_DefaultLocale(t *testing.T) {
	srv := httptest.NewServer((&fixtureServer{
		t:      t,
		bodies: sportsRoutesWithEmptyTournaments(),
	}).handler())
	defer srv.Close()

	mgr := newSportManager(t, srv)
	if _, err := mgr.Sports(t.Context()); err != nil {
		t.Errorf("Sports: %v", err)
	}
}

func TestSport_LocalizedAvailableTournaments(t *testing.T) {
	srv := httptest.NewServer((&fixtureServer{
		t: t,
		bodies: map[string]string{
			"/sports/en/sports/od:sport:1/tournaments":    sportTournamentsListBody,
			"/sports/en/tournaments/od:tournament:1/info": tournamentInfoBody,
			"/sports/en/sports":                           sportsBody,
		},
	}).handler())
	defer srv.Close()

	mgr := newSportManager(t, srv)
	urn, _ := types.ParseURN("od:sport:1")
	got, err := mgr.LocalizedAvailableTournaments(t.Context(), *urn, types.EnLocale)
	if err != nil {
		t.Fatalf("LocalizedAvailableTournaments: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("got %d tournaments, want 1", len(got))
	}
}

func TestSport_AvailableTournaments_DefaultLocale(t *testing.T) {
	srv := httptest.NewServer((&fixtureServer{
		t: t,
		bodies: map[string]string{
			"/sports/en/sports/od:sport:1/tournaments":    sportTournamentsListBody,
			"/sports/en/tournaments/od:tournament:1/info": tournamentInfoBody,
			"/sports/en/sports":                           sportsBody,
		},
	}).handler())
	defer srv.Close()

	mgr := newSportManager(t, srv)
	urn, _ := types.ParseURN("od:sport:1")
	if _, err := mgr.AvailableTournaments(t.Context(), *urn); err != nil {
		t.Errorf("AvailableTournaments: %v", err)
	}
}

func TestSport_LocalizedFixtureChanges(t *testing.T) {
	srv := httptest.NewServer((&fixtureServer{
		t: t,
		bodies: map[string]string{
			"/fixtures/changes": fixtureChangesBody,
		},
	}).handler())
	defer srv.Close()

	mgr := newSportManager(t, srv)
	got, err := mgr.LocalizedFixtureChanges(t.Context(), types.EnLocale, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("LocalizedFixtureChanges: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %d, want 2", len(got))
	}
	if got[0].SportEventID().ToString() != "od:match:1" {
		t.Errorf("first event = %v", got[0].SportEventID())
	}
}

func TestSport_FixtureChanges_DefaultLocale(t *testing.T) {
	srv := httptest.NewServer((&fixtureServer{
		t: t,
		bodies: map[string]string{
			"/fixtures/changes": fixtureChangesBody,
		},
	}).handler())
	defer srv.Close()

	mgr := newSportManager(t, srv)
	if _, err := mgr.FixtureChanges(t.Context(), time.Now().Add(-time.Hour)); err != nil {
		t.Errorf("FixtureChanges: %v", err)
	}
}

func TestSport_LocalizedListOfMatches_LimitChecks(t *testing.T) {
	srv := httptest.NewServer((&fixtureServer{t: t, bodies: map[string]string{}}).handler())
	defer srv.Close()

	mgr := newSportManager(t, srv)
	if _, err := mgr.LocalizedListOfMatches(t.Context(), 0, 1001, types.EnLocale); err == nil {
		t.Error("limit > 1000 should error")
	}
	if _, err := mgr.LocalizedListOfMatches(t.Context(), 0, 0, types.EnLocale); err == nil {
		t.Error("limit < 1 should error")
	}
}

func TestSport_LocalizedSportActiveTournaments_NotFoundError(t *testing.T) {
	srv := httptest.NewServer((&fixtureServer{
		t:      t,
		bodies: sportsRoutesWithEmptyTournaments(),
	}).handler())
	defer srv.Close()

	mgr := newSportManager(t, srv)
	if _, err := mgr.LocalizedSportActiveTournaments(t.Context(), "nonexistent-sport", types.EnLocale); err == nil {
		t.Error("expected error when sport name doesn't match")
	}
}

func TestSport_FixtureChangeImpl_Accessors(t *testing.T) {
	urn, _ := types.ParseURN("od:match:1")
	now := time.Now()
	f := fixtureChangeImpl{id: *urn, updatedTime: now}
	if f.SportEventID() != *urn {
		t.Errorf("SportEventID = %v", f.SportEventID())
	}
	if !f.UpdateTime().Equal(now) {
		t.Errorf("UpdateTime = %v", f.UpdateTime())
	}
}

func TestSport_ClearMethods(t *testing.T) {
	srv := httptest.NewServer((&fixtureServer{
		t:      t,
		bodies: map[string]string{},
	}).handler())
	defer srv.Close()

	mgr := newSportManager(t, srv)
	urn, _ := types.ParseURN("od:match:1")
	// Ensure no panic.
	mgr.ClearMatch(*urn)
	mgr.ClearTournament(*urn)
	mgr.ClearCompetitor(*urn)
	mgr.ClearPlayer(*urn)
	mgr.ClearSport(*urn)
}

// TestSport_LocalizedSport exercises the new singular Sport accessor.
// The sportsBody fixture exposes od:sport:1; we verify name resolution.
func TestSport_LocalizedSport(t *testing.T) {
	srv := httptest.NewServer((&fixtureServer{
		t:      t,
		bodies: sportsRoutesWithEmptyTournaments(),
	}).handler())
	defer srv.Close()

	mgr := newSportManager(t, srv)
	urn, _ := types.ParseURN("od:sport:1")
	got, err := mgr.LocalizedSport(t.Context(), *urn, types.EnLocale)
	if err != nil {
		t.Fatalf("LocalizedSport: %v", err)
	}
	if got.Name(types.EnLocale).ValueOr("") != "Soccer" {
		t.Errorf("name = %v, want Soccer", got.Name(types.EnLocale))
	}
}

func TestSport_SportActiveTournaments_DefaultLocale(t *testing.T) {
	srv := httptest.NewServer((&fixtureServer{
		t:      t,
		bodies: sportsRoutesWithEmptyTournaments(),
	}).handler())
	defer srv.Close()

	mgr := newSportManager(t, srv)
	if _, err := mgr.SportActiveTournaments(t.Context(), "nonexistent"); err == nil {
		t.Error("expected error")
	}
}

// TestMultiLocalizedListOfMatches_RejectsNegativeStartIndex pins the
// fix for the negative-pagination-offset finding: a negative startIndex
// must fail LOCALLY (deterministic argument error) instead of being
// serialized as ?start=-1 and sent upstream. The zero-value Manager is
// sufficient because the bounds switch returns before any config/API
// access.
func TestMultiLocalizedListOfMatches_RejectsNegativeStartIndex(t *testing.T) {
	m := &Manager{}
	_, err := m.MultiLocalizedListOfMatches(t.Context(), -1, 50, []types.Locale{types.EnLocale})
	if err == nil {
		t.Fatal("negative startIndex was accepted; expected a local error")
	}
	if !strings.Contains(err.Error(), "start index") {
		t.Fatalf("error = %v, want it to mention start index", err)
	}
	// (startIndex 0 and positive values pass the bounds check and take
	// the normal fetch path, which the other manager tests exercise with
	// a live test server.)
}
