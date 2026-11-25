package recovery

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/oddin-gg/gosdk/types"
)

// These tests run the recovery manager, with its actor and machine, over the
// fixture API and on a clock the test moves, the way the feed drives it: sessions
// register, alives and messages are told, snapshot_completes come back, and what
// the client sees is read through the manager's own accessors.

func matchURN(n int) types.URN { return types.URN{Prefix: "od", Type: "match", ID: n} }

// The first recovery: the start asks nothing, the first alive of a producer asks
// for a full snapshot, and the producer is up once every session that receives it
// has seen the snapshot_complete.
func TestManager_StartAliveRequestSnapshotCompleteOnEverySessionProducerUp(t *testing.T) {
	r := newRig(t)
	r.open()
	s1, s2 := uuid.New(), uuid.New()
	r.openSession(s1, types.AllMessageInterest)
	r.openSession(s2, types.AllMessageInterest)
	r.sync()
	if got := len(r.hits.snapshots()); got != 0 {
		t.Fatalf("recoveries asked for before any alive: %d", got)
	}
	if _, ok := r.status(liveID); ok {
		t.Fatal("a producer status exists before the first recovery")
	}

	r.alive(liveID)
	asked := r.waitSnapshots(1)
	if asked[0].producer != "live" || asked[0].afterMillis != 0 {
		t.Fatalf("first request = %+v, want a full snapshot of the live producer", asked[0])
	}
	requestedAt := r.clock.Now().Wall()

	id := asked[0].requestID
	r.snapshotComplete(s1, liveID, id)
	r.sync()
	if _, ok := r.status(liveID); ok {
		t.Fatal("the producer came up on the snapshot_complete of one of its two sessions")
	}
	if !r.isDown(liveID) {
		t.Fatal("the producer is not flagged down while a session still awaits its snapshot_complete")
	}

	r.snapshotComplete(s2, liveID, id)
	eventually(t, "the live producer up", func() bool { s, ok := r.status(liveID); return ok && !s.IsDown() })
	s, _ := r.status(liveID)
	if s.ProducerStatusReason() != types.FirstRecoveryCompletedProducerStatusReason {
		t.Fatalf("up for %v, want the first recovery", s.ProducerStatusReason())
	}
	if r.isDown(liveID) {
		t.Fatal("the producer manager still flags the producer down")
	}
	if got := r.statuses(liveID); len(got) != 1 || got[0].IsDown() {
		t.Fatalf("streamed statuses = %d, want the one up", len(got))
	}
	// the recovery info of the accepted request, and the resume point at the ack
	eventually(t, "the recovery info", func() bool {
		p, _ := r.pm.GetProducer(t.Context(), liveID)
		return p.RecoveryInfo() != nil
	})
	p, _ := r.pm.GetProducer(t.Context(), liveID)
	if info := *p.RecoveryInfo(); info.RequestID() != id || !info.Successful() {
		t.Fatalf("recovery info = request %d successful %v, want %d, true", info.RequestID(), info.Successful(), id)
	}
	eventually(t, "the resume point", func() bool { return r.timestampForRecovery(liveID).Equal(requestedAt) })
}

// A lost channel takes its session's producers down and, once the queue is bound
// again, a recovery is asked for from what the session had finished: the
// checkpoint, not the last alive.
func TestManager_ChannelLostAndReopenedRecoversFromTheCheckpoint(t *testing.T) {
	r := newRig(t)
	r.open()
	s1, s2 := uuid.New(), uuid.New()
	r.openSession(s1, types.AllMessageInterest)
	r.openSession(s2, types.AllMessageInterest)
	r.alive(liveID)
	first := r.waitSnapshots(1)[0]
	r.snapshotComplete(s1, liveID, first.requestID)
	r.snapshotComplete(s2, liveID, first.requestID)
	eventually(t, "the live producer up", func() bool { s, ok := r.status(liveID); return ok && !s.IsDown() })

	r.clock.advance(10 * time.Second)
	r.alive(liveID)
	checkpoint2 := r.processed(s2, liveID) // session 2 finished a message at this time
	r.processed(s1, liveID)
	r.sync()
	r.clock.advance(5 * time.Second)
	r.alive(liveID)
	r.processed(s1, liveID) // session 1 is ahead of it

	r.sync() // what the sessions finished is taken before the loss
	r.mgr.OnFeedChannelLost(s2)
	eventually(t, "the producer down for the lost channel", func() bool {
		s, ok := r.status(liveID)
		return ok && s.IsDown() && s.ProducerStatusReason() == types.ConnectionDownProducerStatusReason
	})
	if !r.isDown(liveID) {
		t.Fatal("the producer manager does not flag the producer down")
	}
	// nothing is asked for before the new queue is bound, whatever alives come
	r.sync()
	r.clock.advance(time.Second)
	r.alive(liveID)
	r.sync()
	if got := len(r.hits.snapshots()); got != 1 {
		t.Fatalf("recoveries asked for while the channel awaits its queue: %d, want the first only", got)
	}

	r.mgr.OnFeedChannelRestored(s2)
	r.alive(liveID)
	second := r.waitSnapshots(2)[1]
	if second.requestID == first.requestID {
		t.Fatal("the second request reuses the first one's id")
	}
	if want := checkpoint2.UnixMilli(); second.afterMillis != want {
		t.Fatalf("recovery after %d, want session 2's checkpoint %d", second.afterMillis, want)
	}

	r.snapshotComplete(s1, liveID, second.requestID)
	r.snapshotComplete(s2, liveID, second.requestID)
	eventually(t, "the producer up again", func() bool { s, ok := r.status(liveID); return ok && !s.IsDown() })
	s, _ := r.status(liveID)
	if s.ProducerStatusReason() != types.ReturnedFromInactivityProducerStatusReason {
		t.Fatalf("up for %v, want returned from inactivity", s.ProducerStatusReason())
	}
	if r.isDown(liveID) {
		t.Fatal("the producer manager still flags the producer down")
	}
}

// An event recovery asked for through the public call comes back as a handle,
// is sent, and ends on the snapshot_complete of every session that receives its
// producer, with the stream telling of it.
func TestManager_EventRecoveryFromTheCallToItsHandlesEnd(t *testing.T) {
	r := newRig(t)
	r.open()
	s1, s2 := uuid.New(), uuid.New()
	r.openSession(s1, types.AllMessageInterest)
	r.openSession(s2, types.LiveOnlyMessageInterest)
	r.alive(liveID)
	first := r.waitSnapshots(1)[0]
	r.snapshotComplete(s1, liveID, first.requestID)
	r.snapshotComplete(s2, liveID, first.requestID)
	eventually(t, "the live producer up", func() bool { s, ok := r.status(liveID); return ok && !s.IsDown() })

	h, err := r.mgr.InitiateEventOddsRecoveryHandle(t.Context(), liveID, matchURN(7))
	if err != nil {
		t.Fatalf("InitiateEventOddsRecoveryHandle: %v", err)
	}
	if h.Status() != types.RecoveryStatusPending || h.ProducerID() != liveID || h.EventID() != matchURN(7) {
		t.Fatalf("handle = status %v producer %d event %v", h.Status(), h.ProducerID(), h.EventID())
	}
	if got, ok := r.mgr.LookupHandle(h.RequestID()); !ok || got != h {
		t.Fatal("the handle is not registered under its request id")
	}
	// the same request while it is in flight is the same handle
	again, err := r.mgr.InitiateEventOddsRecoveryHandle(t.Context(), liveID, matchURN(7))
	if err != nil || again != h {
		t.Fatalf("the same request again = %v, %v, want the handle in flight", again, err)
	}
	eventually(t, "the event recovery request", func() bool { return r.hits.eventRecover.Load() == 1 })
	req := r.hits.all()[len(r.hits.all())-1]
	if req.requestID != h.RequestID() || req.producer != "live" || req.event != "od:match:7" {
		t.Fatalf("request = %+v, want the handle's request for od:match:7 of live", req)
	}

	r.snapshotComplete(s1, liveID, h.RequestID())
	select {
	case <-h.Done():
		t.Fatal("the event recovery ended on the snapshot_complete of one of its two sessions")
	case <-time.After(50 * time.Millisecond):
	}
	r.snapshotComplete(s2, liveID, h.RequestID())
	select {
	case <-h.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the event recovery did not end on every session's snapshot_complete")
	}
	if res := h.Result(); res.Status != types.RecoveryStatusCompleted || res.Err != nil {
		t.Fatalf("result = %v, %v, want completed", res.Status, res.Err)
	}
	eventually(t, "the event recovery on the stream", func() bool {
		for _, m := range r.streamed() {
			if e := m.EventRecoveryMessage; e != nil && e.RequestID() == h.RequestID() && e.EventID() == matchURN(7) {
				return true
			}
		}
		return false
	})

	// a stateful one goes to its own endpoint
	hs, err := r.mgr.InitiateEventStatefulRecoveryHandle(t.Context(), liveID, matchURN(7))
	if err != nil {
		t.Fatalf("InitiateEventStatefulRecoveryHandle: %v", err)
	}
	if hs == h {
		t.Fatal("a stateful request coalesced onto an odds one")
	}
	eventually(t, "the stateful request", func() bool { return r.hits.statefulRecover.Load() == 1 })
}

// An event recovery ends in failure when the session's queue it waits for is
// lost, and keeps going through the rest: a producer going down for another cause
// is no reason.
func TestManager_EventRecoveryFailsOnTheLossOfAQueueOnly(t *testing.T) {
	r := newRig(t)
	r.open()
	s1 := uuid.New()
	r.openSession(s1, types.AllMessageInterest)
	r.alive(liveID)
	first := r.waitSnapshots(1)[0]
	r.snapshotComplete(s1, liveID, first.requestID)
	eventually(t, "the live producer up", func() bool { s, ok := r.status(liveID); return ok && !s.IsDown() })

	h, err := r.mgr.InitiateEventOddsRecoveryHandle(t.Context(), liveID, matchURN(1))
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the request", func() bool { return r.hits.eventRecover.Load() == 1 })
	// the producer goes silent: down for the alive interval
	r.sync()
	r.clock.advance(30 * time.Second)
	r.tick()
	eventually(t, "the producer down for silence", func() bool { s, ok := r.status(liveID); return ok && s.IsDown() })
	if h.IsTerminal() {
		t.Fatalf("an alive-interval down ended the event recovery: %v", h.Snapshot())
	}

	r.mgr.OnFeedChannelLost(s1)
	select {
	case <-h.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the loss of the queue did not end the event recovery")
	}
	if res := h.Result(); res.Status != types.RecoveryStatusFailed || res.Err == nil {
		t.Fatalf("result = %v, %v, want failed with a reason", res.Status, res.Err)
	}
}

// An event recovery the API refuses is no recovery: its handle fails with the
// reason, and the slot is free.
func TestManager_EventRecoveryTheApiRefusedFails(t *testing.T) {
	r := newRig(t)
	r.open()
	s1 := uuid.New()
	r.openSession(s1, types.AllMessageInterest)
	r.hits.refuse.Store(true)

	h, err := r.mgr.InitiateEventOddsRecoveryHandle(t.Context(), liveID, matchURN(1))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("a refused event recovery did not end")
	}
	if res := h.Result(); res.Status != types.RecoveryStatusFailed || res.Err == nil {
		t.Fatalf("result = %v, %v, want failed with a reason", res.Status, res.Err)
	}
}

// An event recovery asked for while a session awaits its queue waits for it: its
// snapshot could go to either queue, or to none.
func TestManager_EventRecoveryAskedForWhileASessionAwaitsItsQueueWaitsForIt(t *testing.T) {
	r := newRig(t)
	r.open()
	s1 := uuid.New()
	r.openSession(s1, types.AllMessageInterest)
	r.alive(liveID)
	first := r.waitSnapshots(1)[0]
	r.snapshotComplete(s1, liveID, first.requestID)
	eventually(t, "the live producer up", func() bool { s, ok := r.status(liveID); return ok && !s.IsDown() })

	s2 := uuid.New()
	r.mgr.OnClientSessionOpened(s2, types.LiveOnlyMessageInterest, r.clock.Now()) // its queue is not bound yet
	h, err := r.mgr.InitiateEventOddsRecoveryHandle(t.Context(), liveID, matchURN(5))
	if err != nil {
		t.Fatal(err)
	}
	r.sync()
	if got := r.hits.eventRecover.Load(); got != 0 {
		t.Fatalf("the request went out while a session awaited its queue (%d)", got)
	}
	if h.IsTerminal() {
		t.Fatalf("the handle ended while it waits: %v", h.Snapshot())
	}
	r.mgr.OnFeedChannelRestored(s2)
	eventually(t, "the request once the queue is bound", func() bool { return r.hits.eventRecover.Load() == 1 })
	if got := r.hits.all()[len(r.hits.all())-1].requestID; got != h.RequestID() {
		t.Fatalf("sent under id %d, the caller has %d", got, h.RequestID())
	}
}

// A producer the catalog did not have when the feed opened gets its track when
// its first fact arrives.
func TestManager_ProducerLearnedAfterOpenIsTracked(t *testing.T) {
	r := newRig(t)
	r.open()
	s1 := uuid.New()
	r.openSession(s1, types.AllMessageInterest)
	// a producer id the catalog does not have is counted and ignored
	r.alive(99)
	r.sync()
	if got := r.mgr.actor().machine.counters.unknownProducers; got != 1 {
		t.Fatalf("unknown producers counted = %d, want 1", got)
	}
	if got := len(r.hits.snapshots()); got != 0 {
		t.Fatalf("a recovery was asked for a producer the catalog does not have: %d", got)
	}
}

// A producer disabled by the client is not recovered, and its alives change nothing.
func TestManager_DisabledProducerIsNotRecovered(t *testing.T) {
	r := newRig(t)
	if err := r.pm.Open(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := r.pm.SetProducerState(t.Context(), liveID, false); err != nil {
		t.Fatal(err)
	}
	r.open()
	s1 := uuid.New()
	r.openSession(s1, types.AllMessageInterest)
	r.alive(liveID)
	r.sync()
	if got := len(r.hits.snapshots()); got != 0 {
		t.Fatalf("a disabled producer was recovered: %d requests", got)
	}
	r.alive(preID)
	if got := r.waitSnapshots(1)[0].producer; got != "pre" {
		t.Fatalf("recovered %q, want the enabled prematch producer", got)
	}
}

// A request the API refused is asked for again after the backoff.
func TestManager_RefusedRecoveryIsAskedForAgainAfterTheBackoff(t *testing.T) {
	r := newRig(t)
	r.open()
	s1 := uuid.New()
	r.openSession(s1, types.AllMessageInterest)
	r.hits.refuse.Store(true)
	r.alive(liveID)
	r.waitSnapshots(1)
	eventually(t, "the refusal to be taken", func() bool {
		p, _ := r.pm.GetProducer(t.Context(), liveID)
		info := p.RecoveryInfo()
		return info != nil && !(*info).Successful()
	})
	r.hits.refuse.Store(false)

	r.clock.advance(4 * time.Second)
	r.alive(liveID)
	r.tick()
	r.sync()
	if got := len(r.hits.snapshots()); got != 1 {
		t.Fatalf("asked for again before the backoff (5 s): %d requests", got)
	}
	r.clock.advance(2 * time.Second)
	r.alive(liveID)
	r.tick()
	second := r.waitSnapshots(2)[1]
	r.snapshotComplete(s1, liveID, second.requestID)
	eventually(t, "the producer up", func() bool { s, ok := r.status(liveID); return ok && !s.IsDown() })
}

// The recovery start the client sets after the feed opened lowers the start of the
// next recovery and the resume point, and opens no request of its own.
func TestManager_RecoveryStartSetAfterTheFeedOpenedRewindsTheNextRecovery(t *testing.T) {
	r := newRig(t)
	r.open()
	s1 := uuid.New()
	r.openSession(s1, types.AllMessageInterest)
	r.alive(liveID)
	first := r.waitSnapshots(1)[0]
	r.snapshotComplete(s1, liveID, first.requestID)
	eventually(t, "the live producer up", func() bool { s, ok := r.status(liveID); return ok && !s.IsDown() })
	r.clock.advance(15 * time.Second)
	r.alive(liveID)
	r.processed(s1, liveID)
	rewound := r.clock.Now().Wall().Add(-10 * time.Minute)

	if err := r.pm.SetProducerRecoveryFromTimestamp(t.Context(), liveID, rewound); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the resume point rewound", func() bool { return r.timestampForRecovery(liveID).Equal(rewound) })
	r.sync()
	if got := len(r.hits.snapshots()); got != 1 {
		t.Fatalf("a rewind alone asked for a recovery: %d requests", got)
	}
	// clearing it after the start changes nothing
	if err := r.pm.SetProducerRecoveryFromTimestamp(t.Context(), liveID, time.Time{}); err != nil {
		t.Fatal(err)
	}
	r.sync()
	if !r.timestampForRecovery(liveID).Equal(rewound) {
		t.Fatalf("a cleared start moved the resume point to %v", r.timestampForRecovery(liveID))
	}

	r.sync()
	r.mgr.OnFeedChannelLost(s1)
	r.mgr.OnFeedChannelRestored(s1)
	r.alive(liveID)
	second := r.waitSnapshots(2)[1]
	if second.afterMillis != rewound.UnixMilli() {
		t.Fatalf("recovery after %d, want the rewound start %d", second.afterMillis, rewound.UnixMilli())
	}
}

// The producer's stalled client holds the snapshot deadline: with a session's
// callback stuck, the recovery is not asked for again however long it takes.
func TestManager_StuckClientCallbackHoldsTheSnapshotDeadline(t *testing.T) {
	r := newRig(t)
	r.open()
	s1 := uuid.New()
	r.openSession(s1, types.AllMessageInterest)
	r.alive(liveID)
	first := r.waitSnapshots(1)[0]
	r.sync()

	// the session's callback takes a message and never returns
	r.mgr.OnMessageProcessingStarted(s1, liveID, r.clock.Now())
	for range 40 {
		r.clock.advance(time.Minute)
		r.alive(liveID)
		r.tick()
		r.sync()
	}
	if got := len(r.hits.snapshots()); got != 1 {
		t.Fatalf("a stuck callback did not hold the deadline: %d requests after 40 minutes", got)
	}

	// it returns: the deadline counts again, from there
	r.mgr.OnMessageProcessingEnded(s1, liveID, time.Time{})
	r.clock.advance(5*time.Minute + time.Second)
	r.alive(liveID)
	r.tick()
	r.sync()
	r.clock.advance(6 * time.Second) // the backoff
	r.alive(liveID)
	r.tick()
	second := r.waitSnapshots(2)[1]
	if second.requestID == first.requestID {
		t.Fatal("asked for again under the same id")
	}
}

// What the client persists is not moved by the facts of the sessions that close
// after the manager did: the point is final when Close returns.
func TestManager_ResumePointIsFinalOnceCloseReturns(t *testing.T) {
	r := newRig(t)
	r.open()
	s1, s2 := uuid.New(), uuid.New()
	r.openSession(s1, types.AllMessageInterest)
	r.openSession(s2, types.AllMessageInterest)
	r.alive(liveID)
	first := r.waitSnapshots(1)[0]
	r.snapshotComplete(s1, liveID, first.requestID)
	r.snapshotComplete(s2, liveID, first.requestID)
	eventually(t, "the live producer up", func() bool { s, ok := r.status(liveID); return ok && !s.IsDown() })
	r.clock.advance(10 * time.Second)
	r.alive(liveID)
	behind := r.processed(s1, liveID)
	r.sync()
	r.clock.advance(time.Second)
	r.processed(s2, liveID)
	r.sync()
	if got := r.timestampForRecovery(liveID); !got.Equal(behind) {
		t.Fatalf("resume point before the close = %v, want the slower session's %v", got, behind)
	}

	r.mgr.Close()
	// the sessions close one by one after the manager: no fact of theirs reaches it
	r.mgr.OnFeedSessionGone(s1)
	r.mgr.OnFeedSessionGone(s2)
	if got := r.timestampForRecovery(liveID); !got.Equal(behind) {
		t.Fatalf("resume point after the close = %v, want %v still", got, behind)
	}
}

func TestManager_ClosedManagerAnswersEventRecoveriesAsClosed(t *testing.T) {
	r := newRig(t)
	r.open()
	r.mgr.Close()
	if _, err := r.mgr.InitiateEventOddsRecoveryHandle(t.Context(), liveID, matchURN(1)); !errors.Is(err, ErrManagerClosed) {
		t.Fatalf("err = %v, want ErrManagerClosed", err)
	}
}

// Every producer starts down, whatever an earlier feed left in the producer
// manager: the machine says so, and the manager's flag follows.
func TestManager_EveryProducerStartsDownWhateverAnEarlierFeedLeft(t *testing.T) {
	r := newRig(t)
	if err := r.pm.Open(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := r.pm.SetProducerDown(liveID, false); err != nil {
		t.Fatal(err)
	}
	r.open()
	r.sync()
	for _, id := range []int{liveID, preID, mixedID} {
		if !r.isDown(id) {
			t.Fatalf("producer %d is not flagged down at the start", id)
		}
	}
}

// ---- what the session callbacks do to the producer manager

// The processing callbacks move the producer's last-message and last-processed
// timestamps, monotonically; a zero timestamp moves nothing.
func TestManager_ProcessingCallbacksMoveTheProducersTimestamps(t *testing.T) {
	r := newRig(t)
	r.open()
	s1 := uuid.New()
	t1 := r.clock.Now()
	r.mgr.OnMessageProcessingStarted(s1, liveID, t1)
	t2 := t1.Wall().Add(time.Minute)
	r.mgr.OnMessageProcessingEnded(s1, liveID, t2)
	p, _ := r.pm.GetProducer(t.Context(), liveID)
	if !p.LastMessageTimestamp().Equal(t1.Wall()) {
		t.Fatalf("last message timestamp = %v, want %v", p.LastMessageTimestamp(), t1.Wall())
	}
	if !p.LastProcessedMessageGenTimestamp().Equal(t2) {
		t.Fatalf("last processed timestamp = %v, want %v", p.LastProcessedMessageGenTimestamp(), t2)
	}
	// an older message of a slower session moves neither back; zero moves nothing
	r.mgr.OnMessageProcessingStarted(s1, liveID, NewStamp(t1.Wall().Add(-time.Hour), t1.Mono()))
	r.mgr.OnMessageProcessingEnded(s1, liveID, t2.Add(-time.Hour))
	r.mgr.OnMessageProcessingStarted(s1, liveID, NewStamp(time.Time{}, t1.Mono()))
	r.mgr.OnMessageProcessingEnded(s1, liveID, time.Time{})
	if !p.LastMessageTimestamp().Equal(t1.Wall()) || !p.LastProcessedMessageGenTimestamp().Equal(t2) {
		t.Fatalf("timestamps went back: %v, %v", p.LastMessageTimestamp(), p.LastProcessedMessageGenTimestamp())
	}
	// the processing of the session is known while a message is in it, and only then
	r.mgr.OnMessageProcessingStarted(s1, liveID, t1)
	if got := r.mgr.processingSince(); len(got) != 1 || got[s1] != t1.Mono() {
		t.Fatalf("processing since = %v, want session %s at %v", got, s1, t1.Mono())
	}
	r.mgr.OnMessageProcessingEnded(s1, liveID, time.Time{})
	if got := r.mgr.processingSince(); len(got) != 0 {
		t.Fatalf("processing since = %v after the message ended, want none", got)
	}
}

// What a session finishes, a message or an alive, moves the resume point the client
// persists: the point is the oldest checkpoint over the sessions once nothing is open.
func TestManager_SessionAliveAndProcessedMessagesMoveTheResumePoint(t *testing.T) {
	r := newRig(t)
	r.open()
	s1, s2 := uuid.New(), uuid.New()
	r.openSession(s1, types.AllMessageInterest)
	r.openSession(s2, types.AllMessageInterest)
	r.alive(liveID)
	first := r.waitSnapshots(1)[0]
	r.snapshotComplete(s1, liveID, first.requestID)
	r.snapshotComplete(s2, liveID, first.requestID)
	eventually(t, "the live producer up", func() bool { s, ok := r.status(liveID); return ok && !s.IsDown() })
	start := r.clock.Now().Wall()
	eventually(t, "the point at the request", func() bool { return r.timestampForRecovery(liveID).Equal(start) })

	r.clock.advance(10 * time.Second)
	r.alive(liveID)
	now := r.clock.Now()
	r.mgr.OnSessionAlive(s1, liveID, now.Wall(), now, true)
	r.processed(s2, liveID)
	r.sync()
	if got := r.timestampForRecovery(liveID); !got.Equal(now.Wall()) {
		t.Fatalf("resume point = %v, want both sessions' checkpoint %v", got, now.Wall())
	}
	// an unsubscribed alive of a session moves nothing, and one of a session of the
	// client reaches no other session's checkpoint
	r.clock.advance(10 * time.Second)
	later := r.clock.Now()
	r.alive(liveID)
	r.mgr.OnSessionAlive(s1, liveID, later.Wall(), later, true)
	r.sync()
	if got := r.timestampForRecovery(liveID); !got.Equal(now.Wall()) {
		t.Fatalf("resume point = %v, want session 2's older checkpoint %v", got, now.Wall())
	}
}

// The loss of a channel no session of the actor's owns, the SDK's own alive channel,
// opens no gap and takes no producer down; a callback before Open is nothing.
func TestManager_LossOfTheSDKsOwnAliveChannelOpensNoGap(t *testing.T) {
	r := newRig(t)
	r.open()
	s1 := uuid.New()
	r.openSession(s1, types.AllMessageInterest)
	r.alive(liveID)
	first := r.waitSnapshots(1)[0]
	r.snapshotComplete(s1, liveID, first.requestID)
	eventually(t, "the live producer up", func() bool { s, ok := r.status(liveID); return ok && !s.IsDown() })
	before := len(r.statuses(liveID))

	// the alive-only session is never registered; its channel's loss and rebind are no facts of a session
	aliveOnly := uuid.New()
	r.mgr.OnClientSessionOpened(aliveOnly, types.SystemAliveOnly, r.clock.Now())
	r.mgr.OnFeedChannelLost(aliveOnly)
	r.mgr.OnFeedChannelRestored(aliveOnly)
	r.mgr.OnFeedSessionGone(aliveOnly)
	r.alive(liveID)
	r.sync()
	if got := len(r.statuses(liveID)); got != before {
		t.Fatalf("statuses = %d, want the %d from before: the loss took the producer down", got, before)
	}
	if got := len(r.hits.snapshots()); got != 1 {
		t.Fatalf("recoveries = %d, want the first only", got)
	}
}

func TestManager_FeedCallbacksBeforeOpenAreNoOps(t *testing.T) {
	r := newRig(t)
	s1 := uuid.New()
	now := r.clock.Now()
	r.mgr.OnClientSessionOpened(s1, types.AllMessageInterest, now)
	r.mgr.OnFeedChannelLost(s1)
	r.mgr.OnFeedChannelRestored(s1)
	r.mgr.OnFeedSessionGone(s1)
	r.mgr.OnMessageProcessed(s1, liveID, now.Wall(), now, 0)
	r.mgr.OnSessionAlive(s1, liveID, now.Wall(), now, true)
	r.mgr.OnAliveReceived(liveID, types.MessageTimestamp{Created: now.Wall(), Received: now.Wall()}, now, true)
	if _, ok := r.mgr.ProducerStatus(liveID); ok {
		t.Fatal("a status exists before Open")
	}
	// opened afterwards, the manager knows nothing of them
	r.open()
	r.sync()
	if got := len(r.mgr.actor().machine.sessions); got != 0 {
		t.Fatalf("sessions the machine knows = %d, want none", got)
	}
}

// ---- the requests

// A request in flight does not hold up the actor: it answers its other facts
// whenever the API is slow.
func TestManager_SnapshotRequestInFlightDoesNotBlockTheActor(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newRigWith(t, func(w http.ResponseWriter, req *http.Request) bool {
		if strings.Contains(req.URL.Path, "/recovery/initiate_request") {
			entered <- struct{}{}
			<-release
			_, _ = io.WriteString(w, okBody)
			return true
		}
		return false
	})
	defer close(release)
	r.open()
	s1 := uuid.New()
	r.openSession(s1, types.AllMessageInterest)
	r.alive(liveID)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the request never reached the server")
	}
	r.sync() // the actor takes its facts while the request hangs
	h, err := r.mgr.InitiateEventOddsRecoveryHandle(t.Context(), preID, matchURN(1))
	if err != nil || h == nil {
		t.Fatalf("an event recovery while a snapshot request hangs = %v, %v", h, err)
	}
}

// An event recovery request is cancelled when its recovery ends, so a slow API
// does not keep its goroutine for the rest of the API timeout.
func TestManager_EventRecoveryThatEndsHasItsRequestCancelled(t *testing.T) {
	entered := make(chan struct{}, 1)
	aborted := make(chan struct{}, 1)
	r := newRigWith(t, func(w http.ResponseWriter, req *http.Request) bool {
		if strings.Contains(req.URL.Path, "/odds/events/") {
			entered <- struct{}{}
			<-req.Context().Done()
			aborted <- struct{}{}
			return true
		}
		return false
	})
	r.open()
	s1 := uuid.New()
	r.openSession(s1, types.AllMessageInterest)
	r.alive(liveID)
	first := r.waitSnapshots(1)[0]
	r.snapshotComplete(s1, liveID, first.requestID)
	eventually(t, "the live producer up", func() bool { s, ok := r.status(liveID); return ok && !s.IsDown() })

	h, err := r.mgr.InitiateEventOddsRecoveryHandle(t.Context(), liveID, matchURN(1))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the request never reached the server")
	}
	r.sync()
	r.mgr.OnFeedChannelLost(s1) // the queue the recovery waits for is gone: it ends
	select {
	case <-h.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the event recovery did not end")
	}
	select {
	case <-aborted:
	case <-time.After(3 * time.Second):
		t.Fatal("the request of the ended event recovery was not cancelled")
	}
}

// Closing the manager cancels an event recovery request in flight at once and fails
// its handle as closed.
func TestManager_CloseAbortsAnEventRecoveryRequestInFlightAndFailsItsHandle(t *testing.T) {
	entered := make(chan struct{}, 1)
	aborted := make(chan struct{}, 1)
	r := newRigWith(t, func(w http.ResponseWriter, req *http.Request) bool {
		if strings.Contains(req.URL.Path, "/odds/events/") {
			entered <- struct{}{}
			<-req.Context().Done()
			aborted <- struct{}{}
			return true
		}
		return false
	})
	r.open()
	s1 := uuid.New()
	r.openSession(s1, types.AllMessageInterest)
	h, err := r.mgr.InitiateEventOddsRecoveryHandle(t.Context(), liveID, matchURN(1))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the request never reached the server")
	}
	start := time.Now()
	if !r.mgr.CloseCtx(t.Context()) {
		t.Fatal("close did not complete")
	}
	select {
	case <-aborted:
	case <-time.After(2 * time.Second):
		t.Fatal("close did not cancel the request in flight")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("close took %v", time.Since(start))
	}
	select {
	case <-h.Done():
	default:
		t.Fatal("the handle did not end with the close")
	}
	if res := h.Result(); res.Status != types.RecoveryStatusFailed || !errors.Is(res.Err, ErrManagerClosed) {
		t.Fatalf("result = %v, %v, want failed as closed", res.Status, res.Err)
	}
}

// ---- the producers of both scopes

// A producer of both scopes completes once every session that receives it has taken
// a snapshot_complete: a live-only and a prematch-only session each take their own,
// and a live-only session alone is enough when it is the only one.
func TestManager_MixedProducerCompletesOnEverySessionsSnapshotComplete(t *testing.T) {
	r := newRig(t)
	r.open()
	live, pre := uuid.New(), uuid.New()
	r.openSession(live, types.LiveOnlyMessageInterest)
	r.openSession(pre, types.PrematchOnlyMessageInterest)
	r.alive(mixedID)
	first := r.waitSnapshots(1)[0]
	r.snapshotComplete(live, mixedID, first.requestID)
	r.sync()
	if _, ok := r.status(mixedID); ok {
		t.Fatal("the mixed producer came up on the live session's snapshot_complete alone")
	}
	r.snapshotComplete(pre, mixedID, first.requestID)
	eventually(t, "the mixed producer up", func() bool { s, ok := r.status(mixedID); return ok && !s.IsDown() })

	only := newRig(t)
	only.open()
	s := uuid.New()
	only.openSession(s, types.LiveOnlyMessageInterest)
	only.alive(mixedID)
	req := only.waitSnapshots(1)[0]
	only.snapshotComplete(s, mixedID, req.requestID)
	eventually(t, "the mixed producer up with a live-only session", func() bool {
		st, ok := only.status(mixedID)
		return ok && !st.IsDown()
	})
}
