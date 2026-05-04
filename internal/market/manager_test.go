package market

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/cache"
	"github.com/oddin-gg/gosdk/internal/factory"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

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
	t, _ := url.Parse(rt.target)
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

const marketsBody = `<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="1" name="1x2" groups="all">
    <outcomes>
      <outcome id="1" name="home"/>
      <outcome id="2" name="draw"/>
      <outcome id="3" name="away"/>
    </outcomes>
  </market>
  <market id="18" name="total" groups="all">
    <outcomes>
      <outcome id="13" name="under {total}"/>
      <outcome id="12" name="over {total}"/>
    </outcomes>
    <specifiers>
      <specifier name="total" type="decimal"/>
    </specifiers>
  </market>
</market_descriptions>`

const voidReasonsBody = `<?xml version="1.0"?>
<void_reasons response_code="OK">
  <void_reason id="1" name="game_canceled" description="The game was canceled" template="Game canceled"/>
  <void_reason id="2" name="player_substituted" description="Player substituted" template="Player {p} substituted">
    <param name="p"/>
  </void_reason>
</void_reasons>`

// --- tests ---

func newMarketManager(t *testing.T, srv *httptest.Server) *Manager {
	t.Helper()
	cfg := &minimalCfg{}
	apiClient := newAPIClient(t, srv)
	cm := cache.NewManager(apiClient, cfg, log.New(nil))
	mdf := factory.NewMarketDescriptionFactory(
		cm.MarketDescriptionCache,
		cm.MarketVoidReasonsCache,
		cm.PlayersCache,
		cm.CompetitorCache,
	)
	return NewManager(cm, mdf, cfg)
}

func TestMarketManager_LocalizedMarketDescriptions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		switch {
		case contains(r.URL.Path, "/markets"):
			_, _ = io.WriteString(w, marketsBody)
		case contains(r.URL.Path, "/void_reasons"):
			_, _ = io.WriteString(w, voidReasonsBody)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	mgr := newMarketManager(t, srv)
	descs, err := mgr.LocalizedMarketDescriptions(context.Background(), types.EnLocale)
	if err != nil {
		t.Fatalf("LocalizedMarketDescriptions: %v", err)
	}
	if len(descs) != 2 {
		t.Errorf("got %d descriptions, want 2", len(descs))
	}
}

func TestMarketManager_MarketDescriptionByIDAndVariant(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, marketsBody)
	}))
	defer srv.Close()

	mgr := newMarketManager(t, srv)
	desc, err := mgr.MarketDescriptionByIDAndVariant(context.Background(), 1, nil)
	if err != nil {
		t.Fatalf("MarketDescriptionByIDAndVariant: %v", err)
	}
	if desc == nil || desc.ID != 1 {
		t.Errorf("desc = %+v", desc)
	}
	if name := desc.LocalizedName(types.EnLocale); name == nil || *name != "1x2" {
		t.Errorf("name = %v", name)
	}
}

func TestMarketManager_MarketDescriptions_DefaultLocale(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, marketsBody)
	}))
	defer srv.Close()

	mgr := newMarketManager(t, srv)
	descs, err := mgr.MarketDescriptions(context.Background())
	if err != nil {
		t.Fatalf("MarketDescriptions: %v", err)
	}
	if len(descs) != 2 {
		t.Errorf("got %d", len(descs))
	}
}

func TestMarketManager_VoidReasons(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, voidReasonsBody)
	}))
	defer srv.Close()

	mgr := newMarketManager(t, srv)
	reasons, err := mgr.MarketVoidReasons(context.Background())
	if err != nil {
		t.Fatalf("MarketVoidReasons: %v", err)
	}
	if len(reasons) != 2 {
		t.Errorf("got %d reasons, want 2", len(reasons))
	}
	// One reason has params, one doesn't.
	hasParams := false
	for _, r := range reasons {
		if len(r.Params) > 0 {
			hasParams = true
		}
	}
	if !hasParams {
		t.Error("expected one reason with params")
	}
}

func TestMarketManager_ReloadVoidReasons(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, voidReasonsBody)
	}))
	defer srv.Close()

	mgr := newMarketManager(t, srv)
	if _, err := mgr.MarketVoidReasons(context.Background()); err != nil {
		t.Fatalf("first MarketVoidReasons: %v", err)
	}
	if hits != 1 {
		t.Errorf("expected 1 hit, got %d", hits)
	}
	if _, err := mgr.ReloadMarketVoidReasons(context.Background()); err != nil {
		t.Fatalf("ReloadMarketVoidReasons: %v", err)
	}
	if hits != 2 {
		t.Errorf("expected 2 hits after reload, got %d", hits)
	}
}

func TestMarketManager_ClearMarketDescription(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, marketsBody)
	}))
	defer srv.Close()

	mgr := newMarketManager(t, srv)
	// Populate cache.
	if _, err := mgr.MarketDescriptions(context.Background()); err != nil {
		t.Fatalf("MarketDescriptions: %v", err)
	}
	// Clear should not panic; subsequent calls still work.
	mgr.ClearMarketDescription(1, nil)
	if _, err := mgr.MarketDescriptionByIDAndVariant(context.Background(), 1, nil); err != nil {
		t.Errorf("after clear: %v", err)
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
