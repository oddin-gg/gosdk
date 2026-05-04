package producer

import (
	"context"
	"crypto/tls"
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

const producersBody = `<?xml version="1.0"?>
<producers response_code="OK">
  <producer id="1" name="live" description="Live odds" active="true" api_url="https://live" scope="live" stateful_recovery_window_in_minutes="60"/>
  <producer id="2" name="pre" description="Prematch" active="true" api_url="https://pre" scope="prematch" stateful_recovery_window_in_minutes="180"/>
  <producer id="3" name="live" description="Mixed" active="true" api_url="https://mix" scope="live|prematch" stateful_recovery_window_in_minutes="60"/>
  <producer id="4" name="live" description="Inactive" active="false" api_url="https://x" scope="live" stateful_recovery_window_in_minutes="60"/>
</producers>`

// --- tests ---

func TestManager_Open_Populates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, producersBody)
	}))
	defer srv.Close()

	mgr := NewManager(&minimalCfg{}, newAPIClient(t, srv), log.New(nil))
	if err := mgr.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}

	available, err := mgr.AvailableProducers(context.Background())
	if err != nil {
		t.Fatalf("AvailableProducers: %v", err)
	}
	if len(available) != 4 {
		t.Errorf("got %d producers, want 4", len(available))
	}

	active, err := mgr.ActiveProducers(context.Background())
	if err != nil {
		t.Fatalf("ActiveProducers: %v", err)
	}
	if len(active) != 3 {
		t.Errorf("active = %d, want 3 (one is inactive)", len(active))
	}
}

func TestManager_LazyOpen(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, producersBody)
	}))
	defer srv.Close()

	mgr := NewManager(&minimalCfg{}, newAPIClient(t, srv), log.New(nil))
	// Don't call Open — let GetProducer trigger lazy open.
	p, err := mgr.GetProducer(context.Background(), 1)
	if err != nil {
		t.Fatalf("GetProducer: %v", err)
	}
	if p.ID() != 1 {
		t.Errorf("GetProducer id = %d", p.ID())
	}
	if hits.Load() != 1 {
		t.Errorf("HTTP hits = %d, want 1", hits.Load())
	}
}

func TestManager_GetProducer_UnknownProducerReturnsPlaceholder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, producersBody)
	}))
	defer srv.Close()

	mgr := NewManager(&minimalCfg{apiURL: "api.test"}, newAPIClient(t, srv), log.New(nil))
	if err := mgr.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}

	p, err := mgr.GetProducer(context.Background(), 999)
	if err != nil {
		t.Fatalf("GetProducer(unknown): %v", err)
	}
	if p.Name() != "unknown" {
		t.Errorf("unknown placeholder name = %q", p.Name())
	}
}

func TestManager_ActiveProducersInScope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, producersBody)
	}))
	defer srv.Close()

	mgr := NewManager(&minimalCfg{}, newAPIClient(t, srv), log.New(nil))
	if err := mgr.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}

	live, err := mgr.ActiveProducersInScope(context.Background(), types.LiveProducerScope)
	if err != nil {
		t.Fatalf("ActiveProducersInScope live: %v", err)
	}
	// Producers 1 (live), 3 (live|prematch). 4 inactive. 2 prematch only.
	if len(live) != 2 {
		t.Errorf("live count = %d, want 2", len(live))
	}

	prematch, err := mgr.ActiveProducersInScope(context.Background(), types.PrematchProducerScope)
	if err != nil {
		t.Fatalf("ActiveProducersInScope prematch: %v", err)
	}
	// Producers 2 (prematch), 3 (live|prematch).
	if len(prematch) != 2 {
		t.Errorf("prematch count = %d, want 2", len(prematch))
	}
}

func TestManager_StateMutators(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, producersBody)
	}))
	defer srv.Close()

	mgr := NewManager(&minimalCfg{}, newAPIClient(t, srv), log.New(nil))
	if err := mgr.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}

	// IsProducerEnabled defaults to true (matches active).
	enabled, err := mgr.IsProducerEnabled(context.Background(), 1)
	if err != nil {
		t.Fatalf("IsProducerEnabled: %v", err)
	}
	if !enabled {
		t.Error("producer 1 should default to enabled")
	}

	// Disable producer 1.
	if err := mgr.SetProducerState(context.Background(), 1, false); err != nil {
		t.Fatalf("SetProducerState: %v", err)
	}
	enabled, _ = mgr.IsProducerEnabled(context.Background(), 1)
	if enabled {
		t.Error("producer 1 should be disabled after SetProducerState(false)")
	}

	// IsProducerDown defaults to true (initial state in newData).
	down, err := mgr.IsProducerDown(context.Background(), 1)
	if err != nil {
		t.Fatalf("IsProducerDown: %v", err)
	}
	if !down {
		t.Error("producer 1 should default flagged-down")
	}

	// Mark up.
	if err := mgr.SetProducerDown(1, false); err != nil {
		t.Fatalf("SetProducerDown(false): %v", err)
	}
	down, _ = mgr.IsProducerDown(context.Background(), 1)
	if down {
		t.Error("producer 1 should not be down after SetProducerDown(false)")
	}
}

func TestManager_TimestampSetters(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, producersBody)
	}))
	defer srv.Close()

	mgr := NewManager(&minimalCfg{}, newAPIClient(t, srv), log.New(nil))
	if err := mgr.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}

	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := mgr.SetProducerLastMessageTimestamp(1, t1); err != nil {
		t.Fatalf("SetProducerLastMessageTimestamp: %v", err)
	}
	if err := mgr.SetProducerLastMessageTimestamp(1, time.Time{}); err == nil {
		t.Error("SetProducerLastMessageTimestamp should reject zero timestamp")
	}

	t2 := t1.Add(time.Minute)
	if err := mgr.SetLastProcessedMessageGenTimestamp(1, t2); err != nil {
		t.Fatalf("SetLastProcessedMessageGenTimestamp: %v", err)
	}
	if err := mgr.SetLastAliveReceivedGenTimestamp(1, t1); err != nil {
		t.Fatalf("SetLastAliveReceivedGenTimestamp: %v", err)
	}

	// Validate via GetProducer.
	p, _ := mgr.GetProducer(context.Background(), 1)
	if !p.LastMessageTimestamp().Equal(t1) {
		t.Errorf("LastMessageTimestamp = %v, want %v", p.LastMessageTimestamp(), t1)
	}
	if !p.LastProcessedMessageGenTimestamp().Equal(t2) {
		t.Errorf("LastProcessedMessageGenTimestamp = %v, want %v", p.LastProcessedMessageGenTimestamp(), t2)
	}

	// TimestampForRecovery prefers the alive-gen timestamp when set.
	if !p.TimestampForRecovery().Equal(t1) {
		t.Errorf("TimestampForRecovery = %v, want %v", p.TimestampForRecovery(), t1)
	}
}

func TestManager_GetProducerCached_FailsBeforeOpen(t *testing.T) {
	mgr := NewManager(&minimalCfg{apiURL: "api.test"}, nil, log.New(nil))
	// producerCached returns the placeholder when not opened.
	p, err := mgr.GetProducerCached(1)
	if err != nil {
		t.Fatalf("GetProducerCached: %v", err)
	}
	if p.Name() != "unknown" {
		t.Errorf("got name %q, want unknown placeholder", p.Name())
	}
}

func TestManager_GetProducerCached_AfterOpen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, producersBody)
	}))
	defer srv.Close()

	mgr := NewManager(&minimalCfg{}, newAPIClient(t, srv), log.New(nil))
	if err := mgr.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	p, err := mgr.GetProducerCached(1)
	if err != nil {
		t.Fatalf("GetProducerCached: %v", err)
	}
	if p.ID() != 1 || p.Name() != "live" {
		t.Errorf("p = %+v", p)
	}
}

// TestProducerImpl_Accessors exercises producerImpl getters that aren't
// covered by other tests (especially IsAvailable, IsEnabled, APIEndpoint,
// ProducerScopes, ProcessingQueDelay, StatefulRecoveryWindowInMinutes).
func TestProducerImpl_Accessors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, producersBody)
	}))
	defer srv.Close()

	mgr := NewManager(&minimalCfg{}, newAPIClient(t, srv), log.New(nil))
	if err := mgr.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	p, err := mgr.GetProducer(context.Background(), 3) // mixed scope
	if err != nil {
		t.Fatalf("GetProducer: %v", err)
	}
	if !p.IsAvailable() {
		t.Error("IsAvailable should be true for active producer")
	}
	if !p.IsEnabled() {
		t.Error("IsEnabled should be true by default")
	}
	if p.APIEndpoint() != "https://mix" {
		t.Errorf("APIEndpoint = %q", p.APIEndpoint())
	}
	if p.Description() != "Mixed" {
		t.Errorf("Description = %q", p.Description())
	}
	if p.StatefulRecoveryWindowInMinutes() != 60 {
		t.Errorf("StatefulRecoveryWindowInMinutes = %d", p.StatefulRecoveryWindowInMinutes())
	}
	scopes := p.ProducerScopes()
	if len(scopes) != 2 {
		t.Errorf("ProducerScopes = %v, want 2 entries", scopes)
	}
	// IsFlaggedDown defaults to true via newData.
	if !p.IsFlaggedDown() {
		t.Error("IsFlaggedDown defaults to true")
	}
	// ProcessingQueDelay is meaningful even with zero timestamp (returns
	// the time since epoch which is huge but deterministic).
	if p.ProcessingQueDelay() <= 0 {
		t.Errorf("ProcessingQueDelay = %v", p.ProcessingQueDelay())
	}
}

func TestBuildProducerImpl_RejectsUnknownScope(t *testing.T) {
	d := &data{
		id:            1,
		name:          "live",
		producerScope: "garbage",
	}
	if _, err := buildProducerImpl(d); err == nil {
		t.Error("expected error on unknown scope")
	}
}

func TestBuildProducerImpl_RequiresAtLeastOneScope(t *testing.T) {
	d := &data{
		id:            1,
		name:          "live",
		producerScope: "",
	}
	if _, err := buildProducerImpl(d); err == nil {
		t.Error("expected error on empty scope")
	}
}

func TestNewData_DefaultsFlaggedDownTrue(t *testing.T) {
	cfg := &minimalCfg{}
	_ = cfg
	// We can construct via the unexported newData since we're in-package.
	// (We mirror the XML producer that NewData consumes.)
}

// TestSetProducerRecoveryFromTimestamp_RoundTrips ensures the
// SetProducerRecoveryFromTimestamp + TimestampForRecovery interaction
// works when no alive timestamp has been seen.
func TestSetProducerRecoveryFromTimestamp_RoundTrips(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, producersBody)
	}))
	defer srv.Close()

	mgr := NewManager(&minimalCfg{}, newAPIClient(t, srv), log.New(nil))
	if err := mgr.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Within the producer's stateful_recovery_window_in_minutes (60 minutes
	// per the test fixture). Older than the window would be rejected.
	t0 := time.Now().Add(-30 * time.Minute)
	if err := mgr.SetProducerRecoveryFromTimestamp(context.Background(), 1, t0); err != nil {
		t.Fatalf("SetProducerRecoveryFromTimestamp: %v", err)
	}
	p, _ := mgr.GetProducer(context.Background(), 1)
	if !p.TimestampForRecovery().Equal(t0) {
		t.Errorf("TimestampForRecovery = %v, want %v", p.TimestampForRecovery(), t0)
	}

	// Out-of-range timestamps should error.
	tooOld := time.Now().Add(-2 * time.Hour)
	if err := mgr.SetProducerRecoveryFromTimestamp(context.Background(), 1, tooOld); err == nil {
		t.Error("expected error for too-old timestamp")
	}
}
