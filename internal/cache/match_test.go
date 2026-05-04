package cache

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/internal/api"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

// matchSummaryBody returns a minimal match-summary XML for the given
// (urn, locale) so the merge path can populate the entry.
func matchSummaryBody(matchURN, locale string) string {
	return fmt.Sprintf(`<?xml version="1.0"?>
<match_summary generated_at="2026-01-01T00:00:00Z">
  <sport_event id="%s" name="Match %s name in %s" scheduled="2026-01-01T12:00:00Z">
    <tournament id="od:tournament:7">
      <sport id="od:sport:1"/>
    </tournament>
  </sport_event>
  <sport_event_status status="not_started" match_status_code="0" scoreboard_available="false"/>
</match_summary>`, matchURN, matchURN, locale)
}

// minimalCfg is the smallest OddsFeedConfiguration that satisfies api.Client.
type minimalCfg struct {
	apiURL string
	token  string
}

func (c *minimalCfg) AccessToken() *string                                       { return &c.token }
func (c *minimalCfg) DefaultLocale() types.Locale                            { return types.EnLocale }
func (c *minimalCfg) MaxInactivitySeconds() int                                  { return 20 }
func (c *minimalCfg) MaxRecoveryExecutionMinutes() int                           { return 360 }
func (c *minimalCfg) MessagingPort() int                                         { return 5672 }
func (c *minimalCfg) SdkNodeID() *int                                            { return nil }
func (c *minimalCfg) SelectedEnvironment() *types.Environment                { return nil }
func (c *minimalCfg) SelectedRegion() types.Region                           { return types.RegionDefault }
func (c *minimalCfg) SetRegion(types.Region) types.OddsFeedConfiguration { return c }
func (c *minimalCfg) ExchangeName() string                                       { return "oddinfeed" }
func (c *minimalCfg) ReplayExchangeName() string                                 { return "oddinreplay" }
func (c *minimalCfg) ReportExtendedData() bool                                   { return false }
func (c *minimalCfg) SetExchangeName(string) types.OddsFeedConfiguration     { return c }
func (c *minimalCfg) SetAPIURL(string) types.OddsFeedConfiguration           { return c }
func (c *minimalCfg) SetMQURL(string) types.OddsFeedConfiguration            { return c }
func (c *minimalCfg) SetMessagingPort(int) types.OddsFeedConfiguration       { return c }
func (c *minimalCfg) APIURL() (string, error)                                    { return c.apiURL, nil }
func (c *minimalCfg) MQURL() (string, error)                                     { return "", nil }
func (c *minimalCfg) SportIDPrefix() string                                      { return "od:sport:" }
func (c *minimalCfg) SetSportIDPrefix(string) types.OddsFeedConfiguration    { return c }

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

// newAPIClientForTest builds an api.Client whose every request is
// rewritten to point at the supplied test server.
func newAPIClientForTest(t *testing.T, srv *httptest.Server) *api.Client {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("bad server url: %v", err)
	}
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

// --- tests ---

// TestMatchCache_FetchesAndPopulates verifies a first call hits the API
// and the subsequent call serves from cache (no second HTTP request).
func TestMatchCache_FetchesAndPopulates(t *testing.T) {
	matchURN := "od:match:42"
	urn, _ := types.ParseURN(matchURN)

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, matchSummaryBody(matchURN, "en"))
	}))
	defer srv.Close()

	mc := newMatchCache(newAPIClientForTest(t, srv), log.New(nil))
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	got, err := mc.Match(ctx, *urn, []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if got.ID() != *urn {
		t.Errorf("ID = %v, want %v", got.ID(), *urn)
	}
	name, ok := got.Name(types.EnLocale)
	if !ok || name == "" {
		t.Errorf("Name(en) = (%q, %v), want non-empty", name, ok)
	}

	// Second call — should hit cache, not the server.
	if _, err = mc.Match(ctx, *urn, []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("Match (cached): %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("server hits = %d, want 1 (second call should be cached)", got)
	}
}

// TestMatchCache_MultiLocaleFillIn confirms two locales coexist on the
// cached entry — adding a second locale doesn't overwrite the first.
// This is the multi-locale fix called out in NEXT.md.
func TestMatchCache_MultiLocaleFillIn(t *testing.T) {
	matchURN := "od:match:99"
	urn, _ := types.ParseURN(matchURN)

	var enHits, ruHits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		// Path is /v1/sports/<locale>/sport_events/<urn>/summary
		switch {
		case contains(r.URL.Path, "/sports/en/"):
			enHits.Add(1)
			_, _ = io.WriteString(w, matchSummaryBody(matchURN, "en"))
		case contains(r.URL.Path, "/sports/ru/"):
			ruHits.Add(1)
			_, _ = io.WriteString(w, matchSummaryBody(matchURN, "ru"))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	mc := newMatchCache(newAPIClientForTest(t, srv), log.New(nil))
	ctx := t.Context()

	// First call: en only.
	if _, err := mc.Match(ctx, *urn, []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("Match en: %v", err)
	}

	// Second call: ru. Must NOT re-fetch en (already cached); must
	// fetch ru once.
	got, err := mc.Match(ctx, *urn, []types.Locale{types.EnLocale, types.RuLocale})
	if err != nil {
		t.Fatalf("Match en+ru: %v", err)
	}

	if enHits.Load() != 1 {
		t.Errorf("en hits = %d, want 1 (re-fetched a cached locale)", enHits.Load())
	}
	if ruHits.Load() != 1 {
		t.Errorf("ru hits = %d, want 1", ruHits.Load())
	}

	// Both locales now coexist on the entry.
	if name, ok := got.Name(types.EnLocale); !ok || name == "" {
		t.Errorf("en name missing after ru fetch")
	}
	if name, ok := got.Name(types.RuLocale); !ok || name == "" {
		t.Errorf("ru name missing after fetch")
	}
}

// TestMatchCache_ClearForcesRefetch verifies ClearCacheItem evicts the
// entry; subsequent reads refetch.
func TestMatchCache_ClearForcesRefetch(t *testing.T) {
	matchURN := "od:match:1"
	urn, _ := types.ParseURN(matchURN)

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, matchSummaryBody(matchURN, "en"))
	}))
	defer srv.Close()

	mc := newMatchCache(newAPIClientForTest(t, srv), log.New(nil))
	ctx := t.Context()

	if _, err := mc.Match(ctx, *urn, []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("Match: %v", err)
	}
	mc.ClearCacheItem(*urn)
	if _, err := mc.Match(ctx, *urn, []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("Match after clear: %v", err)
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("hits = %d, want 2 (clear should force refetch)", got)
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
