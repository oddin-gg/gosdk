package whoami

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/types"
)

// minimalCfg satisfies types.OddsFeedConfiguration for tests.
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

func newAPIClient(t *testing.T, srv *httptest.Server) *api.Client {
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

const okBody = `<?xml version="1.0"?>
<bookmaker_details response_code="OK" expire_at="2099-01-01T00:00:00" bookmaker_id="42" virtual_host="/vhost"/>`

const expiringBody = `<?xml version="1.0"?>
<bookmaker_details response_code="OK" expire_at="2026-05-08T00:00:00" bookmaker_id="42" virtual_host="/vhost"/>`

// --- tests ---

func TestManager_BookmakerDetails_FetchesAndCaches(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, okBody)
	}))
	defer srv.Close()

	cfg := &minimalCfg{}
	mgr := NewManager(cfg, newAPIClient(t, srv))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	bd1, err := mgr.BookmakerDetails(ctx)
	if err != nil {
		t.Fatalf("BookmakerDetails: %v", err)
	}
	if bd1.BookmakerID() != 42 {
		t.Errorf("BookmakerID = %d, want 42", bd1.BookmakerID())
	}
	if bd1.VirtualHost() != "/vhost" {
		t.Errorf("VirtualHost = %q, want /vhost", bd1.VirtualHost())
	}
	if bd1.ExpireAt().IsZero() {
		t.Error("ExpireAt should be populated")
	}

	// Second call should hit the cache (no new HTTP request).
	bd2, err := mgr.BookmakerDetails(ctx)
	if err != nil {
		t.Fatalf("BookmakerDetails (cached): %v", err)
	}
	if bd2 != bd1 {
		t.Errorf("cached call returned a different value")
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("server hits = %d, want 1", got)
	}
}

func TestManager_BookmakerDetails_PropagatesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w,
			`<?xml version="1.0"?><response response_code="FORBIDDEN"><action>auth</action><message>bad</message></response>`)
	}))
	defer srv.Close()

	cfg := &minimalCfg{}
	mgr := NewManager(cfg, newAPIClient(t, srv))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if _, err := mgr.BookmakerDetails(ctx); err == nil {
		t.Fatal("expected error on 401")
	}
}

// TestManager_LogsWhenTokenExpiresSoon verifies the soon-to-expire warning.
// Capture slog output via a custom handler.
func TestManager_LogsWhenTokenExpiresSoon(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, expiringBody)
	}))
	defer srv.Close()

	var captured warnCounter
	logger := slog.New(&captured)
	cfg := &minimalCfg{}
	mgr := NewManagerWithLogger(cfg, newAPIClient(t, srv), logger)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := mgr.BookmakerDetails(ctx); err != nil {
		t.Fatalf("BookmakerDetails: %v", err)
	}

	// The expireAt is "2026-05-08" — well within the 7-day-warning window
	// once today is past 2026-05-01. We assert the warning fires when
	// the date is within 7d, and that's the case for the canned date.
	// (If you run this test in 2027+, the canned date is in the past
	// and < 7 days from now, so it still warns.)
	if !captured.hasWarn() {
		t.Error("expected a warn-level log when token expires within 7 days")
	}
}

// warnCounter is a slog.Handler that records whether any Warn-level
// record passed through.
type warnCounter struct {
	warns atomic.Int64
}

func (w *warnCounter) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelWarn
}
func (w *warnCounter) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		w.warns.Add(1)
	}
	return nil
}
func (w *warnCounter) WithAttrs(_ []slog.Attr) slog.Handler { return w }
func (w *warnCounter) WithGroup(_ string) slog.Handler      { return w }
func (w *warnCounter) hasWarn() bool                        { return w.warns.Load() > 0 }

// TestManager_BookmakerDetailImpl_Accessors covers the small impl type's
// accessors.
func TestManager_BookmakerDetailImpl_Accessors(t *testing.T) {
	now := time.Now()
	b := bookmakerDetailImpl{
		expireAt:    now,
		bookmakerID: 7,
		virtualHost: "/vhost",
	}
	if !b.ExpireAt().Equal(now) {
		t.Errorf("ExpireAt = %v", b.ExpireAt())
	}
	if b.BookmakerID() != 7 {
		t.Errorf("BookmakerID = %d", b.BookmakerID())
	}
	if b.VirtualHost() != "/vhost" {
		t.Errorf("VirtualHost = %q", b.VirtualHost())
	}
}
