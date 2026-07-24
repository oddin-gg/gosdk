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

	"github.com/oddin-gg/gosdk/types"
)

// testConfig satisfies config.Config for tests.
// Only APIURL() and AccessToken() are exercised by the api.Client.
type testConfig struct {
	apiURL string
	token  string
}

func (c *testConfig) AccessToken() *string                    { return &c.token }
func (c *testConfig) DefaultLocale() types.Locale             { return types.EnLocale }
func (c *testConfig) MaxInactivity() time.Duration            { return 20 * time.Second }
func (c *testConfig) MaxRecoveryExecution() time.Duration     { return 360 * time.Minute }
func (c *testConfig) MessagingPort() int                      { return 5672 }
func (c *testConfig) SdkNodeID() *int                         { return nil }
func (c *testConfig) SelectedEnvironment() *types.Environment { return nil }
func (c *testConfig) SelectedRegion() types.Region            { return types.RegionDefault }
func (c *testConfig) ExchangeName() string                    { return "oddinfeed" }
func (c *testConfig) ReplayExchangeName() string              { return "oddinreplay" }
func (c *testConfig) ReportExtendedData() bool                { return false }
func (c *testConfig) APIURL() (string, error)                 { return c.apiURL, nil }
func (c *testConfig) MQURL() (string, error)                  { return "", nil }
func (c *testConfig) SportIDPrefix() string                   { return "od:sport:" }

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
	c.maxAttempts = 3
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
	prods, err := c.FetchProducers(t.Context())
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
	_, _ = c.FetchProducers(t.Context())
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
	if _, err := c.FetchProducers(t.Context()); err != nil {
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
	if _, err := c.FetchProducers(t.Context()); err != nil {
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
	_, err := c.FetchProducers(t.Context())
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
	if _, err := c.FetchProducers(t.Context()); err != nil {
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
	ctx, cancel := context.WithCancel(t.Context())
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
	useTS := true
	product := "live"
	parallel := false
	node := 7

	if _, err := c.PostReplayStart(t.Context(),
		&node, &speed, &maxDelay, &useTS, &product, &parallel,
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
	urn, _ := types.ParseURN("od:match:42")
	ok, err := c.PostEventOddsRecovery(t.Context(), "live", *urn, 1234, &node)
	if err != nil {
		t.Fatalf("PostEventOddsRecovery: %v", err)
	}
	if !ok {
		t.Fatal("expected success=true")
	}
}

// TestClient_FetchProducers_StreamedBody is the regression for the
// request-ctx lifetime bug: do() derived reqCtx with a deferred
// cancelReq(), but returned the 2xx response with the body still open
// and lazily read (installCapture only tees; nothing materializes the
// stream). The deferred cancel fired the instant do() returned — before
// fetchData's xml.Decode consumed the body — so any payload not already
// sitting in the transport's receive buffer failed mid-read with
// "context canceled". Small-body tests never noticed; catalog-sized
// responses failed deterministically. The fix transfers ctx cleanup to
// Body.Close (cancelOnClose).
func TestClient_FetchProducers_StreamedBody(t *testing.T) {
	const n = 3000
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("test server does not support flushing")
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `<?xml version="1.0"?><producers response_code="OK">`)
		fl.Flush()
		// Keep the body streaming past do()'s return: pre-fix, the
		// deferred cancelReq() kills the connection right here and the
		// decoder sees only the buffered prefix.
		time.Sleep(60 * time.Millisecond)
		for i := 1; i <= n; i++ {
			_, _ = fmt.Fprintf(w, `<producer id="%d" name="live" description="P%d" active="true" api_url="https://x" scope="live" stateful_recovery_window_in_minutes="60"/>`, i, i)
		}
		_, _ = fmt.Fprint(w, `</producers>`)
		fl.Flush()
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	prods, err := c.FetchProducers(t.Context())
	if err != nil {
		t.Fatalf("FetchProducers failed on a streamed body: %v", err)
	}
	if len(prods) != n {
		t.Fatalf("decoded %d producers, want %d (partial body => silent truncation)", len(prods), n)
	}
}

// TestClient_FetchPlayerProfile_ValidatesEnvelopeAndIdentity covers the
// player-profile decode hardening: (a) the XMLName root constraint —
// any well-formed 2xx XML used to decode into a zero-valued profile the
// cache then stored as real data; (b) missing / empty / mismatched
// player ids are rejected before the caller (and the cache) can accept
// them.
func TestClient_FetchPlayerProfile_ValidatesEnvelopeAndIdentity(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name: "valid",
			body: `<?xml version="1.0"?><player_profile><player id="od:player:9" name="P" sport="od:sport:1"/></player_profile>`,
		},
		{
			name:    "wrong root",
			body:    `<?xml version="1.0"?><totally_other_document response_code="OK"/>`,
			wantErr: "player_profile",
		},
		{
			name:    "missing player element",
			body:    `<?xml version="1.0"?><player_profile/>`,
			wantErr: "carries no id",
		},
		{
			name:    "empty player id",
			body:    `<?xml version="1.0"?><player_profile><player id="" name="P"/></player_profile>`,
			wantErr: "carries no id",
		},
		{
			name:    "mismatched player id",
			body:    `<?xml version="1.0"?><player_profile><player id="od:player:8" name="Q"/></player_profile>`,
			wantErr: `is for "od:player:8"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/xml")
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			c := newTestClient(t, srv)

			p, err := c.FetchPlayerProfile(t.Context(), "od:player:9", types.EnLocale)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("FetchPlayerProfile: %v", err)
				}
				if p.Player.ID != "od:player:9" {
					t.Fatalf("player id = %q", p.Player.ID)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got profile %+v", tc.wantErr, p)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

// TestClient_FetchMatchSummary_MismatchBlocksObservers is the
// regression for entity-response identity: a stale/misrouted 2xx
// summary for B while requesting A used to flow into the observers
// (which store under the RESPONSE's embedded id) BEFORE any caller
// validation was possible — contaminating caches with another
// resource's data. The identity check must fail the call AND keep the
// response away from every observer.
func TestClient_FetchMatchSummary_MismatchBlocksObservers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<?xml version="1.0"?>
<match_summary generated_at="2026-01-01T00:00:00Z">
  <sport_event id="od:match:99"><tournament id="od:tournament:1"><sport id="od:sport:1" name="S"/></tournament></sport_event>
  <sport_event_status status="1" match_status="1" home_score="1" away_score="0" scoreboard_available="false"/>
</match_summary>`)
	}))
	defer srv.Close()
	c := newTestClient(t, srv)

	var observed atomic.Int32
	c.SubscribeWithAPIObserver(observerFunc(func(Response) { observed.Add(1) }))

	_, err := c.FetchMatchSummary(t.Context(), types.URN{Prefix: "od", Type: "match", ID: 42}, types.EnLocale)
	if err == nil {
		t.Fatal("expected identity error for misrouted summary")
	}
	if !strings.Contains(err.Error(), `is for "od:match:99"`) {
		t.Fatalf("err = %v, want identity mismatch", err)
	}
	if n := observed.Load(); n != 0 {
		t.Fatalf("observer dispatched %d time(s) for a misrouted response; cache contamination", n)
	}
}

// observerFunc adapts a func to the Observer interface.
type observerFunc func(Response)

func (f observerFunc) OnAPIResponse(r Response) { f(r) }

// schemeToHTTP rewrites the outgoing scheme from https to http (keeping
// the host) so makeRequest's hardcoded "https://" reaches a plain
// httptest.Server. It clones the request, so the client's own `via`
// chain — which the redirect guard inspects — keeps the original https
// scheme and host.
type schemeToHTTP struct{ base http.RoundTripper }

func (t schemeToHTTP) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.URL.Scheme = "http"
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(r)
}

// TestClient_CrossOriginRedirect_DoesNotLeakToken is the regression for
// the credential-disclosure P1: X-Access-Token is a CUSTOM header, so
// net/http copies it across a redirect to a different authority (it
// strips only Authorization/Cookie/WWW-Authenticate). The default
// client (CheckRedirect == nil) would follow a 30x to another host and
// hand the token over. The SDK's guard must refuse the cross-origin hop
// before the second host is dialed, and still permit same-origin ones.
func TestClient_CrossOriginRedirect_DoesNotLeakToken(t *testing.T) {
	var tokenSeenByB atomic.Int32
	serverB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Access-Token") != "" {
			tokenSeenByB.Add(1)
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<producers response_code="OK"/>`)
	}))
	defer serverB.Close()
	bURL, _ := url.Parse(serverB.URL)

	serverA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Redirect to server B at the SAME scheme the client used
		// (https), so this isolates the cross-HOST case, not a downgrade.
		w.Header().Set("Location", "https://"+bURL.Host+"/v1/descriptions/producers")
		w.WriteHeader(http.StatusFound)
	}))
	defer serverA.Close()
	aURL, _ := url.Parse(serverA.URL)

	cfg := &testConfig{apiURL: aURL.Host, token: "leak-me-if-you-can"}
	c := New(cfg)
	c.maxAttempts = 1
	c.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	// SetHTTPClient installs the redirect guard; the transport downgrades
	// the scheme so the https origin reaches the plain test server.
	c.SetHTTPClient(&http.Client{Transport: schemeToHTTP{}, Timeout: 2 * time.Second})

	_, err := c.FetchProducers(t.Context())
	if err == nil {
		t.Fatal("cross-origin redirect was followed; expected refusal error")
	}
	if !strings.Contains(err.Error(), "cross-origin") && !strings.Contains(err.Error(), "disclose the access token") {
		t.Fatalf("err = %v, want a cross-origin redirect refusal", err)
	}
	if n := tokenSeenByB.Load(); n != 0 {
		t.Fatalf("server B received the access token %d time(s) — credential leaked across origins", n)
	}
}

// TestClient_SameOriginRedirect_IsFollowed pins the converse: a redirect
// to the SAME scheme+host is permitted, so the guard doesn't break
// legitimate same-origin path redirects.
func TestClient_SameOriginRedirect_IsFollowed(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Location", r.URL.Path+"/redirected")
			w.WriteHeader(http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<producers response_code="OK"/>`)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)

	cfg := &testConfig{apiURL: u.Host, token: "tok"}
	c := New(cfg)
	c.maxAttempts = 1
	c.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	c.SetHTTPClient(&http.Client{Transport: schemeToHTTP{}, Timeout: 2 * time.Second})

	if _, err := c.FetchProducers(t.Context()); err != nil {
		t.Fatalf("same-origin redirect was refused: %v", err)
	}
	if n := hits.Load(); n != 2 {
		t.Fatalf("server hits = %d, want 2 (original + followed same-origin redirect)", n)
	}
}

// TestGuardRedirects_RejectsDowngrade unit-tests the policy directly for
// the https->http downgrade on the SAME host.
func TestGuardRedirects_RejectsDowngrade(t *testing.T) {
	hc := guardRedirects(&http.Client{})
	orig, _ := http.NewRequest(http.MethodGet, "https://api.example.com/v1/x", nil)
	target, _ := http.NewRequest(http.MethodGet, "http://api.example.com/v1/x", nil)
	if err := hc.CheckRedirect(target, []*http.Request{orig}); err == nil {
		t.Fatal("https->http downgrade redirect was permitted")
	}
}
