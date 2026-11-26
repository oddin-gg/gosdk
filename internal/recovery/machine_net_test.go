package recovery

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/types"
)

// The safety net: when it acts, what it asks for first, when it is off or paused,
// and its own cap. The limit is two minutes and the window one, as defaultSettings
// has them; the messages here are three minutes old.

const (
	oldAge   = 3 * time.Minute
	freshAge = time.Second
)

// netFeed is a feed with the producers' clocks, and the alives they send, as the
// net's tests set them.
type netFeed struct {
	*feed
	// preSkew and liveSkew are how far behind the SDK's clock each producer's
	// clock is.
	preSkew, liveSkew time.Duration
	// noAlives stops both producers' alives; noLiveAlives only the live one's.
	noAlives, noLiveAlives bool
	// requestID is the request id of the messages stale feeds: 0 for live ones.
	requestID int
}

func newNet(t *testing.T) *netFeed { return &netFeed{feed: newFeed(t)} }

// upNet is session 1 of the interest, started, with both producers' first
// recoveries complete.
func upNet(t *testing.T, interest types.MessageInterest) *netFeed {
	t.Helper()
	return &netFeed{feed: upWith(t, interest)}
}

func (n *netFeed) aliveBoth() {
	at := n.now()
	n.m.alive(pre, at.Add(-n.preSkew), at.Add(-n.preSkew), n.stamp(), true)
	if !n.noLiveAlives {
		n.m.alive(live, at.Add(-n.liveSkew), at.Add(-n.liveSkew), n.stamp(), true)
	}
}

// stale is, second by second, session taking a live message of the prematch
// producer that is age old by that producer's clock, with both producers' alives
// every ten seconds of the clock, until until holds. It is the seconds that
// took, or -1 when it did not hold within limit.
func (n *netFeed) stale(session int, age, limit time.Duration, until func() bool) int {
	return n.staleRun(session, age, limit, until, false)
}

// staleBeforeTheTick is stale with until looked at right after the message,
// before the second's tick: what the message itself did is all there is.
func (n *netFeed) staleBeforeTheTick(session int, age, limit time.Duration, until func() bool) int {
	return n.staleRun(session, age, limit, until, true)
}

func (n *netFeed) staleRun(session int, age, limit time.Duration, until func() bool, beforeTheTick bool) int {
	for second := 1; second <= int(limit/time.Second); second++ {
		n.clock.advance(time.Second)
		if !n.noAlives && int(n.now().Sub(n.start)/time.Second)%10 == 0 {
			n.aliveBoth()
		}
		at := n.now().Add(-n.preSkew - age)
		n.m.processed(sid(session), pre, at, at, n.stamp(), n.requestID)
		if beforeTheTick && until() {
			return second
		}
		n.m.tick()
		if until() {
			return second
		}
	}
	return -1
}

// calls is how many requests, snapshots and events, have gone out.
func (n *netFeed) calls() int { return len(n.out.calls) }

// untilAsked holds once more requests have gone out than before.
func (n *netFeed) untilAsked(before int) func() bool {
	return func() bool { return n.calls() > before }
}

func (f *feed) lagging(n int) bool {
	s := f.m.session(sid(n))
	return s != nil && s.lagging
}

// resetDone is the transport making the last reset, and refused the transport
// not taking it.
func (f *feed) resetDone() {
	f.t.Helper()
	f.reportReset(true)
}

func (f *feed) resetRefused() {
	f.t.Helper()
	f.reportReset(false)
}

func (f *feed) reportReset(replaced bool) {
	f.t.Helper()
	if len(f.out.resets) == 0 {
		f.t.Fatal("no reset was asked for")
	}
	last := len(f.out.resets) - 1
	f.m.resetDone(f.out.resets[last], f.out.resetNumbers[last], replaced)
}

// resetsOf are the numbers of the session's sessions the machine asked to reset,
// as session numbers.
func (f *feed) resetSessions() []int {
	var out []int
	for _, id := range f.out.resets {
		for _, n := range f.opened {
			if sid(n) == id {
				out = append(out, n)
			}
		}
	}
	return out
}

// logCount is how many of the machine's log lines so far contain what.
func (f *feed) logCount(what string) int {
	f.seenLogs = append(f.seenLogs, f.logs.take()...)
	n := 0
	for _, line := range f.seenLogs {
		if strings.Contains(line, what) {
			n++
		}
	}
	return n
}

func (n *netFeed) expectCause(p int, want statusCause, what string) {
	n.t.Helper()
	expect(n.t, n.lastStatus(p).cause, want, what)
}

func expectSessions(t *testing.T, got, want []int, what string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("%s: got %v, want %v", what, got, want)
	}
}

// ---- SafetyNetTest

// SafetyNetTest: liveMessagesTooOldForTheWindowGetRecoveriesFirstAndTheResetOnceAllAreAccepted
func TestNet_LiveMessagesTooOldForTheWindowGetRecoveriesFirstAndTheResetOnceAllAreAccepted(t *testing.T) {
	f := upNet(t, all)
	before := f.calls()
	expect(t, f.stale(1, oldAge, 5*time.Minute, f.untilAsked(before)), 61, "seconds to the first request: the window after the first stale sample")
	preReq, liveReq := f.lastSnapshot(pre), f.lastSnapshot(live)
	expect(t, f.calls(), before+2, "one recovery for each producer of the session")
	cp, _ := f.checkpoint(1, pre)
	expectTime(t, preReq.after, cp, "where the recovery starts")
	expect(t, len(f.out.resets), 0, "resets before the API has accepted")

	f.accept(preReq)
	expect(t, len(f.out.resets), 0, "resets with one of two accepted")
	f.accept(liveReq)
	expectSessions(t, f.resetSessions(), []int{1}, "resets")
	expect(t, f.m.counters.resets, 0, "resets made before the transport has made it")
	expect(t, f.logCount("WARN The safety net resets session"), 1, "warnings of the reset")
	f.expectCause(pre, causeSafetyNetReset, "the prematch producer's cause")
	f.expectCause(live, causeSafetyNetReset, "the live producer's cause")
	expect(t, f.lastStatus(pre).reason(), types.ProcessingQueueDelayViolationProducerStatusReason, "the public reason")
	expect(t, f.calls(), before+2, "requests while the transport resets")

	// what the first two sent before the reset may have gone with the old queue
	f.resetDone()
	expect(t, f.m.counters.resets, 1, "resets made")
	preAgain, liveAgain := f.lastSnapshot(pre), f.lastSnapshot(live)
	expect(t, f.calls(), before+4, "requests once the reset is done")
	expectTime(t, preAgain.after, preReq.after, "the prematch recovery asked for again")
	expectTime(t, liveAgain.after, liveReq.after, "the live recovery asked for again")
	f.complete(preAgain, 1)
	f.complete(liveAgain, 1)
	f.expectCause(pre, causeProcessingDelay, "recovered, but its session still processes it late")
	expectDown(t, f.feed, live, false, "the live producer")
}

// SafetyNetTest: aSessionsAliveWithoutAPositiveTimestampIsNoSampleAndMovesNothing
func TestNet_SessionAliveWithoutAPositiveTimestampIsNoSampleAndMovesNothing(t *testing.T) {
	for _, stamp := range []time.Time{time.UnixMilli(0), time.UnixMilli(-time.Hour.Milliseconds())} {
		f := upNet(t, all)
		f.clock.advance(time.Second)
		f.processed(1, pre, f.now(), 0)
		checkpoint, _ := f.checkpoint(1, pre)
		before := f.calls()
		// the session takes nothing but such alives, past the window after the limit
		for second := 1; second <= int((5 * time.Minute).Seconds()); second++ {
			f.clock.advance(time.Second)
			if second%10 == 0 {
				f.aliveBoth()
			}
			f.m.sessionAlive(sid(1), pre, stamp, stamp, f.stamp(), second%2 == 0, false)
			f.m.tick()
		}
		expect(t, f.calls(), before, "requests, with the session's alives stamped "+stamp.String())
		got, _ := f.checkpoint(1, pre)
		expectTime(t, got, checkpoint, "the checkpoint")
	}
}

// SafetyNetTest: aSnapshotCompleteWhileTheResetIsUnderWayIsIgnoredAndTheRecoveryAskedForAgain
func TestNet_SnapshotCompleteWhileTheResetIsUnderWayIsIgnoredAndTheRecoveryAskedForAgain(t *testing.T) {
	f := &netFeed{feed: newFeed(t)}
	f.open(1, all)
	f.open(2, all)
	f.startFeed()
	f.bothUp(1, 2)
	before := f.calls()
	f.stale(1, oldAge, 5*time.Minute, f.untilAsked(before))
	preReq, liveReq := f.lastSnapshot(pre), f.lastSnapshot(live)
	f.accept(preReq)
	f.accept(liveReq)
	expectSessions(t, f.resetSessions(), []int{1}, "resets")

	// the transport has not replaced the channel yet: these come from the old queue
	f.snapshotComplete(1, pre, preReq.requestID)
	f.snapshotComplete(2, pre, preReq.requestID)
	f.runWithAlives(20 * time.Second)
	expectDown(t, f.feed, pre, true, "while the reset is under way")
	expect(t, f.calls(), before+2, "requests while it is")

	f.resetDone()
	again := f.lastSnapshot(pre)
	if again.requestID == preReq.requestID {
		t.Fatal("the recovery was not asked for under a new request id")
	}
	f.complete(again, 1, 2)
	expectDown(t, f.feed, pre, false, "after the new recovery")
	// a late report of the same reset changes nothing
	f.resetDone()
	expectDown(t, f.feed, pre, false, "after a late report")
}

// SafetyNetTest: theNetWaitsWhileAnotherProducerOfTheSessionHasARecoveryInFlight
func TestNet_WaitsWhileAnotherProducerOfTheSessionHasARecoveryInFlight(t *testing.T) {
	f := upNet(t, all)
	f.unsubscribed(live)
	liveReq := f.lastSnapshot(live)
	f.accept(liveReq)
	before := f.calls()
	expect(t, f.stale(1, oldAge, 3*time.Minute, f.untilAsked(before)), -1, "a request while the live producer recovers")
	expect(t, len(f.out.resets), 0, "resets")

	f.snapshotComplete(1, live, liveReq.requestID)
	expect(t, f.stale(1, oldAge, 3*time.Minute, f.untilAsked(before)), 1, "seconds to the net's request once it is done: stale all along")
	expect(t, f.calls(), before+2, "requests")
}

// SafetyNetTest: aProducerWithItsCapSpentDoesNotHoldTheNetBack
func TestNet_ProducerWithItsCapSpentDoesNotHoldTheNetBack(t *testing.T) {
	f := upNet(t, all)
	f.unsubscribed(live)
	f.refuse(f.lastSnapshot(live))
	preBefore := len(f.snapshots(pre))
	// the live producer's recovery is refused each time it is asked for again, until
	// it is at its slow retry
	seconds := 0
	for len(f.snapshots(pre)) == preBefore && seconds < 180 {
		f.stale(1, oldAge, time.Second, func() bool { return false })
		seconds++
		liveReq := f.lastSnapshot(live)
		if f.inFlight(live) == liveReq.requestID {
			f.refuse(liveReq)
		}
	}
	expect(t, seconds, 61, "seconds to the net's request: the window")
	expect(t, f.m.counters.failed, 4, "the live producer's retries spent by then")
	liveAsked := len(f.snapshots(live))
	f.accept(f.lastSnapshot(pre))
	expectSessions(t, f.resetSessions(), []int{1}, "the one producer it could ask for accepted")
	f.resetDone()
	expect(t, len(f.snapshots(live)), liveAsked, "the live producer, waiting for its slow retry, waits")
}

// SafetyNetTest: aSilentProducerDoesNotHoldTheNetBackAndIsAskedForOnceItSpeaksAgain
func TestNet_SilentProducerDoesNotHoldTheNetBackAndIsAskedForOnceItSpeaksAgain(t *testing.T) {
	f := upNet(t, all)
	before := f.calls()
	f.noLiveAlives = true
	expect(t, f.stale(1, oldAge, 3*time.Minute, f.untilAsked(before)), 61, "seconds to the net's request, the live producer silent since 20 s")
	f.expectCause(live, causeAliveIntervalViolation, "the live producer's cause")
	expect(t, f.calls(), before+1, "requests: the prematch producer only")
	liveCheckpoint, _ := f.checkpoint(1, live)
	f.accept(f.lastSnapshot(pre))
	expectSessions(t, f.resetSessions(), []int{1}, "resets")
	f.resetDone()
	expect(t, len(f.snapshots(pre)), 3, "recoveries of the prematch producer: asked for again after the reset")

	f.noLiveAlives = false
	f.alive(live)
	expectTime(t, f.lastSnapshot(live).after, liveCheckpoint,
		"the live producer misses what the reset dropped of it, and what it missed while silent")
}

// SafetyNetTest: aProducerIsNotUpWhileALowPrioritySessionIsBeingReset
func TestNet_ProducerIsNotUpWhileALowPrioritySessionIsBeingReset(t *testing.T) {
	f := &netFeed{feed: newFeed(t)}
	f.open(1, hi)
	f.open(2, low)
	f.startFeed()
	f.bothUp(1, 2)
	before := f.calls()
	f.stale(2, oldAge, 5*time.Minute, f.untilAsked(before))
	preReq, liveReq := f.lastSnapshot(pre), f.lastSnapshot(live)
	f.accept(preReq)
	f.accept(liveReq)
	expectSessions(t, f.resetSessions(), []int{2}, "resets")

	// the high-priority session sees them while the low-priority one is still being reset
	f.snapshotComplete(1, pre, preReq.requestID)
	f.snapshotComplete(1, live, liveReq.requestID)
	f.runWithAlives(20 * time.Second)
	expectDown(t, f.feed, pre, true, "while the reset is under way")
	expect(t, f.calls(), before+2, "requests into a queue about to go")

	f.resetDone()
	expect(t, f.calls(), before+4, "requests once the reset is done")
	f.complete(f.lastSnapshot(pre), 1, 2)
	expectDown(t, f.feed, pre, false, "after the new recovery")
}

// SafetyNetTest: aResetTheTransportCouldNotTakeAsksAgainForWhatItIgnoredAndDropsNothing
func TestNet_ResetTheTransportCouldNotTakeAsksAgainForWhatItIgnoredAndDropsNothing(t *testing.T) {
	f := upNet(t, all)
	before := f.calls()
	f.stale(1, oldAge, 5*time.Minute, f.untilAsked(before))
	preReq := f.lastSnapshot(pre)
	f.accept(preReq)
	f.accept(f.lastSnapshot(live))
	// ignored while the reset was meant to be under way
	f.snapshotComplete(1, pre, preReq.requestID)
	f.resetRefused()
	expect(t, f.calls(), before+4, "requests: asked for again, nothing else waiting for the session")
	expectTime(t, f.lastSnapshot(pre).after, preReq.after, "from where the net started")
	f.complete(f.lastSnapshot(pre), 1)
	f.complete(f.lastSnapshot(live), 1)
	f.expectCause(pre, causeProcessingDelay, "recovered, but its session still processes it late")
	expectDown(t, f.feed, live, false, "the live producer")
	expect(t, f.m.counters.resets, 0, "resets made")
}

// SafetyNetTest: aSessionThatSeesTheSnapshotCompleteOfARecoveryItsResetAskedForIsNotReset
func TestNet_SessionThatSeesTheSnapshotCompleteOfARecoveryItsResetAskedForIsNotReset(t *testing.T) {
	f := &netFeed{feed: newFeed(t)}
	f.open(1, all)
	f.open(2, all)
	f.startFeed()
	f.bothUp(1, 2)
	before := f.calls()
	f.stale(1, oldAge, 5*time.Minute, f.untilAsked(before))
	preReq := f.lastSnapshot(pre)
	f.accept(preReq)
	// the lagging session has what the recovery sent; the other session has yet to see it
	f.snapshotComplete(1, pre, preReq.requestID)
	f.accept(f.lastSnapshot(live))
	expect(t, len(f.out.resets), 0, "resets: a reset now would drop what it just got")
}

// SafetyNetTest: aResetTheTransportCouldNotTakeCountsAgainstNeitherTheCapNorTheBackoff
func TestNet_ResetTheTransportCouldNotTakeCountsAgainstNeitherTheCapNorTheBackoff(t *testing.T) {
	f := upNet(t, all)
	start := f.now()
	for round := 1; round <= 4; round++ {
		before := f.calls()
		expect(t, f.stale(1, oldAge, 5*time.Minute, f.untilAsked(before)), 61, "seconds to the net's request in round")
		f.accept(f.lastSnapshot(pre))
		f.accept(f.lastSnapshot(live))
		f.resetRefused()
		f.complete(f.lastSnapshot(pre), 1)
		f.complete(f.lastSnapshot(live), 1)
	}
	expect(t, f.lagging(1), false, "lagging after four resets not made")
	expect(t, f.m.counters.resets, 0, "resets made")
	expect(t, f.now().Sub(start), 4*61*time.Second, "time the four rounds took")
}

// SafetyNetTest: whatTheResetDropsOfAProducerItCouldNotAskForStartsWhereItWasWhenTheResetWasMade
func TestNet_WhatTheResetDropsOfAProducerItCouldNotAskForStartsWhereItWasWhenTheResetWasMade(t *testing.T) {
	f := &netFeed{feed: newFeed(t)}
	f.open(1, all)
	f.open(2, liveOnly)
	f.startFeed()
	f.bothUp(1, 2)
	// the live producer's retries spent, for a gap of session 2 that starts far ahead
	ahead := f.now().Add(time.Hour)
	f.m.processed(sid(2), live, ahead, ahead, f.stamp(), 0)
	f.m.channelLost(sid(2))
	f.m.channelReopened(sid(2))
	for range 4 {
		f.refuse(f.lastSnapshot(live))
		f.runWithAlives(20 * time.Second)
	}
	f.expectCause(live, causeRecoveryFailed, "the live producer's cause")

	// its slow retry goes out now and then, and is refused each time
	preBefore := len(f.snapshots(pre))
	for seconds := 0; len(f.snapshots(pre)) == preBefore && seconds < 300; seconds++ {
		f.stale(1, oldAge, time.Second, func() bool { return false })
		if liveReq := f.lastSnapshot(live); f.inFlight(live) == liveReq.requestID {
			f.refuse(liveReq)
		}
	}
	expect(t, len(f.snapshots(pre)), preBefore+1, "recoveries of the prematch producer")
	atReset, _ := f.checkpoint(1, live)
	f.accept(f.lastSnapshot(pre))
	expectSessions(t, f.resetSessions(), []int{1}, "resets")
	// the new channel delivers before the transport reports the reset done
	later := f.now().Add(2 * time.Hour)
	f.m.sessionAlive(sid(1), live, later, later, f.stamp(), true, false)
	f.resetDone()

	f.runWithAlives(10 * time.Minute)
	expectTime(t, f.lastSnapshot(live).after, atReset, "from where session 1 was when the reset was made")
}

// SafetyNetTest: aSessionNoOneTakesSnapshotCompletesForIsResetOnceTheApiAcceptedEveryRecovery
//
// Every session with a lane takes snapshot_completes in Go and is awaited: a recovery
// never completes on the API's acceptance while a session receives its producer.
// What the rule says here, that the reset waits for the acceptance of every
// recovery, is the first test's.

// SafetyNetTest: aLaggingSessionStopsLaggingOnceTheTransportHasMadeTheNextReset
func TestNet_LaggingSessionStopsLaggingOnceTheTransportHasMadeTheNextReset(t *testing.T) {
	f := upNet(t, all)
	for round := 0; !f.lagging(1); round++ {
		if round > 2*safetyNetResets {
			t.Fatal("the session never lagged: the net kept resetting it")
		}
		before := f.calls()
		f.stale(1, oldAge, 10*time.Minute, func() bool { return f.calls() > before || f.lagging(1) })
		if f.calls() > before {
			f.accept(f.lastSnapshot(pre))
			f.accept(f.lastSnapshot(live))
			f.resetDone()
			f.complete(f.lastSnapshot(pre), 1)
			f.complete(f.lastSnapshot(live), 1)
		}
	}
	// still behind, until the first reset ages out of the cool-down and the net acts again
	before := f.calls()
	f.stale(1, oldAge, 10*time.Minute, f.untilAsked(before))
	f.accept(f.lastSnapshot(pre))
	f.accept(f.lastSnapshot(live))
	expect(t, f.lagging(1), true, "lagging until the transport has made the reset")
	f.resetDone()
	expect(t, f.lagging(1), false, "lagging once it has")
	expect(t, f.m.counters.resets, 4, "resets made")
	expect(t, f.logCount("INFO Session "+sid(1).String()+" caught up"), 1, "lines saying it caught up")
}

// SafetyNetTest: aResetWithTheTransportWhenTheSessionsChannelGoesStillCountsWhenItIsDone
func TestNet_ResetWithTheTransportWhenTheSessionsChannelGoesStillCountsWhenItIsDone(t *testing.T) {
	f := upNet(t, all)
	before := f.calls()
	f.stale(1, oldAge, 5*time.Minute, f.untilAsked(before))
	f.accept(f.lastSnapshot(pre))
	f.accept(f.lastSnapshot(live))
	expectSessions(t, f.resetSessions(), []int{1}, "resets")

	f.m.channelLost(sid(1))
	f.alive(pre)
	f.alive(live)
	// the reset opens the new channel, and then reports itself done
	f.m.channelReopened(sid(1))
	expect(t, f.calls(), before+2, "requests while the reset may still drop the new channel's queue")
	f.resetDone()
	expect(t, f.calls(), before+4, "requests once it is done")
	expect(t, f.m.counters.resets, 1, "resets made")
}

// SafetyNetTest: anEventRecoveryAskedForWhileAResetIsUnderWayGoesOutOnceItIsDone
func TestNet_EventRecoveryAskedForWhileAResetIsUnderWayGoesOutOnceItIsDone(t *testing.T) {
	f := resetUnderWay(t)
	calls := f.calls()
	id := f.recoverEvent(live)
	expect(t, f.calls(), calls, "requests: its snapshot could go to either channel")
	expect(t, len(f.out.events), 0, "event requests while the reset is under way")
	f.resetDone()
	expect(t, len(f.out.events), 1, "event requests once it is done")
	ev := f.lastEventAsked()
	expect(t, ev.requestID, id, "the request id it was given when asked")
	f.acceptEvent(id)
	f.snapshotComplete(1, live, id)
	f.expectEnded(id, completed)
}

// resetUnderWay is session 1 behind, the API's acceptance of both recoveries, and the reset
// with the transport.
func resetUnderWay(t *testing.T) *netFeed {
	t.Helper()
	f := upNet(t, all)
	before := f.calls()
	f.stale(1, oldAge, 5*time.Minute, f.untilAsked(before))
	f.accept(f.lastSnapshot(pre))
	f.accept(f.lastSnapshot(live))
	expectSessions(t, f.resetSessions(), []int{1}, "resets")
	return f
}

// SafetyNetTest: anotherSessionsSnapshotCompleteDoesNotCancelALowPrioritySessionsReset
func TestNet_AnotherSessionsSnapshotCompleteDoesNotCancelALowPrioritySessionsReset(t *testing.T) {
	f := &netFeed{feed: newFeed(t)}
	f.open(1, hi)
	f.open(2, low)
	f.startFeed()
	f.bothUp(1, 2)
	before := f.calls()
	f.stale(2, oldAge, 5*time.Minute, f.untilAsked(before))
	preReq, liveReq := f.lastSnapshot(pre), f.lastSnapshot(live)
	// the high-priority session sees one before the API has answered the other
	f.snapshotComplete(1, pre, preReq.requestID)
	f.accept(liveReq)
	expectSessions(t, f.resetSessions(), []int{2}, "resets: no evidence session 2's backlog was replaced")
	f.resetDone()
	expect(t, f.calls(), before+4, "requests: both asked for again at once, session 2 has seen neither")
	if f.lastSnapshot(pre).requestID == preReq.requestID || f.lastSnapshot(live).requestID == liveReq.requestID {
		t.Fatal("a recovery was not asked for under a new request id")
	}
}

// SafetyNetTest: anotherSessionsSnapshotCompleteOfTheLastRequestLetsALowPrioritySessionsResetGoAhead
func TestNet_AnotherSessionsSnapshotCompleteOfTheLastRequestLetsALowPrioritySessionsResetGoAhead(t *testing.T) {
	f := &netFeed{feed: newFeed(t)}
	f.open(1, hi)
	f.open(2, low)
	f.startFeed()
	f.bothUp(1, 2)
	before := f.calls()
	f.stale(2, oldAge, 5*time.Minute, f.untilAsked(before))
	preReq, liveReq := f.lastSnapshot(pre), f.lastSnapshot(live)
	f.accept(preReq)
	// the last one's snapshot_complete, seen by the high-priority session before the API answers
	f.snapshotComplete(1, live, liveReq.requestID)
	expectSessions(t, f.resetSessions(), []int{2}, "resets: the acceptance it proves lets the reset go ahead")
	expectDown(t, f.feed, live, true, "the live producer while session 2 is reset")
	f.resetDone()
	expect(t, f.calls(), before+4, "requests once it is done: both asked for again")
	f.complete(f.lastSnapshot(live), 1, 2)
	expectDown(t, f.feed, live, false, "the live producer after its new recovery")
	// the other, given up with the reset, completes nothing when its old snapshot_complete comes
	f.snapshotComplete(1, pre, preReq.requestID)
	expect(t, f.calls(), before+4, "requests after the old one's completion")
}

// SafetyNetTest: deferredEventRecoveriesCountAgainstTheCap
func TestNet_DeferredEventRecoveriesCountAgainstTheCap(t *testing.T) {
	f := resetUnderWay(t)
	calls := f.calls()
	for i := 1; i <= maxPendingEventRecoveries; i++ {
		if _, err := f.askEvent(live, i, false); err != nil {
			t.Fatalf("event recovery %d refused: %v", i, err)
		}
	}
	_, err := f.askEvent(live, maxPendingEventRecoveries+1, false)
	if !errors.Is(err, ErrTooManyPendingEventRecoveries) {
		t.Fatalf("the 129th: err = %v, want ErrTooManyPendingEventRecoveries", err)
	}
	expect(t, f.m.counters.eventRefused, 1, "event recoveries refused")
	expect(t, f.calls(), calls, "requests: all of them wait for the reset")
	expect(t, len(f.out.events), 0, "event requests: all of them wait for the reset")
}

// SafetyNetTest: closingAnswersADeferredEventRecoveryNull
func TestNet_ClosingEndsADeferredEventRecoveryAsFailed(t *testing.T) {
	f := resetUnderWay(t)
	id := f.recoverEvent(live)
	f.m.close()
	out := f.expectEnded(id, failedEnd)
	if !errors.Is(out.err, errFeedClosing) {
		t.Fatalf("err = %v, want errFeedClosing", out.err)
	}
}

// SafetyNetTest: aDeferredEventRecoveryGoesOutWhenTheResettingSessionCloses
func TestNet_DeferredEventRecoveryGoesOutWhenTheResettingSessionCloses(t *testing.T) {
	f := resetUnderWay(t)
	f.recoverEvent(live)
	expect(t, len(f.out.events), 0, "event requests while the reset is under way")
	f.close(1)
	expect(t, len(f.out.events), 1, "event requests once the session is gone")
}

// SafetyNetTest: aDeferredEventRecoveryGoesOutWhenTheResetIsTurnedAway
func TestNet_DeferredEventRecoveryGoesOutWhenTheResetIsTurnedAway(t *testing.T) {
	f := resetUnderWay(t)
	f.recoverEvent(live)
	f.resetRefused()
	expect(t, len(f.out.events), 1, "event requests")
}

// SafetyNetTest: aDeferredEventRecoveryExpiresAsOneAskedForWould
func TestNet_DeferredEventRecoveryExpiresAsOneAskedForWould(t *testing.T) {
	f := resetUnderWay(t)
	id := f.recoverEvent(live)
	f.runWithAlives(6 * time.Hour)
	f.expectNotEnded(id)
	f.runWithAlives(time.Second)
	f.expectEnded(id, timedOut)
	expect(t, f.m.counters.eventExpired, 1, "event recoveries expired")
	f.runWithAlives(time.Second)
	expect(t, f.m.counters.eventExpired, 1, "event recoveries expired: counted once")
	f.resetDone()
	expect(t, len(f.out.events), 0, "event requests: nothing sent for it")
}

// SafetyNetTest: expiredDeferredEventRecoveriesFreeTheirPlaces
func TestNet_ExpiredDeferredEventRecoveriesFreeTheirPlaces(t *testing.T) {
	f := resetUnderWay(t)
	for i := 1; i <= maxPendingEventRecoveries; i++ {
		f.event(live, i)
	}
	f.runWithAlives(6*time.Hour + time.Second)
	expect(t, f.m.counters.eventExpired, maxPendingEventRecoveries, "event recoveries expired")
	f.event(live, 1000)
	expect(t, f.m.counters.eventRefused, 0, "event recoveries refused: a place again")
	f.resetDone()
	expect(t, len(f.out.events), 1, "event requests")
}

// SafetyNetTest: aSnapshotCompleteOfARecoveryTheResetDidNotAskForLeavesItWaiting
func TestNet_SnapshotCompleteOfARecoveryTheResetDidNotAskForLeavesItWaiting(t *testing.T) {
	f := upNet(t, all)
	before := f.calls()
	f.noLiveAlives = true
	f.stale(1, oldAge, 3*time.Minute, f.untilAsked(before))
	expect(t, f.calls(), before+1, "requests: the prematch producer only")
	// the silent producer speaks while the reset waits for the API, and is recovered on its own
	f.noLiveAlives = false
	f.alive(live)
	f.complete(f.lastSnapshot(live), 1)
	f.accept(f.lastSnapshot(pre))
	expectSessions(t, f.resetSessions(), []int{1}, "resets: a completion of a recovery it did not ask for")
}

// SafetyNetTest: aReportOfNoResetUnderWayChangesNothing
func TestNet_ReportOfNoResetUnderWayChangesNothing(t *testing.T) {
	f := upNet(t, all)
	before := f.calls()
	f.stale(1, oldAge, 5*time.Minute, f.untilAsked(before))
	preReq, liveReq := f.lastSnapshot(pre), f.lastSnapshot(live)
	// while it still waits for the API, a report with the number it will have
	f.accept(preReq)
	f.m.resetDone(sid(1), 1, true)
	f.m.resetDone(sid(1), 0, true)
	expect(t, f.calls(), before+2, "requests before the reset is made")
	f.accept(liveReq)
	number := f.out.resetNumbers[len(f.out.resetNumbers)-1]
	f.m.resetDone(sid(1), number+1, true)
	f.m.resetDone(sid(1), 0, true)
	expect(t, f.calls(), before+2, "requests after reports of other resets")
	f.m.resetDone(sid(1), number, true)
	expect(t, f.calls(), before+4, "requests after its own report")
}

// SafetyNetTest: aResetWithTheTransportWhenTheConnectionGoesStillCountsWhenItIsDone
func TestNet_ResetWithTheTransportWhenTheConnectionGoesStillCountsWhenItIsDone(t *testing.T) {
	f := resetUnderWay(t)
	before := f.calls() - 2
	f.m.connectionDown()
	f.m.connectionUp()
	f.alive(pre)
	f.alive(live)
	expect(t, f.calls(), before+2, "requests while the old reset may still drop the new queue")
	f.resetDone()
	expect(t, f.calls(), before+4, "requests once it is done")
	f.complete(f.lastSnapshot(pre), 1)
	f.complete(f.lastSnapshot(live), 1)
	f.expectCause(pre, causeProcessingDelay, "recovered, but its session still processes it late")
	expectDown(t, f.feed, live, false, "the live producer")
}

// SafetyNetTest: aRefusedRequestOfTheNetLeavesTheProducersRetriesWhole
func TestNet_RefusedRequestOfTheNetLeavesTheProducersRetriesWhole(t *testing.T) {
	f := upNet(t, all)
	before := f.calls()
	f.stale(1, oldAge, 5*time.Minute, f.untilAsked(before))
	f.refuse(f.lastSnapshot(pre))
	f.refuse(f.lastSnapshot(live))
	// a real recovery after that is refused once: asked for again after the first pause
	f.unsubscribed(pre)
	f.refuse(f.lastSnapshot(pre))
	asked := len(f.snapshots(pre))
	f.runWithAlives(5 * time.Second)
	expect(t, len(f.snapshots(pre)), asked+1, "recoveries after 5 s")
	expect(t, f.m.counters.reissued, 1, "re-issues: a first request, not a re-issue")
}

// SafetyNetTest: agesAreCorrectedByEachProducersOwnClockOffset
func TestNet_AgesAreCorrectedByEachProducersOwnClockOffset(t *testing.T) {
	f := newNet(t)
	f.preSkew = time.Hour
	f.liveSkew = -30 * time.Minute
	f.open(1, all)
	f.startFeed()
	f.aliveBoth()
	f.complete(f.lastSnapshot(pre), 1)
	f.complete(f.lastSnapshot(live), 1)
	before := f.calls()

	// an hour old by the SDK's clock, a second by the producer's
	expect(t, f.stale(1, freshAge, 3*time.Minute, f.untilAsked(before)), -1, "a request")
	expect(t, f.logCount("WARN The safety net"), 0, "warnings of the net")
	f.stale(1, oldAge, 3*time.Minute, f.untilAsked(before))
	expect(t, f.calls(), before+2, "requests: three minutes old by the producer's clock")
}

// SafetyNetTest: theNetIsOffForAProducerWhoseLastAliveIsOlderThanTwoIntervals
func TestNet_OffForAProducerWhoseLastAliveIsOlderThanTwoIntervals(t *testing.T) {
	f := &netFeed{feed: withSettings(t, func(s *recoverySettings) { s.maxInactivity = 5 * time.Minute })}
	f.open(1, all)
	f.startFeed()
	f.bothUp(1)
	before := f.calls()
	f.noAlives = true
	expect(t, f.stale(1, oldAge, 3*time.Minute, f.untilAsked(before)), -1, "a request without alives")
	f.noAlives = false
	expect(t, f.stale(1, oldAge, 3*time.Minute, f.untilAsked(before)), 70,
		"seconds to a request once alives resume: the first at 10 s, then the window")
}

// SafetyNetTest: snapshotMessagesAreNoSamples
func TestNet_SnapshotMessagesAreNoSamples(t *testing.T) {
	f := upNet(t, all)
	before := f.calls()
	checkpoint, _ := f.checkpoint(1, pre)
	f.requestID = f.lastSnapshot(pre).requestID
	expect(t, f.stale(1, oldAge, 3*time.Minute, f.untilAsked(before)), -1, "a request")
	got, _ := f.checkpoint(1, pre)
	expectTime(t, got, checkpoint, "the checkpoint")
}

// recoveringWithTwoSessions is sessions 1 and 2, both producers up, and a recovery of
// the prematch producer in flight, accepted.
func recoveringWithTwoSessions(t *testing.T) (*netFeed, snapshotRequest) {
	t.Helper()
	f := &netFeed{feed: newFeed(t)}
	f.open(1, all)
	f.open(2, all)
	f.startFeed()
	f.bothUp(1, 2)
	f.unsubscribed(pre)
	recovery := f.lastSnapshot(pre)
	f.accept(recovery)
	return f, recovery
}

// SafetyNetTest: theNetIsPausedOnEverySessionWhileARecoveryOfTheProducerIsInFlight
func TestNet_PausedOnEverySessionWhileARecoveryOfTheProducerIsInFlight(t *testing.T) {
	f, recovery := recoveringWithTwoSessions(t)
	before := f.calls()
	// the recovery's snapshot is ahead of session 2's live messages, which come out old
	expect(t, f.stale(2, oldAge, 3*time.Minute, f.untilAsked(before)), -1, "a request")

	f.complete(recovery, 1, 2)
	expect(t, f.stale(2, oldAge, 3*time.Minute, f.untilAsked(before)), 61,
		"seconds from the completion to the net's request: the window starts again")
}

// SafetyNetTest: aSampleTakenBeforeTheCompletionButHandledAfterItIsStillTheRecoverys
func TestNet_SampleTakenBeforeTheCompletionButHandledAfterItIsStillTheRecoverys(t *testing.T) {
	f, recovery := recoveringWithTwoSessions(t)
	before := f.calls()
	f.complete(recovery, 1, 2)
	completedAt := f.now()
	taken := NewStamp(completedAt.Add(-time.Second), f.clock.Now().Mono()-time.Second)
	f.m.processed(sid(2), pre, completedAt.Add(-oldAge-time.Second), completedAt.Add(-oldAge-time.Second), taken, 0)
	expect(t, f.stale(2, oldAge, 3*time.Minute, f.untilAsked(before)), 61, "seconds from the completion to the net's request")
}

// SafetyNetTest: aRequestTheApiDidNotAcceptMeansNoResetAndTheNetBacksOff
func TestNet_RequestTheApiDidNotAcceptMeansNoResetAndTheNetBacksOff(t *testing.T) {
	f := upNet(t, all)
	before := f.calls()
	f.stale(1, oldAge, 5*time.Minute, f.untilAsked(before))
	preReq, liveReq := f.lastSnapshot(pre), f.lastSnapshot(live)
	f.accept(liveReq)
	f.refuse(preReq)
	expect(t, len(f.out.resets), 0, "resets")
	expect(t, f.logCount("WARN The safety net did not reset session"), 1, "warnings of the reset not made")
	expect(t, f.m.counters.resetRequestsFailed, 1, "net requests failed")
	for _, c := range f.out.statuses {
		if c.cause == causeSafetyNetReset {
			t.Fatal("the net took a producer down")
		}
	}
	f.runWithAlives(10 * time.Second)
	expect(t, len(f.snapshots(pre)), 2, "recoveries of the prematch producer: nothing missing, so nothing to ask for again")

	f.complete(liveReq, 1)
	after := f.calls()
	expect(t, f.stale(1, oldAge, 5*time.Minute, f.untilAsked(after)), 61,
		"seconds to the next try: the window again, past the minute's backoff")
	expect(t, f.calls(), after+2, "requests: a recovery for each producer")
}

// SafetyNetTest: aProducerDisabledBetweenTheNetsSelectionAndItsRequestCancelsTheResetAndTheNetActsAgain
func TestNet_ProducerDisabledBetweenTheNetsSelectionAndItsRequestCancelsTheResetAndTheNetActsAgain(t *testing.T) {
	f := upNet(t, all)
	// the client disables live while the net's request for prematch goes out: live's turn comes after
	f.out.onRequest = func(call snapshotRequest) {
		if call.producerID == pre {
			f.cat.disabled[live] = true
		}
	}
	before := f.calls()
	f.staleBeforeTheTick(1, oldAge, 5*time.Minute, f.untilAsked(before))
	f.out.onRequest = nil

	// at once, by the message that made the request: no tick has run since
	expect(t, f.logCount("WARN The safety net did not reset session"), 1, "warnings of the reset not made")
	expect(t, f.m.counters.resetRequestsFailed, 1, "net requests failed")
	f.m.tick()
	expect(t, f.logCount("WARN The safety net did not reset session"), 1, "warnings after the tick")
	expect(t, f.m.counters.resetRequestsFailed, 1, "net requests failed after the tick")
	expect(t, len(f.snapshots(live)), 1, "recoveries of the live producer: it was never asked")
	expect(t, len(f.out.resets), 0, "resets")

	// the recovery that did go out ends, which pauses the net until then
	f.complete(f.lastSnapshot(pre), 1)
	after := f.calls()
	if f.stale(1, oldAge, 5*time.Minute, f.untilAsked(after)) <= 0 {
		t.Fatal("the net did not act again: it is stuck on the disabled producer")
	}
	expect(t, f.calls(), after+1, "requests: for the one that is enabled")
	f.accept(f.lastSnapshot(pre))
	expectSessions(t, f.resetSessions(), []int{1}, "resets")
}

// SafetyNetTest: aProducerDisabledWhileTheResetWaitsForItsAcceptanceCancelsTheResetOnTheNextTick
func TestNet_ProducerDisabledWhileTheResetWaitsForItsAcceptanceCancelsTheResetOnTheNextTick(t *testing.T) {
	f := upNet(t, all)
	before := f.calls()
	f.stale(1, oldAge, 5*time.Minute, f.untilAsked(before))
	preReq, liveReq := f.lastSnapshot(pre), f.lastSnapshot(live)
	f.accept(preReq)

	f.cat.disabled[live] = true
	f.advance(time.Second)
	expect(t, f.logCount("WARN The safety net did not reset session"), 1, "warnings of the reset not made")
	expect(t, f.m.counters.resetRequestsFailed, 1, "net requests failed")
	f.accept(liveReq)
	expect(t, len(f.out.resets), 0, "resets: the late answer finds no reset to make")

	// enabled again, the net works on the session as before
	delete(f.cat.disabled, live)
	f.complete(preReq, 1)
	f.complete(liveReq, 1)
	after := f.calls()
	f.stale(1, oldAge, 5*time.Minute, f.untilAsked(after))
	expect(t, f.calls(), after+2, "requests: a recovery for each producer again")
	f.accept(f.lastSnapshot(pre))
	f.accept(f.lastSnapshot(live))
	expectSessions(t, f.resetSessions(), []int{1}, "resets")
}

// SafetyNetTest: aResetWaitingOnlyForEnabledProducersIsLeftAloneByTheTicks
func TestNet_ResetWaitingOnlyForEnabledProducersIsLeftAloneByTheTicks(t *testing.T) {
	f := upNet(t, all)
	before := f.calls()
	f.stale(1, oldAge, 5*time.Minute, f.untilAsked(before))
	preReq, liveReq := f.lastSnapshot(pre), f.lastSnapshot(live)
	f.accept(preReq)

	f.advance(time.Second)
	f.advance(time.Second)
	expect(t, f.logCount("WARN The safety net did not reset session"), 0, "warnings of the reset not made")
	expect(t, f.m.counters.resetRequestsFailed, 0, "net requests failed")
	f.accept(liveReq)
	expectSessions(t, f.resetSessions(), []int{1}, "resets")
}

// resetUntilTheSessionLags is the net's resets, each one taken and finished, until
// session 1 lags: the seconds of each from start.
func resetUntilTheSessionLags(f *netFeed, start time.Time) []int {
	var resetAt []int
	for f.now().Sub(start) < 10*time.Minute {
		before := f.calls()
		took := f.stale(1, oldAge, 10*time.Minute, func() bool { return f.calls() > before || f.lagging(1) })
		if took > 0 && f.lagging(1) {
			break
		}
		f.accept(f.lastSnapshot(pre))
		f.accept(f.lastSnapshot(live))
		resetAt = append(resetAt, int(f.now().Sub(start)/time.Second))
		f.resetDone()
		f.complete(f.lastSnapshot(pre), 1)
		f.complete(f.lastSnapshot(live), 1)
	}
	return resetAt
}

// SafetyNetTest: theNetResetsASessionThreeTimesPerCooldownWithBackoffThenTheSessionLags
func TestNet_ResetsASessionThreeTimesPerCooldownWithBackoffThenTheSessionLags(t *testing.T) {
	f := upNet(t, all)
	start := f.now()
	resetAt := resetUntilTheSessionLags(f, start)
	if !slices.Equal(resetAt, []int{61, 122, 242}) {
		t.Fatalf("seconds of the resets: got %v, want the window, then 1 and 2 minutes apart: [61 122 242]", resetAt)
	}
	expect(t, f.lagging(1), true, "lagging")
	expect(t, f.m.counters.lagging, 1, "times it began to lag")
	expect(t, f.logCount("it lags"), 1, "warnings that it lags")
	down := 0
	for _, c := range f.out.statuses {
		if c.cause == causeSafetyNetReset {
			down++
		}
	}
	expect(t, down, 6, "producers the net took down, once per reset each")

	// caught up
	f.stale(1, freshAge, time.Second, func() bool { return false })
	expect(t, f.lagging(1), false, "lagging once caught up")
	expect(t, f.logCount("INFO Session "+sid(1).String()+" caught up"), 1, "lines saying it caught up")

	// the first reset ages out of the cool-down: the net may reset again
	before := f.calls()
	f.stale(1, oldAge, 10*time.Minute, f.untilAsked(before))
	expect(t, int(f.now().Sub(start)/time.Second), 661, "seconds to the next try")
}

// SafetyNetTest: aLaggingSessionKeepsTheProducerDownForTheProcessingDelayUntilItIsOnTimeAgain
func TestNet_LaggingSessionKeepsTheProducerDownForTheProcessingDelayUntilItIsOnTimeAgain(t *testing.T) {
	f := upNet(t, all)
	resetUntilTheSessionLags(f, f.now())
	expect(t, f.lagging(1), true, "lagging")

	// the net takes no producer down, the processing delay does: the session takes it three minutes late
	late := f.lastStatus(pre)
	expect(t, late.down, true, "down")
	expect(t, late.cause, causeProcessingDelay, "cause")
	changes := len(f.statusesOf(pre))
	f.stale(1, oldAge, 2*time.Minute, func() bool { return false })
	expect(t, f.lagging(1), true, "lagging still")
	expect(t, len(f.statusesOf(pre)), changes, "status changes: down all the while it lags")

	// on time again: the lag clears and the producer is back
	f.stale(1, freshAge, time.Second, func() bool { return false })
	expect(t, f.lagging(1), false, "lagging once on time")
	back := f.lastStatus(pre)
	expect(t, back.down, false, "down")
	expect(t, back.cause, causeDelayStabilized, "cause")
	expect(t, len(f.statusesOf(pre)), changes+1, "status changes")
}

// SafetyNetTest: aSnapshotCompleteBeforeTheApisAnswerCancelsTheReset
func TestNet_SnapshotCompleteBeforeTheApisAnswerCancelsTheReset(t *testing.T) {
	f := upNet(t, all)
	before := f.calls()
	f.stale(1, oldAge, 5*time.Minute, f.untilAsked(before))
	preReq, liveReq := f.lastSnapshot(pre), f.lastSnapshot(live)
	f.snapshotComplete(1, pre, preReq.requestID)
	f.accept(preReq)
	f.accept(liveReq)
	expect(t, len(f.out.resets), 0, "resets: a reset now would drop what the recovery sent")

	// the reset given up, the net may act again once the other recovery is done
	f.snapshotComplete(1, live, liveReq.requestID)
	after := f.calls()
	expect(t, f.stale(1, oldAge, 5*time.Minute, f.untilAsked(after)), 61,
		"seconds to the net's next request: a window of stale messages counted again from the recovery's end")
}

// SafetyNetTest: aSnapshotCompleteBeforeTheApisAnswerLetsTheNetActAgainOnASessionOfOneProducer
func TestNet_SnapshotCompleteBeforeTheApisAnswerLetsTheNetActAgainOnASessionOfOneProducer(t *testing.T) {
	f := &netFeed{feed: newFeed(t)}
	f.open(1, prematch)
	f.startFeed()
	f.alive(pre)
	f.complete(f.lastSnapshot(pre), 1)
	before := f.calls()
	f.stale(1, oldAge, 5*time.Minute, f.untilAsked(before))
	preReq := f.lastSnapshot(pre)
	f.snapshotComplete(1, pre, preReq.requestID)
	f.accept(preReq)
	expect(t, len(f.out.resets), 0, "resets")
	after := f.calls()
	expect(t, f.stale(1, oldAge, 5*time.Minute, f.untilAsked(after)), 61,
		"seconds to the net's next request, the reset it waited for given up: the window counted again from the recovery's end")
}

// SafetyNetTest: aResetTheTransportCouldNotTakeAfterOneItMadeWaitsNoLongerThanTheOneItMade
func TestNet_ResetTheTransportCouldNotTakeAfterOneItMadeWaitsNoLongerThanTheOneItMade(t *testing.T) {
	f := upNet(t, all)
	before := f.calls()
	f.stale(1, oldAge, 5*time.Minute, f.untilAsked(before))
	f.accept(f.lastSnapshot(pre))
	f.accept(f.lastSnapshot(live))
	f.resetDone()
	f.complete(f.lastSnapshot(pre), 1)
	f.complete(f.lastSnapshot(live), 1)

	second := f.calls()
	expect(t, f.stale(1, oldAge, 5*time.Minute, f.untilAsked(second)), 61, "seconds to the second request")
	f.accept(f.lastSnapshot(pre))
	f.accept(f.lastSnapshot(live))
	f.resetRefused()
	f.complete(f.lastSnapshot(pre), 1)
	f.complete(f.lastSnapshot(live), 1)

	third := f.calls()
	expect(t, f.stale(1, oldAge, 5*time.Minute, f.untilAsked(third)), 61,
		"seconds to the next try: the window, past the minute the reset made set, not two")
	expect(t, f.m.counters.resets, 1, "resets made")
}

// SafetyNetTest: aRecoveryWhoseSessionTheTransportStillResetsDoesNotTimeOutAndIsAskedForAgainOnceItIsDone
func TestNet_RecoveryWhoseSessionTheTransportStillResetsDoesNotTimeOutAndIsAskedForAgainOnceItIsDone(t *testing.T) {
	f := resetUnderWay(t)
	before := f.calls() - 2

	// the new channel takes the transport longer than the deadline for a snapshot_complete
	f.runWithAlives(6 * time.Minute)
	expect(t, f.m.counters.timedOut, 0, "recoveries timed out")
	expect(t, f.calls(), before+2, "requests while the reset is under way")
	f.resetDone()
	expect(t, f.calls(), before+4, "requests once it is done")
	expect(t, f.m.counters.failed, 0, "recoveries failed")
}

// SafetyNetTest: aLostConnectionWhileAResetWaitsMakesNone
func TestNet_LostConnectionWhileAResetWaitsMakesNone(t *testing.T) {
	f := upNet(t, all)
	before := f.calls()
	f.stale(1, oldAge, 5*time.Minute, f.untilAsked(before))
	f.m.connectionDown()
	f.accept(f.lastSnapshot(pre))
	f.accept(f.lastSnapshot(live))
	expect(t, len(f.out.resets), 0, "resets")
}

// ---- RecoveryClockTest: the safety net

// monoStale is, second by second on the monotonic clock alone, session taking a
// live message of the prematch producer that is old by its own clock, with both
// producers' alives every ten seconds, until until holds. It is the seconds that
// took, or -1 when it did not hold within limit.
func (f *feed) monoStale(session, limit int, until func() bool) int {
	for second := 1; second <= limit; second++ {
		f.advanceMono(time.Second)
		f.monoCount++
		if f.monoCount%10 == 0 {
			f.alive(pre)
			f.alive(live)
		}
		at := f.now().Add(-oldAge)
		f.m.processed(sid(session), pre, at, at, f.stamp(), 0)
		f.m.tick()
		if until() {
			return second
		}
	}
	return -1
}

// resetAndRecover is the net's reset made, and what it asked for recovered by
// session 1.
func (f *feed) resetAndRecover() {
	f.accept(f.lastSnapshot(pre))
	f.accept(f.lastSnapshot(live))
	f.resetDone()
	f.complete(f.lastSnapshot(pre), 1)
	f.complete(f.lastSnapshot(live), 1)
}

// RecoveryClockTest: theWindowOfStaleMessagesCountsOnTheMonotonicClock
func TestNetClock_WindowOfStaleMessagesCountsOnTheMonotonicClock(t *testing.T) {
	for _, step := range []time.Duration{90 * time.Second, -90 * time.Second} {
		f := upWith(t, all)
		before := len(f.out.calls)
		asked := func() bool { return len(f.out.calls) > before }
		expect(t, f.monoStale(1, 10, asked), -1, "a request in the first ten seconds")

		f.stepWall(step)
		// the producer's clock stepped with the SDK's, as its next alive says
		f.alive(pre)
		expect(t, len(f.out.calls), before, "requests after the step")
		expect(t, f.monoStale(1, 100, asked), 51, "seconds on to the net's request: 61 from the first stale message")
		expect(t, len(f.out.resets), 0, "resets")
	}
}

// RecoveryClockTest: theNetsBackoffAndCapCountOnTheMonotonicClock
func TestNetClock_BackoffAndCapCountOnTheMonotonicClock(t *testing.T) {
	for _, direction := range []int{1, -1} {
		f := upWith(t, all)
		start := f.clock.Now().Mono()
		for round := 1; round <= 2; round++ {
			before := len(f.out.calls)
			expect(t, f.monoStale(1, 300, func() bool { return len(f.out.calls) > before }), 61, "seconds to the net's request in a round")
			f.resetAndRecover()
		}

		// the third reset waits for the backoff of two minutes, not the window of 61 s
		f.stepWall(time.Duration(300*direction) * time.Second)
		f.alive(pre)
		before := len(f.out.calls)
		expect(t, f.monoStale(1, 300, func() bool { return len(f.out.calls) > before }), 120, "seconds to the net's request in round 3")
		f.resetAndRecover()

		// three resets spend the cap: the session lags, whatever the wall clock says of the cool-down
		f.stepWall(time.Duration(20*direction) * time.Minute)
		f.alive(pre)
		spent := len(f.out.calls)
		expect(t, f.monoStale(1, 300, func() bool { return f.lagging(1) }), 61, "seconds to lagging")
		expect(t, len(f.out.calls), spent, "requests with the cap spent")
		if f.monoStale(1, 1000, func() bool { return len(f.out.calls) > spent }) <= 0 {
			t.Fatal("the net never acted again")
		}
		expect(t, int((f.clock.Now().Mono()-start)/time.Second), 661, "seconds to the next try: the first reset ages out of the cool-down")
	}
}

// RecoveryClockTest: aSampleTakenBeforeTheCompletionOnTheMonotonicClockIsTheRecoverysWhateverItsWallReadingSays
func TestNetClock_SampleTakenBeforeTheCompletionOnTheMonotonicClockIsTheRecoverysWhateverItsWallReadingSays(t *testing.T) {
	f, recovery := recoveringWithTwoSessions(t)
	before := len(f.out.calls)
	f.complete(recovery, 1, 2)
	wall, mono := f.now(), f.clock.Now().Mono()
	// taken a second before the completion, though the wall clock, set back since, has it after
	at := wall.Add(-oldAge - time.Second)
	f.m.processed(sid(2), pre, at, at, NewStamp(wall.Add(5*time.Second), mono-time.Second), 0)
	expect(t, f.monoStale(2, 300, func() bool { return len(f.out.calls) > before }), 61,
		"seconds from the completion to the net's request: the window starts again")
}

// ---- what Go's own rules add

// A feed that is closing resets nothing: the reset would drop what the shutdown is
// about to settle, and the recoveries it asks for are not waited for.
func TestNet_NothingIsAskedOnceTheFeedIsClosing(t *testing.T) {
	f := upNet(t, all)
	f.m.beginClosing()
	before := f.calls()
	expect(t, f.stale(1, oldAge, 5*time.Minute, f.untilAsked(before)), -1, "a request")
	expect(t, len(f.out.resets), 0, "resets")
}

// Settings that carry no stale limit switch the net off, the samples' ages still
// read for the delay of a session.
func TestNet_NoStaleLimitSwitchesTheNetOff(t *testing.T) {
	f := &netFeed{feed: withSettings(t, func(s *recoverySettings) { s.staleLimit, s.staleWindow = 0, 0 })}
	f.open(1, all)
	f.startFeed()
	f.bothUp(1)
	before := f.calls()
	expect(t, f.stale(1, oldAge, 5*time.Minute, f.untilAsked(before)), -1, "a request")
	f.expectCause(pre, causeProcessingDelay, "the producer's cause: the ages are still read")
}

// A session that closes while its reset waits for the API is not reset.
func TestNet_SessionThatClosesWhileItsResetWaitsIsNotReset(t *testing.T) {
	f := &netFeed{feed: newFeed(t)}
	f.open(1, all)
	f.open(2, all)
	f.startFeed()
	f.bothUp(1, 2)
	before := f.calls()
	f.stale(1, oldAge, 5*time.Minute, f.untilAsked(before))
	preReq, liveReq := f.lastSnapshot(pre), f.lastSnapshot(live)
	f.accept(preReq)
	f.close(1)
	f.accept(liveReq)
	expect(t, len(f.out.resets), 0, "resets")
}

// An event recovery whose awaited session has its channel replaced is given up as
// for a lost queue, whether or not the transport replaced the channel: what it sent
// into the queue went with it, or the snapshot_complete was ignored meanwhile.
func TestNet_EventRecoveryAwaitingAResetSessionIsGivenUpOnceTheResetIsDone(t *testing.T) {
	for _, replaced := range []bool{true, false} {
		f := upNet(t, all)
		id := f.recoverEvent(live)
		f.acceptEvent(id)
		before := f.calls()
		f.stale(1, oldAge, 5*time.Minute, f.untilAsked(before))
		f.accept(f.lastSnapshot(pre))
		f.accept(f.lastSnapshot(live))
		expectSessions(t, f.resetSessions(), []int{1}, "resets")

		// its snapshot_complete comes from the queue being replaced
		f.snapshotComplete(1, live, id)
		f.expectNotEnded(id)
		f.reportReset(replaced)
		out := f.expectEnded(id, failedEnd)
		if !errors.Is(out.err, errLostQueue) {
			t.Fatalf("replaced=%v: err = %v, want errLostQueue", replaced, out.err)
		}
		expect(t, f.m.counters.eventAbandoned, 1, "event recoveries given up")
	}
}

// A gap of the net's that its reset has not made yet does not keep a producer
// down: the producer is up once nothing else is missing.
func TestNet_GapOfAResetNotMadeYetDoesNotKeepAProducerDown(t *testing.T) {
	f := upNet(t, all)
	f.m.markDown(f.m.byID[pre], causeConnectionLost)
	expectDown(t, f.feed, pre, true, "the producer")
	l := f.m.session(sid(1)).lane(pre)
	l.gap = f.m.openGap(nil, l.checkpoint, true)
	f.m.upIfNothingMissing(f.m.byID[pre])
	expectDown(t, f.feed, pre, false, "the producer with only the net's gap open")

	f.m.markDown(f.m.byID[pre], causeConnectionLost)
	l.gap.pendingReset = false
	f.m.upIfNothingMissing(f.m.byID[pre])
	expectDown(t, f.feed, pre, true, "the producer with a gap of its session's queue open")
}

// A recovery the net's reset waits for the API to accept, given up because its
// producer went silent, cancels the reset: the recovery is asked for again on the
// producer's next alive, and its acceptance then is no reason to reset a session
// that has stopped being behind.
func TestNet_RecoveryGivenUpWhileItsResetWaitsCancelsTheReset(t *testing.T) {
	f := upNet(t, all)
	before := f.calls()
	f.stale(1, oldAge, 5*time.Minute, f.untilAsked(before))
	for range 25 {
		f.advance(time.Second)
	}
	expect(t, f.m.counters.abandoned, 2, "recoveries given up: both producers fell silent")
	f.alive(pre)
	f.alive(live)
	f.accept(f.lastSnapshot(pre))
	f.accept(f.lastSnapshot(live))
	expect(t, len(f.out.resets), 0, "resets")
}

// A recovery in flight pauses the net even when the gap it was asked for is gone:
// the producer's snapshot is still ahead of the live messages. The window starts
// again once it completes.
func TestNet_PausedWhileARecoveryIsInFlightWhoseGapWentWithItsSession(t *testing.T) {
	f := &netFeed{feed: newFeed(t)}
	f.open(1, all)
	f.open(2, all)
	f.startFeed()
	f.bothUp(1, 2)
	f.m.channelLost(sid(2))
	f.m.channelReopened(sid(2))
	preReq, liveReq := f.lastSnapshot(pre), f.lastSnapshot(live)
	f.accept(preReq)
	f.accept(liveReq)
	f.close(2)
	expect(t, f.m.hasGaps(f.m.byID[pre]), false, "gaps of the prematch producer")
	before := f.calls()
	expect(t, f.stale(1, oldAge, 3*time.Minute, f.untilAsked(before)), -1, "a request while the recovery is in flight")

	f.complete(preReq, 1)
	f.complete(liveReq, 1)
	expect(t, f.stale(1, oldAge, 3*time.Minute, f.untilAsked(before)), 61, "seconds from the completion to the net's request")
}

// An open gap pauses the net even when no recovery is in flight for it, as while
// another session awaits its channel: its snapshot will be ahead of the live
// messages, so they say nothing about the session. The window starts again once
// the producer's recovery completes.
func TestNet_PausedWhileAGapIsOpenWithNoRecoveryInFlight(t *testing.T) {
	f := &netFeed{feed: newFeed(t)}
	f.open(1, all)
	f.open(2, all)
	f.startFeed()
	f.bothUp(1, 2)
	f.m.channelLost(sid(2))
	before := f.calls()
	expect(t, f.stale(1, oldAge, 3*time.Minute, f.untilAsked(before)), -1, "a request while session 2 awaits its channel")

	f.m.channelReopened(sid(2))
	f.complete(f.lastSnapshot(pre), 1, 2)
	f.complete(f.lastSnapshot(live), 1, 2)
	after := f.calls()
	expect(t, f.stale(1, oldAge, 3*time.Minute, f.untilAsked(after)), 61, "seconds from the completion to the net's request")
}

// ---- what a reset must not drop

// upNetWithWindow is upNet with the producers' stateful recovery window set.
func upNetWithWindow(t *testing.T, minutes int) *netFeed {
	t.Helper()
	f := newFeedWith(t, defaultSettings(), minutes)
	f.open(1, all)
	f.startFeed()
	f.bothUp(1)
	return &netFeed{feed: f}
}

// runFor moves the clock, with no message of the session, by minutes of alives of
// both producers every ten seconds: the session's checkpoint stays where it was.
func (n *netFeed) runFor(d time.Duration) {
	for range int(d / (10 * time.Second)) {
		n.clock.advance(10 * time.Second)
		n.aliveBoth()
		n.m.tick()
	}
}

const notResetOutsideWindow = "the safety net does not reset it"

// A session whose backlog reaches back past the producer's recovery window is not
// reset: no recovery repeats what a reset would drop. It lags, with one warning for
// the episode, and nothing is asked.
func TestNet_BacklogBeyondTheRecoveryWindowIsNotResetButLags(t *testing.T) {
	f := upNetWithWindow(t, 60)
	f.runFor(70 * time.Minute)
	before := f.calls()
	expect(t, f.stale(1, 70*time.Minute, 5*time.Minute, f.untilAsked(before)), -1, "a request")
	expect(t, f.calls(), before, "requests")
	expect(t, len(f.out.resets), 0, "resets")
	expect(t, f.lagging(1), true, "the session lags")
	expect(t, f.m.counters.lagging, 1, "episodes of lagging")
	expect(t, f.logCount("WARN Session"), 1, "warnings of the episode")
	expect(t, f.logCount(notResetOutsideWindow), 1, "warnings of the net not resetting it")
	expect(t, f.m.session(sid(1)).pending == nil, true, "a reset pending")
	for _, l := range f.m.session(sid(1)).lanes {
		expect(t, l.gap == nil, true, "a gap of the lane")
	}
	// it stops lagging the usual way
	f.stale(1, freshAge, 2*time.Second, func() bool { return false })
	expect(t, f.lagging(1), false, "the session lags once its messages are fresh")
}

func TestNet_BacklogInsideTheRecoveryWindowIsResetAsBefore(t *testing.T) {
	f := upNetWithWindow(t, 60)
	f.runFor(50 * time.Minute)
	before := f.calls()
	f.stale(1, 50*time.Minute, 5*time.Minute, f.untilAsked(before))
	f.accept(f.lastSnapshot(pre))
	f.accept(f.lastSnapshot(live))
	expectSessions(t, f.resetSessions(), []int{1}, "resets")
	expect(t, f.lagging(1), false, "the session lags")
	expect(t, f.logCount(notResetOutsideWindow), 0, "warnings of a reset not made")
}

// A checkpoint that falls out of the window between the net's selection and the
// last acceptance is not reset either.
func TestNet_CheckpointThatLeavesTheWindowBeforeTheLastAcceptanceCancelsTheReset(t *testing.T) {
	f := upNetWithWindow(t, 60)
	f.runFor(55 * time.Minute)
	before := f.calls()
	f.stale(1, 55*time.Minute, 5*time.Minute, f.untilAsked(before))
	preReq, liveReq := f.lastSnapshot(pre), f.lastSnapshot(live)
	f.accept(preReq)
	f.clock.advance(10 * time.Minute)
	f.accept(liveReq)
	expect(t, len(f.out.resets), 0, "resets")
	expect(t, f.m.session(sid(1)).pending == nil, true, "a reset pending")
	expect(t, f.m.counters.resetRequestsFailed, 1, "net requests failed")
	expect(t, f.logCount("WARN The safety net did not reset session"), 1, "warnings of the reset not made")
	for _, c := range f.out.statuses {
		if c.cause == causeSafetyNetReset {
			t.Fatal("the net took a producer down")
		}
	}
	for _, l := range f.m.session(sid(1)).lanes {
		expect(t, l.gap == nil, true, "a gap of the net's")
	}
}

// ---- the end of a reset that waits

// The acceptance of the last recovery a reset waits for, handled before any tick,
// finds the producer disabled meanwhile: no reset.
func TestNet_AcceptanceBeforeTheTickOfAProducerDisabledMeanwhileMakesNoReset(t *testing.T) {
	f := upNet(t, all)
	before := f.calls()
	f.stale(1, oldAge, 5*time.Minute, f.untilAsked(before))
	preReq, liveReq := f.lastSnapshot(pre), f.lastSnapshot(live)
	f.accept(preReq)
	f.cat.disabled[live] = true
	f.accept(liveReq) // no tick between
	expect(t, len(f.out.resets), 0, "resets")
	expect(t, f.m.session(sid(1)).pending == nil, true, "a reset pending")
	expect(t, f.logCount("WARN The safety net did not reset session"), 1, "warnings of the reset not made")
	expect(t, f.m.counters.resetRequestsFailed, 1, "net requests failed")
	f.m.tick()
	expect(t, f.logCount("WARN The safety net did not reset session"), 1, "warnings after the tick")
	for _, c := range f.out.statuses {
		if c.cause == causeSafetyNetReset {
			t.Fatal("the net took a producer down")
		}
	}
}

// The acceptance of a producer that is disabled while another is not yet accepted
// cancels the reset at once too, and the tick has nothing left to do.
func TestNet_AcceptanceOfADisabledProducerWhileAnotherIsAwaitedCancelsTheReset(t *testing.T) {
	f := upNet(t, all)
	before := f.calls()
	f.stale(1, oldAge, 5*time.Minute, f.untilAsked(before))
	preReq, liveReq := f.lastSnapshot(pre), f.lastSnapshot(live)
	f.cat.disabled[pre] = true
	f.accept(preReq)
	expect(t, f.logCount("WARN The safety net did not reset session"), 1, "warnings of the reset not made")
	f.accept(liveReq)
	expect(t, len(f.out.resets), 0, "resets")
	f.m.tick()
	expect(t, f.logCount("WARN The safety net did not reset session"), 1, "warnings after the tick")
}

// An acceptance queued at shutdown, handled once the feed is closing, makes no
// reset; the answer is still the recovery's.
func TestNet_AcceptanceHandledOnceTheFeedIsClosingMakesNoReset(t *testing.T) {
	f := upNet(t, all)
	before := f.calls()
	f.stale(1, oldAge, 5*time.Minute, f.untilAsked(before))
	preReq, liveReq := f.lastSnapshot(pre), f.lastSnapshot(live)
	f.accept(preReq)
	f.m.beginClosing()
	f.accept(liveReq)
	expect(t, len(f.out.resets), 0, "resets")
	expect(t, f.m.session(sid(1)).pending == nil, true, "a reset pending")
	expect(t, f.m.session(sid(1)).underway, uint64(0), "a reset under way")
	for _, c := range f.out.statuses {
		if c.cause == causeSafetyNetReset {
			t.Fatal("the net took a producer down for a reset that is not made")
		}
	}
	a := f.m.byID[live].active
	expect(t, a != nil && a.accepted, true, "the answer counted as the recovery's")
	for _, l := range f.m.session(sid(1)).lanes {
		expect(t, l.gap == nil || !l.gap.pendingReset, true, "a gap of the net's left open")
	}
}

// A channel lost while a reset waits for the API cancels it: its queue is gone
// already, and a late acceptance has nothing to reset.
func TestNet_ChannelLostWhileTheResetWaitsCancelsIt(t *testing.T) {
	f := upNet(t, all)
	before := f.calls()
	f.stale(1, oldAge, 5*time.Minute, f.untilAsked(before))
	preReq, liveReq := f.lastSnapshot(pre), f.lastSnapshot(live)
	f.accept(preReq)
	f.m.channelLost(sid(1))
	f.m.channelReopened(sid(1))
	expect(t, f.m.session(sid(1)).pending == nil, true, "a reset pending after the loss")
	// the loss is no cancellation by the net: the recoveries given up for it find no
	// reset to cancel, so the net's backoff does not start
	expect(t, f.m.session(sid(1)).nextResetAt, time.Duration(0), "the net's backoff after the loss")
	f.accept(liveReq)
	f.accept(f.lastSnapshot(pre))
	f.accept(f.lastSnapshot(live))
	expect(t, len(f.out.resets), 0, "resets")
	expect(t, f.m.session(sid(1)).pending == nil, true, "a reset pending")
}

// ---- the window that counts only stale time

// A window of stale messages that began before a recovery of the producer does not
// carry over it, even when the session took no message of the producer while it ran.
func TestNet_StaleWindowDoesNotCountTheTimeOfARecoveryThatTookNoSampleOfTheLane(t *testing.T) {
	f := &netFeed{feed: newFeed(t)}
	f.open(1, all)
	f.open(2, all)
	f.startFeed()
	f.bothUp(1, 2)
	before := f.calls()
	expect(t, f.stale(1, oldAge, 50*time.Second, f.untilAsked(before)), -1, "a request inside the window")

	// session 2 loses its channel: a recovery of each producer runs, and session 1
	// takes nothing of them
	f.m.channelLost(sid(2))
	f.m.channelReopened(sid(2))
	for range 3 {
		f.clock.advance(10 * time.Second)
		f.aliveBoth()
		f.m.tick()
	}
	f.complete(f.lastSnapshot(pre), 1, 2)
	f.complete(f.lastSnapshot(live), 1, 2)
	after := f.calls()
	expect(t, f.stale(1, oldAge, 3*time.Minute, f.untilAsked(after)), 61, "seconds from the completion to the net's request")
}
