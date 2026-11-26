package recovery

import (
	"errors"
	"strings"
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

// A producer whose recovery was given up and waits for an alive holds the net back,
// though the stale window elapses meanwhile; once it has spoken and recovered, the net
// goes on.
func TestNet_ProducerWaitingForAnAliveAfterTheGiveUpHoldsTheNetBack(t *testing.T) {
	f := &netFeed{feed: withSettings(t, func(s *recoverySettings) { s.staleWindow = 2 * time.Second })}
	f.open(1, all)
	f.startFeed()
	f.bothUp(1)
	f.unsubscribed(live)
	f.accept(f.lastSnapshot(live))
	f.clock.advance(time.Second)
	f.unsubscribed(live)
	expect(t, f.m.byID[live].awaitsAlive, true, "the live producer waits for an alive")
	before := f.calls()
	sample := func() {
		at := f.now().Add(-oldAge)
		f.m.processed(sid(1), pre, at, at, f.stamp(), 0)
	}
	// the window of two seconds has long elapsed
	for range 8 {
		f.clock.advance(time.Second)
		sample()
		f.m.tick()
	}
	expect(t, f.calls(), before, "requests")
	expect(t, f.m.session(sid(1)).pending == nil, true, "a reset pending")
	expect(t, f.lagging(1), false, "the session lags")

	f.alive(live)
	f.complete(f.lastSnapshot(live), 1)
	after := f.calls()
	for range 5 {
		f.clock.advance(time.Second)
		f.alive(live)
		sample()
		f.m.tick()
	}
	expect(t, f.calls(), after+2, "requests once the producer has spoken and recovered: both")
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

// staleOf is, second by second, session taking a message of the producer that is age
// old by its clock, with both producers' alives every ten seconds, until until holds:
// the seconds that took, or -1.
func (n *netFeed) staleOf(session, producer int, age time.Duration, limit time.Duration, until func() bool) int {
	for second := 1; second <= int(limit/time.Second); second++ {
		n.clock.advance(time.Second)
		if int(n.now().Sub(n.start)/time.Second)%10 == 0 {
			n.aliveBoth()
		}
		at := n.now().Add(-age)
		n.m.processed(sid(session), producer, at, at, n.stamp(), 0)
		n.m.tick()
		if until() {
			return second
		}
	}
	return -1
}

// The live producer's messages can start the net as the prematch producer's do: the
// same recoveries, the same reset, and the pending reset names the live producer.
func TestNet_LiveProducersStaleMessagesStartTheNetAsPrematchOnesDo(t *testing.T) {
	f := upNet(t, all)
	before := f.calls()
	expect(t, f.staleOf(1, live, oldAge, 5*time.Minute, f.untilAsked(before)), 61, "seconds to the first request")
	expect(t, f.calls(), before+2, "requests: both producers")
	expect(t, f.m.session(sid(1)).pending.producerID, live, "the producer the reset names")
	f.accept(f.lastSnapshot(pre))
	f.accept(f.lastSnapshot(live))
	expectSessions(t, f.resetSessions(), []int{1}, "resets")
	expect(t, f.m.session(sid(1)).underwayProducer, live, "the producer the reset under way names")
}

// The live producer disabled on the sample that would start the net starts nothing.
func TestNet_LiveTriggeringProducerDisabledOnTheSampleStartsNothing(t *testing.T) {
	f := upNet(t, all)
	before := f.calls()
	f.staleOf(1, live, oldAge, 60*time.Second, f.untilAsked(before))
	expect(t, f.calls(), before, "requests before the window is over")
	f.cat.disabled[live] = true
	f.staleOf(1, live, oldAge, 3*time.Second, f.untilAsked(before))
	expect(t, f.calls(), before, "requests")
	expect(t, f.m.session(sid(1)).pending == nil, true, "a reset pending")
	expect(t, f.lagging(1), false, "the session lags")
}

// A session fed only delayed valid alives of its own, the producers' own alives
// current, starts the net after the full window; at the limit it does not.
func TestNet_SessionAlivesAloneStartTheNetAfterTheFullWindowAndNotAtTheLimit(t *testing.T) {
	aliveStale := func(f *netFeed, age time.Duration, held bool, limit time.Duration, until func() bool) int {
		for second := 1; second <= int(limit/time.Second); second++ {
			f.clock.advance(time.Second)
			if int(f.now().Sub(f.start)/time.Second)%10 == 0 {
				f.aliveBoth()
			}
			at := f.now().Add(-age)
			f.m.sessionAlive(sid(1), pre, at, at, f.stamp(), true, held)
			f.m.tick()
			if until() {
				return second
			}
		}
		return -1
	}
	f := upNet(t, all)
	before := f.calls()
	expect(t, aliveStale(f, 2*time.Minute, false, 3*time.Minute, f.untilAsked(before)), -1, "a request with the alives at the limit")

	f = upNet(t, all)
	before = f.calls()
	expect(t, aliveStale(f, oldAge, false, 5*time.Minute, f.untilAsked(before)), 61, "seconds to the first request")
	expect(t, f.calls(), before+2, "requests: both producers")

	// a message handed to the client that has not settled holds the checkpoint, not the net
	f = upNet(t, all)
	before = f.calls()
	expect(t, aliveStale(f, oldAge, true, 5*time.Minute, f.untilAsked(before)), 61, "seconds to the first request with the alives held")

	// an open gap pauses the net
	f = upNet(t, all)
	f.m.byID[pre].gap = f.m.openGap(nil, f.now(), false)
	before = f.calls()
	aliveStale(f, oldAge, false, 3*time.Minute, func() bool { return false })
	expect(t, f.calls(), before+1, "requests with a gap open: its own recovery only")
	expect(t, f.m.session(sid(1)).pending == nil, true, "a reset pending")
}

// The net's warning says how a recovery ended, never what the API answered: a
// response text that reflects a credential does not reach the log.
func TestNet_WarningOfARefusedRequestCarriesNoTextOfTheAnswer(t *testing.T) {
	f := upNet(t, all)
	before := f.calls()
	f.stale(1, oldAge, 5*time.Minute, f.untilAsked(before))
	f.m.answered(f.lastSnapshot(pre).requestID, errors.New("response_code=tok-3f9a-secret"))
	var warnings []string
	for _, line := range f.logs.take() {
		if strings.HasPrefix(line, "WARN The safety net did not reset") {
			warnings = append(warnings, line)
		}
	}
	expect(t, len(warnings), 1, "warnings of the reset not made")
	if strings.Contains(warnings[0], "secret") || !strings.Contains(warnings[0], endedRefused) {
		t.Fatalf("the warning is %q, want the classification %q and none of the answer", warnings[0], endedRefused)
	}
}

// The producer that is accepted and then silent, before the last acceptance, would
// have its follow-up blocked once the queue is gone: no reset, with the net's backoff.
func TestNet_ProducerThatCannotBeAskedAtTheLastAcceptanceCancelsTheReset(t *testing.T) {
	f := upNet(t, all)
	before := f.calls()
	f.stale(1, oldAge, 5*time.Minute, f.untilAsked(before))
	preReq, liveReq := f.lastSnapshot(pre), f.lastSnapshot(live)
	f.accept(liveReq)
	// the live producer's alive is older than the inactivity limit; no tick has run
	f.clock.advance(25 * time.Second)
	f.alive(pre)
	f.accept(preReq)
	expect(t, len(f.out.resets), 0, "resets")
	s := f.m.session(sid(1))
	expect(t, s.pending == nil, true, "a reset pending")
	expect(t, f.m.counters.resetRequestsFailed, 1, "net requests failed")
	expect(t, s.nextResetAt > f.m.mono(), true, "the net waits before it tries again")
}

// After a step back of the producers' clocks the session's checkpoints lie after their
// now: a recovery is capped at it, so the net waits, with no lagging or backoff, until
// the clocks have passed them.
func TestNet_CheckpointAheadOfTheProducersClockHoldsTheNetBack(t *testing.T) {
	f := upNet(t, all)
	f.preSkew, f.liveSkew = 30*time.Minute, 30*time.Minute
	before := f.calls()
	expect(t, f.stale(1, oldAge, 3*time.Minute, f.untilAsked(before)), -1, "a request")
	expect(t, f.calls(), before, "requests")
	expect(t, f.lagging(1), false, "the session lags")
	expect(t, f.m.session(sid(1)).pending == nil, true, "a reset pending")
	expect(t, f.m.session(sid(1)).nextResetAt, time.Duration(0), "the net's backoff")
}

// The same step back between the net's request and the last acceptance cancels the reset.
func TestNet_CheckpointAheadOfTheProducersClockAtTheLastAcceptanceCancelsTheReset(t *testing.T) {
	f := upNet(t, all)
	before := f.calls()
	f.stale(1, oldAge, 5*time.Minute, f.untilAsked(before))
	preReq, liveReq := f.lastSnapshot(pre), f.lastSnapshot(live)
	at := f.now().Add(-30 * time.Minute)
	f.m.alive(pre, at, at, f.stamp(), true)
	f.m.alive(live, at, at, f.stamp(), true)
	f.accept(preReq)
	f.accept(liveReq)
	expect(t, len(f.out.resets), 0, "resets")
	s := f.m.session(sid(1))
	expect(t, s.pending == nil, true, "a reset pending")
	expect(t, s.nextResetAt > f.m.mono(), true, "the net waits before it tries again")
	expect(t, f.logCount("lies after the producer's clock"), 1, "warnings of the clock")
}

// A recovery that reaches its absolute deadline while the reset of its session is
// under way is given up, not failed: no retry delay, and the follow-up goes out in the
// step that ends the reset, from where the session was.
func TestNet_DeadlineOfARecoveryWhileTheResetIsUnderWayLeavesNoRetryDelay(t *testing.T) {
	f := &netFeed{feed: withSettings(t, func(s *recoverySettings) { s.maxRecoveryTime = 10 * time.Minute })}
	f.open(1, all)
	f.startFeed()
	f.bothUp(1)
	before := f.calls()
	f.stale(1, oldAge, 5*time.Minute, f.untilAsked(before))
	preAt, _ := f.checkpoint(1, pre)
	liveAt, _ := f.checkpoint(1, live)
	f.accept(f.lastSnapshot(pre))
	f.accept(f.lastSnapshot(live))
	expectSessions(t, f.resetSessions(), []int{1}, "resets")
	f.runWithAlives(11 * time.Minute)
	expect(t, f.inFlight(pre), 0, "the prematch recovery in flight: past its deadline")
	expect(t, f.m.counters.failed, 0, "recoveries failed")
	expect(t, f.m.counters.timedOut, 0, "recoveries timed out")
	underway := f.calls()
	f.resetDone()
	expect(t, f.calls(), underway+2, "requests in the step that ends the reset")
	expectTime(t, f.lastSnapshot(pre).after, preAt, "prematch from where the session was")
	expectTime(t, f.lastSnapshot(live).after, liveAt, "live from where the session was")
	for _, p := range []int{pre, live} {
		expect(t, f.m.byID[p].retryAt, time.Duration(0), "the producer's retry delay")
	}
}

// The channel of another session that receives a producer the reset asked for is lost
// after that session saw the producer's snapshot_complete: the reset goes with it, with
// no backoff, and the loss gap of that session's lane stays.
func TestNet_SiblingChannelLostAfterItSawTheSnapshotCompleteCancelsTheResetWithoutBackoff(t *testing.T) {
	f := twoSessionNet(t)
	before := f.calls()
	f.stale(1, oldAge, 5*time.Minute, f.untilAsked(before))
	liveReq := f.lastSnapshot(live)
	f.snapshotComplete(2, live, liveReq.requestID)
	expect(t, f.m.session(sid(1)).pending != nil, true, "a reset waiting for the API")
	f.m.channelLost(sid(2))
	expect(t, f.inFlight(live), liveReq.requestID, "the live recovery: its session had seen it, it stays")
	s := f.m.session(sid(1))
	expect(t, s.pending == nil, true, "a reset pending")
	expect(t, s.nextResetAt, time.Duration(0), "the net's backoff")
	if l := f.m.session(sid(2)).lane(live); l.gap == nil || !l.hasLoss {
		t.Fatal("the loss gap of session 2's lane is gone")
	}
	for _, l := range s.lanes {
		expect(t, l.gap == nil, true, "a gap of session 1's lane: the net's close")
	}
}

// An answer handled long after it arrived is judged, and its cancellation timed, by
// the actor's own clock.
func TestNet_CancellationOfALateHandledAnswerStartsTheBackoffNow(t *testing.T) {
	f := upNet(t, all)
	before := f.calls()
	f.stale(1, oldAge, 5*time.Minute, f.untilAsked(before))
	preReq := f.lastSnapshot(pre)
	f.cat.disabled[live] = true
	f.m.answeredAt(preReq.requestID, nil, f.m.mono()-10*time.Minute)
	s := f.m.session(sid(1))
	expect(t, s.pending == nil, true, "a reset pending")
	expect(t, s.nextResetAt-f.m.mono(), time.Minute, "the net's backoff from now")
}

// A producer enabled while a reset of one of its sessions is under way is reported
// down for it, not left up with the reset's gap open.
func TestNet_ProducerEnabledWhileTheResetIsUnderWayIsDown(t *testing.T) {
	f := upNet(t, all)
	f.cat.disabled[live] = true
	before := f.calls()
	f.stale(1, oldAge, 5*time.Minute, f.untilAsked(before))
	f.accept(f.lastSnapshot(pre))
	expectSessions(t, f.resetSessions(), []int{1}, "resets")
	expectDown(t, f.feed, live, false, "the disabled live producer, untouched by the reset")
	delete(f.cat.disabled, live)
	f.alive(live)
	expectDown(t, f.feed, live, true, "the live producer enabled during the reset")
	f.expectCause(live, causeSafetyNetReset, "its cause")
	f.resetDone()
	f.complete(f.lastSnapshot(live), 1)
	f.complete(f.lastSnapshot(pre), 1)
	f.m.tick()
	expectDown(t, f.feed, live, false, "the live producer after the recovery asked for after the reset")
}

// The old recovery of a producer enabled during the reset completes, from another
// session, while the reset is under way: the producer stays down until the recovery
// asked for after the reset has completed.
func TestNet_OldRecoveryCompletedByAnotherSessionDuringTheResetDoesNotBringTheProducerUp(t *testing.T) {
	f, liveReq := liveDisabledWithARecoverySeenByOneSession(t)
	before := f.calls()
	f.stale(1, oldAge, 5*time.Minute, f.untilAsked(before))
	f.accept(f.lastSnapshot(pre))
	expectSessions(t, f.resetSessions(), []int{1}, "resets")
	delete(f.cat.disabled, live)
	f.alive(live)
	f.snapshotComplete(2, live, liveReq.requestID)
	expectDown(t, f.feed, live, true, "the live producer after the old recovery completed")
	f.resetDone()
	again := f.lastSnapshot(live)
	if again.requestID == liveReq.requestID {
		t.Fatal("the live recovery was not asked for again")
	}
	expectDown(t, f.feed, live, true, "the live producer before the recovery after the reset completes")
	f.complete(again, 1, 2)
	f.complete(f.lastSnapshot(pre), 1, 2)
	expectDown(t, f.feed, live, false, "the live producer after it")
}

// A lane that is disabled does not keep the session lagging: its stale flag is not
// looked at, and the tick clears it.
func TestNet_DisabledLaneDoesNotKeepTheSessionLagging(t *testing.T) {
	f := upNet(t, all)
	s := f.m.session(sid(1))
	s.lagging = true
	s.lane(pre).above = true
	f.cat.disabled[pre] = true
	f.clock.advance(time.Second)
	f.alive(live)
	f.live(1, live, freshAge)
	expect(t, f.lagging(1), false, "the session lags")
}

func TestNet_TickClearsTheStaleWindowOfADisabledProducersLanes(t *testing.T) {
	f := upNet(t, all)
	l := f.m.session(sid(1)).lane(pre)
	l.above, l.aboveSince = true, 5*time.Second
	f.cat.disabled[pre] = true
	f.m.tick()
	expect(t, l.above, false, "the lane above the limit")
	expect(t, l.aboveSince, time.Duration(0), "the window's start")
}

// The final window check looks at every producer of the session: only the other
// producer's checkpoint crosses the margin, the sampled one's stays inside.
func TestNet_CheckpointOfTheOtherProducerLeavesTheWindowBeforeTheLastAcceptanceCancelsTheReset(t *testing.T) {
	f := upNetWithWindow(t, 60)
	f.runFor(40 * time.Minute)
	before := f.calls()
	f.stale(1, oldAge, 5*time.Minute, f.untilAsked(before))
	preReq, liveReq := f.lastSnapshot(pre), f.lastSnapshot(live)
	f.accept(preReq)
	f.clock.advance(10 * time.Minute)
	f.aliveBoth()
	f.m.processed(sid(1), pre, f.now(), f.now(), f.stamp(), 0) // the sampled producer's checkpoint is current
	f.accept(liveReq)
	expect(t, len(f.out.resets), 0, "resets")
	expect(t, f.logCount("reaches back past the producer's recovery window of 60 minutes"), 1, "warnings of the window")
	expect(t, f.m.session(sid(1)).pending == nil, true, "a reset pending")
}
