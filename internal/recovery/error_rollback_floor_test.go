package recovery

import (
	"errors"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/types"
)

// failRepeatPOST delivers the failure of the POST of the request now in
// flight, as the detached call does once its retries are spent, and checks
// that the actor rolled back to Error with nothing in flight.
func failRepeatPOST(t *testing.T, a *recoveryActor) {
	t.Helper()
	id := a.currentRecovery.recoveryID
	a.onSnapshotRecoveryAPICompleted(evSnapshotRecoveryAPICompleted{
		requestID: id,
		err:       errors.New("the API is down"),
	})
	if a.recoveryState != types.ErrorRecoveryState || a.currentRecovery != nil {
		t.Fatalf("after the failed POST: state=%v current=%v, want Error and nothing in flight", a.recoveryState, a.currentRecovery)
	}
}

// A snapshot recovery whose POST fails after its retries is rolled back to
// Error, and the next alive starts a new one. That one must reach back to
// where the failed one was to start: for a repeat of a recovery processing
// fell behind during, the gap lies before the last processed message, and
// the request that was to cover it is the only record of it (the floor was
// discharged when the recovery that fell behind completed, and the repeat
// zeroes its own waiting state when it is issued). Without the floor the
// producer would go up with the dropped messages never requested.
func TestActor_FailedRepeatPOST_NextRecoveryReachesBackToTheRepeatsStart(t *testing.T) {
	fake := newFakeManagerOps()
	a, hits, r1, _ := genuineDelayDuringRecovery(t, fake)
	p := holdProcessing(a)
	from1 := a.currentRecovery.recoverFrom
	if err := a.snapshotRecoveryFinished(r1); err != nil {
		t.Fatal(err)
	}
	catchUp(a, p, time.Now())
	waitRecoverHits(t, hits, 3)
	if got := hits.lastAfterMillis.Load(); got != from1.UnixMilli() {
		t.Fatalf("setup: repeat after=%d, want %d", got, from1.UnixMilli())
	}
	if !a.recoveryFloor.IsZero() || a.recoveryFloorFull {
		t.Fatalf("setup: floor=%v full=%v, want none once the repeat is issued", a.recoveryFloor, a.recoveryFloorFull)
	}

	failRepeatPOST(t, a)

	if err := a.systemAliveReceived(aliveAt(time.Now()), true); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 4)
	if got := hits.lastAfterMillis.Load(); got == 0 || got > from1.UnixMilli() {
		t.Fatalf("recovery after the failed repeat: after=%d (%v), want no later than the repeat's start %d (%v)",
			got, time.UnixMilli(got), from1.UnixMilli(), from1)
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// A full-history repeat whose POST fails stays a full-history one: no
// timestamp expresses it, so the next request carries no `after`.
func TestActor_FailedFullHistoryRepeatPOST_NextRecoveryIsFullHistory(t *testing.T) {
	fake := newFakeManagerOps()
	now := time.Now().Truncate(time.Millisecond)
	a, hits := steadyActor(t, fake, now.Add(-10*time.Second))

	r1 := delayDuringRecoveryFrom(t, a, hits, now, now.Add(-5*time.Second))
	if !a.currentRecovery.recoverFrom.IsZero() {
		t.Fatalf("setup: recoverFrom = %v, want zero (full history)", a.currentRecovery.recoverFrom)
	}
	p := holdProcessing(a)
	if err := a.snapshotRecoveryFinished(r1); err != nil {
		t.Fatal(err)
	}
	catchUp(a, p, time.Now())
	waitRecoverHits(t, hits, 3)
	if got := hits.lastAfterMillis.Load(); got != 0 {
		t.Fatalf("setup: repeat after=%d, want none", got)
	}

	failRepeatPOST(t, a)
	if !a.recoveryFloorFull {
		t.Fatal("the failed full-history repeat left no full-history floor")
	}

	if err := a.systemAliveReceived(aliveAt(time.Now()), true); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 4)
	if got := hits.lastAfterMillis.Load(); got != 0 {
		t.Fatalf("recovery after the failed repeat: after=%d (%v), want none (the full history)", got, time.UnixMilli(got))
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// The same holds for a recovery that outlives MaxRecoveryExecution: the
// sweep of the next alive rolls it back to Error and starts a new one,
// which must still reach back to where the expired one was to start.
func TestActor_MaxRecoveryExecutionSweep_KeepsTheExpiredRecoverysRange(t *testing.T) {
	fake := newFakeManagerOps()
	a, hits, _, _ := genuineDelayDuringRecovery(t, fake)
	from1 := a.currentRecovery.recoverFrom
	a.currentRecovery.chainStartedAt = time.Now().Add(-400 * time.Hour)
	// The alives that arrive meanwhile move the cursor past the expired
	// request's start; only the floor still reaches back to it.
	if err := a.pm.SetLastAliveReceivedGenTimestamp(a.producerID, time.Now()); err != nil {
		t.Fatal(err)
	}

	if err := a.systemAliveReceived(aliveAt(time.Now()), true); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 3)
	if got := hits.lastAfterMillis.Load(); got == 0 || got > from1.UnixMilli() {
		t.Fatalf("recovery after the sweep: after=%d (%v), want no later than the expired one's start %d (%v)",
			got, time.UnixMilli(got), from1.UnixMilli(), from1)
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// The tick expires a stuck snapshot recovery the same way, and keeps its
// range for the recovery the next alive starts.
func TestActor_ExpireStuckSnapshotRecovery_KeepsTheExpiredRecoverysRange(t *testing.T) {
	fake := newFakeManagerOps()
	a, hits, _, _ := genuineDelayDuringRecovery(t, fake)
	from1 := a.currentRecovery.recoverFrom
	a.currentRecovery.chainStartedAt = time.Now().Add(-400 * time.Hour)
	// The alives that arrive meanwhile move the cursor past the expired
	// request's start; only the floor still reaches back to it.
	if err := a.pm.SetLastAliveReceivedGenTimestamp(a.producerID, time.Now()); err != nil {
		t.Fatal(err)
	}

	a.expireStuckSnapshotRecovery(time.Now())
	if a.recoveryState != types.ErrorRecoveryState || a.currentRecovery != nil {
		t.Fatalf("after the expiry: state=%v current=%v, want Error and nothing in flight", a.recoveryState, a.currentRecovery)
	}

	if err := a.systemAliveReceived(aliveAt(time.Now()), true); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 3)
	if got := hits.lastAfterMillis.Load(); got == 0 || got > from1.UnixMilli() {
		t.Fatalf("recovery after the expiry: after=%d (%v), want no later than the expired one's start %d (%v)",
			got, time.UnixMilli(got), from1.UnixMilli(), from1)
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// The POST of a recovery already marked for a processing delay fails. The
// replacement the next alive starts takes the mark over: completing while
// processing still lags, it does not report the producer up; a repeat
// follows and the producer comes up when that completes.
func TestActor_FailedPOSTOfAMarkedRecovery_ReplacementStillRepeats(t *testing.T) {
	fake := newFakeManagerOps()
	a, hits, r1, _ := genuineDelayDuringRecovery(t, fake)
	p := holdProcessing(a)
	if a.delayDownRequestID != r1 {
		t.Fatalf("setup: delay mark = %d, want R1 %d", a.delayDownRequestID, r1)
	}
	failRepeatPOST(t, a)
	_, upBefore := downEventCounts(fake)

	if err := a.systemAliveReceived(aliveAt(time.Now()), true); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 3)
	r2 := a.currentRecovery.recoveryID
	if r2 == r1 || a.delayDownRequestID != r2 {
		t.Fatalf("replacement id=%d (R1 %d) mark=%d, want the mark on the replacement", r2, r1, a.delayDownRequestID)
	}

	if err := a.snapshotRecoveryFinished(r2); err != nil {
		t.Fatal(err)
	}
	if _, up := downEventCounts(fake); up != upBefore {
		t.Fatalf("producer-up events went from %d to %d, want none while processing lags", upBefore, up)
	}
	if !a.isFlaggedDown() || !a.delayRepeatWaiting {
		t.Fatalf("down=%v waiting=%v, want down with a repeat waiting", a.isFlaggedDown(), a.delayRepeatWaiting)
	}

	catchUp(a, p, time.Now())
	waitRecoverHits(t, hits, 4)
	if err := a.snapshotRecoveryFinished(a.currentRecovery.recoveryID); err != nil {
		t.Fatal(err)
	}
	if a.isFlaggedDown() {
		t.Fatal("producer still down after the repeat completed")
	}
	waitDetachedDone(t, a, 3*time.Second)
}
