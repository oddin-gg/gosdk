package recovery

import (
	"errors"
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
	f.m.unhandledAtClose()
	got, _ := f.out.lastPoint(pre)
	expectTime(t, got, lastSubscribed, "back to the last subscribed alive")
	if _, ok := f.out.lastPoint(live); ok {
		t.Fatal("a point was published for a producer no session receives")
	}
	expect(t, len(f.out.pointsOf(pre)), 2, "points published")
	f.m.unhandledAtClose()
	expect(t, len(f.out.pointsOf(pre)), 2, "a second call moves nothing")
}

func TestMachineClose_UnhandledFactsWithNoSubscribedAliveTakeThePointBackToAFullSnapshot(t *testing.T) {
	f := newFeed(t)
	seed := f.now().Add(-time.Hour)
	f.cat.from[pre] = seed
	f.open(1, all)
	f.startFeed()
	point, _ := f.resume(pre)
	expectTime(t, point, seed, "the client's start")

	f.m.beginClosing()
	f.m.unhandledAtClose()
	got, ok := f.out.lastPoint(pre)
	expect(t, ok, true, "a point published")
	expectTime(t, got, time.Time{}, "a full snapshot")
}

func TestMachineClose_FeedThatNeverStartedKeepsTheClientsStart(t *testing.T) {
	f := newFeed(t)
	f.cat.from[pre] = f.now().Add(-time.Hour)
	f.open(1, all)
	f.m.unhandledAtClose()
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
	f.m.unhandledAtClose()
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
