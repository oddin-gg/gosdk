package recovery

import (
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	id     uint
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

func (f *fakeManagerOps) completeHandle(id uint, status types.RecoveryRequestStatus, err error) *Handle {
	f.mu.Lock()
	f.completed = append(f.completed, completedHandle{id: id, status: status, err: err})
	f.mu.Unlock()
	return nil
}

func (f *fakeManagerOps) nextRequestID() uint {
	return uint(f.nextID.Add(1))
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
		eventRecoveries: make(map[uint]*eventRecovery),
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
