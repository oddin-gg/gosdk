package recovery

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/oddin-gg/gosdk/internal/config"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

// newDiscardLogger builds a recovery logger that drops everything —
// keeps test output clean.
func newDiscardLogger() *log.Logger {
	return log.New(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// --- generator (unchanged from Phase 5b) ---

func TestGenerator_NextMonotonic(t *testing.T) {
	g := newGenerator(1)
	prev := g.next()
	for i := 0; i < 100; i++ {
		v := g.next()
		if v != prev+1 {
			t.Fatalf("non-monotonic: got %d after %d", v, prev)
		}
		prev = v
	}
}

func TestGenerator_NextConcurrentUnique(t *testing.T) {
	g := newGenerator(1)
	var seen sync.Map
	var wg sync.WaitGroup
	const goroutines = 16
	const perG = 256
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perG; j++ {
				v := g.next()
				if _, dup := seen.LoadOrStore(v, struct{}{}); dup {
					t.Errorf("duplicate id %d", v)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// --- recoveryData / eventRecovery (unchanged from Phase 5b) ---

func TestRecoveryData_SnapshotComplete_Accumulates(t *testing.T) {
	rd := newRecoveryData(42, time.Now())

	got := rd.snapshotComplete(types.LiveOnlyMessageInterest)
	if len(got) != 1 || got[0] != types.LiveOnlyMessageInterest {
		t.Errorf("first call = %v", got)
	}

	got = rd.snapshotComplete(types.PrematchOnlyMessageInterest)
	if len(got) != 2 {
		t.Errorf("second call = %v, want 2 entries", got)
	}

	// Idempotent: same interest doesn't grow the set.
	got = rd.snapshotComplete(types.LiveOnlyMessageInterest)
	if len(got) != 2 {
		t.Errorf("dup call = %v, want 2 entries", got)
	}
}

func TestRecoveryData_SnapshotComplete_RaceSafe(t *testing.T) {
	rd := newRecoveryData(1, time.Now())
	var wg sync.WaitGroup
	const goroutines = 32
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			interest := types.LiveOnlyMessageInterest
			if i%2 == 0 {
				interest = types.PrematchOnlyMessageInterest
			}
			_ = rd.snapshotComplete(interest)
		}(i)
	}
	wg.Wait()

	final := rd.snapshotComplete(types.LiveOnlyMessageInterest)
	if len(final) > 2 {
		t.Errorf("final size = %d, want <=2", len(final))
	}
}

func TestEventRecovery_CarriesEventID(t *testing.T) {
	urn, _ := types.ParseURN("od:match:42")
	er := newEventRecovery(*urn, 7, time.Now())
	if er.eventID != *urn {
		t.Errorf("eventID = %v, want %v", er.eventID, *urn)
	}
	if er.recoveryID != 7 {
		t.Errorf("recoveryID = %d, want 7", er.recoveryID)
	}
}

// --- recoveryActor (Phase 5 v2 actor model) ---
//
// These tests drive the actor through its inbox without a real
// producer.Manager / api.Client. We construct an actor with nil
// dependencies and exercise only the pure-state methods that don't
// touch them. State-machine flows that would call into the producer
// manager are tested via the dispatch path.

// fakeManagerOps captures actorManagerOps invocations so tests can
// observe what the actor emitted.
type fakeManagerOps struct {
	mu          sync.Mutex
	registered  []*Handle
	completed   []completedHandle
	nextID      atomic.Uint32
	emittedMsgs []types.RecoveryMessage
}

type completedHandle struct {
	id     int
	status types.RecoveryRequestStatus
	err    error
}

func newFakeManagerOps() *fakeManagerOps {
	f := &fakeManagerOps{}
	f.nextID.Store(0)
	return f
}

func (f *fakeManagerOps) registerHandle(h *Handle) {
	f.mu.Lock()
	f.registered = append(f.registered, h)
	f.mu.Unlock()
}

func (f *fakeManagerOps) completeHandle(id int, status types.RecoveryRequestStatus, err error) *Handle {
	f.mu.Lock()
	f.completed = append(f.completed, completedHandle{id: id, status: status, err: err})
	f.mu.Unlock()
	return nil
}

func (f *fakeManagerOps) nextRequestID() int {
	return int(f.nextID.Add(1))
}

func (f *fakeManagerOps) LookupHandle(id int) (*Handle, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, h := range f.registered {
		if h.RequestID() == id {
			return h, true
		}
	}
	return nil, false
}

func (f *fakeManagerOps) emitRecoveryMessage(msg types.RecoveryMessage) {
	f.mu.Lock()
	f.emittedMsgs = append(f.emittedMsgs, msg)
	f.mu.Unlock()
}

// newTestActor builds an actor with nil pm/api so tests can drive
// pure-state methods. Methods that would dereference pm/api are
// avoided in these tests.
func newTestActor(t *testing.T, mgr actorManagerOps) *recoveryActor {
	t.Helper()
	return &recoveryActor{
		producerID:      1,
		mgr:             mgr,
		ctx:             t.Context(),
		inbox:           make(chan actorEvent, 32),
		shutdown:        make(chan struct{}),
		done:            make(chan struct{}),
		eventRecoveries: make(map[int]*eventRecovery),
	}
}

func TestActor_RecoveryStateTransitions(t *testing.T) {
	a := newTestActor(t, newFakeManagerOps())

	if a.recoveryState != types.DefaultRecoveryState {
		t.Errorf("initial state = %v, want Default", a.recoveryState)
	}
	if a.isPerformingRecovery() {
		t.Error("isPerformingRecovery should be false in Default state")
	}

	a.recoveryState = types.StartedRecoveryState
	if !a.isPerformingRecovery() {
		t.Error("isPerformingRecovery should be true in Started state")
	}

	a.recoveryState = types.InterruptedRecoveryState
	if !a.isPerformingRecovery() {
		t.Error("isPerformingRecovery should be true in Interrupted state")
	}

	a.recoveryState = types.CompletedRecoveryState
	if a.isPerformingRecovery() {
		t.Error("isPerformingRecovery should be false in Completed state")
	}

	a.recoveryState = types.ErrorRecoveryState
	if a.isPerformingRecovery() {
		t.Error("isPerformingRecovery should be false in Error state")
	}
}

func TestActor_IsKnownRecovery(t *testing.T) {
	a := newTestActor(t, newFakeManagerOps())
	urn, _ := types.ParseURN("od:match:1")

	if a.isKnownRecovery(7) {
		t.Error("unknown id reported as known")
	}

	a.currentRecovery = newRecoveryData(7, time.Now())
	if !a.isKnownRecovery(7) {
		t.Error("current recovery id not known")
	}

	a.eventRecoveries[9] = newEventRecovery(*urn, 9, time.Now())
	if !a.isKnownRecovery(9) {
		t.Error("event recovery id not known")
	}

	if a.isKnownRecovery(99) {
		t.Error("unrelated id reported as known")
	}
}

func TestActor_SnapshotValidationNeeded(t *testing.T) {
	a := newTestActor(t, newFakeManagerOps())
	cases := map[types.MessageInterest]bool{
		types.LiveOnlyMessageInterest:             true,
		types.PrematchOnlyMessageInterest:         true,
		types.AllMessageInterest:                  false,
		types.HiPriorityOnlyMessageInterest:       false,
		types.LowPriorityOnlyMessageInterest:      false,
		types.SystemAliveOnly:                     false,
		types.SpecifiedMatchesOnlyMessageInterest: false,
	}
	for interest, want := range cases {
		if got := a.snapshotValidationNeeded(interest); got != want {
			t.Errorf("interest %s: got %v, want %v", interest, got, want)
		}
	}
}

// TestActor_ValidateSnapshotComplete confirms the gating logic matches
// Java/.NET: snapshot completes are only accepted when the actor is
// performing recovery (Started OR Interrupted state) AND the request
// id matches the current recovery.
func TestActor_ValidateSnapshotComplete(t *testing.T) {
	a := newTestActor(t, newFakeManagerOps())

	// No current recovery → false.
	if a.validateSnapshotComplete(7, types.AllMessageInterest) {
		t.Error("should be false when not performing recovery")
	}

	// Started + matching request id + non-validating interest → true.
	a.recoveryState = types.StartedRecoveryState
	a.currentRecovery = newRecoveryData(7, time.Now())
	if !a.validateSnapshotComplete(7, types.AllMessageInterest) {
		t.Error("Started + matching request id + AllMessageInterest should validate")
	}

	// Mismatched request id → false.
	if a.validateSnapshotComplete(99, types.AllMessageInterest) {
		t.Error("mismatched request id should not validate")
	}

	// Interrupted state is also accepted (matches Java/.NET).
	a.recoveryState = types.InterruptedRecoveryState
	if !a.validateSnapshotComplete(7, types.AllMessageInterest) {
		t.Error("Interrupted + matching id should validate (matches Java/.NET)")
	}

	// Default state → false.
	a.recoveryState = types.DefaultRecoveryState
	if a.validateSnapshotComplete(7, types.AllMessageInterest) {
		t.Error("Default state should not validate")
	}
}

// fakeCfgWithRecoveryWindow is a minimal config exposing the
// MaxRecoveryExecution value used by expireStuckEventRecoveries.
type fakeCfgWithRecoveryWindow struct {
	mins int
}

func (f *fakeCfgWithRecoveryWindow) AccessToken() *string         { s := ""; return &s }
func (f *fakeCfgWithRecoveryWindow) DefaultLocale() types.Locale  { return types.EnLocale }
func (f *fakeCfgWithRecoveryWindow) MaxInactivity() time.Duration { return 20 * time.Second }
func (f *fakeCfgWithRecoveryWindow) MaxRecoveryExecution() time.Duration {
	return time.Duration(f.mins) * time.Minute
}
func (f *fakeCfgWithRecoveryWindow) MessagingPort() int                      { return 0 }
func (f *fakeCfgWithRecoveryWindow) SdkNodeID() *int                         { return nil }
func (f *fakeCfgWithRecoveryWindow) SelectedEnvironment() *types.Environment { return nil }
func (f *fakeCfgWithRecoveryWindow) SelectedRegion() types.Region            { return types.RegionDefault }
func (f *fakeCfgWithRecoveryWindow) ExchangeName() string                    { return "" }
func (f *fakeCfgWithRecoveryWindow) ReplayExchangeName() string              { return "" }
func (f *fakeCfgWithRecoveryWindow) ReportExtendedData() bool                { return false }
func (f *fakeCfgWithRecoveryWindow) APIURL() (string, error)                 { return "", nil }
func (f *fakeCfgWithRecoveryWindow) MQURL() (string, error)                  { return "", nil }
func (f *fakeCfgWithRecoveryWindow) SportIDPrefix() string                   { return "" }
func (f *fakeCfgWithRecoveryWindow) SetSportIDPrefix(string) config.Config   { return f }

// TestManager_Cleanup_IsIdempotent verifies cleanup's atomic-Swap
// contract: a second call after the first must not panic
// (double-close on closed channels) and the channels stay closed.
func TestManager_Cleanup_IsIdempotent(t *testing.T) {
	out := make(chan types.RecoveryMessage, 1)
	closeCh := make(chan struct{})
	m := &Manager{
		actors: make(map[int]*recoveryActor),
		logger: newDiscardLogger(),
	}
	m.session.Store(&lifecycleSession{
		out:     out,
		closeCh: closeCh,
	})

	m.lifecycleMu.Lock()
	m.cleanup(context.Background())
	// Second call must not panic — session is nil now, no-op.
	m.cleanup(context.Background())
	m.lifecycleMu.Unlock()

	// The pre-cleanup channels were closed exactly once.
	select {
	case <-closeCh:
	default:
		t.Error("closeCh not closed after cleanup")
	}
	select {
	case _, ok := <-out:
		if ok {
			t.Error("out should be closed (recv ok=false)")
		}
	default:
		t.Error("out channel still open after cleanup")
	}
	// And the session pointer is nil after cleanup (Swap took it).
	if m.session.Load() != nil {
		t.Error("session pointer should be nil after cleanup")
	}
}

// TestManager_OpenCloseRace_NoLeak drives the reviewer's scenario:
// Close runs cleanup() while Open is still pre-publication; Open
// then publishes a session via Store; Open's CAS-fail cleanup() must
// take that session via Swap and release its resources.
func TestManager_OpenCloseRace_NoLeak(t *testing.T) {
	m := &Manager{
		actors:          make(map[int]*recoveryActor),
		processingTimes: make(map[uuid.UUID]time.Time),
		logger:          newDiscardLogger(),
	}
	// Phase 1: Open got past NotOpened → Opening. Close raced in,
	// transitioned to Closed, and called cleanup() — session is
	// empty, so cleanup is a no-op.
	m.state.Store(mgrStateOpening)
	m.lifecycleMu.Lock()
	m.state.Store(mgrStateClosed)
	m.cleanup(context.Background())
	m.lifecycleMu.Unlock()

	// Phase 2: Open continues, allocates resources, publishes session.
	cancelCalled := false
	m.session.Store(&lifecycleSession{
		cancelCtx: func() { cancelCalled = true },
		out:       make(chan types.RecoveryMessage, 1),
		closeCh:   make(chan struct{}),
	})

	// Phase 3: Open's CAS fails. cleanup() must Swap the session and
	// tear down its resources.
	if m.state.CompareAndSwap(mgrStateOpening, mgrStateOpen) {
		t.Fatal("CAS swapped Closed → Open")
	}
	m.lifecycleMu.Lock()
	m.cleanup(context.Background())
	m.lifecycleMu.Unlock()

	if !cancelCalled {
		t.Error("cancelCtx not invoked on Open-CAS-fail cleanup")
	}
	if m.session.Load() != nil {
		t.Error("session should be nil after cleanup")
	}
}

// TestManager_OpenCloseStress drives the publish/cleanup race directly
// across many iterations — concurrent session.Store + Close-cleanup
// + concurrent Loaders. Run with -race; the test passes if there's no
// race report.
//
// We don't go through Manager.Open here because that pulls in
// producer.Manager.ActiveProducers (HTTP). The race surface we care
// about is session.Store/Swap + state.Store + cleanup() under
// lifecycleMu — exercised directly.
func TestManager_OpenCloseStress(t *testing.T) {
	for i := 0; i < 200; i++ {
		m := &Manager{
			actors:          make(map[int]*recoveryActor),
			handles:         make(map[int]*Handle),
			processingTimes: make(map[uuid.UUID]time.Time),
			logger:          newDiscardLogger(),
		}
		m.state.Store(mgrStateOpening)

		var wg sync.WaitGroup
		// "Publisher" goroutine — mirrors Open's publish step.
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.lifecycleMu.Lock()
			if m.state.Load() != mgrStateClosed {
				m.session.Store(&lifecycleSession{
					cancelCtx: func() {},
					out:       make(chan types.RecoveryMessage, 1),
					closeCh:   make(chan struct{}),
				})
				m.state.CompareAndSwap(mgrStateOpening, mgrStateOpen)
			}
			m.lifecycleMu.Unlock()
		}()
		// "Closer" goroutine — mirrors Close.
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.Close()
		}()
		// "Reader" goroutine — exercises the same atomic.Pointer.Load
		// path that findOrSpawn / emitRecoveryMessage take. We only
		// load (no sends) because in production only actors send on
		// session.out, and actors are stopped before cleanup closes
		// it. The reader proves the race detector doesn't fire on
		// concurrent Load/Store/Swap on session + state.
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = m.session.Load()
				_ = m.state.Load()
			}
		}()
		wg.Wait()
	}
}

// TestManager_OpenClose_ConcurrentDoesNotResurrect verifies the v2.10
// review fix: Open's final state-publish uses CAS(Opening, Open) so a
// Close that landed during init can't be overwritten back to Open.
//
// We exercise this directly by pre-setting state to Closed (simulating
// a concurrent Close that beat Open's final transition) and confirming
// the CAS refuses to revive the manager.
func TestManager_OpenClose_ConcurrentDoesNotResurrect(t *testing.T) {
	m := &Manager{
		actors: make(map[int]*recoveryActor),
		logger: newDiscardLogger(),
	}
	// Pretend Open ran to the publish step but Close raced in.
	m.state.Store(mgrStateOpening)
	m.state.Store(mgrStateClosed)

	// The CAS Opening → Open in Open's success path must fail.
	swapped := m.state.CompareAndSwap(mgrStateOpening, mgrStateOpen)
	if swapped {
		t.Error("CAS swapped Closed → Open; should have refused")
	}
	if m.state.Load() != mgrStateClosed {
		t.Errorf("state = %d, want Closed (%d)", m.state.Load(), mgrStateClosed)
	}
}

// TestManager_LifecycleGate_Opening: a manager mid-init (state ==
// Opening, fields half-built) must reject dispatchRecoverEvent so
// callers don't reach into a half-built actor map. Verifies the
// v2.9 review fix.
func TestManager_LifecycleGate_Opening(t *testing.T) {
	m := &Manager{
		actors: make(map[int]*recoveryActor),
		logger: newDiscardLogger(),
	}
	m.state.Store(mgrStateOpening)

	urn, _ := types.ParseURN("od:match:1")
	_, err := m.dispatchRecoverEvent(t.Context(), 1, *urn, false)
	if !errors.Is(err, ErrManagerNotOpen) {
		t.Errorf("err = %v, want ErrManagerNotOpen during Opening", err)
	}
	if a := m.findOrSpawn(1); a != nil {
		t.Error("findOrSpawn returned non-nil during Opening")
	}
}

// TestManager_LifecycleGate_NotOpened: dispatchRecoverEvent before
// Open returns ErrManagerNotOpen, no actor is spawned.
func TestManager_LifecycleGate_NotOpened(t *testing.T) {
	m := &Manager{
		actors: make(map[int]*recoveryActor),
		logger: newDiscardLogger(),
	}
	urn, _ := types.ParseURN("od:match:1")
	_, err := m.dispatchRecoverEvent(t.Context(), 1, *urn, false)
	if !errors.Is(err, ErrManagerNotOpen) {
		t.Errorf("err = %v, want ErrManagerNotOpen", err)
	}
	if len(m.actors) != 0 {
		t.Errorf("actor spawned despite not-opened state: %d", len(m.actors))
	}
}

// TestManager_LifecycleGate_Closed: after Close, dispatch returns
// ErrManagerClosed and findOrSpawn returns nil.
func TestManager_LifecycleGate_Closed(t *testing.T) {
	m := &Manager{
		actors:          make(map[int]*recoveryActor),
		processingTimes: make(map[uuid.UUID]time.Time),
		logger:          newDiscardLogger(),
	}
	m.state.Store(mgrStateClosed)

	urn, _ := types.ParseURN("od:match:2")
	_, err := m.dispatchRecoverEvent(t.Context(), 1, *urn, true)
	if !errors.Is(err, ErrManagerClosed) {
		t.Errorf("err = %v, want ErrManagerClosed", err)
	}
	if a := m.findOrSpawn(1); a != nil {
		t.Error("findOrSpawn returned non-nil after Close")
	}
}

// TestActor_ExpireStuckEventRecoveries verifies the v2.8 review fix
// for item 3: a per-tick scan transitions event recoveries that
// exceed MaxRecoveryExecution to RecoveryStatusTimedOut.
func TestActor_ExpireStuckEventRecoveries(t *testing.T) {
	mgr := newFakeManagerOps()
	a := newTestActor(t, mgr)
	a.cfg = &fakeCfgWithRecoveryWindow{mins: 1}

	urn, _ := types.ParseURN("od:match:42")
	old := time.Now().Add(-2 * time.Minute) // older than 1-minute window
	a.eventRecoveries[7] = newEventRecovery(*urn, 7, old)

	a.expireStuckEventRecoveries(time.Now())

	if _, ok := a.eventRecoveries[7]; ok {
		t.Error("expired event recovery still in map")
	}
	if len(mgr.completed) != 1 || mgr.completed[0].id != 7 || mgr.completed[0].status != types.RecoveryStatusTimedOut {
		t.Errorf("expected one TimedOut completion, got %+v", mgr.completed)
	}
}

// TestActor_ExpireStuckEventRecoveries_FreshNotExpired confirms the
// scan ignores in-window recoveries.
func TestActor_ExpireStuckEventRecoveries_FreshNotExpired(t *testing.T) {
	mgr := newFakeManagerOps()
	a := newTestActor(t, mgr)
	a.cfg = &fakeCfgWithRecoveryWindow{mins: 60}

	urn, _ := types.ParseURN("od:match:1")
	a.eventRecoveries[1] = newEventRecovery(*urn, 1, time.Now())
	a.expireStuckEventRecoveries(time.Now())

	if _, ok := a.eventRecoveries[1]; !ok {
		t.Error("fresh event recovery was incorrectly expired")
	}
	if len(mgr.completed) != 0 {
		t.Errorf("unexpected completions: %+v", mgr.completed)
	}
}

// TestActor_OnTick_UnarmedExpiresButSuppressesProducerDown pins the
// arming decoupling: during the manager warm-up window (inactivityArmed
// false), onTick STILL expires stuck event recoveries — so
// MaxRecoveryExecution enforcement isn't delayed by the full initialDelay
// — but it must NOT flag the producer down (no alive baseline yet).
func TestActor_OnTick_UnarmedExpiresButSuppressesProducerDown(t *testing.T) {
	srv, _ := fixtureSrv(t)
	defer srv.Close()
	fake := newFakeManagerOps()
	a := newWiredActor(t, srv, fake)
	a.cfg = &fakeCfgWithRecoveryWindow{mins: 1}

	urn, _ := types.ParseURN("od:match:99")
	old := time.Now().Add(-2 * time.Minute) // older than the 1-minute window
	a.eventRecoveries[99] = newEventRecovery(*urn, 99, old)

	// downReason starts at its zero value; the inactivity path (armed only)
	// is the sole writer here. Producers default to flaggedDown=true, so
	// IsFlaggedDown can't discriminate — downReason can.
	before := a.downReason

	// lastSystemAlive is nil → an ARMED tick would transition the down
	// reason to AliveInternalViolation. Unarmed must not.
	a.onTick(time.Now(), false)

	// Expiry still ran during warm-up.
	if _, ok := a.eventRecoveries[99]; ok {
		t.Error("unarmed tick did not expire the stuck recovery")
	}
	if len(fake.completed) != 1 || fake.completed[0].status != types.RecoveryStatusTimedOut {
		t.Errorf("expected one TimedOut completion, got %+v", fake.completed)
	}
	// Producer-down inactivity check suppressed while unarmed.
	if a.downReason != before {
		t.Errorf("unarmed tick ran the producer-down inactivity check (downReason %v→%v); warm-up gating failed", before, a.downReason)
	}

	// Contrast: an armed tick with the same nil-alive state DOES transition.
	a.onTick(time.Now(), true)
	if a.downReason != types.AliveInternalViolationProducerDownReason {
		t.Errorf("armed tick with no alive should flag AliveInternalViolation, got downReason=%v", a.downReason)
	}
}

// TestActor_StatusSnapshot verifies the polling-getter contract for
// review item 3: Manager.ProducerStatus(producerID) returns the most
// recent ProducerStatus the actor emitted, surviving any drop on the
// lossy RecoveryEvents channel.
func TestActor_StatusSnapshot(t *testing.T) {
	a := newTestActor(t, newFakeManagerOps())
	if got := a.currentStatus(); got != nil {
		t.Errorf("currentStatus() = %v before any emission, want nil", got)
	}
	// Simulate a status emission directly on the snapshot field — the
	// actor's emit path stores via .Store as the only writer.
	impl := &producerStatusImpl{
		isDown:               true,
		producerStatusReason: types.AliveIntervalViolationProducerStatusReason,
	}
	a.statusSnapshot.Store(impl)

	got := a.currentStatus()
	if got == nil {
		t.Fatal("currentStatus() = nil after Store")
	}
	if !got.IsDown() {
		t.Error("IsDown() = false, want true")
	}
}

// TestActor_RunLoopStartsAndStops verifies the actor's run loop
// processes events from its inbox and stops cleanly.
func TestActor_RunLoopStartsAndStops(t *testing.T) {
	a := newTestActor(t, newFakeManagerOps())
	go a.run()

	// Stop should return promptly.
	doneCh := make(chan struct{})
	go func() {
		a.stop()
		close(doneCh)
	}()
	select {
	case <-doneCh:
	case <-time.After(time.Second):
		t.Fatal("actor.stop() did not return within 1s")
	}
}

// TestActor_StopIsIdempotent verifies multiple stop() calls don't panic.
func TestActor_StopIsIdempotent(t *testing.T) {
	a := newTestActor(t, newFakeManagerOps())
	go a.run()
	a.stop()
	a.stop() // second stop must not panic
	a.stop() // third either
}

// TestActor_SendNonBlocking verifies that a full inbox returns false
// from send() rather than blocking.
func TestActor_SendNonBlocking(t *testing.T) {
	a := newTestActor(t, newFakeManagerOps())
	// Don't run() — leave events queued so we can test inbox capacity.
	for i := 0; i < cap(a.inbox); i++ {
		if !a.send(evTick{now: time.Now()}) {
			t.Fatalf("send %d should succeed (inbox not full)", i)
		}
	}
	// Next send should fail (inbox full).
	if a.send(evTick{now: time.Now()}) {
		t.Error("send to full inbox should return false")
	}
}

// TestActor_DispatchHandlesUnknownEvent verifies the default case in
// dispatch logs but doesn't panic on an unrecognized event type.
func TestActor_DispatchHandlesUnknownEvent(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("dispatch panicked on unknown event: %v", r)
		}
	}()
	a := newTestActor(t, newFakeManagerOps())
	a.logger = newDiscardLogger()
	a.dispatch(unknownTestEvent{})
}

type unknownTestEvent struct{}

func (unknownTestEvent) isActorEvent() {}
