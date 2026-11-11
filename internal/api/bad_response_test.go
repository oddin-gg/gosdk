package api

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/oddin-gg/gosdk/internal/apipolicy"
	"github.com/oddin-gg/gosdk/types"
)

const policySportsXML = `<?xml version="1.0"?><sports generated_at="2026-01-01T00:00:00"><sport id="od:sport:1" name="Soccer" abbreviation="SOC"/></sports>`

// A 2xx whose body is cut off, malformed or followed by a second document is
// reported as ErrBadResponse; an answer such as 404, and a complete body, are
// not. Consumers tell a failure of the API call from a local error by it.
func TestClient_BadResponse_IsTaggedOnUndecodableBodies(t *testing.T) {
	var mode atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		switch mode.Load() {
		case "truncated":
			w.Header().Set("Content-Length", "4096")
			_, _ = io.WriteString(w, `<?xml version="1.0"?><sports generated_at="2026-01-01T00:00:00"><sport id="od:sp`)
		case "invalid":
			_, _ = io.WriteString(w, `<sports><sport></sports>`)
		case "trailing":
			_, _ = io.WriteString(w, policySportsXML+policySportsXML)
		case "missing":
			http.Error(w, "<error/>", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv)
	for _, tc := range []struct {
		mode string
		want bool
	}{{"truncated", true}, {"invalid", true}, {"trailing", true}, {"missing", false}} {
		mode.Store(tc.mode)
		ctx := apipolicy.With(t.Context(), apipolicy.Message)
		_, err := c.FetchSports(ctx, types.EnLocale)
		if err == nil {
			t.Fatalf("%s: no error", tc.mode)
		}
		if got := errors.Is(err, ErrBadResponse); got != tc.want {
			t.Fatalf("%s: errors.Is(ErrBadResponse) = %v, want %v (err: %v)", tc.mode, got, tc.want, err)
		}
	}
}
