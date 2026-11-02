package recovery

import (
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/types"
)

// TestActor_ReturnFromProcessingDelayDown_RequestsASnapshotRecovery is the
// regression for a gap that was never refilled: a producer whose processing
// fell behind was flagged down (ProcessingQueueDelayViolation); while the
// backlog stood, the broker dropped messages from the head of its queue.
// The producer came back as soon as its timing was fresh again
// (ReturnedFromInactivity) and nothing requested the dropped messages.
//
// Now the way back is a snapshot recovery, from where processing stood when
// the producer fell behind (not from where the drained backlog has got to,
// which lies past the gap), and the producer is reported up when that
// recovery completes.
func TestActor_ReturnFromProcessingDelayDown_RequestsASnapshotRecovery(t *testing.T) {
	fake := newFakeManagerOps()
	now := time.Now().Truncate(time.Millisecond)
	a, hits := steadyActor(t, fake, now.Add(-10*time.Second))
	if hits.recover.Load() != 1 {
		t.Fatalf("setup: recovery POSTs = %d, want 1", hits.recover.Load())
	}

	// Processing stands two minutes behind; the next tick flags the
	// producer down for it.
	stoodAt := now.Add(-2 * time.Minute)
	a.onMessageProcessingEnded(stoodAt)
	a.onTick(now, true)
	if !a.isFlaggedDown() || a.downReason != types.ProcessingQueueDelayViolationProducerDownReason {
		t.Fatalf("setup: down=%v reason=%v, want down for the processing delay", a.isFlaggedDown(), a.downReason)
	}
	eventsWhenDown := len(producerStatusEvents(fake))

	// The backlog drains: processing is current again, the next system alive
	// arrives.
	a.onMessageProcessingEnded(now)
	if err := a.systemAliveReceived(aliveAt(now), true); err != nil {
		t.Fatal(err)
	}

	// A snapshot recovery was requested from where processing stood.
	waitRecoverHits(t, hits, 2)
	if got := hits.lastAfterMillis.Load(); got != stoodAt.UnixMilli() {
		t.Fatalf("recovery after=%d (%v), want the cursor at the time of the delay %d (%v)",
			got, time.UnixMilli(got), stoodAt.UnixMilli(), stoodAt)
	}
	if a.recoveryState != types.StartedRecoveryState || a.currentRecovery == nil {
		t.Fatalf("recovery state = %v, want Started", a.recoveryState)
	}
	// The producer is NOT reported up before the recovery completes.
	if !a.isFlaggedDown() {
		t.Fatal("producer came back up before its recovery completed")
	}
	if got := len(producerStatusEvents(fake)); got != eventsWhenDown {
		t.Fatalf("status events changed from %d to %d before the recovery completed", eventsWhenDown, got)
	}

	// Processing stands behind AGAIN while the recovery runs: the producer is
	// already down for the delay, so this is the same episode, not a new
	// delay beginning during the request (no follow-up for it below).
	late := now.Add(10 * time.Minute)
	a.noteRecoveryMessage(a.currentRecovery.recoveryID, late) // the recovery is alive
	a.lastSystemAlive = &late
	a.onTick(late, true)
	if a.downReason != types.ProcessingQueueDelayViolationProducerDownReason || a.recoveryState != types.StartedRecoveryState {
		t.Fatalf("setup: down reason %v state %v", a.downReason, a.recoveryState)
	}

	// Further alives while it runs do not stack requests.
	if err := a.systemAliveReceived(aliveAt(now.Add(time.Second)), true); err != nil {
		t.Fatal(err)
	}
	if hits.recover.Load() != 2 {
		t.Fatalf("recovery POSTs = %d, want still 2 while one is in flight", hits.recover.Load())
	}

	// Completion brings the producer up, as after any other recovery.
	if err := a.snapshotRecoveryFinished(a.currentRecovery.recoveryID); err != nil {
		t.Fatal(err)
	}
	if a.isFlaggedDown() || a.recoveryState != types.CompletedRecoveryState {
		t.Fatalf("after completion: down=%v state=%v", a.isFlaggedDown(), a.recoveryState)
	}
	evs := producerStatusEvents(fake)
	if last := evs[len(evs)-1]; last.IsDown() || last.ProducerStatusReason() != types.ReturnedFromInactivityProducerStatusReason {
		t.Fatalf("last status: down=%v reason=%v, want up / ReturnedFromInactivity", last.IsDown(), last.ProducerStatusReason())
	}
	if !a.recoveryFloor.IsZero() {
		t.Fatalf("recovery floor %v not discharged by the recovery that reached it", a.recoveryFloor)
	}
	if hits.recover.Load() != 2 {
		t.Fatalf("recovery POSTs = %d, want 2: a delay down BEFORE the request needs no follow-up", hits.recover.Load())
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// The floor keeps the EARLIEST point: processing standing still for several
// ticks while down does not move it forward.
func TestActor_ProcessingDelayDown_FloorKeepsTheEarliestCursor(t *testing.T) {
	fake := newFakeManagerOps()
	now := time.Now().Truncate(time.Millisecond)
	a, _ := steadyActor(t, fake, now.Add(-10*time.Second))

	stoodAt := now.Add(-2 * time.Minute)
	a.onMessageProcessingEnded(stoodAt)
	a.onTick(now, true)
	// Processing creeps forward but stays behind.
	a.onMessageProcessingEnded(now.Add(-time.Minute))
	a.onTick(now.Add(10*time.Second), true)
	if !a.recoveryFloor.Equal(stoodAt) {
		t.Fatalf("floor = %v, want the earliest cursor %v", a.recoveryFloor, stoodAt)
	}
}

// Only a processing delay lowers the floor: a down for another reason keeps
// its own recovery path untouched.
func TestActor_OtherDownReasons_DoNotLowerTheFloor(t *testing.T) {
	fake := newFakeManagerOps()
	now := time.Now().Truncate(time.Millisecond)
	a, _ := steadyActor(t, fake, now.Add(-10*time.Second))
	a.onMessageProcessingEnded(now.Add(-2 * time.Minute))

	if err := a.producerDown(types.AliveInternalViolationProducerDownReason); err != nil {
		t.Fatal(err)
	}
	if !a.recoveryFloor.IsZero() {
		t.Fatalf("floor %v set by a down that is not a processing delay", a.recoveryFloor)
	}
}

// startRecoveryAfterLoss takes a steady actor down with a channel loss and
// lets the next alive start its snapshot recovery R1 (state Started).
func startRecoveryAfterLoss(t *testing.T, a *recoveryActor, hits *recoveryHits, now time.Time) int {
	t.Helper()
	lossAt(t, a, now.Add(-2*time.Second))
	if err := a.systemAliveReceived(aliveAt(now), true); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 2)
	if a.recoveryState != types.StartedRecoveryState {
		t.Fatalf("setup: state = %v, want Started", a.recoveryState)
	}
	return a.currentRecovery.recoveryID
}

// genuineDelayDuringRecovery brings an actor to the state of a genuine
// processing delay that began while a snapshot recovery R1 runs: processing
// stood at stoodAt (just before the loss, LATER than the point R1 starts
// from), then fell behind. Returns R1's id and stoodAt.
func genuineDelayDuringRecovery(t *testing.T, fake *fakeManagerOps) (*recoveryActor, *recoveryHits, int, time.Time) {
	t.Helper()
	now := time.Now().Truncate(time.Millisecond)
	a, hits := steadyActor(t, fake, now.Add(-10*time.Second))
	// A steady producer has a recovery cursor: R1 starts from there.
	if err := a.systemAliveReceived(aliveAt(now.Add(-8*time.Second)), true); err != nil {
		t.Fatal(err)
	}
	stoodAt := now.Add(-5 * time.Second)
	a.onMessageProcessingEnded(stoodAt)
	// An alive that says "not subscribed" takes the producer down and starts
	// R1 from the recovery cursor (no loss, so no floor yet).
	if err := a.systemAliveReceived(aliveAt(now), false); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 2)
	if a.recoveryState != types.StartedRecoveryState || !a.recoveryFloor.IsZero() {
		t.Fatalf("setup: state=%v floor=%v, want Started and no floor", a.recoveryState, a.recoveryFloor)
	}
	r1 := a.currentRecovery.recoveryID

	tickAt := now.Add(30 * time.Second) // processing stands 35 s behind
	a.lastSystemAlive = &tickAt
	a.onTick(tickAt, true)
	if a.downReason != types.ProcessingQueueDelayViolationProducerDownReason {
		t.Fatalf("setup: down reason = %v, want the processing delay", a.downReason)
	}
	if a.recoveryState != types.StartedRecoveryState || a.currentRecovery.recoveryID != r1 {
		t.Fatalf("the delay interrupted the running recovery: state=%v id=%d", a.recoveryState, a.currentRecovery.recoveryID)
	}
	if !a.recoveryFloor.Equal(stoodAt) || !a.recoveryFloor.After(a.currentRecovery.recoverFrom) {
		t.Fatalf("setup: floor %v must be later than R1's recoverFrom %v", a.recoveryFloor, a.currentRecovery.recoverFrom)
	}
	return a, hits, r1, stoodAt
}

// A processing delay that begins while a snapshot recovery is already
// running does not interrupt it (that exemption keeps a recovery's own
// replay from looping on itself), but the broker may drop messages after
// that request went out. When the delay is genuinely newer than the
// recovery's start (the floor lies later than its recover-from point), its
// completion requests ONE follow-up from the floor before the producer is
// reported up, and never a second one in the chain.
func TestActor_DelayDownDuringRecovery_OneFollowUpBeforeTheProducerIsUp(t *testing.T) {
	fake := newFakeManagerOps()
	a, hits, r1, stoodAt := genuineDelayDuringRecovery(t, fake)
	eventsBefore := len(producerStatusEvents(fake))

	// R1 completes: a follow-up from the floor, the producer stays down.
	if err := a.snapshotRecoveryFinished(r1); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 3)
	if got := hits.lastAfterMillis.Load(); got != stoodAt.UnixMilli() {
		t.Fatalf("follow-up after=%d (%v), want the floor %d (%v)", got, time.UnixMilli(got), stoodAt.UnixMilli(), stoodAt)
	}
	r2 := a.currentRecovery.recoveryID
	if r2 == r1 || a.recoveryState != types.StartedRecoveryState || !a.isFlaggedDown() {
		t.Fatalf("after R1: id=%d (R1 %d) state=%v down=%v, want a new Started recovery and the producer down",
			r2, r1, a.recoveryState, a.isFlaggedDown())
	}
	if got := len(producerStatusEvents(fake)); got != eventsBefore {
		t.Fatalf("producer reported up before the follow-up completed (%d new status events)", got-eventsBefore)
	}

	// Even if a delay began again during the follow-up, with its cursor later
	// than the follow-up's start (so it WOULD qualify), it must not chain
	// another recovery.
	a.delayCursor = a.currentRecovery.recoverFrom.Add(time.Minute)
	a.delayDownRequestID = r2
	if !a.delayFollowUpNeeded() {
		t.Fatal("setup: the second delay should qualify for a follow-up on its own")
	}
	if err := a.snapshotRecoveryFinished(r2); err != nil {
		t.Fatal(err)
	}
	if hits.recover.Load() != 3 {
		t.Fatalf("recovery POSTs = %d, want 3 (one follow-up, not a second)", hits.recover.Load())
	}
	if a.isFlaggedDown() || a.recoveryState != types.CompletedRecoveryState {
		t.Fatalf("after the follow-up: down=%v state=%v, want up + Completed", a.isFlaggedDown(), a.recoveryState)
	}
	waitDetachedDone(t, a, 3*time.Second)
	if a.delayFollowUpIssued {
		t.Fatal("follow-up allowance not reset when the producer came up")
	}
}

// The need for the follow-up belongs to the chain: when the snapshot-complete
// deadline replaces the request that was in flight when the delay began, the
// replacement's completion still follows up (exactly once).
func TestActor_DelayDownDuringRecovery_FollowUpSurvivesADeadlineReissue(t *testing.T) {
	fake := newFakeManagerOps()
	a, hits, r1, stoodAt := genuineDelayDuringRecovery(t, fake)
	started := a.currentRecovery.recoveryStartedAt

	// R1 goes silent past the deadline and is replaced by R2.
	tickAt := started.Add(6 * time.Minute)
	a.lastSystemAlive = &tickAt
	a.onTick(tickAt, true)
	r2 := a.currentRecovery.recoveryID
	if r2 == r1 {
		t.Fatal("setup: no deadline re-issue")
	}
	waitRecoverHits(t, hits, 3)

	if err := a.snapshotRecoveryFinished(r2); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 4)
	if got := hits.lastAfterMillis.Load(); got != stoodAt.UnixMilli() {
		t.Fatalf("follow-up after=%d, want the floor %d", got, stoodAt.UnixMilli())
	}
	r3 := a.currentRecovery.recoveryID
	if r3 == r2 || !a.isFlaggedDown() {
		t.Fatalf("after R2: id=%d down=%v, want a follow-up and the producer down", r3, a.isFlaggedDown())
	}
	if err := a.snapshotRecoveryFinished(r3); err != nil {
		t.Fatal(err)
	}
	if hits.recover.Load() != 4 || a.isFlaggedDown() {
		t.Fatalf("POSTs=%d down=%v, want exactly one follow-up and the producer up", hits.recover.Load(), a.isFlaggedDown())
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// A delay flagged during the replay of an ordinary recovery after an outage
// is the outage's own staleness (the cursor is old because of the outage the
// recovery is repairing): the floor lies at or before the point the recovery
// starts from, so it already covers it and no second recovery is requested.
func TestActor_DelayDownDuringOutageRecovery_NoFollowUp(t *testing.T) {
	fake := newFakeManagerOps()
	now := time.Now().Truncate(time.Millisecond)
	a, hits := steadyActor(t, fake, now.Add(-10*time.Second))
	if err := a.systemAliveReceived(aliveAt(now.Add(-8*time.Second)), true); err != nil {
		t.Fatal(err)
	}
	a.onMessageProcessingEnded(now.Add(-2 * time.Minute)) // stale because of the outage
	r1 := startRecoveryAfterLoss(t, a, hits, now)

	a.onTick(now, true)
	if a.downReason != types.ProcessingQueueDelayViolationProducerDownReason {
		t.Fatalf("setup: down reason = %v, want the delay flagged during the replay", a.downReason)
	}
	if a.recoveryFloor.After(a.currentRecovery.recoverFrom) {
		t.Fatalf("setup: floor %v later than recoverFrom %v", a.recoveryFloor, a.currentRecovery.recoverFrom)
	}
	if err := a.snapshotRecoveryFinished(r1); err != nil {
		t.Fatal(err)
	}
	if hits.recover.Load() != 2 {
		t.Fatalf("recovery POSTs = %d, want 2: no redundant second recovery", hits.recover.Load())
	}
	if a.isFlaggedDown() || a.recoveryState != types.CompletedRecoveryState {
		t.Fatalf("down=%v state=%v, want up + Completed", a.isFlaggedDown(), a.recoveryState)
	}
}

// Without a delay during the recovery there is no follow-up.
func TestActor_RecoveryWithoutDelay_NoFollowUp(t *testing.T) {
	fake := newFakeManagerOps()
	now := time.Now().Truncate(time.Millisecond)
	a, hits := steadyActor(t, fake, now.Add(-10*time.Second))
	r1 := startRecoveryAfterLoss(t, a, hits, now)
	if err := a.snapshotRecoveryFinished(r1); err != nil {
		t.Fatal(err)
	}
	if hits.recover.Load() != 2 || a.isFlaggedDown() {
		t.Fatalf("POSTs=%d down=%v, want 2 and up", hits.recover.Load(), a.isFlaggedDown())
	}
}

// delayDuringRecoveryFrom starts a snapshot recovery R1 with `alive` (not
// subscribed: it takes the producer down and requests from the recovery
// cursor and the floor), then lets processing stand at stoodAt and fall
// behind while R1 runs. Returns R1's id.
func delayDuringRecoveryFrom(t *testing.T, a *recoveryActor, hits *recoveryHits, now, stoodAt time.Time) int {
	t.Helper()
	if err := a.systemAliveReceived(aliveAt(now), false); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 2)
	if a.recoveryState != types.StartedRecoveryState {
		t.Fatalf("setup: state = %v, want Started", a.recoveryState)
	}
	r1 := a.currentRecovery.recoveryID
	a.onMessageProcessingEnded(stoodAt)
	tickAt := now.Add(30 * time.Second)
	a.lastSystemAlive = &tickAt
	a.onTick(tickAt, true)
	if a.downReason != types.ProcessingQueueDelayViolationProducerDownReason {
		t.Fatalf("setup: down reason = %v, want the processing delay", a.downReason)
	}
	return r1
}

// The follow-up is decided by the DELAY's own cursor, not by the general
// recovery floor. Here the floor is older than the new delay and equals the
// point R1 started from (an interrupted recovery restarted from its floor):
// the floor says "already covered", but processing fell behind AFTER R1's
// request went out, so what the broker drops from then on is not in its
// replay. The follow-up starts from where processing stood when the delay
// began, and the older floor is discharged by R1, not replayed again.
func TestActor_DelayDownDuringRecovery_OlderFloorDoesNotHideANewDelay(t *testing.T) {
	fake := newFakeManagerOps()
	now := time.Now().Truncate(time.Millisecond)
	a, hits := steadyActor(t, fake, now.Add(-10*time.Second))
	if err := a.systemAliveReceived(aliveAt(now.Add(-8*time.Second)), true); err != nil {
		t.Fatal(err)
	}
	floor := now.Add(-time.Minute)
	a.lowerRecoveryFloor(floor) // e.g. left by an interrupted recovery
	stoodAt := now.Add(-5 * time.Second)

	r1 := delayDuringRecoveryFrom(t, a, hits, now, stoodAt)
	if !a.currentRecovery.recoverFrom.Equal(floor) || !a.recoveryFloor.Equal(floor) {
		t.Fatalf("setup: recoverFrom=%v floor=%v, want both at %v", a.currentRecovery.recoverFrom, a.recoveryFloor, floor)
	}

	if err := a.snapshotRecoveryFinished(r1); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 3)
	if got := hits.lastAfterMillis.Load(); got != stoodAt.UnixMilli() {
		t.Fatalf("follow-up after=%d (%v), want the delay cursor %d (%v), not the older floor", got, time.UnixMilli(got), stoodAt.UnixMilli(), stoodAt)
	}
	if !a.isFlaggedDown() || a.recoveryState != types.StartedRecoveryState {
		t.Fatalf("down=%v state=%v, want the producer down and the follow-up Started", a.isFlaggedDown(), a.recoveryState)
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// A recovery that started from zero asked for the full history: it covers
// everything, so a delay during it needs no follow-up.
func TestActor_DelayDownDuringFullHistoryRecovery_NoFollowUp(t *testing.T) {
	fake := newFakeManagerOps()
	now := time.Now().Truncate(time.Millisecond)
	a, hits := steadyActor(t, fake, now.Add(-10*time.Second))

	r1 := delayDuringRecoveryFrom(t, a, hits, now, now.Add(-5*time.Second))
	if !a.currentRecovery.recoverFrom.IsZero() {
		t.Fatalf("setup: recoverFrom = %v, want zero (full history)", a.currentRecovery.recoverFrom)
	}
	if err := a.snapshotRecoveryFinished(r1); err != nil {
		t.Fatal(err)
	}
	if hits.recover.Load() != 2 {
		t.Fatalf("recovery POSTs = %d, want 2: a full-history recovery needs no follow-up", hits.recover.Load())
	}
	if a.isFlaggedDown() || a.recoveryState != types.CompletedRecoveryState {
		t.Fatalf("down=%v state=%v, want up + Completed", a.isFlaggedDown(), a.recoveryState)
	}
}

// A processing delay that begins before anything was processed (a stale
// user-session alive can flag it first) has no cursor to remember, and a
// timestamp floor cannot say "from the beginning": the full-history floor is
// recorded, so the recovery on the way back asks for everything instead of
// starting from the then-current cursor, past what the broker dropped.
func TestActor_DelayDownBeforeAnythingWasProcessed_RecoversFromTheFullHistory(t *testing.T) {
	fake := newFakeManagerOps()
	now := time.Now().Truncate(time.Millisecond)
	a, hits := steadyActor(t, fake, now.Add(-10*time.Second))
	if lp, _ := a.lastProcessedMessageGenTimestamp(); !lp.IsZero() {
		t.Fatalf("setup: processed cursor = %v, want none", lp)
	}

	// The user-session alive is stale, so the tick flags the delay.
	a.lastUserSessionAlive = now.Add(-time.Hour)
	a.onTick(now, true)
	if a.downReason != types.ProcessingQueueDelayViolationProducerDownReason {
		t.Fatalf("setup: down reason = %v, want the processing delay", a.downReason)
	}
	if !a.recoveryFloorFull {
		t.Fatal("no full-history floor recorded for a delay without a processed cursor")
	}

	// The backlog drains: processing and the user alive are current again.
	a.onMessageProcessingEnded(now.Add(time.Second))
	a.onAlive(evAlive{timestamp: types.MessageTimestamp{Created: now.Add(time.Second)}, isSubscribed: true, messageInterest: types.AllMessageInterest})
	if err := a.systemAliveReceived(aliveAt(now.Add(time.Second)), true); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 2)
	if got := hits.lastAfterMillis.Load(); got != 0 {
		t.Fatalf("recovery after=%d (%v), want the full history (no after)", got, time.UnixMilli(got))
	}
	// Completing it covers the floor.
	if err := a.snapshotRecoveryFinished(a.currentRecovery.recoveryID); err != nil {
		t.Fatal(err)
	}
	if a.recoveryFloorFull || a.isFlaggedDown() {
		t.Fatalf("after the full recovery: floorFull=%v down=%v, want the floor discharged and the producer up", a.recoveryFloorFull, a.isFlaggedDown())
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// The same delay with a resume point (the first recovery after an open, with
// nothing processed in this run yet): the floor is a timestamp floor at the
// resume point, NOT the full-history floor. A full floor would linger — a
// recovery from the resume point does not discharge it — and some later,
// unrelated recovery would silently become a full-history one.
func TestActor_DelayDownBeforeAnythingWasProcessed_WithAResumePointFloorsAtIt(t *testing.T) {
	fake := newFakeManagerOps()
	now := time.Now().Truncate(time.Millisecond)
	a, hits := steadyActor(t, fake, now.Add(-10*time.Second))
	resume := now.Add(-5 * time.Minute)
	if err := a.pm.SetProducerRecoveryFromTimestamp(t.Context(), a.producerID, resume); err != nil {
		t.Fatal(err)
	}
	if lp, _ := a.lastProcessedMessageGenTimestamp(); !lp.IsZero() {
		t.Fatalf("setup: processed cursor = %v, want none", lp)
	}

	a.lastUserSessionAlive = now.Add(-time.Hour)
	a.onTick(now, true)
	if a.downReason != types.ProcessingQueueDelayViolationProducerDownReason {
		t.Fatalf("setup: down reason = %v, want the processing delay", a.downReason)
	}
	if a.recoveryFloorFull {
		t.Fatal("full-history floor recorded although a resume point exists")
	}
	if !a.recoveryFloor.Equal(resume) {
		t.Fatalf("floor = %v, want the resume point %v", a.recoveryFloor, resume)
	}

	// The way back recovers from the resume point, not from the full history.
	a.onMessageProcessingEnded(now.Add(time.Second))
	a.onAlive(evAlive{timestamp: types.MessageTimestamp{Created: now.Add(time.Second)}, isSubscribed: true, messageInterest: types.AllMessageInterest})
	if err := a.systemAliveReceived(aliveAt(now.Add(time.Second)), true); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 2)
	if got := hits.lastAfterMillis.Load(); got != resume.UnixMilli() {
		t.Fatalf("recovery after=%d, want the resume point %d", got, resume.UnixMilli())
	}
	if err := a.snapshotRecoveryFinished(a.currentRecovery.recoveryID); err != nil {
		t.Fatal(err)
	}
	if a.recoveryFloorFull || !a.recoveryFloor.IsZero() || a.isFlaggedDown() {
		t.Fatalf("after the recovery: floorFull=%v floor=%v down=%v, want everything discharged and the producer up",
			a.recoveryFloorFull, a.recoveryFloor, a.isFlaggedDown())
	}

	// A later, unrelated recovery is NOT a full-history one.
	if err := a.systemAliveReceived(aliveAt(now.Add(2*time.Second)), false); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 3)
	if got := hits.lastAfterMillis.Load(); got == 0 {
		t.Fatal("a later unrelated recovery asked for the full history")
	}
	waitDetachedDone(t, a, 3*time.Second)
}
