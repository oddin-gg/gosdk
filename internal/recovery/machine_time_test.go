package recovery

import (
	"strings"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/types"
)

// The rules that need the clock to run, and the clock's own: durations run on the
// monotonic side, what is published on the wall side. The two sides of the fake
// clock start at different numbers and a test can move them apart: a step of the
// wall clock, forward or back, must neither fire a deadline nor hold one back,
// and the monotonic clock moving past a limit, the wall one standing still, must
// fire it.

// advance moves both sides of the clock and ticks the machine once at the end, as
// the actor does at least once a second.
func (f *feed) advance(d time.Duration) {
	f.clock.advance(d)
	f.m.tick()
}

// stepWall moves the wall clock only, as a step of the system's clock does, and
// ticks the machine.
func (f *feed) stepWall(d time.Duration) {
	f.clock.stepWall(d)
	f.m.tick()
}

// advanceMono moves the monotonic clock only, and ticks the machine.
func (f *feed) advanceMono(d time.Duration) {
	f.clock.advanceMono(d)
	f.m.tick()
}

// runWithAlives moves the clock second by second, ticking each time, with an
// alive of both producers every ten seconds of the clock, on the alive channel and
// in every session's queue.
func (f *feed) runWithAlives(total time.Duration) {
	for range int(total / time.Second) {
		f.clock.advance(time.Second)
		if int(f.now().Sub(f.start)/time.Second)%10 == 0 {
			for _, p := range []int{preProducer, liveProducer} {
				f.alive(p)
				for _, s := range f.opened {
					f.sessionAlive(s, p)
				}
			}
		}
		f.m.tick()
	}
}

// monoSeconds moves the monotonic clock alone, second by second, ticking each time,
// with an alive of both producers every ten of those seconds.
func (f *feed) monoSeconds(n int) {
	for range n {
		f.advanceMono(time.Second)
		f.monoCount++
		if f.monoCount%10 == 0 {
			f.alive(pre)
			f.alive(live)
		}
	}
}

// withSettings is a feed whose default settings were changed.
func withSettings(t *testing.T, change func(*recoverySettings)) *feed {
	t.Helper()
	s := defaultSettings()
	change(&s)
	return newFeedWith(t, s, statefulWindow)
}

// backoffs are the pauses before the three re-issues of a failed recovery.
var backoffs = []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second}

// producerWentDown is the log line of a producer going down.
const producerWentDown = "INFO Producer 1 down: "

// ---- a producer that stops sending

// RecoveryMachineTest: aProducerGoingDownIsAnInfoLine
func TestMachineTime_ProducerGoingDownIsAnInfoLine(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.bothUp(1)
	f.logs.take()
	for range 21 {
		f.advance(time.Second)
	}
	for _, line := range f.logs.take() {
		if strings.HasPrefix(line, producerWentDown) {
			return
		}
	}
	t.Fatal("no info line of the producer going down")
}

// RecoveryMachineTest: aProducerWithoutAlivesForTooLongIsDownUntilAnAliveBringsARecovery
func TestMachineTime_ProducerWithoutAlivesForTooLongIsDownUntilAnAliveBringsARecovery(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.bothUp(1)
	lastAlive := f.now()
	for range 20 {
		f.advance(time.Second)
	}
	expectDown(t, f, pre, false, "at the maximum inactivity")
	f.advance(time.Second)
	down := f.lastStatus(pre)
	expect(t, down.cause, causeAliveIntervalViolation, "cause")
	expect(t, down.reason(), types.AliveIntervalViolationProducerStatusReason, "reason")
	expectCalls(t, f, pre, 1, "no recovery without an alive")

	f.alive(pre)
	r := f.lastSnapshot(pre)
	expectTime(t, r.after, lastAlive, "from the last alive")
	f.complete(r, 1)
	expect(t, f.lastStatus(pre).reason(), types.ReturnedFromInactivityProducerStatusReason, "reason when it is back")
}

// Q14: a producer with no alive within the maximum inactivity of the start is
// reported down with the alive-violation reason, not left silently down.
// RecoveryMachineTest: aProducerThatNeverSentAnAliveIsReportedDownOnceTheMaximumInactivityHasPassed
func TestMachineTime_ProducerThatNeverSentAnAliveIsReportedDownOnceTheMaximumInactivityHasPassed(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	for range 20 {
		f.advance(time.Second)
	}
	expect(t, len(f.out.statuses), 0, "status changes at the maximum inactivity")
	f.advance(time.Second)
	got := causes(f.out.statuses)
	if len(got) != 2 || got[0] != causeAliveIntervalViolation || got[1] != causeAliveIntervalViolation {
		t.Fatalf("causes = %v, want the violation for each of the two producers", got)
	}
	for _, c := range f.out.statuses {
		expect(t, c.reason(), types.AliveIntervalViolationProducerStatusReason, "reason")
		expect(t, c.public, true, "public")
	}
	// nothing was received to miss: the start's own gap stands
	f.alive(pre)
	expectTime(t, f.lastSnapshot(pre).after, time.Time{}, "the recovery's start")
}

// RecoveryMachineTest: aProducerSilentDuringARecoveryGivesItUpAndIsAskedAgainAtItsNextAlive
func TestMachineTime_ProducerSilentDuringARecoveryGivesItUpAndIsAskedAgainAtItsNextAlive(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.bothUp(1)
	f.unsubscribed(pre)
	lost := f.lastSnapshot(pre)
	f.accept(lost)
	for range 21 {
		f.advance(time.Second)
	}
	expect(t, f.lastStatus(pre).cause, causeAliveIntervalViolation, "cause")
	expect(t, f.inFlight(pre), 0, "given up")
	expect(t, f.m.counters.abandoned, 1, "abandoned")
	expect(t, f.m.counters.failed, 0, "failed")
	expectCalls(t, f, pre, 2, "not without an alive")

	f.alive(pre)
	again := f.lastSnapshot(pre)
	expect(t, again.requestID != lost.requestID, true, "a new request")
	expectTime(t, again.after, lost.after, "from the same point")
	f.complete(again, 1)
	expectDown(t, f, pre, false, "after it")
}

// RecoveryMachineTest: aRecoveryAskedForAgainStartsFromTheSamePoint
func TestMachineTime_RecoveryAskedForAgainStartsFromTheSamePoint(t *testing.T) {
	f := newFeed(t)
	from := f.now().Add(-time.Hour)
	f.cat.from[pre] = from
	f.open(1, prematch)
	f.startFeed()
	f.alive(pre)
	first := f.lastSnapshot(pre)
	f.accept(first)
	// live messages and alives go on while the snapshot comes, and some of it arrives
	at := f.now()
	f.processed(1, pre, at.Add(-time.Second), first.requestID)
	f.processed(1, pre, at, 0)
	f.sessionAlive(1, pre)
	cp, _ := f.checkpoint(1, pre)
	expectTime(t, cp, at, "the checkpoint")

	// the rest never comes: past the deadline for its snapshot_complete it is asked for again
	f.runWithAlives(5*time.Minute + 6*time.Second)
	again := f.lastSnapshot(pre)
	expect(t, again.requestID != first.requestID, true, "a new request")
	expect(t, f.m.counters.timedOut, 1, "timed out")
	expectTime(t, again.after, first.after, "the same start")
	expectTime(t, again.after, from, "the client's start")
}

// ---- the clamp, with the clock running

// RecoveryMachineTest: theReissuesOfARecoveryClampedToTheWindowAreNoMoreWarnings
func TestMachineTime_ReissuesOfARecoveryClampedToTheWindowAreNoMoreWarnings(t *testing.T) {
	f := newFeed(t)
	from := f.now().Add(-3*24*time.Hour + time.Minute)
	f.cat.from[pre] = from
	f.open(1, all)
	f.startFeed()
	f.clock.advance(time.Hour)
	f.logs.take()
	f.alive(pre)
	first := f.lastSnapshot(pre)
	f.refuse(first)
	f.advance(5 * time.Second)
	again := f.lastSnapshot(pre)
	expect(t, again.requestID != first.requestID, true, "asked for again")
	expectTime(t, again.after, first.after.Add(5*time.Second), "clamped again, the window moved on")
	var clamps []string
	for _, line := range f.logs.take() {
		if strings.Contains(line, "stateful recovery window") {
			clamps = append(clamps, line)
		}
	}
	want := []string{clampLine("WARN", first, from), clampLine("DEBUG", again, from)}
	if len(clamps) != 2 || clamps[0] != want[0] || clamps[1] != want[1] {
		t.Fatalf("clamp lines = %q, want one warning for the gap, then the same said at debug: %q", clamps, want)
	}
}

// RecoveryMachineTest: aNewGapClampedToTheWindowIsAWarningOfItsOwn
func TestMachineTime_NewGapClampedToTheWindowIsAWarningOfItsOwn(t *testing.T) {
	f := newFeed(t)
	from := f.now().Add(-3*24*time.Hour + time.Minute)
	f.cat.from[pre] = from
	f.open(1, all)
	f.startFeed()
	f.clock.advance(time.Hour)
	f.logs.take()
	f.alive(pre)
	first := f.lastSnapshot(pre)
	f.complete(first, 1)
	// the producer is quiet for four days: the gap its next alive finds began, at its
	// last alive, before the window
	lastAlive := f.now()
	f.advance(4 * 24 * time.Hour)
	f.alive(pre)
	second := f.lastSnapshot(pre)
	expect(t, second.requestID != first.requestID, true, "a second request")
	var clamps []string
	for _, line := range f.logs.take() {
		if strings.Contains(line, "stateful recovery window") {
			clamps = append(clamps, line)
		}
	}
	want := []string{clampLine("WARN", first, from), clampLine("WARN", second, lastAlive)}
	if len(clamps) != 2 || clamps[0] != want[0] || clamps[1] != want[1] {
		t.Fatalf("clamp lines = %q, want a warning for each gap: %q", clamps, want)
	}
}

// RecoveryMachineTest: aProducerWithNoStatefulWindowClampsNothing
func TestMachineTime_ProducerWithNoStatefulWindowClampsNothing(t *testing.T) {
	f := newFeedWith(t, defaultSettings(), 0)
	f.open(1, all)
	f.startFeed()
	f.alive(pre)
	f.complete(f.lastSnapshot(pre), 1)
	// quiet for ten days: the gap its next alive finds began at its last alive
	lastAlive := f.now()
	f.advance(10 * 24 * time.Hour)
	f.logs.take()
	f.alive(pre)
	expectCalls(t, f, pre, 2, "recoveries")
	expectTime(t, f.lastSnapshot(pre).after, lastAlive, "from where the gap began, however long ago")
	for _, line := range f.logs.take() {
		if strings.Contains(line, "stateful recovery window") {
			t.Fatalf("a clamp was logged: %s", line)
		}
	}
}

// RecoveryMachineTest: aColdStartWithAnInitialSnapshotIntervalAsksThatFarBack
func TestMachineTime_ColdStartWithAnInitialSnapshotIntervalAsksThatFarBackAndAskingAgainKeepsIt(t *testing.T) {
	f := withSettings(t, func(s *recoverySettings) { s.initialSnapshot = 30 * time.Minute })
	f.open(1, all)
	f.startFeed()
	boundary := f.now().Add(-30 * time.Minute)
	f.clock.advance(3 * time.Second)
	f.alive(pre)
	expectTime(t, f.lastSnapshot(pre).after, boundary, "from the interval before the feed opened")

	// asked for again a minute later: the boundary stays where it was
	f.refuse(f.lastSnapshot(pre))
	f.runWithAlives(time.Minute)
	expectCalls(t, f, pre, 2, "recoveries")
	expectTime(t, f.lastSnapshot(pre).after, boundary, "the boundary stays")
}

// ---- failures, backoff and the slow retry

// Q8: backoff 5 s, 10 s, 20 s and three re-issues, then a slow retry every 60 s.
// RecoveryMachineTest: aFailedRecoveryIsAskedForAgainWithBackoffThreeTimesThenNotUntilTheCooldown
func TestMachineTime_FailedRecoveryIsAskedForAgainWithBackoffThreeTimesThenEverySixtySeconds(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.alive(pre)
	for _, pause := range backoffs {
		f.refuse(f.lastSnapshot(pre))
		asked := len(f.snapshots(pre))
		f.runWithAlives(pause - time.Second)
		expect(t, len(f.snapshots(pre)), asked, "recoveries before the pause of "+pause.String())
		f.runWithAlives(time.Second)
		expect(t, len(f.snapshots(pre)), asked+1, "recoveries after the pause of "+pause.String())
	}
	f.refuse(f.lastSnapshot(pre))
	failed := f.lastStatus(pre)
	expect(t, failed.cause, causeRecoveryFailed, "cause")
	expect(t, failed.down, true, "down")
	expect(t, failed.reason(), types.OtherProducerStatusReason, "the public reason")
	expect(t, f.m.counters.failed, 4, "failed")
	expect(t, f.m.counters.reissued, 3, "reissued")

	// not the ten minutes of a cool-down: the slow retry, steadily, for as long as it fails
	for round := 1; round <= 3; round++ {
		asked := len(f.snapshots(pre))
		f.runWithAlives(59 * time.Second)
		expect(t, len(f.snapshots(pre)), asked, "recoveries 59 s after the failure, round "+string(rune('0'+round)))
		f.runWithAlives(time.Second)
		expect(t, len(f.snapshots(pre)), asked+1, "recoveries 60 s after the failure, round "+string(rune('0'+round)))
		f.refuse(f.lastSnapshot(pre))
	}
	expect(t, f.lastStatus(pre).cause, causeRecoveryFailed, "the producer stays down for the failure")
	expect(t, len(f.statusesOf(pre)), 1, "status changes: no more than the one")

	// the API is back: the next try completes it and the producer is up at once
	f.runWithAlives(time.Minute)
	f.complete(f.lastSnapshot(pre), 1)
	expectDown(t, f, pre, false, "after a success")
	expect(t, f.lastStatus(pre).cause, causeFirstRecoveryCompleted, "cause")
}

// A success after the slow retry starts the backoff over for the next failure.
func TestMachineTime_SuccessAfterTheSlowRetryStartsTheBackoffOver(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.alive(pre)
	for range 4 {
		f.refuse(f.lastSnapshot(pre))
		f.runWithAlives(20 * time.Second)
	}
	f.runWithAlives(time.Minute)
	f.complete(f.lastSnapshot(pre), 1)
	f.unsubscribed(pre)
	f.refuse(f.lastSnapshot(pre))
	asked := len(f.snapshots(pre))
	f.runWithAlives(5 * time.Second)
	expect(t, len(f.snapshots(pre)), asked+1, "the first backoff again")
}

// RecoveryMachineTest: onlyTheFirstFailureOfAStreakIsAWarningAndTheCapSpentOneToo
func TestMachineTime_OnlyTheFirstFailureOfAStreakIsAWarningAndTheOneThatSpendsTheCapToo(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.alive(pre)
	f.logs.take()
	for _, pause := range backoffs {
		f.refuse(f.lastSnapshot(pre))
		f.runWithAlives(pause)
	}
	f.refuse(f.lastSnapshot(pre))
	var failures []string
	for _, line := range f.logs.take() {
		if strings.Contains(line, "Recovery ") && strings.Contains(line, " failed, ") {
			failures = append(failures, line)
		}
	}
	expect(t, len(failures), 4, "failure lines")
	for i, want := range []struct{ level, part string }{
		{"WARN ", "asked for again in 5s"},
		{"DEBUG ", "asked for again in 10s"},
		{"DEBUG ", "asked for again in 20s"},
		{"WARN ", "the producer stays down and it is asked for every 1m0s"},
	} {
		if !strings.HasPrefix(failures[i], want.level) || !strings.Contains(failures[i], want.part) {
			t.Fatalf("failure line %d = %q, want %q ... %q", i, failures[i], want.level, want.part)
		}
	}
}

// RecoveryMachineTest: anAliveAfterAGapReArmsASpentCapAtOnce
func TestMachineTime_AliveAfterAGapReArmsASpentCapAtOnce(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.alive(pre)
	for range 4 {
		f.refuse(f.lastSnapshot(pre))
		f.runWithAlives(20 * time.Second)
	}
	expectCalls(t, f, pre, 4, "recoveries with the cap spent")
	expect(t, f.lastStatus(pre).cause, causeRecoveryFailed, "cause")

	for range 21 {
		f.advance(time.Second)
	}
	expect(t, f.lastStatus(pre).cause, causeAliveIntervalViolation, "cause after a gap")
	f.alive(pre)
	expectCalls(t, f, pre, 5, "recoveries after the alive: re-armed at once, not after the slow retry")
}

// RecoveryMachineTest: aProducerBroughtUpWithNothingMissingHasItsCapReArmed
func TestMachineTime_ProducerBroughtUpWithNothingMissingHasItsCapReArmed(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.bothUp(1)
	f.open(2, prematch)
	f.m.channelLost(sid(2))
	f.m.channelReopened(sid(2))
	f.alive(pre)
	for range 4 {
		f.refuse(f.lastSnapshot(pre))
		f.runWithAlives(20 * time.Second)
	}
	expect(t, f.lastStatus(pre).cause, causeRecoveryFailed, "cause")
	f.close(2)
	expectDown(t, f, pre, false, "after the session closed")
	asked := len(f.snapshots(pre))
	f.unsubscribed(pre)
	expect(t, len(f.snapshots(pre)), asked+1, "asked for at once, not after the slow retry")
}

// RecoveryMachineTest: aMaximumInactivityUnderASecondBreaksNoAlive
func TestMachineTime_MaximumInactivityUnderASecondBreaksNoAlive(t *testing.T) {
	f := withSettings(t, func(s *recoverySettings) { s.maxInactivity = 0 })
	f.open(1, all)
	f.startFeed()
	f.alive(pre)
	f.clock.advance(2 * time.Second)
	f.alive(pre)
	expectCalls(t, f, pre, 1, "asked for at the first alive")
	expect(t, f.m.byID[pre].lastAliveAt, f.stamp().mono, "the second alive handled")
}

// ---- the deadlines of a recovery

// RecoveryMachineTest: aRecoveryWhoseSnapshotCompleteIsLostIsAskedForAgainAfterItsDeadline
func TestMachineTime_RecoveryWhoseSnapshotCompleteIsLostIsAskedForAgainAfterItsDeadline(t *testing.T) {
	f := newFeed(t)
	f.open(1, prematch)
	f.startFeed()
	f.alive(pre)
	r := f.lastSnapshot(pre)
	f.accept(r)
	// the session takes the snapshot, and live messages after it; its snapshot_complete never comes
	f.processed(1, pre, f.now(), r.requestID)
	f.runWithAlives(5 * time.Minute)
	expectCalls(t, f, pre, 1, "at the deadline")
	expect(t, f.m.counters.timedOut, 0, "timed out at the deadline")
	f.runWithAlives(time.Second)
	expect(t, f.m.counters.timedOut, 1, "timed out")
	expect(t, f.recoveryInfo(pre).Successful(), false, "the timed-out one")
	f.runWithAlives(5 * time.Second)
	expectCalls(t, f, pre, 2, "after the first backoff")
	f.complete(f.lastSnapshot(pre), 1)
	expectDown(t, f, pre, false, "up within minutes")
}

// minutes runs the clock a minute at a time, n times, with what the session takes
// after each minute.
func (f *feed) minutes(n int, took func()) {
	for range n {
		f.runWithAlives(time.Minute)
		if took != nil {
			took()
		}
	}
}

// RecoveryMachineTest: aSnapshotStillOnItsWayPutsOffTheDeadlineForItsSnapshotComplete
func TestMachineTime_SnapshotStillOnItsWayPutsOffTheDeadlineForItsSnapshotComplete(t *testing.T) {
	f := newFeed(t)
	f.open(1, prematch)
	f.startFeed()
	f.alive(pre)
	r := f.lastSnapshot(pre)
	f.accept(r)
	requestedAt := f.now()
	// a slow session: first what was queued before the request, then the snapshot
	f.minutes(6, func() { f.processed(1, pre, requestedAt.Add(-time.Second), 0) })
	f.minutes(4, func() { f.processed(1, pre, f.now(), r.requestID) })
	expect(t, f.m.counters.timedOut, 0, "ten minutes in, still coming")
	f.runWithAlives(5 * time.Minute)
	expect(t, f.m.counters.timedOut, 0, "five minutes after the last of it")
	f.runWithAlives(time.Second)
	expect(t, f.m.counters.timedOut, 1, "a second later")
}

// RecoveryMachineTest: anAliveSentBeforeTheRequestPutsOffTheDeadlineForItsSnapshotComplete
func TestMachineTime_AliveSentBeforeTheRequestPutsOffTheDeadlineForItsSnapshotComplete(t *testing.T) {
	f := newFeed(t)
	f.open(1, prematch)
	f.startFeed()
	f.alive(pre)
	f.accept(f.lastSnapshot(pre))
	requestedAt := f.now()
	// a slow session takes nothing but alives from before the request
	f.minutes(6, func() {
		f.m.sessionAlive(sid(1), pre, requestedAt.Add(-time.Second), requestedAt.Add(-time.Second), f.stamp(), true)
	})
	expect(t, f.m.counters.timedOut, 0, "six minutes in, still coming")
	f.runWithAlives(5 * time.Minute)
	expect(t, f.m.counters.timedOut, 0, "five minutes after the last of it")
	f.runWithAlives(time.Second)
	expect(t, f.m.counters.timedOut, 1, "a second later")
}

// RecoveryMachineTest: aSessionsAliveWithoutAPositiveStampDoesNotPutOffTheDeadlineForItsSnapshotComplete
func TestMachineTime_SessionAliveWithoutAPositiveStampDoesNotPutOffTheDeadline(t *testing.T) {
	f := newFeed(t)
	f.open(1, prematch)
	f.startFeed()
	f.alive(pre)
	f.accept(f.lastSnapshot(pre))
	f.minutes(4, func() {
		f.m.sessionAlive(sid(1), pre, time.UnixMilli(0), time.UnixMilli(0), f.stamp(), true)
		f.m.sessionAlive(sid(1), pre, time.UnixMilli(-3_600_000), time.UnixMilli(-3_600_000), f.stamp(), true)
	})
	expect(t, f.m.counters.timedOut, 0, "four minutes in")
	f.runWithAlives(defaultSettings().snapshotCompleteTimeout - 4*time.Minute + time.Second)
	expect(t, f.m.counters.timedOut, 1, "the snapshot-complete timeout after the request")
}

// RecoveryMachineTest: theSnapshotOfARecoveryGivenUpPutsOffTheDeadlineOfTheNextOne
func TestMachineTime_SnapshotOfARecoveryGivenUpPutsOffTheDeadlineOfTheNextOne(t *testing.T) {
	f := newFeed(t)
	f.open(1, prematch)
	f.startFeed()
	f.alive(pre)
	f.complete(f.lastSnapshot(pre), 1)
	f.unsubscribed(pre)
	lost := f.lastSnapshot(pre)
	f.accept(lost)
	f.clock.advance(10 * time.Second)
	f.unsubscribed(pre)
	expect(t, f.m.counters.abandoned, 1, "abandoned")
	f.clock.advance(10 * time.Second)
	f.alive(pre)
	again := f.lastSnapshot(pre)
	expect(t, again.requestID != lost.requestID, true, "a new request")
	f.accept(again)
	// a slow session takes what the one given up had sent, which the new one's queues behind
	f.minutes(6, func() { f.processed(1, pre, f.now(), lost.requestID) })
	expect(t, f.m.counters.timedOut, 0, "six minutes in, still coming")
	f.runWithAlives(5 * time.Minute)
	expect(t, f.m.counters.timedOut, 0, "five minutes after the last of it")
	f.runWithAlives(time.Second)
	expect(t, f.m.counters.timedOut, 1, "a second later")
}

// RecoveryMachineTest: onlyTheLastEightRecoveriesThatEndedPutOffTheDeadline
func TestMachineTime_OnlyTheLastEightRecoveriesThatEndedPutOffTheDeadline(t *testing.T) {
	f := newFeed(t)
	f.open(1, prematch)
	f.startFeed()
	f.alive(pre)
	f.complete(f.lastSnapshot(pre), 1)
	ended := make([]int, 0, 9)
	// the producer restarts nine times, each time after it took the recovery
	for range 9 {
		f.clock.advance(10 * time.Second)
		f.unsubscribed(pre)
		lost := f.lastSnapshot(pre)
		f.accept(lost)
		f.clock.advance(10 * time.Second)
		f.unsubscribed(pre)
		ended = append(ended, lost.requestID)
	}
	expect(t, f.m.counters.abandoned, 9, "abandoned")
	f.alive(pre)
	again := f.lastSnapshot(pre)
	f.accept(again)
	// what the first of them sent no longer counts
	f.minutes(5, func() { f.processed(1, pre, f.now(), ended[0]) })
	expect(t, f.m.counters.timedOut, 0, "at the deadline")
	f.runWithAlives(time.Second)
	expect(t, f.m.counters.timedOut, 1, "five minutes after the request")

	// the timed-out one is remembered now, and the third of the nine is the oldest that counts
	f.runWithAlives(5 * time.Second)
	third := f.lastSnapshot(pre)
	expect(t, third.requestID != again.requestID, true, "asked for again")
	f.accept(third)
	f.minutes(6, func() { f.processed(1, pre, f.now(), ended[2]) })
	expect(t, f.m.counters.timedOut, 1, "six minutes in, still coming")
	f.runWithAlives(5 * time.Minute)
	expect(t, f.m.counters.timedOut, 1, "five minutes after the last of it")
	f.runWithAlives(time.Second)
	expect(t, f.m.counters.timedOut, 2, "a second later")
}

// RecoveryMachineTest: whatTwoRecoveriesGivenUpStillSendBothPutOffTheDeadlineOfTheNextOne
func TestMachineTime_WhatTwoRecoveriesGivenUpStillSendBothPutOffTheDeadlineOfTheNextOne(t *testing.T) {
	f := newFeed(t)
	f.open(1, prematch)
	f.startFeed()
	f.alive(pre)
	f.complete(f.lastSnapshot(pre), 1)
	lost := make([]int, 0, 2)
	for range 2 {
		f.clock.advance(10 * time.Second)
		f.unsubscribed(pre)
		r := f.lastSnapshot(pre)
		f.accept(r)
		f.clock.advance(10 * time.Second)
		f.unsubscribed(pre)
		lost = append(lost, r.requestID)
	}
	expect(t, f.m.counters.abandoned, 2, "abandoned")
	f.alive(pre)
	next := f.lastSnapshot(pre)
	f.accept(next)
	// a slow session takes what the first sent, then what the second sent, then the next one's
	for _, id := range lost {
		f.minutes(6, func() { f.processed(1, pre, f.now(), id) })
	}
	expect(t, f.m.counters.timedOut, 0, "twelve minutes in, still coming")
	f.runWithAlives(5 * time.Minute)
	expect(t, f.m.counters.timedOut, 0, "five minutes after the last of it")
	f.runWithAlives(time.Second)
	expect(t, f.m.counters.timedOut, 1, "a second later")
}

// RecoveryMachineTest: aRecoveryStillOnItsWayAtTheMaximumRecoveryTimeTimesOut
func TestMachineTime_RecoveryStillOnItsWayAtTheMaximumRecoveryTimeTimesOut(t *testing.T) {
	f := withSettings(t, func(s *recoverySettings) { s.maxRecoveryTime = 30 * time.Minute })
	f.open(1, prematch)
	f.startFeed()
	f.alive(pre)
	r := f.lastSnapshot(pre)
	f.accept(r)
	f.minutes(30, func() { f.processed(1, pre, f.now(), r.requestID) })
	expect(t, f.m.counters.timedOut, 0, "at the maximum recovery time")
	f.runWithAlives(time.Second)
	expect(t, f.m.counters.timedOut, 1, "a second later")

	// asked for again after the backoff, behind what the timed-out one still sends
	f.runWithAlives(5 * time.Second)
	again := f.lastSnapshot(pre)
	expect(t, again.requestID != r.requestID, true, "asked for again")
	f.accept(again)
	f.minutes(6, func() { f.processed(1, pre, f.now(), r.requestID) })
	expect(t, f.m.counters.timedOut, 1, "the next one, still behind it")
	f.runWithAlives(5*time.Minute + time.Second)
	expect(t, f.m.counters.timedOut, 2, "the next one timed out")
}

// RecoveryMachineTest: aSnapshotMessageStampedNoPositiveTimeStillPutsOffTheDeadlineForItsSnapshotComplete
func TestMachineTime_SnapshotMessageStampedNoPositiveTimeStillPutsOffTheDeadline(t *testing.T) {
	f := newFeed(t)
	f.open(1, prematch)
	f.startFeed()
	f.alive(pre)
	r := f.lastSnapshot(pre)
	f.accept(r)
	n := 0
	// the snapshot is on its way, its messages stamped with no time
	f.minutes(6, func() {
		n++
		stamp := time.UnixMilli(-3_600_000)
		if n%2 == 0 {
			stamp = time.UnixMilli(0)
		}
		f.m.processed(sid(1), pre, stamp, stamp, f.stamp(), r.requestID)
	})
	expect(t, f.m.counters.timedOut, 0, "six minutes in, still coming")
	f.runWithAlives(5 * time.Minute)
	expect(t, f.m.counters.timedOut, 0, "five minutes after the last of it")
	f.runWithAlives(time.Second)
	expect(t, f.m.counters.timedOut, 1, "a second later")
}

// RecoveryMachineTest: aLiveMessageStampedNoPositiveTimeDoesNotPutOffTheDeadlineForTheSnapshotComplete
func TestMachineTime_LiveMessageStampedNoPositiveTimeDoesNotPutOffTheDeadline(t *testing.T) {
	f := newFeed(t)
	f.open(1, prematch)
	f.startFeed()
	f.alive(pre)
	f.accept(f.lastSnapshot(pre))
	n := 0
	// live messages with no time keep coming, and every one would look older than the request
	f.minutes(5, func() {
		n++
		stamp := time.UnixMilli(-3_600_000)
		if n%2 == 0 {
			stamp = time.UnixMilli(0)
		}
		f.m.processed(sid(1), pre, stamp, stamp, f.stamp(), 0)
	})
	expect(t, f.m.counters.timedOut, 0, "five minutes in, still within it")
	f.runWithAlives(time.Second)
	expect(t, f.m.counters.timedOut, 1, "the timeout since the request, whatever came in between")
}

// Q7: the heard rule is per session: a session that has seen its snapshot_complete
// is no longer waited for, and only what a session that still awaits it takes
// puts the deadline off.
func TestMachineTime_OnlyASessionStillAwaitingTheSnapshotCompletePutsOffTheDeadline(t *testing.T) {
	f := newFeed(t)
	f.open(1, prematch)
	f.open(2, prematch)
	f.startFeed()
	f.alive(pre)
	r := f.lastSnapshot(pre)
	f.accept(r)
	f.snapshotComplete(1, pre, r.requestID)
	// session 1 has seen it: what it takes puts nothing off
	f.minutes(4, func() { f.processed(1, pre, f.now(), r.requestID) })
	f.runWithAlives(time.Minute + time.Second)
	expect(t, f.m.counters.timedOut, 1, "five minutes after the request: session 1's messages count for nothing")
	f.runWithAlives(5 * time.Second)
	again := f.lastSnapshot(pre)
	f.accept(again)
	// session 2 still awaits it: its messages put the deadline off
	f.minutes(6, func() { f.processed(2, pre, f.now(), again.requestID) })
	expect(t, f.m.counters.timedOut, 1, "six minutes in, session 2's snapshot still coming")
}

// ---- the session that processes late, per session (Q12)

// RecoveryMachineTest: aSessionThatProcessesLateTakesTheProducerDownWithoutARecoveryUntilItCatchesUp
func TestMachineTime_SessionThatProcessesLateTakesTheProducerDownWithoutARecoveryUntilItCatchesUp(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.bothUp(1)
	f.live(1, pre, 25*time.Second)
	f.advance(10 * time.Millisecond)
	late := f.lastStatus(pre)
	expect(t, late.cause, causeProcessingDelay, "cause")
	expect(t, late.reason(), types.ProcessingQueueDelayViolationProducerStatusReason, "reason")
	expect(t, late.delayed, true, "delayed")
	expectDown(t, f, live, false, "the other producer")
	expectCalls(t, f, pre, 1, "no recovery for a delay")

	f.live(1, pre, time.Second)
	f.advance(10 * time.Millisecond)
	back := f.lastStatus(pre)
	expect(t, back.down, false, "down")
	expect(t, back.cause, causeDelayStabilized, "cause")
	expect(t, back.reason(), types.ReturnedFromInactivityProducerStatusReason, "reason")
	expect(t, back.delayed, false, "delayed")
}

// Q12: one slow session next to a fast one is late, as the fast one does not hide it.
func TestMachineTime_LatenessIsMeasuredPerSession(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.open(2, all)
	f.startFeed()
	f.bothUp(1, 2)
	f.live(1, pre, 0)
	f.live(2, pre, 25*time.Second)
	f.advance(10 * time.Millisecond)
	expect(t, f.lastStatus(pre).cause, causeProcessingDelay, "the slow session takes the producer down")
	f.live(1, pre, 0)
	f.advance(10 * time.Millisecond)
	expectDown(t, f, pre, true, "the fast session's message does not bring it back")
	f.live(2, pre, 0)
	f.advance(10 * time.Millisecond)
	expectDown(t, f, pre, false, "once the slow session is on time")
}

// RecoveryMachineTest: aSessionThatTakesNothingWhileAlivesFlowIsLate
func TestMachineTime_SessionThatTakesNothingWhileAlivesFlowIsLate(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.bothUp(1)
	f.live(1, pre, 0)
	// alives on the alive channel only: the session's own queue does not move
	for second := 1; second <= 21; second++ {
		f.clock.advance(time.Second)
		if second%10 == 0 {
			f.alive(pre)
			f.alive(live)
		}
		f.m.tick()
	}
	expect(t, f.lastStatus(pre).cause, causeProcessingDelay, "cause")
	expectDown(t, f, live, false, "a producer the session has taken nothing of yet")
}

// RecoveryMachineTest: aRecoveryThatCompletesWhileASessionProcessesTheProducerLateKeepsItDownForThat
func TestMachineTime_RecoveryThatCompletesWhileASessionProcessesTheProducerLateKeepsItDownForThat(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.bothUp(1)
	f.unsubscribed(pre)
	r := f.lastSnapshot(pre)
	f.live(1, pre, 25*time.Second)
	f.advance(10 * time.Millisecond)
	f.complete(r, 1)
	late := f.lastStatus(pre)
	expect(t, late.down, true, "recovered, but processed late: down")
	expect(t, late.cause, causeProcessingDelay, "cause")
	for _, c := range causes(f.statusesOf(pre)) {
		expect(t, c == causeRecoveryCompleted, false, "a RecoveryCompleted cause")
	}

	f.live(1, pre, time.Second)
	f.advance(10 * time.Millisecond)
	expect(t, f.lastStatus(pre).cause, causeDelayStabilized, "cause when caught up")
}

// RecoveryMachineTest: aProducerLateAtItsFirstRecoveryIsUpForItsFirstRecoveryOnceOnTime
func TestMachineTime_ProducerLateAtItsFirstRecoveryIsUpForItsFirstRecoveryOnceOnTime(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.alive(pre)
	first := f.lastSnapshot(pre)
	f.live(1, pre, 25*time.Second)
	f.advance(10 * time.Millisecond)
	f.complete(first, 1)
	expect(t, f.lastStatus(pre).cause, causeProcessingDelay, "cause")
	f.live(1, pre, time.Second)
	f.advance(10 * time.Millisecond)
	var ups []statusCause
	for _, c := range f.statusesOf(pre) {
		if !c.down {
			ups = append(ups, c.cause)
		}
	}
	if len(ups) != 1 || ups[0] != causeFirstRecoveryCompleted {
		t.Fatalf("ups = %v, want the first recovery once", ups)
	}
}

// RecoveryMachineTest: aMessageStampedNoPositiveTimeIsNoAgeForTheSafetyNet
func TestMachineTime_MessageStampedNoPositiveTimeIsNoAge(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.bothUp(1)
	asked := len(f.snapshots(pre))
	changes := len(f.statusesOf(pre))
	// the offset is fresh: read as an age, a stamp of 0 is the whole epoch
	for second := 1; second <= 25; second++ {
		f.clock.advance(time.Second)
		stamp := time.UnixMilli(-3_600_000)
		if second%2 == 0 {
			stamp = time.UnixMilli(0)
		}
		f.m.processed(sid(1), pre, stamp, stamp, f.stamp(), 0)
		if second%10 == 0 {
			f.alive(pre)
			f.alive(live)
		}
		f.m.tick()
	}
	expect(t, len(f.statusesOf(pre)), changes, "status changes: no delay")
	expectDown(t, f, pre, false, "the producer")
	expect(t, len(f.snapshots(pre)), asked, "recoveries")
}

// ---- RecoveryClockTest: a step of the wall clock

// RecoveryClockTest: aStepOfTheWallClockMovesNoProducerDownAndTheMonotonicOneDoes
func TestMachineClock_StepOfTheWallClockMovesNoProducerDownAndTheMonotonicOneDoes(t *testing.T) {
	for _, step := range []time.Duration{30 * time.Second, -30 * time.Second} {
		f := newFeed(t)
		f.open(1, all)
		f.startFeed()
		f.bothUp(1)
		statuses, asked := len(f.out.statuses), len(f.out.calls)

		f.stepWall(step)
		expect(t, len(f.out.statuses), statuses, "status changes after the step")
		expect(t, len(f.out.calls), asked, "requests after the step")
		expectDown(t, f, pre, false, "after the step")

		f.advanceMono(20 * time.Second)
		expectDown(t, f, pre, false, "at the maximum inactivity")
		f.advanceMono(time.Second)
		expect(t, f.lastStatus(pre).cause, causeAliveIntervalViolation, "cause")
		expectDown(t, f, pre, true, "past it")
	}
}

// RecoveryClockTest: theInactivityOfAProducerThatNeverSentAnAliveCountsFromTheStartOnTheMonotonicClock
func TestMachineClock_InactivityOfAProducerThatNeverSentAnAliveCountsFromTheStartOnTheMonotonicClock(t *testing.T) {
	for _, step := range []time.Duration{30 * time.Second, -30 * time.Second} {
		f := newFeed(t)
		f.open(1, all)
		f.startFeed()
		f.stepWall(step)
		expect(t, len(f.out.statuses), 0, "status changes after the step")
		f.advanceMono(20 * time.Second)
		expect(t, len(f.out.statuses), 0, "status changes at the maximum inactivity")
		f.advanceMono(time.Second)
		got := causes(f.out.statuses)
		if len(got) != 2 || got[0] != causeAliveIntervalViolation || got[1] != causeAliveIntervalViolation {
			t.Fatalf("causes = %v, want the violation twice", got)
		}
	}
}

// RecoveryClockTest: aSessionsQuietIsMeasuredOnTheMonotonicClock
func TestMachineClock_SessionsQuietIsMeasuredOnTheMonotonicClock(t *testing.T) {
	for _, step := range []time.Duration{30 * time.Second, -30 * time.Second} {
		f := newFeed(t)
		f.open(1, all)
		f.startFeed()
		f.bothUp(1)
		f.live(1, pre, 0)

		f.stepWall(step)
		expect(t, f.lastStatus(pre).cause != causeProcessingDelay, true, "delayed by a step of the wall clock")
		f.monoSeconds(20)
		expectDown(t, f, pre, false, "20 seconds after the last message")
		f.monoSeconds(1)
		expect(t, f.lastStatus(pre).cause, causeProcessingDelay, "cause a second later")
	}
}

// RecoveryClockTest: aStepOfTheWallClockBetweenTwoAlivesLeavesTheAliveIntervalTheOffsetsFreshnessCountsOn
func TestMachineClock_StepBetweenTwoAlivesLeavesTheAliveIntervalTheOffsetsFreshnessCountsOn(t *testing.T) {
	for _, tc := range []struct {
		step, probe time.Duration
		delayed     bool
	}{{-30 * time.Second, 18 * time.Second, true}, {30 * time.Second, 25 * time.Second, false}} {
		f := withSettings(t, func(s *recoverySettings) { s.maxInactivity = 60 * time.Second })
		f.open(1, all)
		f.startFeed()
		f.bothUp(1)
		// ten seconds between the alives on the monotonic clock, whatever the wall one says
		f.advanceMono(10 * time.Second)
		f.stepWall(tc.step)
		f.alive(pre)
		f.advanceMono(tc.probe)
		// 90 s old by the producer's clock, over the maximum inactivity, when its offset is
		// fresh: it is fresh for two alive intervals, which stay the 10 s that were measured
		f.live(1, pre, 90*time.Second)
		f.m.tick()
		expectDown(t, f, pre, tc.delayed, "down for the session's delay, "+tc.probe.String()+" after the alive")
	}
}

// RecoveryClockTest: aMessageTakenAfterAStepOfTheWallClockIsNotAgedByTheStepBeforeTheNextAliveMeasuresTheOffset
func TestMachineClock_MessageTakenAfterAStepIsNotAgedByTheStepBeforeTheNextAliveMeasuresTheOffset(t *testing.T) {
	for _, step := range []time.Duration{30 * time.Second, -30 * time.Second} {
		f := newFeed(t)
		f.open(1, all)
		f.startFeed()
		f.bothUp(1)

		f.stepWall(step)
		// the producer's clock did not move: its message, taken now, is as old as it was sent. A
		// session's copy of an alive is handled before the alive channel's copy measures the offset anew
		producerNow := f.now().Add(-step)
		f.m.sessionAlive(sid(1), pre, producerNow, producerNow, f.stamp(), true)
		f.m.processed(sid(1), live, producerNow, producerNow, f.stamp(), 0)
		f.m.tick()

		expectDown(t, f, pre, false, "producer 1 after an alive")
		expectDown(t, f, live, false, "producer 2 after a message")
		for _, c := range causes(f.out.statuses) {
			expect(t, c == causeProcessingDelay, false, "a delay cause")
		}
	}
}

// RecoveryClockTest: aMessageThatIsReallyLateIsLateAfterAStepOfTheWallClockToo
func TestMachineClock_MessageThatIsReallyLateIsLateAfterAStepToo(t *testing.T) {
	for _, step := range []time.Duration{30 * time.Second, -30 * time.Second} {
		f := newFeed(t)
		f.open(1, all)
		f.startFeed()
		f.bothUp(1)
		f.stepWall(step)
		producerNow := f.now().Add(-step)
		at := producerNow.Add(-25 * time.Second)
		f.m.processed(sid(1), pre, at, at, f.stamp(), 0)
		f.m.tick()
		expectDown(t, f, pre, true, "25 s old, over the 20 s limit")
		expect(t, f.lastStatus(pre).cause, causeProcessingDelay, "cause")
	}
}

// RecoveryClockTest: aMovementOfTheWallClockUnderASecondIsNotAStepAndOneOverItIs
func TestMachineClock_MovementOfTheWallClockUnderASecondIsNotAStepAndOneOverItIs(t *testing.T) {
	for _, tc := range []struct {
		move    time.Duration
		delayed bool
	}{{900 * time.Millisecond, true}, {1100 * time.Millisecond, false}} {
		f := newFeed(t)
		f.open(1, all)
		f.startFeed()
		f.bothUp(1)
		f.stepWall(tc.move)
		// 19.5 s old by the producer's clock: 20.4 s as the unmoved offset reads it, over the limit
		producerNow := f.now().Add(-tc.move)
		at := producerNow.Add(-19500 * time.Millisecond)
		f.m.processed(sid(1), pre, at, at, f.stamp(), 0)
		f.m.tick()
		expectDown(t, f, pre, tc.delayed, "after a movement of "+tc.move.String())
	}
}

// RecoveryClockTest: aResumePointIsCappedAtTheProducersNowWhateverTheWallClockStepped
func TestMachineClock_ResumePointIsCappedAtTheProducersNowWhateverTheWallClockStepped(t *testing.T) {
	for _, step := range []time.Duration{30 * time.Second, -30 * time.Second} {
		f := newFeed(t)
		f.open(1, all)
		f.startFeed()
		f.bothUp(1)
		f.stepWall(step)
		producerNow := f.now().Add(-step)
		ahead := producerNow.Add(10 * time.Second)
		f.m.processed(sid(1), pre, ahead, ahead, f.stamp(), 0)
		f.m.processed(sid(1), live, ahead, ahead, f.stamp(), 0)
		got, _ := f.resume(pre)
		expectTime(t, got, producerNow, "the resume point after a step of "+step.String())
	}
}

// RecoveryClockTest: aRecoveryAskedForAfterAStepOfTheWallClockStartsAWindowBackFromTheProducersNow
func TestMachineClock_RecoveryAskedForAfterAStepStartsAWindowBackFromTheProducersNow(t *testing.T) {
	for _, step := range []time.Duration{30 * time.Second, -30 * time.Second} {
		f := newFeed(t)
		window := 3 * 24 * time.Hour
		// the client's start is inside the producer's stateful window by two seconds, and
		// outside it by the time the second request goes out five seconds on
		from := f.now().Add(-window + 2*time.Second)
		f.cat.from[pre] = from
		f.open(1, all)
		f.startFeed()
		f.alive(pre)
		first := f.lastSnapshot(pre)
		expectTime(t, first.after, from, "the first request's start")
		f.refuse(first)
		asked := len(f.snapshots(pre))

		// the step, and the backoff of the second request on both clocks, with no alive to
		// measure the offset anew: the producer's clock has not stepped, so its now is the SDK's less the step
		f.stepWall(step)
		f.advance(5 * time.Second)

		expect(t, len(f.snapshots(pre)), asked+1, "asked for again")
		producerNow := f.now().Add(-step)
		expectTime(t, f.lastSnapshot(pre).after, producerNow.Add(-window), "the start of the window")
	}
}

// RecoveryClockTest: theInitialSnapshotIntervalCountsBackFromTheWallClockWhateverTheMonotonicOneSays
func TestMachineClock_InitialSnapshotIntervalCountsBackFromTheWallClock(t *testing.T) {
	for _, minutes := range []time.Duration{30 * time.Minute, -30 * time.Minute} {
		f := withSettings(t, func(s *recoverySettings) { s.initialSnapshot = 10 * time.Minute })
		f.open(1, all)
		f.stepWall(minutes)
		f.startFeed()
		boundary := f.now().Add(-10 * time.Minute)
		f.alive(pre)
		expectTime(t, f.lastSnapshot(pre).after, boundary, "the interval before the start, by the wall clock")
	}
}

// RecoveryClockTest: theDeadlineForASnapshotCompleteCountsOnTheMonotonicClock
func TestMachineClock_DeadlineForASnapshotCompleteCountsOnTheMonotonicClock(t *testing.T) {
	for _, step := range []time.Duration{600 * time.Second, -600 * time.Second} {
		f := newFeed(t)
		f.open(1, prematch)
		f.startFeed()
		f.alive(pre)
		r := f.lastSnapshot(pre)
		f.accept(r)

		f.stepWall(step)
		expect(t, f.m.counters.timedOut, 0, "timed out after the step")
		expect(t, f.inFlight(pre), r.requestID, "in flight after the step")
		f.monoSeconds(300)
		expect(t, f.m.counters.timedOut, 0, "at the deadline")
		f.monoSeconds(1)
		expect(t, f.m.counters.timedOut, 1, "a second later")
		expectCalls(t, f, pre, 1, "not asked for again before its backoff")
	}
}

// RecoveryClockTest: whatAHeardSnapshotPutsOffIsCountedOnTheMonotonicClock
func TestMachineClock_WhatAHeardSnapshotPutsOffIsCountedOnTheMonotonicClock(t *testing.T) {
	for _, step := range []time.Duration{600 * time.Second, -600 * time.Second} {
		f := newFeed(t)
		f.open(1, prematch)
		f.startFeed()
		f.alive(pre)
		r := f.lastSnapshot(pre)
		f.accept(r)
		f.monoSeconds(240)
		f.processed(1, pre, f.now(), r.requestID)

		f.stepWall(step)
		f.monoSeconds(300)
		expect(t, f.m.counters.timedOut, 0, "five minutes after the last of the snapshot")
		f.monoSeconds(1)
		expect(t, f.m.counters.timedOut, 1, "a second later")
	}
}

// RecoveryClockTest: theMaximumRecoveryTimeCountsOnTheMonotonicClock
func TestMachineClock_MaximumRecoveryTimeCountsOnTheMonotonicClock(t *testing.T) {
	for _, step := range []time.Duration{600 * time.Second, -600 * time.Second} {
		f := withSettings(t, func(s *recoverySettings) {
			s.maxRecoveryTime = 10 * time.Minute
			s.snapshotCompleteTimeout = time.Hour
		})
		f.open(1, prematch)
		f.startFeed()
		f.alive(pre)
		f.accept(f.lastSnapshot(pre))

		f.stepWall(step)
		expect(t, f.m.counters.timedOut, 0, "timed out after the step")
		f.monoSeconds(600)
		expect(t, f.m.counters.timedOut, 0, "at the maximum")
		f.monoSeconds(1)
		expect(t, f.m.counters.timedOut, 1, "a second later")
	}
}

// RecoveryClockTest: aFailedRecoveryIsAskedForAgainAfterItsBackoffOnTheMonotonicClock
func TestMachineClock_FailedRecoveryIsAskedForAgainAfterItsBackoffOnTheMonotonicClock(t *testing.T) {
	for _, step := range []time.Duration{30 * time.Second, -30 * time.Second} {
		f := newFeed(t)
		f.open(1, all)
		f.startFeed()
		f.alive(pre)
		f.refuse(f.lastSnapshot(pre))
		asked := len(f.snapshots(pre))

		f.stepWall(step)
		expect(t, len(f.snapshots(pre)), asked, "after the step")
		f.advanceMono(4 * time.Second)
		expect(t, len(f.snapshots(pre)), asked, "before the first backoff of 5 s")
		f.advanceMono(time.Second)
		expect(t, len(f.snapshots(pre)), asked+1, "after it")
	}
}

// RecoveryClockTest: aSpentCapIsReArmedAfterTheCooldownOnTheMonotonicClock, with the slow retry
func TestMachineClock_SlowRetryCountsOnTheMonotonicClock(t *testing.T) {
	for _, step := range []time.Duration{660 * time.Second, -660 * time.Second} {
		f := newFeed(t)
		f.open(1, all)
		f.startFeed()
		f.alive(pre)
		for _, pause := range backoffs {
			f.refuse(f.lastSnapshot(pre))
			f.runWithAlives(pause)
		}
		f.refuse(f.lastSnapshot(pre))
		expect(t, f.lastStatus(pre).cause, causeRecoveryFailed, "cause")
		expectCalls(t, f, pre, 4, "recoveries with the cap spent")

		f.stepWall(step)
		expectCalls(t, f, pre, 4, "after the step")
		f.monoSeconds(59)
		expectCalls(t, f, pre, 4, "59 s into the slow retry")
		f.monoSeconds(1)
		expectCalls(t, f, pre, 5, "at 60 s")
	}
}

// RecoveryClockTest: aMessageGeneratedBeforeARequestPutsOffItsDeadlineAndOneGeneratedWithItDoesNotWhateverTheWallClockStepped (the snapshot request)
func TestMachineClock_MessageGeneratedBeforeARequestPutsOffItsDeadlineAndOneGeneratedWithItDoesNot(t *testing.T) {
	for _, step := range []time.Duration{30 * time.Second, -30 * time.Second} {
		for _, before := range []bool{true, false} {
			for _, liveMessage := range []bool{true, false} {
				f := newFeed(t)
				f.open(1, prematch)
				f.startFeed()
				f.alive(pre)
				// asked for after the step, with no alive since to measure the offset anew
				first := f.lastSnapshot(pre)
				f.refuse(first)
				f.stepWall(step)
				f.advance(5 * time.Second)
				again := f.lastSnapshot(pre)
				expect(t, again.requestID != first.requestID, true, "asked for again")
				f.accept(again)
				requestedAt := f.now().Add(-step)

				f.monoSeconds(10)
				generatedAt := requestedAt
				if before {
					generatedAt = requestedAt.Add(-time.Millisecond)
				}
				if liveMessage {
					f.m.processed(sid(1), pre, generatedAt, generatedAt, f.stamp(), 0)
				} else {
					f.m.sessionAlive(sid(1), pre, generatedAt, generatedAt, f.stamp(), true)
				}
				f.monoSeconds(290)
				expect(t, f.m.counters.timedOut, 0, "at the deadline of its request")
				f.monoSeconds(5)
				want := 0
				if !before {
					want = 1
				}
				expect(t, f.m.counters.timedOut, want, "five seconds past it: put off by a message generated before the request, not by one with it")
				if before {
					f.monoSeconds(6)
					expect(t, f.m.counters.timedOut, 1, "past the put-off deadline")
				}
			}
		}
	}
}

// RecoveryClockTest: aRecoveryAskedForAfterAStepOfTheWallClockMovesTheCheckpointToTheProducersNowAtTheRequest
func TestMachineClock_RecoveryAskedForAfterAStepMovesTheCheckpointToTheProducersNowAtTheRequest(t *testing.T) {
	for _, step := range []time.Duration{30 * time.Second, -30 * time.Second} {
		f := newFeed(t)
		f.open(1, prematch)
		f.startFeed()
		f.alive(pre)
		first := f.lastSnapshot(pre)
		f.refuse(first)
		f.stepWall(step)
		f.advance(5 * time.Second)
		again := f.lastSnapshot(pre)
		f.accept(again)
		requestedAt := f.now().Add(-step)
		cp, _ := f.checkpoint(1, pre)
		expect(t, cp.Before(requestedAt), true, "the checkpoint before the completion")

		f.complete(again, 1)
		cp, _ = f.checkpoint(1, pre)
		expectTime(t, cp, requestedAt, "the snapshot is as of the request, by the producer's clock")
	}
}

// RecoveryClockTest: anAliveIsJoinedOrGivesTheRecoveryUpByWhenItWasReceivedOnTheMonotonicClock
func TestMachineClock_AliveIsJoinedOrGivesTheRecoveryUpByWhenItWasReceivedOnTheMonotonicClock(t *testing.T) {
	for _, receivedBeforeTheAnswer := range []bool{true, false} {
		f := newFeed(t)
		f.open(1, all)
		f.startFeed()
		f.bothUp(1)
		f.unsubscribed(pre)
		r := f.lastSnapshot(pre)
		base := f.stamp()
		f.advanceMono(2 * time.Second)
		f.accept(r)

		// its wall reading says the opposite of its monotonic one
		received := NewStamp(base.Wall().Add(-time.Minute), base.Mono()+3*time.Second)
		if receivedBeforeTheAnswer {
			received = NewStamp(base.Wall().Add(time.Minute), base.Mono()+time.Second)
		}
		f.m.alive(pre, f.now(), f.now(), received, false)
		if receivedBeforeTheAnswer {
			expect(t, f.inFlight(pre), r.requestID, "joined, not given up")
			expect(t, f.m.counters.abandoned, 0, "abandoned")
		} else {
			expect(t, f.m.counters.abandoned, 1, "given up")
		}
	}
}

// RecoveryClockTest: whatIsPublishedFollowsTheWallClockWhateverTheMonotonicOneDoes
func TestMachineClock_WhatIsPublishedFollowsTheWallClockWhateverTheMonotonicOneDoes(t *testing.T) {
	for _, step := range []time.Duration{30 * time.Second, -30 * time.Second} {
		f := newFeed(t)
		f.open(1, prematch)
		f.startFeed()
		f.stepWall(step)
		wall := f.now()
		f.alive(pre)
		r := f.lastSnapshot(pre)
		f.accept(r)
		expectTime(t, f.recoveryInfo(pre).Timestamp(), wall, "when the recovery was asked for")
		f.complete(r, 1)
		expectTime(t, f.lastStatus(pre).at, wall, "when the producer came up")

		// on from there the wall clock only moves forward
		abs := step
		if abs < 0 {
			abs = -abs
		}
		f.stepWall(abs)
		later := f.now()
		// the producer's clock did not step with the SDK's: its now is where the step left it
		producerNow := later.Add(-abs)
		f.m.processed(sid(1), pre, producerNow, producerNow, f.stamp(), 0)
		got, _ := f.resume(pre)
		expectTime(t, got, producerNow, "the resume point")

		f.stepWall(abs)
		f.unsubscribed(pre)
		again := f.lastSnapshot(pre)
		expectTime(t, again.after, wall, "the next recovery starts from the last subscribed alive")
		f.accept(again)
		expectTime(t, f.recoveryInfo(pre).After(), wall, "the recovery info's start")
		expectTime(t, f.recoveryInfo(pre).Timestamp(), f.now(), "the recovery info's time")
		expectTime(t, f.lastStatus(pre).at, f.now(), "when it went down")
	}
}

// A track learned late by a message, not an alive, is not inactive at once: its
// inactivity counts from when it was learned, not from the start of the feed.
func TestMachineTime_LateTrackLearnedByAMessageIsNotInactiveAtOnce(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.clock.advance(time.Hour)
	f.alive(pre)
	f.alive(live)
	f.cat.list = append(f.cat.list, fakeProducer{id: 9, name: "late", window: statefulWindow})
	f.m.rewind(9, f.now()) // a first fact that is not an alive
	f.m.tick()
	if f.m.byID[9] == nil || f.m.byID[9].inactive {
		t.Fatal("producer 9 was learned a moment ago and is already inactive")
	}
	f.advance(21 * time.Second)
	expect(t, f.m.byID[9].inactive, true, "inactive once the maximum inactivity has passed since it was learned")
}

// d: a session opened after the request is not awaited by it, so what it takes
// puts nothing off.
func TestMachineTime_LateSessionNotAwaitedByTheRecoveryPutsNothingOff(t *testing.T) {
	f := newFeed(t)
	f.open(1, prematch)
	f.startFeed()
	f.alive(pre)
	f.accept(f.lastSnapshot(pre))
	requestedAt := f.now()
	f.open(2, prematch)
	f.minutes(6, func() { f.processed(2, pre, requestedAt.Add(-time.Second), 0) })
	expect(t, f.m.counters.timedOut, 1, "the recovery timed out at five minutes, whatever session 2 took")
}

// g: lateness by an old sample needs the offset to be fresh when it is judged.
func TestMachineTime_LateSampleIsNoLatenessOnceTheOffsetIsStale(t *testing.T) {
	f := withSettings(t, func(s *recoverySettings) { s.maxInactivity = 60 * time.Second })
	f.open(1, all)
	f.startFeed()
	f.bothUp(1)
	f.live(1, pre, 70*time.Second) // sampled with a fresh offset: 70 s old
	f.clock.advance(30 * time.Second)
	f.m.tick() // the alive is 30 s old: recent, but the offset is stale
	expect(t, f.m.byID[pre].delayed, false, "delayed")
}

// g: lateness by quiet needs an alive to be recent when it is judged.
func TestMachineTime_QuietSessionIsNoLatenessWhenNoAliveIsRecent(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.bothUp(1)
	f.live(1, pre, 0)
	f.clock.advance(25 * time.Second)
	f.m.tick()
	expect(t, f.lastStatus(pre).cause, causeAliveIntervalViolation, "cause")
	expect(t, f.m.byID[pre].delayed, false, "delayed")
}
