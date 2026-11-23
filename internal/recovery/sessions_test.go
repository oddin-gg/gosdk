package recovery

import (
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/oddin-gg/gosdk/types"
)

func TestSessionRegistry_RecordHoldsInterestAndProducersOfInterest(t *testing.T) {
	m, _ := openedManagerWithActors(t, 1, 2, 3)

	// The fixture catalog: 1 is live, 2 is prematch, 3 has both scopes.
	for _, tc := range []struct {
		interest types.MessageInterest
		want     []int
	}{
		{types.LiveOnlyMessageInterest, []int{1, 3}},
		{types.PrematchOnlyMessageInterest, []int{2, 3}},
		{types.AllMessageInterest, []int{1, 2, 3}},
	} {
		id := uuid.New()
		m.OnClientSessionOpened(id, tc.interest)
		rec, ok := m.sessionFactsOf(id)
		if !ok {
			t.Fatalf("%s: session not registered", tc.interest)
		}
		if rec.id != id || rec.interest != tc.interest {
			t.Fatalf("%s: record = %+v", tc.interest, rec)
		}
		if !slices.Equal(rec.producers, tc.want) {
			t.Fatalf("%s: producers = %v, want %v", tc.interest, rec.producers, tc.want)
		}
	}
}

// The alive-only interest has no lane: it is not registered, takes no
// snapshot_complete and keeps no facts, so it holds no recovery. That is the
// SDK's own alive session and a client's alive-only subscription alike.
func TestSessionRegistry_AliveOnlyHasNoLane(t *testing.T) {
	m, _ := openedManagerWithActors(t, 1)
	id := uuid.New()
	m.OnClientSessionOpened(id, types.SystemAliveOnly)

	if _, ok := m.sessionFactsOf(id); ok {
		t.Fatal("an alive-only session was registered")
	}
	if len(m.clientSessions) != 0 {
		t.Fatalf("registry = %v, want empty", m.clientSessions)
	}
	if hasLane(types.SystemAliveOnly) {
		t.Fatal("an alive-only session has no lane")
	}
	m.OnMessageProcessed(id, 1, time.Now(), systemClock{}.now(), 0)
	m.OnSessionAlive(id, 1, time.Now(), systemClock{}.now(), true, types.SystemAliveOnly)
	if _, ok := m.sessionFactsOf(id); ok {
		t.Fatal("a fact registered an alive-only session")
	}
	if m.clientSessionsCanComplete(1) {
		t.Fatal("an alive-only session must not count as a place a replay can reach")
	}
}

// Every session with a lane takes snapshot_complete, fixed when it opens: a
// low-priority session next to a high-priority one records a completion like
// any other, and the other sessions coming and going change nothing for it.
func TestSessionRegistry_LowPriorityNextToHighTakesSnapshotCompleteToo(t *testing.T) {
	m, _ := openedManagerWithActors(t, 1)
	low, hi := uuid.New(), uuid.New()
	m.OnClientSessionOpened(low, types.LowPriorityOnlyMessageInterest)
	m.OnClientSessionOpened(hi, types.HiPriorityOnlyMessageInterest)

	for name, id := range map[string]uuid.UUID{"low": low, "hi": hi} {
		m.recordSnapshotComplete(id, 1, 5)
		rec, ok := m.sessionFactsOf(id)
		if !ok || rec.snapshotComplete[1].requestID != 5 {
			t.Fatalf("%s: snapshot_complete fact = %+v (ok=%v)", name, rec.snapshotComplete, ok)
		}
	}

	m.OnFeedSessionGone(hi)
	m.recordSnapshotComplete(low, 1, 6)
	rec, ok := m.sessionFactsOf(low)
	if !ok || rec.snapshotComplete[1].requestID != 6 {
		t.Fatalf("after the high-priority session is gone: %+v (ok=%v)", rec.snapshotComplete, ok)
	}
}

// What a session reports is kept per session and per producer, with the ids,
// the request id and the stamps it came with; the later fact replaces the
// earlier of the same producer, and another session's are its own.
func TestSessionRegistry_KeepsFactsPerSessionAndProducer(t *testing.T) {
	m, _ := openedManagerWithActors(t, 1, 3)
	clk := newFakeClock()
	m.clk = clk
	a, b := uuid.New(), uuid.New()
	m.OnClientSessionOpened(a, types.AllMessageInterest)
	m.OnClientSessionOpened(b, types.AllMessageInterest)

	gen1, gen2 := time.Unix(1_700_000_000, 0), time.Unix(1_700_000_005, 0)
	taken1 := clk.now()
	clk.advance(time.Second)
	taken2 := clk.now()

	m.OnMessageProcessed(a, 1, gen1, taken1, 0)
	m.OnMessageProcessed(a, 1, gen2, taken2, 55)
	m.OnMessageProcessed(a, 3, gen1, taken1, 0)

	ra, _ := m.sessionFactsOf(a)
	f := ra.processed[1]
	if !f.generatedAt.Equal(gen2) || f.taken != taken2 || f.requestID != 55 || f.count != 2 {
		t.Fatalf("session a, producer 1 = %+v, want the second message (count 2)", f)
	}
	if f3 := ra.processed[3]; !f3.generatedAt.Equal(gen1) || f3.count != 1 {
		t.Fatalf("session a, producer 3 = %+v", f3)
	}
	if !ra.processed[1].taken.wall.After(taken1.wall) || ra.processed[1].taken.mono <= taken1.mono {
		t.Fatal("the later fact must carry the later stamp")
	}
	if rb, _ := m.sessionFactsOf(b); len(rb.processed) != 0 {
		t.Fatalf("session b has facts of session a: %+v", rb.processed)
	}

	m.OnSessionAlive(b, 3, gen2, taken2, true, types.AllMessageInterest)
	rb, _ := m.sessionFactsOf(b)
	if al := rb.alive[3]; !al.generatedAt.Equal(gen2) || al.taken != taken2 || !al.subscribed {
		t.Fatalf("session b alive = %+v", al)
	}
	if ra2, _ := m.sessionFactsOf(a); len(ra2.alive) != 0 {
		t.Fatalf("session a has an alive of session b: %+v", ra2.alive)
	}
}

// A snapshot_complete admitted to the actor is recorded for its session at
// the manager's clock; one that is not admitted is not, since the broker
// will hand it over again.
func TestSessionRegistry_SnapshotCompleteRecordedOnlyWhenAdmitted(t *testing.T) {
	m, _ := openedManagerWithActors(t, 3)
	clk := newFakeClock()
	m.clk = clk
	id := uuid.New()
	m.OnClientSessionOpened(id, types.LiveOnlyMessageInterest)

	if err := m.OnSnapshotCompleteReceived(t.Context(), id, 3, 77, types.LiveOnlyMessageInterest); err != nil {
		t.Fatal(err)
	}
	rec, _ := m.sessionFactsOf(id)
	f, ok := rec.snapshotComplete[3]
	if !ok || f.requestID != 77 || f.at != clk.now() {
		t.Fatalf("snapshot complete = %+v (ok=%v), want request 77 at the fake clock's reading", f, ok)
	}

	m.Close()
	if err := m.OnSnapshotCompleteReceived(t.Context(), id, 3, 78, types.LiveOnlyMessageInterest); err == nil {
		t.Fatal("a closed manager admitted a snapshot_complete")
	}
	rec, _ = m.sessionFactsOf(id)
	if rec.snapshotComplete[3].requestID != 77 {
		t.Fatalf("a completion that was not admitted was recorded: %+v", rec.snapshotComplete[3])
	}
}

// Registration comes before the first delivery's facts and removal after the
// last of them; a fact that arrives after the session is gone is dropped and
// does not bring it back.
func TestSessionRegistry_FactAfterRemovalIsDropped(t *testing.T) {
	m, _ := openedManagerWithActors(t, 1)
	id := uuid.New()

	// Before it is registered a fact has nowhere to go.
	m.OnMessageProcessed(id, 1, time.Now(), systemClock{}.now(), 0)
	if _, ok := m.sessionFactsOf(id); ok {
		t.Fatal("a fact registered a session")
	}

	m.OnClientSessionOpened(id, types.AllMessageInterest)
	m.OnMessageProcessed(id, 1, time.Now(), systemClock{}.now(), 0)
	if rec, ok := m.sessionFactsOf(id); !ok || rec.processed[1].count != 1 {
		t.Fatalf("record = %+v (ok=%v), want one processed fact", rec, ok)
	}

	m.OnFeedSessionGone(id)
	m.OnMessageProcessed(id, 1, time.Now(), systemClock{}.now(), 0)
	m.OnSessionAlive(id, 1, time.Now(), systemClock{}.now(), true, types.AllMessageInterest)
	if err := m.OnSnapshotCompleteReceived(t.Context(), id, 1, 9, types.AllMessageInterest); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.sessionFactsOf(id); ok {
		t.Fatal("a late fact brought a removed session back")
	}
	if len(m.clientSessions) != 0 {
		t.Fatalf("registry = %v, want empty", m.clientSessions)
	}
}

// The registry is the one the deadline guards read: what they answer is
// unchanged by the facts the records now keep.
func TestSessionRegistry_DerivedViewsUnchanged(t *testing.T) {
	m, _ := openedManagerWithActors(t, 1, 2, 3)
	live := uuid.New()
	m.OnClientSessionOpened(live, types.LiveOnlyMessageInterest)
	if !m.clientSessionsCanComplete(1) || m.clientSessionsCanComplete(2) || m.clientSessionsCanComplete(3) {
		t.Fatal("completable producers changed with the record")
	}
	m.OnMessageProcessingStarted(live, 1, time.Now().Add(-time.Hour))
	if !m.clientProcessingStalled(1, time.Now(), time.Minute) {
		t.Fatal("a stalled live session must hold the deadline of a live producer")
	}
	if m.clientProcessingStalled(2, time.Now(), time.Minute) {
		t.Fatal("a stalled live session must not hold the deadline of a prematch producer")
	}
}

// The alive of a client session reaches the actor as the sign that the
// client side is getting messages, whether or not the registry still holds
// the session: the fact goes to the record, the alive to the actor.
func TestManager_OnSessionAliveForwardsToActor(t *testing.T) {
	m, pm := openedManagerWithActors(t, 1)
	id := uuid.New()
	m.OnClientSessionOpened(id, types.AllMessageInterest)

	created := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	m.OnSessionAlive(id, 1, created, systemClock{}.now(), true, types.AllMessageInterest)
	actorBarrier(t, m, pm, 1)
	if got := actorOf(t, m, 1).lastUserSessionAlive; !got.Equal(created) {
		t.Fatalf("lastUserSessionAlive = %v, want %v", got, created)
	}

	m.OnFeedSessionGone(id)
	later := created.Add(time.Minute)
	m.OnSessionAlive(id, 1, later, systemClock{}.now(), true, types.AllMessageInterest)
	actorBarrier(t, m, pm, 1)
	if _, ok := m.sessionFactsOf(id); ok {
		t.Fatal("an alive brought a removed session back")
	}
	if got := actorOf(t, m, 1).lastUserSessionAlive; !got.Equal(later) {
		t.Fatalf("after the session is gone: lastUserSessionAlive = %v, want %v", got, later)
	}
}
