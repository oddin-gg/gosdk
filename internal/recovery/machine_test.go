package recovery

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/types"
)

// The recovery state machine, rule by rule, on a clock the test moves: when a
// recovery is asked for and from where, what completes it, what joins it, what
// gives it up, and what the client hears. The rules that need the clock to run
// (deadlines, backoff, inactivity, delay) are in machine_time_test.go.

const (
	pre  = preProducer
	live = liveProducer

	all      = types.AllMessageInterest
	prematch = types.PrematchOnlyMessageInterest
	liveOnly = types.LiveOnlyMessageInterest
	hi       = types.HiPriorityOnlyMessageInterest
	low      = types.LowPriorityOnlyMessageInterest
	aliveOne = types.SystemAliveOnly
)

// ---- the first recovery

// RecoveryMachineTest: theFirstAliveAsksForAFullSnapshotAndItsCompletionBringsTheProducerUp
func TestMachine_FirstAliveAsksForAFullSnapshotAndItsCompletionBringsTheProducerUp(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.clock.advance(5 * time.Second)
	expect(t, len(f.out.calls), 0, "requests before any alive")
	expectDown(t, f, pre, true, "before any alive")

	f.alive(pre)
	r := f.lastSnapshot(pre)
	expect(t, r.producer, "pre", "the producer's name")
	expectTime(t, r.after, time.Time{}, "a cold start with no recovery point")
	expectCalls(t, f, live, 0, "producer 2 has sent no alive")
	expect(t, len(f.out.statuses), 0, "status changes while the first recovery runs")

	f.complete(r, 1)
	up := f.lastStatus(pre)
	expect(t, up.down, false, "down")
	expect(t, up.cause, causeFirstRecoveryCompleted, "cause")
	expect(t, up.reason(), types.FirstRecoveryCompletedProducerStatusReason, "reason")
	expectTime(t, up.at, f.now(), "when it came up")
	expectDown(t, f, pre, false, "after the completion")
	info := f.recoveryInfo(pre)
	expect(t, info.RequestID(), r.requestID, "recovery info request id")
	expect(t, info.Successful(), true, "recovery info successful")
	expect(t, info.NodeID().ValueOr(0), testNodeID, "recovery info node id")
	expect(t, info.After().IsZero(), true, "recovery info of a full snapshot")
	expect(t, f.m.counters.completed, 1, "completed")
}

// RecoveryMachineTest: theClientsRecoveryPointSeedsTheFirstRecovery
func TestMachine_ClientsRecoveryPointSeedsTheFirstRecovery(t *testing.T) {
	f := newFeed(t)
	from := f.now().Add(-time.Hour)
	f.cat.from[pre] = from
	f.open(1, all)
	f.startFeed()
	f.alive(pre)
	expectTime(t, f.lastSnapshot(pre).after, from, "the first recovery's start")
	cp, _ := f.checkpoint(1, pre)
	expectTime(t, cp, from, "the checkpoint")
}

// RecoveryMachineTest: aRecoveryPointIsClampedToTheStatefulRecoveryWindow
func TestMachine_RecoveryStartIsClampedToTheStatefulRecoveryWindow(t *testing.T) {
	f := newFeed(t)
	// inside the window when the client set it, outside once the feed has run an hour
	f.cat.from[pre] = f.now().Add(-3*24*time.Hour + time.Minute)
	f.open(1, all)
	f.startFeed()
	f.clock.advance(time.Hour)
	f.alive(pre)
	expectTime(t, f.lastSnapshot(pre).after, f.now().Add(-3*24*time.Hour), "the clamped start")
}

// RecoveryMachineTest: aRecoveryClampedToTheStatefulRecoveryWindowIsAWarningWithWhatIsDropped
func TestMachine_ClampToTheStatefulRecoveryWindowIsAWarningWithWhatIsDropped(t *testing.T) {
	f := newFeed(t)
	from := f.now().Add(-3*24*time.Hour + time.Minute)
	f.cat.from[pre] = from
	f.open(1, all)
	f.startFeed()
	f.clock.advance(time.Hour)
	f.logs.take()
	f.alive(pre)
	r := f.lastSnapshot(pre)
	expectLine(t, f.logs.take(), clampLine("WARN", r, from))
}

// clampLine is what a recovery clamped to the stateful window of 4320 minutes
// logs, at the level.
func clampLine(level string, r snapshotRequest, gapStart time.Time) string {
	return level + " Recovery " + strconv.Itoa(r.requestID) + " of producer 1: the gap began at " + fmtTime(gapStart) +
		", before the producer's stateful recovery window of 4320 minutes; asking from " + fmtTime(r.after) +
		", so " + r.after.Sub(gapStart).String() + " of what the producer sent cannot be recovered"
}

// RecoveryMachineTest: aRecoveryInsideTheStatefulRecoveryWindowIsNoWarning
func TestMachine_RecoveryInsideTheStatefulRecoveryWindowIsNoWarning(t *testing.T) {
	f := newFeed(t)
	from := f.now().Add(-3*24*time.Hour + time.Minute)
	f.cat.from[pre] = from
	f.open(1, all)
	f.startFeed()
	f.logs.take()
	f.alive(pre)
	expectTime(t, f.lastSnapshot(pre).after, from, "the start")
	for _, line := range f.logs.take() {
		if strings.Contains(line, "stateful recovery window") {
			t.Fatalf("a clamp was logged: %s", line)
		}
	}
}

// RecoveryMachineTest: aFullSnapshotIsNoClamp
func TestMachine_FullSnapshotIsNoClamp(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.clock.advance(time.Hour)
	f.logs.take()
	f.alive(pre)
	expectTime(t, f.lastSnapshot(pre).after, time.Time{}, "a full snapshot")
	for _, line := range f.logs.take() {
		if strings.Contains(line, "stateful recovery window") {
			t.Fatalf("a clamp was logged: %s", line)
		}
	}
}

// RecoveryMachineTest: aStartCappedAtTheProducersNowIsNoClamp
func TestMachine_StartCappedAtTheProducersNowIsNoClamp(t *testing.T) {
	f := newFeed(t)
	// the client's start is an hour ahead of the producer's clock: the recovery asks from its now
	f.cat.from[pre] = f.now().Add(time.Hour)
	f.open(1, all)
	f.startFeed()
	f.logs.take()
	f.alive(pre)
	expectTime(t, f.lastSnapshot(pre).after, f.now(), "capped at the producer's now")
	for _, line := range f.logs.take() {
		if strings.Contains(line, "stateful recovery window") {
			t.Fatalf("a clamp was logged: %s", line)
		}
	}
}

// RecoveryMachineTest: aClientsRecoveryPointInTheFutureAsksFromNow
func TestMachine_ClientsRecoveryPointInTheFutureAsksFromNow(t *testing.T) {
	f := newFeed(t)
	f.cat.from[pre] = f.now().Add(time.Hour)
	f.open(1, all)
	f.startFeed()
	f.alive(pre)
	expectTime(t, f.lastSnapshot(pre).after, f.now(), "the recovery's start")
	got, _ := f.resume(pre)
	expectTime(t, got, f.now(), "the resume point")
}

// RecoveryMachineTest: theWindowIsCountedBackByTheProducersClockWhicheverWayItIsOff
func TestMachine_WindowIsCountedBackByTheProducersClockWhicheverWayItIsOff(t *testing.T) {
	for _, offset := range []time.Duration{10 * time.Minute, -10 * time.Minute} {
		f := newFeed(t)
		f.cat.from[pre] = f.now().Add(-3*24*time.Hour + time.Minute)
		f.open(1, all)
		f.startFeed()
		f.clock.advance(time.Hour)
		// the producer's clock behind the SDK's by the offset, or ahead of it
		at := f.now().Add(-offset)
		f.m.alive(pre, at, at, f.stamp(), true)
		expectTime(t, f.lastSnapshot(pre).after, f.now().Add(-offset-3*24*time.Hour),
			"the window start, with an offset of "+offset.String())
	}
}

// RecoveryMachineTest: aMessageFromTheProducersFutureMovesNeitherTheResumePointNorTheRecoveryPastItsNow
func TestMachine_MessageFromTheProducersFutureMovesNeitherResumePointNorRecoveryPastItsNow(t *testing.T) {
	for _, offset := range []time.Duration{0, 10 * time.Minute, -10 * time.Minute} {
		f := upWith(t, all)
		f.clock.advance(5 * time.Second)
		// the producer's clock behind the SDK's by the offset, or ahead of it, as its alive says
		producerNow := f.now().Add(-offset)
		f.m.alive(pre, producerNow, producerNow, f.stamp(), true)
		// one message stamped an hour ahead: a component's fast clock, or a bad value
		ahead := producerNow.Add(time.Hour)
		f.m.processed(sid(1), pre, ahead, ahead, f.stamp(), 0)
		cp, _ := f.checkpoint(1, pre)
		expectTime(t, cp, ahead, "the checkpoint keeps it")
		got, _ := f.resume(pre)
		expectTime(t, got, producerNow, "the resume point, with an offset of "+offset.String())

		f.m.connectionDown()
		f.clock.advance(10 * time.Second)
		f.m.connectionUp()
		producerNow = f.now().Add(-offset)
		f.m.alive(pre, producerNow, producerNow, f.stamp(), true)
		expectTime(t, f.lastSnapshot(pre).after, producerNow, "the recovery, with an offset of "+offset.String())
		got, _ = f.resume(pre)
		expectTime(t, got, producerNow, "the resume point after it")
	}
}

// RecoveryMachineTest: anAliveFromTheProducersFutureMovesNoRecoveryPastItsNow
func TestMachine_AliveFromTheProducersFutureMovesNoRecoveryPastItsNow(t *testing.T) {
	f := upWith(t, all)
	f.clock.advance(5 * time.Second)
	ahead := f.now().Add(time.Hour)
	f.m.alive(pre, ahead, ahead, f.stamp(), true)
	// the next alive is on time again: the offset is back to none
	f.clock.advance(10 * time.Second)
	f.alive(pre)
	f.clock.advance(time.Second)
	// the producer stopped sending: every session misses what came after its last subscribed alive
	before := len(f.snapshots(pre))
	f.unsubscribed(pre)
	expect(t, len(f.snapshots(pre)), before+1, "recoveries after the unsubscribed alive")
	expectTime(t, f.lastSnapshot(pre).after, f.now(), "not the alive an hour ahead")
}

// RecoveryMachineTest: anAliveWithoutAPositiveTimestampMovesNeitherTheResumePointNorTheRecovery
func TestMachine_AliveWithoutAPositiveTimestampMovesNeitherResumePointNorRecovery(t *testing.T) {
	for _, stamp := range []time.Time{time.UnixMilli(0), time.UnixMilli(-3_600_000), {}} {
		f := upWith(t, all)
		f.clock.advance(5 * time.Second)
		processed := f.now()
		f.processed(1, pre, processed, 0)
		f.clock.advance(time.Second)
		// a bad value: as an offset it would put the producer's now at or before the epoch
		f.m.alive(pre, stamp, stamp, f.stamp(), true)
		got, _ := f.resume(pre)
		expectTime(t, got, processed, "the resume point after an alive stamped "+fmtTime(stamp))

		before := len(f.snapshots(pre))
		f.m.alive(pre, stamp, stamp, f.stamp(), false)
		expect(t, len(f.snapshots(pre)), before, "recoveries after an unsubscribed alive stamped "+fmtTime(stamp))
		expectDown(t, f, pre, false, "after the unsubscribed alive")

		f.m.connectionDown()
		f.clock.advance(10 * time.Second)
		f.m.connectionUp()
		f.alive(pre)
		expectTime(t, f.lastSnapshot(pre).after, processed, "the recovery after a loss")
	}
}

// RecoveryMachineTest: aLossToldBeforeStartLeavesTheFirstRecoveryAtTheClientsRecoveryPoint
func TestMachine_LossToldBeforeStartLeavesTheFirstRecoveryAtTheClientsRecoveryPoint(t *testing.T) {
	for _, interval := range []time.Duration{0, 30 * time.Minute} {
		for _, loss := range []string{"connection", "channel"} {
			s := defaultSettings()
			s.initialSnapshot = interval
			f := newFeedWith(t, s, statefulWindow)
			// more recent than the interval, so neither a full snapshot nor the interval hides it
			from := f.now().Add(-10 * time.Minute)
			f.cat.from[pre] = from
			f.open(1, all)
			f.lose(loss)
			f.clock.advance(3 * time.Second)
			f.startFeed()
			f.alive(pre)
			expectTime(t, f.lastSnapshot(pre).after, from, "the "+loss+" lost before start")
			cp, _ := f.checkpoint(1, pre)
			expectTime(t, cp, from, "the checkpoint")
		}
	}
}

// RecoveryMachineTest: aLossToldBeforeAColdStartLeavesTheFirstRecoveryAtTheIntervalBeforeTheStart
func TestMachine_LossToldBeforeAColdStartLeavesTheFirstRecoveryAtTheIntervalBeforeTheStart(t *testing.T) {
	for _, loss := range []string{"connection", "channel"} {
		s := defaultSettings()
		s.initialSnapshot = 30 * time.Minute
		f := newFeedWith(t, s, statefulWindow)
		f.open(1, all)
		f.lose(loss)
		f.clock.advance(time.Minute)
		f.startFeed()
		boundary := f.now().Add(-30 * time.Minute)
		f.alive(pre)
		expectTime(t, f.lastSnapshot(pre).after, boundary, "the "+loss+" lost a minute before start")
	}
}

// lose is the connection, or session 1's channel, lost and back, before the feed starts.
func (f *feed) lose(loss string) {
	if loss == "connection" {
		f.m.connectionDown()
		return
	}
	f.m.channelLost(sid(1))
	f.m.channelReopened(sid(1))
}

// RecoveryMachineTest: nothingIsAskedForBeforeStartOrWithoutASessionToReceiveIt
func TestMachine_NothingIsAskedForBeforeStartOrWithoutASessionToReceiveIt(t *testing.T) {
	f := newFeed(t)
	f.open(1, prematch)
	f.alive(pre)
	expect(t, len(f.out.calls), 0, "requests before start")
	f.startFeed()
	f.alive(live)
	expectCalls(t, f, live, 0, "no session takes the live producer")
	f.alive(pre)
	expectCalls(t, f, pre, 1, "the prematch producer")
}

// RecoveryMachineTest: nothingIsAskedForBeforeTheTransportHasBoundEverySessionsChannel
func TestMachine_NothingIsAskedForBeforeTheTransportHasBoundEverySessionsChannel(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.m.start()
	f.alive(pre)
	f.clock.advance(5 * time.Second)
	f.alive(pre)
	expect(t, len(f.out.calls), 0, "requests before the transport's first up")

	// the transport tells up once every session's channel is open
	f.m.connectionUp()
	expectCalls(t, f, pre, 1, "at once, its alive being recent")
	expectCalls(t, f, live, 0, "before its first alive")
	f.alive(live)
	expectCalls(t, f, live, 1, "at its first alive")
	f.complete(f.lastSnapshot(pre), 1)
	expectDown(t, f, pre, false, "after the completion")
	expect(t, f.m.counters.failed, 0, "failed")
}

// ---- completion per session

// RecoveryMachineTest: aProducerIsUpOnceEverySessionThatReceivesItHasSeenItsSnapshotComplete
func TestMachine_ProducerIsUpOnceEverySessionThatReceivesItHasSeenItsSnapshotComplete(t *testing.T) {
	f := newFeed(t)
	f.open(1, prematch)
	f.open(2, liveOnly) // does not receive the prematch producer
	f.open(3, hi)
	f.open(4, low) // next to a high-priority session it takes snapshot_complete too
	f.startFeed()
	f.alive(pre)
	r := f.lastSnapshot(pre)
	f.accept(r)

	f.snapshotComplete(1, pre, r.requestID)
	f.snapshotComplete(2, pre, r.requestID)
	f.snapshotComplete(3, pre, r.requestID)
	expectDown(t, f, pre, true, "with the low-priority session yet to see it")
	f.snapshotComplete(4, pre, r.requestID)
	expectDown(t, f, pre, false, "once every receiving session has")
	expect(t, len(f.statusesOf(pre)), 1, "status changes")
	expect(t, f.m.counters.unknownCompletions, 0, "the live-only session's completion is no unknown id")
}

// Q2: no exception for a low-priority session next to a high-priority one.
func TestMachine_LowPrioritySessionNextToAHighPriorityOneIsAwaitedToo(t *testing.T) {
	f := newFeed(t)
	f.open(1, hi)
	f.open(2, low)
	f.startFeed()
	f.alive(pre)
	requestedAt := f.now()
	r := f.lastSnapshot(pre)
	f.accept(r)
	f.snapshotComplete(1, pre, r.requestID)
	expectDown(t, f, pre, true, "the low-priority session has not seen its snapshot_complete")
	f.snapshotComplete(2, pre, r.requestID)
	expectDown(t, f, pre, false, "both have")
	cp, _ := f.checkpoint(2, pre)
	expectTime(t, cp, requestedAt, "the low-priority session's checkpoint moved to the request")
}

// RecoveryMachineTest: aCompletionForAnIdNotInFlightIsIgnoredAndCounted
func TestMachine_CompletionForAnIdNotInFlightIsIgnoredAndCounted(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.alive(pre)
	f.alive(live)
	f.snapshotComplete(1, pre, 1000)
	f.snapshotComplete(1, pre, f.lastSnapshot(live).requestID)
	expectDown(t, f, pre, true, "producer 1")
	expectDown(t, f, live, true, "producer 2")
	expect(t, f.m.counters.unknownCompletions, 2, "unknown completions")
}

// RecoveryMachineTest: aSnapshotCompleteQuickerThanTheApisAnswerCompletesTheRecovery
func TestMachine_SnapshotCompleteQuickerThanTheApisAnswerCompletesTheRecovery(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.alive(pre)
	r := f.lastSnapshot(pre)
	f.snapshotComplete(1, pre, r.requestID)
	expectDown(t, f, pre, false, "after the snapshot_complete")
	expect(t, f.recoveryInfo(pre).Successful(), true, "recovery info")
	f.accept(r)
	expectDown(t, f, pre, false, "after the answer")
}

// ---- checkpoints

// RecoveryMachineTest: aSessionsCheckpointIsTheMostRecentLiveMessageOrSubscribedAliveItFinished
func TestMachine_CheckpointIsTheMostRecentLiveMessageOrSubscribedAliveTheSessionFinished(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.bothUp(1)
	requestedAt := f.now()
	cp, _ := f.checkpoint(1, pre)
	expectTime(t, cp, requestedAt, "having seen the snapshot_complete, everything up to the request")

	f.clock.advance(time.Minute)
	at := f.now()
	check := func(want time.Time, what string) {
		t.Helper()
		got, _ := f.checkpoint(1, pre)
		expectTime(t, got, want, what)
	}
	f.m.processed(sid(1), pre, at.Add(-10*time.Second), at.Add(-10*time.Second), f.stamp(), 0)
	check(at.Add(-10*time.Second), "a live message")
	f.m.processed(sid(1), pre, at.Add(-50*time.Second), at.Add(-50*time.Second), f.stamp(), 0)
	check(at.Add(-10*time.Second), "an older message")
	f.m.processed(sid(1), pre, at, at, f.stamp(), f.lastSnapshot(pre).requestID)
	check(at.Add(-10*time.Second), "a snapshot message")
	f.m.sessionAlive(sid(1), pre, at.Add(-5*time.Second), at.Add(-5*time.Second), f.stamp(), false)
	check(at.Add(-10*time.Second), "an unsubscribed alive")
	f.m.sessionAlive(sid(1), pre, at.Add(-5*time.Second), at.Add(-5*time.Second), f.stamp(), true)
	check(at.Add(-5*time.Second), "a subscribed alive")
	other, _ := f.checkpoint(1, live)
	expectTime(t, other, requestedAt, "the other producer")
}

// RecoveryMachineTest: aMessageStampedAheadOfItsProducerMovesTheCheckpointOnlyToTheMarkItIsGiven
func TestMachine_MessageStampedAheadOfItsProducerMovesTheCheckpointOnlyToItsMark(t *testing.T) {
	f := upWith(t, all)
	f.clock.advance(time.Minute)
	at := f.now()
	f.m.processed(sid(1), pre, at.Add(10*time.Minute), at, f.stamp(), 0)
	cp, _ := f.checkpoint(1, pre)
	expectTime(t, cp, at, "the checkpoint")

	f.clock.advance(10 * time.Second)
	f.m.connectionDown()
	f.clock.advance(10 * time.Second)
	f.m.connectionUp()
	f.alive(pre)
	expectTime(t, f.lastSnapshot(pre).after, at, "the last real stamp, not the request's time")
}

// RecoveryMachineTest: anAliveOfTheSessionStampedAheadOfItsProducerMovesTheCheckpointOnlyToTheMarkItIsGiven
func TestMachine_SessionAliveStampedAheadOfItsProducerMovesTheCheckpointOnlyToItsMark(t *testing.T) {
	f := upWith(t, all)
	f.clock.advance(time.Minute)
	at := f.now()
	f.m.sessionAlive(sid(1), pre, at.Add(10*time.Minute), at, f.stamp(), true)
	cp, _ := f.checkpoint(1, pre)
	expectTime(t, cp, at, "the checkpoint")

	f.clock.advance(10 * time.Second)
	f.m.connectionDown()
	f.clock.advance(10 * time.Second)
	f.m.connectionUp()
	f.alive(pre)
	expectTime(t, f.lastSnapshot(pre).after, at, "the recovery")
}

// RecoveryMachineTest: anAliveStampedAheadOfItsProducerMovesTheSafePointOnlyToTheMarkItIsGiven
func TestMachine_AliveStampedAheadOfItsProducerMovesTheSafePointOnlyToItsMark(t *testing.T) {
	f := upWith(t, all)
	f.clock.advance(time.Minute)
	at := f.now()
	f.m.alive(pre, at.Add(10*time.Minute), at, f.stamp(), true)
	f.clock.advance(10 * time.Second)
	f.unsubscribed(pre)
	f.clock.advance(10 * time.Second)
	f.alive(pre)
	expectTime(t, f.lastSnapshot(pre).after, at, "the producer's gap starts at the last safe point, which is the mark")
}

// RecoveryMachineTest: aMessageNotAheadOfItsProducerMovesTheCheckpointToItsOwnStamp
func TestMachine_MessageNotAheadOfItsProducerMovesTheCheckpointToItsOwnStamp(t *testing.T) {
	f := upWith(t, all)
	f.clock.advance(time.Minute)
	at := f.now()
	f.processed(1, pre, at.Add(-10*time.Second), 0)
	cp, _ := f.checkpoint(1, pre)
	expectTime(t, cp, at.Add(-10*time.Second), "after a message")
	f.m.sessionAlive(sid(1), pre, at.Add(-5*time.Second), at.Add(-5*time.Second), f.stamp(), true)
	cp, _ = f.checkpoint(1, pre)
	expectTime(t, cp, at.Add(-5*time.Second), "after an alive")
}

// RecoveryMachineTest: aLostConnectionRecoversFromTheOldestCheckpointOfTheSessionsThatReceiveTheProducer
func TestMachine_LostConnectionRecoversFromTheOldestCheckpointOfTheSessionsThatReceiveTheProducer(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.open(2, prematch)
	f.open(3, liveOnly)
	f.startFeed()
	f.bothUp(1, 2, 3)
	f.clock.advance(time.Minute)
	at := f.now()
	f.processed(1, pre, at.Add(-10*time.Second), 0)
	f.processed(2, pre, at.Add(-40*time.Second), 0)
	f.processed(1, live, at.Add(-5*time.Second), 0)
	f.processed(3, live, at.Add(-2*time.Second), 0)

	f.m.connectionDown()
	down := f.lastStatus(pre)
	expect(t, down.cause, causeConnectionLost, "cause")
	expect(t, down.reason(), types.ConnectionDownProducerStatusReason, "reason")
	expectDown(t, f, live, true, "the live producer")
	f.alive(pre)
	expectCalls(t, f, pre, 1, "while the connection is down")

	f.m.connectionUp()
	f.alive(pre)
	f.alive(live)
	expectTime(t, f.lastSnapshot(pre).after, at.Add(-40*time.Second), "the prematch producer's recovery")
	expectTime(t, f.lastSnapshot(live).after, at.Add(-5*time.Second), "the live producer's recovery")
}

// ---- the resume point

// RecoveryMachineTest: theRecoveryTimestampIsTheClientsStartUntilTheStartThenTheFeedsOwn
func TestMachine_ResumePointIsNotPublishedBeforeStartThenTheSeedThenTheFeedsOwn(t *testing.T) {
	f := newFeed(t)
	from := f.now().Add(-time.Hour)
	f.cat.from[pre] = from
	f.open(1, all)
	_, published := f.resume(pre)
	expect(t, published, false, "published before the start")

	f.startFeed()
	got, _ := f.resume(pre)
	expectTime(t, got, from, "seeded")
	got, ok := f.resume(live)
	expect(t, ok, true, "the live producer's point is published")
	expectTime(t, got, time.Time{}, "a session that misses everything")
	// a full snapshot is a point of its own: what the client sets now does not show through
	f.cat.from[live] = f.now()
	got, _ = f.resume(live)
	expectTime(t, got, time.Time{}, "a full snapshot, whatever the client sets once the feed is open")

	f.clock.advance(5 * time.Second)
	f.alive(pre)
	requestedAt := f.now()
	f.complete(f.lastSnapshot(pre), 1)
	got, _ = f.resume(pre)
	expectTime(t, got, requestedAt, "the completed recovery's request")
}

// RecoveryMachineTest: theResumePointIsTheOldestCheckpointAmongTheSessions
func TestMachine_ResumePointIsTheOldestCheckpointAmongTheSessions(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.open(2, all)
	f.startFeed()
	f.bothUp(1, 2)
	f.clock.advance(time.Minute)
	f.live(1, pre, 0)
	// session 2 is half a minute behind: what it has not processed is only in its queue
	f.live(2, pre, 30*time.Second)
	got, _ := f.resume(pre)
	expectTime(t, got, f.now().Add(-30*time.Second), "the resume point")

	f.close(2)
	got, _ = f.resume(pre)
	expectTime(t, got, f.now(), "with the session behind closed")
}

// RecoveryMachineTest: whileTheFeedClosesItsSessionsInEitherOrderTheResumePointOnlyGoesBack
func TestMachine_WhileTheFeedClosesItsSessionsInEitherOrderTheResumePointOnlyGoesBack(t *testing.T) {
	for _, order := range [][]int{{1, 2}, {2, 1}} {
		f := newFeed(t)
		f.open(1, all)
		f.open(2, all)
		f.startFeed()
		f.bothUp(1, 2)
		lastSubscribed := f.now()
		f.clock.advance(time.Minute)
		f.live(1, pre, 0)
		f.live(2, pre, 30*time.Second)
		behind := f.now().Add(-30 * time.Second)
		got, _ := f.resume(pre)
		expectTime(t, got, behind, "before the close")

		f.m.beginClosing()
		f.close(order[0])
		got, _ = f.resume(pre)
		expectTime(t, got, behind, "the first session closed")
		// a gap opened meanwhile still takes it back
		f.unsubscribed(pre)
		got, _ = f.resume(pre)
		expectTime(t, got, lastSubscribed, "after a gap")
		f.close(order[1])
		got, _ = f.resume(pre)
		expectTime(t, got, lastSubscribed, "every session closed")
	}
}

// RecoveryMachineTest: aProducerGapAfterTheLastSessionClosedAtShutdownStillTakesThePointBack
func TestMachine_ProducerGapAfterTheLastSessionClosedAtShutdownStillTakesThePointBack(t *testing.T) {
	f := newFeed(t)
	from := f.now().Add(-time.Hour)
	f.cat.from[live] = from
	f.open(1, prematch)
	f.startFeed()
	f.alive(pre)
	f.complete(f.lastSnapshot(pre), 1)
	lastSubscribed := f.now()
	f.clock.advance(10 * time.Second)
	f.live(1, pre, 0)
	processed := f.now()
	got, _ := f.resume(pre)
	expectTime(t, got, processed, "before the close")

	// the documented order: closing, every session closed, then what is still queued
	f.m.beginClosing()
	f.close(1)
	got, _ = f.resume(pre)
	expectTime(t, got, processed, "the session closed")
	f.unsubscribed(pre)
	got, _ = f.resume(pre)
	expectTime(t, got, lastSubscribed, "the producer's gap, opened after its last session closed")
	f.unsubscribed(live)
	_, published := f.resume(live)
	expect(t, published, false, "a producer no session ever received publishes nothing: the client's start stands")
}

// RecoveryMachineTest: aProducerGapAfterTheLastSessionClosedWhileTheFeedRunsChangesNothing
func TestMachine_ProducerGapAfterTheLastSessionClosedWhileTheFeedRunsChangesNothing(t *testing.T) {
	f := upWith(t, all)
	f.clock.advance(10 * time.Second)
	f.live(1, pre, 0)
	processed := f.now()
	got, _ := f.resume(pre)
	expectTime(t, got, processed, "before the close")
	f.close(1)
	f.unsubscribed(pre)
	got, _ = f.resume(pre)
	expectTime(t, got, processed, "no session receives it: it keeps what it had")
}

// RecoveryMachineTest: aProducersFirstPointIsPublishedEvenOnceTheFeedIsClosing
func TestMachine_ProducersFirstPointIsPublishedEvenOnceTheFeedIsClosing(t *testing.T) {
	s := defaultSettings()
	s.initialSnapshot = 30 * time.Minute
	f := newFeedWith(t, s, statefulWindow)
	f.open(1, all)
	f.m.start()
	// closing before anything was published: the first point still goes out
	f.m.beginClosing()
	got, ok := f.resume(pre)
	expect(t, ok, true, "published")
	expectTime(t, got, f.now().Add(-30*time.Minute), "the initial interval's start, not the client's none")
}

// RecoveryMachineTest: aGapOlderThanTheCheckpointHoldsTheResumePointUntilItsRecoveryCompletes
func TestMachine_GapOlderThanTheCheckpointHoldsTheResumePointUntilItsRecoveryCompletes(t *testing.T) {
	f := upWith(t, all)
	f.clock.advance(time.Minute)
	f.live(1, pre, 0)
	lost := f.now()
	f.m.connectionDown()
	got, _ := f.resume(pre)
	expectTime(t, got, lost, "after the loss")

	f.clock.advance(10 * time.Second)
	f.m.connectionUp()
	f.live(1, pre, 0)
	cp, _ := f.checkpoint(1, pre)
	expectTime(t, cp, f.now(), "the checkpoint")
	got, _ = f.resume(pre)
	expectTime(t, got, lost, "the gap's start, older than the checkpoint")

	f.clock.advance(10 * time.Second)
	f.alive(pre)
	r := f.lastSnapshot(pre)
	expectTime(t, r.after, lost, "the recovery's start")
	requestedAt := f.now()
	got, _ = f.resume(pre)
	expectTime(t, got, lost, "while the recovery is in flight")
	f.complete(r, 1)
	got, _ = f.resume(pre)
	expectTime(t, got, requestedAt, "the recovery's request, past the checkpoint")
}

// RecoveryMachineTest: aLaterAliveDoesNotMoveTheResumePointPastWhatTheSessionProcessed
func TestMachine_LaterAliveDoesNotMoveTheResumePointPastWhatTheSessionProcessed(t *testing.T) {
	f := upWith(t, all)
	f.clock.advance(10 * time.Second)
	f.live(1, pre, 0)
	checkpoint := f.now()
	// the alive channel is ahead of the session, whose queue still holds what came since
	f.clock.advance(10 * time.Second)
	f.alive(pre)
	cp, _ := f.checkpoint(1, pre)
	expectTime(t, cp, checkpoint, "the checkpoint")
	got, _ := f.resume(pre)
	expectTime(t, got, checkpoint, "the session's checkpoint, not the later alive")
}

// RecoveryMachineTest: aProducerThatStopsSendingHoldsTheResumePointAtItsLastSubscribedAlive
func TestMachine_ProducerThatStopsSendingHoldsTheResumePointAtItsLastSubscribedAlive(t *testing.T) {
	f := upWith(t, all)
	lastSubscribed := f.now()
	f.clock.advance(10 * time.Second)
	f.live(1, pre, 0)
	got, _ := f.resume(pre)
	expectTime(t, got, f.now(), "before the gap")
	f.unsubscribed(pre)
	got, _ = f.resume(pre)
	expectTime(t, got, lastSubscribed, "the producer's gap, older than the session's checkpoint")
}

// RecoveryMachineTest: aProducerNoSessionReceivesReportsTheClientsStartAndOneWhoseSessionClosedItsLastPoint
func TestMachine_ProducerNoSessionReceivesReportsTheClientsStartAndOneWhoseSessionClosedItsLastPoint(t *testing.T) {
	f := newFeed(t)
	from := f.now().Add(-time.Hour)
	f.cat.from[pre] = from
	f.cat.from[live] = from
	f.open(1, liveOnly)
	f.open(2, aliveOne)
	f.startFeed()
	for range 3 {
		f.clock.advance(10 * time.Second)
		f.alive(pre)
		f.alive(live)
		f.sessionAlive(1, live)
	}
	_, published := f.resume(pre)
	expect(t, published, false, "no session receives the prematch producer: the client's start stands")

	// the session's last alive is its checkpoint once the recovery has closed its gap
	checkpoint := f.now()
	f.complete(f.lastSnapshot(live), 1)
	got, _ := f.resume(live)
	expectTime(t, got, checkpoint, "after the recovery")
	f.close(1)
	f.clock.advance(30 * time.Second)
	f.alive(live)
	got, _ = f.resume(live)
	expectTime(t, got, checkpoint, "its only session closed")
}

// RecoveryMachineTest: theResumePointIsNotClampedToTheStatefulRecoveryWindow
func TestMachine_ResumePointIsNotClampedToTheStatefulRecoveryWindow(t *testing.T) {
	f := newFeed(t)
	from := f.now().Add(-3*24*time.Hour + time.Minute)
	f.cat.from[pre] = from
	f.open(1, all)
	f.startFeed()
	f.clock.advance(time.Hour)
	f.alive(pre)
	expectTime(t, f.lastSnapshot(pre).after, f.now().Add(-3*24*time.Hour), "the request is clamped")
	got, _ := f.resume(pre)
	expectTime(t, got, from, "the resume point is not")
}

// RecoveryMachineTest: aColdStartWithAnInitialSnapshotIntervalResumesFromThatInterval
func TestMachine_ColdStartWithAnInitialSnapshotIntervalResumesFromThatInterval(t *testing.T) {
	s := defaultSettings()
	s.initialSnapshot = 30 * time.Minute
	f := newFeedWith(t, s, statefulWindow)
	f.open(1, all)
	f.startFeed()
	boundary := f.now().Add(-30 * time.Minute)
	f.clock.advance(5 * time.Minute)
	got, _ := f.resume(pre)
	expectTime(t, got, boundary, "the resume point")
}

// ---- a producer that stops sending

// RecoveryMachineTest: anUnsubscribedAliveRecoversFromTheLastSubscribedOneEvenWithSessionsBehind
func TestMachine_UnsubscribedAliveRecoversFromTheLastSubscribedOneEvenWithSessionsBehind(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.bothUp(1)
	f.clock.advance(time.Minute)
	// the alive channel has seen these two; the session has not finished either yet
	lastWhileUp := f.now().Add(-2 * time.Second)
	f.m.alive(pre, lastWhileUp, lastWhileUp, f.stamp(), true)
	f.m.alive(pre, lastWhileUp.Add(time.Second), lastWhileUp.Add(time.Second), f.stamp(), false)

	down := f.lastStatus(pre)
	expect(t, down.down, true, "down")
	expect(t, down.cause, causeUnsubscribed, "cause")
	expect(t, down.reason(), types.OtherProducerStatusReason, "reason")
	r := f.lastSnapshot(pre)
	expectTime(t, r.after, lastWhileUp, "the recovery's start")

	f.complete(r, 1)
	back := f.lastStatus(pre)
	expect(t, back.down, false, "back")
	expect(t, back.cause, causeRecoveryCompleted, "cause")
	expect(t, back.reason(), types.ReturnedFromInactivityProducerStatusReason, "reason")
}

// ---- coalescing

// RecoveryMachineTest: triggersWhileARecoveryIsInFlightJoinItAndWhatOpenedSinceAsksForOneMore
func TestMachine_TriggersWhileARecoveryIsInFlightJoinItAndWhatOpenedSinceAsksForOneMore(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.bothUp(1)
	f.unsubscribed(pre)
	r := f.lastSnapshot(pre)
	// before the producer took the request: it may not have seen it yet
	f.unsubscribed(pre)
	f.alive(pre)
	f.accept(r)
	// another session loses its channel: the producer still has what the recovery is sending
	f.open(2, all)
	f.m.channelLost(sid(2))
	f.m.channelReopened(sid(2))
	expectCalls(t, f, pre, 2, "one in flight at a time")

	f.snapshotComplete(1, pre, r.requestID)
	more := f.lastSnapshot(pre)
	expect(t, more.requestID != r.requestID, true, "one more, for what opened since it was asked for")
	expectCalls(t, f, pre, 3, "after the first completes")
	expectDown(t, f, pre, true, "while the follow-up runs")

	f.complete(more, 1, 2)
	expectDown(t, f, pre, false, "after the follow-up")
	expectCalls(t, f, pre, 3, "no more")
}

// RecoveryMachineTest: anUnsubscribedAliveBeforeTheProducerTookTheRecoveryJoinsItAndAsksForOneMoreAfterIt
func TestMachine_UnsubscribedAliveBeforeTheProducerTookTheRecoveryJoinsItAndAsksForOneMoreAfterIt(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.bothUp(1)
	f.unsubscribed(pre)
	r := f.lastSnapshot(pre)
	// sent before the API had answered: the producer may not have seen the request yet
	f.unsubscribed(pre)
	f.accept(r)
	expectCalls(t, f, pre, 2, "before the completion")
	f.snapshotComplete(1, pre, r.requestID)
	expectCalls(t, f, pre, 3, "one more")
	expectDown(t, f, pre, true, "while it runs")
}

// RecoveryMachineTest: anUnsubscribedAliveReceivedBeforeTheApisAnswerAndHandledAfterItJoinsTheRecovery
func TestMachine_UnsubscribedAliveReceivedBeforeTheApisAnswerAndHandledAfterItJoinsTheRecovery(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.bothUp(1)
	f.unsubscribed(pre)
	r := f.lastSnapshot(pre)
	receivedEarlier := f.stamp()
	receivedEarlier.mono += time.Second
	f.clock.advance(2 * time.Second)
	f.accept(r)
	// alives reach the actor apart from the answers: this one was received before the answer
	f.m.alive(pre, f.now(), f.now(), receivedEarlier, false)
	expect(t, f.inFlight(pre), r.requestID, "joined, not given up")
	expect(t, f.m.counters.abandoned, 0, "abandoned")
	f.snapshotComplete(1, pre, r.requestID)
	expectCalls(t, f, pre, 3, "one more")
	expectDown(t, f, pre, true, "while it runs")
}

// RecoveryMachineTest: aProducerThatRestartsDuringARecoveryGivesItUpAndIsAskedAgainAtTheNextAlive
func TestMachine_ProducerThatRestartsDuringARecoveryGivesItUpAndIsAskedAgainAtTheNextAlive(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.open(2, all)
	f.startFeed()
	f.bothUp(1, 2)
	f.unsubscribed(pre)
	lost := f.lastSnapshot(pre)
	f.accept(lost)
	f.snapshotComplete(1, pre, lost.requestID)

	// the producer took the request, then restarted: it says unsubscribed again
	f.clock.advance(10 * time.Second)
	f.unsubscribed(pre)
	expect(t, f.inFlight(pre), 0, "given up")
	expect(t, f.m.counters.abandoned, 1, "abandoned")
	expect(t, f.m.counters.failed, 0, "not the request's failure")
	f.clock.advance(5 * time.Second)
	expectCalls(t, f, pre, 2, "not before the next alive")

	f.clock.advance(5 * time.Second)
	f.unsubscribed(pre)
	again := f.lastSnapshot(pre)
	expect(t, again.requestID != lost.requestID, true, "a new request")
	expectTime(t, again.after, lost.after, "from the same point")
	// what the lost one would have completed changes nothing
	f.snapshotComplete(2, pre, lost.requestID)
	expect(t, f.m.counters.unknownCompletions, 1, "unknown completions")
	f.complete(again, 1, 2)
	expectDown(t, f, pre, false, "after the new one")
}

// RecoveryMachineTest: aLostConnectionGivesUpTheRecoveryInFlightWhoseSnapshotWentWithTheQueues
func TestMachine_LostConnectionGivesUpTheRecoveryInFlightWhoseSnapshotWentWithTheQueues(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.alive(pre)
	lost := f.lastSnapshot(pre)
	f.accept(lost)
	f.m.connectionDown()
	f.m.connectionUp()
	f.alive(pre)
	again := f.lastSnapshot(pre)
	expect(t, again.requestID != lost.requestID, true, "a new request")
	expect(t, f.m.counters.abandoned, 1, "abandoned")
	expect(t, f.m.counters.failed, 0, "not the API's failure")

	f.snapshotComplete(1, pre, lost.requestID)
	expectDown(t, f, pre, true, "after the old one's completion")
	expect(t, f.m.counters.unknownCompletions, 1, "unknown completions")
	f.complete(again, 1)
	expectDown(t, f, pre, false, "after the new one")
}

// RecoveryMachineTest: aSessionsLostChannelRecoversFromItsCheckpointAndGivesUpARecoveryWaitingForIt
func TestMachine_SessionsLostChannelRecoversFromItsCheckpointAndGivesUpARecoveryWaitingForIt(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.open(2, all)
	f.startFeed()
	f.bothUp(1, 2)
	f.clock.advance(time.Minute)
	at := f.now()
	f.processed(1, pre, at.Add(-time.Second), 0)
	f.processed(2, pre, at.Add(-30*time.Second), 0)
	f.alive(pre)
	f.unsubscribed(pre)
	waiting := f.lastSnapshot(pre)
	f.accept(waiting)
	f.snapshotComplete(1, pre, waiting.requestID)

	f.m.channelLost(sid(2))
	expect(t, f.lastStatus(pre).cause, causeChannelLost, "cause")
	f.alive(pre)
	expect(t, f.lastSnapshot(pre).requestID, waiting.requestID, "nothing asked for before the new channel is bound")
	f.m.channelReopened(sid(2))
	again := f.lastSnapshot(pre)
	expect(t, again.requestID != waiting.requestID, true, "asked for again")
	expectTime(t, again.after, at.Add(-30*time.Second), "from the session's checkpoint")
	expect(t, f.m.counters.abandoned, 1, "abandoned")
	expectCalls(t, f, live, 1, "the live producer is asked for at its next alive")
	f.alive(live)
	expectCalls(t, f, live, 2, "after its alive")
}

// RecoveryMachineTest: aLostChannelHoldsTheRecoveryOfItsSessionsProducersUntilItsNewOneIsBound
func TestMachine_LostChannelHoldsTheRecoveryOfItsSessionsProducersUntilItsNewOneIsBound(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.open(2, liveOnly)
	f.startFeed()
	f.bothUp(1, 2)
	before := len(f.out.calls)

	f.m.channelLost(sid(2))
	for range 6 {
		f.clock.advance(10 * time.Second)
		f.alive(pre)
		f.alive(live)
	}
	expect(t, len(f.out.calls), before, "nothing for the live producer, whose snapshot would reach no queue of session 2")
	// the prematch producer is not session 2's: nothing held it, and nothing was missing
	expectDown(t, f, pre, false, "the prematch producer")

	f.m.channelReopened(sid(2))
	expectCalls(t, f, live, 2, "at once, the alives being recent")
	f.complete(f.lastSnapshot(live), 1, 2)
	expectDown(t, f, live, false, "after the completion")
	// a second report of the same new channel changes nothing
	f.m.channelReopened(sid(2))
	expectCalls(t, f, live, 2, "after a second report")
}

// ---- sessions opening and closing

// Q5: a session opened for a producer that has recovered starts at the producer's
// now with no gap, no down and no request.
// (RecoveryMachineTest: aSessionThatOpensStartsFromTheProducersRecoveryPointAndAsksForARecovery, changed)
func TestMachine_SessionOpenedForARecoveredProducerStartsNowWithNoGap(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.bothUp(1)
	f.clock.advance(time.Minute)
	f.processed(1, pre, f.now().Add(-5*time.Second), 0)
	statuses := len(f.out.statuses)
	calls := len(f.out.calls)

	f.open(2, prematch)
	f.alive(pre)
	cp, _ := f.checkpoint(2, pre)
	expectTime(t, cp, f.now(), "the new session starts at the producer's now")
	_, received := f.checkpoint(2, live)
	expect(t, received, false, "the live producer is not received")
	expect(t, len(f.out.statuses), statuses, "status changes")
	expect(t, len(f.out.calls), calls, "requests")
	expectDown(t, f, pre, false, "the producer")
	got, _ := f.resume(pre)
	expectTime(t, got, f.now().Add(-5*time.Second), "the resume point is the oldest checkpoint")
}

// Q5: a lane of a producer that has not recovered yet gets the seed gap, as at start.
func TestMachine_SessionOpenedForAProducerThatNeverRecoveredGetsTheSeedGap(t *testing.T) {
	f := newFeed(t)
	from := f.now().Add(-time.Hour)
	f.cat.from[live] = from
	f.open(1, prematch)
	f.startFeed()
	f.alive(pre)
	f.complete(f.lastSnapshot(pre), 1)

	f.clock.advance(time.Minute)
	f.open(2, liveOnly)
	cp, _ := f.checkpoint(2, live)
	expectTime(t, cp, from, "the late session's checkpoint is the seed")
	f.alive(live)
	r := f.lastSnapshot(live)
	expectTime(t, r.after, from, "the first recovery of the producer starts at the seed")
	f.complete(r, 2)
	expectDown(t, f, live, false, "after it")
}

// A late lane for a producer whose first recovery is in flight is not awaited by
// it, and its seed gap asks for one more once it completes.
func TestMachine_SessionOpenedDuringTheFirstRecoveryIsNotAwaitedByItButAsksForOneMore(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.alive(pre)
	first := f.lastSnapshot(pre)
	f.accept(first)
	f.open(2, prematch)
	f.snapshotComplete(1, pre, first.requestID)
	expectCalls(t, f, pre, 2, "one more for the new session's gap")
	expectDown(t, f, pre, true, "until the new session has recovered")
	f.complete(f.lastSnapshot(pre), 1, 2)
	expectDown(t, f, pre, false, "after the follow-up")
}

// Q1: a producer that is not in the catalog at the start gets its track lazily.
func TestMachine_ProducerLearnedAfterTheStartGetsItsTrackWhenItsFirstFactArrives(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	late := fakeProducer{id: 9, name: "late", scopes: []types.ProducerScope{types.LiveProducerScope}, window: statefulWindow}
	f.cat.list = append(f.cat.list, late)
	if f.m.byID[9] != nil {
		t.Fatal("the producer had a track before any fact about it")
	}
	f.alive(9)
	expect(t, f.m.byID[9] != nil, true, "a track after its first alive")
	r := f.lastSnapshot(9)
	expect(t, r.producer, "late", "the late producer's request")
	f.complete(r, 1)
	expectDown(t, f, 9, false, "after its recovery")
	expect(t, f.m.counters.unknownProducers, 0, "unknown producers")
	f.processed(1, 9, f.now(), 0)
	cp, ok := f.checkpoint(1, 9)
	expect(t, ok, true, "the existing session got a lane")
	expectTime(t, cp, f.now(), "the lane's checkpoint")
}

// A client's own SYSTEM_ALIVE_ONLY session receives alives only: nothing waits
// for its snapshot_complete, and losing its channel, or a new one replacing it,
// takes no producer down and asks for nothing.
// RecoveryMachineTest: aClientsAliveOnlySessionReceivesNoProducer
func TestMachine_ClientsAliveOnlySessionReceivesNoProducer(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.open(2, aliveOne)
	f.startFeed()
	f.alive(pre)
	r := f.lastSnapshot(pre)
	f.accept(r)
	f.snapshotComplete(1, pre, r.requestID)
	expectDown(t, f, pre, false, "up without the alive-only session's snapshot_complete")
	_, ok := f.checkpoint(2, pre)
	expect(t, ok, false, "a lane of the alive-only session")

	statuses := len(f.out.statuses)
	f.m.channelLost(sid(2))
	expectDown(t, f, pre, false, "after its channel was lost")
	expect(t, len(f.out.statuses), statuses, "status changes")
	f.m.channelReopened(sid(2))
	expectCalls(t, f, pre, 1, "recoveries")
}

// Q13: losing the SDK's own alive channel alone opens no gap: it is no session, so
// the machine hears nothing of it, and a channel it does not know changes nothing.
func TestMachine_ChannelOfNoSessionOpensNoGap(t *testing.T) {
	f := upWith(t, all)
	statuses := len(f.out.statuses)
	calls := len(f.out.calls)
	f.m.channelLost(sid(99))
	f.m.channelReopened(sid(99))
	expect(t, len(f.out.statuses), statuses, "status changes")
	expect(t, len(f.out.calls), calls, "requests")
	expect(t, f.m.hasGaps(f.m.byID[pre]), false, "gaps")
}

// Q2: a producer no session with a lane receives has nothing to recover: it comes
// up on a fresh alive, without a recovery.
// (RecoveryMachineTest: aFeedWhoseOnlySessionIsAliveOnlyAsksForNoRecoveryAndTellsNoProducerUp, changed)
func TestMachine_FeedWhoseOnlySessionIsAliveOnlyAsksForNoRecoveryAndComesUpOnAFreshAlive(t *testing.T) {
	f := newFeed(t)
	f.open(1, aliveOne)
	f.startFeed()
	for range 3 {
		f.clock.advance(10 * time.Second)
		f.alive(pre)
		f.alive(live)
	}
	expect(t, len(f.out.calls), 0, "recoveries asked for")
	expectDown(t, f, pre, false, "the prematch producer")
	expectDown(t, f, live, false, "the live producer")
	expect(t, f.lastStatus(pre).cause, causeFirstRecoveryCompleted, "cause")

	f.unsubscribed(pre)
	expectDown(t, f, pre, true, "after an unsubscribed alive")
	f.alive(pre)
	expectDown(t, f, pre, false, "after the next alive")
	expect(t, len(f.out.calls), 0, "recoveries asked for after all")
}

// RecoveryMachineTest: aSessionThatClosesLeavesTheCompletionsAndTheCheckpointsAtOnce
func TestMachine_SessionThatClosesLeavesTheCompletionsAndTheCheckpointsAtOnce(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.open(2, all)
	f.startFeed()
	f.alive(pre)
	r := f.lastSnapshot(pre)
	f.accept(r)
	f.snapshotComplete(1, pre, r.requestID)
	expectDown(t, f, pre, true, "waiting for session 2")

	f.close(2)
	expectDown(t, f, pre, false, "the closed session was all it waited for")
	_, ok := f.checkpoint(2, pre)
	expect(t, ok, false, "the closed session's lane")

	// the closed session's checkpoint no longer holds a recovery back
	f.processed(1, pre, f.now(), 0)
	f.m.connectionDown()
	f.m.connectionUp()
	f.alive(pre)
	expectTime(t, f.lastSnapshot(pre).after, f.now(), "the recovery's start")
	// a late fact of the closed session changes nothing
	f.m.processed(sid(2), pre, time.UnixMilli(1), time.UnixMilli(1), f.stamp(), 0)
	f.snapshotComplete(2, pre, f.lastSnapshot(pre).requestID)
	expectDown(t, f, pre, true, "after a late fact of the closed session")
}

// RecoveryMachineTest: aProducerDownOnlyForAClosedSessionsGapComesBack
func TestMachine_ProducerDownOnlyForAClosedSessionsGapComesBack(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.bothUp(1)
	f.open(2, prematch)
	f.m.channelLost(sid(2))
	f.m.channelReopened(sid(2))
	f.alive(pre)
	expectCalls(t, f, pre, 2, "recoveries for session 2's gap")
	f.refuse(f.lastSnapshot(pre))
	expectDown(t, f, pre, true, "after the refusal")
	f.close(2)
	expectDown(t, f, pre, false, "after the session closed")
	expect(t, f.lastStatus(pre).cause, causeRecoveryCompleted, "cause")
}

// RecoveryMachineTest: sessionsClosingAtShutdownCompleteNoRecoveryAndBringNoProducerUp
func TestMachine_SessionsClosingAtShutdownCompleteNoRecoveryAndBringNoProducerUp(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.open(2, all)
	f.startFeed()
	f.alive(pre)
	r := f.lastSnapshot(pre)
	f.accept(r)
	// one session has its snapshot_complete, the other's is still coming as the feed closes
	f.snapshotComplete(1, pre, r.requestID)
	told := len(f.out.statuses)

	f.m.beginClosing()
	f.close(2)
	f.close(1)
	expectDown(t, f, pre, true, "never recovered")
	expect(t, f.m.counters.completed, 0, "completions")
	expect(t, len(f.out.statuses), told, "status changes")
}

// RecoveryMachineTest: aProducerDownOnlyForASessionsGapStaysDownAsTheFeedCloses
func TestMachine_ProducerDownOnlyForASessionsGapStaysDownAsTheFeedCloses(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.bothUp(1)
	f.open(2, prematch)
	f.m.channelLost(sid(2))
	f.m.channelReopened(sid(2))
	f.alive(pre)
	f.refuse(f.lastSnapshot(pre))
	expectDown(t, f, pre, true, "after the refusal")
	told := len(f.out.statuses)

	f.m.beginClosing()
	f.close(2)
	expectDown(t, f, pre, true, "as the feed closes")
	expect(t, len(f.out.statuses), told, "status changes")
}

// RecoveryMachineTest: closingTheLastSessionWhileTheConnectionIsDownKeepsTheProducerDownUntilItIsBack
func TestMachine_ClosingTheLastSessionWhileTheConnectionIsDownKeepsTheProducerDownUntilItIsBack(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.bothUp(1)
	f.m.connectionDown()
	f.close(1)
	expectDown(t, f, pre, true, "while the connection is down")
	f.m.connectionUp()
	expectDown(t, f, pre, false, "no session misses anything")
}

// RecoveryMachineTest: aSessionThatSawTheSnapshotCompleteAndClosedStillCompletesIt
func TestMachine_SessionThatSawTheSnapshotCompleteAndClosedStillCompletesIt(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.open(2, all)
	f.startFeed()
	f.alive(pre)
	r := f.lastSnapshot(pre)
	// quicker than the API's answer, as a snapshot_complete may be
	f.snapshotComplete(2, pre, r.requestID)
	f.close(1)
	expectDown(t, f, pre, false, "after the closed session left")
}

// RecoveryMachineTest: aSessionClosingBeforeTheApiAnswersLeavesTheAnswerToDecide
func TestMachine_SessionClosingBeforeTheApiAnswersLeavesTheAnswerToDecide(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.alive(pre)
	accepted := f.lastSnapshot(pre)
	f.alive(live)
	refused := f.lastSnapshot(live)
	// the only session closes while both requests wait for the API
	f.close(1)
	expect(t, f.m.counters.completed, 0, "no snapshot_complete, no answer yet")
	expectDown(t, f, pre, true, "before the answer")

	f.accept(accepted)
	expectDown(t, f, pre, false, "accepted, with nothing to wait for")
	f.refuse(refused)
	expectDown(t, f, live, true, "refused")
	expect(t, f.m.counters.completed, 1, "completed")
	expect(t, f.m.counters.failed, 1, "failed")
	expect(t, f.recoveryInfo(live).Successful(), false, "recovery info of the refused one")
}

// RecoveryMachineTest: aRecoveryThatFailsWithNothingLeftToRecoverBringsTheProducerUp
func TestMachine_RecoveryThatFailsWithNothingLeftToRecoverBringsTheProducerUp(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.bothUp(1)
	f.open(2, prematch)
	f.m.channelLost(sid(2))
	f.m.channelReopened(sid(2))
	f.alive(pre)
	r := f.lastSnapshot(pre)
	// the session it was for closes while it is in flight, then it is refused
	f.close(2)
	expectDown(t, f, pre, true, "while it is in flight")
	f.refuse(r)
	expectDown(t, f, pre, false, "nothing left missing")
}

// RecoveryMachineTest: aProducerIsUpForItsFirstRecoveryTheFirstTimeItComesUpAfterAFollowUp
func TestMachine_ProducerIsUpForItsFirstRecoveryTheFirstTimeItComesUpAfterAFollowUp(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.alive(pre)
	first := f.lastSnapshot(pre)
	// the producer stops sending while the first recovery waits for the API
	f.unsubscribed(pre)
	f.accept(first)
	f.snapshotComplete(1, pre, first.requestID)
	followUp := f.lastSnapshot(pre)
	expect(t, followUp.requestID != first.requestID, true, "a follow-up")
	f.complete(followUp, 1)
	var ups []statusCause
	for _, c := range f.statusesOf(pre) {
		if !c.down {
			ups = append(ups, c.cause)
		}
	}
	expect(t, len(ups), 1, "times up")
	expect(t, ups[0], causeFirstRecoveryCompleted, "cause")
}

// RecoveryMachineTest: aProducerIsUpForItsFirstRecoveryWhenAClosedSessionBringsItUp
func TestMachine_ProducerIsUpForItsFirstRecoveryWhenAClosedSessionBringsItUp(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.alive(pre)
	first := f.lastSnapshot(pre)
	f.accept(first)
	f.open(2, prematch)
	f.snapshotComplete(1, pre, first.requestID)
	// what session 2 misses is asked for, refused, and session 2 closes
	f.refuse(f.lastSnapshot(pre))
	f.close(2)
	up := f.lastStatus(pre)
	expect(t, up.down, false, "down")
	expect(t, up.cause, causeFirstRecoveryCompleted, "cause")
	expect(t, up.reason(), types.FirstRecoveryCompletedProducerStatusReason, "reason")
}

// RecoveryMachineTest: theFirstSessionsSnapshotCompleteSaysTheApiTookARecoveryWhateverItAnswersLater
func TestMachine_FirstSessionsSnapshotCompleteSaysTheApiTookARecoveryWhateverItAnswersLater(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.open(2, all)
	f.startFeed()
	f.alive(pre)
	f.alive(live)
	refusedLate := f.lastSnapshot(pre)
	acceptedLate := f.lastSnapshot(live)
	f.snapshotComplete(1, pre, refusedLate.requestID)
	f.snapshotComplete(1, live, acceptedLate.requestID)
	f.refuse(refusedLate)
	f.accept(acceptedLate)
	expect(t, f.m.counters.failed, 0, "a refusal after the snapshot_complete")
	expectCalls(t, f, pre, 1, "nothing asked for again")
	expect(t, f.recoveryInfo(pre).Successful(), true, "recovery info")
	expectDown(t, f, pre, true, "waiting for session 2")
	f.snapshotComplete(2, pre, refusedLate.requestID)
	f.snapshotComplete(2, live, acceptedLate.requestID)
	expectDown(t, f, pre, false, "producer 1 after session 2")
	expectDown(t, f, live, false, "producer 2 after session 2")
}

// ---- what the client hears

// RecoveryMachineTest: anAliveSayingAProducerStillDownIsUnsubscribedRaisesOnlyTheCauseEvent
func TestMachine_AliveSayingAProducerStillDownIsUnsubscribedRaisesOnlyTheCauseEvent(t *testing.T) {
	// the producer is down for Other, and unsubscribed is Other too: nothing public changes
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.alive(pre)
	f.alive(pre)
	expect(t, len(f.out.statuses), 0, "alives change no status")
	f.unsubscribed(pre)
	expect(t, len(f.publicStatuses()), 0, "public events")
	got := causes(f.out.statuses)
	expect(t, len(got), 1, "cause events")
	expect(t, got[0], causeUnsubscribed, "cause")
	f.unsubscribed(pre)
	expect(t, len(f.out.statuses), 1, "the same cause again")
	expectCalls(t, f, pre, 1, "the recovery still asked for, and joined")
}

// Causes that share a public reason raise one public event and a cause event each:
// a lost connection and a lost channel are both ConnectionDown, and an alive that
// says unsubscribed after either is Other.
// RecoveryMachineTest: causesSharingAPublicReasonRaiseOnePublicEventAndACauseEventEach
func TestMachine_CausesSharingAPublicReasonRaiseOnePublicEventAndACauseEventEach(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.bothUp(1)
	seen := len(f.statusesOf(pre))

	f.m.connectionDown()
	f.m.channelLost(sid(1))
	f.unsubscribed(pre)
	changes := f.statusesOf(pre)[seen:]
	var public []statusCause
	for _, c := range changes {
		if c.public {
			public = append(public, c.cause)
		}
	}
	got := causes(changes)
	want := []statusCause{causeConnectionLost, causeChannelLost, causeUnsubscribed}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("cause events = %v, want %v", got, want)
	}
	if len(public) != 2 || public[0] != causeConnectionLost || public[1] != causeUnsubscribed {
		t.Fatalf("public events = %v, want the first of ConnectionDown and the change to Other", public)
	}
	expect(t, changes[1].reason(), types.ConnectionDownProducerStatusReason, "a lost channel after a lost connection")
	expect(t, changes[2].reason(), types.OtherProducerStatusReason, "unsubscribed")
}

// RecoveryMachineTest: aDisabledProducerIsNotRecoveredAndAnUnknownOneIsCounted
func TestMachine_DisabledProducerIsNotRecoveredAndAnUnknownOneIsCounted(t *testing.T) {
	f := newFeed(t)
	f.cat.disabled[live] = true
	f.open(1, all)
	f.startFeed()
	f.alive(live)
	f.unsubscribed(live)
	f.m.alive(9, f.now(), f.now(), f.stamp(), true)
	f.m.processed(sid(1), 9, f.now(), f.now(), f.stamp(), 0)
	expect(t, len(f.out.calls), 0, "requests")
	expect(t, len(f.out.statuses), 0, "status changes")
	expect(t, f.m.counters.unknownProducers, 2, "unknown producers")
}

// ---- Q4: a rewind of the recovery timestamp after the start

func TestMachine_RewindAfterStartOpensNoRequestOnItsOwn(t *testing.T) {
	f := upWith(t, all)
	f.clock.advance(time.Minute)
	f.live(1, pre, 0)
	calls := len(f.out.calls)
	f.m.rewind(pre, f.now().Add(-time.Hour))
	f.alive(pre)
	expect(t, len(f.out.calls), calls, "requests after a rewind alone")
	expectDown(t, f, pre, false, "the producer")
}

func TestMachine_RewindFoldsIntoTheNextRequestAndTheResumePointAndClosesOnACoveringCompletion(t *testing.T) {
	f := upWith(t, all)
	f.clock.advance(time.Minute)
	f.live(1, pre, 0)
	rewound := f.now().Add(-time.Hour)
	f.m.rewind(pre, rewound)
	got, _ := f.resume(pre)
	expectTime(t, got, rewound, "the resume point after the rewind")

	// a request that something else asks for starts at the rewind
	f.m.connectionDown()
	f.m.connectionUp()
	f.alive(pre)
	r := f.lastSnapshot(pre)
	expectTime(t, r.after, rewound, "the next request starts at the rewind")
	// a failed request keeps it
	f.refuse(r)
	got, _ = f.resume(pre)
	expectTime(t, got, rewound, "the resume point after a refusal")
	expect(t, f.m.byID[pre].rewind != nil, true, "the rewind after a refusal")

	f.clock.advance(5 * time.Second)
	f.alive(pre)
	again := f.lastSnapshot(pre)
	expectTime(t, again.after, rewound, "the next request after a refusal")
	f.complete(again, 1)
	expect(t, f.m.byID[pre].rewind == nil, true, "the rewind after a covering completion")
	got, _ = f.resume(pre)
	expectTime(t, got, f.now(), "the resume point after the completion")
}

func TestMachine_RewindIsKeptByARecoveryAskedForBeforeIt(t *testing.T) {
	f := newFeed(t)
	f.cat.from[pre] = f.now().Add(-2 * time.Hour)
	f.open(1, all)
	f.startFeed()
	f.alive(pre)
	first := f.lastSnapshot(pre)
	f.accept(first)
	rewound := f.now().Add(-3 * time.Hour)
	f.m.rewind(pre, rewound)
	f.snapshotComplete(1, pre, first.requestID)
	expect(t, f.m.byID[pre].rewind != nil, true, "the rewind after a recovery that did not cover it")
	got, _ := f.resume(pre)
	expectTime(t, got, rewound, "the resume point")
}

func TestMachine_RewindToALaterPointIsIgnoredWithAWarning(t *testing.T) {
	f := upWith(t, all)
	f.clock.advance(time.Minute)
	f.live(1, pre, 0)
	f.logs.take()
	f.m.rewind(pre, f.now().Add(time.Hour))
	if f.m.byID[pre].rewind != nil {
		t.Fatal("a later point was taken")
	}
	lines := f.logs.take()
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "WARN The recovery timestamp of producer 1") {
		t.Fatalf("log = %v, want one warning", lines)
	}
	got, _ := f.resume(pre)
	expectTime(t, got, f.now(), "the resume point")
}

func TestMachine_RewindBeforeTheStartIsLeftToTheSeed(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.m.rewind(pre, f.now().Add(-time.Hour))
	if f.m.byID[pre].rewind != nil {
		t.Fatal("a rewind before the start was taken")
	}
}

// ---- shutdown, first lanes and late tracks

// A no-lane producer is not brought up while the feed closes: nothing is up for a
// session that has closed at shutdown.
// RecoveryMachineTest: sessionsClosingAtShutdownCompleteNoRecoveryAndBringNoProducerUp
func TestMachine_AliveAfterTheLastSessionClosedAtShutdownBringsNoProducerUp(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.alive(pre)
	_ = f.lastSnapshot(pre) // asked for, never completed
	f.m.beginClosing()
	f.close(1)
	expectDown(t, f, pre, true, "after the sessions closed at shutdown")
	f.clock.advance(5 * time.Second)
	f.alive(pre)
	expectDown(t, f, pre, true, "after another alive while the feed closes")
}

// A producer that is up with no lane goes down when a session opens whose lane gets
// the seed gap, until that recovery completes.
func TestMachine_ProducerUpWithNoLaneIsDownWhileTheNewSessionsSeedRecoveryRuns(t *testing.T) {
	f := newFeed(t)
	f.open(1, aliveOne)
	f.startFeed()
	f.alive(pre)
	expectDown(t, f, pre, false, "no-lane producer up")
	f.clock.advance(time.Minute)
	f.open(2, prematch)
	expectDown(t, f, pre, true, "as the new session opens")
	down := f.lastStatus(pre)
	expect(t, down.reason(), types.OtherProducerStatusReason, "reason")
	f.alive(pre)
	r := f.lastSnapshot(pre)
	expectDown(t, f, pre, true, "while the seed recovery of the new session is in flight")
	f.complete(r, 2)
	expectDown(t, f, pre, false, "after it")
	expect(t, f.lastStatus(pre).cause, causeRecoveryCompleted, "cause")
}

// A producer gap opened while no lane received the producer does not drive the
// recovery of a session that opens later: that session starts now, with no gap.
func TestMachine_ProducerGapOfTheNoLanePeriodIsDroppedWhenTheProducerComesUp(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.bothUp(1)
	f.close(1) // last lane gone
	f.clock.advance(time.Second)
	f.unsubscribed(pre)
	f.clock.advance(time.Hour)
	f.alive(pre) // no-lane up
	expectDown(t, f, pre, false, "up")
	calls := len(f.snapshots(pre))
	f.open(2, prematch)
	f.alive(pre)
	expect(t, len(f.snapshots(pre)), calls, "recoveries for a session that starts now")
	expectDown(t, f, pre, false, "the producer")
}

// The same rewind point set again, while a recovery that covers it is in flight,
// does not outlive that recovery.
func TestMachine_RewindSetAgainToTheSamePointDoesNotOutliveTheRecoveryThatCoveredIt(t *testing.T) {
	f := upWith(t, all)
	f.clock.advance(time.Minute)
	f.live(1, pre, 0)
	rewound := f.now().Add(-time.Hour)
	f.m.rewind(pre, rewound)
	f.m.connectionDown()
	f.m.connectionUp()
	f.alive(pre)
	r := f.lastSnapshot(pre)
	expectTime(t, r.after, rewound, "the request starts at the rewind")
	f.accept(r)
	f.m.rewind(pre, rewound) // the client sets the same value again
	f.snapshotComplete(1, pre, r.requestID)
	f.clock.advance(time.Minute)
	f.live(1, pre, 0)
	got, _ := f.resume(pre)
	expectTime(t, got, f.now(), "the resume point after the recovery that started at the rewind")
}

// A second, earlier rewind lowers the start of the next request.
func TestMachine_SecondEarlierRewindLowersTheStartOfTheNextRequest(t *testing.T) {
	f := upWith(t, all)
	f.clock.advance(time.Minute)
	f.live(1, pre, 0)
	f.m.rewind(pre, f.now().Add(-time.Hour))
	earlier := f.now().Add(-2 * time.Hour)
	f.m.rewind(pre, earlier)
	f.m.rewind(pre, f.now().Add(-90*time.Minute)) // later than the lowest: no change
	f.m.connectionDown()
	f.m.connectionUp()
	f.alive(pre)
	expectTime(t, f.lastSnapshot(pre).after, earlier, "the next request's start")
}

// A request id of 0 stands for a live message: the machine draws again.
func TestMachine_RequestIdOfZeroIsRefusedAndDrawnAgain(t *testing.T) {
	f := newFeed(t)
	drawn := []int{0, 0, 5}
	f.m.nextID = func() int {
		id := drawn[0]
		drawn = drawn[1:]
		return id
	}
	f.open(1, all)
	f.startFeed()
	f.alive(pre)
	expect(t, f.lastSnapshot(pre).requestID, 5, "the request id")
	expect(t, len(drawn), 0, "ids drawn")
}

// ---- a no-lane producer: what holds it back

// a: a producer no session has a lane for is not taken down by a lost connection.
func TestMachine_NoLaneProducerIsNotTakenDownByALostConnection(t *testing.T) {
	f := newFeed(t)
	f.open(1, aliveOne)
	f.startFeed()
	f.alive(pre)
	f.m.connectionDown()
	expectDown(t, f, pre, false, "after the connection went down")
	f.m.connectionUp()
	expectDown(t, f, pre, false, "after it is back")
}

// b: the no-lane up needs the feed started...
func TestMachine_NoLaneProducerDoesNotComeUpBeforeTheStart(t *testing.T) {
	f := newFeed(t)
	f.open(1, aliveOne)
	f.m.connectionUp()
	f.alive(pre)
	expectDown(t, f, pre, true, "an alive before the start")
}

// b: ... and the transport connected.
func TestMachine_NoLaneProducerDoesNotComeUpBeforeTheConnectionIsUp(t *testing.T) {
	f := newFeed(t)
	f.open(1, aliveOne)
	f.m.start()
	f.alive(pre)
	expectDown(t, f, pre, true, "an alive before the connection is up")
	f.m.connectionUp()
	f.clock.advance(10 * time.Second)
	f.alive(pre)
	expectDown(t, f, pre, false, "the next alive")
}

// c: a producer disabled after its alive is not asked for when the connection comes up.
func TestMachine_ProducerDisabledBeforeTheConnectionComesUpIsNotRequested(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.m.start()
	f.alive(pre)
	f.cat.disabled[pre] = true
	f.m.connectionUp()
	expect(t, len(f.out.calls), 0, "requests")
}

// e: a lost session that does not receive the producer does not hold its requests.
func TestMachine_LostChannelOfASessionNotReceivingTheProducerHoldsNothingOfIt(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.open(2, liveOnly)
	f.startFeed()
	f.bothUp(1, 2)
	f.m.channelLost(sid(2))
	f.unsubscribed(pre)
	expectCalls(t, f, pre, 2, "the prematch producer is asked for at once")
}

// ---- the first recovery needs a witness

// A recovery the API accepted whose only awaited session closed before its
// snapshot_complete has no witness: the producer comes up, but it is not
// recovered, and a session opened next starts from the client's seed.
func TestMachine_RecoveryWhoseOnlySessionClosedBeforeItsSnapshotCompleteLeavesTheProducerUnrecovered(t *testing.T) {
	f := newFeed(t)
	from := f.now().Add(-time.Hour)
	f.cat.from[pre] = from
	f.open(1, prematch)
	f.startFeed()
	f.alive(pre)
	r := f.lastSnapshot(pre)
	f.accept(r)
	f.close(1)
	expectDown(t, f, pre, false, "nothing is missing once the only session is gone")

	f.clock.advance(5 * time.Second)
	f.open(2, prematch)
	cp, _ := f.checkpoint(2, pre)
	expectTime(t, cp, from, "the new session starts at the seed")
	expectDown(t, f, pre, true, "the new session has a recovery to make")
	expect(t, f.lastStatus(pre).cause, causeSessionOpened, "cause")
	f.alive(pre)
	expectTime(t, f.lastSnapshot(pre).after, from, "the recovery starts at the seed")
	expectCalls(t, f, pre, 2, "recoveries asked for")
}

// The same with the new session opening before the old one closes: both orders
// agree.
func TestMachine_SessionOpenedBeforeTheOnlyWitnessClosesAlsoStartsAtTheSeed(t *testing.T) {
	f := newFeed(t)
	from := f.now().Add(-time.Hour)
	f.cat.from[pre] = from
	f.open(1, prematch)
	f.startFeed()
	f.alive(pre)
	f.accept(f.lastSnapshot(pre))
	f.clock.advance(5 * time.Second)
	f.open(2, prematch)
	f.close(1)
	cp, _ := f.checkpoint(2, pre)
	expectTime(t, cp, from, "the new session starts at the seed")
	expectDown(t, f, pre, true, "the new session has a recovery to make")
	expect(t, f.lastStatus(pre).cause, causeSessionOpened, "cause")
	f.alive(pre)
	expectTime(t, f.lastSnapshot(pre).after, from, "the recovery starts at the seed")
	expectCalls(t, f, pre, 2, "recoveries asked for")
}

// A snapshot_complete seen by a session that is still open is a witness: a session
// opened next starts at the producer's now, with no gap.
func TestMachine_RecoverySeenByAnOpenSessionLetsALaterSessionStartWithNoGap(t *testing.T) {
	f := newFeed(t)
	f.cat.from[pre] = f.now().Add(-time.Hour)
	f.open(1, prematch)
	f.startFeed()
	f.alive(pre)
	f.complete(f.lastSnapshot(pre), 1)
	f.clock.advance(5 * time.Second)
	f.open(2, prematch)
	cp, _ := f.checkpoint(2, pre)
	expectTime(t, cp, f.now(), "the new session starts at the producer's now")
	expectDown(t, f, pre, false, "the producer")
	expectCalls(t, f, pre, 1, "recoveries asked for")
}

// ---- closing

// Once the feed is closing, a request refused with no gap left recovered nothing:
// no producer comes up for it.
func TestMachine_RefusedRequestAfterTheSessionsClosedAtShutdownBringsNoProducerUp(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.bothUp(1)
	f.m.connectionDown()
	f.m.connectionUp()
	r := f.lastSnapshot(pre)
	f.m.beginClosing()
	f.close(1)
	told := len(f.out.statuses)

	f.refuse(r)
	expectDown(t, f, pre, true, "after the refusal")
	expect(t, len(f.out.statuses), told, "status changes")
}

// Nor does the connection coming back bring one up while the feed is closing.
func TestMachine_ConnectionBackAfterTheSessionsClosedAtShutdownBringsNoProducerUp(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.bothUp(1)
	f.m.connectionDown()
	f.m.beginClosing()
	f.close(1)
	told := len(f.out.statuses)

	f.m.connectionUp()
	expectDown(t, f, pre, true, "the connection is back")
	expect(t, len(f.out.statuses), told, "status changes")
}

// ---- the lane's checkpoint at the opening of a session

// A session is opened with the reading taken before its queue was bound: a message
// in the queue stamped after that reading and before the handling is not older
// than the checkpoint, so a loss of the channel asks from the reading, not from
// the handling.
func TestMachine_SessionOpenedWithAnEarlierReadingStartsItsLaneAtTheReading(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.bothUp(1)
	f.clock.advance(time.Minute)
	f.alive(pre)
	reading := f.stamp()
	f.clock.advance(10 * time.Second)

	f.openAt(2, prematch, reading)
	cp, _ := f.checkpoint(2, pre)
	expectTime(t, cp, reading.Wall(), "the lane starts at the reading, not at the handling")
	expectDown(t, f, pre, false, "no gap yet")

	f.m.channelLost(sid(2))
	f.m.channelReopened(sid(2))
	f.alive(pre)
	r := f.lastSnapshot(pre)
	expectTime(t, r.after, reading.Wall(), "the recovery starts at the reading")
}

// ---- the answer to an earlier request

// The answer to a request that was given up leaves the one asked for since alone.
func TestMachine_RefusalOfAnEarlierRequestLeavesTheNewerOneAlone(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.alive(pre)
	first := f.lastSnapshot(pre)
	f.m.connectionDown()
	f.m.connectionUp()
	second := f.lastSnapshot(pre)
	if second.requestID == first.requestID {
		t.Fatal("the second request has the id of the first")
	}
	failed := f.m.counters.failed
	infos := len(f.out.infos)

	f.refuse(first)
	expect(t, f.inFlight(pre), second.requestID, "the request in flight")
	expect(t, f.m.counters.failed, failed, "failures counted")
	expect(t, f.m.byID[pre].failures, 0, "failures in a row")
	expect(t, f.m.byID[pre].retryAt, time.Duration(0), "backoff")
	expect(t, len(f.out.infos), infos, "recovery infos published")
	expectCalls(t, f, pre, 2, "recoveries asked for")
}

// So does a late success: it does not mark the newer request accepted.
func TestMachine_LateSuccessOfAnEarlierRequestDoesNotAcceptTheNewerOne(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.alive(pre)
	first := f.lastSnapshot(pre)
	f.m.connectionDown()
	f.m.connectionUp()
	second := f.lastSnapshot(pre)
	infos := len(f.out.infos)

	f.accept(first)
	expect(t, f.inFlight(pre), second.requestID, "the request in flight")
	expect(t, f.m.byID[pre].active.accepted, false, "the newer request accepted")
	expect(t, len(f.out.infos), infos, "recovery infos published")
	f.complete(second, 1)
	expectDown(t, f, pre, false, "after the newer one completes")
}

// ---- a late producer and a session that does not receive it

// A producer the catalog adds after the machine was made gets no lane in a
// session whose interest leaves it out, and the recovery of it does not wait for
// that session.
func TestMachine_LateTrackGivesNoLaneToASessionWhoseInterestExcludesTheProducer(t *testing.T) {
	f := newFeed(t)
	f.open(1, prematch)
	f.open(2, liveOnly)
	f.startFeed()
	late := fakeProducer{id: 9, name: "late", scopes: []types.ProducerScope{types.LiveProducerScope}, window: statefulWindow}
	f.cat.list = append(f.cat.list, late)

	f.alive(9)
	_, received := f.checkpoint(1, 9)
	expect(t, received, false, "the prematch session receives the live producer")
	_, received = f.checkpoint(2, 9)
	expect(t, received, true, "the live session receives it")
	r := f.lastSnapshot(9)
	f.complete(r, 2)
	expectDown(t, f, 9, false, "completed without the prematch session's snapshot_complete")
}

// ---- the resume points published

// A resume point is published when it moves, not again while it stands.
func TestMachine_ResumePointIsPublishedOnlyWhenItMoves(t *testing.T) {
	f := upWith(t, all)
	f.m.publishResumePoints()
	published := len(f.out.pointsOf(pre))
	expect(t, published, 1, "publications after the first call")
	f.m.publishResumePoints()
	expect(t, len(f.out.pointsOf(pre)), published, "publications with nothing moved")

	f.clock.advance(time.Minute)
	f.live(1, pre, 0)
	f.m.publishResumePoints()
	expect(t, len(f.out.pointsOf(pre)), published+1, "publications after the checkpoint moved")
	got := f.out.pointsOf(pre)
	expectTime(t, got[len(got)-1], f.now(), "the new point")
}
