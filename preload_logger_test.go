package gosdk

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/types"
)

// recordingHandler captures slog records for assertion.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.records = append(h.records, r.Clone())
	h.mu.Unlock()
	return nil
}
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }
func (h *recordingHandler) messages() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.records))
	for i, r := range h.records {
		out[i] = r.Message
	}
	return out
}

// TestNew_PreloadLocales_WarmsCatalogsEagerly pins the fifth-pass P2:
// WithPreloadLocales promises the sports and market-description
// catalogs are fetched EAGERLY during New for every listed locale —
// previously the locales were only threaded into constructors and the
// first fetch happened on the message hot path.
func TestNew_PreloadLocales_WarmsCatalogsEagerly(t *testing.T) {
	var sportsHits, marketsHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/users/whoami"):
			_, _ = w.Write([]byte(whoAmIBody))
		case strings.HasSuffix(path, "/descriptions/producers"):
			_, _ = w.Write([]byte(`<?xml version="1.0"?><producers response_code="OK"><producer id="1" name="live" description="d" active="true" api_url="u" scope="live" stateful_recovery_window_in_minutes="60"/></producers>`))
		case strings.Contains(path, "/markets"):
			marketsHits.Add(1)
			_, _ = w.Write([]byte(`<?xml version="1.0"?><market_descriptions response_code="OK"><market id="1" name="One"><outcomes><outcome id="1" name="o1"/></outcomes></market></market_descriptions>`))
		case strings.HasSuffix(path, "/sports"):
			sportsHits.Add(1)
			_, _ = w.Write([]byte(`<?xml version="1.0"?><sports><sport id="od:sport:1" name="Football" abbreviation="FB"/></sports>`))
		default:
			_, _ = w.Write([]byte(`<?xml version="1.0"?><response response_code="OK"/>`))
		}
	}))
	defer srv.Close()

	cfg := NewConfig("t", types.IntegrationEnvironment,
		WithAPIHost("api.example.test"),
		WithHTTPClient(newTestHTTPClient(srv)),
		WithPreloadLocales(types.EnLocale, types.RuLocale),
	)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = c.Close(closeCtx)
	})

	// New must have warmed BOTH catalogs for BOTH locales (en + ru).
	if got := sportsHits.Load(); got != 2 {
		t.Errorf("sports catalog hits during New = %d, want 2 (en+ru eager warm)", got)
	}
	if got := marketsHits.Load(); got != 2 {
		t.Errorf("market catalog hits during New = %d, want 2 (en+ru eager warm)", got)
	}
}

// TestNew_WhoAmIWarning_UsesConfiguredLogger pins the fifth-pass P2:
// the token-expiry warning fires from the who-am-i probe inside New and
// must go through the WithLogger-supplied sink — previously the whoami
// manager was constructed with the nil-logger variant and warned into
// slog.Default().
func TestNew_WhoAmIWarning_UsesConfiguredLogger(t *testing.T) {
	soon := time.Now().Add(24 * time.Hour).UTC().Format("2006-01-02T15:04:05+00:00")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		switch {
		case strings.HasSuffix(r.URL.Path, "/users/whoami"):
			_, _ = w.Write([]byte(`<?xml version="1.0"?><bookmaker_details response_code="OK" expire_at="` + soon + `" bookmaker_id="42" virtual_host="/vhost"/>`))
		default:
			_, _ = w.Write([]byte(`<?xml version="1.0"?><response response_code="OK"/>`))
		}
	}))
	defer srv.Close()

	rec := &recordingHandler{}
	cfg := NewConfig("t", types.IntegrationEnvironment,
		WithAPIHost("api.example.test"),
		WithHTTPClient(newTestHTTPClient(srv)),
		WithLogger(slog.New(rec)),
	)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = c.Close(closeCtx)
	})

	for _, msg := range rec.messages() {
		if strings.Contains(msg, "access token expires soon") {
			return
		}
	}
	t.Fatalf("token-expiry warning did not reach the configured logger; got messages: %v", rec.messages())
}

// TestNew_PreloadFailure_FailsConstruction pins the sixth-pass P2:
// catalog warming is a construction step (NEXT.md §8) — a warm failure
// must fail New with an error, not warn and return a client whose
// promised preload silently didn't happen.
func TestNew_PreloadFailure_FailsConstruction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		switch {
		case strings.HasSuffix(r.URL.Path, "/users/whoami"):
			_, _ = w.Write([]byte(whoAmIBody))
		case strings.HasSuffix(r.URL.Path, "/sports"):
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
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := New(ctx, cfg); err == nil {
		t.Fatal("New succeeded despite the sports-catalog warm failing")
	} else if !strings.Contains(err.Error(), "preload sports catalog") {
		t.Fatalf("New error = %v, want preload context", err)
	}
}
