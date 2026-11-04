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
	// The last repeat falls behind as well: no further repeat. Processing
	// is current when it completes, so the producer is reported up.
	p.set(time.Now())
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

// A waiting repeat replaced by a subscription loss: the alive that says "not
// subscribed" starts the replacement recovery directly (the producer is
// already down, so producerDown is not involved). The replacement must reach
// back to the repeat's start, not begin at the newer alive cursor, and the
// waiting state and the delay chain are cleared.
func TestActor_DelayRepeat_ReplacedByASubscriptionLoss_ReachesBackToTheRepeatsStart(t *testing.T) {
	fake := newFakeManagerOps()
	a, hits, r1, _ := genuineDelayDuringRecovery(t, fake)
	p := holdProcessing(a)
	from1 := a.currentRecovery.recoverFrom
	if err := a.snapshotRecoveryFinished(r1); err != nil {
		t.Fatal(err)
	}
	if !a.delayRepeatWaiting || !a.delayRepeatFrom.Equal(from1) {
		t.Fatalf("setup: waiting=%v from=%v, want a repeat waiting from %v", a.delayRepeatWaiting, a.delayRepeatFrom, from1)
	}
	if !a.isFlaggedDown() {
		t.Fatal("setup: the producer should be down while the repeat waits")
	}
	// The alive cursor is newer than where the repeat starts.
	now := time.Now().Add(20 * time.Second).Truncate(time.Millisecond)
	p.set(now)
	if err := a.pm.SetLastAliveReceivedGenTimestamp(a.producerID, now); err != nil {
		t.Fatal(err)
	}
	if cur, err := a.timestampForRecovery(); err != nil || !cur.After(from1) {
		t.Fatalf("setup: recovery cursor %v (err %v) must be later than the repeat's start %v", cur, err, from1)
	}

	if err := a.systemAliveReceived(aliveAt(now), false); err != nil {
		t.Fatal(err)
	}
	if a.delayRepeatWaiting || !a.delayRepeatFrom.IsZero() {
		t.Fatalf("waiting=%v from=%v, want the repeat dropped", a.delayRepeatWaiting, a.delayRepeatFrom)
	}
	if a.delayDownRequestID != 0 || a.delayRepeats != 0 || !a.delayCursor.IsZero() {
		t.Fatalf("chain not reset: request=%d repeats=%d cursor=%v", a.delayDownRequestID, a.delayRepeats, a.delayCursor)
	}
	waitRecoverHits(t, hits, 3)
	if got := hits.lastAfterMillis.Load(); got > from1.UnixMilli() {
		t.Fatalf("replacement recovery after=%d (%v), want at or before the repeat's start %v",
			got, time.UnixMilli(got), from1)
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
	p.set(time.Now()) // the repeat completes with processing current
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
	a, hits := freshActor(t, fake)
	resume := now.Add(-5 * time.Minute)
	if err := a.pm.SetProducerRecoveryFromTimestamp(t.Context(), a.producerID, resume); err != nil {
		t.Fatal(err)
	}
	if lp, _ := a.lastProcessedMessageGenTimestamp(); !lp.IsZero() {
		t.Fatalf("setup: processed cursor = %v, want none", lp)
	}

	a.lastUserSessionAlive = now.Add(-time.Hour)
	tickAt(a, now)
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
	waitRecoverHits(t, hits, 1)
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
	waitRecoverHits(t, hits, 2)
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

// A repeat waits while the first message is still in processing: with no
// last-processed cursor timing looks fresh, but the repeat would queue behind
// that message just like behind a backlog.
func TestActor_DelayRepeat_WaitsWhileTheFirstMessageIsStillInProcessing(t *testing.T) {
	fake := newFakeManagerOps()
	a, hits, at := waitingRepeat(t, fake)
	a.lastProcessedFn = func() (time.Time, error) { return time.Time{}, nil }

	a.onMessageProcessingStarted(at)
	tickAt(a, at)
	time.Sleep(50 * time.Millisecond)
	if hits.recover.Load() != 2 || !a.delayRepeatWaiting || a.recoveryState != types.CompletedRecoveryState {
		t.Fatalf("message in processing: POSTs=%d waiting=%v state=%v, want 2, waiting, Completed",
			hits.recover.Load(), a.delayRepeatWaiting, a.recoveryState)
	}

	// The message ends (with a zero timestamp, as snapshot_complete does):
	// nothing holds the repeat back any more.
	a.onMessageProcessingEnded(time.Time{})
	tickAt(a, at.Add(time.Second))
	waitRecoverHits(t, hits, 3)
	if a.delayRepeatWaiting {
		t.Fatal("the repeat still waits after the message ended")
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

// freshActor is an actor no recovery has been issued on yet: the state of a
// producer right after the SDK opened, before its first alive.
func freshActor(t *testing.T, fake *fakeManagerOps) (*recoveryActor, *recoveryHits) {
	t.Helper()
	srv, hits := fixtureSrv(t)
	t.Cleanup(srv.Close)
	return newWiredActor(t, srv, fake), hits
}

// recoveryAfterAlive brings a producer to a recovery that started at
// `start` (a steady producer, processing current) while the freshest system
// alive it has seen is well past that start: the situation in which the
// alive cursor is newer than messages still waiting in the consumer queue.
func recoveryAfterAlive(t *testing.T, fake *fakeManagerOps, now time.Time) (*recoveryActor, *recoveryHits, time.Time) {
	t.Helper()
	a, hits := steadyActor(t, fake, now.Add(-10*time.Second))
	start := now.Add(-3 * time.Minute)
	startRecovery(t, a, hits, start, 2)
	if err := a.snapshotRecoveryFinished(a.currentRecovery.recoveryID); err != nil {
		t.Fatal(err)
	}
	alive := now.Add(-4 * time.Second)
	if err := a.pm.SetLastAliveReceivedGenTimestamp(a.producerID, alive); err != nil {
		t.Fatal(err)
	}
	if cursor, err := a.timestampForRecovery(); err != nil || !cursor.After(start) {
		t.Fatalf("setup: alive cursor %v (%v), want one newer than the recovery start %v", cursor, err, start)
	}
	if !a.currentRecovery.recoverFrom.IsZero() {
		t.Fatalf("setup: the completed placeholder carries a start %v", a.currentRecovery.recoverFrom)
	}
	return a, hits, a.lastIssuedRecoverFrom
}

// A processing delay whose cursor cannot be read on the tick that flags it
// still keeps a floor: the start of the recovery last issued, a point that
// precedes every message still in the queue. NOT the alive cursor, which is
// newer than messages still waiting: once the backlog drained, the return
// recovery would have begun after them.
func TestActor_DelayDownWithAnUnreadableCursor_FloorIsTheLastRecoveryStartNotTheAliveCursor(t *testing.T) {
	fake := newFakeManagerOps()
	now := time.Now().Truncate(time.Millisecond)
	a, hits, start := recoveryAfterAlive(t, fake, now)
	if start.IsZero() {
		t.Fatal("setup: the last recovery has no start")
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
	if !a.recoveryFloor.Equal(start) || a.recoveryFloorFull {
		t.Fatalf("floor = %v (full=%v), want the start of the last recovery %v", a.recoveryFloor, a.recoveryFloorFull, start)
	}
	if !strings.Contains(sink.String(), "last processed message unknown") {
		t.Fatalf("the failed lookup was not logged:\n%s", sink.String())
	}

	// The lookup works again and the backlog has drained: the return
	// recovery reaches back to that start, not the alive cursor.
	failing = false
	back := time.Now()
	p.set(back)
	if err := a.systemAliveReceived(aliveAt(back), true); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 3)
	if got := hits.lastAfterMillis.Load(); got != start.UnixMilli() {
		t.Fatalf("return recovery after=%d (%v), want the recovery start %d (%v)",
			got, time.UnixMilli(got), start.UnixMilli(), start)
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// The same for a cursor that reads as zero (nothing processed yet) while a
// recovery is still in flight: the floor is that recovery's start.
func TestActor_DelayDownWithAZeroCursor_FloorIsTheRecoveryInFlightNotTheAliveCursor(t *testing.T) {
	fake := newFakeManagerOps()
	now := time.Now().Truncate(time.Millisecond)
	a, hits := steadyActor(t, fake, now.Add(-10*time.Second))
	start := now.Add(-3 * time.Minute)
	startRecovery(t, a, hits, start, 2)
	if err := a.pm.SetLastAliveReceivedGenTimestamp(a.producerID, now.Add(-4*time.Second)); err != nil {
		t.Fatal(err)
	}
	a.lastProcessedFn = func() (time.Time, error) { return time.Time{}, nil }

	a.lastUserSessionAlive = now.Add(-time.Hour)
	a.onTick(now, true)
	if a.downReason != types.ProcessingQueueDelayViolationProducerDownReason {
		t.Fatalf("setup: down reason = %v, want the processing delay", a.downReason)
	}
	if a.recoveryFloorFull || !a.recoveryFloor.Equal(a.currentRecovery.recoverFrom) || !a.recoveryFloor.Equal(start) {
		t.Fatalf("floor = %v (full=%v), want the start of the recovery in flight %v", a.recoveryFloor, a.recoveryFloorFull, start)
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// With no recovery issued and no resume point given, only the full history
// is a safe floor; the alive cursor that exists is not one.
func TestActor_DelayDownWithAnUnreadableCursor_NoRecoveryNoResumePoint_RecoversFromTheFullHistory(t *testing.T) {
	fake := newFakeManagerOps()
	now := time.Now().Truncate(time.Millisecond)
	a, hits := freshActor(t, fake)
	if err := a.pm.SetLastAliveReceivedGenTimestamp(a.producerID, now.Add(-4*time.Second)); err != nil {
		t.Fatal(err)
	}
	if cursor, err := a.timestampForRecovery(); err != nil || cursor.IsZero() {
		t.Fatalf("setup: alive cursor %v (%v), want one", cursor, err)
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

	tickAt(a, now)
	if a.downReason != types.ProcessingQueueDelayViolationProducerDownReason {
		t.Fatalf("setup: down reason = %v, want the processing delay", a.downReason)
	}
	if !a.recoveryFloorFull || !a.recoveryFloor.IsZero() {
		t.Fatalf("floor = %v (full=%v), want the full-history floor", a.recoveryFloor, a.recoveryFloorFull)
	}

	failing = false
	back := time.Now()
	p.set(back)
	if err := a.systemAliveReceived(aliveAt(back), true); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 1)
	if got := hits.lastAfterMillis.Load(); got != 0 {
		t.Fatalf("return recovery after=%d (%v), want the full history (no after)", got, time.UnixMilli(got))
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// downEventCounts returns how many producer-status events are down and how
// many are up.
func downEventCounts(fake *fakeManagerOps) (down, up int) {
	for _, e := range producerStatusEvents(fake) {
		if e.IsDown() {
			down++
		} else {
			up++
		}
	}
	return down, up
}

// requireDownForDelayWithFloor checks the state of a chain that ran out
// while processing still lagged: the producer stays down for the delay with
// no up reported, nothing waits or runs, and the range is kept as the floor.
// repeats is the count the chain carries on to the return path.
func requireDownForDelayWithFloor(t *testing.T, a *recoveryActor, fake *fakeManagerOps, upBefore int, want time.Time, repeats int) {
	t.Helper()
	if _, up := downEventCounts(fake); up != upBefore {
		t.Fatalf("producer-up events went from %d to %d, want none while processing still lags", upBefore, up)
	}
	if evs := producerStatusEvents(fake); !evs[len(evs)-1].IsDown() {
		t.Fatal("last status is up")
	}
	if !a.isFlaggedDown() || a.downReason != types.ProcessingQueueDelayViolationProducerDownReason ||
		a.delayRepeatWaiting || a.recoveryState != types.CompletedRecoveryState || a.delayRepeats != repeats {
		t.Fatalf("down=%v reason=%v waiting=%v state=%v repeats=%d, want down for the delay, nothing waiting, Completed, %d repeats counted",
			a.isFlaggedDown(), a.downReason, a.delayRepeatWaiting, a.recoveryState, a.delayRepeats, repeats)
	}
	if want.IsZero() || !a.recoveryFloor.Equal(want) {
		t.Fatalf("floor = %v, want the range the chain was to cover %v", a.recoveryFloor, want)
	}
}

// requireReturnRecoversFrom lets processing catch up and an alive arrive:
// the return path recovers from the floor, and its completion reports the
// producer up.
func requireReturnRecoversFrom(t *testing.T, a *recoveryActor, hits *recoveryHits, p *procClock, posts int32, want time.Time) {
	t.Helper()
	back := time.Now()
	p.set(back)
	if err := a.systemAliveReceived(aliveAt(back), true); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, posts)
	if got := hits.lastAfterMillis.Load(); got != want.UnixMilli() {
		t.Fatalf("return recovery after=%d (%v), want the floor %d (%v)", got, time.UnixMilli(got), want.UnixMilli(), want)
	}
	if err := a.snapshotRecoveryFinished(a.currentRecovery.recoveryID); err != nil {
		t.Fatal(err)
	}
	if a.isFlaggedDown() || !a.recoveryFloor.IsZero() || a.recoveryFloorFull {
		t.Fatalf("after the return recovery: down=%v floor=%v full=%v, want up and discharged", a.isFlaggedDown(), a.recoveryFloor, a.recoveryFloorFull)
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// The wait for a repeat runs past MaxRecoveryExecution while processing
// still lags: reporting the producer up would be undone by the timing check
// of the same tick. It stays down for the delay, with what the repeat was to
// cover kept as the floor, and the way back recovers from there.
func TestActor_DelayRepeat_WaitExpiresWhileStillLagging_StaysDownAndKeepsTheFloor(t *testing.T) {
	fake := newFakeManagerOps()
	a, hits, r1, _ := genuineDelayDuringRecovery(t, fake)
	p := holdProcessing(a)
	sink := &lockedBuffer{}
	a.logger = debugLogger(sink)
	if err := a.snapshotRecoveryFinished(r1); err != nil {
		t.Fatal(err)
	}
	want := a.delayRepeatFrom
	_, upBefore := downEventCounts(fake)

	lag(a, p, a.currentRecovery.chainStartedAt.Add(a.cfg.MaxRecoveryExecution()+time.Minute))
	requireDownForDelayWithFloor(t, a, fake, upBefore, want, 0)
	if n := strings.Count(sink.String(), "processing fell behind during every recovery attempt"); n != 1 {
		t.Fatalf("warn logged %d times, want 1:\n%s", n, sink.String())
	}
	// Further stale ticks neither warn again nor report up.
	lag(a, p, time.Now().Add(time.Hour))
	requireDownForDelayWithFloor(t, a, fake, upBefore, want, 0)
	if n := strings.Count(sink.String(), "processing fell behind during every recovery attempt"); n != 1 {
		t.Fatalf("warn logged %d times after more ticks, want 1", n)
	}
	if hits.recover.Load() != 2 {
		t.Fatalf("recovery POSTs = %d, want 2: no repeat was requested", hits.recover.Load())
	}
	requireReturnRecoversFrom(t, a, hits, p, 3, want)
}

// The same when the repeats run out: the last repeat completes while
// processing still lags.
func TestActor_DelayDownDuringEveryAttempt_StillLaggingStaysDownAndKeepsTheFloor(t *testing.T) {
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
		catchUp(a, p, at)
		waitRecoverHits(t, hits, int32(2+i))
		id = a.currentRecovery.recoveryID
		at = at.Add(time.Second)
		lag(a, p, at)
		at = at.Add(time.Second)
	}
	want := a.currentRecovery.recoverFrom
	if !a.delayCursor.IsZero() && a.delayCursor.Before(want) {
		want = a.delayCursor
	}
	_, upBefore := downEventCounts(fake)

	// The last repeat completes with processing still behind.
	if err := a.snapshotRecoveryFinished(id); err != nil {
		t.Fatal(err)
	}
	requireDownForDelayWithFloor(t, a, fake, upBefore, want, maxDelayRepeats)
	if n := strings.Count(sink.String(), "processing fell behind during every recovery attempt"); n != 1 {
		t.Fatalf("warn logged %d times, want 1:\n%s", n, sink.String())
	}
	// A tick that finds processing still behind keeps it that way.
	lag(a, p, time.Now().Add(time.Hour))
	requireDownForDelayWithFloor(t, a, fake, upBefore, want, maxDelayRepeats)
	requireReturnRecoversFrom(t, a, hits, p, int32(3+maxDelayRepeats), want)
}

// failableTiming makes the last-processed lookup of the actor fail on demand
// and returns the switch.
func failableTiming(a *recoveryActor) *atomic.Bool {
	failing := &atomic.Bool{}
	lookup := a.lastProcessedFn
	a.lastProcessedFn = func() (time.Time, error) {
		if failing.Load() {
			return time.Time{}, errors.New("lookup failed")
		}
		return lookup()
	}
	return failing
}

// A chain that ends while the timing cannot be read ends like one that ends
// while processing lags: the producer stays down, no up is reported, the
// range is kept as the floor, one warning is logged; the floor is recovered
// from once the lookup works again.
func TestActor_DelayRepeat_WaitExpiresWithUnreadableTiming_StaysDownAndKeepsTheFloor(t *testing.T) {
	fake := newFakeManagerOps()
	a, hits, r1, _ := genuineDelayDuringRecovery(t, fake)
	p := holdProcessing(a)
	failing := failableTiming(a)
	sink := &lockedBuffer{}
	a.logger = debugLogger(sink)
	if err := a.snapshotRecoveryFinished(r1); err != nil {
		t.Fatal(err)
	}
	want := a.delayRepeatFrom
	_, upBefore := downEventCounts(fake)

	failing.Store(true)
	tickAt(a, a.currentRecovery.chainStartedAt.Add(a.cfg.MaxRecoveryExecution()+time.Minute))
	requireDownForDelayWithFloor(t, a, fake, upBefore, want, 0)
	tickAt(a, time.Now().Add(time.Hour))
	requireDownForDelayWithFloor(t, a, fake, upBefore, want, 0)
	if n := strings.Count(sink.String(), "processing fell behind during every recovery attempt"); n != 1 {
		t.Fatalf("warn logged %d times, want 1:\n%s", n, sink.String())
	}
	if !strings.Contains(sink.String(), "lookup failed") {
		t.Fatalf("the cause of the unreadable timing was not logged:\n%s", sink.String())
	}
	if hits.recover.Load() != 2 {
		t.Fatalf("recovery POSTs = %d, want 2: no repeat was requested", hits.recover.Load())
	}

	failing.Store(false)
	requireReturnRecoversFrom(t, a, hits, p, 3, want)
}

// The same when the repeats run out.
func TestActor_DelayDownDuringEveryAttempt_UnreadableTimingStaysDownAndKeepsTheFloor(t *testing.T) {
	fake := newFakeManagerOps()
	a, hits, r1, _ := genuineDelayDuringRecovery(t, fake)
	p := holdProcessing(a)
	failing := failableTiming(a)
	sink := &lockedBuffer{}
	a.logger = debugLogger(sink)

	id := r1
	at := time.Now()
	for i := 1; i <= maxDelayRepeats; i++ {
		if err := a.snapshotRecoveryFinished(id); err != nil {
			t.Fatal(err)
		}
		catchUp(a, p, at)
		waitRecoverHits(t, hits, int32(2+i))
		id = a.currentRecovery.recoveryID
		at = at.Add(time.Second)
		lag(a, p, at)
		at = at.Add(time.Second)
	}
	want := a.currentRecovery.recoverFrom
	if !a.delayCursor.IsZero() && a.delayCursor.Before(want) {
		want = a.delayCursor
	}
	_, upBefore := downEventCounts(fake)

	// The last repeat completes while the timing cannot be read.
	failing.Store(true)
	if err := a.snapshotRecoveryFinished(id); err != nil {
		t.Fatal(err)
	}
	requireDownForDelayWithFloor(t, a, fake, upBefore, want, maxDelayRepeats)
	tickAt(a, time.Now().Add(time.Hour))
	requireDownForDelayWithFloor(t, a, fake, upBefore, want, maxDelayRepeats)
	if n := strings.Count(sink.String(), "processing fell behind during every recovery attempt"); n != 1 {
		t.Fatalf("warn logged %d times, want 1:\n%s", n, sink.String())
	}

	failing.Store(false)
	requireReturnRecoversFrom(t, a, hits, p, int32(3+maxDelayRepeats), want)
}

// A recovery that processing fell behind during is interrupted by an alive
// that says "not subscribed", and replaced once it completes. The
// replacement starts while processing may still lag, so its replay can be
// dropped as well: it carries the need to repeat, and its completion with
// processing behind does not report the producer up.
func TestActor_DelayDownDuringRecovery_InterruptedReplacementKeepsTheRepeatDecision(t *testing.T) {
	fake := newFakeManagerOps()
	a, hits, r1, _ := genuineDelayDuringRecovery(t, fake)
	p := holdProcessing(a)
	if a.delayDownRequestID != r1 {
		t.Fatalf("setup: delay mark = %d, want R1 %d", a.delayDownRequestID, r1)
	}
	if err := a.systemAliveReceived(aliveAt(time.Now()), false); err != nil {
		t.Fatal(err)
	}
	if a.recoveryState != types.InterruptedRecoveryState {
		t.Fatalf("setup: state = %v, want Interrupted", a.recoveryState)
	}
	_, upBefore := downEventCounts(fake)

	// R1 completes on the wire; the interruption asks for a fresh one.
	if err := a.snapshotRecoveryFinished(r1); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 3)
	r2 := a.currentRecovery.recoveryID
	if r2 == r1 || a.recoveryState != types.StartedRecoveryState {
		t.Fatalf("replacement: id=%d (R1 %d) state=%v, want a new Started recovery", r2, r1, a.recoveryState)
	}
	if a.delayDownRequestID != r2 {
		t.Fatalf("delay mark = %d, want the replacement %d", a.delayDownRequestID, r2)
	}

	// The replacement completes while processing still lags.
	if err := a.snapshotRecoveryFinished(r2); err != nil {
		t.Fatal(err)
	}
	if _, up := downEventCounts(fake); up != upBefore {
		t.Fatalf("producer-up events went from %d to %d, want none", upBefore, up)
	}
	if !a.isFlaggedDown() || !a.delayRepeatWaiting || a.recoveryState != types.CompletedRecoveryState {
		t.Fatalf("down=%v waiting=%v state=%v, want down with a repeat waiting", a.isFlaggedDown(), a.delayRepeatWaiting, a.recoveryState)
	}

	// Processing catches up: the repeat goes out and completes the chain.
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

// The same when the interrupted recovery is replaced by the next alive
// before it has completed (a lost subscription ends this way).
func TestActor_DelayDownDuringRecovery_InterruptedThenReplacedByAnAlive_KeepsTheRepeatDecision(t *testing.T) {
	fake := newFakeManagerOps()
	a, hits, r1, _ := genuineDelayDuringRecovery(t, fake)
	holdProcessing(a)
	if err := a.systemAliveReceived(aliveAt(time.Now()), false); err != nil {
		t.Fatal(err)
	}
	if err := a.systemAliveReceived(aliveAt(time.Now()), true); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 3)
	r2 := a.currentRecovery.recoveryID
	if r2 == r1 || a.delayDownRequestID != r2 {
		t.Fatalf("replacement id=%d (R1 %d) delay mark=%d, want the mark on the replacement", r2, r1, a.delayDownRequestID)
	}
	if err := a.snapshotRecoveryFinished(r2); err != nil {
		t.Fatal(err)
	}
	if !a.isFlaggedDown() || !a.delayRepeatWaiting {
		t.Fatalf("down=%v waiting=%v, want down with a repeat waiting", a.isFlaggedDown(), a.delayRepeatWaiting)
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// The wait for a repeat runs past MaxRecoveryExecution while another
// session's first message is still in processing: with no last-processed
// cursor the timing looks fresh, but the queue may be backed up behind that
// message, so the producer is not reported up.
func TestActor_DelayRepeat_WaitExpiresWhileTheFirstMessageIsInProcessing_StaysDownAndKeepsTheFloor(t *testing.T) {
	fake := newFakeManagerOps()
	a, hits, at := waitingRepeat(t, fake)
	a.lastProcessedFn = func() (time.Time, error) { return time.Time{}, nil }
	sink := &lockedBuffer{}
	a.logger = debugLogger(sink)
	want := a.delayRepeatFrom
	_, upBefore := downEventCounts(fake)

	a.onMessageProcessingStarted(at)
	expired := a.currentRecovery.chainStartedAt.Add(a.cfg.MaxRecoveryExecution() + time.Minute)
	tickAt(a, expired)
	requireDownForDelayWithFloor(t, a, fake, upBefore, want, 0)
	tickAt(a, expired.Add(time.Hour))
	requireDownForDelayWithFloor(t, a, fake, upBefore, want, 0)
	if n := strings.Count(sink.String(), "processing fell behind during every recovery attempt"); n != 1 {
		t.Fatalf("warn logged %d times, want 1:\n%s", n, sink.String())
	}
	if hits.recover.Load() != 2 {
		t.Fatalf("recovery POSTs = %d, want 2: no repeat was requested", hits.recover.Load())
	}
}

// The return path of a processing delay recovers from the last processed
// message and falls back to the recovery floor when that cannot be told: the
// lookup fails, or reads zero, between the timing check that lets the alive
// through and the request. The fallback keeps the request reaching back to
// the delay (the full history when the floor is the full one).
func TestActor_ReturnFromProcessingDelayDown_CursorLostAtTheAlive_RecoversFromTheFloor(t *testing.T) {
	for _, tc := range []struct {
		name string
		// unreadable: the lookup fails on the call after the timing check;
		// otherwise it reads zero from the alive on.
		unreadable bool
		full       bool
	}{
		{"zero cursor", false, false},
		{"unreadable cursor", true, false},
		{"zero cursor, full history floor", false, true},
		{"unreadable cursor, full history floor", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeManagerOps()
			now := time.Now().Truncate(time.Millisecond)
			var a *recoveryActor
			var hits *recoveryHits
			var start time.Time
			posts := int32(3)
			if tc.full {
				a, hits = freshActor(t, fake)
				if err := a.pm.SetLastAliveReceivedGenTimestamp(a.producerID, now.Add(-4*time.Second)); err != nil {
					t.Fatal(err)
				}
				posts = 1
			} else {
				a, hits, start = recoveryAfterAlive(t, fake, now)
			}
			p := holdProcessing(a)
			lookup := a.lastProcessedFn
			var mode atomic.Int32 // 0 as the clock, 1 zero, 2 fail after one read
			var reads atomic.Int32
			a.lastProcessedFn = func() (time.Time, error) {
				switch mode.Load() {
				case 1:
					return time.Time{}, nil
				case 2:
					if reads.Add(1) > 1 {
						return time.Time{}, errors.New("lookup failed")
					}
				}
				return lookup()
			}

			// The delay is flagged with the lookup failing: the floor comes
			// from the last recovery's start, or is the full history.
			failing := failableTiming(a)
			failing.Store(true)
			tickAt(a, now)
			failing.Store(false)
			if a.downReason != types.ProcessingQueueDelayViolationProducerDownReason {
				t.Fatalf("setup: down reason = %v, want the processing delay", a.downReason)
			}
			wantFloor := start
			if tc.full != a.recoveryFloorFull || (!tc.full && !a.recoveryFloor.Equal(start)) {
				t.Fatalf("setup: floor = %v (full=%v), want %v (full=%v)", a.recoveryFloor, a.recoveryFloorFull, wantFloor, tc.full)
			}

			// Processing is current again, but the cursor reads as zero (or
			// the lookup fails after the timing check) when the alive comes.
			p.set(time.Now())
			if tc.unreadable {
				mode.Store(2)
			} else {
				mode.Store(1)
			}
			if err := a.systemAliveReceived(aliveAt(time.Now()), true); err != nil {
				t.Fatal(err)
			}
			waitRecoverHits(t, hits, posts)
			want := start.UnixMilli()
			if tc.full {
				want = 0
			}
			if got := hits.lastAfterMillis.Load(); got != want {
				t.Fatalf("return recovery after=%d (%v), want the floor %d", got, time.UnixMilli(got), want)
			}
			waitDetachedDone(t, a, 3*time.Second)
		})
	}
}

// The floor of a processing delay is where processing stood when it fell
// behind, and the system alives that arrive while it lags must not move it:
// they say nothing about the messages the broker dropped from the queue of a
// stalled session. The recovery on the way back reaches back to that point,
// however many system alives came in between, and the producer manager's
// cursor stays where the stalled processing left it.
func TestActor_ProcessingDelayDown_SystemAlivesDoNotMoveTheFloor(t *testing.T) {
	fake := newFakeManagerOps()
	now := time.Now().Truncate(time.Millisecond)
	a, hits := steadyActor(t, fake, now.Add(-10*time.Second))
	stoodAt := now.Add(-2 * time.Minute)
	a.onMessageProcessingEnded(stoodAt)
	a.onTick(now, true)
	if a.downReason != types.ProcessingQueueDelayViolationProducerDownReason {
		t.Fatalf("setup: down reason = %v, want the processing delay", a.downReason)
	}
	if !a.recoveryFloor.Equal(stoodAt) {
		t.Fatalf("setup: floor = %v, want %v", a.recoveryFloor, stoodAt)
	}

	// System alives keep coming while the user's processing is stalled.
	for i := 1; i <= 3; i++ {
		a.onAlive(evAlive{
			timestamp:       aliveAt(now.Add(time.Duration(i) * time.Second)),
			isSubscribed:    true,
			messageInterest: types.SystemAliveOnly,
		})
	}
	prod, err := a.pm.GetProducer(a.ctx, a.producerID)
	if err != nil {
		t.Fatal(err)
	}
	if got := prod.LastProcessedMessageGenTimestamp(); !got.Equal(stoodAt) {
		t.Fatalf("processed cursor = %v after system alives, want it where processing stood %v", got, stoodAt)
	}
	if !a.recoveryFloor.Equal(stoodAt) || a.recoveryFloorFull {
		t.Fatalf("floor = %v (full=%v) after system alives, want %v", a.recoveryFloor, a.recoveryFloorFull, stoodAt)
	}
	if hits.recover.Load() != 1 {
		t.Fatalf("recovery POSTs = %d, want 1: processing still lags", hits.recover.Load())
	}

	// Processing catches up; the next system alive starts the way back.
	back := now.Add(10 * time.Second)
	a.onMessageProcessingEnded(back)
	a.onAlive(evAlive{timestamp: aliveAt(back), isSubscribed: true, messageInterest: types.SystemAliveOnly})
	waitRecoverHits(t, hits, 2)
	if got := hits.lastAfterMillis.Load(); got > stoodAt.UnixMilli() {
		t.Fatalf("recovery after=%d (%v), want at or before where processing stood %d (%v)",
			got, time.UnixMilli(got), stoodAt.UnixMilli(), stoodAt)
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// The tick that decides processing fell behind and the floor it sets read
// the same cursor. A session can end a newer message between two reads, and
// a floor taken from the second one lands past messages the broker dropped.
func TestActor_ProcessingDelayDown_FloorIsTheCursorTheTickJudged(t *testing.T) {
	fake := newFakeManagerOps()
	now := time.Now().Truncate(time.Millisecond)
	a, _ := steadyActor(t, fake, now.Add(-10*time.Second))

	stale := now.Add(-2 * time.Minute)
	var reads atomic.Int32
	a.lastProcessedFn = func() (time.Time, error) {
		if reads.Add(1) == 1 {
			return stale, nil
		}
		return now, nil // a session finished a newer message meanwhile
	}
	tickAt(a, now)
	if a.downReason != types.ProcessingQueueDelayViolationProducerDownReason {
		t.Fatalf("setup: down reason = %v, want the processing delay", a.downReason)
	}
	if !a.recoveryFloor.Equal(stale) {
		t.Fatalf("floor = %v, want the cursor the tick judged %v", a.recoveryFloor, stale)
	}
}

// The same when the tick could not read the cursor: the floor falls back to
// the last recovery's start, even if the cursor is readable a moment later.
func TestActor_ProcessingDelayDown_UnreadableCursorOnTheTickIsNotReadAgain(t *testing.T) {
	fake := newFakeManagerOps()
	now := time.Now().Truncate(time.Millisecond)
	a, _ := steadyActor(t, fake, now.Add(-10*time.Second))

	var reads atomic.Int32
	a.lastProcessedFn = func() (time.Time, error) {
		if reads.Add(1) == 1 {
			return time.Time{}, errors.New("lookup failed")
		}
		return now, nil
	}
	tickAt(a, now)
	if a.downReason != types.ProcessingQueueDelayViolationProducerDownReason {
		t.Fatalf("setup: down reason = %v, want the processing delay", a.downReason)
	}
	if a.recoveryFloor.Equal(now) {
		t.Fatalf("floor = %v: taken from a second read of the cursor", a.recoveryFloor)
	}
}

// Processing falls behind after a lost subscription put the recovery into
// the interrupted state and before its replacement starts. The delay is
// marked on the interrupted recovery and handed to the replacement, which
// must not report the producer up with the gap unfilled.
func TestActor_DelayDownDuringAnInterruptedRecovery_ReplacementIsRepeated(t *testing.T) {
	fake := newFakeManagerOps()
	now := time.Now().Truncate(time.Millisecond)
	a, hits := steadyActor(t, fake, now.Add(-10*time.Second))
	if err := a.systemAliveReceived(aliveAt(now.Add(-8*time.Second)), true); err != nil {
		t.Fatal(err)
	}
	a.onMessageProcessingEnded(now.Add(-5 * time.Second))
	if err := a.systemAliveReceived(aliveAt(now), false); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 2)
	r1 := a.currentRecovery.recoveryID
	// The subscription is lost again while R1 runs.
	if err := a.systemAliveReceived(aliveAt(now), false); err != nil {
		t.Fatal(err)
	}
	if a.recoveryState != types.InterruptedRecoveryState {
		t.Fatalf("setup: state = %v, want Interrupted", a.recoveryState)
	}

	// Processing falls behind now, with R1 interrupted.
	p := holdProcessing(a)
	tickAt(a, now.Add(30*time.Second))
	if a.downReason != types.ProcessingQueueDelayViolationProducerDownReason || a.delayDownRequestID != r1 {
		t.Fatalf("down reason %v, delay mark %d, want the delay marked on the interrupted R1 %d",
			a.downReason, a.delayDownRequestID, r1)
	}
	_, upBefore := downEventCounts(fake)

	// R1 completes on the wire; the replacement is issued with the mark.
	if err := a.snapshotRecoveryFinished(r1); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 3)
	r2 := a.currentRecovery.recoveryID
	if r2 == r1 || a.delayDownRequestID != r2 {
		t.Fatalf("replacement id=%d (R1 %d) mark=%d, want the mark carried to the replacement", r2, r1, a.delayDownRequestID)
	}

	// The replacement completes while processing still lags.
	if err := a.snapshotRecoveryFinished(r2); err != nil {
		t.Fatal(err)
	}
	if _, up := downEventCounts(fake); up != upBefore {
		t.Fatalf("producer-up events went from %d to %d, want none", upBefore, up)
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

// snapshot_complete can be handled after the broker dropped queued replay
// messages and before the next tick sees processing behind. The completion
// itself judges the timing: it does not report the producer up, and the
// repeat reaches back to the cursor it saw (earlier than the recovery's own
// start), which is also the floor.
func TestActor_RecoveryCompletingWhileProcessingLags_IsRepeatedWithoutATick(t *testing.T) {
	fake := newFakeManagerOps()
	now := time.Now().Truncate(time.Millisecond)
	a, hits := steadyActor(t, fake, now.Add(-10*time.Second))
	if err := a.systemAliveReceived(aliveAt(now.Add(-8*time.Second)), true); err != nil {
		t.Fatal(err)
	}
	a.onMessageProcessingEnded(now.Add(-5 * time.Second))
	if err := a.systemAliveReceived(aliveAt(now), false); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 2)
	r1 := a.currentRecovery.recoveryID
	p := holdProcessing(a) // behind by a minute; no tick has seen it
	stale := time.Unix(0, p.v.Load())
	if a.delayDownRequestID != 0 {
		t.Fatalf("setup: delay mark = %d, want none", a.delayDownRequestID)
	}
	_, upBefore := downEventCounts(fake)

	if err := a.snapshotRecoveryFinished(r1); err != nil {
		t.Fatal(err)
	}
	if _, up := downEventCounts(fake); up != upBefore {
		t.Fatalf("producer-up events went from %d to %d, want none while processing lags", upBefore, up)
	}
	if !a.isFlaggedDown() || !a.delayRepeatWaiting || a.recoveryState != types.CompletedRecoveryState {
		t.Fatalf("down=%v waiting=%v state=%v, want down with a repeat waiting", a.isFlaggedDown(), a.delayRepeatWaiting, a.recoveryState)
	}
	if !a.recoveryFloor.Equal(stale) {
		t.Fatalf("floor = %v, want the cursor the completion saw %v", a.recoveryFloor, stale)
	}

	catchUp(a, p, time.Now())
	waitRecoverHits(t, hits, 3)
	if got := hits.lastAfterMillis.Load(); got != stale.UnixMilli() {
		t.Fatalf("repeat after=%d, want the cursor the completion saw %d", got, stale.UnixMilli())
	}
	p.set(time.Now())
	if err := a.snapshotRecoveryFinished(a.currentRecovery.recoveryID); err != nil {
		t.Fatal(err)
	}
	if a.isFlaggedDown() || hits.recover.Load() != 3 {
		t.Fatalf("down=%v POSTs=%d, want up after one repeat", a.isFlaggedDown(), hits.recover.Load())
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// A completion with current processing is untouched: up at once, no repeat,
// no floor, no further request.
func TestActor_RecoveryCompletingWithFreshTiming_ReportsUpWithoutARepeat(t *testing.T) {
	fake := newFakeManagerOps()
	now := time.Now().Truncate(time.Millisecond)
	a, hits := steadyActor(t, fake, now.Add(-10*time.Second))
	if err := a.systemAliveReceived(aliveAt(now), false); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 2)
	p := holdProcessing(a)
	p.set(time.Now())
	if err := a.snapshotRecoveryFinished(a.currentRecovery.recoveryID); err != nil {
		t.Fatal(err)
	}
	if a.isFlaggedDown() || a.delayRepeatWaiting || a.delayDownRequestID != 0 || !a.recoveryFloor.IsZero() {
		t.Fatalf("down=%v waiting=%v mark=%d floor=%v, want up and untouched",
			a.isFlaggedDown(), a.delayRepeatWaiting, a.delayDownRequestID, a.recoveryFloor)
	}
	time.Sleep(50 * time.Millisecond)
	if hits.recover.Load() != 2 {
		t.Fatalf("recovery POSTs = %d, want 2", hits.recover.Load())
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// A chain that stayed down because the first message is still in processing
// is not returned by the next system alive while it is: the return recovery
// would be queued behind that message, the situation the guard protects. It
// is issued once the message has ended.
func TestActor_ReturnFromProcessingDelayDown_WaitsWhileTheFirstMessageIsInProcessing(t *testing.T) {
	fake := newFakeManagerOps()
	a, hits, at := waitingRepeat(t, fake)
	var cursor atomic.Int64 // 0 = no message has ended
	a.lastProcessedFn = func() (time.Time, error) {
		if v := cursor.Load(); v != 0 {
			return time.Unix(0, v), nil
		}
		return time.Time{}, nil
	}
	want := a.delayRepeatFrom
	_, upBefore := downEventCounts(fake)

	a.onMessageProcessingStarted(at)
	expired := a.currentRecovery.chainStartedAt.Add(a.cfg.MaxRecoveryExecution() + time.Minute)
	tickAt(a, expired)
	requireDownForDelayWithFloor(t, a, fake, upBefore, want, 0)

	if err := a.systemAliveReceived(aliveAt(time.Now()), true); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if hits.recover.Load() != 2 {
		t.Fatalf("recovery POSTs = %d, want 2: no return recovery behind the first message", hits.recover.Load())
	}

	// The message ends: the next alive returns the chain.
	cursor.Store(time.Now().UnixNano())
	a.onMessageProcessingEnded(time.Now())
	if err := a.systemAliveReceived(aliveAt(time.Now()), true); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 3)
	if got := hits.lastAfterMillis.Load(); got != want.UnixMilli() {
		t.Fatalf("return recovery after=%d, want the floor %d", got, want.UnixMilli())
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// The bound of repeats holds across a stay-down: the return recovery that
// follows a chain that ran out does not get three repeats of its own. When
// it completes while processing still lags the producer stays down again
// at once, and the count is cleared only by a recovery that completes with
// processing current, which reports the producer up.
func TestActor_DelayRepeatBound_HoldsAcrossAStayDown(t *testing.T) {
	fake := newFakeManagerOps()
	a, hits, r1, _ := genuineDelayDuringRecovery(t, fake)
	p := holdProcessing(a)
	id := r1
	at := time.Now()
	for i := 1; i <= maxDelayRepeats; i++ {
		if err := a.snapshotRecoveryFinished(id); err != nil {
			t.Fatal(err)
		}
		catchUp(a, p, at)
		waitRecoverHits(t, hits, int32(2+i))
		id = a.currentRecovery.recoveryID
		at = at.Add(time.Second)
		lag(a, p, at)
		at = at.Add(time.Second)
	}
	want := a.currentRecovery.recoverFrom
	if !a.delayCursor.IsZero() && a.delayCursor.Before(want) {
		want = a.delayCursor
	}
	_, upBefore := downEventCounts(fake)
	if err := a.snapshotRecoveryFinished(id); err != nil {
		t.Fatal(err)
	}
	requireDownForDelayWithFloor(t, a, fake, upBefore, want, maxDelayRepeats)

	// Processing is current for the alive, the return recovery goes out and
	// processing falls behind again while it runs.
	posts := int32(3 + maxDelayRepeats)
	p.set(time.Now())
	if err := a.systemAliveReceived(aliveAt(time.Now()), true); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, posts)
	lag(a, p, time.Now().Add(time.Second))
	if err := a.snapshotRecoveryFinished(a.currentRecovery.recoveryID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if hits.recover.Load() != posts {
		t.Fatalf("recovery POSTs = %d, want %d: the return recovery was repeated", hits.recover.Load(), posts)
	}
	if !a.isFlaggedDown() || a.delayRepeatWaiting || a.delayRepeats != maxDelayRepeats {
		t.Fatalf("down=%v waiting=%v repeats=%d, want down, nothing waiting, %d repeats still counted",
			a.isFlaggedDown(), a.delayRepeatWaiting, a.delayRepeats, maxDelayRepeats)
	}
	if _, up := downEventCounts(fake); up != upBefore {
		t.Fatalf("producer-up events went from %d to %d, want none", upBefore, up)
	}

	// A return recovery that completes with processing current reports the
	// producer up and clears the count.
	p.set(time.Now())
	if err := a.systemAliveReceived(aliveAt(time.Now()), true); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, posts+1)
	p.set(time.Now())
	if err := a.snapshotRecoveryFinished(a.currentRecovery.recoveryID); err != nil {
		t.Fatal(err)
	}
	if a.isFlaggedDown() || a.delayRepeats != 0 || a.delayStayedDown {
		t.Fatalf("down=%v repeats=%d stayedDown=%v, want up with the count cleared", a.isFlaggedDown(), a.delayRepeats, a.delayStayedDown)
	}
	waitDetachedDone(t, a, 3*time.Second)
}
