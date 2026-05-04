package recovery

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/producer"
	"github.com/oddin-gg/gosdk/types"
)

// minimalCfg satisfies types.OddsFeedConfiguration.
type minimalCfg struct {
	apiURL string
	token  string
}

func (c *minimalCfg) AccessToken() *string                                       { return &c.token }
func (c *minimalCfg) DefaultLocale() types.Locale                            { return types.EnLocale }
func (c *minimalCfg) MaxInactivitySeconds() int                                  { return 20 }
func (c *minimalCfg) MaxRecoveryExecutionMinutes() int                           { return 360 }
func (c *minimalCfg) MessagingPort() int                                         { return 5672 }
func (c *minimalCfg) SdkNodeID() *int                                            { return nil }
func (c *minimalCfg) SelectedEnvironment() *types.Environment                { return nil }
func (c *minimalCfg) SelectedRegion() types.Region                           { return types.RegionDefault }
func (c *minimalCfg) SetRegion(types.Region) types.OddsFeedConfiguration { return c }
func (c *minimalCfg) ExchangeName() string                                       { return "oddinfeed" }
func (c *minimalCfg) ReplayExchangeName() string                                 { return "oddinreplay" }
func (c *minimalCfg) ReportExtendedData() bool                                   { return false }
func (c *minimalCfg) SetExchangeName(string) types.OddsFeedConfiguration     { return c }
func (c *minimalCfg) SetAPIURL(string) types.OddsFeedConfiguration           { return c }
func (c *minimalCfg) SetMQURL(string) types.OddsFeedConfiguration            { return c }
func (c *minimalCfg) SetMessagingPort(int) types.OddsFeedConfiguration       { return c }
func (c *minimalCfg) APIURL() (string, error)                                    { return c.apiURL, nil }
func (c *minimalCfg) MQURL() (string, error)                                     { return "", nil }
func (c *minimalCfg) SportIDPrefix() string                                      { return "od:sport:" }
func (c *minimalCfg) SetSportIDPrefix(string) types.OddsFeedConfiguration    { return c }

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
			hits.recover++
			_, _ = io.WriteString(w, `<?xml version="1.0"?><response response_code="OK"/>`)
		case strings.Contains(r.URL.Path, "/odds/events/"):
			hits.eventRecover++
			_, _ = io.WriteString(w, `<?xml version="1.0"?><response response_code="OK"/>`)
		case strings.Contains(r.URL.Path, "/stateful_messages/events/"):
			hits.statefulRecover++
			_, _ = io.WriteString(w, `<?xml version="1.0"?><response response_code="OK"/>`)
		default:
			_, _ = io.WriteString(w, `<?xml version="1.0"?><response response_code="OK"/>`)
		}
	}))
	return srv, hits
}

type recoveryHits struct {
	recover         int
	eventRecover    int
	statefulRecover int
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
	if err := mgr.Open(context.Background()); err != nil {
		t.Fatalf("producer manager Open: %v", err)
	}
	return mgr
}

// newWiredActor builds an actor with a real producer.Manager (httptest
// backed) and a fake managerOps to capture emissions.
func newWiredActor(t *testing.T, srv *httptest.Server, fake *fakeManagerOps) *recoveryActor {
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
	a := newRecoveryActor(context.Background(), 1, cfg, apiClient, pm, fake, newDiscardLogger(), 32)
	return a
}

// --- onMessageProcessingStarted / Ended ---

func TestActor_OnMessageProcessingStarted_Records(t *testing.T) {
	srv, _ := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, newFakeManagerOps())

	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	a.onMessageProcessingStarted(now)

	prod, _ := a.pm.GetProducer(context.Background(), 1)
	if !prod.LastMessageTimestamp().Equal(now) {
		t.Errorf("LastMessageTimestamp = %v, want %v", prod.LastMessageTimestamp(), now)
	}
}

func TestActor_OnMessageProcessingStarted_IgnoresZero(t *testing.T) {
	srv, _ := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, newFakeManagerOps())

	a.onMessageProcessingStarted(time.Time{})
	prod, _ := a.pm.GetProducer(context.Background(), 1)
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
	prod, _ := a.pm.GetProducer(context.Background(), 1)
	if !prod.LastProcessedMessageGenTimestamp().Equal(now) {
		t.Errorf("LastProcessedMessageGenTimestamp = %v", prod.LastProcessedMessageGenTimestamp())
	}
	// Zero-timestamp is a no-op.
	prev := prod.LastProcessedMessageGenTimestamp()
	a.onMessageProcessingEnded(time.Time{})
	prod, _ = a.pm.GetProducer(context.Background(), 1)
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

func TestActor_OnAlive_DisabledProducerNoOp(t *testing.T) {
	srv, _ := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, newFakeManagerOps())
	if err := a.pm.SetProducerState(context.Background(), 1, false); err != nil {
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
	if err := a.pm.SetProducerState(context.Background(), 1, false); err != nil {
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
	// because both are within MaxInactivitySeconds.
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
	prod, _ := a.pm.GetProducer(context.Background(), 1)
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
	prod, _ = a.pm.GetProducer(context.Background(), 1)
	if !prod.IsFlaggedDown() {
		t.Error("after producerDown, IsFlaggedDown should be true")
	}
}

func TestActor_ProducerDown_DisabledIsNoOp(t *testing.T) {
	srv, _ := fixtureSrv(t)
	defer srv.Close()
	fake := newFakeManagerOps()
	a := newWiredActor(t, srv, fake)
	if err := a.pm.SetProducerState(context.Background(), 1, false); err != nil {
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
	if err := a.pm.SetProducerState(context.Background(), 1, false); err != nil {
		t.Fatalf("SetProducerState: %v", err)
	}

	a.onTick(time.Now())
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
	a.onTick(time.Now())
	prod, _ := a.pm.GetProducer(context.Background(), 1)
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
		ctx:              context.Background(),
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
		ctx:              context.Background(),
		eventID:          *urn,
		statefulRecovery: true,
		reply:            reply,
	})
	<-reply
	if hits.statefulRecover == 0 {
		t.Error("expected stateful recovery endpoint to be hit")
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

func TestActor_SystemAliveReceived_NotSubscribedTriggersRecovery(t *testing.T) {
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	fake := newFakeManagerOps()
	a := newWiredActor(t, srv, fake)

	now := time.Now()
	if err := a.systemAliveReceived(types.MessageTimestamp{Received: now, Created: now}, false); err != nil {
		t.Fatalf("systemAliveReceived: %v", err)
	}
	// Should have triggered recovery (PostRecovery endpoint hit).
	if hits.recover == 0 {
		t.Error("expected recovery initiate to be hit")
	}
	// Recovery state should be Started.
	if a.recoveryState != types.StartedRecoveryState {
		t.Errorf("recoveryState = %v, want Started", a.recoveryState)
	}
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
	if hits.recover == 0 {
		t.Error("expected default branch to call makeSnapshotRecovery")
	}
	// lastSystemAlive should be set.
	if a.lastSystemAlive == nil {
		t.Error("lastSystemAlive should be populated")
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
