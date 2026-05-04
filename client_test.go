package gosdk

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

	"github.com/oddin-gg/gosdk/protocols"
)

// rewriteTransport reroutes outbound requests to a test server's host
// while preserving the path. Mirrors internal/api's test harness.
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

// newTestHTTPClient returns an http.Client whose RoundTripper rewrites
// every request's host to point at the supplied test server.
func newTestHTTPClient(srv *httptest.Server) *http.Client {
	return &http.Client{
		Timeout: 2 * time.Second,
		Transport: &rewriteTransport{
			target: srv.URL,
			base: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		},
	}
}

const whoAmIBody = `<?xml version="1.0"?><bookmaker_details response_code="OK" expire_at="2099-01-01T00:00:00+00:00" bookmaker_id="42" virtual_host="/vhost"/>`

// TestClient_New_EagerWhoAmI confirms New performs the bookmaker probe
// up-front and surfaces the resulting BookmakerDetails through the
// public accessor.
func TestClient_New_EagerWhoAmI(t *testing.T) {
	var sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("X-Access-Token")
		if !strings.HasSuffix(r.URL.Path, "/users/whoami") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, whoAmIBody)
	}))
	defer srv.Close()

	cfg := NewConfig("test-token", protocols.IntegrationEnvironment,
		WithAPIURL("api.example.test"),
		WithHTTPClient(newTestHTTPClient(srv)),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	c, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = c.Close(closeCtx)
	}()

	if sawAuth != "test-token" {
		t.Errorf("X-Access-Token = %q, want test-token", sawAuth)
	}

	// Sanity: state, accessors, public types wired.
	if c.ConnectionState() != ConnectionStateNotConnected {
		t.Errorf("state = %v, want NotConnected", c.ConnectionState())
	}
	if c.Replay() == nil {
		t.Error("Replay() returned nil")
	}
	if c.ConnectionEvents() == nil || c.RecoveryEvents() == nil || c.APIEvents() == nil {
		t.Error("event channels not wired")
	}

	// Re-issue the BookmakerDetails call; this exercises the public method
	// (it will hit the cache/whoami manager rather than re-probing — but
	// the wiring is what we care about).
	bd, err := c.BookmakerDetails(ctx)
	if err != nil {
		t.Fatalf("BookmakerDetails: %v", err)
	}
	if bd == nil || bd.BookmakerID() != 42 {
		t.Errorf("BookmakerDetails = %v", bd)
	}
}

// TestClient_New_BookmakerProbeError surfaces auth errors immediately.
func TestClient_New_BookmakerProbeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w,
			`<?xml version="1.0"?><response response_code="FORBIDDEN"><action>auth</action><message>bad token</message></response>`)
	}))
	defer srv.Close()

	cfg := NewConfig("bad-token", protocols.IntegrationEnvironment,
		WithAPIURL("api.example.test"),
		WithHTTPClient(newTestHTTPClient(srv)),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if _, err := New(ctx, cfg); err == nil {
		t.Fatal("New should fail when whoami returns 401")
	}
}

// TestClient_APIEvents_OffByDefault verifies WithAPICallLogging defaults
// to APILogOff — no events should arrive on APIEvents() despite the
// whoami probe firing during New.
func TestClient_APIEvents_OffByDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, whoAmIBody)
	}))
	defer srv.Close()

	cfg := NewConfig("t", protocols.IntegrationEnvironment,
		WithAPIURL("api.example.test"),
		WithHTTPClient(newTestHTTPClient(srv)),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	c, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(ctx) })

	select {
	case ev := <-c.APIEvents():
		t.Fatalf("unexpected APIEvent at default level: %+v", ev)
	case <-time.After(20 * time.Millisecond):
		// expected
	}
}

// TestClient_APIEvents_MetadataLevel exercises the redaction + dispatch
// path: with APILogMetadata the event fires (URL stripped of query) but
// no Response body is captured.
func TestClient_APIEvents_MetadataLevel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, whoAmIBody)
	}))
	defer srv.Close()

	cfg := NewConfig("t", protocols.IntegrationEnvironment,
		WithAPIURL("api.example.test"),
		WithHTTPClient(newTestHTTPClient(srv)),
		WithAPICallLogging(APILogMetadata),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	c, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(ctx) })

	select {
	case ev := <-c.APIEvents():
		if ev.Method != "GET" {
			t.Errorf("Method = %q, want GET", ev.Method)
		}
		if !strings.Contains(ev.URL, "/users/whoami") {
			t.Errorf("URL = %q, missing path", ev.URL)
		}
		if strings.Contains(ev.URL, "?") {
			t.Errorf("URL = %q has query string (should be redacted)", ev.URL)
		}
		if ev.Status != 200 {
			t.Errorf("Status = %d, want 200", ev.Status)
		}
		if len(ev.Response) != 0 {
			t.Errorf("Response should be empty at APILogMetadata, got %d bytes", len(ev.Response))
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("no APIEvent fired")
	}
}

// TestClient_APIEvents_ResponsesLevel captures the response body bytes
// and the redaction path.
func TestClient_APIEvents_ResponsesLevel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, whoAmIBody)
	}))
	defer srv.Close()

	cfg := NewConfig("t", protocols.IntegrationEnvironment,
		WithAPIURL("api.example.test"),
		WithHTTPClient(newTestHTTPClient(srv)),
		WithAPICallLogging(APILogResponses),
		WithAPICallBodyLimit(1024),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	c, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(ctx) })

	select {
	case ev := <-c.APIEvents():
		if !strings.Contains(string(ev.Response), "bookmaker_details") {
			t.Errorf("Response missing payload: %q", string(ev.Response))
		}
		if ev.Truncated {
			t.Errorf("Truncated=true with 1KiB limit and short body")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("no APIEvent fired at APILogResponses")
	}
}

// TestClient_APIEvents_BodyTruncation verifies the body-limit clamp.
func TestClient_APIEvents_BodyTruncation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, whoAmIBody)
	}))
	defer srv.Close()

	cfg := NewConfig("t", protocols.IntegrationEnvironment,
		WithAPIURL("api.example.test"),
		WithHTTPClient(newTestHTTPClient(srv)),
		WithAPICallLogging(APILogResponses),
		WithAPICallBodyLimit(16), // shorter than whoAmIBody
	)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	c, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(ctx) })

	select {
	case ev := <-c.APIEvents():
		if !ev.Truncated {
			t.Errorf("Truncated should be true with 16-byte limit")
		}
		if len(ev.Response) > 16 {
			t.Errorf("Response = %d bytes, want <=16", len(ev.Response))
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("no APIEvent fired")
	}
}

// TestClient_Close_Idempotent verifies Close can be called repeatedly.
func TestClient_Close_Idempotent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, whoAmIBody)
	}))
	defer srv.Close()

	cfg := NewConfig("t", protocols.IntegrationEnvironment,
		WithAPIURL("api.example.test"),
		WithHTTPClient(newTestHTTPClient(srv)),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	c, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := c.Close(ctx); err != nil {
		t.Fatalf("Close 1: %v", err)
	}
	if err := c.Close(ctx); err != nil {
		t.Fatalf("Close 2: %v", err)
	}
	if c.ConnectionState() != ConnectionStateClosed {
		t.Errorf("state = %v, want Closed", c.ConnectionState())
	}
}
