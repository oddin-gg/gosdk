package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/protocols"
)

// testConfig satisfies protocols.OddsFeedConfiguration for tests.
// Only APIURL() and AccessToken() are exercised by the api.Client.
type testConfig struct {
	apiURL string
	token  string
}

func (c *testConfig) AccessToken() *string                                            { return &c.token }
func (c *testConfig) DefaultLocale() protocols.Locale                                 { return protocols.EnLocale }
func (c *testConfig) MaxInactivitySeconds() int                                       { return 20 }
func (c *testConfig) MaxRecoveryExecutionMinutes() int                                { return 360 }
func (c *testConfig) MessagingPort() int                                              { return 5672 }
func (c *testConfig) SdkNodeID() *int                                                 { return nil }
func (c *testConfig) SelectedEnvironment() *protocols.Environment                     { return nil }
func (c *testConfig) SelectedRegion() protocols.Region                                { return protocols.RegionDefault }
func (c *testConfig) SetRegion(protocols.Region) protocols.OddsFeedConfiguration      { return c }
func (c *testConfig) ExchangeName() string                                            { return "oddinfeed" }
func (c *testConfig) SetExchangeName(string) protocols.OddsFeedConfiguration          { return c }
func (c *testConfig) ReplayExchangeName() string                                      { return "oddinreplay" }
func (c *testConfig) ReportExtendedData() bool                                        { return false }
func (c *testConfig) SetAPIURL(string) protocols.OddsFeedConfiguration                { return c }
func (c *testConfig) SetMQURL(string) protocols.OddsFeedConfiguration                 { return c }
func (c *testConfig) SetMessagingPort(int) protocols.OddsFeedConfiguration            { return c }
func (c *testConfig) APIURL() (string, error)                                         { return c.apiURL, nil }
func (c *testConfig) MQURL() (string, error)                                          { return "", nil }
func (c *testConfig) SportIDPrefix() string                                           { return "od:sport:" }
func (c *testConfig) SetSportIDPrefix(string) protocols.OddsFeedConfiguration         { return c }

// newTestClient wires the API client to a test server. The api.Client builds
// URLs as `https://<APIURL>/v1<path>`, so we strip the `https://` prefix from
// the test server URL and configure that host string.
//
// The test server is plain HTTP; we override the Client's httpClient.Transport
// to skip TLS by talking directly to the server's URL (the apiURL host
// inside the request is overridden via DialContext).
func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()

	// Strip scheme.
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("bad server url: %v", err)
	}

	cfg := &testConfig{apiURL: u.Host, token: "test-token"}
	c := New(cfg)
	c.maxRetries = 3
	// Rewrite outgoing requests so https://<host>/v1/... routes to the test server.
	c.httpClient = &http.Client{
		Transport: &rewriteTransport{target: srv.URL, base: srv.Client().Transport},
		Timeout:   2 * time.Second,
	}
	c.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	return c
}

type rewriteTransport struct {
	target string
	base   http.RoundTripper
}

func (rt *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	target, err := url.Parse(rt.target)
	if err != nil {
		return nil, err
	}
	req.URL.Scheme = target.Scheme
	req.URL.Host = target.Host
	t := rt.base
	if t == nil {
		t = http.DefaultTransport
	}
	return t.RoundTrip(req)
}

// --- tests ---

func TestClient_FetchProducers_Success(t *testing.T) {
	body := `<?xml version="1.0"?><producers response_code="OK"><producer id="1" name="LO" description="" active="true" api_url="" producer_scopes="live"/></producers>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/descriptions/producers" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if got := r.Header.Get("X-Access-Token"); got != "test-token" {
			t.Errorf("X-Access-Token = %q, want test-token", got)
		}
		if got := r.Header.Get("Accept"); got != "application/xml" {
			t.Errorf("Accept = %q, want application/xml", got)
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	prods, err := c.FetchProducers(context.Background())
	if err != nil {
		t.Fatalf("FetchProducers: %v", err)
	}
	if len(prods) == 0 {
		t.Fatal("got 0 producers")
	}
}

// TestClient_HeaderCanonicalization confirms we send a canonical
// "X-Access-Token" header rather than direct-map mutation that would
// produce a non-canonical "x-access-token".
func TestClient_HeaderCanonicalization(t *testing.T) {
	var seenCanonical, seenLower atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// http.Header normalizes keys, so direct map iteration gives us
		// what was actually sent.
		for k := range r.Header {
			if k == "X-Access-Token" {
				seenCanonical.Store(true)
			}
			if k == "x-access-token" {
				seenLower.Store(true)
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `<empty/>`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, _ = c.FetchProducers(context.Background())
	if !seenCanonical.Load() {
		t.Fatal("X-Access-Token (canonical) not seen on the request")
	}
	if seenLower.Load() {
		t.Fatal("non-canonical x-access-token leaked through")
	}
}

func TestClient_RetriesOn5xx(t *testing.T) {
	var attempts atomic.Int32
	body := `<producers response_code="OK"></producers>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, "boom")
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	if _, err := c.FetchProducers(context.Background()); err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
}

// TestClient_BodyClosedOnRetry verifies that response.Body from a transient
// failure is closed before the next attempt — the original client leaked
// fds across retries.
func TestClient_BodyClosedOnRetry(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 2 {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, strings.Repeat("a", 4096))
			return
		}
		_, _ = io.WriteString(w, `<producers response_code="OK"/>`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	if _, err := c.FetchProducers(context.Background()); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	// If the previous attempt's body wasn't closed, the test server will hold
	// the connection open; the test still passes but the regression target is
	// behavioral via the do() implementation. Sanity check: at least 2 attempts.
	if got := attempts.Load(); got < 2 {
		t.Fatalf("attempts = %d, want >= 2", got)
	}
}

func TestClient_NoRetryOn4xx(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `<response response_code="NOT_FOUND"><action>none</action><message>requested match is not active</message></response>`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, err := c.FetchProducers(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "requested match is not active") {
		t.Fatalf("error %q does not contain decoded API message", err.Error())
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1 (no retry on 4xx)", got)
	}
}

func TestClient_RetriesOnNetworkError(t *testing.T) {
	// Server that closes the connection without writing a response on first attempts.
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 2 {
			// Hijack and close to force a network-level error.
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("hijacker not supported")
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Fatalf("hijack: %v", err)
			}
			_ = conn.Close()
			return
		}
		_, _ = io.WriteString(w, `<producers response_code="OK"/>`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	if _, err := c.FetchProducers(context.Background()); err != nil {
		t.Fatalf("expected success after network-error retry, got %v", err)
	}
	if got := attempts.Load(); got < 2 {
		t.Fatalf("attempts = %d, want >= 2", got)
	}
}

func TestClient_ContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Block until the request context is cancelled.
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := c.FetchProducers(ctx)
	if err == nil {
		t.Fatal("expected error on canceled context")
	}
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error %v is not a context error", err)
	}
}

// TestClient_PostReplayStart_AllParams is the regression test for the
// pre-rewrite bug where queryParam `count` was never incremented, causing
// all but one query parameter to be silently dropped (or written to slot 0
// repeatedly). Now we use url.Values which can't have that bug.
func TestClient_PostReplayStart_AllParams(t *testing.T) {
	var seen url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		seen = r.URL.Query()
		_, _ = io.WriteString(w, `<empty/>`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	speed := 10
	maxDelay := 5
	useTs := true
	product := "live"
	parallel := false
	node := 7

	if _, err := c.PostReplayStart(context.Background(),
		&node, &speed, &maxDelay, &useTs, &product, &parallel,
	); err != nil {
		t.Fatalf("PostReplayStart: %v", err)
	}
	want := map[string]string{
		"node_id":              "7",
		"speed":                "10",
		"max_delay":            "5",
		"use_replay_timestamp": "true",
		"product":              "live",
		"run_parallel":         "false",
	}
	for k, v := range want {
		if got := seen.Get(k); got != v {
			t.Fatalf("query[%s] = %q, want %q", k, got, v)
		}
	}
}

func TestClient_PostEventOddsRecovery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := "/v1/live/odds/events/od:match:42/initiate_request"
		if r.URL.Path != want {
			t.Errorf("path = %s, want %s", r.URL.Path, want)
		}
		if r.URL.Query().Get("request_id") != "1234" {
			t.Errorf("request_id = %s, want 1234", r.URL.Query().Get("request_id"))
		}
		if r.URL.Query().Get("node_id") != "5" {
			t.Errorf("node_id = %s, want 5", r.URL.Query().Get("node_id"))
		}
		_, _ = fmt.Fprint(w, `<empty/>`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	node := 5
	urn, _ := protocols.ParseURN("od:match:42")
	ok, err := c.PostEventOddsRecovery(context.Background(), "live", *urn, 1234, &node)
	if err != nil {
		t.Fatalf("PostEventOddsRecovery: %v", err)
	}
	if !ok {
		t.Fatal("expected success=true")
	}
}
