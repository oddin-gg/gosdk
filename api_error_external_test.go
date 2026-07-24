// Package gosdk_test exercises the SDK exactly the way a CONSUMER
// does — external package, public identifiers only. The typed-error
// classification below was impossible before gosdk.APIError existed:
// the concrete error type lived in internal/api, so applications had
// to parse error strings.
package gosdk_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk"
	"github.com/oddin-gg/gosdk/types"
)

// rewriteRT routes the SDK's https://<host>/v1/... requests at the
// plain-HTTP test server, the same way a consumer would wire a proxy.
type rewriteRT struct{ target string }

func (rt rewriteRT) RoundTrip(req *http.Request) (*http.Response, error) {
	t, _ := url.Parse(rt.target)
	req.URL.Scheme = t.Scheme
	req.URL.Host = t.Host
	return http.DefaultTransport.RoundTrip(req)
}

// TestConsumer_ClassifiesAPIErrorWithErrorsAs demonstrates (from an
// external package) that a failed HTTP call surfaced by a public
// method can be classified with errors.As on gosdk.APIError — HTTP
// status and envelope response code included.
func TestConsumer_ClassifiesAPIErrorWithErrorsAs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `<?xml version="1.0"?><error><message>no such bookmaker</message></error>`)
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	cfg := gosdk.NewConfig("external-test-token", types.IntegrationEnvironment,
		gosdk.WithAPIHost(u.Host),
		gosdk.WithHTTPClient(&http.Client{Transport: rewriteRT{target: srv.URL}, Timeout: 2 * time.Second}),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := gosdk.New(ctx, cfg) // synchronous who-am-i probe fails with 404
	if err == nil {
		t.Fatal("expected New to fail against a 404 API")
	}

	var apiErr *gosdk.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("errors.As(*gosdk.APIError) = false for %v", err)
	}
	if apiErr.Status != http.StatusNotFound {
		t.Fatalf("apiErr.Status = %d, want 404", apiErr.Status)
	}
	if apiErr.Method != http.MethodGet || apiErr.Path == "" {
		t.Fatalf("apiErr = %+v; want method GET and a request path", apiErr)
	}
}

// TestConsumer_ClassifiesCacheSentinelsWithErrorsIs proves (from an
// external package) that the "definitive absence" outcome on catalog
// lookups is classifiable with errors.Is on gosdk.ErrItemNotFound —
// Codex P2: internal wrap sites promised errors.Is classification, but
// the sentinel lived in internal/cache, which consumers cannot import,
// so external modules had to parse error strings to distinguish "API
// said no such item" from a retryable API/network failure.
func TestConsumer_ClassifiesCacheSentinelsWithErrorsIs(t *testing.T) {
	const whoAmI = `<?xml version="1.0"?><bookmaker_details response_code="OK" expire_at="2099-01-01T00:00:00+00:00" bookmaker_id="42" virtual_host="/vhost"/>`
	const sports = `<?xml version="1.0"?>
<sports generated_at="2026-01-01T00:00:00">
  <sport id="od:sport:1" name="Soccer" abbreviation="SOC"/>
</sports>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		switch {
		case strings.HasSuffix(r.URL.Path, "/users/whoami"):
			_, _ = io.WriteString(w, whoAmI)
		default: // sports catalog
			_, _ = io.WriteString(w, sports)
		}
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	cfg := gosdk.NewConfig("external-test-token", types.IntegrationEnvironment,
		gosdk.WithAPIHost(u.Host),
		gosdk.WithHTTPClient(&http.Client{Transport: rewriteRT{target: srv.URL}, Timeout: 2 * time.Second}),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := gosdk.New(ctx, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = c.Close(ctx)
	}()

	unknown, _ := types.ParseURN("od:sport:999")
	_, err = c.Sport(ctx, *unknown)
	if err == nil {
		t.Fatal("Sport(unknown) succeeded; want definitive-absence error")
	}
	if !errors.Is(err, gosdk.ErrItemNotFound) {
		t.Fatalf("errors.Is(err, gosdk.ErrItemNotFound) = false for %v", err)
	}
	// The other public cache sentinels must at least be nameable and
	// distinct from each other for errors.Is branching.
	if gosdk.ErrMarketLocaleIncomplete == nil || gosdk.ErrLocaleNotLoaded == nil {
		t.Fatal("cache sentinels are nil")
	}
	if errors.Is(err, gosdk.ErrMarketLocaleIncomplete) || errors.Is(err, gosdk.ErrLocaleNotLoaded) {
		t.Fatal("not-found error wrongly matches the locale sentinels")
	}
}

// TestConsumer_EntityNotFoundClassifiedAsErrItemNotFound proves the P2
// fix: the single-entity reads the ErrItemNotFound docs name (Match,
// Tournament, Player) resolve via a by-id API fetch whose absence is a
// 404 — pre-fix they propagated a bare APIError and errors.Is(err,
// ErrItemNotFound) silently failed. A definitive 404 now classifies as
// ErrItemNotFound while the APIError stays reachable via errors.As.
func TestConsumer_EntityNotFoundClassifiedAsErrItemNotFound(t *testing.T) {
	const whoAmI = `<?xml version="1.0"?><bookmaker_details response_code="OK" expire_at="2099-01-01T00:00:00+00:00" bookmaker_id="42" virtual_host="/vhost"/>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if strings.HasSuffix(r.URL.Path, "/users/whoami") {
			_, _ = io.WriteString(w, whoAmI)
			return
		}
		// Every entity read: definitive 404.
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `<?xml version="1.0"?><error><message>not found</message></error>`)
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	cfg := gosdk.NewConfig("external-test-token", types.IntegrationEnvironment,
		gosdk.WithAPIHost(u.Host),
		gosdk.WithHTTPClient(&http.Client{Transport: rewriteRT{target: srv.URL}, Timeout: 2 * time.Second}),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := gosdk.New(ctx, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = c.Close(ctx)
	}()

	matchID, _ := types.ParseURN("od:match:999")
	tournamentID, _ := types.ParseURN("od:tournament:999")
	playerID, _ := types.ParseURN("od:player:999")

	cases := map[string]func() error{
		"Match":      func() error { _, e := c.Match(ctx, *matchID); return e },
		"Tournament": func() error { _, e := c.Tournament(ctx, *tournamentID); return e },
		"Player":     func() error { _, e := c.Player(ctx, *playerID); return e },
	}
	for name, call := range cases {
		err := call()
		if err == nil {
			t.Fatalf("%s(unknown) succeeded; want definitive-absence error", name)
		}
		if !errors.Is(err, gosdk.ErrItemNotFound) {
			t.Fatalf("%s: errors.Is(err, gosdk.ErrItemNotFound) = false for %v", name, err)
		}
		var apiErr *gosdk.APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("%s: APIError no longer reachable via errors.As: %v", name, err)
		}
		if apiErr.Status != http.StatusNotFound {
			t.Fatalf("%s: apiErr.Status = %d, want 404", name, apiErr.Status)
		}
	}
}
