package recovery

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The rules the actor needs of the machine beyond the facts of the feed: a
// session whose client callback is stuck holds the deadlines, a session opened
// with its queue not bound yet holds the requests, the marks of the facts are
// bounded by the producer's clock, a flip of the delayed flag is told, and the
// resume points go back when the actor closes with facts it did not handle.

// stuckSince tells the machine that session n has had a message in processing
// since the monotonic reading at.
func (f *feed) stuckSince(at time.Duration, sessions ...int) {
	since := make(map[uuid.UUID]time.Duration, len(sessions))
	for _, n := range sessions {
		since[sid(n)] = at
	}
	f.m.processing(since)
}

// ---- the stalled hold

func TestMachineHold_SessionWithAMessageInProcessingForLongerThanTheMaximumInactivityHoldsTheDeadline(t *testing.T) {
	f := newFeed(t)
	f.open(1, prematch)
	f.startFeed()
	f.alive(pre)
	r := f.lastSnapshot(pre)
	f.accept(r)
	f.stuckSince(f.clock.Now().mono, 1)

	f.runWithAlives(30 * time.Minute)
	expect(t, f.m.counters.timedOut, 0, "half an hour in, the session's callback stuck")
	expectCalls(t, f, pre, 1, "no second request queued behind the stuck message")

	f.stuckSince(0) // the callback returns
	f.runWithAlives(5 * time.Minute)
	expect(t, f.m.counters.timedOut, 0, "at the deadline, counted from the last of the hold")
	f.runWithAlives(2 * time.Second)
	expect(t, f.m.counters.timedOut, 1, "a moment later")
}

func TestMachineHold_MessageInProcessingForLessThanTheMaximumInactivityHoldsNothing(t *testing.T) {
	f := newFeed(t)
	f.open(1, prematch)
	f.startFeed()
	f.alive(pre)
	f.accept(f.lastSnapshot(pre))

	for range int(5*time.Minute/time.Second) + 2 {
		f.clock.advance(time.Second)
		// always a message in processing, never for long
		f.stuckSince(f.clock.Now().mono-5*time.Second, 1)
		if int(f.now().Sub(f.start)/time.Second)%10 == 0 {
			f.alive(pre)
		}
		f.m.tick()
	}
	expect(t, f.m.counters.timedOut, 1, "a busy client holds nothing")
}

func TestMachineHold_OnlyASessionThatAwaitsTheSnapshotCompleteHoldsTheDeadline(t *testing.T) {
	f := newFeed(t)
	f.open(1, prematch)
	f.open(2, prematch)
	f.open(3, liveOnly) // not a session of the prematch producer
	f.startFeed()
	f.alive(pre)
	r := f.lastSnapshot(pre)
	f.accept(r)
	f.snapshotComplete(1, pre, r.requestID)
	// session 1 has the snapshot_complete, session 3 does not receive the producer
	f.stuckSince(f.clock.Now().mono, 1, 3)

	f.runWithAlives(5*time.Minute + 2*time.Second)
	expect(t, f.m.counters.timedOut, 1, "neither of the two holds it")
}

func TestMachineHold_MaximumRecoveryTimeStillEndsARecoveryWhoseSessionIsStuck(t *testing.T) {
	s := defaultSettings()
	s.maxRecoveryTime = 20 * time.Minute
	f := newFeedWith(t, s, statefulWindow)
	f.open(1, prematch)
	f.startFeed()
	f.alive(pre)
	f.accept(f.lastSnapshot(pre))
	f.stuckSince(f.clock.Now().mono, 1)

	f.runWithAlives(20 * time.Minute)
	expect(t, f.m.counters.timedOut, 0, "at the maximum recovery time")
	f.runWithAlives(2 * time.Second)
	expect(t, f.m.counters.timedOut, 1, "past it")
}

func TestMachineHold_EventRecoveryIsHeldByAStuckSessionToo(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.bothUp(1)
	id := f.event(live, 1)
	f.acceptEvent(id)
	f.stuckSince(f.clock.Now().mono, 1)

	f.runWithAlives(30 * time.Minute)
	f.expectNotEnded(id)

	f.stuckSince(0)
	f.runWithAlives(5*time.Minute + 2*time.Second)
	f.expectEnded(id, timedOut)
}

func TestMachineHold_SessionThatClosesIsNotKeptInTheHold(t *testing.T) {
	f := newFeed(t)
	f.open(1, prematch)
	f.startFeed()
	f.alive(pre)
	f.accept(f.lastSnapshot(pre))
	f.stuckSince(f.clock.Now().mono, 1)
	f.close(1)
	f.m.processing(map[uuid.UUID]time.Duration{sid(1): f.clock.Now().mono})
	f.runWithAlives(10 * time.Minute)
	expect(t, f.m.counters.timedOut, 0, "no session is left to wait for: the recovery completes")
	expectDown(t, f, pre, false, "the producer")
}

// ---- a session whose queue is not bound yet

func TestMachineHold_SessionOpeningHoldsTheRequestsOfItsProducersUntilItsQueueIsBound(t *testing.T) {
	f := newFeed(t)
	f.startFeed()
	f.alive(pre)
	expectCalls(t, f, pre, 0, "no session receives the producer")

	f.opened = append(f.opened, 1)
	f.m.sessionOpening(sid(1), prematch, f.stamp())
	expectCalls(t, f, pre, 0, "its queue is not bound")
	f.clock.advance(time.Second)
	f.alive(pre)
	expectCalls(t, f, pre, 0, "an alive does not release it")

	f.m.channelReopened(sid(1))
	expectCalls(t, f, pre, 1, "asked for once the queue is bound")
	expectTime(t, f.lastSnapshot(pre).after, time.Time{}, "a producer that never recovered asks from the client's start")
}

func TestMachineHold_EventRecoveryAskedForWhileASessionIsOpeningWaitsForItsQueue(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.bothUp(1)
	f.m.sessionOpening(sid(2), liveOnly, f.stamp())

	id := f.event(live, 1)
	expect(t, len(f.eventsAsked()), 0, "sent while a session awaits its queue")
	f.m.channelReopened(sid(2))
	expect(t, len(f.eventsAsked()), 1, "sent once the queue is bound")
	expect(t, f.lastEventAsked().requestID, id, "under the id the caller got")
}

func TestMachineHold_SessionWhoseOpenFailedIsNoLongerWaitedFor(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.bothUp(1)
	f.m.sessionOpening(sid(2), liveOnly, f.stamp())
	id := f.event(live, 1)
	f.m.sessionClosed(sid(2))
	expect(t, len(f.eventsAsked()), 1, "the request goes out once nothing awaits a queue")
	f.acceptEvent(id)
	f.snapshotComplete(1, live, id)
	f.expectEnded(id, completed)
}

// ---- the marks

func TestMachineMark_MessageAheadOfTheProducersClockIsMarkedAtTheProducersNow(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.alive(pre)
	at := f.stamp()
	ahead := f.now().Add(10 * time.Minute)
	expectTime(t, f.m.markOf(pre, ahead, at), f.now(), "ten minutes ahead")
	earlier := f.now().Add(-time.Second)
	expectTime(t, f.m.markOf(pre, earlier, at), earlier, "a second behind")
}

func TestMachineMark_BeforeTheFirstAliveOnlyAGrossFutureIsBounded(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	at := f.stamp()
	slightly := f.now().Add(30 * time.Second)
	expectTime(t, f.m.markOf(pre, slightly, at), slightly, "30 s ahead is a fast clock")
	expectTime(t, f.m.markOf(pre, f.now().Add(time.Hour), at), f.now(), "an hour ahead")
	expectTime(t, f.m.markOf(99, f.now().Add(time.Hour), at), f.now(), "a producer not tracked")
}

func TestMachineMark_AliveFromTheFutureIsMarkedAtTheSDKsNow(t *testing.T) {
	f := newFeed(t)
	at := f.stamp()
	expectTime(t, aliveMark(f.now().Add(time.Hour), at), f.now(), "an hour ahead")
	expectTime(t, aliveMark(f.now().Add(time.Second), at), f.now().Add(time.Second), "a second ahead")
	expectTime(t, aliveMark(f.now().Add(-time.Hour), at), f.now().Add(-time.Hour), "an hour behind")
}

// ---- the delayed flag

func TestMachineDelayed_FlagFlippingWhileTheProducerIsDownForAnotherCauseIsToldOnItsOwn(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.bothUp(1)
	f.m.channelLost(sid(1))
	f.m.channelReopened(sid(1))
	down := f.lastStatus(pre)
	expect(t, down.down, true, "down")
	expect(t, down.delayed, false, "delayed")
	before := len(f.statusesOf(pre))

	f.live(1, pre, 25*time.Second)
	f.advance(10 * time.Millisecond)
	statuses := f.statusesOf(pre)
	expect(t, len(statuses), before+1, "one status for the flag")
	flip := statuses[len(statuses)-1]
	expect(t, flip.delayed, true, "delayed")
	expect(t, flip.down, true, "down")
	expect(t, flip.cause, causeChannelLost, "the cause it keeps")
	expect(t, flip.public, true, "told to the client")

	f.advance(time.Second)
	expect(t, len(f.statusesOf(pre)), before+1, "no repeat while the flag stands")
	f.live(1, pre, time.Second)
	f.advance(10 * time.Millisecond)
	statuses = f.statusesOf(pre)
	expect(t, len(statuses), before+2, "one for the flag going back")
	expect(t, statuses[len(statuses)-1].delayed, false, "delayed")
}

func TestMachineDelayed_NothingIsToldBeforeTheProducerHasHadAStatus(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.alive(pre)
	f.live(1, pre, 25*time.Second)
	f.advance(10 * time.Millisecond)
	expect(t, len(f.statusesOf(pre)), 0, "statuses of a producer that has not recovered")
}

// ---- the actor closes with facts it did not handle

func TestMachineClose_UnhandledFactsTakeThePointBackToTheLastSubscribedAlive(t *testing.T) {
	f := newFeed(t)
	f.open(1, prematch)
	f.startFeed()
	f.alive(pre)
	f.complete(f.lastSnapshot(pre), 1)
	lastSubscribed := f.now()
	f.clock.advance(10 * time.Second)
	f.live(1, pre, 0)
	point, _ := f.resume(pre)
	expectTime(t, point, f.now(), "the point follows the session")

	f.m.beginClosing()
	f.m.unhandledAtClose(nil)
	got, _ := f.out.lastPoint(pre)
	expectTime(t, got, lastSubscribed, "back to the last subscribed alive")
	if _, ok := f.out.lastPoint(live); ok {
		t.Fatal("a point was published for a producer no session receives")
	}
	expect(t, len(f.out.pointsOf(pre)), 2, "points published")
	f.m.unhandledAtClose(nil)
	expect(t, len(f.out.pointsOf(pre)), 2, "a second call moves nothing")
}

// With no subscribed alive nothing can take the point further back than it is: the
// unsubscribed alive that opens a gap has no safe point to start it from, and the
// client's start is kept, not replaced by a full snapshot.
func TestMachineClose_UnhandledFactsWithNoSubscribedAliveKeepTheClientsStart(t *testing.T) {
	f := newFeed(t)
	seed := f.now().Add(-time.Hour)
	f.cat.from[pre] = seed
	f.open(1, all)
	f.startFeed()
	point, _ := f.resume(pre)
	expectTime(t, point, seed, "the client's start")
	published := len(f.out.pointsOf(pre))

	f.m.beginClosing()
	f.m.unhandledAtClose(nil)
	expect(t, len(f.out.pointsOf(pre)), published, "points published")
	got, _ := f.out.lastPoint(pre)
	expectTime(t, got, seed, "the point after the close")
}

// A start the client set that is left unhandled takes the point back to it, and one
// later than the point changes nothing.
func TestMachineClose_StartTheClientSetAndLeftUnhandledTakesThePointBackToIt(t *testing.T) {
	f := upWith(t, all)
	f.clock.advance(10 * time.Minute)
	f.alive(pre)
	f.live(1, pre, 0)
	point, _ := f.resume(pre)
	published := len(f.out.pointsOf(pre))

	f.m.beginClosing()
	f.m.unhandledAtClose(map[int]time.Time{pre: point.Add(time.Hour)})
	expect(t, len(f.out.pointsOf(pre)), published, "points published for a start later than the point")

	earlier := point.Add(-10 * time.Minute)
	f.m.unhandledAtClose(map[int]time.Time{pre: earlier})
	got, _ := f.out.lastPoint(pre)
	expectTime(t, got, earlier, "the point after the close")
}

func TestMachineClose_FeedThatNeverStartedKeepsTheClientsStart(t *testing.T) {
	f := newFeed(t)
	f.cat.from[pre] = f.now().Add(-time.Hour)
	f.open(1, all)
	f.m.unhandledAtClose(nil)
	expect(t, len(f.out.points), 0, "points published")
}

func TestMachineClose_PointAlreadyBehindTheLastSubscribedAliveIsKept(t *testing.T) {
	f := upWith(t, all)
	f.clock.advance(10 * time.Second)
	f.live(1, pre, 0)
	processed := f.now()
	f.clock.advance(10 * time.Second)
	f.alive(pre)
	point, _ := f.resume(pre)
	expectTime(t, point, processed, "behind the alive")
	published := len(f.out.pointsOf(pre))

	f.m.beginClosing()
	f.m.unhandledAtClose(nil)
	expect(t, len(f.out.pointsOf(pre)), published, "points published")
}

// ---- the stand-alone rules the actor relies on

func TestMachineHold_ProcessingTellsOnlyTheSessionsItNamesAndForgetsTheRest(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.open(2, all)
	f.stuckSince(5*time.Second, 1, 2)
	expect(t, f.m.session(sid(1)).processing, true, "session 1")
	f.stuckSince(7*time.Second, 2)
	expect(t, f.m.session(sid(1)).processing, false, "session 1 after")
	expect(t, f.m.session(sid(2)).processingSince, 7*time.Second, "session 2")
	f.m.processing(nil)
	expect(t, f.m.session(sid(2)).processing, false, "session 2 after")
}

func TestMachineHold_RefusedRequestOfAStuckSessionStillFailsAndIsAskedAgain(t *testing.T) {
	f := newFeed(t)
	f.open(1, prematch)
	f.startFeed()
	f.alive(pre)
	r := f.lastSnapshot(pre)
	f.stuckSince(f.clock.Now().mono, 1)
	f.m.answered(r.requestID, errors.New("503"))
	f.runWithAlives(6 * time.Second)
	expectCalls(t, f, pre, 2, "a refusal is not held")
	expect(t, f.recoveryInfo(pre).Successful(), false, "the refused request's outcome")
}

// ---- a disabled producer, and several sessions that lost their channels

func TestMachineHold_ProducerDisabledDuringARecoveryIsNotAskedForAgain(t *testing.T) {
	f := newFeed(t)
	f.open(1, prematch)
	f.startFeed()
	f.alive(pre)
	r := f.lastSnapshot(pre)
	f.refuse(r)
	f.cat.disabled[pre] = true
	f.runWithAlives(2 * time.Minute)
	expectCalls(t, f, pre, 1, "a disabled producer asked for again after its backoff")
}

func TestMachineHold_ProducerDisabledAndSilentIsNotTakenDown(t *testing.T) {
	f := upWith(t, all)
	statuses := len(f.statusesOf(pre))
	f.cat.disabled[pre] = true
	f.clock.advance(time.Minute)
	f.m.tick()
	expect(t, len(f.statusesOf(pre)), statuses, "statuses of the disabled producer")
	expectDown(t, f, pre, false, "the disabled producer")
	expectDown(t, f, live, true, "the enabled one, silent for a minute")
}

func TestMachineHold_RecoveryWaitsForEverySessionThatLostItsChannel(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.open(2, all)
	f.open(3, all)
	f.startFeed()
	f.bothUp(1, 2, 3)
	calls := len(f.snapshots(live))

	// a connection drop loses several channels at once
	f.m.channelLost(sid(2))
	f.m.channelLost(sid(3))
	f.alive(live)
	f.m.channelReopened(sid(2))
	f.alive(live)
	expect(t, len(f.snapshots(live)), calls, "recoveries asked for while session 3 awaits its queue")
	f.m.channelReopened(sid(3))
	expect(t, len(f.snapshots(live)), calls+1, "recoveries asked for once the last queue is bound")
}

// ---- the client's start while the producer has not recovered

// A start set after the feed opened and before any session subscribed is the seed
// of the first recovery, with an alive before it or not, and nothing is logged.
func TestMachineSeed_StartSetBeforeTheFirstSessionIsTheFirstRecoverysStart(t *testing.T) {
	for _, withAlive := range []bool{false, true} {
		f := newFeed(t)
		f.startFeed()
		if withAlive {
			f.alive(pre)
		}
		set := f.now().Add(-10 * time.Minute)
		f.logs.take()
		f.m.rewind(pre, set)
		f.open(1, all)
		f.alive(pre)
		expectTime(t, f.lastSnapshot(pre).after, set, "the first recovery")
		for _, line := range f.logs.take() {
			if strings.HasPrefix(line, "WARN") {
				t.Fatalf("withAlive=%v: a warning for a start that is taken: %s", withAlive, line)
			}
		}
		got, _ := f.resume(pre)
		expectTime(t, got, set, "the resume point")
	}
}

// A start the client sets replaces the seed, later or earlier than it, for the lanes
// that have it and for the next request.
func TestMachineSeed_StartSetReplacesTheSeedEarlierOrLater(t *testing.T) {
	for _, delta := range []time.Duration{-time.Hour, 10 * time.Minute} {
		f := newFeed(t)
		f.cat.from[pre] = f.now().Add(-30 * time.Minute)
		f.open(1, all)
		f.startFeed()
		set := f.now().Add(-30*time.Minute + delta)
		f.m.rewind(pre, set)
		expectTime(t, f.m.session(sid(1)).lane(pre).checkpoint, set, "the checkpoint no message moved")
		f.alive(pre)
		expectTime(t, f.lastSnapshot(pre).after, set, "the first recovery")
	}
}

// A message a session finished keeps its checkpoint when the seed is replaced.
func TestMachineSeed_CheckpointAMessageMovedIsLeftWhenTheSeedIsReplaced(t *testing.T) {
	f := newFeed(t)
	f.cat.from[pre] = f.now().Add(-time.Hour)
	f.open(1, all)
	f.startFeed()
	f.clock.advance(time.Minute)
	f.alive(pre)
	f.live(1, pre, 0)
	moved, _ := f.checkpoint(1, pre)
	f.m.rewind(pre, f.now().Add(-2*time.Hour))
	got, _ := f.checkpoint(1, pre)
	expectTime(t, got, moved, "the checkpoint")
}

// Once the producer has recovered, a later value is ignored with a warning and an
// earlier one is a rewind.
func TestMachineSeed_OnceRecoveredALaterStartIsIgnoredWithAWarningAndAnEarlierOneRewinds(t *testing.T) {
	f := upWith(t, all)
	f.clock.advance(time.Minute)
	f.live(1, pre, 0)
	f.logs.take()
	f.m.rewind(pre, f.now().Add(time.Minute))
	lines := f.logs.take()
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "WARN") {
		t.Fatalf("log = %v, want one warning", lines)
	}
	earlier := f.now().Add(-time.Hour)
	f.m.rewind(pre, earlier)
	got, _ := f.resume(pre)
	expectTime(t, got, earlier, "the resume point after an earlier start")
}

// ---- a full snapshot stays one

// A recovery for the producer's whole history asks for it again, whatever made it
// ask again: a refusal, the deadline, a lost queue.
func TestMachineSeed_FullSnapshotIsAskedForAgainAsOneWhateverMadeItAskAgain(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause func(f *feed, r snapshotRequest)
	}{
		{"refused", func(f *feed, r snapshotRequest) { f.refuse(r) }},
		{"deadline", func(f *feed, r snapshotRequest) {
			f.accept(r)
			f.runWithAlives(6 * time.Minute)
		}},
		{"queue lost", func(f *feed, r snapshotRequest) {
			f.accept(r)
			f.m.channelLost(sid(1))
			f.m.channelReopened(sid(1))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFeed(t)
			f.open(1, prematch)
			f.startFeed()
			f.alive(pre)
			first := f.lastSnapshot(pre)
			expectTime(t, first.after, time.Time{}, "the first request")
			tc.cause(f, first)
			f.runWithAlives(30 * time.Second)
			f.alive(pre)
			if got := len(f.snapshots(pre)); got < 2 {
				t.Fatalf("asked for again %d times, want at least once", got-1)
			}
			for _, r := range f.snapshots(pre) {
				expectTime(t, r.after, time.Time{}, "request "+strconv.Itoa(r.requestID))
			}
		})
	}
}

// ---- a disabled producer's other facts

// A producer the client disabled is left alone by a lost channel, a snapshot_complete
// and a session's delay: nothing is told of it and nothing is asked for it.
func TestMachineHold_DisabledProducerIsLeftAloneByALostChannelASnapshotCompleteAndADelay(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.bothUp(1)
	statuses := len(f.statusesOf(pre))
	calls := len(f.snapshots(pre))
	f.cat.disabled[pre] = true

	f.m.channelLost(sid(1))
	f.m.channelReopened(sid(1))
	f.alive(pre)
	f.m.snapshotComplete(sid(1), pre, 12345)
	f.live(1, pre, 25*time.Second)
	f.advance(10 * time.Millisecond)
	expect(t, len(f.statusesOf(pre)), statuses, "statuses of the disabled producer")
	expect(t, len(f.snapshots(pre)), calls, "recoveries asked for the disabled producer")
	expectDown(t, f, pre, false, "the disabled producer")
}

// ---- a late answer, and a late snapshot_complete

// The API's refusal of a request that a snapshot_complete has already completed changes
// nothing: the producer stays up, no failure is counted and no failed outcome is told.
func TestMachineHold_LateRefusalOfARequestThatCompletedChangesNothing(t *testing.T) {
	f := newFeed(t)
	f.open(1, prematch)
	f.startFeed()
	f.alive(pre)
	r := f.lastSnapshot(pre)
	f.snapshotComplete(1, pre, r.requestID)
	expectDown(t, f, pre, false, "the producer after its snapshot_complete")
	infos := len(f.out.infos)
	f.refuse(r)
	expectDown(t, f, pre, false, "the producer after the late refusal")
	expect(t, f.m.counters.failed, 0, "failures counted")
	expect(t, len(f.out.infos), infos, "outcomes told")
	f.runWithAlives(time.Minute)
	expectCalls(t, f, pre, 1, "recoveries asked for")
}

// A snapshot_complete of a request that has been abandoned does not put off the deadline
// of the one that replaced it.
func TestMachineHold_SnapshotCompleteOfAnAbandonedRequestDoesNotPutOffTheNewDeadline(t *testing.T) {
	f := newFeed(t)
	f.open(1, prematch)
	f.startFeed()
	f.alive(pre)
	first := f.lastSnapshot(pre)
	f.accept(first)
	f.runWithAlives(5*time.Minute + 2*time.Second) // the deadline passes: the request failed
	f.runWithAlives(6 * time.Second)               // its backoff
	second := f.lastSnapshot(pre)
	expect(t, second.requestID != first.requestID, true, "a new request")
	f.accept(second)
	timedOut := f.m.counters.timedOut
	f.runWithAlives(4 * time.Minute)
	f.snapshotComplete(1, pre, first.requestID) // late, of the abandoned id
	f.runWithAlives(time.Minute + 2*time.Second)
	expect(t, f.m.counters.timedOut, timedOut+1, "the new request timed out at its own deadline")
}

// ---- the status details

// A producer that sends only unsubscribed alives is alive: it stays down for being
// unsubscribed and is not reported as silent.
func TestMachineHold_ProducerSendingOnlyUnsubscribedAlivesStaysUnsubscribedNotSilent(t *testing.T) {
	f := upWith(t, all)
	for range 10 {
		f.clock.advance(10 * time.Second)
		f.unsubscribed(pre)
		f.m.tick()
	}
	expectDown(t, f, pre, true, "the producer")
	expect(t, f.lastStatus(pre).cause, causeUnsubscribed, "cause")
}

// A recovery that completes while a session lags brings the producer straight to down
// for the delay: it is never told up first.
func TestMachineHold_RecoveryCompletingWhileASessionLagsIsNeverToldUp(t *testing.T) {
	f := newFeed(t)
	f.open(1, prematch)
	f.startFeed()
	f.alive(pre)
	r := f.lastSnapshot(pre)
	f.accept(r)
	f.live(1, pre, 25*time.Second)
	f.advance(10 * time.Millisecond)
	f.snapshotComplete(1, pre, r.requestID)
	for _, s := range f.statusesOf(pre) {
		if !s.down {
			t.Fatalf("the producer was told up while a session lags: %+v", s)
		}
	}
	expect(t, f.lastStatus(pre).cause, causeProcessingDelay, "cause")
}

// ---- what is left unhandled at the close is never later than handling it would be

// A producer whose only handled alives were unsubscribed has a point and no safe point.
// Left unhandled at the close, a subscribed alive generated at g and an unsubscribed one
// after it would open a gap from g: the point goes back to g, not where it was, which a
// message finished after g put later.
func TestMachineClose_SubscribedAliveLeftUnhandledBeforeAnUnsubscribedOneTakesThePointBackToIt(t *testing.T) {
	run := func(handle bool) (point, g time.Time) {
		f := newFeed(t)
		f.open(1, all)
		f.startFeed()
		f.unsubscribed(pre) // the only alive handled is unsubscribed
		req := f.lastSnapshot(pre)
		f.accept(req)
		f.snapshotComplete(1, pre, req.requestID)
		f.m.publishResumePoints()
		if !f.m.byID[pre].safePoint.IsZero() {
			t.Fatal("setup: a safe point")
		}
		f.clock.advance(5 * time.Second)
		g = f.now() // a subscribed alive generated now, still on its way
		gStamp := f.stamp()
		f.clock.advance(time.Second)
		f.live(1, pre, 0) // a live message generated after it, finished by the session
		f.m.publishResumePoints()
		f.clock.advance(10 * time.Second)
		uAt, uStamp := f.now(), f.stamp()
		f.m.beginClosing()
		if handle {
			f.m.alive(pre, g, g, gStamp, true)
			f.m.alive(pre, uAt, uAt, uStamp, false)
		} else {
			f.m.unhandledAtClose(map[int]time.Time{pre: g}) // what the actor takes from the facts left
		}
		f.m.publishResumePoints()
		point, _ = f.out.lastPoint(pre)
		return point, g
	}
	handled, g := run(true)
	unhandled, _ := run(false)
	expectTime(t, handled, g, "the point when the facts are handled")
	if unhandled.After(handled) {
		t.Fatalf("left unhandled the point is %s, later than %s when handled", fmtTime(unhandled), fmtTime(handled))
	}
}

// Whatever is left unhandled, on a producer that is up, the point is never later than
// handling the same facts would make it.
func TestMachineClose_NothingLeftUnhandledMakesThePointLaterThanHandlingItWould(t *testing.T) {
	type step func(f *feed, handle bool, backTo map[int]time.Time)
	cases := map[string]step{
		"channel lost": func(f *feed, handle bool, _ map[int]time.Time) {
			if handle {
				f.m.channelLost(sid(1))
			}
		},
		"session closed": func(f *feed, handle bool, _ map[int]time.Time) {
			if handle {
				f.m.sessionClosed(sid(1))
			}
		},
		"session opened": func(f *feed, handle bool, _ map[int]time.Time) {
			if handle {
				f.m.sessionOpening(sid(2), all, f.stamp())
			}
		},
		"rewind earlier": func(f *feed, handle bool, backTo map[int]time.Time) {
			to := f.now().Add(-time.Hour)
			if handle {
				f.m.rewind(pre, to)
			} else {
				backTo[pre] = to
			}
		},
		"rewind later": func(f *feed, handle bool, backTo map[int]time.Time) {
			to := f.now().Add(30 * time.Second)
			if handle {
				f.m.rewind(pre, to)
			} else {
				backTo[pre] = to
			}
		},
		"unsubscribed alive": func(f *feed, handle bool, backTo map[int]time.Time) {
			if handle {
				f.unsubscribed(pre)
			} else {
				backTo[pre] = f.now()
			}
		},
		"subscribed alive then an unsubscribed one, safe point handled": func(f *feed, handle bool, backTo map[int]time.Time) {
			g, gStamp := f.now().Add(-time.Second), f.stamp()
			uAt, uStamp := f.now(), f.stamp()
			if handle {
				f.m.alive(pre, g, g, gStamp, true)
				f.m.alive(pre, uAt, uAt, uStamp, false)
			} else {
				backTo[pre] = g
			}
		},
	}
	for name, do := range cases {
		run := func(handle bool) time.Time {
			f := upWith(t, all)
			f.clock.advance(10 * time.Second)
			f.alive(pre)
			f.clock.advance(3 * time.Second)
			f.live(1, pre, 0)
			f.m.publishResumePoints()
			f.clock.advance(2 * time.Second)
			f.m.beginClosing()
			backTo := map[int]time.Time{}
			do(f, handle, backTo)
			if !handle {
				f.m.unhandledAtClose(backTo)
			}
			f.m.publishResumePoints()
			p, _ := f.out.lastPoint(pre)
			return p
		}
		if h, u := run(true), run(false); u.After(h) {
			t.Errorf("%s: left unhandled %s, later than %s when handled", name, fmtTime(u), fmtTime(h))
		}
	}
}

// ---- a start the client sets, before the producer has recovered

// A start later than the checkpoint a lost queue left does not move the gap past it: the
// queue's contents are missing whatever the client's start says.
func TestMachineSeed_StartSetLaterThanTheCheckpointOfALostQueueDoesNotMoveTheGapPastIt(t *testing.T) {
	f := newFeed(t)
	f.cat.from[pre] = f.now().Add(-time.Hour)
	f.open(1, all)
	f.startFeed()
	f.alive(pre)
	f.refuse(f.lastSnapshot(pre)) // the producer has not recovered
	f.clock.advance(time.Second)
	f.live(1, pre, 0)
	c, _ := f.checkpoint(1, pre)
	f.m.channelLost(sid(1)) // the queue is lost at c
	f.clock.advance(2 * time.Minute)
	f.m.rewind(pre, c.Add(time.Minute)) // later than the checkpoint, not in the future
	f.m.channelReopened(sid(1))
	f.clock.advance(10 * time.Second)
	f.alive(pre)
	f.m.tick()
	expectTime(t, f.lastSnapshot(pre).after, c, "the recovery starts at the lost queue's checkpoint")
	// an earlier start still goes earlier than the checkpoint
	g := newFeed(t)
	g.cat.from[pre] = g.now().Add(-time.Hour)
	g.open(1, all)
	g.startFeed()
	g.alive(pre)
	g.refuse(g.lastSnapshot(pre))
	g.clock.advance(time.Second)
	g.live(1, pre, 0)
	c2, _ := g.checkpoint(1, pre)
	g.m.channelLost(sid(1))
	g.m.rewind(pre, c2.Add(-30*time.Minute))
	g.m.channelReopened(sid(1))
	g.clock.advance(10 * time.Second)
	g.alive(pre)
	g.m.tick()
	expectTime(t, g.lastSnapshot(pre).after, c2.Add(-30*time.Minute), "an earlier start")
}

// The same start set again once the producer has recovered and a second recovery for it
// is in flight does not hold the resume point where it is: it is the point already.
func TestMachineSeed_SameStartSetAgainWhileTheRecoveryForItIsInFlightDoesNotPinTheResumePoint(t *testing.T) {
	f := newFeed(t)
	seed := f.now().Add(-30 * time.Minute)
	f.cat.from[pre] = seed
	f.open(1, all)
	f.startFeed()
	f.alive(pre)
	first := f.lastSnapshot(pre)
	to := seed.Add(-time.Hour)
	f.m.rewind(pre, to)
	f.accept(first)
	f.snapshotComplete(1, pre, first.requestID)
	second := f.lastSnapshot(pre)
	f.m.rewind(pre, to) // the same again, while the recovery that covers it is in flight
	if f.m.byID[pre].rewind != nil {
		t.Fatal("a rewind to the point it is at was opened")
	}
	f.accept(second)
	f.snapshotComplete(1, pre, second.requestID)
	for range 30 {
		f.clock.advance(10 * time.Second)
		f.alive(pre)
		f.live(1, pre, 0)
		f.m.tick()
	}
	p, _ := f.resume(pre)
	if p.Equal(to) {
		t.Fatalf("the resume point is pinned at %s", fmtTime(to))
	}
}

// Setting the value the seed already has, while the first recovery is in flight or at any
// time before it completes, asks for nothing more.
func TestMachineSeed_StartSetToTheValueTheSeedAlreadyHasDoesNothing(t *testing.T) {
	f := newFeed(t)
	seed := f.now().Add(-30 * time.Minute)
	f.cat.from[pre] = seed
	f.open(1, all)
	f.startFeed()
	f.alive(pre)
	for range 3 {
		req := f.lastSnapshot(pre)
		f.m.rewind(pre, seed)
		f.accept(req)
		f.snapshotComplete(1, pre, req.requestID)
	}
	expectCalls(t, f, pre, 1, "recoveries for a start the seed already was")
	expectDown(t, f, pre, false, "the producer")
}

// A reseed replaces the seed only: it opens no rewind.
func TestMachineSeed_ReseedOpensNoRewind(t *testing.T) {
	f := newFeed(t)
	f.cat.from[pre] = f.now().Add(-30 * time.Minute)
	f.open(1, all)
	f.startFeed()
	f.m.rewind(pre, f.now().Add(-time.Hour))
	if f.m.byID[pre].rewind != nil {
		t.Fatal("a reseed opened a rewind")
	}
}

// A start later than the point, set while a rewind is already open, is ignored with a
// warning like any other.
func TestMachineSeed_LaterStartWhileARewindIsOpenIsIgnoredWithAWarning(t *testing.T) {
	f := upWith(t, all)
	f.clock.advance(time.Minute)
	f.live(1, pre, 0)
	f.m.rewind(pre, f.now().Add(-time.Hour))
	if f.m.byID[pre].rewind == nil {
		t.Fatal("no rewind")
	}
	f.logs.take()
	f.m.rewind(pre, f.now().Add(-30*time.Minute)) // later than the rewind there is
	lines := f.logs.take()
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "WARN") {
		t.Fatalf("log = %v, want one warning", lines)
	}
}
