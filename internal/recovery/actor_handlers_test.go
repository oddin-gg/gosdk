package recovery

import (
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/producer"
	"github.com/oddin-gg/gosdk/types"
)

// minimalCfg satisfies config.Config.
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

const producersBody = `<?xml version="1.0"?>
<producers response_code="OK">
  <producer id="1" name="live" description="Live" active="true" api_url="https://x" scope="live" stateful_recovery_window_in_minutes="60"/>
  <producer id="2" name="pre" description="Pre" active="true" api_url="https://x" scope="prematch" stateful_recovery_window_in_minutes="60"/>
  <producer id="3" name="mixed" description="Mixed" active="true" api_url="https://x" scope="live|prematch" stateful_recovery_window_in_minutes="60"/>
</producers>`

// fixtureSrv routes /descriptions/producers and recovery/event-recovery
// endpoints. Other paths return 200 with an empty body so the api.Client
// considers them successful.
func fixtureSrv(t *testing.T) (*httptest.Server, *recoveryHits) {
	t.Helper()
	hits := &recoveryHits{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		switch {
		case strings.HasSuffix(r.URL.Path, "/descriptions/producers"):
			_, _ = io.WriteString(w, producersBody)
		case strings.Contains(r.URL.Path, "/recovery/initiate_request"):
			hits.recover.Add(1)
			_, _ = io.WriteString(w, `<?xml version="1.0"?><response response_code="OK"/>`)
		case strings.Contains(r.URL.Path, "/odds/events/"):
			hits.eventRecover.Add(1)
			_, _ = io.WriteString(w, `<?xml version="1.0"?><response response_code="OK"/>`)
		case strings.Contains(r.URL.Path, "/stateful_messages/events/"):
			hits.statefulRecover.Add(1)
			_, _ = io.WriteString(w, `<?xml version="1.0"?><response response_code="OK"/>`)
		default:
			_, _ = io.WriteString(w, `<?xml version="1.0"?><response response_code="OK"/>`)
		}
	}))
	return srv, hits
}

type recoveryHits struct {
	// Atomic counters: the fixture HTTP handler runs on the server's
	// goroutine while the test goroutine polls these. Pre-v2.24 the
	// API call was synchronous with the actor reply, so test reads
	// implicitly happened-after the handler write; with the v2.24
	// detached-API restructure the test polls while the handler may
	// still be writing.
	recover         atomic.Int32
	eventRecover    atomic.Int32
	statefulRecover atomic.Int32
}

// newProducerManagerFor builds a producer.Manager talking to srv.
func newProducerManagerFor(t *testing.T, srv *httptest.Server) *producer.Manager {
	t.Helper()
	u, _ := url.Parse(srv.URL)
	cfg := &minimalCfg{apiURL: u.Host, token: "tok"}
	apiClient := api.New(cfg)
	apiClient.SetHTTPClient(&http.Client{
		Transport: &rewriteTransport{
			target: srv.URL,
			base:   &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		},
		Timeout: 2 * time.Second,
	})
	mgr := producer.NewManager(cfg, apiClient, newDiscardLogger())
	if err := mgr.Open(t.Context()); err != nil {
		t.Fatalf("producer manager Open: %v", err)
	}
	return mgr
}

// newWiredActor builds an actor with a real producer.Manager (httptest
// backed) and a fake managerOps to capture emissions.
func newWiredActor(t *testing.T, srv *httptest.Server, fake *fakeManagerOps) *recoveryActor {
	return newWiredActorForProducer(t, srv, fake, 1)
}

// newWiredActorForProducer is the parameterized variant — useful when
// a test needs an actor bound to a non-default producer (e.g., the
// mixed-scope producer id=3 in the fixture body).
func newWiredActorForProducer(t *testing.T, srv *httptest.Server, fake *fakeManagerOps, producerID int) *recoveryActor {
	t.Helper()
	pm := newProducerManagerFor(t, srv)
	u, _ := url.Parse(srv.URL)
	cfg := &minimalCfg{apiURL: u.Host, token: "tok"}
	apiClient := api.New(cfg)
	apiClient.SetHTTPClient(&http.Client{
		Transport: &rewriteTransport{
			target: srv.URL,
			base:   &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		},
		Timeout: 2 * time.Second,
	})
	a := newRecoveryActor(t.Context(), producerID, cfg, apiClient, pm, fake, newDiscardLogger(), 32, 0)
	return a
}

// --- onMessageProcessingStarted / Ended ---

func TestActor_OnMessageProcessingStarted_Records(t *testing.T) {
	srv, _ := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, newFakeManagerOps())

	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	a.onMessageProcessingStarted(now)

	prod, _ := a.pm.GetProducer(t.Context(), 1)
	if !prod.LastMessageTimestamp().Equal(now) {
		t.Errorf("LastMessageTimestamp = %v, want %v", prod.LastMessageTimestamp(), now)
	}
}

func TestActor_OnMessageProcessingStarted_IgnoresZero(t *testing.T) {
	srv, _ := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, newFakeManagerOps())

	a.onMessageProcessingStarted(time.Time{})
	prod, _ := a.pm.GetProducer(t.Context(), 1)
	if !prod.LastMessageTimestamp().IsZero() {
		t.Errorf("zero-timestamp call should be a no-op")
	}
}

func TestActor_OnMessageProcessingEnded(t *testing.T) {
	srv, _ := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, newFakeManagerOps())

	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	a.onMessageProcessingEnded(now)
	prod, _ := a.pm.GetProducer(t.Context(), 1)
	if !prod.LastProcessedMessageGenTimestamp().Equal(now) {
		t.Errorf("LastProcessedMessageGenTimestamp = %v", prod.LastProcessedMessageGenTimestamp())
	}
	// Zero-timestamp is a no-op.
	prev := prod.LastProcessedMessageGenTimestamp()
	a.onMessageProcessingEnded(time.Time{})
	prod, _ = a.pm.GetProducer(t.Context(), 1)
	if !prod.LastProcessedMessageGenTimestamp().Equal(prev) {
		t.Errorf("zero-timestamp shouldn't update")
	}
}

// --- onAlive (user-session path; no isSubscribed branch) ---

func TestActor_OnAlive_UserSessionUpdatesTimestamp(t *testing.T) {
	srv, _ := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, newFakeManagerOps())

	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	a.onAlive(evAlive{
		timestamp:       types.MessageTimestamp{Created: now},
		isSubscribed:    true,
		messageInterest: types.AllMessageInterest, // non-system → user session
	})
	if !a.lastUserSessionAlive.Equal(now) {
		t.Errorf("lastUserSessionAlive = %v, want %v", a.lastUserSessionAlive, now)
	}
}

// TestActor_EnqueueAlive_SystemNotOverwrittenByUser is the regression for
// the cross-interest alive-coalescing loss (3/3-reviewer Require Change):
// user sessions bind the system-alive routing key, so a broker alive fans
// out to the system session AND every user session near-simultaneously.
// Only SystemAliveOnly alives drive the recovery state machine
// (lastSystemAlive). Pre-fix a single pending slot let a user-session
// alive, stored before the actor drained, OVERWRITE the unprocessed
// system alive — lastSystemAlive stayed nil and repeated losses falsely
// flagged the producer down. With per-interest slots, draining processes
// BOTH.
func TestActor_EnqueueAlive_SystemNotOverwrittenByUser(t *testing.T) {
	srv, _ := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, newFakeManagerOps())

	sysRecv := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	userCreated := time.Date(2026, 1, 15, 12, 0, 5, 0, time.UTC)

	// System alive enqueued first; a user-session alive arrives before the
	// actor drains and lands in a SEPARATE slot.
	a.enqueueAlive(evAlive{
		timestamp:       types.MessageTimestamp{Created: sysRecv, Received: sysRecv},
		isSubscribed:    true,
		messageInterest: types.SystemAliveOnly,
	})
	a.enqueueAlive(evAlive{
		timestamp:       types.MessageTimestamp{Created: userCreated},
		isSubscribed:    true,
		messageInterest: types.AllMessageInterest, // non-system → user session
	})

	a.drainPendingAlive()

	if a.lastSystemAlive == nil {
		t.Fatal("system alive was lost — a user-session alive overwrote it in the pending slot")
	}
	if !a.lastSystemAlive.Equal(sysRecv) {
		t.Errorf("lastSystemAlive = %v, want %v", *a.lastSystemAlive, sysRecv)
	}
	if !a.lastUserSessionAlive.Equal(userCreated) {
		t.Errorf("lastUserSessionAlive = %v, want %v", a.lastUserSessionAlive, userCreated)
	}
}

func TestActor_OnAlive_DisabledProducerNoOp(t *testing.T) {
	srv, _ := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, newFakeManagerOps())
	if err := a.pm.SetProducerState(t.Context(), 1, false); err != nil {
		t.Fatalf("SetProducerState: %v", err)
	}

	now := time.Now()
	a.onAlive(evAlive{
		timestamp:    types.MessageTimestamp{Created: now},
		isSubscribed: true,
	})
	if !a.lastUserSessionAlive.IsZero() {
		t.Errorf("disabled producer should not update timestamp, got %v", a.lastUserSessionAlive)
	}
}

// --- onSnapshotComplete ---

func TestActor_OnSnapshotComplete_UnknownRequestID(t *testing.T) {
	srv, _ := fixtureSrv(t)
	defer srv.Close()
	fake := newFakeManagerOps()
	a := newWiredActor(t, srv, fake)
	// No registered recoveries — request 99 is unknown.
	a.onSnapshotComplete(evSnapshotComplete{requestID: 99, messageInterest: types.AllMessageInterest})
	// Nothing should be emitted.
	if len(fake.emittedMsgs) != 0 {
		t.Errorf("unknown snapshot should not emit, got %d", len(fake.emittedMsgs))
	}
}

func TestActor_OnSnapshotComplete_DisabledProducerLogsAndReturns(t *testing.T) {
	srv, _ := fixtureSrv(t)
	defer srv.Close()
	fake := newFakeManagerOps()
	a := newWiredActor(t, srv, fake)
	if err := a.pm.SetProducerState(t.Context(), 1, false); err != nil {
		t.Fatalf("SetProducerState: %v", err)
	}
	a.onSnapshotComplete(evSnapshotComplete{requestID: 7})
	if len(fake.emittedMsgs) != 0 {
		t.Errorf("disabled producer should not emit on snapshot, got %d", len(fake.emittedMsgs))
	}
}

// --- calculateTiming ---

func TestActor_CalculateTiming(t *testing.T) {
	srv, _ := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, newFakeManagerOps())

	now := time.Now()
	// Set both timestamps to "now-ish": calculateTiming should return true
	// because both are within MaxInactivity.
	a.lastUserSessionAlive = now
	if err := a.pm.SetLastProcessedMessageGenTimestamp(1, now); err != nil {
		t.Fatalf("SetLastProcessedMessageGenTimestamp: %v", err)
	}
	if !a.calculateTiming(now) {
		t.Error("expected timing to be ok")
	}

	// Now put the user session alive far in the past — should fail.
	a.lastUserSessionAlive = now.Add(-time.Hour)
	if a.calculateTiming(now) {
		t.Error("expected timing to fail with stale user alive")
	}
}

// --- producerDown / producerUp / notifyProducerChangedState ---

func TestActor_ProducerDown_AndUp(t *testing.T) {
	srv, _ := fixtureSrv(t)
	defer srv.Close()
	fake := newFakeManagerOps()
	a := newWiredActor(t, srv, fake)

	// Producer starts flagged down (newData defaults). producerUp should
	// flip it via SetProducerDown(false) and emit.
	if err := a.producerUp(types.FirstRecoveryCompletedProducerUpReason); err != nil {
		t.Fatalf("producerUp: %v", err)
	}
	if len(fake.emittedMsgs) == 0 {
		t.Error("producerUp should emit a status message")
	}
	prod, _ := a.pm.GetProducer(t.Context(), 1)
	if prod.IsFlaggedDown() {
		t.Error("after producerUp, IsFlaggedDown should be false")
	}

	// Now down again with a reason — should re-emit (different status reason).
	prevEmissions := len(fake.emittedMsgs)
	if err := a.producerDown(types.AliveInternalViolationProducerDownReason); err != nil {
		t.Fatalf("producerDown: %v", err)
	}
	if len(fake.emittedMsgs) == prevEmissions {
		t.Error("producerDown should emit a new status (different reason)")
	}
	prod, _ = a.pm.GetProducer(t.Context(), 1)
	if !prod.IsFlaggedDown() {
		t.Error("after producerDown, IsFlaggedDown should be true")
	}
}

func TestActor_ProducerDown_DisabledIsNoOp(t *testing.T) {
	srv, _ := fixtureSrv(t)
	defer srv.Close()
	fake := newFakeManagerOps()
	a := newWiredActor(t, srv, fake)
	if err := a.pm.SetProducerState(t.Context(), 1, false); err != nil {
		t.Fatalf("SetProducerState: %v", err)
	}
	if err := a.producerDown(types.OtherProducerDownReason); err != nil {
		t.Fatalf("producerDown: %v", err)
	}
	if len(fake.emittedMsgs) != 0 {
		t.Errorf("disabled producer should be a no-op, got %d emissions", len(fake.emittedMsgs))
	}
}

func TestActor_NotifyProducerChangedState_DedupesSameReason(t *testing.T) {
	srv, _ := fixtureSrv(t)
	defer srv.Close()
	fake := newFakeManagerOps()
	a := newWiredActor(t, srv, fake)

	// First call: status reason changes from default → emits.
	if err := a.notifyProducerChangedState(types.AliveIntervalViolationProducerStatusReason); err != nil {
		t.Fatalf("first notify: %v", err)
	}
	if len(fake.emittedMsgs) != 1 {
		t.Errorf("first notify should emit, got %d", len(fake.emittedMsgs))
	}
	// Second call with same reason: no change → no emission.
	if err := a.notifyProducerChangedState(types.AliveIntervalViolationProducerStatusReason); err != nil {
		t.Fatalf("second notify: %v", err)
	}
	if len(fake.emittedMsgs) != 1 {
		t.Errorf("second notify with same reason should be deduped, got %d", len(fake.emittedMsgs))
	}
}

// --- onTick ---

func TestActor_OnTick_DisabledIsNoOp(t *testing.T) {
	srv, _ := fixtureSrv(t)
	defer srv.Close()
	fake := newFakeManagerOps()
	a := newWiredActor(t, srv, fake)
	if err := a.pm.SetProducerState(t.Context(), 1, false); err != nil {
		t.Fatalf("SetProducerState: %v", err)
	}

	a.onTick(time.Now(), true)
	if len(fake.emittedMsgs) != 0 {
		t.Errorf("tick on disabled producer should be no-op, got %d emissions", len(fake.emittedMsgs))
	}
}

func TestActor_OnTick_NoLastSystemAliveFlagsDown(t *testing.T) {
	srv, _ := fixtureSrv(t)
	defer srv.Close()
	fake := newFakeManagerOps()
	a := newWiredActor(t, srv, fake)

	// lastSystemAlive is nil → aliveInterval is huge → flagged down via
	// AliveInternalViolation.
	a.onTick(time.Now(), true)
	prod, _ := a.pm.GetProducer(t.Context(), 1)
	if !prod.IsFlaggedDown() {
		t.Error("tick with no alive should flag the producer down")
	}
}

// --- onRecoverEvent ---

func TestActor_OnRecoverEvent_HappyPath(t *testing.T) {
	srv, _ := fixtureSrv(t)
	defer srv.Close()
	fake := newFakeManagerOps()
	a := newWiredActor(t, srv, fake)

	urn, _ := types.ParseURN("od:match:1")
	reply := make(chan recoverEventReply, 1)
	a.onRecoverEvent(evRecoverEvent{
		ctx:              t.Context(),
		eventID:          *urn,
		statefulRecovery: false,
		reply:            reply,
	})

	select {
	case r := <-reply:
		if r.err != nil {
			t.Fatalf("reply.err = %v", r.err)
		}
		if r.handle == nil {
			t.Fatal("reply.handle = nil")
		}
		if len(fake.registered) != 1 {
			t.Errorf("expected 1 registered handle, got %d", len(fake.registered))
		}
	case <-time.After(time.Second):
		t.Fatal("no reply within 1s")
	}
}

func TestActor_OnRecoverEvent_StatefulFlagSetsCorrectEndpoint(t *testing.T) {
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	fake := newFakeManagerOps()
	a := newWiredActor(t, srv, fake)

	urn, _ := types.ParseURN("od:match:1")
	reply := make(chan recoverEventReply, 1)
	a.onRecoverEvent(evRecoverEvent{
		ctx:              t.Context(),
		eventID:          *urn,
		statefulRecovery: true,
		reply:            reply,
	})
	<-reply

	// The API call now runs in a detached goroutine, so the endpoint
	// hit may not be visible immediately after the reply. Poll briefly.
	deadline := time.Now().Add(time.Second)
	for hits.statefulRecover.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if hits.statefulRecover.Load() == 0 {
		t.Error("expected stateful recovery endpoint to be hit")
	}
}

// --- M1: tick-drop counter + per-producer rate-limited warn ---
//
// Pre-fix the periodic tick fan-out silently dropped evTick events on
// full actor inboxes. recordTickDrop now bumps a counter and emits a
// rate-limited warn so operators can see sustained backpressure.
func TestManager_RecordTickDrop_CountsAndDedupesPerProducer(t *testing.T) {
	m := &Manager{
		logger:             newDiscardLogger(),
		tickDropLastWarnAt: make(map[int]time.Time),
	}

	// First drop for producer 1: counted + warn emitted (we can't
	// easily assert the warn fired with discardLogger; the behaviour
	// is exercised though — no panic, no race).
	m.recordTickDrop(1)
	if got := m.TickDropCount(); got != 1 {
		t.Errorf("TickDropCount after 1 drop = %d, want 1", got)
	}

	// Second drop for the same producer within the rate-limit window:
	// counted but warn suppressed (lastWarnAt unchanged within
	// tickDropWarnInterval).
	m.recordTickDrop(1)
	if got := m.TickDropCount(); got != 2 {
		t.Errorf("TickDropCount after 2 drops = %d, want 2", got)
	}

	// Different producer is independently tracked.
	m.recordTickDrop(2)
	if got := m.TickDropCount(); got != 3 {
		t.Errorf("TickDropCount after 3 drops = %d, want 3", got)
	}

	// Rewind producer 1's last-warn time to simulate elapsed window;
	// next drop should be eligible to emit a fresh warn.
	m.tickDropWarnMu.Lock()
	m.tickDropLastWarnAt[1] = time.Now().Add(-2 * tickDropWarnInterval)
	m.tickDropWarnMu.Unlock()

	m.recordTickDrop(1)
	if got := m.TickDropCount(); got != 4 {
		t.Errorf("TickDropCount after 4 drops = %d, want 4", got)
	}
	// Verify the producer's last-warn was advanced (proving the
	// rate-limit window check fired on the eligible branch).
	m.tickDropWarnMu.Lock()
	last := m.tickDropLastWarnAt[1]
	m.tickDropWarnMu.Unlock()
	if time.Since(last) > time.Second {
		t.Errorf("producer 1 last-warn timestamp not advanced (elapsed=%v)", time.Since(last))
	}
}

// --- isPerformingRecovery covers both Started and Interrupted ---
// (covered in actor_test.go; keep one quick sanity here)

func TestActor_IsPerformingRecovery_Sanity(t *testing.T) {
	a := &recoveryActor{}
	if a.isPerformingRecovery() {
		t.Error("default state shouldn't be performing recovery")
	}
	a.recoveryState = types.StartedRecoveryState
	if !a.isPerformingRecovery() {
		t.Error("Started should be performing recovery")
	}
	a.recoveryState = types.InterruptedRecoveryState
	if !a.isPerformingRecovery() {
		t.Error("Interrupted should be performing recovery")
	}
}

// --- systemAliveReceived ---

// TestActor_SystemAliveReceived_NotSubscribedDuringRecovery_TransitionsToInterrupted
// is the regression for the H1 finding: a stale subscribed=false alive
// arriving while a snapshot recovery is in flight previously called
// makeSnapshotRecovery unconditionally — overwriting currentRecovery
// and resetting recoveryState to Started. That orphaned the existing
// handle and double-issued the recovery.
//
// Strategy: pre-load the actor with an in-flight recovery (state=
// Started + non-nil currentRecovery + recorded request id), feed in
// subscribed=false alive, verify the existing recovery is preserved
// and recoveryState transitions to Interrupted (mirrors Java/.NET
// behavior — snapshotRecoveryFinished sees Interrupted and re-issues).
func TestActor_SystemAliveReceived_NotSubscribedDuringRecovery_TransitionsToInterrupted(t *testing.T) {
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	fake := newFakeManagerOps()
	a := newWiredActor(t, srv, fake)

	// Simulate an in-flight snapshot recovery.
	a.recoveryState = types.StartedRecoveryState
	originalRecovery := newRecoveryData(42, time.Now())
	a.currentRecovery = originalRecovery

	now := time.Now()
	if err := a.systemAliveReceived(types.MessageTimestamp{Received: now, Created: now}, false); err != nil {
		t.Fatalf("systemAliveReceived: %v", err)
	}

	// PostRecovery (initiate_request) must NOT have been hit a second
	// time — pre-fix we would have stomped the in-flight recovery and
	// fired a fresh one.
	if got := hits.recover.Load(); got != 0 {
		t.Errorf("recovery initiate hits = %d, want 0 (in-flight recovery must not be re-issued)", got)
	}
	// The original recovery handle should still be intact.
	if a.currentRecovery != originalRecovery {
		t.Errorf("currentRecovery was overwritten — H1 regression")
	}
	// State should now be Interrupted; snapshotRecoveryFinished's
	// completion path will re-issue when the original snapshot finishes.
	if a.recoveryState != types.InterruptedRecoveryState {
		t.Errorf("recoveryState = %v, want Interrupted", a.recoveryState)
	}
}

func TestActor_SystemAliveReceived_NotSubscribedTriggersRecovery(t *testing.T) {
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	fake := newFakeManagerOps()
	a := newWiredActor(t, srv, fake)

	now := time.Now()
	if err := a.systemAliveReceived(types.MessageTimestamp{Received: now, Created: now}, false); err != nil {
		t.Fatalf("systemAliveReceived: %v", err)
	}
	// Recovery state should be Started immediately (state mutation
	// is inline; the API call is detached).
	if a.recoveryState != types.StartedRecoveryState {
		t.Errorf("recoveryState = %v, want Started", a.recoveryState)
	}
	// PostRecovery is now detached — wait briefly for the goroutine
	// to fire the upstream call. v2.x restructure (parity with the
	// v2.24 detach-event-recovery work) moved the HTTP off the actor
	// goroutine so a slow upstream doesn't queue alives/ticks.
	waitFor(t, "recovery initiate hit", time.Second, func() bool {
		return hits.recover.Load() > 0
	})
}

func TestActor_SystemAliveReceived_SubscribedDefaultBranch(t *testing.T) {
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	fake := newFakeManagerOps()
	a := newWiredActor(t, srv, fake)

	// Initial state: NotStarted (state default), flagged-down (default
	// from producer.Manager). This falls into the default branch
	// → makeSnapshotRecovery.
	now := time.Now()
	if err := a.systemAliveReceived(types.MessageTimestamp{Received: now, Created: now}, true); err != nil {
		t.Fatalf("systemAliveReceived: %v", err)
	}
	// lastSystemAlive should be set immediately.
	if a.lastSystemAlive == nil {
		t.Error("lastSystemAlive should be populated")
	}
	// PostRecovery is detached — wait for the goroutine.
	waitFor(t, "default branch makeSnapshotRecovery hit", time.Second, func() bool {
		return hits.recover.Load() > 0
	})
}

// waitFor polls until cond returns true or the timeout elapses. Used by
// tests that need to observe the result of a detached goroutine
// (PostRecovery is now off the actor goroutine — see makeSnapshotRecovery).
func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("timeout waiting for: %s", what)
}

// TestActor_MakeSnapshotRecovery_DetachedAPI_DoesNotBlockActor is the
// regression for the v2.x architectural fix: pre-fix makeSnapshotRecovery
// ran PostRecovery inline on the actor goroutine, so a slow recovery
// API call queued every alive/tick/snapshot-complete behind it. Now
// the API call is detached (mirrors the v2.24 event-recovery fix); the
// actor stays responsive while PostRecovery is in flight.
//
// Strategy: hang the recovery initiate handler. Trigger a snapshot
// recovery (sets state=Started, spawns the detached API goroutine,
// returns). Then call onMessageProcessingStarted on the actor — this
// is a synchronous handler that mutates state. Pre-fix it would block
// behind the inline HTTP call; post-fix it returns immediately. We
// also assert the recovery state transitioned to Started before the
// API call was even attempted (i.e., the state mutation is inline,
// the network call is detached).
func TestActor_MakeSnapshotRecovery_DetachedAPI_DoesNotBlockActor(t *testing.T) {
	apiHit := make(chan struct{}, 1)
	releaseAPI := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		switch {
		case strings.HasSuffix(r.URL.Path, "/descriptions/producers"):
			_, _ = io.WriteString(w, producersBody)
		case strings.Contains(r.URL.Path, "/recovery/initiate_request"):
			select {
			case apiHit <- struct{}{}:
			default:
			}
			// Block until the test releases — simulates a slow
			// recovery initiate. Pre-fix this hung the actor.
			<-releaseAPI
			_, _ = io.WriteString(w, `<?xml version="1.0"?><response response_code="OK"/>`)
		default:
			_, _ = io.WriteString(w, `<?xml version="1.0"?><response response_code="OK"/>`)
		}
	}))
	defer srv.Close()
	defer close(releaseAPI) // ensure goroutine exits on test return

	fake := newFakeManagerOps()
	a := newWiredActor(t, srv, fake)

	// Trigger snapshot recovery. Pre-fix: blocks here for the full
	// HTTP duration. Post-fix: returns within microseconds — only
	// state mutation runs inline.
	start := time.Now()
	if err := a.makeSnapshotRecovery(time.Now()); err != nil {
		t.Fatalf("makeSnapshotRecovery: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("makeSnapshotRecovery blocked for %v — should be near-instant (API call is detached)", elapsed)
	}

	// State must be Started immediately, before the API call returns.
	if a.recoveryState != types.StartedRecoveryState {
		t.Errorf("recoveryState = %v, want Started (state mutation is inline)", a.recoveryState)
	}
	if a.currentRecovery == nil {
		t.Error("currentRecovery = nil; expected the just-allocated recovery data")
	}

	// The actor must remain responsive: another handler runs
	// synchronously without waiting for the in-flight API call.
	syncStart := time.Now()
	a.onMessageProcessingStarted(time.Now())
	if elapsed := time.Since(syncStart); elapsed > 100*time.Millisecond {
		t.Errorf("onMessageProcessingStarted blocked %v — actor goroutine still busy with detached API call", elapsed)
	}

	// Wait for the detached goroutine to actually fire the upstream
	// hit, then release. (Sanity: confirms the API call really did
	// happen, just off the actor goroutine.)
	select {
	case <-apiHit:
	case <-time.After(2 * time.Second):
		t.Fatal("recovery initiate endpoint never hit by detached goroutine")
	}
}

// --- snapshotRecoveryFinished ---

func TestActor_SnapshotRecoveryFinished_TransitionsToCompleted(t *testing.T) {
	srv, _ := fixtureSrv(t)
	defer srv.Close()
	fake := newFakeManagerOps()
	a := newWiredActor(t, srv, fake)

	// Set up a recovery that's "in progress".
	a.recoveryState = types.StartedRecoveryState
	a.currentRecovery = newRecoveryData(42, time.Now().Add(-time.Minute))

	if err := a.snapshotRecoveryFinished(42); err != nil {
		t.Fatalf("snapshotRecoveryFinished: %v", err)
	}
	if a.recoveryState != types.CompletedRecoveryState {
		t.Errorf("recoveryState = %v, want Completed", a.recoveryState)
	}
	if !a.firstRecoveryCompleted {
		t.Error("firstRecoveryCompleted should be true after snapshot finish")
	}
	// Status emission for producer up.
	if len(fake.emittedMsgs) == 0 {
		t.Error("expected producer-up emission")
	}
}

// TestActor_SnapshotRecoveryFinished_InterruptedReissuesAndPreservesNewState
// is the regression for the v2.x review-pass finding: pre-fix the
// Interrupted re-issue path called makeSnapshotRecovery (which sets
// currentRecovery=NEW_recovery + recoveryState=Started) and then
// continued past the if-block, OVERWRITING currentRecovery to the
// just-finished OLD recovery's id and state to Completed. The
// re-issued recovery's eventual snapshot_complete would then arrive
// with a requestID that no longer matched currentRecovery and be
// silently ignored — the new recovery hung forever.
//
// Strategy: pre-load actor with state=Interrupted + currentRecovery
// pointing to a "finished" recovery. Call snapshotRecoveryFinished.
// Verify (a) recoveryState transitioned to Started (NOT Completed),
// (b) currentRecovery now points to a NEW recovery with a NEW
// requestID (NOT the original 42), and (c) no producerUp emission
// (we're still recovering, not done).
func TestActor_SnapshotRecoveryFinished_InterruptedReissuesAndPreservesNewState(t *testing.T) {
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	fake := newFakeManagerOps()
	a := newWiredActor(t, srv, fake)

	// Set up the just-finished original recovery.
	const oldRequestID int = 42
	a.recoveryState = types.InterruptedRecoveryState
	a.currentRecovery = newRecoveryData(oldRequestID, time.Now().Add(-time.Minute))
	a.lastValidAliveGen = time.Now()

	if err := a.snapshotRecoveryFinished(oldRequestID); err != nil {
		t.Fatalf("snapshotRecoveryFinished: %v", err)
	}

	// Recovery state must be Started — the re-issue is in flight.
	if a.recoveryState != types.StartedRecoveryState {
		t.Errorf("recoveryState = %v, want Started (re-issue should not be marked Completed)", a.recoveryState)
	}
	// currentRecovery must point to the NEW recovery, not the old.
	if a.currentRecovery == nil {
		t.Fatal("currentRecovery is nil; expected new recovery data after re-issue")
	}
	if a.currentRecovery.recoveryID == oldRequestID {
		t.Errorf("currentRecovery.recoveryID = %d (still the old id); expected a fresh requestID from the re-issue", oldRequestID)
	}
	// No producerUp emission — we're still recovering. Pre-fix the
	// continuation called producerUp inappropriately.
	for _, msg := range fake.emittedMsgs {
		if msg.ProducerStatus != nil && !msg.ProducerStatus.IsDown() {
			t.Errorf("unexpected producerUp emission during interrupted re-issue: %+v", msg.ProducerStatus)
		}
	}
	// Detached PostRecovery should fire (eventually) for the new request.
	waitFor(t, "re-issued recovery PostRecovery hit", time.Second, func() bool {
		return hits.recover.Load() > 0
	})
}

// TestActor_OnSnapshotRecoveryAPICompleted_ErrorTransitionsToErrorState
// is the regression for the second v2.x review-pass finding: pre-fix
// onSnapshotRecoveryAPICompleted on API error logged and returned
// without rolling back state. The actor stayed in StartedRecoveryState
// forever — no snapshot_complete would ever arrive (the recovery never
// started on the upstream side), and the next tick/alive couldn't
// re-issue because isPerformingRecovery() was still true.
//
// Strategy: invoke the handler with err set; verify state transitions
// to ErrorRecoveryState and currentRecovery is cleared.
func TestActor_OnSnapshotRecoveryAPICompleted_ErrorTransitionsToErrorState(t *testing.T) {
	srv, _ := fixtureSrv(t)
	defer srv.Close()
	fake := newFakeManagerOps()
	a := newWiredActor(t, srv, fake)

	// Pre-state: recovery just initiated, awaiting API completion.
	const reqID int = 99
	a.recoveryState = types.StartedRecoveryState
	a.currentRecovery = newRecoveryData(reqID, time.Now())

	a.onSnapshotRecoveryAPICompleted(evSnapshotRecoveryAPICompleted{
		requestID: reqID,
		err:       errors.New("network error"),
	})

	if a.recoveryState != types.ErrorRecoveryState {
		t.Errorf("recoveryState = %v, want Error", a.recoveryState)
	}
	if a.currentRecovery != nil {
		t.Errorf("currentRecovery = %v, want nil after API failure", a.currentRecovery)
	}
	// Actor is now NOT performing recovery — next alive can re-issue.
	if a.isPerformingRecovery() {
		t.Error("isPerformingRecovery() should be false after Error transition")
	}
}

// TestActor_OnSnapshotRecoveryAPICompleted_LateErrorAfterCompletionDropped
// guards the same-requestID ordering hazard: PostRecovery is idempotent,
// so a retried call can lose the race against the AMQP snapshot the
// server already accepted for attempt 1 — snapshot_complete lands,
// snapshotRecoveryFinished re-creates currentRecovery with the SAME
// requestID and sets CompletedRecoveryState, and only then does the
// detached call deliver its late transport error. The recoveryID-only
// stale guard passes; pre-fix the error branch then nil'd
// currentRecovery and rolled a healthy, completed producer back to
// ErrorRecoveryState, forcing a redundant full re-recovery.
func TestActor_OnSnapshotRecoveryAPICompleted_LateErrorAfterCompletionDropped(t *testing.T) {
	srv, _ := fixtureSrv(t)
	defer srv.Close()
	fake := newFakeManagerOps()
	a := newWiredActor(t, srv, fake)

	// Recovery already completed on the wire: snapshotRecoveryFinished
	// re-created currentRecovery with the same requestID.
	const reqID int = 77
	a.recoveryState = types.CompletedRecoveryState
	a.currentRecovery = newRecoveryData(reqID, time.Now().Add(-time.Minute))
	a.firstRecoveryCompleted = true

	// Late API error for the SAME requestID — must be dropped.
	a.onSnapshotRecoveryAPICompleted(evSnapshotRecoveryAPICompleted{
		requestID: reqID,
		err:       errors.New("late transport error"),
	})

	if a.recoveryState != types.CompletedRecoveryState {
		t.Errorf("recoveryState = %v, want Completed (late error must not roll back a settled recovery)", a.recoveryState)
	}
	if a.currentRecovery == nil || a.currentRecovery.recoveryID != reqID {
		t.Errorf("currentRecovery = %v, want unchanged with id=%d", a.currentRecovery, reqID)
	}
}

// TestActor_OnSnapshotRecoveryAPICompleted_StaleEventIgnored guards the
// re-entrant case: if makeSnapshotRecovery was called again (e.g., the
// Interrupted re-issue path) between sending and receiving the API
// completion event, currentRecovery has rotated to the new requestID.
// The stale completion must NOT clobber the new recovery's state.
func TestActor_OnSnapshotRecoveryAPICompleted_StaleEventIgnored(t *testing.T) {
	srv, _ := fixtureSrv(t)
	defer srv.Close()
	fake := newFakeManagerOps()
	a := newWiredActor(t, srv, fake)

	const newReqID int = 200
	a.recoveryState = types.StartedRecoveryState
	a.currentRecovery = newRecoveryData(newReqID, time.Now())

	// Stale event for an OLD requestID — must be ignored.
	a.onSnapshotRecoveryAPICompleted(evSnapshotRecoveryAPICompleted{
		requestID: 100, // stale
		err:       errors.New("network error"),
	})

	if a.recoveryState != types.StartedRecoveryState {
		t.Errorf("recoveryState = %v, want Started (stale event must not transition)", a.recoveryState)
	}
	if a.currentRecovery == nil || a.currentRecovery.recoveryID != newReqID {
		t.Errorf("currentRecovery = %v, want unchanged with id=%d", a.currentRecovery, newReqID)
	}
}

// --- eventRecoveryFinished ---

func TestActor_EventRecoveryFinished_EmitsEventRecoveryAndCompletesHandle(t *testing.T) {
	srv, _ := fixtureSrv(t)
	defer srv.Close()
	fake := newFakeManagerOps()
	a := newWiredActor(t, srv, fake)

	urn, _ := types.ParseURN("od:match:1")
	a.eventRecoveries[7] = newEventRecovery(*urn, 7, time.Now().Add(-time.Second))

	if err := a.eventRecoveryFinished(7); err != nil {
		t.Fatalf("eventRecoveryFinished: %v", err)
	}
	// Emitted event recovery message.
	if len(fake.emittedMsgs) == 0 || fake.emittedMsgs[0].EventRecoveryMessage == nil {
		t.Errorf("expected EventRecoveryMessage emission, got %+v", fake.emittedMsgs)
	}
	// Handle marked complete.
	if len(fake.completed) == 0 || fake.completed[0].id != 7 {
		t.Errorf("expected handle 7 completed, got %+v", fake.completed)
	}
	// Recovery removed from map.
	if _, ok := a.eventRecoveries[7]; ok {
		t.Error("event recovery 7 should be removed after finish")
	}
}

func TestActor_EventRecoveryFinished_UnknownIDError(t *testing.T) {
	srv, _ := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, newFakeManagerOps())

	if err := a.eventRecoveryFinished(99); err == nil {
		t.Error("eventRecoveryFinished on unknown id should error")
	}
}

// --- validateProducerSnapshotCompletes ---

func TestActor_ValidateProducerSnapshotCompletes(t *testing.T) {
	srv, _ := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, newFakeManagerOps())

	// Producer 1 has live scope only. A LiveOnly snapshot complete fully
	// validates.
	ok, err := a.validateProducerSnapshotCompletes([]types.MessageInterest{
		types.LiveOnlyMessageInterest,
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !ok {
		t.Error("live-only producer with live snapshot complete should validate")
	}

	// Empty list: not validated.
	ok, _ = a.validateProducerSnapshotCompletes([]types.MessageInterest{})
	if ok {
		t.Error("empty completes list should not validate")
	}

	// Mismatch: prematch interest on a live-only producer.
	ok, _ = a.validateProducerSnapshotCompletes([]types.MessageInterest{
		types.PrematchOnlyMessageInterest,
	})
	if ok {
		t.Error("prematch interest on live-only producer should not validate")
	}
}

// TestActor_ValidateProducerSnapshotCompletes_MixedScope is the
// regression for the v2.25 finding: a producer with both Live and
// Prematch scopes must validate when both LiveOnly and PrematchOnly
// snapshot completes are received. Pre-fix the inner loop overwrote
// finished[i] on each iteration of `received`, so the second locale
// always cleared the first scope's match — validation could never
// succeed and event recoveries hung.
func TestActor_ValidateProducerSnapshotCompletes_MixedScope(t *testing.T) {
	srv, _ := fixtureSrv(t)
	defer srv.Close()
	// Producer 3 in the fixture has scope="live|prematch".
	a := newWiredActorForProducer(t, srv, newFakeManagerOps(), 3)

	// Both scopes received → validates.
	ok, err := a.validateProducerSnapshotCompletes([]types.MessageInterest{
		types.LiveOnlyMessageInterest,
		types.PrematchOnlyMessageInterest,
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !ok {
		t.Error("mixed-scope producer with both interests should validate")
	}

	// Reverse order — set membership, not positional.
	ok, _ = a.validateProducerSnapshotCompletes([]types.MessageInterest{
		types.PrematchOnlyMessageInterest,
		types.LiveOnlyMessageInterest,
	})
	if !ok {
		t.Error("mixed-scope producer must validate regardless of completion order")
	}

	// Only one scope received → not yet validated.
	ok, _ = a.validateProducerSnapshotCompletes([]types.MessageInterest{
		types.LiveOnlyMessageInterest,
	})
	if ok {
		t.Error("mixed-scope producer with only Live complete should NOT validate")
	}
	ok, _ = a.validateProducerSnapshotCompletes([]types.MessageInterest{
		types.PrematchOnlyMessageInterest,
	})
	if ok {
		t.Error("mixed-scope producer with only Prematch complete should NOT validate")
	}
}

// --- ProducerName error path ---

func TestActor_ProducerName(t *testing.T) {
	srv, _ := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, newFakeManagerOps())

	name, err := a.producerName()
	if err != nil {
		t.Fatalf("producerName: %v", err)
	}
	if name != "live" {
		t.Errorf("got %q, want live", name)
	}
}

// --- TimestampForRecovery ---

func TestActor_TimestampForRecovery(t *testing.T) {
	srv, _ := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, newFakeManagerOps())

	// Default: zero (no alive received yet).
	got, err := a.timestampForRecovery()
	if err != nil {
		t.Fatalf("timestampForRecovery: %v", err)
	}
	if !got.IsZero() {
		t.Errorf("default timestamp = %v, want zero", got)
	}

	// After SetLastAliveReceivedGenTimestamp: returns that.
	moment := time.Now().Add(-30 * time.Minute)
	if err := a.pm.SetLastAliveReceivedGenTimestamp(1, moment); err != nil {
		t.Fatalf("SetLastAliveReceivedGenTimestamp: %v", err)
	}
	got, _ = a.timestampForRecovery()
	if !got.Equal(moment) {
		t.Errorf("after alive timestamp: got %v, want %v", got, moment)
	}
}

// TestActor_OnRecoverEvent_CoalescesIdenticalPending is the regression
// for recovery fan-out (Codex P2): repeated submissions of the SAME
// (event, kind) while one is already in flight must return the existing
// handle — pre-fix every call created a fresh handle + map entry +
// detached POST goroutine with no bound (the 256-slot inbox bounds
// nothing because commands dequeue quickly).
func TestActor_OnRecoverEvent_CoalescesIdenticalPending(t *testing.T) {
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	fake := newFakeManagerOps()
	a := newWiredActor(t, srv, fake)

	urn, _ := types.ParseURN("od:match:1")
	submit := func(stateful bool) *Handle {
		reply := make(chan recoverEventReply, 1)
		a.onRecoverEvent(evRecoverEvent{ctx: t.Context(), eventID: *urn, statefulRecovery: stateful, reply: reply})
		r := <-reply
		if r.err != nil {
			t.Fatalf("reply.err = %v", r.err)
		}
		return r.handle
	}

	h1 := submit(false)
	h2 := submit(false)
	if h1 != h2 {
		t.Fatalf("identical pending requests got DIFFERENT handles (%d vs %d) — no coalescing", h1.RequestID(), h2.RequestID())
	}
	if len(a.eventRecoveries) != 1 {
		t.Fatalf("eventRecoveries = %d, want 1 (coalesced)", len(a.eventRecoveries))
	}

	// A DIFFERENT kind for the same event is a separate operation.
	h3 := submit(true)
	if h3 == h1 {
		t.Fatal("stateful request coalesced onto the odds-only handle")
	}
	if len(a.eventRecoveries) != 2 {
		t.Fatalf("eventRecoveries = %d, want 2 (per kind)", len(a.eventRecoveries))
	}
	_ = hits
}

// TestActor_OnRecoverEvent_BoundedPendingAdmission pins the per-producer
// cap: distinct-event floods are rejected with
// ErrTooManyPendingEventRecoveries instead of growing pending state
// until the MaxRecoveryExecution sweep hours later.
func TestActor_OnRecoverEvent_BoundedPendingAdmission(t *testing.T) {
	srv, _ := fixtureSrv(t)
	defer srv.Close()
	fake := newFakeManagerOps()
	a := newWiredActor(t, srv, fake)

	for i := 0; i < maxPendingEventRecoveries; i++ {
		reply := make(chan recoverEventReply, 1)
		a.onRecoverEvent(evRecoverEvent{
			ctx:     t.Context(),
			eventID: types.URN{Prefix: "od", Type: "match", ID: 1000 + i},
			reply:   reply,
		})
		if r := <-reply; r.err != nil {
			t.Fatalf("request %d rejected below the cap: %v", i, r.err)
		}
	}

	reply := make(chan recoverEventReply, 1)
	a.onRecoverEvent(evRecoverEvent{
		ctx:     t.Context(),
		eventID: types.URN{Prefix: "od", Type: "match", ID: 999999},
		reply:   reply,
	})
	r := <-reply
	if !errors.Is(r.err, ErrTooManyPendingEventRecoveries) {
		t.Fatalf("over-cap request err = %v, want ErrTooManyPendingEventRecoveries", r.err)
	}
	if len(a.eventRecoveries) != maxPendingEventRecoveries {
		t.Fatalf("eventRecoveries = %d, want exactly %d", len(a.eventRecoveries), maxPendingEventRecoveries)
	}
}

// TestActor_ProducerDown_CancelsInflightRecoveryPOSTs is the regression
// for the concurrency-cap bypass (Codex P2): failAllEventRecoveries
// cleared the pending map (freeing all admission slots) but left the
// detached recovery POSTs running, so a producer-down/flap freed 128
// slots while 128 POSTs were still in flight — the documented per-
// producer cap could be exceeded. The transition must CANCEL each POST's
// context so it terminates before the next batch can overlap it.
func TestActor_ProducerDown_CancelsInflightRecoveryPOSTs(t *testing.T) {
	postCtxDone := make(chan struct{}, 4)
	postStarted := make(chan struct{}, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		switch {
		case strings.HasSuffix(r.URL.Path, "/descriptions/producers"):
			_, _ = io.WriteString(w, producersBody)
		case strings.Contains(r.URL.Path, "/odds/events/"):
			// Block until the request context is cancelled — that is the
			// signal that failAllEventRecoveries invoked cancelAPI.
			select {
			case postStarted <- struct{}{}:
			default:
			}
			<-r.Context().Done()
			postCtxDone <- struct{}{}
			http.Error(w, "cancelled", http.StatusGatewayTimeout)
		default:
			_, _ = io.WriteString(w, `<?xml version="1.0"?><response response_code="OK"/>`)
		}
	}))
	defer srv.Close()

	fake := newFakeManagerOps()
	a := newWiredActor(t, srv, fake)

	urn, _ := types.ParseURN("od:match:1")
	reply := make(chan recoverEventReply, 1)
	a.onRecoverEvent(evRecoverEvent{ctx: t.Context(), eventID: *urn, reply: reply})
	if r := <-reply; r.err != nil {
		t.Fatalf("recover reply err: %v", r.err)
	}
	select {
	case <-postStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("recovery POST never reached the server")
	}

	// Producer-down clears the pending map — it MUST cancel the in-flight POST.
	if err := a.producerDown(types.AliveInternalViolationProducerDownReason); err != nil {
		t.Fatalf("producerDown: %v", err)
	}
	if len(a.eventRecoveries) != 0 {
		t.Fatalf("eventRecoveries = %d after producerDown, want 0", len(a.eventRecoveries))
	}
	select {
	case <-postCtxDone:
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight recovery POST was not cancelled by producerDown — cap bypass")
	}
}
