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

// minimalCfg is the smallest config.Config that satisfies api.Client.
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

	mc := newMatchCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))
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

	mc := newMatchCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))
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

	mc := newMatchCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))
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

// TestMatchCache_ReferenceIDsDecode is the regression for the
// forward-port of main commit fcc3c0d (PR #38): SportEvent's
// `reference_ids` block must be merged into LocalizedMatch.referenceIDs
// and surface on the cached entry. The XML decode + cache merge
// happens inside mc.Match; we read the entry directly here (the
// public types.Match.ReferenceIDs projection is exercised via
// BuildMatch in the gosdk-level tests).
func TestMatchCache_ReferenceIDsDecode(t *testing.T) {
	matchURN := "od:match:99"
	urn, _ := types.ParseURN(matchURN)

	body := `<?xml version="1.0"?>
<match_summary generated_at="2026-01-01T00:00:00Z">
  <sport_event id="` + matchURN + `" name="X" scheduled="2026-01-01T12:00:00Z">
    <tournament id="od:tournament:7"><sport id="od:sport:1"/></tournament>
    <reference_ids>
      <reference_id name="betradar" value="abc123"/>
      <reference_id name="external" value="xyz789"/>
    </reference_ids>
  </sport_event>
  <sport_event_status status="not_started" match_status_code="0" scoreboard_available="false"/>
</match_summary>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	mc := newMatchCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))
	entry, err := mc.Match(t.Context(), *urn, []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("Match: %v", err)
	}

	_, _, _, _, _, _, _, _, _, refIDs := entry.snapshot()
	if got, want := refIDs["betradar"], "abc123"; got != want {
		t.Errorf("refIDs[betradar] = %q, want %q", got, want)
	}
	if got, want := refIDs["external"], "xyz789"; got != want {
		t.Errorf("refIDs[external] = %q, want %q", got, want)
	}
	if len(refIDs) != 2 {
		t.Errorf("len(refIDs) = %d, want 2", len(refIDs))
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

// TestMatchCache_LiveOddsMapping is the regression for the
// missing/unknown-liveodds default (Codex P2): only "not_available"
// mapped to NotAvailable and EVERYTHING else — including an absent
// attribute, a malformed value, and future enum values — defaulted to
// Available, so consumers could enable live behavior with no confirmed
// availability. Post-fix the known bookable states map to Available and
// everything else fails closed.
func TestMatchCache_LiveOddsMapping(t *testing.T) {
	cases := []struct {
		liveodds string // raw attribute; "" = omitted
		want     types.LiveOddsAvailability
	}{
		{"booked", types.AvailableLiveOddsAvailability},
		{"bookable", types.AvailableLiveOddsAvailability},
		{"buyable", types.AvailableLiveOddsAvailability},
		{"not_available", types.NotAvailableLiveOddsAvailability},
		{"", types.NotAvailableLiveOddsAvailability},                  // absent attribute
		{"some_future_state", types.NotAvailableLiveOddsAvailability}, // unknown enum
	}
	for i, tc := range cases {
		matchURN := fmt.Sprintf("od:match:%d", 9100+i)
		urn, _ := types.ParseURN(matchURN)
		attr := ""
		if tc.liveodds != "" {
			attr = fmt.Sprintf(` liveodds="%s"`, tc.liveodds)
		}
		body := fmt.Sprintf(`<?xml version="1.0"?>
<match_summary generated_at="2026-01-01T00:00:00Z">
  <sport_event id="%s" name="M" scheduled="2026-01-01T12:00:00Z"%s>
    <tournament id="od:tournament:7">
      <sport id="od:sport:1"/>
    </tournament>
  </sport_event>
  <sport_event_status status="not_started" match_status_code="0" scoreboard_available="false"/>
</match_summary>`, matchURN, attr)

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(w, body)
		}))

		mc := newMatchCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))
		got, err := mc.Match(t.Context(), *urn, []types.Locale{types.EnLocale})
		if err != nil {
			srv.Close()
			t.Fatalf("liveodds=%q: Match: %v", tc.liveodds, err)
		}
		if la := got.LiveOddsAvailability(); la == nil || *la != tc.want {
			t.Errorf("liveodds=%q mapped to %v, want %v", tc.liveodds, la, tc.want)
		}
		srv.Close()
	}
}
