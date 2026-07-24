package recovery

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

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/producer"
)

// TestManager_Open_BootstrapHonorsCallerCtx is the regression for
// the v2.33 finding: pre-fix, recovery.Manager.Open used the caller-
// provided ctx for both purposes — bootstrap producer fetch AND
// actor lifetime. The caller (gosdk.Client.ensureNormal) had already
// applied WithoutCancel, so the bootstrap HTTP fetch effectively
// ignored the user's Subscribe timeout. A slow /descriptions/producers
// endpoint blocked Subscribe past its declared timeout.
//
// The fix splits the two roles: the caller-passed ctx bounds the
// bootstrap fetch, and Open derives its OWN actor-lifetime ctx via
// WithoutCancel internally.
//
// Strategy: stand up a fixture that hangs the /descriptions/producers
// endpoint until the test releases it (or the caller's ctx fires).
// Call Open with a 200ms ctx; assert it returns ctx.DeadlineExceeded
// promptly (not after the fixture hang ends).
func TestManager_Open_BootstrapHonorsCallerCtx(t *testing.T) {
	hang := make(chan struct{})
	defer close(hang)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/descriptions/producers") {
			http.NotFound(w, r)
			return
		}
		// Hang until release OR the request ctx fires.
		select {
		case <-hang:
			w.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(w, producersBody)
		case <-r.Context().Done():
			return
		}
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	cfg := &minimalCfg{apiURL: u.Host, token: "tok"}
	apiClient := api.New(cfg)
	apiClient.SetHTTPClient(&http.Client{
		Transport: &rewriteTransport{
			target: srv.URL,
			base:   &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		},
		// Generous transport timeout so the SDK's ctx — not the http
		// client's Timeout — is what enforces the user's bound.
		Timeout: 30 * time.Second,
	})
	// IMPORTANT: do NOT pre-Open the producer manager here — the
	// bug we're regressing is that recovery.Manager.Open's lazy
	// ActiveProducers HTTP call ignored the caller's ctx. If we
	// pre-Open, the call goes through the cache and the bug's
	// invisible.
	pm := producer.NewManager(cfg, apiClient, newDiscardLogger())

	mgr := NewManager(cfg, pm, apiClient, newDiscardLogger(), 0)
	defer mgr.Close()

	// Caller's ctx: 200ms. With the fix this bounds the boot fetch.
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	t0 := time.Now()
	_, err := mgr.Open(ctx)
	elapsed := time.Since(t0)

	if err == nil {
		t.Fatal("expected ctx error on hung /descriptions/producers, got nil")
	}
	// Accept either DeadlineExceeded or a wrapped variant —
	// errors.Is handles wrapping.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded (or wrapping)", err)
	}
	// The fixture hangs forever; the elapsed time should be close to
	// the caller's 200ms timeout. Generous bound (1s) for slow CI.
	if elapsed > time.Second {
		t.Errorf("Open blocked for %v, want <1s — caller ctx not honored for boot fetch", elapsed)
	}
}
