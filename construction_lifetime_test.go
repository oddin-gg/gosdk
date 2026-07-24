package gosdk

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/types"
)

// TestNew_FailedConstruction_CancelsDetachedPreload pins the seventh-pass
// P2: when New fails (here: the caller's ctx expires while the preload
// catalog fetch is in flight), teardownPartialInit must CANCEL the
// detached load — pre-fix the singleflight detach (WithoutCancel +
// LoadTimeout) let the HTTP request run for up to 60s against a client
// that would never exist, retaining it and mutating its caches after
// teardown.
func TestNew_FailedConstruction_CancelsDetachedPreload(t *testing.T) {
	entered := make(chan struct{}, 8)
	cancelled := make(chan bool, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		switch {
		case strings.HasSuffix(r.URL.Path, "/users/whoami"):
			_, _ = w.Write([]byte(whoAmIBody))
		case strings.HasSuffix(r.URL.Path, "/sports"):
			entered <- struct{}{}
			select {
			case <-r.Context().Done():
				cancelled <- true
			case <-time.After(5 * time.Second):
				cancelled <- false
			}
			w.WriteHeader(http.StatusInternalServerError)
		default:
			_, _ = w.Write([]byte(`<?xml version="1.0"?><response response_code="OK"/>`))
		}
	}))
	defer srv.Close()

	cfg := NewConfig("t", types.IntegrationEnvironment,
		WithAPIHost("api.example.test"),
		WithHTTPClient(newTestHTTPClient(srv)),
		WithPreloadLocales(types.EnLocale),
	)
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	if _, err := New(ctx, cfg); err == nil {
		t.Fatal("New succeeded despite the gated preload — test setup broken")
	}

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("preload fetch never reached the server")
	}
	select {
	case ok := <-cancelled:
		if !ok {
			t.Fatal("failed construction did NOT cancel the in-flight preload request — the detached load outlived teardown")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the gated handler to observe cancellation")
	}
}

// TestNew_FailedConstruction_CancelsDetachedWhoAmI is the who-am-i twin:
// the probe's singleflight fetch detaches from the caller's ctx, so when
// New gives up waiting, teardown must cancel the fetch via the client's
// construction-lifetime ctx.
func TestNew_FailedConstruction_CancelsDetachedWhoAmI(t *testing.T) {
	entered := make(chan struct{}, 8)
	cancelled := make(chan bool, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if strings.HasSuffix(r.URL.Path, "/users/whoami") {
			entered <- struct{}{}
			select {
			case <-r.Context().Done():
				cancelled <- true
			case <-time.After(5 * time.Second):
				cancelled <- false
			}
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`<?xml version="1.0"?><response response_code="OK"/>`))
	}))
	defer srv.Close()

	cfg := NewConfig("t", types.IntegrationEnvironment,
		WithAPIHost("api.example.test"),
		WithHTTPClient(newTestHTTPClient(srv)),
	)
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	if _, err := New(ctx, cfg); err == nil {
		t.Fatal("New succeeded despite the gated who-am-i — test setup broken")
	}

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("who-am-i probe never reached the server")
	}
	select {
	case ok := <-cancelled:
		if !ok {
			t.Fatal("failed construction did NOT cancel the in-flight who-am-i request")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the gated handler to observe cancellation")
	}
}
