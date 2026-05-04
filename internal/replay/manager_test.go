package replay

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/protocols"
)

// minimalCfg is the smallest OddsFeedConfiguration that satisfies the
// api.Client + the replay manager.
type minimalCfg struct {
	apiURL string
	token  string
	nodeID *int
}

func (c *minimalCfg) AccessToken() *string                                       { return &c.token }
func (c *minimalCfg) DefaultLocale() protocols.Locale                            { return protocols.EnLocale }
func (c *minimalCfg) MaxInactivitySeconds() int                                  { return 20 }
func (c *minimalCfg) MaxRecoveryExecutionMinutes() int                           { return 360 }
func (c *minimalCfg) MessagingPort() int                                         { return 5672 }
func (c *minimalCfg) SdkNodeID() *int                                            { return c.nodeID }
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
	t, err := url.Parse(rt.target)
	if err != nil {
		return nil, err
	}
	req.URL.Scheme = t.Scheme
	req.URL.Host = t.Host
	base := rt.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

func newAPIClient(t *testing.T, srv *httptest.Server) *api.Client {
	t.Helper()
	u, _ := url.Parse(srv.URL)
	c := api.New(&minimalCfg{apiURL: u.Host, token: "tok"})
	c.SetHTTPClient(&http.Client{
		Transport: &rewriteTransport{
			target: srv.URL,
			base:   &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		},
		Timeout: 2 * time.Second,
	})
	return c
}

// fakeSportsInfo returns a deterministic Match (or error) for any URN.
type fakeSportsInfo struct {
	calls atomic.Int64
	err   error
}

func (f *fakeSportsInfo) Match(ctx context.Context, id protocols.URN) (protocols.Match, error) {
	f.calls.Add(1)
	if f.err != nil {
		return protocols.Match{}, f.err
	}
	return protocols.Match{ID: id}, nil
}

// The remaining SportsInfoManager methods are not exercised by Replay;
// stub them out.
func (f *fakeSportsInfo) Sports(context.Context) ([]protocols.Sport, error)         { return nil, nil }
func (f *fakeSportsInfo) LocalizedSports(context.Context, protocols.Locale) ([]protocols.Sport, error) {
	return nil, nil
}
func (f *fakeSportsInfo) ActiveTournaments(context.Context) ([]protocols.Tournament, error) {
	return nil, nil
}
func (f *fakeSportsInfo) LocalizedActiveTournaments(context.Context, protocols.Locale) ([]protocols.Tournament, error) {
	return nil, nil
}
func (f *fakeSportsInfo) SportActiveTournaments(context.Context, string) ([]protocols.Tournament, error) {
	return nil, nil
}
func (f *fakeSportsInfo) LocalizedSportActiveTournaments(context.Context, string, protocols.Locale) ([]protocols.Tournament, error) {
	return nil, nil
}
func (f *fakeSportsInfo) MatchesFor(context.Context, time.Time) ([]protocols.Match, error) {
	return nil, nil
}
func (f *fakeSportsInfo) LocalizedMatchesFor(context.Context, time.Time, protocols.Locale) ([]protocols.Match, error) {
	return nil, nil
}
func (f *fakeSportsInfo) LiveMatches(context.Context) ([]protocols.Match, error) { return nil, nil }
func (f *fakeSportsInfo) LocalizedLiveMatches(context.Context, protocols.Locale) ([]protocols.Match, error) {
	return nil, nil
}
func (f *fakeSportsInfo) LocalizedMatch(ctx context.Context, id protocols.URN, _ protocols.Locale) (protocols.Match, error) {
	return f.Match(ctx, id)
}
func (f *fakeSportsInfo) Competitor(context.Context, protocols.URN) (protocols.Competitor, error) {
	return protocols.Competitor{}, nil
}
func (f *fakeSportsInfo) LocalizedCompetitor(context.Context, protocols.URN, protocols.Locale) (protocols.Competitor, error) {
	return protocols.Competitor{}, nil
}
func (f *fakeSportsInfo) FixtureChanges(context.Context, time.Time) ([]protocols.FixtureChange, error) {
	return nil, nil
}
func (f *fakeSportsInfo) LocalizedFixtureChanges(context.Context, protocols.Locale, time.Time) ([]protocols.FixtureChange, error) {
	return nil, nil
}
func (f *fakeSportsInfo) ListOfMatches(context.Context, uint, uint) ([]protocols.Match, error) {
	return nil, nil
}
func (f *fakeSportsInfo) LocalizedListOfMatches(context.Context, uint, uint, protocols.Locale) ([]protocols.Match, error) {
	return nil, nil
}
func (f *fakeSportsInfo) AvailableTournaments(context.Context, protocols.URN) ([]protocols.Tournament, error) {
	return nil, nil
}
func (f *fakeSportsInfo) LocalizedAvailableTournaments(context.Context, protocols.URN, protocols.Locale) ([]protocols.Tournament, error) {
	return nil, nil
}
func (f *fakeSportsInfo) ClearMatch(protocols.URN)      {}
func (f *fakeSportsInfo) ClearTournament(protocols.URN) {}
func (f *fakeSportsInfo) ClearCompetitor(protocols.URN) {}

// --- tests ---

func TestReplay_AddSportEventID(t *testing.T) {
	var pathSeen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pathSeen = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `<empty/>`)
	}))
	defer srv.Close()

	cfg := &minimalCfg{}
	mgr := NewManager(newAPIClient(t, srv), cfg, &fakeSportsInfo{})

	urn, _ := protocols.ParseURN("od:match:42")
	ok, err := mgr.AddSportEventID(context.Background(), *urn)
	if err != nil {
		t.Fatalf("AddSportEventID: %v", err)
	}
	if !ok {
		t.Error("AddSportEventID returned false")
	}
	if pathSeen != "/v1/replay/events/od:match:42" {
		t.Errorf("path = %q", pathSeen)
	}
}

func TestReplay_AddSportEventID_WithNodeID(t *testing.T) {
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `<empty/>`)
	}))
	defer srv.Close()

	id := 7
	cfg := &minimalCfg{nodeID: &id}
	mgr := NewManager(newAPIClient(t, srv), cfg, &fakeSportsInfo{})

	urn, _ := protocols.ParseURN("od:match:42")
	if _, err := mgr.AddSportEventID(context.Background(), *urn); err != nil {
		t.Fatalf("AddSportEventID: %v", err)
	}
	if query != "node_id=7" {
		t.Errorf("query = %q, want node_id=7", query)
	}
}

func TestReplay_RemoveSportEventID(t *testing.T) {
	var method string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `<empty/>`)
	}))
	defer srv.Close()

	cfg := &minimalCfg{}
	mgr := NewManager(newAPIClient(t, srv), cfg, &fakeSportsInfo{})

	urn, _ := protocols.ParseURN("od:match:42")
	if _, err := mgr.RemoveSportEventID(context.Background(), *urn); err != nil {
		t.Fatalf("RemoveSportEventID: %v", err)
	}
	if method != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", method)
	}
}

func TestReplay_Play(t *testing.T) {
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `<empty/>`)
	}))
	defer srv.Close()

	cfg := &minimalCfg{}
	mgr := NewManager(newAPIClient(t, srv), cfg, &fakeSportsInfo{})

	speed := 10
	maxDelay := 50
	rewrite := true
	if _, err := mgr.Play(context.Background(), protocols.ReplayPlayParams{
		Speed:             &speed,
		MaxDelayInMs:      &maxDelay,
		RewriteTimestamps: &rewrite,
	}); err != nil {
		t.Fatalf("Play: %v", err)
	}
	for _, want := range []string{"speed=10", "max_delay=50", "use_replay_timestamp=true"} {
		if !contains(query, want) {
			t.Errorf("query %q missing %q", query, want)
		}
	}
}

func TestReplay_StopAndClear(t *testing.T) {
	stopHit, clearHit := false, false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/replay/stop":
			stopHit = true
		case "/v1/replay/clear":
			clearHit = true
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `<empty/>`)
	}))
	defer srv.Close()

	cfg := &minimalCfg{}
	mgr := NewManager(newAPIClient(t, srv), cfg, &fakeSportsInfo{})
	if _, err := mgr.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := mgr.Clear(context.Background()); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if !stopHit || !clearHit {
		t.Errorf("expected both stop and clear paths hit (stop=%v clear=%v)", stopHit, clearHit)
	}
}

func TestReplay_ReplayList_PopulatesMatches(t *testing.T) {
	body := `<?xml version="1.0"?>
<replay_set_content>
  <replay_event id="od:match:1" position="0"/>
  <replay_event id="od:match:2" position="1"/>
</replay_set_content>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	cfg := &minimalCfg{}
	si := &fakeSportsInfo{}
	mgr := NewManager(newAPIClient(t, srv), cfg, si)

	matches, err := mgr.ReplayList(context.Background())
	if err != nil {
		t.Fatalf("ReplayList: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("got %d matches, want 2", len(matches))
	}
	if matches[0].ID.ToString() != "od:match:1" || matches[1].ID.ToString() != "od:match:2" {
		t.Errorf("ids = %v / %v", matches[0].ID, matches[1].ID)
	}
	if si.calls.Load() != 2 {
		t.Errorf("sportsInfoManager.Match calls = %d, want 2", si.calls.Load())
	}
}

func TestReplay_ReplayList_PropagatesSportsInfoError(t *testing.T) {
	body := `<?xml version="1.0"?>
<replay_set_content>
  <replay_event id="od:match:1" position="0"/>
</replay_set_content>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	cfg := &minimalCfg{}
	si := &fakeSportsInfo{err: errors.New("boom")}
	mgr := NewManager(newAPIClient(t, srv), cfg, si)

	if _, err := mgr.ReplayList(context.Background()); err == nil {
		t.Fatal("expected SportsInfoManager error to propagate")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
