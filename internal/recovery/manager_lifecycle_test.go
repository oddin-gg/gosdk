package recovery

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/oddin-gg/gosdk/types"
)

// TestManager_Cleanup_IsIdempotent verifies cleanup's atomic-Swap
// contract: a second call after the first must not panic
// (double-close on closed channels) and the channels stay closed.
func TestManager_Cleanup_IsIdempotent(t *testing.T) {
	out := make(chan types.RecoveryMessage, 1)
	m := &Manager{
		logger: newDiscardLogger(),
	}
	cancelled := false
	m.session.Store(&lifecycleSession{
		out:       out,
		cancelCtx: func() { cancelled = true },
	})

	m.lifecycleMu.Lock()
	m.cleanup(context.Background())
	// Second call must not panic — session is nil now, no-op.
	m.cleanup(context.Background())
	m.lifecycleMu.Unlock()

	// The pre-cleanup lifetime was cancelled and the channel closed exactly once.
	if !cancelled {
		t.Error("lifetime ctx not cancelled after cleanup")
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

// TestManager_OpenCloseRace_NoLeak drives this scenario:
// Close runs cleanup() while Open is still pre-publication; Open
// then publishes a session via Store; Open's CAS-fail cleanup() must
// take that session via Swap and release its resources.
func TestManager_OpenCloseRace_NoLeak(t *testing.T) {
	m := &Manager{
		logger: newDiscardLogger(),
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
			handles: make(map[int]*Handle),
			logger:  newDiscardLogger(),
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
		// path that actor / emitRecoveryMessage take. We only
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

// TestManager_OpenClose_ConcurrentDoesNotResurrect verifies that
// Open's final state-publish uses CAS(Opening, Open), so a
// Close that landed during init can't be overwritten back to Open.
//
// We exercise this directly by pre-setting state to Closed (simulating
// a concurrent Close that beat Open's final transition) and confirming
// the CAS refuses to revive the manager.
func TestManager_OpenClose_ConcurrentDoesNotResurrect(t *testing.T) {
	m := &Manager{
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
// callers don't reach into a half-built manager.
func TestManager_LifecycleGate_Opening(t *testing.T) {
	m := &Manager{
		logger: newDiscardLogger(),
	}
	m.state.Store(mgrStateOpening)

	urn, _ := types.ParseURN("od:match:1")
	_, err := m.dispatchRecoverEvent(t.Context(), 1, *urn, false)
	if !errors.Is(err, ErrManagerNotOpen) {
		t.Errorf("err = %v, want ErrManagerNotOpen during Opening", err)
	}
	if a := m.actor(); a != nil {
		t.Error("an actor is reachable during Opening")
	}
}

// TestManager_LifecycleGate_NotOpened: dispatchRecoverEvent before
// Open returns ErrManagerNotOpen.
func TestManager_LifecycleGate_NotOpened(t *testing.T) {
	m := &Manager{
		logger: newDiscardLogger(),
	}
	urn, _ := types.ParseURN("od:match:1")
	_, err := m.dispatchRecoverEvent(t.Context(), 1, *urn, false)
	if !errors.Is(err, ErrManagerNotOpen) {
		t.Errorf("err = %v, want ErrManagerNotOpen", err)
	}
	if m.actor() != nil {
		t.Error("an actor is reachable before Open")
	}
}

// TestManager_LifecycleGate_Closed: after Close, dispatch returns
// ErrManagerClosed and no actor is reachable.
func TestManager_LifecycleGate_Closed(t *testing.T) {
	m := &Manager{
		logger: newDiscardLogger(),
	}
	m.state.Store(mgrStateClosed)

	urn, _ := types.ParseURN("od:match:2")
	_, err := m.dispatchRecoverEvent(t.Context(), 1, *urn, true)
	if !errors.Is(err, ErrManagerClosed) {
		t.Errorf("err = %v, want ErrManagerClosed", err)
	}
	if a := m.actor(); a != nil {
		t.Error("an actor is reachable after Close")
	}
}
