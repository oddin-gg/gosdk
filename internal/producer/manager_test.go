package producer

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/api/xml"
	log "github.com/oddin-gg/gosdk/internal/log"
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
	t, _ := url.Parse(rt.target)
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
	u, _ := url.Parse(srv.URL)
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

const producersBody = `<?xml version="1.0"?>
<producers response_code="OK">
  <producer id="1" name="live" description="Live odds" active="true" api_url="https://live" scope="live" stateful_recovery_window_in_minutes="60"/>
  <producer id="2" name="pre" description="Prematch" active="true" api_url="https://pre" scope="prematch" stateful_recovery_window_in_minutes="180"/>
  <producer id="3" name="live" description="Mixed" active="true" api_url="https://mix" scope="live|prematch" stateful_recovery_window_in_minutes="60"/>
  <producer id="4" name="live" description="Inactive" active="false" api_url="https://x" scope="live" stateful_recovery_window_in_minutes="60"/>
</producers>`

// --- tests ---

func TestManager_Open_Populates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, producersBody)
	}))
	defer srv.Close()

	mgr := NewManager(&minimalCfg{}, newAPIClient(t, srv), log.New(nil))
	if err := mgr.Open(t.Context()); err != nil {
		t.Fatalf("Open: %v", err)
	}

	available, err := mgr.AvailableProducers(t.Context())
	if err != nil {
		t.Fatalf("AvailableProducers: %v", err)
	}
	if len(available) != 4 {
		t.Errorf("got %d producers, want 4", len(available))
	}

	active, err := mgr.ActiveProducers(t.Context())
	if err != nil {
		t.Fatalf("ActiveProducers: %v", err)
	}
	if len(active) != 3 {
		t.Errorf("active = %d, want 3 (one is inactive)", len(active))
	}
}

// TestManager_Open_PreservesCallerOwnedState verifies the v2.23 fix
// to finding F1: SetProducerState / SetProducerRecoveryFromTimestamp
// called BEFORE Connect mutate in-memory state, but Connect calls
// Manager.Open again as part of ensureNormal. Pre-fix, Open replaced
// the entire producerMap with fresh newData entries — the caller's
// disable / recovery-from override silently disappeared.
//
// The fix snapshots the existing map under RLock and copies mutable
// state (enabled, flaggedDown, lastMessageTimestamp,
// lastProcessedMessageGenTimestamp, lastAliveReceivedGenTimestamp,
// recoveryFromTimestamp, lastRecoveryInfo) onto fresh entries before
// installing the new map. Catalog fields (name/description/scope/
// active) are refreshed from the API.
func TestManager_Open_PreservesCallerOwnedState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, producersBody)
	}))
	defer srv.Close()

	mgr := NewManager(&minimalCfg{}, newAPIClient(t, srv), log.New(nil))
	if err := mgr.Open(t.Context()); err != nil {
		t.Fatalf("first Open: %v", err)
	}

	// Caller toggles producer 1 from enabled (default, since active=true)
	// to disabled. Mirrors the documented "disable a producer before
	// Connect" pattern.
	if err := mgr.SetProducerState(t.Context(), 1, false); err != nil {
		t.Fatalf("SetProducerState: %v", err)
	}
	enabled, err := mgr.IsProducerEnabled(t.Context(), 1)
	if err != nil {
		t.Fatalf("IsProducerEnabled (pre-reopen): %v", err)
	}
	if enabled {
		t.Fatal("producer 1 still enabled after SetProducerState(false)")
	}

	// Set a recovery-from override on producer 2 (within window).
	recoverFrom := time.Now().Add(-30 * time.Minute)
	if err := mgr.SetProducerRecoveryFromTimestamp(t.Context(), 2, recoverFrom); err != nil {
		t.Fatalf("SetProducerRecoveryFromTimestamp: %v", err)
	}

	// Re-Open (simulating ensureNormal calling producerManager.Open
	// during a Connect that ran AFTER the caller's overrides).
	if err := mgr.Open(t.Context()); err != nil {
		t.Fatalf("second Open: %v", err)
	}

	// Caller's disable for producer 1 must survive.
	enabled, err = mgr.IsProducerEnabled(t.Context(), 1)
	if err != nil {
		t.Fatalf("IsProducerEnabled (post-reopen): %v", err)
	}
	if enabled {
		t.Fatal("producer 1 became enabled after re-Open — caller override was stomped")
	}

	// Caller's recovery-from override for producer 2 must survive too.
	d, err := mgr.producerCached(2)
	if err != nil {
		t.Fatalf("producerCached(2): %v", err)
	}
	d.mu.RLock()
	gotRecover := d.recoveryFromTimestamp
	d.mu.RUnlock()
	if !gotRecover.Equal(recoverFrom) {
		t.Errorf("recoveryFromTimestamp = %v, want %v (override stomped)", gotRecover, recoverFrom)
	}
}

// TestManager_Open_DropsProducersAbsentFromAPI verifies the converse:
// producers that disappear from the API on a subsequent Open are
// removed from the map. The catalog is authoritative for "exists at
// all"; only mutable fields on still-present producers are preserved.
func TestManager_Open_DropsProducersAbsentFromAPI(t *testing.T) {
	calls := atomic.Int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		// First call returns 4 producers; subsequent calls return only 2.
		if calls.Add(1) == 1 {
			_, _ = io.WriteString(w, producersBody)
			return
		}
		_, _ = io.WriteString(w, `<?xml version="1.0"?>
<producers response_code="OK">
  <producer id="1" name="live" description="Live odds" active="true" api_url="https://live" scope="live" stateful_recovery_window_in_minutes="60"/>
  <producer id="2" name="pre" description="Prematch" active="true" api_url="https://pre" scope="prematch" stateful_recovery_window_in_minutes="180"/>
</producers>`)
	}))
	defer srv.Close()

	mgr := NewManager(&minimalCfg{}, newAPIClient(t, srv), log.New(nil))
	if err := mgr.Open(t.Context()); err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := mgr.Open(t.Context()); err != nil {
		t.Fatalf("second Open: %v", err)
	}

	available, err := mgr.AvailableProducers(t.Context())
	if err != nil {
		t.Fatalf("AvailableProducers: %v", err)
	}
	if len(available) != 2 {
		t.Errorf("got %d producers after re-Open with shrunken catalog, want 2", len(available))
	}
	if _, ok := available[3]; ok {
		t.Error("producer 3 still present after API stopped reporting it")
	}
	if _, ok := available[4]; ok {
		t.Error("producer 4 still present after API stopped reporting it")
	}
}

func TestManager_LazyOpen(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, producersBody)
	}))
	defer srv.Close()

	mgr := NewManager(&minimalCfg{}, newAPIClient(t, srv), log.New(nil))
	// Don't call Open — let GetProducer trigger lazy open.
	p, err := mgr.GetProducer(t.Context(), 1)
	if err != nil {
		t.Fatalf("GetProducer: %v", err)
	}
	if p.ID() != 1 {
		t.Errorf("GetProducer id = %d", p.ID())
	}
	if hits.Load() != 1 {
		t.Errorf("HTTP hits = %d, want 1", hits.Load())
	}
}

// TestManager_GetProducer_UnknownProducerIsNotFound pins the strict
// public lookup: an unknown id must surface the classifiable
// ErrProducerNotFound. Pre-fix it fabricated a synthetic
// active/enabled both-scopes producer named "unknown" — a public
// caller could not distinguish a typo from a catalog member and could
// initiate recovery against it. The placeholder remains available only
// via UnknownProducerPlaceholder (feed diagnostics).
func TestManager_GetProducer_UnknownProducerIsNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, producersBody)
	}))
	defer srv.Close()

	mgr := NewManager(&minimalCfg{apiURL: "api.test"}, newAPIClient(t, srv), log.New(nil))
	if err := mgr.Open(t.Context()); err != nil {
		t.Fatalf("Open: %v", err)
	}

	p, err := mgr.GetProducer(t.Context(), 999)
	if !errors.Is(err, ErrProducerNotFound) {
		t.Fatalf("GetProducer(unknown) err = %v, want ErrProducerNotFound", err)
	}
	if p != nil {
		t.Fatalf("GetProducer(unknown) = %v, want nil", p)
	}

	// The diagnostics placeholder is still constructible on demand.
	ph, err := mgr.UnknownProducerPlaceholder(999)
	if err != nil || ph.Name() != "unknown" {
		t.Fatalf("UnknownProducerPlaceholder = %v, %v", ph, err)
	}
}

func TestManager_ActiveProducersInScope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, producersBody)
	}))
	defer srv.Close()

	mgr := NewManager(&minimalCfg{}, newAPIClient(t, srv), log.New(nil))
	if err := mgr.Open(t.Context()); err != nil {
		t.Fatalf("Open: %v", err)
	}

	live, err := mgr.ActiveProducersInScope(t.Context(), types.LiveProducerScope)
	if err != nil {
		t.Fatalf("ActiveProducersInScope live: %v", err)
	}
	// Producers 1 (live), 3 (live|prematch). 4 inactive. 2 prematch only.
	if len(live) != 2 {
		t.Errorf("live count = %d, want 2", len(live))
	}

	prematch, err := mgr.ActiveProducersInScope(t.Context(), types.PrematchProducerScope)
	if err != nil {
		t.Fatalf("ActiveProducersInScope prematch: %v", err)
	}
	// Producers 2 (prematch), 3 (live|prematch).
	if len(prematch) != 2 {
		t.Errorf("prematch count = %d, want 2", len(prematch))
	}
}

func TestProducerHasScope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, producersBody)
	}))
	defer srv.Close()

	mgr := NewManager(&minimalCfg{}, newAPIClient(t, srv), log.New(nil))
	if err := mgr.Open(t.Context()); err != nil {
		t.Fatalf("Open: %v", err)
	}

	get := func(id int) types.Producer {
		p, err := mgr.GetProducer(t.Context(), id)
		if err != nil {
			t.Fatalf("GetProducer(%d): %v", id, err)
		}
		return p
	}

	// Producer 1 = live only, 2 = prematch only, 3 = live|prematch (the
	// multi-scope / Corwyn-style case parsed from the "|"-delimited wire).
	cases := []struct {
		id                int
		wantLive, wantPre bool
		wantScopeCount    int
	}{
		{1, true, false, 1},
		{2, false, true, 1},
		{3, true, true, 2},
	}
	for _, tc := range cases {
		p := get(tc.id)
		if got := types.ProducerHasScope(p, types.LiveProducerScope); got != tc.wantLive {
			t.Errorf("producer %d ProducerHasScope(Live) = %v, want %v", tc.id, got, tc.wantLive)
		}
		if got := types.ProducerHasScope(p, types.PrematchProducerScope); got != tc.wantPre {
			t.Errorf("producer %d ProducerHasScope(Prematch) = %v, want %v", tc.id, got, tc.wantPre)
		}
		if got := len(p.ProducerScopes()); got != tc.wantScopeCount {
			t.Errorf("producer %d scope count = %d, want %d", tc.id, got, tc.wantScopeCount)
		}
	}

	// Scope values are the self-describing wire strings.
	if types.LiveProducerScope != "live" || types.PrematchProducerScope != "prematch" {
		t.Fatalf("scope constants = %q/%q, want \"live\"/\"prematch\"", types.LiveProducerScope, types.PrematchProducerScope)
	}
}

// TestManager_RetainedProducerObservesStateAfterOpen pins the fix for the
// stale-handle finding: a producer obtained before a catalog refresh
// (Open, as Connect triggers) must keep observing later state changes.
// Pre-fix, Open allocated a fresh *data per producer and copied mutable
// state forward, so a retained handle pointed at the orphaned old object
// and reported stale IsEnabled / IsFlaggedDown / timestamps.
func TestManager_RetainedProducerObservesStateAfterOpen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, producersBody)
	}))
	defer srv.Close()

	mgr := NewManager(&minimalCfg{}, newAPIClient(t, srv), log.New(nil))
	if err := mgr.Open(t.Context()); err != nil {
		t.Fatalf("first Open: %v", err)
	}

	// Retain a handle obtained BEFORE the refresh.
	available, err := mgr.AvailableProducers(t.Context())
	if err != nil {
		t.Fatalf("AvailableProducers: %v", err)
	}
	retained, ok := available[1]
	if !ok {
		t.Fatal("producer 1 missing")
	}
	if !retained.IsEnabled() {
		t.Fatal("producer 1 should start enabled (active=true)")
	}

	// A catalog refresh (what Connect does via ensureNormal).
	if err := mgr.Open(t.Context()); err != nil {
		t.Fatalf("second Open: %v", err)
	}

	// Mutate state AFTER the refresh, through the manager.
	if err := mgr.SetProducerState(t.Context(), 1, false); err != nil {
		t.Fatalf("SetProducerState: %v", err)
	}
	if err := mgr.SetProducerDown(1, false); err != nil {
		t.Fatalf("SetProducerDown: %v", err)
	}
	ts := time.Unix(1_700_000_000, 0).UTC()
	if err := mgr.SetProducerLastMessageTimestamp(1, ts); err != nil {
		t.Fatalf("SetProducerLastMessageTimestamp: %v", err)
	}

	// The RETAINED handle must observe all of it.
	if retained.IsEnabled() {
		t.Error("retained producer still reports IsEnabled()=true after SetProducerState(false) post-Open — stale handle")
	}
	if retained.IsFlaggedDown() {
		t.Error("retained producer still reports IsFlaggedDown()=true after SetProducerDown(false) post-Open")
	}
	if got := retained.LastMessageTimestamp(); !got.Equal(ts) {
		t.Errorf("retained producer LastMessageTimestamp() = %v, want %v (stale handle)", got, ts)
	}
}

func TestManager_StateMutators(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, producersBody)
	}))
	defer srv.Close()

	mgr := NewManager(&minimalCfg{}, newAPIClient(t, srv), log.New(nil))
	if err := mgr.Open(t.Context()); err != nil {
		t.Fatalf("Open: %v", err)
	}

	// IsProducerEnabled defaults to true (matches active).
	enabled, err := mgr.IsProducerEnabled(t.Context(), 1)
	if err != nil {
		t.Fatalf("IsProducerEnabled: %v", err)
	}
	if !enabled {
		t.Error("producer 1 should default to enabled")
	}

	// Disable producer 1.
	if err := mgr.SetProducerState(t.Context(), 1, false); err != nil {
		t.Fatalf("SetProducerState: %v", err)
	}
	enabled, _ = mgr.IsProducerEnabled(t.Context(), 1)
	if enabled {
		t.Error("producer 1 should be disabled after SetProducerState(false)")
	}

	// IsProducerDown defaults to true (initial state in newData).
	down, err := mgr.IsProducerDown(t.Context(), 1)
	if err != nil {
		t.Fatalf("IsProducerDown: %v", err)
	}
	if !down {
		t.Error("producer 1 should default flagged-down")
	}

	// Mark up.
	if err := mgr.SetProducerDown(1, false); err != nil {
		t.Fatalf("SetProducerDown(false): %v", err)
	}
	down, _ = mgr.IsProducerDown(t.Context(), 1)
	if down {
		t.Error("producer 1 should not be down after SetProducerDown(false)")
	}
}

func TestManager_TimestampSetters(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, producersBody)
	}))
	defer srv.Close()

	mgr := NewManager(&minimalCfg{}, newAPIClient(t, srv), log.New(nil))
	if err := mgr.Open(t.Context()); err != nil {
		t.Fatalf("Open: %v", err)
	}

	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := mgr.SetProducerLastMessageTimestamp(1, t1); err != nil {
		t.Fatalf("SetProducerLastMessageTimestamp: %v", err)
	}
	if err := mgr.SetProducerLastMessageTimestamp(1, time.Time{}); err == nil {
		t.Error("SetProducerLastMessageTimestamp should reject zero timestamp")
	}

	t2 := t1.Add(time.Minute)
	if err := mgr.SetLastProcessedMessageGenTimestamp(1, t2); err != nil {
		t.Fatalf("SetLastProcessedMessageGenTimestamp: %v", err)
	}
	if err := mgr.SetLastAliveReceivedGenTimestamp(1, t1); err != nil {
		t.Fatalf("SetLastAliveReceivedGenTimestamp: %v", err)
	}

	// Validate via GetProducer.
	p, _ := mgr.GetProducer(t.Context(), 1)
	if !p.LastMessageTimestamp().Equal(t1) {
		t.Errorf("LastMessageTimestamp = %v, want %v", p.LastMessageTimestamp(), t1)
	}
	if !p.LastProcessedMessageGenTimestamp().Equal(t2) {
		t.Errorf("LastProcessedMessageGenTimestamp = %v, want %v", p.LastProcessedMessageGenTimestamp(), t2)
	}

	// TimestampForRecovery prefers the alive-gen timestamp when set.
	if !p.TimestampForRecovery().Equal(t1) {
		t.Errorf("TimestampForRecovery = %v, want %v", p.TimestampForRecovery(), t1)
	}
}

// TestManager_ActiveProducers_SkipsInactiveWithBadScope pins that an
// INACTIVE producer carrying an empty/unknown scope does not fail the
// active-set queries. The build-then-filter fix briefly parsed scopes
// for every producer (active or not); an inactive producer with a scope
// the SDK doesn't recognise then made ActiveProducers /
// ActiveProducersInScope error, even though it should just be excluded.
func TestManager_ActiveProducers_SkipsInactiveWithBadScope(t *testing.T) {
	// Dedicated fixture: producer 2 is inactive with an unknown scope.
	// (Not added to the shared producersBody because AvailableProducers
	// builds ALL producers and would legitimately fail on it.)
	const body = `<?xml version="1.0"?>
<producers response_code="OK">
  <producer id="1" name="live" description="Live" active="true" api_url="https://live" scope="live" stateful_recovery_window_in_minutes="60"/>
  <producer id="2" name="live" description="Retired" active="false" api_url="https://x" scope="some_future_scope" stateful_recovery_window_in_minutes="60"/>
</producers>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	mgr := NewManager(&minimalCfg{}, newAPIClient(t, srv), log.New(nil))
	if err := mgr.Open(t.Context()); err != nil {
		t.Fatalf("Open: %v", err)
	}

	active, err := mgr.ActiveProducers(t.Context())
	if err != nil {
		t.Fatalf("ActiveProducers errored on an inactive bad-scope producer: %v", err)
	}
	if _, ok := active[2]; ok {
		t.Error("inactive producer 2 leaked into ActiveProducers")
	}
	if _, ok := active[1]; !ok {
		t.Error("active producer 1 missing from ActiveProducers")
	}

	inScope, err := mgr.ActiveProducersInScope(t.Context(), types.LiveProducerScope)
	if err != nil {
		t.Fatalf("ActiveProducersInScope errored on an inactive bad-scope producer: %v", err)
	}
	if _, ok := inScope[2]; ok {
		t.Error("inactive producer 2 leaked into ActiveProducersInScope")
	}
	if _, ok := inScope[1]; !ok {
		t.Error("active producer 1 missing from ActiveProducersInScope(Live)")
	}
}

// TestManager_ConcurrentOpenAndActiveProducers pins the fix for the
// catalog-read race: after Open began reusing *data and mutating
// data.active in place under the data lock, ActiveProducers /
// ActiveProducersInScope still read d.active WITHOUT that lock (and
// after producers() released m.mu). Concurrent Open + Active* reads
// then raced. Both now snapshot each producer under the data lock and
// filter on the snapshot's active flag. The race detector is the
// assertion here.
func TestManager_ConcurrentOpenAndActiveProducers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, producersBody)
	}))
	defer srv.Close()

	mgr := NewManager(&minimalCfg{}, newAPIClient(t, srv), log.New(nil))
	if err := mgr.Open(t.Context()); err != nil {
		t.Fatalf("initial Open: %v", err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Refresher: hammer Open (mutates data.active in place via refreshCatalog).
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				_ = mgr.Open(t.Context())
			}
		}
	})
	// Readers: hammer the availability filters.
	for range 2 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					_, _ = mgr.ActiveProducers(t.Context())
					_, _ = mgr.ActiveProducersInScope(t.Context(), types.LiveProducerScope)
				}
			}
		})
	}

	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestManager_ConcurrentOpenAndSetters is the regression for the
// v2.25 finding: pre-fix, Open snapshotted the existing map under
// RLock, released the lock, built a new map by copying mutable
// state from each old entry, then took Lock to install. A
// concurrent setter could mutate an old entry between the
// per-entry copy and the install, and Open would silently drop
// the mutation.
//
// The fix holds m.mu.Lock for the entire carry-over+install, and
// setters hold m.mu.RLock through their data.mu mutation — Lock
// blocks until any in-flight setter completes, at which point its
// mutation is visible on the OLD entry and is preserved by the
// carry-over.
//
// Strategy: spin Open and SetProducerDown in parallel for many
// iterations; after each pair, verify the down flag survived. With
// -race this also catches the data race that the pre-fix code had.
func TestManager_ConcurrentOpenAndSetters(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, producersBody)
	}))
	defer srv.Close()

	mgr := NewManager(&minimalCfg{}, newAPIClient(t, srv), log.New(nil))
	if err := mgr.Open(t.Context()); err != nil {
		t.Fatalf("initial Open: %v", err)
	}

	const iterations = 200
	// Base for a UNIQUE, strictly-increasing timestamp per iteration.
	// Both properties matter: unique so a lost write leaves a value that
	// DIFFERS from what we assert (a constant timestamp made a dropped
	// write invisible — the prior iteration had already stored it), and
	// increasing because SetLastAliveReceivedGenTimestamp is monotonic
	// (it ignores non-advancing values).
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < iterations; i++ {
		// Alternate the flag so a dropped SetProducerDown leaves the
		// PREVIOUS iteration's value — detectable — instead of silently
		// matching a constant expectation.
		wantDown := i%2 == 0
		wantTS := base.Add(time.Duration(i+1) * time.Hour)

		var wg sync.WaitGroup
		// Release all three goroutines as simultaneously as possible to
		// widen the snapshot-vs-install race window Open must survive.
		start := make(chan struct{})
		errs := make([]error, 3)
		wg.Add(3)
		// Goroutine A: re-Open the manager.
		go func() {
			defer wg.Done()
			<-start
			errs[0] = mgr.Open(t.Context())
		}()
		// Goroutine B: set the down flag (alternating value).
		go func() {
			defer wg.Done()
			<-start
			errs[1] = mgr.SetProducerDown(1, wantDown)
		}()
		// Goroutine C: stamp a unique, advancing timestamp.
		go func() {
			defer wg.Done()
			<-start
			errs[2] = mgr.SetLastAliveReceivedGenTimestamp(1, wantTS)
		}()
		close(start)
		wg.Wait()

		// No goroutine may error — the whole point is that a concurrent
		// Open does not fail a setter (nor vice versa).
		for gi, e := range errs {
			if e != nil {
				t.Fatalf("iter %d: goroutine %d returned error: %v", i, gi, e)
			}
		}

		// After all three settle, BOTH setter writes must be visible.
		// Pre-fix, an Open running concurrently with the setters could
		// silently drop one (or both).
		down, err := mgr.IsProducerDown(t.Context(), 1)
		if err != nil {
			t.Fatalf("iter %d IsProducerDown: %v", i, err)
		}
		if down != wantDown {
			t.Fatalf("iter %d: SetProducerDown(%v) lost across concurrent Open (got %v)", i, wantDown, down)
		}
		// LastAliveReceivedGenTimestamp isn't on the public Producer
		// interface — read it via the internal *data accessor.
		d, err := mgr.producerCached(1)
		if err != nil {
			t.Fatalf("iter %d producerCached: %v", i, err)
		}
		d.mu.RLock()
		got := d.lastAliveReceivedGenTimestamp
		d.mu.RUnlock()
		if !got.Equal(wantTS) {
			t.Fatalf("iter %d: SetLastAliveReceivedGenTimestamp lost across concurrent Open (want %v got %v)", i, wantTS, got)
		}
	}
}

func TestManager_GetProducerCached_FailsBeforeOpen(t *testing.T) {
	mgr := NewManager(&minimalCfg{apiURL: "api.test"}, nil, log.New(nil))
	// Not-opened must surface as ErrNotOpened — NOT the unknown-producer
	// placeholder. The placeholder (enabled=true, active=true) made
	// "manager has no data yet" indistinguishable from a live producer
	// for messages processed before Open completed.
	if _, err := mgr.GetProducerCached(1); !errors.Is(err, ErrNotOpened) {
		t.Fatalf("GetProducerCached err = %v, want ErrNotOpened", err)
	}
	// The diagnostics-path placeholder stays available explicitly.
	p, err := mgr.UnknownProducerPlaceholder(1)
	if err != nil {
		t.Fatalf("UnknownProducerPlaceholder: %v", err)
	}
	if p.Name() != "unknown" {
		t.Errorf("got name %q, want unknown placeholder", p.Name())
	}
}

func TestManager_GetProducerCached_AfterOpen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, producersBody)
	}))
	defer srv.Close()

	mgr := NewManager(&minimalCfg{}, newAPIClient(t, srv), log.New(nil))
	if err := mgr.Open(t.Context()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	p, err := mgr.GetProducerCached(1)
	if err != nil {
		t.Fatalf("GetProducerCached: %v", err)
	}
	if p.ID() != 1 || p.Name() != "live" {
		t.Errorf("p = %+v", p)
	}
}

// TestProducerImpl_Accessors exercises producerImpl getters that aren't
// covered by other tests (especially IsAvailable, IsEnabled, APIEndpoint,
// ProducerScopes, ProcessingQueDelay, StatefulRecoveryWindowInMinutes).
func TestProducerImpl_Accessors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, producersBody)
	}))
	defer srv.Close()

	mgr := NewManager(&minimalCfg{}, newAPIClient(t, srv), log.New(nil))
	if err := mgr.Open(t.Context()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	p, err := mgr.GetProducer(t.Context(), 3) // mixed scope
	if err != nil {
		t.Fatalf("GetProducer: %v", err)
	}
	if !p.IsAvailable() {
		t.Error("IsAvailable should be true for active producer")
	}
	if !p.IsEnabled() {
		t.Error("IsEnabled should be true by default")
	}
	if p.APIEndpoint() != "https://mix" {
		t.Errorf("APIEndpoint = %q", p.APIEndpoint())
	}
	if p.Description() != "Mixed" {
		t.Errorf("Description = %q", p.Description())
	}
	if p.StatefulRecoveryWindowInMinutes() != 60 {
		t.Errorf("StatefulRecoveryWindowInMinutes = %d", p.StatefulRecoveryWindowInMinutes())
	}
	scopes := p.ProducerScopes()
	if len(scopes) != 2 {
		t.Errorf("ProducerScopes = %v, want 2 entries", scopes)
	}
	// IsFlaggedDown defaults to true via newData.
	if !p.IsFlaggedDown() {
		t.Error("IsFlaggedDown defaults to true")
	}
	// ProcessingQueDelay is meaningful even with zero timestamp (returns
	// the time since epoch which is huge but deterministic).
	if p.ProcessingQueDelay() <= 0 {
		t.Errorf("ProcessingQueDelay = %v", p.ProcessingQueDelay())
	}
	// RecoveryInfo() returns nil when no info recorded yet — the v2.9
	// review fix to the "nil pointer to nil interface" lie.
	if p.RecoveryInfo() != nil {
		t.Error("RecoveryInfo() should return nil before any recorded info")
	}
}

func TestBuildProducerImpl_RejectsUnknownScope(t *testing.T) {
	d := &data{
		id:            1,
		name:          "live",
		producerScope: "garbage",
	}
	if _, err := buildProducerImpl(d); err == nil {
		t.Error("expected error on unknown scope")
	}
}

func TestBuildProducerImpl_RequiresAtLeastOneScope(t *testing.T) {
	d := &data{
		id:            1,
		name:          "live",
		producerScope: "",
	}
	if _, err := buildProducerImpl(d); err == nil {
		t.Error("expected error on empty scope")
	}
}

// TestNewData_DefaultsFlaggedDownTrue pins the safe-start default: a
// freshly-catalogued producer is flagged DOWN (and enabled tracks the
// catalog's active flag) until the first alive proves it up — starting
// up would report a healthy feed before any evidence exists.
func TestNewData_DefaultsFlaggedDownTrue(t *testing.T) {
	d := newData(xml.Producer{ID: 7, Name: "live", Active: true, Scope: "live"})
	if !d.flaggedDown {
		t.Error("newData(...).flaggedDown = false, want true (down until first alive)")
	}
	if !d.enabled {
		t.Error("newData(active).enabled = false, want true (tracks catalog active flag)")
	}
	d = newData(xml.Producer{ID: 8, Name: "pre", Active: false, Scope: "prematch"})
	if d.enabled {
		t.Error("newData(inactive).enabled = true, want false")
	}
}

// TestSetProducerRecoveryFromTimestamp_RoundTrips ensures the
// SetProducerRecoveryFromTimestamp + TimestampForRecovery interaction
// works when no alive timestamp has been seen.
func TestSetProducerRecoveryFromTimestamp_RoundTrips(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, producersBody)
	}))
	defer srv.Close()

	mgr := NewManager(&minimalCfg{}, newAPIClient(t, srv), log.New(nil))
	if err := mgr.Open(t.Context()); err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Within the producer's stateful_recovery_window_in_minutes (60 minutes
	// per the test fixture). Older than the window would be rejected.
	t0 := time.Now().Add(-30 * time.Minute)
	if err := mgr.SetProducerRecoveryFromTimestamp(t.Context(), 1, t0); err != nil {
		t.Fatalf("SetProducerRecoveryFromTimestamp: %v", err)
	}
	p, _ := mgr.GetProducer(t.Context(), 1)
	if !p.TimestampForRecovery().Equal(t0) {
		t.Errorf("TimestampForRecovery = %v, want %v", p.TimestampForRecovery(), t0)
	}

	// Out-of-range timestamps should error.
	tooOld := time.Now().Add(-2 * time.Hour)
	if err := mgr.SetProducerRecoveryFromTimestamp(t.Context(), 1, tooOld); err == nil {
		t.Error("expected error for too-old timestamp")
	}

	// FUTURE timestamps must error too: time.Since(future) is negative,
	// so the too-old check passed and a clock/units mistake (ms-vs-s
	// epoch, wrong zone) was serialized as the recovery `after` value —
	// silently omitting every currently-missing message. Small skew
	// (< recoveryFromMaxSkew) stays accepted.
	future := time.Now().Add(2 * time.Hour)
	if err := mgr.SetProducerRecoveryFromTimestamp(t.Context(), 1, future); err == nil {
		t.Error("expected error for future timestamp")
	}
	skewed := time.Now().Add(10 * time.Second)
	if err := mgr.SetProducerRecoveryFromTimestamp(t.Context(), 1, skewed); err != nil {
		t.Errorf("small clock skew rejected: %v", err)
	}
}

// TestSetProducerRecoveryFromTimestamp_OverridesAliveCursor is the
// regression for the P1 finding: an explicit recovery cursor set AFTER an
// alive timestamp already exists must still win (the setter's documented
// purpose is rewinding the recovery point), and must be consumed once a
// recovery is recorded so later recoveries resume from the freshest alive
// cursor instead of re-rewinding forever.
func TestSetProducerRecoveryFromTimestamp_OverridesAliveCursor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, producersBody)
	}))
	defer srv.Close()

	mgr := NewManager(&minimalCfg{}, newAPIClient(t, srv), log.New(nil))
	if err := mgr.Open(t.Context()); err != nil {
		t.Fatalf("Open: %v", err)
	}

	// An alive advances the alive-gen cursor first.
	aliveGen := time.Now().Add(-10 * time.Minute)
	if err := mgr.SetLastAliveReceivedGenTimestamp(1, aliveGen); err != nil {
		t.Fatalf("SetLastAliveReceivedGenTimestamp: %v", err)
	}
	p, _ := mgr.GetProducer(t.Context(), 1)
	if !p.TimestampForRecovery().Equal(aliveGen) {
		t.Fatalf("pre-override TimestampForRecovery = %v, want alive %v", p.TimestampForRecovery(), aliveGen)
	}

	// A consumer then rewinds explicitly to an EARLIER point. Pre-fix the
	// alive cursor kept winning and the rewind was silently ignored.
	rewind := time.Now().Add(-45 * time.Minute)
	if err := mgr.SetProducerRecoveryFromTimestamp(t.Context(), 1, rewind); err != nil {
		t.Fatalf("SetProducerRecoveryFromTimestamp: %v", err)
	}
	p, _ = mgr.GetProducer(t.Context(), 1)
	if !p.TimestampForRecovery().Equal(rewind) {
		t.Fatalf("post-override TimestampForRecovery = %v, want explicit rewind %v", p.TimestampForRecovery(), rewind)
	}

	// An alive arriving before recovery begins must NOT defeat the override.
	newerAlive := time.Now().Add(-5 * time.Minute)
	if err := mgr.SetLastAliveReceivedGenTimestamp(1, newerAlive); err != nil {
		t.Fatalf("SetLastAliveReceivedGenTimestamp: %v", err)
	}
	p, _ = mgr.GetProducer(t.Context(), 1)
	if !p.TimestampForRecovery().Equal(rewind) {
		t.Fatalf("override defeated by a later alive: TimestampForRecovery = %v, want %v", p.TimestampForRecovery(), rewind)
	}

	// Recording a recovery consumes the one-shot override; the freshest
	// alive cursor then wins again (no perpetual re-rewind).
	if err := mgr.SetProducerRecoveryInfo(1, recoveryInfoStub{}); err != nil {
		t.Fatalf("SetProducerRecoveryInfo: %v", err)
	}
	p, _ = mgr.GetProducer(t.Context(), 1)
	if !p.TimestampForRecovery().Equal(newerAlive) {
		t.Fatalf("override not consumed after recovery recorded: TimestampForRecovery = %v, want alive %v", p.TimestampForRecovery(), newerAlive)
	}
}

// recoveryInfoStub is a minimal types.RecoveryInfo for the override-
// consumption assertion (field values are irrelevant to the test).
type recoveryInfoStub struct{}

func (recoveryInfoStub) After() time.Time            { return time.Time{} }
func (recoveryInfoStub) Timestamp() time.Time        { return time.Time{} }
func (recoveryInfoStub) RequestID() int              { return 0 }
func (recoveryInfoStub) Successful() bool            { return true }
func (recoveryInfoStub) NodeID() types.Optional[int] { return types.None[int]() }

// TestManager_GenCursors_RejectGrossFutureSkew is the regression for the
// future-timestamp poison finding: the monotonic gen cursors
// (lastProcessedMessageGenTimestamp, lastAliveReceivedGenTimestamp) advance
// only forward, so a single corrupt far-future feed timestamp (wrong
// epoch / year-3000) would pin the cursor there permanently — later
// legitimate (smaller) timestamps can't replace it, and calculateTiming
// reads now-cursor via Abs, so the producer looks delayed forever. Grossly-
// future values are now ignored (cursor keeps its last good value); small
// skew is still accepted.
func TestManager_GenCursors_RejectGrossFutureSkew(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, producersBody)
	}))
	defer srv.Close()

	mgr := NewManager(&minimalCfg{}, newAPIClient(t, srv), log.New(nil))
	if err := mgr.Open(t.Context()); err != nil {
		t.Fatalf("Open: %v", err)
	}

	good := time.Now().Add(-5 * time.Second)
	farFuture := time.Now().Add(3000 * time.Hour) // corrupt

	for _, tc := range []struct {
		name string
		set  func(time.Time) error
		read func(*data) time.Time
	}{
		{"processed-gen", func(ts time.Time) error { return mgr.SetLastProcessedMessageGenTimestamp(1, ts) }, func(d *data) time.Time { return d.lastProcessedMessageGenTimestamp }},
		{"alive-gen", func(ts time.Time) error { return mgr.SetLastAliveReceivedGenTimestamp(1, ts) }, func(d *data) time.Time { return d.lastAliveReceivedGenTimestamp }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.set(good); err != nil {
				t.Fatalf("set good: %v", err)
			}
			// A grossly-future timestamp must be ignored — the returned
			// error is nil (silently dropped, like a stale value) but the
			// cursor must NOT advance to the poison value.
			if err := tc.set(farFuture); err != nil {
				t.Fatalf("set far-future returned error (want silent ignore): %v", err)
			}
			d, err := mgr.producerCached(1)
			if err != nil {
				t.Fatalf("producerCached: %v", err)
			}
			d.mu.RLock()
			got := tc.read(d)
			d.mu.RUnlock()
			if !got.Equal(good) {
				t.Fatalf("cursor = %v, want last good %v (far-future value poisoned the cursor)", got, good)
			}
			// A small skew (within tolerance) is still accepted and advances.
			skewed := time.Now().Add(30 * time.Second)
			if err := tc.set(skewed); err != nil {
				t.Fatalf("set small-skew: %v", err)
			}
			d, _ = mgr.producerCached(1)
			d.mu.RLock()
			got = tc.read(d)
			d.mu.RUnlock()
			if !got.Equal(skewed) {
				t.Fatalf("small-skew cursor = %v, want %v (in-tolerance skew should advance)", got, skewed)
			}
		})
	}
}

// TestManager_TimestampCursors_Monotonic is the regression for the
// multi-subscription cursor regression (Codex P2): two subscriptions
// share the producer manager, and the slower one — finishing an OLDER
// message later — must not roll a freshness cursor backwards; a
// regressed cursor made the recovery tick read the producer as delayed
// and could initiate a false producer-down transition. Older values are
// ignored; newer values still advance.
func TestManager_TimestampCursors_Monotonic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, producersBody)
	}))
	defer srv.Close()

	mgr := NewManager(&minimalCfg{}, newAPIClient(t, srv), log.New(nil))
	if err := mgr.Open(t.Context()); err != nil {
		t.Fatalf("Open: %v", err)
	}

	newer := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	older := newer.Add(-time.Minute)
	evenNewer := newer.Add(time.Minute)

	cursors := []struct {
		name string
		set  func(time.Time) error
		get  func() time.Time
	}{
		{
			name: "lastMessageTimestamp",
			set:  func(ts time.Time) error { return mgr.SetProducerLastMessageTimestamp(1, ts) },
			get: func() time.Time {
				p, err := mgr.GetProducer(t.Context(), 1)
				if err != nil {
					t.Fatalf("GetProducer: %v", err)
				}
				return p.LastMessageTimestamp()
			},
		},
		{
			name: "lastProcessedMessageGenTimestamp",
			set:  func(ts time.Time) error { return mgr.SetLastProcessedMessageGenTimestamp(1, ts) },
			get: func() time.Time {
				p, err := mgr.GetProducer(t.Context(), 1)
				if err != nil {
					t.Fatalf("GetProducer: %v", err)
				}
				return p.LastProcessedMessageGenTimestamp()
			},
		},
	}
	for _, c := range cursors {
		if err := c.set(newer); err != nil {
			t.Fatalf("%s: set newer: %v", c.name, err)
		}
		if err := c.set(older); err != nil {
			t.Fatalf("%s: set older: %v", c.name, err)
		}
		if got := c.get(); !got.Equal(newer) {
			t.Errorf("%s regressed to %v after older write, want %v", c.name, got, newer)
		}
		if err := c.set(evenNewer); err != nil {
			t.Fatalf("%s: set even newer: %v", c.name, err)
		}
		if got := c.get(); !got.Equal(evenNewer) {
			t.Errorf("%s = %v after newer write, want %v (monotonic guard must still advance)", c.name, got, evenNewer)
		}
	}
	// SetLastAliveReceivedGenTimestamp shares the identical guard; it has
	// no public accessor, so exercise the setter for error-free operation.
	if err := mgr.SetLastAliveReceivedGenTimestamp(1, newer); err != nil {
		t.Fatalf("SetLastAliveReceivedGenTimestamp newer: %v", err)
	}
	if err := mgr.SetLastAliveReceivedGenTimestamp(1, older); err != nil {
		t.Fatalf("SetLastAliveReceivedGenTimestamp older: %v", err)
	}
}

// TestManager_Setters_UnknownProducerIsNotFound pins the sentinel on the
// mutate path (Codex P2): the missing-id branch of mutateProducerByID
// returned a text-only error, so the PUBLIC setters routed through it
// (Client.SetProducerEnabled / SetProducerRecoveryFromTimestamp) failed
// errors.Is(err, ErrProducerNotFound) while GetProducer succeeded — the
// documented sentinel contract covered lookups only by accident.
func TestManager_Setters_UnknownProducerIsNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, producersBody)
	}))
	defer srv.Close()

	mgr := NewManager(&minimalCfg{apiURL: "api.test"}, newAPIClient(t, srv), log.New(nil))
	if err := mgr.Open(t.Context()); err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := mgr.SetProducerState(t.Context(), 999, false); !errors.Is(err, ErrProducerNotFound) {
		t.Errorf("SetProducerState(unknown) err = %v, want ErrProducerNotFound", err)
	}
	if err := mgr.SetProducerRecoveryFromTimestamp(t.Context(), 999, time.Now().Add(-time.Hour)); !errors.Is(err, ErrProducerNotFound) {
		t.Errorf("SetProducerRecoveryFromTimestamp(unknown) err = %v, want ErrProducerNotFound", err)
	}
	if err := mgr.SetProducerDown(999, true); !errors.Is(err, ErrProducerNotFound) {
		t.Errorf("SetProducerDown(unknown) err = %v, want ErrProducerNotFound", err)
	}
}

// TestManager_Setters_CanceledCtxDoesNotMutate pins the ctx contract on
// the warm-cache mutate path (Codex P3): a setter invoked with an
// already-cancelled context previously mutated producer state and
// returned nil — cancellation must mean NO side effect.
func TestManager_Setters_CanceledCtxDoesNotMutate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, producersBody)
	}))
	defer srv.Close()

	mgr := NewManager(&minimalCfg{apiURL: "api.test"}, newAPIClient(t, srv), log.New(nil))
	if err := mgr.Open(t.Context()); err != nil {
		t.Fatalf("Open: %v", err)
	}

	before, err := mgr.IsProducerEnabled(t.Context(), 1)
	if err != nil {
		t.Fatalf("IsProducerEnabled: %v", err)
	}

	dead, cancel := context.WithCancel(t.Context())
	cancel()
	if err := mgr.SetProducerState(dead, 1, !before); !errors.Is(err, context.Canceled) {
		t.Fatalf("SetProducerState(cancelled ctx) err = %v, want context.Canceled", err)
	}
	after, err := mgr.IsProducerEnabled(t.Context(), 1)
	if err != nil {
		t.Fatalf("IsProducerEnabled: %v", err)
	}
	if after != before {
		t.Fatalf("cancelled-ctx setter mutated state: enabled %v -> %v", before, after)
	}
}
