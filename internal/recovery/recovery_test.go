package recovery

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/protocols"
)

// --- generator ---

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

// --- recoveryData / eventRecovery ---

func TestRecoveryData_SnapshotComplete_Accumulates(t *testing.T) {
	rd := newRecoveryData(42, time.Now())

	got := rd.snapshotComplete(protocols.LiveOnlyMessageInterest)
	if len(got) != 1 || got[0] != protocols.LiveOnlyMessageInterest {
		t.Errorf("first call = %v", got)
	}

	got = rd.snapshotComplete(protocols.PrematchOnlyMessageInterest)
	if len(got) != 2 {
		t.Errorf("second call = %v, want 2 entries", got)
	}

	// Idempotent: same interest doesn't grow the set.
	got = rd.snapshotComplete(protocols.LiveOnlyMessageInterest)
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
			interest := protocols.LiveOnlyMessageInterest
			if i%2 == 0 {
				interest = protocols.PrematchOnlyMessageInterest
			}
			_ = rd.snapshotComplete(interest)
		}(i)
	}
	wg.Wait()

	// Final state: at most 2 distinct interests.
	final := rd.snapshotComplete(protocols.LiveOnlyMessageInterest)
	if len(final) > 2 {
		t.Errorf("final size = %d, want <=2", len(final))
	}
}

func TestEventRecovery_CarriesEventID(t *testing.T) {
	urn, _ := protocols.ParseURN("od:match:42")
	er := newEventRecovery(*urn, 7, time.Now())
	if er.eventID != *urn {
		t.Errorf("eventID = %v, want %v", er.eventID, *urn)
	}
	if er.recoveryID != 7 {
		t.Errorf("recoveryID = %d, want 7", er.recoveryID)
	}
}

// --- producerRecoveryData ---
//
// These tests cover the post-Phase-5b mutex-hygiene rewrite: every
// state field is read/written via locked accessors. We don't need a
// real producer.Manager because the methods exercised here don't
// dereference it.

func newTestPRD() *producerRecoveryData {
	return newProducerRecoveryData(context.Background(), 1, nil)
}

func TestPRD_RecoveryStateRoundTrip(t *testing.T) {
	p := newTestPRD()
	if got := p.getRecoveryState(); got != protocols.DefaultRecoveryState {
		t.Errorf("default state = %v, want Default", got)
	}

	p.setProducerRecoveryState(99, time.Now(), protocols.StartedRecoveryState)
	if got := p.getRecoveryState(); got != protocols.StartedRecoveryState {
		t.Errorf("after set = %v, want Started", got)
	}
	if !p.isPerformingRecovery() {
		t.Errorf("isPerformingRecovery = false, want true while Started")
	}

	p.interruptProducerRecovery()
	if got := p.getRecoveryState(); got != protocols.InterruptedRecoveryState {
		t.Errorf("after interrupt = %v, want Interrupted", got)
	}
	if !p.isPerformingRecovery() {
		t.Errorf("isPerformingRecovery = false during Interrupted, want true")
	}
}

func TestPRD_LastUserSessionAliveTimestampRoundTrip(t *testing.T) {
	p := newTestPRD()
	if !p.getLastUserSessionAliveReceivedTimestamp().IsZero() {
		t.Errorf("default = non-zero")
	}
	now := time.Now()
	p.setLastUserSessionAliveReceivedTimestamp(now)
	if got := p.getLastUserSessionAliveReceivedTimestamp(); !got.Equal(now) {
		t.Errorf("got %v, want %v", got, now)
	}
}

func TestPRD_FirstRecoveryCompleted(t *testing.T) {
	p := newTestPRD()
	if p.getFirstRecoveryCompleted() {
		t.Error("default = true, want false")
	}
	p.setFirstRecoveryCompleted(true)
	if !p.getFirstRecoveryCompleted() {
		t.Error("after set = false")
	}
}

func TestPRD_ProducerStatusReason(t *testing.T) {
	p := newTestPRD()
	if got := p.getProducerStatusReason(); got != protocols.ErrorProducerStatusReason {
		t.Errorf("default = %v", got)
	}
	p.setProducerStatusReason(protocols.AliveIntervalViolationProducerStatusReason)
	if got := p.getProducerStatusReason(); got != protocols.AliveIntervalViolationProducerStatusReason {
		t.Errorf("after set = %v", got)
	}
}

// TestPRD_EventRecoveriesLifecycle round-trips a recovery through the
// event-recoveries map (set → known → completed → forgotten).
func TestPRD_EventRecoveriesLifecycle(t *testing.T) {
	p := newTestPRD()
	urn, _ := protocols.ParseURN("od:match:1")

	if p.isKnownRecovery(7) {
		t.Error("unknown id reported as known")
	}

	p.setEventRecoveryState(*urn, 7, time.Now())
	if !p.isKnownRecovery(7) {
		t.Error("just-set recovery not known")
	}
	if er := p.eventRecovery(7); er == nil || er.eventID != *urn {
		t.Errorf("eventRecovery(7) = %v", er)
	}

	p.eventRecoveryCompleted(7)
	if p.isKnownRecovery(7) {
		t.Error("completed recovery still known")
	}
	if er := p.eventRecovery(7); er != nil {
		t.Errorf("eventRecovery(7) after complete = %v, want nil", er)
	}
}

// TestPRD_SnapshotValidationNeeded confirms the helper logic that
// gates whether multi-scope snapshot validation is required.
func TestPRD_SnapshotValidationNeeded(t *testing.T) {
	p := newTestPRD()
	cases := map[protocols.MessageInterest]bool{
		protocols.LiveOnlyMessageInterest:               true,
		protocols.PrematchOnlyMessageInterest:           true,
		protocols.AllMessageInterest:                    false,
		protocols.HiPriorityOnlyMessageInterest:         false,
		protocols.LowPriorityOnlyMessageInterest:        false,
		protocols.SystemAliveOnly:                       false,
		protocols.SpecifiedMatchesOnlyMessageInterest:   false,
	}
	for interest, want := range cases {
		if got := p.snapshotValidationNeeded(interest); got != want {
			t.Errorf("interest %s: got %v, want %v", interest, got, want)
		}
	}
}

// TestPRD_RaceFreeAccessors exercises all read+write accessors
// concurrently with the race detector enabled. The Phase 5b mutex
// hygiene means no field should race; pre-rewrite this test would
// have tripped on the partial-locking pattern.
func TestPRD_RaceFreeAccessors(t *testing.T) {
	p := newTestPRD()
	urn, _ := protocols.ParseURN("od:match:1")

	var done atomic.Bool
	var wg sync.WaitGroup

	writers := []func(){
		func() { p.setLastUserSessionAliveReceivedTimestamp(time.Now()) },
		func() { p.setFirstRecoveryCompleted(true) },
		func() { p.setProducerStatusReason(protocols.OtherProducerStatusReason) },
		func() { p.setProducerRecoveryState(uint(time.Now().UnixNano()), time.Now(), protocols.StartedRecoveryState) },
		func() { p.interruptProducerRecovery() },
		func() { p.setEventRecoveryState(*urn, uint(time.Now().UnixNano()), time.Now()) },
	}

	readers := []func(){
		func() { _ = p.getRecoveryState() },
		func() { _ = p.getLastSystemAliveReceivedTimestamp() },
		func() { _ = p.getLastUserSessionAliveReceivedTimestamp() },
		func() { _ = p.getLastValidAliveGenTimestampInRecovery() },
		func() { _ = p.getFirstRecoveryCompleted() },
		func() { _ = p.getProducerDownReason() },
		func() { _ = p.getProducerStatusReason() },
		func() { _ = p.isPerformingRecovery() },
		func() { _ = p.isKnownRecovery(123) },
		func() { _ = p.lastRecoveryStartedAt() },
	}

	spawn := func(fns []func()) {
		for _, f := range fns {
			f := f
			wg.Add(1)
			go func() {
				defer wg.Done()
				for !done.Load() {
					f()
				}
			}()
		}
	}
	spawn(writers)
	spawn(readers)

	time.Sleep(40 * time.Millisecond)
	done.Store(true)
	wg.Wait()
}
