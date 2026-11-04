package recovery

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

	// The recovery runs while processing stays current: the producer is down
	// for the earlier delay, but no delay begins during the request, so
	// nothing calls for a repeat below.
	a.onMessageProcessingEnded(now.Add(time.Second))
	a.onTick(now.Add(2*time.Second), true)
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
		t.Fatalf("recovery POSTs = %d, want 2: a delay down BEFORE the request needs no repeat", hits.recover.Load())
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

// procClock replaces the producer manager's last processed timestamp (the
// lookup seam of the actor) with one a test sets, so that "processing lags"
// and "processing is current" do not depend on the monotonic cursor of the
// manager or on the wall clock a completion reads.
type procClock struct{ v atomic.Int64 }

func (p *procClock) set(t time.Time) { p.v.Store(t.UnixNano()) }

// holdProcessing installs a procClock on the actor, behind by a minute.
func holdProcessing(a *recoveryActor) *procClock {
	p := &procClock{}
	p.set(time.Now().Add(-time.Minute))
	a.lastProcessedFn = func() (time.Time, error) { return time.Unix(0, p.v.Load()), nil }
	return p
}

func tickAt(a *recoveryActor, at time.Time) {
	a.lastSystemAlive = &at
	a.onTick(at, true)
}

// lag makes processing stand a minute behind `at` and ticks there.
func lag(a *recoveryActor, p *procClock, at time.Time) {
	p.set(at.Add(-time.Minute))
	tickAt(a, at)
}

// catchUp makes processing current at `at` and ticks there.
func catchUp(a *recoveryActor, p *procClock, at time.Time) {
	p.set(at)
	tickAt(a, at)
}

// A processing delay that begins while a snapshot recovery is already
// running does not interrupt it (that exemption keeps a recovery's own
// replay from looping on itself), but the broker may drop messages after
// that request went out. Its completion does not report the producer up:
// the recovery is repeated once processing is current again, from the point
// it started at.
func TestActor_DelayDownDuringRecovery_RepeatedAfterTheBacklogDrains(t *testing.T) {
	fake := newFakeManagerOps()
	a, hits, r1, _ := genuineDelayDuringRecovery(t, fake)
	p := holdProcessing(a)
	from1 := a.currentRecovery.recoverFrom
	eventsBefore := len(producerStatusEvents(fake))

	// R1 completes while processing still lags: nothing is requested onto
	// the backlog, the producer stays down, the repeat waits.
	if err := a.snapshotRecoveryFinished(r1); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if hits.recover.Load() != 2 {
		t.Fatalf("recovery POSTs = %d, want 2: no repeat onto a backlog", hits.recover.Load())
	}
	if !a.delayRepeatWaiting || a.recoveryState != types.CompletedRecoveryState || !a.isFlaggedDown() {
		t.Fatalf("after R1: waiting=%v state=%v down=%v, want waiting, Completed, down", a.delayRepeatWaiting, a.recoveryState, a.isFlaggedDown())
	}

	// The backlog drains: the next tick issues the repeat from R1's start
	// (not from where processing stood, which lies past the replay's head).
	catchUp(a, p, time.Now())
	waitRecoverHits(t, hits, 3)
	if got := hits.lastAfterMillis.Load(); got != from1.UnixMilli() {
		t.Fatalf("repeat after=%d (%v), want R1's start %d (%v)", got, time.UnixMilli(got), from1.UnixMilli(), from1)
	}
	r2 := a.currentRecovery.recoveryID
	if r2 == r1 || a.recoveryState != types.StartedRecoveryState || !a.isFlaggedDown() || a.delayRepeatWaiting {
		t.Fatalf("after the catch-up: id=%d (R1 %d) state=%v down=%v waiting=%v, want a new Started recovery and the producer down",
			r2, r1, a.recoveryState, a.isFlaggedDown(), a.delayRepeatWaiting)
	}
	for _, ev := range producerStatusEvents(fake)[eventsBefore:] {
		if !ev.IsDown() {
			t.Fatal("producer reported up before the repeat completed")
		}
	}

	// The repeat completes without a delay: the producer is up, the chain
	// state is cleared.
	if err := a.snapshotRecoveryFinished(r2); err != nil {
		t.Fatal(err)
	}
	if hits.recover.Load() != 3 || a.isFlaggedDown() || a.recoveryState != types.CompletedRecoveryState {
		t.Fatalf("after the repeat: POSTs=%d down=%v state=%v, want 3, up, Completed", hits.recover.Load(), a.isFlaggedDown(), a.recoveryState)
	}
	if a.delayRepeats != 0 || a.delayDownRequestID != 0 || a.delayRepeatWaiting {
		t.Fatalf("chain state not reset: repeats=%d id=%d waiting=%v", a.delayRepeats, a.delayDownRequestID, a.delayRepeatWaiting)
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// A delay that begins during a repeat counts the same way: it is repeated
// again, not left for the return path.
func TestActor_DelayDownDuringARepeat_RepeatsAgain(t *testing.T) {
	fake := newFakeManagerOps()
	a, hits, r1, _ := genuineDelayDuringRecovery(t, fake)
	p := holdProcessing(a)
	from1 := a.currentRecovery.recoverFrom
	if err := a.snapshotRecoveryFinished(r1); err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	catchUp(a, p, at)
	waitRecoverHits(t, hits, 3)
	r2 := a.currentRecovery.recoveryID

	// Processing falls behind again during the repeat (the producer is
	// already down for the delay).
	lagAt := at.Add(time.Second)
	lag(a, p, lagAt)
	if a.recoveryState != types.StartedRecoveryState || a.currentRecovery.recoveryID != r2 {
		t.Fatalf("the delay interrupted the repeat: state=%v id=%d", a.recoveryState, a.currentRecovery.recoveryID)
	}
	if err := a.snapshotRecoveryFinished(r2); err != nil {
		t.Fatal(err)
	}
	if !a.delayRepeatWaiting || !a.isFlaggedDown() {
		t.Fatalf("after R2: waiting=%v down=%v, want another repeat waiting and the producer down", a.delayRepeatWaiting, a.isFlaggedDown())
	}
	catchUp(a, p, at.Add(2*time.Second))
	waitRecoverHits(t, hits, 4)
	// The delay during the repeat began with processing a minute behind
	// lagAt, which is before the first recovery's start: the repeat reaches
	// back to that cursor.
	if want := lagAt.Add(-time.Minute); !want.Before(from1) || hits.lastAfterMillis.Load() != want.UnixMilli() {
		t.Fatalf("second repeat after=%d, want the cursor %d (before %v)", hits.lastAfterMillis.Load(), want.UnixMilli(), from1)
	}
	r3 := a.currentRecovery.recoveryID
	if err := a.snapshotRecoveryFinished(r3); err != nil {
		t.Fatal(err)
	}
	if a.isFlaggedDown() || hits.recover.Load() != 4 {
		t.Fatalf("POSTs=%d down=%v, want 4 and the producer up", hits.recover.Load(), a.isFlaggedDown())
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// A consumer that lags during every attempt gets maxDelayRepeats repeats,
// then the producer is reported up anyway with ONE warn that says so.
func TestActor_DelayDownDuringEveryAttempt_BoundedThenUpWithAWarn(t *testing.T) {
	fake := newFakeManagerOps()
	a, hits, r1, _ := genuineDelayDuringRecovery(t, fake)
	p := holdProcessing(a)
	sink := &lockedBuffer{}
	a.logger = debugLogger(sink)

	id := r1
	at := time.Now()
	for i := 1; i <= maxDelayRepeats; i++ {
		if err := a.snapshotRecoveryFinished(id); err != nil {
			t.Fatal(err)
		}
		if !a.delayRepeatWaiting || !a.isFlaggedDown() {
			t.Fatalf("attempt %d: waiting=%v down=%v, want a repeat waiting", i, a.delayRepeatWaiting, a.isFlaggedDown())
		}
		catchUp(a, p, at)
		waitRecoverHits(t, hits, int32(2+i))
		id = a.currentRecovery.recoveryID
		at = at.Add(time.Second)
		lag(a, p, at) // the repeat lags too
		at = at.Add(time.Second)
	}
	// The last repeat falls behind as well: no further repeat.
	if err := a.snapshotRecoveryFinished(id); err != nil {
		t.Fatal(err)
	}
	if a.isFlaggedDown() || a.recoveryState != types.CompletedRecoveryState || a.delayRepeatWaiting {
		t.Fatalf("after the bound: down=%v state=%v waiting=%v, want up, Completed, nothing waiting", a.isFlaggedDown(), a.recoveryState, a.delayRepeatWaiting)
	}
	if got := hits.recover.Load(); got != int32(2+maxDelayRepeats) {
		t.Fatalf("recovery POSTs = %d, want %d (the first plus %d repeats)", got, 2+maxDelayRepeats, maxDelayRepeats)
	}
	if n := strings.Count(sink.String(), "processing fell behind during every recovery attempt"); n != 1 {
		t.Fatalf("bound warn logged %d times, want 1:\n%s", n, sink.String())
	}
	if !strings.Contains(sink.String(), "producer_id=") || !strings.Contains(sink.String(), "attempts=4") {
		t.Fatalf("the warn must carry producer_id and the attempts:\n%s", sink.String())
	}
	if a.delayRepeats != 0 {
		t.Fatalf("repeat counter = %d, want it reset when the chain ended", a.delayRepeats)
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// The repeat waits while processing lags and is issued on the first tick
// that finds it current; ticks in between issue nothing and do not report
// the producer up, however long they last.
func TestActor_DelayRepeat_WaitsWhileTimingIsStale(t *testing.T) {
	fake := newFakeManagerOps()
	a, hits, r1, _ := genuineDelayDuringRecovery(t, fake)
	p := holdProcessing(a)
	if err := a.snapshotRecoveryFinished(r1); err != nil {
		t.Fatal(err)
	}
	base := time.Now()
	for i := 1; i <= 5; i++ {
		lag(a, p, base.Add(time.Duration(i)*time.Second))
	}
	time.Sleep(50 * time.Millisecond)
	if hits.recover.Load() != 2 || !a.delayRepeatWaiting || !a.isFlaggedDown() || a.recoveryState != types.CompletedRecoveryState {
		t.Fatalf("stale ticks: POSTs=%d waiting=%v down=%v state=%v, want 2, waiting, down, Completed",
			hits.recover.Load(), a.delayRepeatWaiting, a.isFlaggedDown(), a.recoveryState)
	}
	// The first tick that finds processing current issues it.
	catchUp(a, p, base.Add(6*time.Second))
	waitRecoverHits(t, hits, 3)
	if a.delayRepeatWaiting {
		t.Fatal("repeat still waiting after a fresh tick")
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// A timing lookup that fails is not "processing is current": the repeat
// keeps waiting.
func TestActor_DelayRepeat_TimingLookupErrorMeansNotYet(t *testing.T) {
	fake := newFakeManagerOps()
	a, hits, r1, _ := genuineDelayDuringRecovery(t, fake)
	p := holdProcessing(a)
	at := time.Now()
	failing := true
	lookup := a.lastProcessedFn
	a.lastProcessedFn = func() (time.Time, error) {
		if failing {
			return time.Time{}, errors.New("lookup failed")
		}
		return lookup()
	}
	if err := a.snapshotRecoveryFinished(r1); err != nil {
		t.Fatal(err)
	}
	p.set(at) // processing is current, but the lookup fails
	a.serviceDelayRepeat(at)
	time.Sleep(50 * time.Millisecond)
	if hits.recover.Load() != 2 || !a.delayRepeatWaiting {
		t.Fatalf("POSTs=%d waiting=%v, want 2 and still waiting after a failed lookup", hits.recover.Load(), a.delayRepeatWaiting)
	}
	failing = false
	a.serviceDelayRepeat(at)
	waitRecoverHits(t, hits, 3)
	waitDetachedDone(t, a, 3*time.Second)
}

// Waiting for processing to catch up is bounded by the chain's
// MaxRecoveryExecution: then the producer is reported up with the warn.
func TestActor_DelayRepeat_WaitingBeyondMaxRecoveryExecutionReportsUp(t *testing.T) {
	fake := newFakeManagerOps()
	a, hits, r1, _ := genuineDelayDuringRecovery(t, fake)
	p := holdProcessing(a)
	sink := &lockedBuffer{}
	a.logger = debugLogger(sink)
	if err := a.snapshotRecoveryFinished(r1); err != nil {
		t.Fatal(err)
	}
	chainStart := a.currentRecovery.chainStartedAt

	// Still within the cap: keeps waiting while processing lags.
	lag(a, p, chainStart.Add(a.cfg.MaxRecoveryExecution()-time.Minute))
	if !a.delayRepeatWaiting || !a.isFlaggedDown() {
		t.Fatalf("within the cap: waiting=%v down=%v, want to keep waiting", a.delayRepeatWaiting, a.isFlaggedDown())
	}

	// Past it: up, with the warn, and no repeat was ever requested.
	// (Processing is current then, so the tick does not flag a new delay
	// right after.)
	catchUpAfterCap := chainStart.Add(a.cfg.MaxRecoveryExecution() + time.Minute)
	catchUp(a, p, catchUpAfterCap)
	if a.isFlaggedDown() || a.delayRepeatWaiting || a.recoveryState != types.CompletedRecoveryState {
		t.Fatalf("past the cap: down=%v waiting=%v state=%v, want up, nothing waiting, Completed", a.isFlaggedDown(), a.delayRepeatWaiting, a.recoveryState)
	}
	if hits.recover.Load() != 2 {
		t.Fatalf("recovery POSTs = %d, want 2", hits.recover.Load())
	}
	if n := strings.Count(sink.String(), "processing fell behind during every recovery attempt"); n != 1 {
		t.Fatalf("warn logged %d times, want 1:\n%s", n, sink.String())
	}
	evs := producerStatusEvents(fake)
	if last := evs[len(evs)-1]; last.IsDown() {
		t.Fatal("last status is still down")
	}
}

// While the repeat waits the recovery state is Completed with the producer
// down for the delay. The alive path must not start a recovery of its own
// (the return path would otherwise issue a second one, without the repeat's
// start point) and the producer-timing tick must not mistake the waiting for
// a stuck recovery.
func TestActor_DelayRepeat_WaitingDoesNotMisfire(t *testing.T) {
	fake := newFakeManagerOps()
	a, hits, r1, _ := genuineDelayDuringRecovery(t, fake)
	p := holdProcessing(a)
	if err := a.snapshotRecoveryFinished(r1); err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	p.set(at)

	// Processing is current and an alive arrives before any tick: the alive
	// path leaves the issue to the tick.
	if err := a.systemAliveReceived(aliveAt(at), true); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if hits.recover.Load() != 2 || !a.delayRepeatWaiting {
		t.Fatalf("alive while waiting: POSTs=%d waiting=%v, want 2 and still waiting", hits.recover.Load(), a.delayRepeatWaiting)
	}

	// A silent stretch past the snapshot-complete deadline is not a lost
	// recovery: nothing is in flight to re-issue.
	silent := a.currentRecovery.recoveryStartedAt.Add(10 * time.Minute)
	a.expireSnapshotCompleteDeadline(silent)
	a.expireStuckSnapshotRecovery(silent)
	if hits.recover.Load() != 2 || a.recoveryState != types.CompletedRecoveryState || !a.delayRepeatWaiting {
		t.Fatalf("sweeps while waiting: POSTs=%d state=%v waiting=%v", hits.recover.Load(), a.recoveryState, a.delayRepeatWaiting)
	}

	// The tick issues the repeat once, and a second tick does not stack one.
	tickAt(a, at)
	waitRecoverHits(t, hits, 3)
	tickAt(a, at)
	time.Sleep(50 * time.Millisecond)
	if hits.recover.Load() != 3 {
		t.Fatalf("recovery POSTs = %d, want exactly one repeat", hits.recover.Load())
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// Another down reason taking over while the repeat waits (here the alive
// stream going silent) ends the chain: the alive path starts an ordinary
// recovery, and what the repeat was to cover is kept as a floor so that
// recovery reaches back to it.
func TestActor_DelayRepeat_AnotherDownReasonTakesOver(t *testing.T) {
	fake := newFakeManagerOps()
	a, hits, r1, _ := genuineDelayDuringRecovery(t, fake)
	p := holdProcessing(a)
	from1 := a.currentRecovery.recoverFrom
	if err := a.snapshotRecoveryFinished(r1); err != nil {
		t.Fatal(err)
	}
	// The next tick finds the alive stream silent (lastSystemAlive stale).
	old := time.Now().Add(-time.Hour)
	a.lastSystemAlive = &old
	a.onTick(time.Now().Add(31*time.Second), true)
	if a.downReason != types.AliveInternalViolationProducerDownReason || a.delayRepeatWaiting {
		t.Fatalf("down reason %v waiting=%v, want the alive violation and no repeat waiting", a.downReason, a.delayRepeatWaiting)
	}
	if !a.recoveryFloor.Equal(from1) {
		t.Fatalf("floor = %v, want the repeat's start %v kept", a.recoveryFloor, from1)
	}
	now := time.Now().Add(32 * time.Second)
	p.set(now)
	if err := a.systemAliveReceived(aliveAt(now), true); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 3)
	if got := hits.lastAfterMillis.Load(); got != from1.UnixMilli() {
		t.Fatalf("recovery after=%d, want the kept start %d", got, from1.UnixMilli())
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// The repeat of a FULL-HISTORY recovery (nothing to remember as a timestamp)
// that another down reason takes over keeps the full-history floor: the
// recovery the alive path starts asks for everything, not for the window
// from the then-current cursor, and the delay chain is reset.
func TestActor_DelayRepeat_AnotherDownReasonTakesOver_FullHistoryKeepsTheFullFloor(t *testing.T) {
	fake := newFakeManagerOps()
	a, hits, r1, _ := genuineDelayDuringRecovery(t, fake)
	p := holdProcessing(a)
	if err := a.snapshotRecoveryFinished(r1); err != nil {
		t.Fatal(err)
	}
	if !a.delayRepeatWaiting {
		t.Fatal("setup: no repeat waiting")
	}
	// The waiting repeat is one of a full-history recovery: no timestamp.
	a.delayRepeatFrom = time.Time{}
	a.recoveryFloor = time.Time{}
	a.delayRepeats = 2
	if a.recoveryFloorFull {
		t.Fatal("setup: the full floor is already set")
	}

	old := time.Now().Add(-time.Hour)
	a.lastSystemAlive = &old
	a.onTick(time.Now().Add(31*time.Second), true)
	if a.downReason != types.AliveInternalViolationProducerDownReason || a.delayRepeatWaiting {
		t.Fatalf("down reason %v waiting=%v, want the alive violation and no repeat waiting", a.downReason, a.delayRepeatWaiting)
	}
	if !a.recoveryFloorFull {
		t.Fatal("the full-history floor was lost when another down reason took the repeat over")
	}
	if a.delayDownRequestID != 0 || a.delayRepeats != 0 || !a.delayCursor.IsZero() || !a.delayRepeatFrom.IsZero() {
		t.Fatalf("chain not reset: request=%d repeats=%d cursor=%v from=%v",
			a.delayDownRequestID, a.delayRepeats, a.delayCursor, a.delayRepeatFrom)
	}

	now := time.Now().Add(32 * time.Second)
	p.set(now)
	if err := a.systemAliveReceived(aliveAt(now), true); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 3)
	if got := hits.lastAfterMillis.Load(); got != 0 {
		t.Fatalf("recovery after=%d (%v), want the full history (no after)", got, time.UnixMilli(got))
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// The deadline re-issue of the request that was in flight when the delay
// began keeps the mark: the replacement's completion repeats it.
func TestActor_DelayDownDuringRecovery_RepeatSurvivesADeadlineReissue(t *testing.T) {
	fake := newFakeManagerOps()
	a, hits, r1, _ := genuineDelayDuringRecovery(t, fake)
	from1 := a.currentRecovery.recoverFrom
	started := a.currentRecovery.recoveryStartedAt

	// R1 goes silent past the deadline with processing current (a lagging
	// processing would hold the re-issue back) and is replaced by R2.
	p := holdProcessing(a)
	tickAt := started.Add(6 * time.Minute)
	p.set(tickAt)
	a.lastSystemAlive = &tickAt
	a.onTick(tickAt, true)
	r2 := a.currentRecovery.recoveryID
	if r2 == r1 {
		t.Fatal("setup: no deadline re-issue")
	}
	waitRecoverHits(t, hits, 3)
	p.set(time.Now().Add(-time.Minute)) // processing lags again
	if err := a.snapshotRecoveryFinished(r2); err != nil {
		t.Fatal(err)
	}
	if !a.delayRepeatWaiting || !a.isFlaggedDown() {
		t.Fatalf("after R2: waiting=%v down=%v, want the repeat waiting and the producer down", a.delayRepeatWaiting, a.isFlaggedDown())
	}
	catchUp(a, p, tickAt.Add(time.Second))
	waitRecoverHits(t, hits, 4)
	if got := hits.lastAfterMillis.Load(); got != from1.UnixMilli() {
		t.Fatalf("repeat after=%d, want %d", got, from1.UnixMilli())
	}
	r3 := a.currentRecovery.recoveryID
	if err := a.snapshotRecoveryFinished(r3); err != nil {
		t.Fatal(err)
	}
	if hits.recover.Load() != 4 || a.isFlaggedDown() {
		t.Fatalf("POSTs=%d down=%v, want exactly one repeat and the producer up", hits.recover.Load(), a.isFlaggedDown())
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// A delay flagged during the replay of an ordinary recovery after an outage
// has a stale cursor (old because of the outage the recovery is repairing),
// at or before the recovery's own start. The recovery is repeated all the
// same, from the cursor: the dropped messages can be the replay itself.
func TestActor_DelayDownDuringOutageRecovery_IsRepeated(t *testing.T) {
	fake := newFakeManagerOps()
	now := time.Now().Truncate(time.Millisecond)
	a, hits := steadyActor(t, fake, now.Add(-10*time.Second))
	if err := a.systemAliveReceived(aliveAt(now.Add(-8*time.Second)), true); err != nil {
		t.Fatal(err)
	}
	a.onMessageProcessingEnded(now.Add(-2 * time.Minute)) // stale because of the outage
	r1 := startRecoveryAfterLoss(t, a, hits, now)
	from1 := a.currentRecovery.recoverFrom

	a.onTick(now, true)
	if a.downReason != types.ProcessingQueueDelayViolationProducerDownReason {
		t.Fatalf("setup: down reason = %v, want the delay flagged during the replay", a.downReason)
	}
	if a.recoveryFloor.After(a.currentRecovery.recoverFrom) {
		t.Fatalf("setup: floor %v later than recoverFrom %v", a.recoveryFloor, a.currentRecovery.recoverFrom)
	}
	p := holdProcessing(a)
	if err := a.snapshotRecoveryFinished(r1); err != nil {
		t.Fatal(err)
	}
	if !a.delayRepeatWaiting || !a.isFlaggedDown() {
		t.Fatalf("waiting=%v down=%v, want the repeat waiting and the producer down", a.delayRepeatWaiting, a.isFlaggedDown())
	}
	catchUp(a, p, now.Add(time.Second))
	waitRecoverHits(t, hits, 3)
	// The delay cursor (where processing stood, before the outage) lies
	// before the recovery's own start: the repeat reaches back to it.
	stood := now.Add(-2 * time.Minute)
	if got := hits.lastAfterMillis.Load(); got != stood.UnixMilli() || !stood.Before(from1) {
		t.Fatalf("repeat after=%d, want the cursor %d (before the first recovery's start %v)", got, stood.UnixMilli(), from1)
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// Without a delay during the recovery there is no repeat.
func TestActor_RecoveryWithoutDelay_NoRepeat(t *testing.T) {
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

// The repeat starts at the completed recovery's own start, which here is the
// older floor (an interrupted recovery restarted from its floor): the older
// point is where the replay began, and the replay is what the broker may have
// dropped. The floor is discharged by the recovery that reached it, so the
// repeat does not pull it in a second time.
func TestActor_DelayDownDuringRecovery_RepeatStartsAtTheRecoverysOwnStart(t *testing.T) {
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

	p := holdProcessing(a)
	if err := a.snapshotRecoveryFinished(r1); err != nil {
		t.Fatal(err)
	}
	if !a.recoveryFloor.IsZero() {
		t.Fatalf("floor %v not discharged by the recovery that reached it", a.recoveryFloor)
	}
	catchUp(a, p, time.Now())
	waitRecoverHits(t, hits, 3)
	if got := hits.lastAfterMillis.Load(); got != floor.UnixMilli() {
		t.Fatalf("repeat after=%d (%v), want the recovery's start %d (%v)", got, time.UnixMilli(got), floor.UnixMilli(), floor)
	}
	if !a.isFlaggedDown() || a.recoveryState != types.StartedRecoveryState {
		t.Fatalf("down=%v state=%v, want the producer down and the repeat Started", a.isFlaggedDown(), a.recoveryState)
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// When the delay cursor lies BEFORE the recovery's start the repeat reaches
// back to the cursor.
func TestActor_DelayDownDuringRecovery_RepeatReachesBackToAnEarlierCursor(t *testing.T) {
	fake := newFakeManagerOps()
	a, hits, r1, _ := genuineDelayDuringRecovery(t, fake)
	p := holdProcessing(a)
	earlier := a.currentRecovery.recoverFrom.Add(-time.Minute)
	a.delayCursor = earlier
	if err := a.snapshotRecoveryFinished(r1); err != nil {
		t.Fatal(err)
	}
	catchUp(a, p, time.Now())
	waitRecoverHits(t, hits, 3)
	if got := hits.lastAfterMillis.Load(); got != earlier.UnixMilli() {
		t.Fatalf("repeat after=%d, want the earlier cursor %d", got, earlier.UnixMilli())
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// A recovery that started from zero asked for the full history. A delay
// during it is repeated like any other, and the repeat asks for the full
// history again.
func TestActor_DelayDownDuringFullHistoryRecovery_IsRepeatedFromTheFullHistory(t *testing.T) {
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
	if !a.delayRepeatWaiting || !a.isFlaggedDown() {
		t.Fatalf("waiting=%v down=%v, want the repeat waiting and the producer down", a.delayRepeatWaiting, a.isFlaggedDown())
	}
	// A window for producers without a cursor (WithInitialSnapshotTime) must
	// not narrow the repeat of a full-history recovery.
	a.initialSnapshotTime = time.Hour
	catchUp(a, p, time.Now())
	waitRecoverHits(t, hits, 3)
	if got := hits.lastAfterMillis.Load(); got != 0 {
		t.Fatalf("repeat after=%d (%v), want none (the full history)", got, time.UnixMilli(got))
	}
	if !a.currentRecovery.recoverFrom.IsZero() || a.recoveryState != types.StartedRecoveryState {
		t.Fatalf("repeat recoverFrom=%v state=%v, want zero and Started", a.currentRecovery.recoverFrom, a.recoveryState)
	}
	if err := a.snapshotRecoveryFinished(a.currentRecovery.recoveryID); err != nil {
		t.Fatal(err)
	}
	if a.isFlaggedDown() || a.recoveryFloorFull {
		t.Fatalf("down=%v floorFull=%v, want up and no floor", a.isFlaggedDown(), a.recoveryFloorFull)
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// A delay with nothing processed yet (a stale user-session alive flags it
// during a recovery from a resume point) has a zero cursor; the recovery is
// repeated from the resume point all the same.
func TestActor_DelayDownWithAZeroCursorDuringRecovery_IsRepeated(t *testing.T) {
	fake := newFakeManagerOps()
	now := time.Now().Truncate(time.Millisecond)
	srv, hits := fixtureSrv(t)
	t.Cleanup(srv.Close)
	a := newWiredActor(t, srv, fake)
	resume := now.Add(-5 * time.Minute)
	if err := a.pm.SetProducerRecoveryFromTimestamp(t.Context(), a.producerID, resume); err != nil {
		t.Fatal(err)
	}
	if err := a.systemAliveReceived(aliveAt(now), true); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 1)
	r1 := a.currentRecovery.recoveryID
	if !a.currentRecovery.recoverFrom.Equal(resume) {
		t.Fatalf("setup: recoverFrom = %v, want the resume point %v", a.currentRecovery.recoverFrom, resume)
	}
	if lp, _ := a.lastProcessedMessageGenTimestamp(); !lp.IsZero() {
		t.Fatalf("setup: processed cursor = %v, want none", lp)
	}

	// The user-session alive is stale: the delay begins during R1.
	a.lastUserSessionAlive = now.Add(-time.Hour)
	a.lastSystemAlive = &now
	a.onTick(now, true)
	if a.downReason != types.ProcessingQueueDelayViolationProducerDownReason || a.delayDownRequestID != r1 || !a.delayCursor.IsZero() {
		t.Fatalf("setup: reason=%v marked=%d (R1 %d) cursor=%v, want the delay marked on R1 with a zero cursor",
			a.downReason, a.delayDownRequestID, r1, a.delayCursor)
	}
	if err := a.snapshotRecoveryFinished(r1); err != nil {
		t.Fatal(err)
	}
	if !a.delayRepeatWaiting || !a.isFlaggedDown() {
		t.Fatalf("waiting=%v down=%v, want the repeat waiting and the producer down", a.delayRepeatWaiting, a.isFlaggedDown())
	}
	// The user-session alive is current again.
	a.lastUserSessionAlive = now.Add(time.Second)
	a.onTick(now.Add(2*time.Second), true)
	waitRecoverHits(t, hits, 2)
	if got := hits.lastAfterMillis.Load(); got != resume.UnixMilli() {
		t.Fatalf("repeat after=%d, want the resume point %d", got, resume.UnixMilli())
	}
	waitDetachedDone(t, a, 3*time.Second)
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

// The repeat starts at the recovery's own start, never later: the lookback
// clamp moves with the clock, and the repeat waits for processing to catch
// up, so a recovery that started near the limit would otherwise be repeated
// from a later point and could not refill the head of its own replay.
func TestActor_DelayRepeat_NeverStartsLaterThanTheRecoveryItRepeats(t *testing.T) {
	a, hits, resolved, started, id := lookbackActor(t)
	p := holdProcessing(a)

	// The recovery completed while processing lagged: the repeat waits.
	a.recoveryState = types.CompletedRecoveryState
	a.delayRepeatWaiting = true
	a.delayRepeatFrom = resolved
	a.serviceDelayRepeat(started.Add(time.Second))
	time.Sleep(50 * time.Millisecond)
	if hits.recover.Load() != 1 || !a.delayRepeatWaiting {
		t.Fatalf("POSTs=%d waiting=%v, want 1 and the repeat waiting while processing lags", hits.recover.Load(), a.delayRepeatWaiting)
	}

	// Real time moves on until a fresh clamp WOULD move the same point,
	// then processing is current.
	waitUntilOlderThan(t, resolved, 2*time.Second)
	p.set(time.Now())
	a.serviceDelayRepeat(started.Add(1500 * time.Millisecond))
	if a.currentRecovery.recoveryID == id {
		t.Fatal("setup: no repeat was issued")
	}
	waitRecoverHits(t, hits, 2)
	if got := hits.lastAfterMillis.Load(); got != resolved.UnixMilli() {
		t.Fatalf("repeat after=%d, want the original %d (moved forward by %v)", got, resolved.UnixMilli(), time.UnixMilli(got).Sub(resolved))
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// waitingRepeat brings an actor to a delay repeat that waits: R1 completed
// while processing lagged. Processing is made current afterwards, so only
// the guard under test can hold the repeat back.
func waitingRepeat(t *testing.T, fake *fakeManagerOps) (*recoveryActor, *recoveryHits, time.Time) {
	t.Helper()
	a, hits, r1, _ := genuineDelayDuringRecovery(t, fake)
	p := holdProcessing(a)
	if err := a.snapshotRecoveryFinished(r1); err != nil {
		t.Fatal(err)
	}
	if !a.delayRepeatWaiting {
		t.Fatal("setup: no repeat waiting")
	}
	at := time.Now()
	p.set(at)
	return a, hits, at
}

// A lost session has no queue until it re-binds: a repeat issued then would
// be requested for a queue that is not there. The repeat waits for the
// re-bind and goes out on the next tick.
func TestActor_DelayRepeat_WaitsForALostSessionToRebind(t *testing.T) {
	fake := newFakeManagerOps()
	a, hits, at := waitingRepeat(t, fake)

	fake.pendingRebind.Store(true)
	tickAt(a, at)
	time.Sleep(50 * time.Millisecond)
	if hits.recover.Load() != 2 || !a.delayRepeatWaiting || a.recoveryState != types.CompletedRecoveryState {
		t.Fatalf("before the re-bind: POSTs=%d waiting=%v state=%v, want 2, waiting, Completed",
			hits.recover.Load(), a.delayRepeatWaiting, a.recoveryState)
	}

	fake.pendingRebind.Store(false)
	tickAt(a, at.Add(time.Second))
	waitRecoverHits(t, hits, 3)
	if a.delayRepeatWaiting {
		t.Fatal("the repeat still waits after the re-bind")
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// A producer that is disabled has no recovery to run: the repeat is not
// issued for it, and it stays waiting for the producer to be enabled again.
func TestActor_DelayRepeat_NotIssuedForADisabledProducer(t *testing.T) {
	fake := newFakeManagerOps()
	a, hits, at := waitingRepeat(t, fake)

	if err := a.pm.SetProducerState(t.Context(), a.producerID, false); err != nil {
		t.Fatal(err)
	}
	a.serviceDelayRepeat(at)
	time.Sleep(50 * time.Millisecond)
	if hits.recover.Load() != 2 || !a.delayRepeatWaiting {
		t.Fatalf("disabled: POSTs=%d waiting=%v, want 2 and the repeat still waiting", hits.recover.Load(), a.delayRepeatWaiting)
	}

	if err := a.pm.SetProducerState(t.Context(), a.producerID, true); err != nil {
		t.Fatal(err)
	}
	a.serviceDelayRepeat(at.Add(time.Second))
	waitRecoverHits(t, hits, 3)
	waitDetachedDone(t, a, 3*time.Second)
}

// A repeat that cannot be requested changes nothing (the request fails
// before it touches state): the repeat stays waiting with the chain's start
// and the next tick retries.
func TestActor_DelayRepeat_FailedIssueLeavesTheStateAndRetries(t *testing.T) {
	fake := newFakeManagerOps()
	a, hits, at := waitingRepeat(t, fake)
	done := a.currentRecovery
	chain := done.chainStartedAt
	from := a.delayRepeatFrom

	// The request cannot be made: the producer is enabled, but the catalog
	// cannot describe it (an unknown scope), so its name is not available.
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<?xml version="1.0"?><producers response_code="OK">`+
			`<producer id="1" name="live" description="Live" active="true" api_url="https://x" scope="bogus" stateful_recovery_window_in_minutes="60"/></producers>`)
	}))
	defer badSrv.Close()
	realPM := a.pm
	a.pm = newProducerManagerFor(t, badSrv)
	a.serviceDelayRepeat(at)
	if a.currentRecovery != done || !a.currentRecovery.chainStartedAt.Equal(chain) || !a.delayRepeatWaiting ||
		a.delayRepeats != 0 || !a.delayRepeatFrom.Equal(from) || a.recoveryState != types.CompletedRecoveryState {
		t.Fatalf("a failed repeat changed the state: same=%v waiting=%v repeats=%d from=%v state=%v",
			a.currentRecovery == done, a.delayRepeatWaiting, a.delayRepeats, a.delayRepeatFrom, a.recoveryState)
	}
	if hits.recover.Load() != 2 {
		t.Fatalf("recovery POSTs = %d, want 2", hits.recover.Load())
	}

	// The next tick retries, and the repeat keeps the chain start.
	a.pm = realPM
	a.serviceDelayRepeat(at.Add(time.Second))
	waitRecoverHits(t, hits, 3)
	if a.delayRepeatWaiting || a.delayRepeats != 1 || !a.currentRecovery.chainStartedAt.Equal(chain) {
		t.Fatalf("after the retry: waiting=%v repeats=%d chain=%v, want issued, 1, %v",
			a.delayRepeatWaiting, a.delayRepeats, a.currentRecovery.chainStartedAt, chain)
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// A repeat belongs to its chain: MaxRecoveryExecution caps the whole chain,
// measured from the FIRST request, not from the repeat. A repeat that
// outlives it expires like any other stuck recovery, without a further one.
func TestActor_DelayRepeat_ChainExpiresMeasuredFromTheFirstRequest(t *testing.T) {
	fake := newFakeManagerOps()
	a, hits, at := waitingRepeat(t, fake)
	maxAge := a.cfg.MaxRecoveryExecution()

	// The chain began just short of the cap.
	chain := time.Now().Add(-maxAge + 10*time.Minute)
	a.currentRecovery.chainStartedAt = chain
	a.serviceDelayRepeat(at)
	waitRecoverHits(t, hits, 3)
	if !a.currentRecovery.chainStartedAt.Equal(chain) || a.recoveryState != types.StartedRecoveryState {
		t.Fatalf("repeat: chain=%v state=%v, want the first request's start %v and Started", a.currentRecovery.chainStartedAt, a.recoveryState, chain)
	}
	waitDetachedDone(t, a, 3*time.Second)

	// Past the cap from the first request, but well within it from the repeat.
	p := &procClock{}
	late := time.Now().Add(11 * time.Minute)
	p.set(late)
	a.lastProcessedFn = func() (time.Time, error) { return time.Unix(0, p.v.Load()), nil }
	tickAt(a, late)
	time.Sleep(50 * time.Millisecond)
	if a.recoveryState != types.ErrorRecoveryState || a.currentRecovery != nil {
		t.Fatalf("past the cap: state=%v current=%v, want the chain expired (Error)", a.recoveryState, a.currentRecovery)
	}
	if hits.recover.Load() != 3 || a.delayRepeatWaiting {
		t.Fatalf("past the cap: POSTs=%d waiting=%v, want 3 and no repeat", hits.recover.Load(), a.delayRepeatWaiting)
	}
}

// A processing delay whose cursor cannot be read on the tick that flags it
// still keeps a floor: the resume point, the place a recovery would start
// from. Without it the recovery on the way back started from the cursor of
// the drained backlog, past the messages the broker dropped meanwhile.
func TestActor_DelayDownWithAnUnreadableCursor_KeepsTheResumePointAsFloor(t *testing.T) {
	fake := newFakeManagerOps()
	now := time.Now().Truncate(time.Millisecond)
	a, hits := steadyActor(t, fake, now.Add(-10*time.Second))
	if err := a.systemAliveReceived(aliveAt(now.Add(-4*time.Second)), true); err != nil {
		t.Fatal(err)
	}
	resume, err := a.timestampForRecovery()
	if err != nil || resume.IsZero() {
		t.Fatalf("setup: resume point %v (%v), want one", resume, err)
	}

	p := holdProcessing(a)
	lookup := a.lastProcessedFn
	failing := true
	a.lastProcessedFn = func() (time.Time, error) {
		if failing {
			return time.Time{}, errors.New("lookup failed")
		}
		return lookup()
	}
	sink := &lockedBuffer{}
	a.logger = debugLogger(sink)

	// The lookup fails on the tick that flags the delay (a failed lookup
	// counts as stale timing).
	tickAt(a, now)
	if a.downReason != types.ProcessingQueueDelayViolationProducerDownReason {
		t.Fatalf("setup: down reason = %v, want the processing delay", a.downReason)
	}
	if !a.recoveryFloor.Equal(resume) || a.recoveryFloorFull {
		t.Fatalf("floor = %v (full=%v), want the resume point %v", a.recoveryFloor, a.recoveryFloorFull, resume)
	}
	if !strings.Contains(sink.String(), "last processed message unknown") {
		t.Fatalf("the failed lookup was not logged:\n%s", sink.String())
	}

	// The lookup works again and the backlog has drained: the return
	// recovery reaches back to the resume point, not the new cursor.
	failing = false
	back := time.Now()
	p.set(back)
	if err := a.systemAliveReceived(aliveAt(back), true); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 2)
	if got := hits.lastAfterMillis.Load(); got != resume.UnixMilli() {
		t.Fatalf("return recovery after=%d (%v), want the resume point %d (%v), not the new cursor %v",
			got, time.UnixMilli(got), resume.UnixMilli(), resume, back)
	}
	waitDetachedDone(t, a, 3*time.Second)
}
