package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oddin-gg/gosdk/types"
)

// TestPathEscaping_CraftedURNCannotAlterRequest pins the M5 fix: every
// dynamic path segment is PathEscape'd, so a URN constructed directly
// (bypassing ParseURN's hardening) with URL-reserved characters cannot
// split the path or inject a query string. Ordinary URNs pass through
// byte-identical (':' is legal in a path segment).
func TestPathEscaping_CraftedURNCannotAlterRequest(t *testing.T) {
	var gotURI, gotRawQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURI = r.RequestURI
		gotRawQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<?xml version="1.0"?><match_summary generated_at="2026-01-01T00:00:00Z"><sport_event id="od:match:5" scheduled="2026-01-01T12:00:00Z"><tournament id="od:tournament:1"><sport id="od:sport:1"/></tournament></sport_event><sport_event_status status="not_started" match_status_code="0" scoreboard_available="false"/></match_summary>`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv)

	// Crafted URN: '?' would start a query, '/' + '..' would traverse.
	crafted := types.URN{Prefix: "od", Type: "ma?tch/../x", ID: 5}
	_, _ = c.FetchMatchSummary(t.Context(), crafted, types.EnLocale)

	if gotURI == "" {
		t.Fatal("request never reached the server")
	}
	if gotRawQuery != "" {
		t.Fatalf("crafted URN injected a query string: %q (uri %q)", gotRawQuery, gotURI)
	}
	if !strings.Contains(gotURI, "%3F") || !strings.Contains(gotURI, "%2F") {
		t.Fatalf("reserved characters were not escaped in the path: %q", gotURI)
	}
	if !strings.HasSuffix(gotURI, "/summary") {
		t.Fatalf("path was truncated or rerouted: %q", gotURI)
	}

	// Ordinary URN: unchanged on the wire.
	gotURI = ""
	ordinary := types.URN{Prefix: "od", Type: "match", ID: 5}
	if _, err := c.FetchMatchSummary(t.Context(), ordinary, types.EnLocale); err != nil {
		t.Fatalf("ordinary fetch: %v", err)
	}
	if want := "/v1/sports/en/sport_events/od:match:5/summary"; gotURI != want {
		t.Fatalf("ordinary URN path = %q, want %q", gotURI, want)
	}
}
