package recovery

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/oddin-gg/gosdk/types"
)

// The machine's event recoveries: one event's messages asked for again, beside
// the producers' recoveries. What the client asks for, what the machine sends to
// a REST worker, and how each ended, are on the fake outbox.

const (
	completed = types.RecoveryStatusCompleted
	failedEnd = types.RecoveryStatusFailed
	timedOut  = types.RecoveryStatusTimedOut
)

func matchN(n int) types.URN { return types.URN{Prefix: "od", Type: "match", ID: n} }

// askEvent asks for the odds or the stateful messages of match n of the producer.
func (f *feed) askEvent(p, n int, stateful bool) (int, error) {
	return f.m.recoverEvent(p, matchN(n), stateful)
}

// event asks for the odds of match n, which must be accepted; it is the request id.
func (f *feed) event(p, n int) int {
	f.t.Helper()
	id, err := f.askEvent(p, n, false)
	if err != nil {
		f.t.Fatalf("event recovery of match %d of producer %d refused: %v", n, p, err)
	}
	return id
}

// recoverEvent asks for the odds of match 1.
func (f *feed) recoverEvent(p int) int { return f.event(p, 1) }

func (f *feed) acceptEvent(id int) { f.m.answered(id, nil) }

func (f *feed) refuseEvent(id int) { f.m.answered(id, errors.New("503")) }

// eventsAsked is the event recovery requests that went out.
func (f *feed) eventsAsked() []eventRequest { return f.out.events }

func (f *feed) lastEventAsked() eventRequest {
	f.t.Helper()
	if len(f.out.events) == 0 {
		f.t.Fatal("no event recovery was asked for")
	}
	return f.out.events[len(f.out.events)-1]
}

// endsOf are how the event recovery of the request id ended: once, at most.
func (f *feed) endsOf(id int) []eventOutcome {
	var out []eventOutcome
	for _, o := range f.out.outcomes {
		if o.requestID == id {
			out = append(out, o)
		}
	}
	return out
}

// expectEnded fails the test unless the event recovery ended once, as status.
func (f *feed) expectEnded(id int, status types.RecoveryRequestStatus) eventOutcome {
	f.t.Helper()
	ends := f.endsOf(id)
	if len(ends) != 1 {
		f.t.Fatalf("event recovery %d ended %d times, want once: %v", id, len(ends), ends)
	}
	expect(f.t, ends[0].status, status, "how event recovery "+strconv.Itoa(id)+" ended")
	return ends[0]
}

func (f *feed) expectNotEnded(id int) {
	f.t.Helper()
	if ends := f.endsOf(id); len(ends) != 0 {
		f.t.Fatalf("event recovery %d ended: %v", id, ends)
	}
}

// expectNoEventState fails the test when the machine still holds an event
// recovery, or one waiting for a channel.
func (f *feed) expectNoEventState() {
	f.t.Helper()
	expect(f.t, len(f.m.eventRecoveries), 0, "event recoveries in flight")
	expect(f.t, len(f.m.deferred), 0, "event recoveries waiting for a channel")
}

// sessionAliveAt is an alive of the session's queue generated at at.
func (f *feed) sessionAliveAt(n, p int, at time.Time) {
	f.m.sessionAlive(sid(n), p, at, at, f.stamp(), true)
}

// ---- the request and its completion

// RecoveryMachineTest: anEventRecoveryAnswersWithItsRequestIdAndCompletesOnceItsSnapshotCompleteIsSeen
func TestMachineEvents_EventRecoveryCompletesOnceEveryAwaitedSessionHasSeenItsSnapshotComplete(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.open(2, liveOnly)
	f.startFeed()
	id := f.recoverEvent(live)
	call := f.lastEventAsked()
	expect(t, call.requestID, id, "request id")
	expect(t, call.producer, "live", "producer name")
	expect(t, call.producerID, live, "producer")
	expect(t, call.eventID, matchN(1), "event")
	expect(t, call.stateful, false, "stateful")
	f.expectNotEnded(id)
	f.acceptEvent(id)
	f.expectNotEnded(id)

	f.snapshotComplete(1, live, id)
	f.expectNotEnded(id)
	f.snapshotComplete(2, live, id)
	end := f.expectEnded(id, completed)
	expect(t, end.err == nil, true, "no error for a completed one")
	expect(t, end.producerID, live, "producer of the outcome")
	expect(t, end.eventID, matchN(1), "event of the outcome")
	f.snapshotComplete(2, live, id)
	expect(t, len(f.out.outcomes), 1, "outcomes: once")
	expect(t, f.m.counters.unknownCompletions, 1, "unknown completions")
	f.expectNoEventState()
}

// Go's public API: the odds and the stateful messages of an event are two
// requests, and each asks the REST worker for its own kind.
func TestMachineEvents_StatefulRecoveryIsAskedForAsStateful(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	id, err := f.askEvent(live, 1, true)
	if err != nil {
		t.Fatal(err)
	}
	expect(t, f.lastEventAsked().stateful, true, "stateful")
	expect(t, f.lastEventAsked().requestID, id, "request id")
}

// RecoveryMachineTest: anEventRecoveryTheApiRefusedAnswersNull
func TestMachineEvents_EventRecoveryTheApiRefusedFailsWithItsReason(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	id := f.event(pre, 1)
	f.refuseEvent(id)
	end := f.expectEnded(id, failedEnd)
	if end.err == nil || !strings.Contains(end.err.Error(), "the API did not accept it: 503") {
		t.Fatalf("reason: %v", end.err)
	}
	expect(t, f.m.counters.eventRefused, 1, "refused")
	f.snapshotComplete(1, pre, id)
	expect(t, len(f.out.outcomes), 1, "nothing more after the refusal")
	f.expectNoEventState()
}

// RecoveryMachineTest: anEventRecoveryOfAnUnknownProducerFails
func TestMachineEvents_EventRecoveryOfAnUnknownProducerIsRefusedAndNothingIsSent(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	if _, err := f.askEvent(9, 1, false); err == nil {
		t.Fatal("an unknown producer was accepted")
	}
	expect(t, len(f.eventsAsked()), 0, "requests")
	f.expectNoEventState()
}

// RecoveryMachineTest: anEventRecoveryWhileTheConnectionIsDownIsRefused
func TestMachineEvents_EventRecoveryWhileTheConnectionIsDownIsRefused(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.m.connectionDown()
	if _, err := f.askEvent(live, 1, false); !errors.Is(err, errEventRecoveryConnectionDown) {
		t.Fatalf("its snapshot would have no queue to go to: %v", err)
	}
	expect(t, len(f.eventsAsked()), 0, "requests")
	expect(t, f.m.counters.eventRefused, 1, "refused")
	f.m.connectionUp()
	f.recoverEvent(live)
	expect(t, len(f.eventsAsked()), 1, "requests once it is back")
}

// ---- coalescing and the cap

// Go's coalescing: the same event and kind asked for again while one is in flight
// is that one.
func TestMachineEvents_SameEventAndKindWhileOneIsInFlightIsThatOne(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	id := f.event(live, 1)
	again, err := f.askEvent(live, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	expect(t, again, id, "the same request")
	expect(t, len(f.eventsAsked()), 1, "requests sent")

	stateful, _ := f.askEvent(live, 1, true)
	other := f.event(live, 2)
	elsewhere := f.event(pre, 1)
	expect(t, len(f.eventsAsked()), 4, "another kind, another event, another producer: a request of their own")
	if stateful == id || other == id || elsewhere == id || stateful == other {
		t.Fatalf("request ids are not distinct: %d %d %d %d", id, stateful, other, elsewhere)
	}

	f.acceptEvent(id)
	f.snapshotComplete(1, live, id)
	f.expectEnded(id, completed)
	next := f.event(live, 1)
	if next == id {
		t.Fatalf("an event recovery that ended was coalesced into: %d", next)
	}
	expect(t, len(f.eventsAsked()), 5, "requests sent after it completed")
}

// Go's coalescing comes before its cap: asking again for one in flight is no new one.
func TestMachineEvents_CoalescingIsNotRefusedAtTheCap(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	ids := make([]int, 129)
	for n := 1; n <= maxPendingEventRecoveries; n++ {
		ids[n] = f.event(live, n)
	}
	again, err := f.askEvent(live, 5, false)
	if err != nil {
		t.Fatalf("a request for one in flight was refused: %v", err)
	}
	expect(t, again, ids[5], "the one in flight")
}

// Go's cap: 128 event recoveries of a producer in flight, and one more is refused
// with the error the client knows; another producer's are counted apart.
func TestMachineEvents_ProducerHasAtMost128EventRecoveriesInFlight(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	for n := 1; n <= maxPendingEventRecoveries; n++ {
		f.event(live, n)
	}
	_, err := f.askEvent(live, 1000, false)
	if !errors.Is(err, ErrTooManyPendingEventRecoveries) {
		t.Fatalf("the 129th: %v", err)
	}
	if !strings.Contains(err.Error(), "producer 2 has 128 pending event recoveries") {
		t.Fatalf("the error says: %v", err)
	}
	expect(t, f.m.counters.eventRefused, 1, "refused")
	expect(t, len(f.eventsAsked()), 128, "requests sent")
	f.event(pre, 1)
	expect(t, len(f.eventsAsked()), 129, "another producer's")
}

// RecoveryMachineTest: eventRecoveriesInFlightAreBoundedPerProducerAndOnesTheApiNeverAnsweredExpireAtTheMaximumRecoveryTime
func TestMachineEvents_EventRecoveriesTheApiNeverAnsweredExpireAtTheMaximumRecoveryTimeAndFreeTheirSlots(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	ids := make([]int, 0, 129)
	for n := 1; n <= maxPendingEventRecoveries; n++ {
		// none answered: the REST worker has its own deadlines, and the maximum recovery time bounds it
		ids = append(ids, f.event(live, n))
	}
	ids = append(ids, f.event(pre, 1))
	f.advance(6 * time.Hour)
	expect(t, f.m.counters.eventExpired, 0, "at the maximum recovery time")
	f.advance(time.Second)
	expect(t, f.m.counters.eventExpired, 129, "128 of one producer, 1 of the other")
	for _, id := range ids {
		end := f.expectEnded(id, timedOut)
		if !strings.Contains(end.err.Error(), "6h0m0s") {
			t.Fatalf("reason: %v", end.err)
		}
	}
	f.expectNoEventState()
	f.event(live, 1)
}

// ---- the deadlines

// RecoveryMachineTest: anAcceptedEventRecoveryWhoseSnapshotCompleteNeverComesGivesUpItsSlotAfterTheDeadline
func TestMachineEvents_AcceptedEventRecoveryWhoseSnapshotCompleteNeverComesGivesUpItsSlotAfterTheDeadline(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	var first int
	for n := 1; n <= maxPendingEventRecoveries; n++ {
		id := f.event(live, n)
		if n == 1 {
			first = id
		}
		f.acceptEvent(id)
	}
	f.advance(f.m.settings.snapshotCompleteTimeout)
	expect(t, f.m.counters.eventExpired, 0, "at the deadline")
	if _, err := f.askEvent(live, 1000, false); !errors.Is(err, ErrTooManyPendingEventRecoveries) {
		t.Fatalf("the 129th: %v", err)
	}
	f.advance(time.Second)
	expect(t, f.m.counters.eventExpired, 128, "expired")
	f.event(live, 1000)
	end := f.expectEnded(first, timedOut)
	if !strings.Contains(end.err.Error(), f.m.settings.snapshotCompleteTimeout.String()) {
		t.Fatalf("reason: %v", end.err)
	}
}

// RecoveryMachineTest: anAcceptedEventRecoveryWhoseSnapshotKeepsArrivingIsNotGivenUpAndCompletes
func TestMachineEvents_AcceptedEventRecoveryWhoseSnapshotKeepsArrivingIsNotGivenUpAndCompletes(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	id := f.recoverEvent(live)
	f.acceptEvent(id)
	// four minutes apart, 16 in all: more than the deadline, never that long a silence
	for range 4 {
		f.advance(4 * time.Minute)
		f.processed(1, live, f.now(), id)
	}
	expect(t, f.m.counters.eventExpired, 0, "expired")
	f.expectNotEnded(id)
	f.snapshotComplete(1, live, id)
	f.expectEnded(id, completed)
}

// RecoveryMachineTest: anEventRecoveryIsGivenUpAfterTheDeadlineSinceTheLastOfItsSnapshotWasHeard
func TestMachineEvents_EventRecoveryIsGivenUpAfterTheDeadlineSinceTheLastOfItsSnapshotWasHeard(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	id := f.recoverEvent(live)
	f.acceptEvent(id)
	f.advance(4 * time.Minute)
	f.processed(1, live, f.now(), id)
	f.advance(f.m.settings.snapshotCompleteTimeout)
	expect(t, f.m.counters.eventExpired, 0, "five minutes after the last of it")
	f.advance(time.Second)
	expect(t, f.m.counters.eventExpired, 1, "expired")
	f.expectEnded(id, timedOut)
}

// RecoveryMachineTest: theDeadlineOfAnEventRecoveryCountsFromTheApisAcceptanceNotFromTheRequest
func TestMachineEvents_DeadlineOfAnEventRecoveryCountsFromTheApisAcceptanceNotFromTheRequest(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	id := f.recoverEvent(live)
	f.advance(4 * time.Minute)
	f.acceptEvent(id)
	f.advance(f.m.settings.snapshotCompleteTimeout)
	expect(t, f.m.counters.eventExpired, 0, "five minutes after the acceptance")
	f.advance(time.Second)
	expect(t, f.m.counters.eventExpired, 1, "expired")
}

// RecoveryMachineTest: anEventRecoveryBehindAProducerRecoveryWhoseSnapshotIsStillBeingTakenIsNotGivenUp
func TestMachineEvents_EventRecoveryBehindAProducerRecoveryWhoseSnapshotIsStillBeingTakenIsNotGivenUp(t *testing.T) {
	f := newFeed(t)
	f.open(1, prematch)
	f.startFeed()
	f.alive(pre)
	recovery := f.lastSnapshot(pre)
	f.accept(recovery)
	f.clock.advance(time.Second)
	id := f.recoverEvent(pre)
	f.acceptEvent(id)
	// the recovery's snapshot is taken for 16 minutes, with the event's snapshot queued behind it
	for range 4 {
		f.runWithAlives(4 * time.Minute)
		f.processed(1, pre, f.now(), recovery.requestID)
	}
	expect(t, f.m.counters.eventExpired, 0, "while the queue moves")
	f.expectNotEnded(id)
	f.runWithAlives(f.m.settings.snapshotCompleteTimeout)
	expect(t, f.m.counters.eventExpired, 0, "five minutes after the last of it")
	f.runWithAlives(time.Second)
	expect(t, f.m.counters.eventExpired, 1, "expired")
}

// RecoveryMachineTest: aSnapshotCompleteOfOneSessionPutsOffTheDeadlineForTheOthers
func TestMachineEvents_SnapshotCompleteOfOneSessionPutsOffTheDeadlineForTheOthers(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.open(2, all)
	f.startFeed()
	id := f.recoverEvent(live)
	f.acceptEvent(id)
	f.advance(4 * time.Minute)
	f.snapshotComplete(1, live, id)
	f.advance(f.m.settings.snapshotCompleteTimeout)
	expect(t, f.m.counters.eventExpired, 0, "expired")
	f.advance(time.Second)
	expect(t, f.m.counters.eventExpired, 1, "expired")
}

// RecoveryMachineTest: aSnapshotCompleteOfASessionRepeatedIsNoSignOfLifeForTheOthers
func TestMachineEvents_SnapshotCompleteOfASessionRepeatedIsNoSignOfLifeForTheOthers(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.open(2, all)
	f.startFeed()
	id := f.recoverEvent(live)
	f.acceptEvent(id)
	f.advance(4 * time.Minute)
	f.snapshotComplete(1, live, id)
	f.advance(4 * time.Minute)
	f.snapshotComplete(1, live, id)
	f.advance(time.Minute)
	expect(t, f.m.counters.eventExpired, 0, "five minutes after the first one")
	f.advance(time.Second)
	expect(t, f.m.counters.eventExpired, 1, "expired")
	f.expectEnded(id, timedOut)
}

// RecoveryMachineTest: aLiveMessageGeneratedBeforeAnEventRecoveryWasAskedForPutsOffItsDeadline
func TestMachineEvents_LiveMessageGeneratedBeforeAnEventRecoveryWasAskedForPutsOffItsDeadline(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	id := f.recoverEvent(live)
	askedAt := f.now()
	f.acceptEvent(id)
	f.advance(4 * time.Minute)
	// queued ahead of the snapshot: sent just before the request
	f.processed(1, live, askedAt.Add(-time.Millisecond), 0)
	f.advance(f.m.settings.snapshotCompleteTimeout)
	expect(t, f.m.counters.eventExpired, 0, "five minutes after the last of it")
	f.advance(time.Second)
	expect(t, f.m.counters.eventExpired, 1, "expired")
}

// RecoveryMachineTest: anAliveGeneratedBeforeAnEventRecoveryWasAskedForPutsOffItsDeadline
func TestMachineEvents_AliveGeneratedBeforeAnEventRecoveryWasAskedForPutsOffItsDeadline(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	id := f.recoverEvent(live)
	askedAt := f.now()
	f.acceptEvent(id)
	f.advance(4 * time.Minute)
	f.sessionAliveAt(1, live, askedAt.Add(-time.Millisecond))
	f.advance(f.m.settings.snapshotCompleteTimeout)
	expect(t, f.m.counters.eventExpired, 0, "five minutes after the last of it")
	f.advance(time.Second)
	expect(t, f.m.counters.eventExpired, 1, "expired")
}

// RecoveryMachineTest: aLiveMessageOrAnAliveGeneratedWhenAnEventRecoveryWasAskedForOrAfterDoesNotPutOffItsDeadline
func TestMachineEvents_LiveMessageOrAliveGeneratedWhenAnEventRecoveryWasAskedForOrAfterDoesNotPutOffItsDeadline(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	id := f.recoverEvent(live)
	askedAt := f.now()
	f.acceptEvent(id)
	f.advance(4 * time.Minute)
	// the snapshot does not queue behind what the producer sent since
	f.processed(1, live, askedAt, 0)
	f.sessionAliveAt(1, live, askedAt)
	f.processed(1, live, f.now(), 0)
	f.sessionAliveAt(1, live, f.now())
	f.advance(time.Minute)
	expect(t, f.m.counters.eventExpired, 0, "five minutes in")
	f.advance(time.Second)
	expect(t, f.m.counters.eventExpired, 1, "expired")
}

// RecoveryMachineTest: theSnapshotOfAnEventRecoveryAskedForEarlierPutsOffTheDeadlineOfOneAskedForAfterIt
func TestMachineEvents_SnapshotOfAnEventRecoveryAskedForEarlierPutsOffTheDeadlineOfOneAskedForAfterIt(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	earlier := f.event(live, 1)
	f.acceptEvent(earlier)
	f.advance(time.Second)
	later := f.event(live, 2)
	f.acceptEvent(later)
	f.advance(4 * time.Minute)
	f.processed(1, live, f.now(), earlier)
	f.advance(time.Minute + time.Second)
	expect(t, f.m.counters.eventExpired, 0,
		"five minutes after the later one was accepted: it is behind the snapshot that came")
	f.advance(4 * time.Minute)
	expect(t, f.m.counters.eventExpired, 2, "five minutes after it")
}

// RecoveryMachineTest: theSnapshotOfAnEventRecoveryAskedForAfterDoesNotPutOffTheDeadlineOfAnEarlierOne
func TestMachineEvents_SnapshotOfAnEventRecoveryAskedForAfterDoesNotPutOffTheDeadlineOfAnEarlierOne(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	earlier := f.event(live, 1)
	f.acceptEvent(earlier)
	f.advance(time.Second)
	later := f.event(live, 2)
	f.acceptEvent(later)
	f.advance(4 * time.Minute)
	f.processed(1, live, f.now(), later)
	f.advance(time.Minute)
	expect(t, f.m.counters.eventExpired, 1, "the earlier one's snapshot does not queue behind the later one's")
	f.expectEnded(earlier, timedOut)
	f.expectNotEnded(later)
}

// What was asked for earlier is ordered by when, never by the value of its id: the
// ids here run backwards.
func TestMachineEvents_OrderOfEventRecoveriesIsByWhenAskedForNotByTheValueOfTheirIds(t *testing.T) {
	askedBackwards := func(t *testing.T) (f *feed, earlier, later int) {
		f = newFeed(t)
		ids := []int{50, 7}
		f.m.nextID = func() int {
			id := ids[0]
			ids = ids[1:]
			return id
		}
		f.open(1, all)
		f.startFeed()
		earlier = f.event(live, 1)
		f.acceptEvent(earlier)
		f.advance(time.Second)
		later = f.event(live, 2)
		f.acceptEvent(later)
		expect(t, earlier, 50, "the earlier one's id")
		expect(t, later, 7, "the later one's id")
		return f, earlier, later
	}
	t.Run("the snapshot of the earlier puts off the later", func(t *testing.T) {
		f, earlier, later := askedBackwards(t)
		f.advance(4 * time.Minute)
		f.processed(1, live, f.now(), earlier)
		f.advance(time.Minute + time.Second)
		f.expectNotEnded(later)
	})
	t.Run("the snapshot of the later does not put off the earlier", func(t *testing.T) {
		f, earlier, later := askedBackwards(t)
		f.advance(4 * time.Minute)
		f.processed(1, live, f.now(), later)
		f.advance(time.Minute)
		f.expectEnded(earlier, timedOut)
		f.expectNotEnded(later)
	})
}

// RecoveryMachineTest: theSnapshotOfAProducerRecoveryAskedForAfterAnEventRecoveryDoesNotPutOffItsDeadline
func TestMachineEvents_SnapshotOfAProducerRecoveryAskedForAfterAnEventRecoveryDoesNotPutOffItsDeadline(t *testing.T) {
	f := newFeed(t)
	f.open(1, prematch)
	f.startFeed()
	id := f.recoverEvent(pre)
	f.acceptEvent(id)
	f.clock.advance(time.Second)
	f.alive(pre)
	recovery := f.lastSnapshot(pre)
	f.accept(recovery)
	for range 4 {
		f.runWithAlives(70 * time.Second)
		f.processed(1, pre, f.now(), recovery.requestID)
	}
	f.runWithAlives(30 * time.Second)
	expect(t, f.m.counters.eventExpired, 1, "five minutes since it was accepted")
	f.expectEnded(id, timedOut)
}

// A producer's recovery and an event recovery are ordered by when each was asked
// for, never by the value of its id: the first asked for has the greater id here.
func TestMachineEvents_OrderOfAProducerRecoveryAndAnEventRecoveryIsByWhenAskedForNotByTheirIds(t *testing.T) {
	// asked puts the first of the two asked for at id 50 and the second at id 7, both
	// accepted, and says the producer recovery's request id and the event recovery's.
	asked := func(t *testing.T, eventFirst bool) (f *feed, recovery, event int) {
		f = newFeed(t)
		ids := []int{50, 7}
		f.m.nextID = func() int {
			id := ids[0]
			ids = ids[1:]
			return id
		}
		f.open(1, prematch)
		f.startFeed()
		if eventFirst {
			event = f.recoverEvent(pre)
			f.acceptEvent(event)
			f.clock.advance(time.Second)
			f.alive(pre)
			recovery = f.lastSnapshot(pre).requestID
			f.accept(f.lastSnapshot(pre))
		} else {
			f.alive(pre)
			recovery = f.lastSnapshot(pre).requestID
			f.accept(f.lastSnapshot(pre))
			f.clock.advance(time.Second)
			event = f.recoverEvent(pre)
			f.acceptEvent(event)
		}
		return f, recovery, event
	}
	t.Run("the snapshot of the recovery asked for first puts off the event recovery", func(t *testing.T) {
		f, recovery, _ := asked(t, false)
		f.runWithAlives(4 * time.Minute)
		f.processed(1, pre, f.now(), recovery)
		f.runWithAlives(f.m.settings.snapshotCompleteTimeout)
		expect(t, f.m.counters.eventExpired, 0, "five minutes after the snapshot")
		f.runWithAlives(time.Second)
		expect(t, f.m.counters.eventExpired, 1, "expired")
	})
	t.Run("the snapshot of the event recovery asked for first puts off the recovery", func(t *testing.T) {
		f, _, event := asked(t, true)
		f.runWithAlives(4 * time.Minute)
		f.processed(1, pre, f.now(), event)
		f.runWithAlives(f.m.settings.snapshotCompleteTimeout)
		expect(t, f.m.counters.timedOut, 0, "five minutes after the snapshot")
		f.runWithAlives(time.Second)
		expect(t, f.m.counters.timedOut, 1, "timed out")
	})
	t.Run("the snapshot of the event recovery asked for second puts off nothing of the recovery", func(t *testing.T) {
		f, _, event := asked(t, false)
		f.runWithAlives(4 * time.Minute)
		f.processed(1, pre, f.now(), event)
		f.runWithAlives(time.Minute + time.Second)
		expect(t, f.m.counters.timedOut, 1, "timed out")
	})
	t.Run("the snapshot of the recovery asked for second puts off nothing of the event recovery", func(t *testing.T) {
		f, recovery, _ := asked(t, true)
		f.runWithAlives(4 * time.Minute)
		f.processed(1, pre, f.now(), recovery)
		f.runWithAlives(time.Minute + time.Second)
		expect(t, f.m.counters.eventExpired, 1, "expired")
	})
}

// What an event recovery that was refused or timed out still sends is queued ahead
// of the snapshot of one asked for after it.
func TestMachineEvents_SnapshotOfAnEventRecoveryThatEndedPutsOffTheDeadlineOfOneAskedForAfterIt(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	ended := f.event(live, 1)
	f.refuseEvent(ended)
	f.advance(time.Second)
	id := f.event(live, 2)
	f.acceptEvent(id)
	f.advance(4 * time.Minute)
	f.processed(1, live, f.now(), ended)
	f.advance(time.Minute + time.Second)
	f.expectNotEnded(id)
	f.advance(4 * time.Minute)
	f.expectEnded(id, timedOut)
}

// What a producer's recovery that failed still sends is queued ahead of the
// snapshot of an event recovery asked for after it.
func TestMachineEvents_SnapshotOfAProducerRecoveryThatFailedPutsOffTheDeadlineOfAnEventRecoveryAskedForAfterIt(t *testing.T) {
	f := newFeed(t)
	f.open(1, prematch)
	f.startFeed()
	f.alive(pre)
	recovery := f.lastSnapshot(pre)
	f.refuse(recovery)
	f.clock.advance(time.Second)
	id := f.recoverEvent(pre)
	f.acceptEvent(id)
	f.runWithAlives(4 * time.Minute)
	f.processed(1, pre, f.now(), recovery.requestID)
	f.runWithAlives(time.Minute + time.Second)
	f.expectNotEnded(id)
	f.runWithAlives(4 * time.Minute)
	f.expectEnded(id, timedOut)
}

// What an event recovery that failed still sends is queued ahead of the snapshot of
// a producer's recovery asked for after it.
func TestMachineEvents_SnapshotOfAnEventRecoveryThatFailedPutsOffTheDeadlineOfAProducerRecoveryAskedForAfterIt(t *testing.T) {
	f := newFeed(t)
	f.open(1, prematch)
	f.startFeed()
	failed := f.recoverEvent(pre)
	f.refuseEvent(failed)
	f.clock.advance(time.Second)
	f.alive(pre)
	recovery := f.lastSnapshot(pre)
	f.accept(recovery)
	f.runWithAlives(4 * time.Minute)
	f.processed(1, pre, f.now(), failed)
	f.runWithAlives(time.Minute + time.Second)
	expect(t, f.m.counters.timedOut, 0, "the recovery is not given up: its snapshot queues behind the event's")
	f.runWithAlives(4 * time.Minute)
	expect(t, f.m.counters.timedOut, 1, "five minutes after")
}

// RecoveryMachineTest: theMaximumRecoveryTimeBoundsAnEventRecoveryWhoseSnapshotKeepsArriving
func TestMachineEvents_MaximumRecoveryTimeBoundsAnEventRecoveryWhoseSnapshotKeepsArriving(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	id := f.recoverEvent(live)
	f.acceptEvent(id)
	for range 90 {
		f.advance(4 * time.Minute)
		f.processed(1, live, f.now(), id)
	}
	expect(t, f.m.counters.eventExpired, 0, "six hours in")
	f.advance(4 * time.Minute)
	expect(t, f.m.counters.eventExpired, 1, "expired")
	end := f.expectEnded(id, timedOut)
	if !strings.Contains(end.err.Error(), "6h0m0s") {
		t.Fatalf("reason: %v", end.err)
	}
	f.expectNoEventState()
}

// RecoveryMachineTest: aSnapshotCompleteAfterAnEventRecoveryWasGivenUpIsCountedAndIgnored
func TestMachineEvents_SnapshotCompleteAfterAnEventRecoveryWasGivenUpIsCountedAndIgnored(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	id := f.recoverEvent(live)
	f.acceptEvent(id)
	f.advance(f.m.settings.snapshotCompleteTimeout + time.Second)
	expect(t, f.m.counters.eventExpired, 1, "expired")
	unknown := f.m.counters.unknownCompletions
	f.snapshotComplete(1, live, id)
	expect(t, f.m.counters.unknownCompletions, unknown+1, "unknown completions")
	f.expectEnded(id, timedOut)
}

// ---- what ends one: a lost queue, and nothing else

// RecoveryMachineTest: aLostConnectionGivesUpTheEventRecoveriesInFlightAndAnswersThoseStillWaiting
func TestMachineEvents_LostConnectionGivesUpTheEventRecoveriesInFlight(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	answered := f.event(live, 1)
	f.acceptEvent(answered)
	waiting := f.event(live, 2)
	f.m.connectionDown()
	for _, id := range []int{answered, waiting} {
		end := f.expectEnded(id, failedEnd)
		if end.err == nil {
			t.Fatal("a failed one has a reason")
		}
	}
	expect(t, f.m.counters.eventAbandoned, 2, "given up")
	f.m.connectionUp()
	f.snapshotComplete(1, live, answered)
	expect(t, len(f.out.outcomes), 2, "given up: nothing more")
	f.expectNoEventState()
}

// RecoveryMachineTest: aSessionsLostChannelGivesUpTheEventRecoveriesWaitingForIt
func TestMachineEvents_SessionsLostChannelGivesUpTheEventRecoveriesWaitingForIt(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.open(2, all)
	f.startFeed()
	seenByBoth := f.event(live, 1)
	seenByOne := f.event(live, 2)
	f.acceptEvent(seenByBoth)
	f.acceptEvent(seenByOne)
	f.snapshotComplete(2, live, seenByBoth)
	f.snapshotComplete(1, live, seenByOne)

	f.m.channelLost(sid(2))
	expect(t, f.m.counters.eventAbandoned, 1, "the one session 2 had yet to see")
	f.expectEnded(seenByOne, failedEnd)
	f.snapshotComplete(1, live, seenByBoth)
	f.expectEnded(seenByBoth, completed)
}

// A session's channel lost takes no event recovery of a producer it does not
// receive.
func TestMachineEvents_ChannelLostOfASessionThatDoesNotReceiveTheProducerLeavesItsEventRecoveries(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.open(2, prematch)
	f.startFeed()
	id := f.recoverEvent(live)
	f.acceptEvent(id)
	f.m.channelLost(sid(2))
	f.expectNotEnded(id)
	// and it is not deferred by a channel it does not wait for
	other := f.event(live, 2)
	expect(t, f.lastEventAsked().requestID, other, "asked for at once")
}

// A producer going down because a session processes it late loses no queue.
func TestMachineEvents_ProducerDownForProcessingDelayLeavesTheEventRecoveries(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.bothUp(1)
	id := f.recoverEvent(live)
	f.acceptEvent(id)
	f.live(1, live, 25*time.Second)
	f.advance(10 * time.Millisecond)
	expect(t, f.lastStatus(live).cause, causeProcessingDelay, "cause")
	f.expectNotEnded(id)
	f.snapshotComplete(1, live, id)
	f.expectEnded(id, completed)
}

// A producer going down for no alive loses no queue.
func TestMachineEvents_ProducerDownForNoAliveLeavesTheEventRecoveries(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.bothUp(1)
	id := f.recoverEvent(live)
	f.acceptEvent(id)
	f.advance(21 * time.Second)
	expect(t, f.lastStatus(live).cause, causeAliveIntervalViolation, "cause")
	f.expectNotEnded(id)
	f.snapshotComplete(1, live, id)
	f.expectEnded(id, completed)
}

// An alive that says the feed is unsubscribed from the producer loses no queue.
func TestMachineEvents_ProducerUnsubscribedLeavesTheEventRecoveries(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.bothUp(1)
	id := f.recoverEvent(live)
	f.acceptEvent(id)
	f.unsubscribed(live)
	expect(t, f.lastStatus(live).cause, causeUnsubscribed, "cause")
	f.expectNotEnded(id)
	f.snapshotComplete(1, live, id)
	f.expectEnded(id, completed)
}

// ---- who is waited for

// RecoveryMachineTest: aRecoveryNoSessionTakesSnapshotCompletesForCompletesWhenTheApiAcceptsIt
func TestMachineEvents_EventRecoveryNoSessionTakesSnapshotCompletesForCompletesWhenTheApiAcceptsIt(t *testing.T) {
	f := newFeed(t)
	f.open(1, aliveOne)
	f.startFeed()
	id := f.recoverEvent(live)
	f.expectNotEnded(id)
	f.acceptEvent(id)
	f.expectEnded(id, completed)
	f.expectNoEventState()
}

// RecoveryMachineTest: aSessionClosingBeforeTheApiAnswersAnEventRecoveryLeavesTheAnswerToDecide
func TestMachineEvents_SessionClosingBeforeTheApiAnswersAnEventRecoveryLeavesTheAnswerToDecide(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	accepted := f.event(live, 1)
	refused := f.event(live, 2)
	f.close(1)
	expect(t, len(f.out.outcomes), 0, "no snapshot_complete, no answer yet")

	f.refuseEvent(refused)
	f.expectEnded(refused, failedEnd)
	f.acceptEvent(accepted)
	f.expectEnded(accepted, completed)
	f.expectNoEventState()
}

// A session that closes leaves the set of those waited for: with the others'
// snapshot_complete in, the event recovery completes.
func TestMachineEvents_SessionThatClosesIsNoLongerWaitedFor(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.open(2, all)
	f.startFeed()
	id := f.recoverEvent(live)
	f.acceptEvent(id)
	f.snapshotComplete(1, live, id)
	f.expectNotEnded(id)
	f.close(2)
	f.expectEnded(id, completed)
	f.expectNoEventState()
}

// A session that saw the snapshot_complete and closed stays counted as having
// seen it, and its closing then is no completion of what others still await.
func TestMachineEvents_SessionThatSawTheSnapshotCompleteAndClosedLeavesTheRest(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.open(2, all)
	f.startFeed()
	id := f.recoverEvent(live)
	f.snapshotComplete(2, live, id)
	f.close(2)
	// the snapshot_complete stood for the API's acceptance, and session 1 is still awaited
	f.expectNotEnded(id)
	f.snapshotComplete(1, live, id)
	f.expectEnded(id, completed)
}

// A session opened after the event recovery was asked for is not waited for.
func TestMachineEvents_SessionOpenedAfterTheEventRecoveryWasAskedForIsNotAwaited(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	id := f.recoverEvent(live)
	f.open(2, all)
	f.acceptEvent(id)
	f.snapshotComplete(1, live, id)
	f.expectEnded(id, completed)
}

// A snapshot_complete of another producer under the event recovery's id is no
// completion of it.
func TestMachineEvents_SnapshotCompleteOfAnotherProducerCompletesNothing(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	id := f.recoverEvent(live)
	f.acceptEvent(id)
	f.snapshotComplete(1, pre, id)
	f.expectNotEnded(id)
	expect(t, f.m.counters.unknownCompletions, 1, "unknown completions")
}

// ---- the lookups

// The producer's recovery in flight is looked up before an event recovery in a
// snapshot_complete, as Java's does. No two requests of a feed share an id, so the
// test puts one under the id of the other.
func TestMachineEvents_SnapshotCompleteLooksForTheProducersRecoveryBeforeAnEventRecovery(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.alive(pre)
	recovery := f.lastSnapshot(pre)
	f.accept(recovery)
	f.m.eventRecoveries[recovery.requestID] = &eventActive{
		requestID: recovery.requestID, producerID: pre, eventID: matchN(1),
		awaited: map[uuid.UUID]bool{sid(1): true}, seen: map[uuid.UUID]bool{},
	}
	f.snapshotComplete(1, pre, recovery.requestID)
	expect(t, f.m.counters.completed, 1, "the producer's recovery completed")
	expect(t, len(f.m.eventRecoveries), 1, "the event recovery is still in flight")
	f.expectNotEnded(recovery.requestID)
}

// An answer is looked up among the event recoveries first, as Java's is.
func TestMachineEvents_AnswerLooksForAnEventRecoveryBeforeTheProducersRecovery(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.alive(pre)
	recovery := f.lastSnapshot(pre)
	f.m.eventRecoveries[recovery.requestID] = &eventActive{
		requestID: recovery.requestID, producerID: pre, eventID: matchN(1),
		awaited: map[uuid.UUID]bool{sid(1): true}, seen: map[uuid.UUID]bool{},
	}
	f.accept(recovery)
	if !f.m.eventRecoveries[recovery.requestID].accepted {
		t.Fatal("the answer did not reach the event recovery")
	}
	if f.m.byID[pre].active.accepted {
		t.Fatal("the answer reached the producer's recovery")
	}
}

// No request takes the id of one in flight, whichever kind: the machine draws again.
func TestMachineEvents_RequestIdOfOneInFlightIsDrawnAgain(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.alive(pre)
	recovery := f.lastSnapshot(pre)
	event := f.event(live, 1)
	drawn := []int{recovery.requestID, event, 0, 900}
	f.m.nextID = func() int {
		id := drawn[0]
		drawn = drawn[1:]
		return id
	}
	expect(t, f.event(live, 2), 900, "the first id not in flight")
	expect(t, len(drawn), 0, "ids drawn")
}

// The id of an event recovery that waits for a channel is taken too: its caller has it.
func TestMachineEvents_RequestIdOfAnEventRecoveryThatWaitsIsDrawnAgain(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.m.channelLost(sid(1))
	waiting := f.event(live, 1)
	drawn := []int{waiting, 901}
	f.m.nextID = func() int {
		id := drawn[0]
		drawn = drawn[1:]
		return id
	}
	expect(t, f.event(live, 2), 901, "the first id not in flight")
}

// ---- the deferral

// Java's deferral: an event recovery asked for while a session that receives its
// producer awaits its channel waits, and is asked for once the channel is back,
// under the request id its caller has.
func TestMachineEvents_EventRecoveryAskedForWhileASessionAwaitsItsChannelWaitsForIt(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.open(2, all)
	f.startFeed()
	f.m.channelLost(sid(2))
	id := f.recoverEvent(live)
	expect(t, len(f.eventsAsked()), 0, "asked for before the channel is back")
	expect(t, len(f.m.deferred), 1, "waiting")
	f.m.channelReopened(sid(2))
	expect(t, len(f.eventsAsked()), 1, "asked for once the channel is back")
	expect(t, f.lastEventAsked().requestID, id, "under the id its caller has")
	expect(t, len(f.m.deferred), 0, "waiting")
	// and awaited by both sessions
	f.acceptEvent(id)
	f.snapshotComplete(1, live, id)
	f.expectNotEnded(id)
	f.snapshotComplete(2, live, id)
	f.expectEnded(id, completed)
	f.expectNoEventState()
}

// The session that awaited its channel closing is as good as the channel back.
func TestMachineEvents_EventRecoveryThatWaitedForAChannelIsAskedForWhenTheSessionCloses(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.open(2, all)
	f.startFeed()
	f.m.channelLost(sid(2))
	id := f.recoverEvent(live)
	f.close(2)
	expect(t, len(f.eventsAsked()), 1, "asked for")
	f.acceptEvent(id)
	f.snapshotComplete(1, live, id)
	f.expectEnded(id, completed)
}

// One that waits is one request: asking again is it, and it takes room under the
// cap.
func TestMachineEvents_EventRecoveryThatWaitsIsCoalescedAndCountsAgainstTheCap(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.m.channelLost(sid(1))
	id := f.event(live, 1)
	again, err := f.askEvent(live, 1, false)
	if err != nil || again != id {
		t.Fatalf("again: %d, %v", again, err)
	}
	for n := 2; n <= maxPendingEventRecoveries; n++ {
		f.event(live, n)
	}
	if _, err := f.askEvent(live, 1000, false); !errors.Is(err, ErrTooManyPendingEventRecoveries) {
		t.Fatalf("the 129th: %v", err)
	}
	f.m.channelReopened(sid(1))
	expect(t, len(f.eventsAsked()), 128, "asked for once the channel is back")
}

// The deferral waits only for a channel of a session that receives the producer.
func TestMachineEvents_LostChannelOfASessionThatDoesNotReceiveTheProducerDefersNothing(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.open(2, prematch)
	f.startFeed()
	f.m.channelLost(sid(2))
	f.recoverEvent(live)
	expect(t, len(f.eventsAsked()), 1, "asked for at once")
	f.recoverEvent(pre)
	expect(t, len(f.eventsAsked()), 1, "the producer the session receives waits")
}

// A deferred one expires like one asked for: at the maximum recovery time.
func TestMachineEvents_DeferredEventRecoveryExpiresAtTheMaximumRecoveryTime(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.m.channelLost(sid(1))
	id := f.recoverEvent(live)
	f.advance(6 * time.Hour)
	f.expectNotEnded(id)
	f.advance(time.Second)
	end := f.expectEnded(id, timedOut)
	if !strings.Contains(end.err.Error(), "6h0m0s") {
		t.Fatalf("reason: %v", end.err)
	}
	expect(t, f.m.counters.eventExpired, 1, "expired")
	f.expectNoEventState()
	f.m.channelReopened(sid(1))
	expect(t, len(f.eventsAsked()), 0, "never asked for")
}

// With the connection down when the channel is back, the one that waited is
// refused, as one asked for then is.
func TestMachineEvents_DeferredEventRecoveryIsRefusedWhenTheChannelIsBackWithTheConnectionDown(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	f.m.channelLost(sid(1))
	id := f.recoverEvent(live)
	f.m.connectionDown()
	f.expectNotEnded(id)
	f.m.channelReopened(sid(1))
	end := f.expectEnded(id, failedEnd)
	expect(t, errors.Is(end.err, errEventRecoveryConnectionDown), true, "reason")
	expect(t, len(f.eventsAsked()), 0, "requests")
	f.expectNoEventState()
}

// ---- close

// At close, the event recoveries in flight and the ones that waited for a channel
// end as failed with the feed closed.
func TestMachineEvents_CloseFailsTheEventRecoveriesInFlightAndTheDeferredOnes(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.open(2, prematch)
	f.startFeed()
	unanswered := f.event(live, 1)
	accepted := f.event(live, 2)
	f.acceptEvent(accepted)
	f.m.channelLost(sid(2))
	waiting := f.event(pre, 1)
	f.m.beginClosing()
	f.m.close()
	for _, id := range []int{unanswered, accepted, waiting} {
		end := f.expectEnded(id, failedEnd)
		expect(t, errors.Is(end.err, errFeedClosing), true, "reason")
	}
	f.expectNoEventState()
	f.snapshotComplete(1, live, accepted)
	expect(t, len(f.out.outcomes), 3, "nothing more")
}

// A session that closes with the feed closing is not a completion of what it was
// waited for: the event recoveries fail as the machine closes.
func TestMachineEvents_SessionsClosingWithTheFeedCompleteNoEventRecovery(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	id := f.recoverEvent(live)
	f.acceptEvent(id)
	f.m.beginClosing()
	f.close(1)
	f.expectNotEnded(id)
	f.m.close()
	f.expectEnded(id, failedEnd)
}

// ---- no map grows

// Every event recovery leaves the machine's maps however it ends, and what it
// remembers of the ones that ended without completing is bounded.
func TestMachineEvents_EveryEndingLeavesNothingBehind(t *testing.T) {
	f := newFeed(t)
	f.open(1, all)
	f.startFeed()
	for n := 1; n <= 40; n++ {
		id := f.event(live, n)
		switch n % 5 {
		case 0: // completed
			f.acceptEvent(id)
			f.snapshotComplete(1, live, id)
		case 1: // refused
			f.refuseEvent(id)
		case 2: // given up with the connection
			f.m.connectionDown()
			f.m.connectionUp()
		case 3: // expired
			f.advance(6*time.Hour + time.Second)
		case 4: // given up with a channel
			f.m.channelLost(sid(1))
			f.m.channelReopened(sid(1))
		}
		f.expectEnded(id, map[int]types.RecoveryRequestStatus{
			0: completed, 1: failedEnd, 2: failedEnd, 3: timedOut, 4: failedEnd,
		}[n%5])
		f.expectNoEventState()
	}
	expect(t, len(f.out.outcomes), 40, "one outcome each")
	expect(t, len(f.m.byID[live].endedEvents) <= endedRemembered, true, "what is remembered is bounded")
	expect(t, len(f.m.byID[live].endedEvents), endedRemembered, "what is remembered")
}
