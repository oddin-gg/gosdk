package recovery

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// The net starts only when every producer of the session that the catalog has
// enabled can be asked for now, asks for all of them, makes the reset only if each
// of those recoveries is still in flight and accepted, and cancels it when any ends.
// What it resets, it asks for again at once.

// liveDisabledWithARecoverySeenByOneSession is two sessions of every producer, the
// live producer disabled while its recovery is in flight, which session 1 has seen
// the snapshot_complete of and session 2 has not: it stays in flight.
func liveDisabledWithARecoverySeenByOneSession(t *testing.T) (*netFeed, snapshotRequest) {
	t.Helper()
	f := &netFeed{feed: newFeed(t)}
	f.open(1, all)
	f.open(2, all)
	f.startFeed()
	f.bothUp(1, 2)
	f.unsubscribed(live)
	liveReq := f.lastSnapshot(live)
	f.accept(liveReq)
	f.snapshotComplete(1, live, liveReq.requestID)
	expect(t, f.inFlight(live), liveReq.requestID, "the live recovery in flight")
	f.cat.disabled[live] = true
	return f, liveReq
}

// A recovery of a producer the session receives that was asked before the reset
// cannot cover the gap the reset opened, whether the session awaited it or saw its
// snapshot_complete already: once the reset is done it is given up, and the
// follow-up goes out in that step, for every enabled producer.
func TestNet_ResetDoneGivesUpARecoveryAskedBeforeItAndAsksAgainAtOnce(t *testing.T) {
	f, liveReq := liveDisabledWithARecoverySeenByOneSession(t)
	before := f.calls()
	f.stale(1, oldAge, 5*time.Minute, f.untilAsked(before))
	expect(t, f.calls(), before+1, "requests: the disabled live producer is not asked")
	f.accept(f.lastSnapshot(pre))
	expectSessions(t, f.resetSessions(), []int{1}, "resets")

	// enabled while the reset is under way: the recovery in flight stays until it is done
	delete(f.cat.disabled, live)
	f.alive(live)
	f.m.tick()
	expect(t, f.inFlight(live), liveReq.requestID, "the live recovery in flight while the reset is under way")
	underway := f.calls()

	f.resetDone()
	expect(t, f.calls(), underway+2, "requests in the step that ends the reset: both producers")
	if id := f.inFlight(live); id == 0 || id == liveReq.requestID {
		t.Fatalf("the live recovery in flight is %d: the one asked before the reset (%d) was not given up and replaced", id, liveReq.requestID)
	}
	expect(t, f.m.counters.failed, 0, "recoveries failed")
	expect(t, f.m.byID[live].retryAt, time.Duration(0), "the live producer's retry delay")
	expect(t, f.m.byID[pre].retryAt, time.Duration(0), "the prematch producer's retry delay")
}

// Every enabled producer is asked again in the step that ends the reset, and the
// give-up delays none of them.
func TestNet_ResetDoneAsksForEveryProducerAgainInTheSameStepWithNoRetryDelay(t *testing.T) {
	f := upNet(t, all)
	before := f.calls()
	f.stale(1, oldAge, 5*time.Minute, f.untilAsked(before))
	preAt, _ := f.checkpoint(1, pre)
	liveAt, _ := f.checkpoint(1, live)
	f.accept(f.lastSnapshot(pre))
	f.accept(f.lastSnapshot(live))
	underway := f.calls()
	f.resetDone()
	expect(t, f.calls(), underway+2, "requests in the step that ends the reset")
	expectTime(t, f.lastSnapshot(pre).after, preAt, "prematch from where the session was")
	expectTime(t, f.lastSnapshot(live).after, liveAt, "live from where the session was")
	expect(t, f.m.counters.failed, 0, "recoveries failed")
	for _, p := range []int{pre, live} {
		expect(t, f.m.byID[p].retryAt, time.Duration(0), "the producer's retry delay")
		expect(t, f.m.byID[p].failures, 0, "the producer's failures")
	}
}

// A producer enabled while the reset is under way has a gap from the reset: nothing
// is asked for it until the reset is done, since no queue is bound, and then it is
// asked for from where the session was when the reset was made.
func TestNet_ProducerEnabledWhileTheResetIsUnderWayIsAskedForOnceItIsDone(t *testing.T) {
	f := upNet(t, all)
	f.cat.disabled[live] = true
	before := f.calls()
	f.stale(1, oldAge, 5*time.Minute, f.untilAsked(before))
	expect(t, f.calls(), before+1, "requests: prematch only")
	liveAt, _ := f.checkpoint(1, live)
	f.accept(f.lastSnapshot(pre))
	expectSessions(t, f.resetSessions(), []int{1}, "resets")

	delete(f.cat.disabled, live)
	underway := f.calls()
	f.alive(live)
	f.m.tick()
	expect(t, f.calls(), underway, "requests while the reset is under way")

	f.resetDone()
	expect(t, f.calls(), underway+2, "requests in the step that ends the reset")
	expectTime(t, f.lastSnapshot(live).after, liveAt, "live from where the session was when the reset was made")
}

// A producer enabled after the net asked, before the last acceptance, has no
// recovery of the net's that covers what the reset drops of it: no reset.
func TestNet_ProducerEnabledBeforeTheLastAcceptanceCancelsTheReset(t *testing.T) {
	f := upNet(t, all)
	f.cat.disabled[live] = true
	before := f.calls()
	f.stale(1, oldAge, 5*time.Minute, f.untilAsked(before))
	expect(t, f.calls(), before+1, "requests: prematch only")
	delete(f.cat.disabled, live)
	f.accept(f.lastSnapshot(pre))
	expect(t, len(f.out.resets), 0, "resets")
	s := f.m.session(sid(1))
	expect(t, s.pending == nil, true, "a reset pending")
	expect(t, s.nextResetAt > f.m.mono(), true, "the net waits before it tries again")
	for _, l := range s.lanes {
		expect(t, l.gap == nil, true, "a gap of the lane")
	}
}

// The producers are down before the transport is told to replace the queue, which
// it may do at once.
func TestNet_ProducersAreDownBeforeTheTransportIsToldToReplaceTheQueue(t *testing.T) {
	f := upNet(t, all)
	before := f.calls()
	f.stale(1, oldAge, 5*time.Minute, f.untilAsked(before))
	var downAtTheReset []bool
	f.out.onReset = func(uuid.UUID, uint64) {
		downAtTheReset = append(downAtTheReset, f.isDown(pre), f.isDown(live))
	}
	f.accept(f.lastSnapshot(pre))
	f.accept(f.lastSnapshot(live))
	expectSessions(t, f.resetSessions(), []int{1}, "resets")
	if len(downAtTheReset) != 2 || !downAtTheReset[0] || !downAtTheReset[1] {
		t.Fatalf("producers down (prematch, live) when the transport was told: %v, want both", downAtTheReset)
	}
}

// A recovery the API accepted, then abandoned because the producer says it is
// unsubscribed, no longer authorizes the reset: the last acceptance of the other
// producer makes none.
func TestNet_AcceptedRecoveryAbandonedBeforeTheResetCancelsIt(t *testing.T) {
	f := upNet(t, all)
	before := f.calls()
	f.stale(1, oldAge, 5*time.Minute, f.untilAsked(before))
	preReq, liveReq := f.lastSnapshot(pre), f.lastSnapshot(live)
	f.accept(liveReq)
	f.clock.advance(time.Second)
	f.unsubscribed(live)
	expect(t, f.inFlight(live), 0, "the live recovery in flight: given up")
	expect(t, f.m.session(sid(1)).pending == nil, true, "a reset pending")
	f.accept(preReq)
	expect(t, len(f.out.resets), 0, "resets")
	expect(t, f.m.session(sid(1)).nextResetAt > f.m.mono(), true, "the net waits before it tries again")
}

// A producer whose recovery was given up and awaits an alive holds the net back.
func TestNet_ProducerWaitingForAnAliveAfterTheGiveUpHoldsTheNetBack(t *testing.T) {
	f := upNet(t, all)
	f.unsubscribed(live)
	liveReq := f.lastSnapshot(live)
	f.accept(liveReq)
	f.clock.advance(time.Second)
	f.unsubscribed(live)
	expect(t, f.m.byID[live].awaitsAlive, true, "the live producer waits for an alive")
	before := f.calls()
	expect(t, f.stale(1, oldAge, 8*time.Second, f.untilAsked(before)), -1, "a request")
	expect(t, f.m.session(sid(1)).pending == nil, true, "a reset pending")
	expect(t, f.lagging(1), false, "the session lags")
}

// A lost connection cancels the reset, and is no failure of the net's: the net's
// backoff stays as it was, after an earlier cancellation too.
func TestNet_LostConnectionLeavesTheNetsBackoffAsItWas(t *testing.T) {
	f := upNet(t, all)
	before := f.calls()
	f.stale(1, oldAge, 5*time.Minute, f.untilAsked(before))
	f.refuse(f.lastSnapshot(pre))
	s := f.m.session(sid(1))
	backoffEnd := s.nextResetAt
	expect(t, backoffEnd > f.m.mono(), true, "the net waits after the refusal")

	f.complete(f.lastSnapshot(live), 1)
	f.runWithAlives(backoffEnd - f.m.mono() + time.Second)
	again := f.calls()
	f.stale(1, oldAge, 3*time.Minute, f.untilAsked(again))
	expect(t, s.pending != nil, true, "a reset waiting for the API")
	afterRefusal := s.nextResetAt
	f.m.connectionDown()
	expect(t, s.pending == nil, true, "a reset pending after the loss")
	expect(t, s.nextResetAt, afterRefusal, "the net's backoff after the loss")
}

// Another session's channel is lost for real while the reset waits for its last
// acceptance: the recovery of the producer both receive goes with that queue, the
// reset is cancelled without the net's backoff, and the loss gap of the other
// session's lane stays. The live-only session's loss reaches the net's reset of a
// session that also receives prematch: no producer of the session is exempt.
func TestNet_AnotherSessionsChannelLostBeforeTheLastAcceptanceCancelsTheResetWithoutBackoff(t *testing.T) {
	f := twoSessionNet(t)
	before := f.calls()
	f.stale(1, oldAge, 5*time.Minute, f.untilAsked(before))
	preReq, liveReq := f.lastSnapshot(pre), f.lastSnapshot(live)
	f.accept(preReq)
	f.m.channelLost(sid(2))
	s := f.m.session(sid(1))
	expect(t, s.pending == nil, true, "a reset pending")
	expect(t, s.nextResetAt, time.Duration(0), "the net's backoff")
	if l := f.m.session(sid(2)).lane(live); l.gap == nil || !l.hasLoss {
		t.Fatal("the loss gap of session 2's lane is gone")
	}
	for _, l := range s.lanes {
		expect(t, l.gap == nil, true, "a gap of session 1's lane: the net's close")
	}
	f.accept(liveReq)
	expect(t, len(f.out.resets), 0, "resets")
}

// A gap that is open pauses the net's sampling for its producer: a stale sample taken
// at the instant the retry of a refused recovery is due, before the tick that sends
// the retry, starts nothing, though the stale window is shorter than the backoff.
func TestNet_StaleSampleAtTheRetryDeadlineOfAnOpenGapStartsNoReset(t *testing.T) {
	f := &netFeed{feed: withSettings(t, func(s *recoverySettings) { s.staleWindow = 2 * time.Second })}
	f.open(1, all)
	f.startFeed()
	f.bothUp(1)
	f.unsubscribed(pre)
	f.refuse(f.lastSnapshot(pre))
	expect(t, f.m.hasGaps(f.m.byID[pre]), true, "a gap of the prematch producer open")
	sample := func() {
		at := f.now().Add(-oldAge)
		f.m.processed(sid(1), pre, at, at, f.stamp(), 0)
	}
	before := f.calls()
	// the backoff is five seconds: the window of two has passed by the fourth
	for range 4 {
		f.clock.advance(time.Second)
		sample()
		f.m.tick()
	}
	expect(t, f.calls(), before, "requests during the backoff")

	// the deadline: the sample comes before the tick
	f.clock.advance(time.Second)
	sample()
	expect(t, f.calls(), before, "requests after the sample at the deadline")
	expect(t, f.m.session(sid(1)).pending == nil, true, "a reset pending")
	f.m.tick()
	expect(t, f.calls(), before+1, "requests after the tick: the ordinary retry")
	expect(t, f.lastSnapshot(pre).requestID, f.inFlight(pre), "the retry of the prematch producer in flight")
	expect(t, f.m.session(sid(1)).pending == nil, true, "a reset pending after the tick")
}

// The triggering producer is disabled by a client callback on the very sample that
// would start the net: nothing of that sample counts, and the net does not reset for
// the producers left.
func TestNet_TriggeringProducerDisabledOnTheSampleStartsNothing(t *testing.T) {
	f := upNet(t, all)
	before := f.calls()
	f.stale(1, oldAge, 60*time.Second, f.untilAsked(before))
	expect(t, f.calls(), before, "requests before the window is over")
	f.cat.disabled[pre] = true
	f.stale(1, oldAge, 3*time.Second, f.untilAsked(before))
	expect(t, f.calls(), before, "requests")
	expect(t, f.m.session(sid(1)).pending == nil, true, "a reset pending")
	expect(t, f.lagging(1), false, "the session lags")
	expect(t, f.m.session(sid(1)).nextResetAt, time.Duration(0), "the net's backoff")
}

// Both producers can be asked; the session's messages of one are stale and its
// checkpoint of it lies inside the window, while its checkpoint of the other lies
// outside it: the session lags and nothing is asked.
func TestNet_BacklogBeyondTheWindowOfAnotherProducerThanTheSampledOneMakesTheSessionLag(t *testing.T) {
	f := upNetWithWindow(t, 60)
	f.runFor(70 * time.Minute)
	at := f.now()
	f.m.processed(sid(1), pre, at, at, f.stamp(), 0)
	before := f.calls()
	expect(t, f.stale(1, oldAge, 3*time.Minute, f.untilAsked(before)), -1, "a request")
	expect(t, f.calls(), before, "requests")
	expect(t, len(f.out.resets), 0, "resets")
	expect(t, f.lagging(1), true, "the session lags")
	expect(t, f.logCount(notResetOutsideWindow), 1, "warnings of the net not resetting it")
}

// An accepted recovery of the net's times out while the other is not accepted yet: the
// reset is cancelled once, with the net's backoff, and the other's acceptance makes none.
func TestNet_TimeoutOfAnAcceptedRecoveryWhileTheOtherIsNotAcceptedCancelsTheReset(t *testing.T) {
	f := upNet(t, all)
	before := f.calls()
	f.stale(1, oldAge, 5*time.Minute, f.untilAsked(before))
	preReq, liveReq := f.lastSnapshot(pre), f.lastSnapshot(live)
	f.accept(preReq)
	f.runWithAlives(4*time.Minute + 50*time.Second)
	// something of the live recovery puts off its own deadline
	f.processed(1, live, f.now(), liveReq.requestID)
	expect(t, f.m.counters.timedOut, 0, "recoveries timed out so far")
	f.runWithAlives(30 * time.Second)
	expect(t, f.m.counters.timedOut, 1, "recoveries timed out")
	s := f.m.session(sid(1))
	expect(t, s.pending == nil, true, "a reset pending")
	expect(t, f.m.counters.resetRequestsFailed, 1, "net requests failed")
	expect(t, s.nextResetAt > f.m.mono(), true, "the net waits before it tries again")
	f.accept(liveReq)
	expect(t, len(f.out.resets), 0, "resets")
	expect(t, f.m.counters.resetRequestsFailed, 1, "net requests failed after the last acceptance")
}

// A producer with a real gap that is not the sampled one holds the net back, and its
// retry, due at the stale sample, is the ordinary one: the net asks for nothing of its
// own, and proceeds once that recovery is complete and a full window has passed.
func TestNet_ProducerWithAGapHoldsTheNetBackAndItsOrdinaryRetryGoesFirst(t *testing.T) {
	f := &netFeed{feed: withSettings(t, func(s *recoverySettings) { s.staleWindow = 2 * time.Second })}
	f.open(1, all)
	f.startFeed()
	f.bothUp(1)
	f.unsubscribed(live)
	f.refuse(f.lastSnapshot(live))
	before := f.calls()
	sample := func() {
		at := f.now().Add(-oldAge)
		f.m.processed(sid(1), pre, at, at, f.stamp(), 0)
	}
	for range 4 {
		f.clock.advance(time.Second)
		sample()
		f.m.tick()
	}
	expect(t, f.calls(), before, "requests during the backoff")
	f.clock.advance(time.Second)
	sample() // the retry is due, the tick that sends it not yet run
	expect(t, f.calls(), before, "requests after the sample at the deadline")
	expect(t, f.m.session(sid(1)).pending == nil, true, "a reset pending")
	f.m.tick()
	expect(t, f.calls(), before+1, "requests after the tick: the ordinary retry")
	expect(t, f.m.session(sid(1)).pending == nil, true, "a reset pending after the tick")
	f.complete(f.lastSnapshot(live), 1)
	after := f.calls()
	if f.stale(1, oldAge, time.Minute, f.untilAsked(after)) < 0 {
		t.Fatal("the net did not act once the live producer was recovered")
	}
	expect(t, f.calls(), after+2, "requests: both producers")
}
