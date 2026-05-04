package recovery

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/protocols"
)

// --- Handle ---

func TestHandle_StartsPending(t *testing.T) {
	urn, _ := protocols.ParseURN("od:match:1")
	h := NewHandle(7, 1, *urn, time.Now())
	if h.RequestID() != 7 {
		t.Errorf("RequestID = %d, want 7", h.RequestID())
	}
	if h.ProducerID() != 1 {
		t.Errorf("ProducerID = %d, want 1", h.ProducerID())
	}
	if h.EventID() != *urn {
		t.Errorf("EventID = %v, want %v", h.EventID(), *urn)
	}
	if h.Status() != protocols.RecoveryStatusPending {
		t.Errorf("Status = %v, want Pending", h.Status())
	}
	if h.IsTerminal() {
		t.Error("IsTerminal should be false on a fresh handle")
	}
	select {
	case <-h.Done():
		t.Error("Done() should not be closed on a fresh handle")
	default:
	}
}

func TestHandle_Complete_ClosesDone(t *testing.T) {
	urn, _ := protocols.ParseURN("od:match:1")
	h := NewHandle(7, 1, *urn, time.Now())
	h.complete(protocols.RecoveryStatusCompleted, nil, time.Now())

	if !h.IsTerminal() {
		t.Error("IsTerminal should be true after complete")
	}
	select {
	case <-h.Done():
	default:
		t.Error("Done() should be closed after complete")
	}
	if h.Status() != protocols.RecoveryStatusCompleted {
		t.Errorf("Status = %v, want Completed", h.Status())
	}
}

func TestHandle_Result_BlocksUntilDone(t *testing.T) {
	urn, _ := protocols.ParseURN("od:match:1")
	started := time.Now()
	h := NewHandle(7, 1, *urn, started)

	resultCh := make(chan protocols.RecoveryResult, 1)
	go func() {
		resultCh <- h.Result()
	}()

	// Result should be blocked.
	select {
	case <-resultCh:
		t.Fatal("Result returned before complete")
	case <-time.After(20 * time.Millisecond):
	}

	bang := errors.New("boom")
	endTime := time.Now()
	h.complete(protocols.RecoveryStatusFailed, bang, endTime)

	select {
	case res := <-resultCh:
		if res.RequestID != 7 || res.Status != protocols.RecoveryStatusFailed || res.Err != bang {
			t.Errorf("res = %+v", res)
		}
		if !res.StartedAt.Equal(started) {
			t.Errorf("StartedAt = %v, want %v", res.StartedAt, started)
		}
	case <-time.After(time.Second):
		t.Fatal("Result blocked after complete")
	}
}

func TestHandle_CompleteIsIdempotent(t *testing.T) {
	urn, _ := protocols.ParseURN("od:match:1")
	h := NewHandle(7, 1, *urn, time.Now())
	h.complete(protocols.RecoveryStatusCompleted, nil, time.Now())
	// Second call must not panic from a duplicate close.
	h.complete(protocols.RecoveryStatusFailed, errors.New("late"), time.Now())
	if h.Status() != protocols.RecoveryStatusCompleted {
		t.Errorf("status changed after second complete: %v", h.Status())
	}
}

// TestHandle_ConcurrentCompleteAndRead exercises the race detector with
// many goroutines reading and one completing.
func TestHandle_ConcurrentCompleteAndRead(t *testing.T) {
	urn, _ := protocols.ParseURN("od:match:1")
	h := NewHandle(7, 1, *urn, time.Now())

	const readers = 32
	var wg sync.WaitGroup
	wg.Add(readers)
	for i := 0; i < readers; i++ {
		go func() {
			defer wg.Done()
			_ = h.Status()
			_ = h.Snapshot()
			<-h.Done()
			_ = h.Result()
		}()
	}

	go h.complete(protocols.RecoveryStatusCompleted, nil, time.Now())

	wg.Wait()
}

func TestHandle_StatusString(t *testing.T) {
	cases := map[protocols.RecoveryRequestStatus]string{
		protocols.RecoveryStatusPending:   "pending",
		protocols.RecoveryStatusCompleted: "completed",
		protocols.RecoveryStatusFailed:    "failed",
		protocols.RecoveryStatusTimedOut:  "timed_out",
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Errorf("%v.String() = %q, want %q", s, got, want)
		}
	}
}

// --- Manager handle tracking ---

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	return &Manager{
		handles: make(map[uint]*Handle),
	}
}

func TestManager_RegisterAndLookup(t *testing.T) {
	m := newTestManager(t)
	urn, _ := protocols.ParseURN("od:match:1")
	h := NewHandle(42, 1, *urn, time.Now())
	m.registerHandle(h)

	got, ok := m.LookupHandle(42)
	if !ok || got != h {
		t.Fatalf("LookupHandle = (%v, %v), want (%v, true)", got, ok, h)
	}
	if _, ok := m.LookupHandle(99); ok {
		t.Error("LookupHandle(99) should be false for unknown id")
	}
}

func TestManager_CompleteHandle(t *testing.T) {
	m := newTestManager(t)
	urn, _ := protocols.ParseURN("od:match:1")
	h := NewHandle(42, 1, *urn, time.Now())
	m.registerHandle(h)

	got := m.completeHandle(42, protocols.RecoveryStatusCompleted, nil)
	if got != h {
		t.Errorf("completeHandle returned %v, want the registered handle", got)
	}
	if !h.IsTerminal() {
		t.Error("handle not terminal after completeHandle")
	}
	if h.Status() != protocols.RecoveryStatusCompleted {
		t.Errorf("Status = %v, want Completed", h.Status())
	}

	// Unknown id is a no-op (returns nil).
	if got := m.completeHandle(99, protocols.RecoveryStatusCompleted, nil); got != nil {
		t.Errorf("completeHandle(99) = %v, want nil", got)
	}
}

func TestManager_GcCompletedHandles(t *testing.T) {
	m := newTestManager(t)
	urn, _ := protocols.ParseURN("od:match:1")

	old := NewHandle(1, 1, *urn, time.Now())
	old.complete(protocols.RecoveryStatusCompleted, nil, time.Now().Add(-2*HandleGCGracePeriod))
	m.registerHandle(old)

	recent := NewHandle(2, 1, *urn, time.Now())
	recent.complete(protocols.RecoveryStatusCompleted, nil, time.Now())
	m.registerHandle(recent)

	pending := NewHandle(3, 1, *urn, time.Now().Add(-2*HandleGCGracePeriod))
	m.registerHandle(pending)

	m.gcCompletedHandles(time.Now())

	if _, ok := m.LookupHandle(1); ok {
		t.Error("old completed handle should have been GC'd")
	}
	if _, ok := m.LookupHandle(2); !ok {
		t.Error("recent completed handle should still be there")
	}
	if _, ok := m.LookupHandle(3); !ok {
		t.Error("pending handle should never be GC'd (not terminal)")
	}
}

func TestManager_FailPendingHandles(t *testing.T) {
	m := newTestManager(t)
	urn, _ := protocols.ParseURN("od:match:1")

	pending := NewHandle(1, 1, *urn, time.Now())
	m.registerHandle(pending)

	completed := NewHandle(2, 1, *urn, time.Now())
	completed.complete(protocols.RecoveryStatusCompleted, nil, time.Now())
	m.registerHandle(completed)

	bang := errors.New("manager closed")
	m.failPendingHandles(bang)

	if pending.Status() != protocols.RecoveryStatusFailed {
		t.Errorf("pending Status = %v, want Failed", pending.Status())
	}
	if pending.Snapshot().Err != bang {
		t.Errorf("pending Err = %v, want %v", pending.Snapshot().Err, bang)
	}
	// The already-completed handle stays Completed.
	if completed.Status() != protocols.RecoveryStatusCompleted {
		t.Errorf("completed Status changed to %v after failPending", completed.Status())
	}
}
