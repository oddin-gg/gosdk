package sport

import (
	"context"
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
	"github.com/oddin-gg/gosdk/protocols"
)

// minimalCfg satisfies protocols.OddsFeedConfiguration.
type minimalCfg struct {
	apiURL string
	token  string
}

func (c *minimalCfg) AccessToken() *string                                       { return &c.token }
func (c *minimalCfg) DefaultLocale() protocols.Locale                            { return protocols.EnLocale }
func (c *minimalCfg) MaxInactivitySeconds() int                                  { return 20 }
func (c *minimalCfg) MaxRecoveryExecutionMinutes() int                           { return 360 }
func (c *minimalCfg) MessagingPort() int                                         { return 5672 }
func (c *minimalCfg) SdkNodeID() *int                                            { return nil }
func (c *minimalCfg) SelectedEnvironment() *protocols.Environment                { return nil }
func (c *minimalCfg) SelectedRegion() protocols.Region                           { return protocols.RegionDefault }
func (c *minimalCfg) SetRegion(protocols.Region) protocols.OddsFeedConfiguration { return c }
func (c *minimalCfg) ExchangeName() string                                       { return "oddinfeed" }
func (c *minimalCfg) ReplayExchangeName() string                                 { return "oddinreplay" }
func (c *minimalCfg) ReportExtendedData() bool                                   { return false }
func (c *minimalCfg) SetExchangeName(string) protocols.OddsFeedConfiguration     { return c }
func (c *minimalCfg) SetAPIURL(string) protocols.OddsFeedConfiguration           { return c }
func (c *minimalCfg) SetMQURL(string) protocols.OddsFeedConfiguration            { return c }
func (c *minimalCfg) SetMessagingPort(int) protocols.OddsFeedConfiguration       { return c }
func (c *minimalCfg) APIURL() (string, error)                                    { return c.apiURL, nil }
func (c *minimalCfg) MQURL() (string, error)                                     { return "", nil }
func (c *minimalCfg) SportIDPrefix() string                                      { return "od:sport:" }
func (c *minimalCfg) SetSportIDPrefix(string) protocols.OddsFeedConfiguration    { return c }

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
	var keys []string
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
	cm := cache.NewManager(apiClient, cfg, log.New(nil))
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

const emptyTournamentListBody = `<?xml version="1.0"?>
<sport_tournaments generated_at="2026-01-01T00:00:00">
  <sport id="od:sport:0" name="Empty" abbreviation="E"/>
</sport_tournaments>`

// --- tests ---

// sportsRoutesWithEmptyTournaments returns the body map for tests that
// only need Sports() to work — provides empty tournament lists for both
// sport URNs in sportsBody so BuildSport's eager fan-out succeeds.
func sportsRoutesWithEmptyTournaments() map[string]string {
	return map[string]string{
		"/sports/en/sports":                        sportsBody,
		"/sports/en/sports/od:sport:1/tournaments": emptyTournamentListBody,
		"/sports/en/sports/od:sport:2/tournaments": emptyTournamentListBody,
	}
}

func TestSport_LocalizedSports(t *testing.T) {
	srv := httptest.NewServer((&fixtureServer{
		t:      t,
		bodies: sportsRoutesWithEmptyTournaments(),
	}).handler())
	defer srv.Close()

	mgr := newSportManager(t, srv)
	got, err := mgr.LocalizedSports(context.Background(), protocols.EnLocale)
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
	if _, err := mgr.Sports(context.Background()); err != nil {
		t.Errorf("Sports: %v", err)
	}
}

func TestSport_LocalizedAvailableTournaments(t *testing.T) {
	srv := httptest.NewServer((&fixtureServer{
		t: t,
		bodies: map[string]string{
			"/sports/en/sports/od:sport:1/tournaments":   sportTournamentsListBody,
			"/sports/en/tournaments/od:tournament:1/info": tournamentInfoBody,
			"/sports/en/sports":                          sportsBody,
		},
	}).handler())
	defer srv.Close()

	mgr := newSportManager(t, srv)
	urn, _ := protocols.ParseURN("od:sport:1")
	got, err := mgr.LocalizedAvailableTournaments(context.Background(), *urn, protocols.EnLocale)
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
			"/sports/en/sports/od:sport:1/tournaments":   sportTournamentsListBody,
			"/sports/en/tournaments/od:tournament:1/info": tournamentInfoBody,
			"/sports/en/sports":                          sportsBody,
		},
	}).handler())
	defer srv.Close()

	mgr := newSportManager(t, srv)
	urn, _ := protocols.ParseURN("od:sport:1")
	if _, err := mgr.AvailableTournaments(context.Background(), *urn); err != nil {
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
	got, err := mgr.LocalizedFixtureChanges(context.Background(), protocols.EnLocale, time.Now().Add(-time.Hour))
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
	if _, err := mgr.FixtureChanges(context.Background(), time.Now().Add(-time.Hour)); err != nil {
		t.Errorf("FixtureChanges: %v", err)
	}
}

func TestSport_LocalizedListOfMatches_LimitChecks(t *testing.T) {
	srv := httptest.NewServer((&fixtureServer{t: t, bodies: map[string]string{}}).handler())
	defer srv.Close()

	mgr := newSportManager(t, srv)
	if _, err := mgr.LocalizedListOfMatches(context.Background(), 0, 1001, protocols.EnLocale); err == nil {
		t.Error("limit > 1000 should error")
	}
	if _, err := mgr.LocalizedListOfMatches(context.Background(), 0, 0, protocols.EnLocale); err == nil {
		t.Error("limit < 1 should error")
	}
}

func TestSport_LocalizedSportActiveTournaments_NotFoundError(t *testing.T) {
	srv := httptest.NewServer((&fixtureServer{
		t: t,
		bodies: sportsRoutesWithEmptyTournaments(),
	}).handler())
	defer srv.Close()

	mgr := newSportManager(t, srv)
	if _, err := mgr.LocalizedSportActiveTournaments(context.Background(), "nonexistent-sport", protocols.EnLocale); err == nil {
		t.Error("expected error when sport name doesn't match")
	}
}

func TestSport_FixtureChangeImpl_Accessors(t *testing.T) {
	urn, _ := protocols.ParseURN("od:match:1")
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
	urn, _ := protocols.ParseURN("od:match:1")
	// Ensure no panic.
	mgr.ClearMatch(*urn)
	mgr.ClearTournament(*urn)
	mgr.ClearCompetitor(*urn)
}

func TestSport_SportActiveTournaments_DefaultLocale(t *testing.T) {
	srv := httptest.NewServer((&fixtureServer{
		t: t,
		bodies: sportsRoutesWithEmptyTournaments(),
	}).handler())
	defer srv.Close()

	mgr := newSportManager(t, srv)
	if _, err := mgr.SportActiveTournaments(context.Background(), "nonexistent"); err == nil {
		t.Error("expected error")
	}
}
