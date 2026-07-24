package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/oddin-gg/gosdk/internal/version"
)

// TestClient_SendsVersionHeaders pins the SDK self-identification on
// every API request: the idiomatic User-Agent plus the structured
// X-Oddin-SDK-Version, set at the shared request choke point so they
// ride all endpoints (here exercised via the producers fetch).
func TestClient_SendsVersionHeaders(t *testing.T) {
	var gotUA, gotVer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotVer = r.Header.Get("X-Oddin-SDK-Version")
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<?xml version="1.0"?><producers response_code="OK"><producer id="1" name="LO" description="" active="true" api_url="" producer_scopes="live"/></producers>`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	if _, err := c.FetchProducers(t.Context()); err != nil {
		t.Fatalf("FetchProducers: %v", err)
	}

	if gotUA != version.UserAgent() {
		t.Errorf("User-Agent = %q, want %q", gotUA, version.UserAgent())
	}
	if gotVer != version.Version() {
		t.Errorf("X-Oddin-SDK-Version = %q, want %q", gotVer, version.Version())
	}
}
