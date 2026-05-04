package gosdk

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/internal/feed"
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

// TestClient_ConnectionEvents_TranslatesFeedEvents drives the feed-layer
// event-translation hook directly and verifies every kind reaches
// ConnectionEvents() with the correct translated kind + err.
func TestClient_ConnectionEvents_TranslatesFeedEvents(t *testing.T) {
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

	bang := errors.New("broker drop")
	c.onFeedEvent(feed.Event{Kind: feed.EventConnected})
	c.onFeedEvent(feed.Event{Kind: feed.EventDisconnected, Err: bang})
	c.onFeedEvent(feed.Event{Kind: feed.EventReconnecting})

	want := []ConnectionEventKind{ConnectionConnected, ConnectionDisconnected, ConnectionReconnecting}
	for i, w := range want {
		select {
		case ev := <-c.ConnectionEvents():
			if ev.Kind != w {
				t.Errorf("event[%d].Kind = %v, want %v", i, ev.Kind, w)
			}
			if w == ConnectionDisconnected && ev.Err == nil {
				t.Errorf("event[%d].Err = nil, want non-nil", i)
			}
		case <-time.After(200 * time.Millisecond):
			t.Fatalf("no event %d", i)
		}
	}
}

// fullFixtureServer is a comprehensive httptest server that handles
// every Oddin REST endpoint a Client.* method might hit. Used to
// exercise the delegation methods without each test wiring its own
// fixtures.
func fullFixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	const producersBody = `<?xml version="1.0"?>
<producers response_code="OK">
  <producer id="1" name="live" description="Live" active="true" api_url="https://x" scope="live" stateful_recovery_window_in_minutes="60"/>
  <producer id="2" name="pre" description="Pre" active="true" api_url="https://x" scope="prematch" stateful_recovery_window_in_minutes="60"/>
</producers>`

	const sportsBody = `<?xml version="1.0"?>
<sports generated_at="2026-01-01T00:00:00">
  <sport id="od:sport:1" name="Soccer" abbreviation="SOC"/>
</sports>`

	const sportTournamentsBody = `<?xml version="1.0"?>
<sport_tournaments generated_at="2026-01-01T00:00:00">
  <sport id="od:sport:1" name="Soccer" abbreviation="SOC"/>
  <tournaments>
    <tournament id="od:tournament:1" name="Premier League" abbreviation="PL" risk_tier="1">
      <sport id="od:sport:1" name="Soccer" abbreviation="SOC"/>
    </tournament>
  </tournaments>
</sport_tournaments>`

	const tournamentInfoBody = `<?xml version="1.0"?>
<tournament_info generated_at="2026-01-01T00:00:00">
  <tournament id="od:tournament:1" name="Premier League" abbreviation="PL" risk_tier="1">
    <sport id="od:sport:1" name="Soccer" abbreviation="SOC"/>
  </tournament>
</tournament_info>`

	const fixtureChangesBody = `<?xml version="1.0"?>
<fixture_changes generated_at="2026-01-01T00:00:00">
  <fixture_change sport_event_id="od:match:1" update_time="2026-01-01T10:00:00"/>
</fixture_changes>`

	const marketsBody = `<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="1" name="1x2" groups="all">
    <outcomes>
      <outcome id="1" name="home"/>
      <outcome id="2" name="away"/>
    </outcomes>
  </market>
</market_descriptions>`

	const voidReasonsBody = `<?xml version="1.0"?>
<void_reasons response_code="OK">
  <void_reason id="1" name="canceled" description="Canceled" template="Canceled"/>
</void_reasons>`

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/users/whoami"):
			_, _ = io.WriteString(w, whoAmIBody)
		case strings.HasSuffix(path, "/descriptions/producers"):
			_, _ = io.WriteString(w, producersBody)
		case strings.HasSuffix(path, "/sports"):
			_, _ = io.WriteString(w, sportsBody)
		case strings.HasSuffix(path, "/tournaments") && strings.Contains(path, "/sports/od:sport:"):
			_, _ = io.WriteString(w, sportTournamentsBody)
		case strings.HasSuffix(path, "/info") && strings.Contains(path, "/tournaments/"):
			_, _ = io.WriteString(w, tournamentInfoBody)
		case strings.HasSuffix(path, "/fixtures/changes"):
			_, _ = io.WriteString(w, fixtureChangesBody)
		case strings.HasSuffix(path, "/markets"):
			_, _ = io.WriteString(w, marketsBody)
		case strings.HasSuffix(path, "/void_reasons"):
			_, _ = io.WriteString(w, voidReasonsBody)
		default:
			t.Logf("unhandled path: %s", path)
			http.NotFound(w, r)
		}
	}))
}

// newTestClient is a helper that builds a Client wired to a full
// fixture server with a 2s timeout.
func newTestClient(t *testing.T) *Client {
	t.Helper()
	srv := fullFixtureServer(t)
	t.Cleanup(srv.Close)

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
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = c.Close(ctx)
	})
	return c
}

// TestClient_Producers_Methods exercises the four producer-listing
// delegations.
func TestClient_Producers_Methods(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()

	all, err := c.Producers(ctx)
	if err != nil {
		t.Fatalf("Producers: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("Producers count = %d, want 2", len(all))
	}

	active, err := c.ActiveProducers(ctx)
	if err != nil {
		t.Fatalf("ActiveProducers: %v", err)
	}
	if len(active) != 2 {
		t.Errorf("ActiveProducers count = %d", len(active))
	}

	live, err := c.ProducersInScope(ctx, protocols.LiveProducerScope)
	if err != nil {
		t.Fatalf("ProducersInScope: %v", err)
	}
	if len(live) != 1 {
		t.Errorf("live count = %d", len(live))
	}

	p, err := c.Producer(ctx, 1)
	if err != nil {
		t.Fatalf("Producer: %v", err)
	}
	if p.ID() != 1 {
		t.Errorf("Producer.ID = %d", p.ID())
	}
}

// TestClient_SetProducerState_AndRecoveryTimestamp exercises producer
// state mutators.
func TestClient_SetProducerState_AndRecoveryTimestamp(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()

	if err := c.SetProducerEnabled(ctx, 1, false); err != nil {
		t.Fatalf("SetProducerEnabled: %v", err)
	}
	when := time.Now().Add(-30 * time.Minute)
	if err := c.SetProducerRecoveryFromTimestamp(ctx, 1, when); err != nil {
		t.Fatalf("SetProducerRecoveryFromTimestamp: %v", err)
	}
}

// TestClient_Sports_Methods covers Sports + ActiveTournaments +
// AvailableTournaments.
func TestClient_Sports_Methods(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()

	if _, err := c.Sports(ctx); err != nil {
		t.Errorf("Sports: %v", err)
	}
	if _, err := c.ActiveTournaments(ctx); err != nil {
		t.Errorf("ActiveTournaments: %v", err)
	}
	urn, _ := protocols.ParseURN("od:sport:1")
	if _, err := c.AvailableTournaments(ctx, *urn); err != nil {
		t.Errorf("AvailableTournaments: %v", err)
	}
}

// TestClient_FixtureChanges exercises the fixture-changes path.
func TestClient_FixtureChanges(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()
	got, err := c.FixtureChanges(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("FixtureChanges: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("got %d", len(got))
	}
}

// TestClient_MarketDescriptions covers the market description / void
// reason delegation paths.
func TestClient_MarketDescriptions(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()

	descs, err := c.MarketDescriptions(ctx)
	if err != nil {
		t.Fatalf("MarketDescriptions: %v", err)
	}
	if len(descs) == 0 {
		t.Error("expected at least one description")
	}

	desc, err := c.MarketDescription(ctx, 1, nil)
	if err != nil {
		t.Fatalf("MarketDescription: %v", err)
	}
	if desc.ID != 1 {
		t.Errorf("MarketDescription.ID = %d", desc.ID)
	}

	reasons, err := c.MarketVoidReasons(ctx)
	if err != nil {
		t.Fatalf("MarketVoidReasons: %v", err)
	}
	if len(reasons) == 0 {
		t.Error("expected at least one void reason")
	}
	if _, err := c.ReloadMarketVoidReasons(ctx); err != nil {
		t.Errorf("ReloadMarketVoidReasons: %v", err)
	}
}

// TestClient_ClearMethods just verifies they don't panic.
func TestClient_ClearMethods(t *testing.T) {
	c := newTestClient(t)
	urn, _ := protocols.ParseURN("od:match:1")
	c.ClearMatch(*urn)
	c.ClearTournament(*urn)
	c.ClearCompetitor(*urn)
	c.ClearMarketDescription(1, nil)
}

// TestClient_Replay_Subtype exercises Replay() returning the subtype
// and that its methods are reachable. The sport-info dependency makes
// most replay endpoints fail without more fixture wiring; we just
// verify the subtype is non-nil.
func TestClient_Replay_Subtype(t *testing.T) {
	c := newTestClient(t)
	if c.Replay() == nil {
		t.Error("Replay() returned nil")
	}
}

// TestClient_EventRecoveryStatus_UnknownID verifies that querying an
// id that was never registered returns false.
func TestClient_EventRecoveryStatus_UnknownID(t *testing.T) {
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

	if _, ok := c.EventRecoveryStatus(99999); ok {
		t.Error("EventRecoveryStatus(99999) should be false for unknown id")
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
