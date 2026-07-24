package whoami

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/types"
)

// minimalCfg satisfies config.Config for tests.
type minimalCfg struct {
	apiURL string
	token  string
}

func (c *minimalCfg) AccessToken() *string                    { return &c.token }
func (c *minimalCfg) DefaultLocale() types.Locale             { return types.EnLocale }
func (c *minimalCfg) MaxInactivity() time.Duration            { return 20 * time.Second }
func (c *minimalCfg) MaxRecoveryExecution() time.Duration     { return 360 * time.Minute }
func (c *minimalCfg) MessagingPort() int                      { return 5672 }
func (c *minimalCfg) SdkNodeID() *int                         { return nil }
func (c *minimalCfg) SelectedEnvironment() *types.Environment { return nil }
func (c *minimalCfg) SelectedRegion() types.Region            { return types.RegionDefault }
func (c *minimalCfg) ExchangeName() string                    { return "oddinfeed" }
func (c *minimalCfg) ReplayExchangeName() string              { return "oddinreplay" }
func (c *minimalCfg) ReportExtendedData() bool                { return false }
func (c *minimalCfg) APIURL() (string, error)                 { return c.apiURL, nil }
func (c *minimalCfg) MQURL() (string, error)                  { return "", nil }
func (c *minimalCfg) SportIDPrefix() string                   { return "od:sport:" }

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

func newAPIClient(t *testing.T, srv *httptest.Server) *api.Client {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("bad server url: %v", err)
	}
	c := api.New(&minimalCfg{apiURL: u.Host, token: "tok"})
	c.SetHTTPClient(&http.Client{
		Transport: &rewriteTransport{
			target: srv.URL,
			base:   &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		},
		Timeout: 2 * time.Second,
	})
	return c
}

const okBody = `<?xml version="1.0"?>
<bookmaker_details response_code="OK" expire_at="2099-01-01T00:00:00" bookmaker_id="42" virtual_host="/vhost"/>`

// bodyExpiringAt renders a valid who-am-i response whose expire_at is
// the given instant. Zone-less timestamps parse as UTC (utils.DateTime),
// so format in UTC to keep the absolute instant exact. Dynamic rather
// than canned: the manager now REJECTS already-expired timestamps, so a
// fixed "expires soon" date would start failing the warn test the day
// it passed.
func bodyExpiringAt(t time.Time) string {
	return `<?xml version="1.0"?>
<bookmaker_details response_code="OK" expire_at="` + t.UTC().Format("2006-01-02T15:04:05") + `" bookmaker_id="42" virtual_host="/vhost"/>`
}

// --- tests ---

func TestManager_BookmakerDetails_FetchesAndCaches(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, okBody)
	}))
	defer srv.Close()

	cfg := &minimalCfg{}
	mgr := NewManager(cfg, newAPIClient(t, srv))

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	bd1, err := mgr.BookmakerDetails(ctx)
	if err != nil {
		t.Fatalf("BookmakerDetails: %v", err)
	}
	if bd1.BookmakerID() != 42 {
		t.Errorf("BookmakerID = %d, want 42", bd1.BookmakerID())
	}
	if bd1.VirtualHost() != "/vhost" {
		t.Errorf("VirtualHost = %q, want /vhost", bd1.VirtualHost())
	}
	if bd1.ExpireAt().IsZero() {
		t.Error("ExpireAt should be populated")
	}

	// Second call should hit the cache (no new HTTP request).
	bd2, err := mgr.BookmakerDetails(ctx)
	if err != nil {
		t.Fatalf("BookmakerDetails (cached): %v", err)
	}
	if bd2 != bd1 {
		t.Errorf("cached call returned a different value")
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("server hits = %d, want 1", got)
	}
}

func TestManager_BookmakerDetails_PropagatesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w,
			`<?xml version="1.0"?><response response_code="FORBIDDEN"><action>auth</action><message>bad</message></response>`)
	}))
	defer srv.Close()

	cfg := &minimalCfg{}
	mgr := NewManager(cfg, newAPIClient(t, srv))

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	if _, err := mgr.BookmakerDetails(ctx); err == nil {
		t.Fatal("expected error on 401")
	}
}

// TestManager_LogsWhenTokenExpiresSoon verifies the soon-to-expire warning.
// Capture slog output via a custom handler.
func TestManager_LogsWhenTokenExpiresSoon(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, bodyExpiringAt(time.Now().Add(72*time.Hour)))
	}))
	defer srv.Close()

	var captured warnCounter
	logger := slog.New(&captured)
	cfg := &minimalCfg{}
	mgr := NewManagerWithLogger(context.Background(), cfg, newAPIClient(t, srv), logger)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, err := mgr.BookmakerDetails(ctx); err != nil {
		t.Fatalf("BookmakerDetails: %v", err)
	}

	// The expireAt is now+72h — still valid (not rejected as expired)
	// but inside the 7-day warning window, so the warn must fire.
	if !captured.hasWarn() {
		t.Error("expected a warn-level log when token expires within 7 days")
	}
}

// TestManager_BookmakerDetails_RejectsExpiredToken is the regression
// for the Codex P3 finding: an expire_at in the past only triggered the
// "expires soon" warning and was then CACHED as valid — gosdk.New
// succeeded and authentication failed later, mid-broker/API work. The
// manager must reject already-expired credentials up front and must not
// cache them.
func TestManager_BookmakerDetails_RejectsExpiredToken(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, bodyExpiringAt(time.Now().Add(-time.Hour)))
	}))
	defer srv.Close()

	cfg := &minimalCfg{}
	mgr := NewManager(cfg, newAPIClient(t, srv))

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	if _, err := mgr.BookmakerDetails(ctx); err == nil {
		t.Fatal("expected error for already-expired expire_at, got nil")
	}

	// The failed response must not have been cached — a second call
	// re-fetches (and fails again) rather than serving expired details.
	if _, err := mgr.BookmakerDetails(ctx); err == nil {
		t.Fatal("second call: expected error, got nil (expired details were cached)")
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("server hits = %d, want 2 (expired response must not be cached)", got)
	}
}

// warnCounter is a slog.Handler that records whether any Warn-level
// record passed through.
type warnCounter struct {
	warns atomic.Int64
}

func (w *warnCounter) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelWarn
}
func (w *warnCounter) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		w.warns.Add(1)
	}
	return nil
}
func (w *warnCounter) WithAttrs(_ []slog.Attr) slog.Handler { return w }
func (w *warnCounter) WithGroup(_ string) slog.Handler      { return w }
func (w *warnCounter) hasWarn() bool                        { return w.warns.Load() > 0 }

// TestManager_BookmakerDetails_ConcurrentCallerCtxIsHonored is the
// regression for the v2.33 finding: pre-fix the manager held a
// plain Mutex across the FetchWhoAmI HTTP call, so a slow first
// caller (A, ctx=30s) blocked every concurrent caller B's ctx —
// B's 5s timeout was silently extended to A's 30s.
//
// Strategy: stand up a fixture that hangs the WhoAmI handler until
// the test releases it. Spawn caller A with a long ctx (no
// timeout). Spawn caller B with a 200ms ctx; assert B returns
// ctx.DeadlineExceeded around the 200ms mark, NOT after A's call
// completes. Then release the hang and verify A returns OK and
// caches.
func TestManager_BookmakerDetails_ConcurrentCallerCtxIsHonored(t *testing.T) {
	hang := make(chan struct{})
	started := make(chan struct{})
	var startOnce sync.Once
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		startOnce.Do(func() { close(started) }) // A's HTTP call provably in flight
		<-hang
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, okBody)
	}))
	defer srv.Close()

	cfg := &minimalCfg{}
	mgr := NewManager(cfg, newAPIClient(t, srv))

	// Caller A: long ctx, will block on the hang.
	type result struct {
		bd  types.BookmakerDetail
		err error
		dt  time.Duration
	}
	aDone := make(chan result, 1)
	go func() {
		ctxA, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		t0 := time.Now()
		bd, err := mgr.BookmakerDetails(ctxA)
		aDone <- result{bd: bd, err: err, dt: time.Since(t0)}
	}()

	// Wait for A's HTTP call to be provably in flight (handshake, not a
	// sleep) so A owns the singleflight before B joins — a descheduled A
	// could otherwise let B become the owner and pass every assertion
	// without ever being a concurrent waiter.
	<-started

	// Caller B: 200ms ctx. Pre-fix would block until A's call
	// returns. With the singleflight + DoChan + ctx-select pattern,
	// B should return ctx.DeadlineExceeded promptly.
	bCtx, bCancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer bCancel()
	bStart := time.Now()
	_, bErr := mgr.BookmakerDetails(bCtx)
	bElapsed := time.Since(bStart)

	if bErr == nil {
		t.Fatal("caller B: expected ctx error, got nil")
	}
	if !errors.Is(bErr, context.DeadlineExceeded) {
		t.Errorf("caller B err = %v, want DeadlineExceeded", bErr)
	}
	if bElapsed > 600*time.Millisecond {
		t.Errorf("caller B blocked for %v — ctx not honored independently", bElapsed)
	}

	// Release A's hang and verify it succeeds.
	close(hang)
	a := <-aDone
	if a.err != nil {
		t.Fatalf("caller A: %v", a.err)
	}
	if a.bd == nil || a.bd.BookmakerID() != 42 {
		t.Errorf("caller A bd = %v", a.bd)
	}

	// Verify the upstream HTTP call only fired once (singleflight
	// coalesced) — both callers shared one in-flight request.
	if got := hits.Load(); got != 1 {
		t.Errorf("server hits = %d, want 1 (singleflight should coalesce)", got)
	}
}

// TestManager_BookmakerDetails_FirstCallerCancellationDoesNotKillSharedFetch
// is the regression for the reviewer's medium finding: if the shared
// FetchWhoAmI is invoked with the first caller's ctx, a short-deadline
// first caller can cancel the HTTP request out from under everyone else
// who joined the same singleflight.
//
// Strategy: server hangs ~250ms before responding. Caller A has a 50ms
// ctx and will deadline-exceed. Caller B (joining ~10ms later) has 2s
// and must still succeed — the load runs under WithoutCancel, so A's
// cancellation can't kill it.
func TestManager_BookmakerDetails_FirstCallerCancellationDoesNotKillSharedFetch(t *testing.T) {
	// Handshake instead of sleeps: `started` proves caller A OWNS the
	// in-flight HTTP fetch before B launches; `release` holds the
	// response open past A's deadline deterministically (see the
	// equivalent rework in lru/singleflight_test.go).
	started := make(chan struct{})
	release := make(chan struct{})
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			close(started)
		}
		// Honor the request's ctx if cancelled — used to detect the
		// pre-fix bug. Pre-fix, when caller A's 50ms ctx expired, the
		// API client would observe ctx cancellation and drop the
		// connection; this select would fire the cancel branch.
		select {
		case <-release:
		case <-r.Context().Done():
			http.Error(w, "request ctx cancelled", http.StatusGatewayTimeout)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, okBody)
	}))
	defer srv.Close()

	cfg := &minimalCfg{}
	mgr := NewManager(cfg, newAPIClient(t, srv))

	type result struct {
		bd  types.BookmakerDetail
		err error
	}

	// Caller A: 50ms ctx. Will deadline-exceed long before serverDelay.
	aDone := make(chan result, 1)
	go func() {
		ctxA, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		bd, err := mgr.BookmakerDetails(ctxA)
		aDone <- result{bd: bd, err: err}
	}()

	// Caller B joins only once A's HTTP fetch is provably in flight.
	<-started
	bDone := make(chan result, 1)
	go func() {
		ctxB, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		bd, err := mgr.BookmakerDetails(ctxB)
		bDone <- result{bd: bd, err: err}
	}()

	a := <-aDone // A deadline-exceeds while the response is held open
	if a.err == nil {
		t.Fatalf("caller A: want ctx error, got bd=%v err=nil", a.bd)
	}
	if !errors.Is(a.err, context.DeadlineExceeded) {
		t.Errorf("caller A err = %v, want DeadlineExceeded", a.err)
	}

	close(release)
	b := <-bDone
	if b.err != nil {
		t.Fatalf("caller B failed: %v — A's cancellation killed the shared fetch", b.err)
	}
	if b.bd == nil || b.bd.BookmakerID() != 42 {
		t.Errorf("caller B bd = %v, want bookmaker_id=42", b.bd)
	}

	// Singleflight should have coalesced both callers into a single
	// upstream hit. (Even with the bug, hits could be 1 — the assertion
	// here is belt-and-suspenders to catch a regression where A's
	// cancellation triggers a retry.)
	if got := hits.Load(); got != 1 {
		t.Errorf("server hits = %d, want 1", got)
	}
}

// TestManager_BookmakerDetails_AlreadyCancelledCtxNoFetch is the
// regression for the reviewer's low finding: an already-cancelled
// caller ctx must not trigger a detached HTTP fetch through
// WithoutCancel. Pre-fix, BookmakerDetails would still hit the
// upstream once even though the caller immediately observes ctx.Err.
func TestManager_BookmakerDetails_AlreadyCancelledCtxNoFetch(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, okBody)
	}))
	defer srv.Close()

	cfg := &minimalCfg{}
	mgr := NewManager(cfg, newAPIClient(t, srv))

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // pre-cancel before any call

	_, err := mgr.BookmakerDetails(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}

	// Give a beat to confirm no async fetch sneaks through.
	time.Sleep(50 * time.Millisecond)
	if got := hits.Load(); got != 0 {
		t.Errorf("server hits = %d, want 0 (cancelled ctx must not start a detached fetch)", got)
	}
}

// TestManager_BookmakerDetailImpl_Accessors covers the small impl type's
// accessors.
func TestManager_BookmakerDetailImpl_Accessors(t *testing.T) {
	now := time.Now()
	b := bookmakerDetailImpl{
		expireAt:    now,
		bookmakerID: 7,
		virtualHost: "/vhost",
	}
	if !b.ExpireAt().Equal(now) {
		t.Errorf("ExpireAt = %v", b.ExpireAt())
	}
	if b.BookmakerID() != 7 {
		t.Errorf("BookmakerID = %d", b.BookmakerID())
	}
	if b.VirtualHost() != "/vhost" {
		t.Errorf("VirtualHost = %q", b.VirtualHost())
	}
}
