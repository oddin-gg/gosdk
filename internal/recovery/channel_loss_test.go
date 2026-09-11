package recovery

import (
	"crypto/tls"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/producer"
	"github.com/oddin-gg/gosdk/types"
)

// These tests pin the reaction to a lost consumer channel. The
// subscription queues are exclusive and auto-delete, so losing the
// channel — with the connection or alone — loses every message published
// until the rebind; only a snapshot recovery can close the gap, and the
// alive-based checks miss any connection drop shorter than MaxInactivity
// and never see a single channel's loss. The actor therefore flags the
// producer down when the loss is reported AND floors its recovery cursor
// at the loss instant, so the next system alive starts a recovery that
// reaches back to the loss whatever order the notice and the alives
// around it arrived in.

func aliveAt(t time.Time) types.MessageTimestamp {
	return types.MessageTimestamp{Created: t, Sent: t, Received: t, Published: t}
}

// steadyActor returns an actor whose first snapshot recovery has
// completed: producer up, state Completed — the state a healthy feed
// sits in between recoveries. firstAlive is the alive that started it.
func steadyActor(t *testing.T, fake *fakeManagerOps, firstAlive time.Time) (*recoveryActor, *recoveryHits) {
	t.Helper()
	srv, hits := fixtureSrv(t)
	t.Cleanup(srv.Close)
	a := newWiredActor(t, srv, fake)
	if err := a.systemAliveReceived(aliveAt(firstAlive), true); err != nil {
		t.Fatalf("first alive: %v", err)
	}
	if a.recoveryState != types.StartedRecoveryState {
		t.Fatalf("first alive should start the initial snapshot recovery, state = %v", a.recoveryState)
	}
	waitRecoverHits(t, hits, 1)
	// Complete it the way onSnapshotComplete would.
	if err := a.snapshotRecoveryFinished(a.currentRecovery.recoveryID); err != nil {
		t.Fatalf("finish initial recovery: %v", err)
	}
	if a.recoveryState != types.CompletedRecoveryState || a.isFlaggedDown() {
		t.Fatalf("expected completed + up, got state=%v down=%v", a.recoveryState, a.isFlaggedDown())
	}
	return a, hits
}

func waitRecoverHits(t *testing.T, hits *recoveryHits, want int32) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for hits.recover.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("recovery POSTs = %d, want %d", hits.recover.Load(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func lossAt(t *testing.T, a *recoveryActor, at time.Time) {
	t.Helper()
	a.enqueueChannelLost(at)
	a.dispatch(evChannelLossNudge{})
}

// TestActor_ChannelLost_RecoversFromTheCursorBeforeTheLoss is the core
// contract in the ordinary order: after a channel loss the producer is
// flagged down with the connection-down reason, the consumer sees that
// transition, and the next alive POSTs a snapshot recovery whose `after=`
// cursor is the LAST ALIVE BEFORE THE LOSS — not the alive after it.
func TestActor_ChannelLost_RecoversFromTheCursorBeforeTheLoss(t *testing.T) {
	fake := newFakeManagerOps()
	now := time.Now().Truncate(time.Millisecond)
	a, hits := steadyActor(t, fake, now.Add(-10*time.Second))

	preLoss := now.Add(-4 * time.Second)
	if err := a.systemAliveReceived(aliveAt(preLoss), true); err != nil {
		t.Fatal(err)
	}
	if a.recoveryState != types.CompletedRecoveryState {
		t.Fatalf("steady alive changed state to %v", a.recoveryState)
	}

	lossAt(t, a, now.Add(-2*time.Second))

	if !a.isFlaggedDown() {
		t.Fatal("producer should be flagged down after a channel loss")
	}
	if a.downReason != types.ConnectionDownProducerDownReason {
		t.Fatalf("down reason = %v, want ConnectionDown", a.downReason)
	}
	if a.pendingLossAt.Load() != 0 {
		t.Fatal("pending notice must be cleared once applied")
	}
	fake.mu.Lock()
	last := fake.emittedMsgs[len(fake.emittedMsgs)-1]
	fake.mu.Unlock()
	if last.ProducerStatus == nil || !last.ProducerStatus.IsDown() ||
		last.ProducerStatus.ProducerStatusReason() != types.ConnectionDownProducerStatusReason {
		t.Fatalf("last emitted status = %+v, want down with ConnectionDown reason", last.ProducerStatus)
	}

	if err := a.systemAliveReceived(aliveAt(now), true); err != nil {
		t.Fatal(err)
	}
	if a.recoveryState != types.StartedRecoveryState {
		t.Fatalf("post-loss alive should start a snapshot recovery, state = %v", a.recoveryState)
	}
	waitRecoverHits(t, hits, 2)
	if got, want := hits.lastAfterMillis.Load(), preLoss.UnixMilli(); got != want {
		t.Fatalf("recovery after= %d (%s), want the pre-loss alive %d (%s)", got, time.UnixMilli(got).UTC(), want, preLoss.UTC())
	}
}

// TestActor_ChannelLost_NoticeBeforeOvertakingAlive_RecoversFromPreLossCursor
// is the race the design must survive, in its realistic shape: the loss
// is REPORTED promptly (watcher), but the actor applies a post-loss alive
// before it drains the notice — the two are separate atomics. The notice
// captured the producer's recovery cursor at report time, so the recovery
// reaches back to the last alive BEFORE the loss, exactly as in the
// ordinary order — covering, too, whatever the dead queue still held
// undelivered from before the loss.
func TestActor_ChannelLost_NoticeBeforeOvertakingAlive_RecoversFromPreLossCursor(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	a, hits := steadyActor(t, newFakeManagerOps(), now.Add(-10*time.Second))
	preLoss := now.Add(-6 * time.Second)
	if err := a.systemAliveReceived(aliveAt(preLoss), true); err != nil {
		t.Fatal(err)
	}

	a.enqueueChannelLost(now.Add(-4 * time.Second)) // reported now, cursor = preLoss
	// The overtaking alive slips in before the notice is drained.
	if err := a.systemAliveReceived(aliveAt(now.Add(-1*time.Second)), true); err != nil {
		t.Fatal(err)
	}
	a.dispatch(evChannelLossNudge{})
	if !a.isFlaggedDown() {
		t.Fatal("loss notice must flag the producer down")
	}

	if err := a.systemAliveReceived(aliveAt(now), true); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 2)
	if got, want := hits.lastAfterMillis.Load(), preLoss.UnixMilli(); got != want {
		t.Fatalf("recovery after= %d (%s), want the pre-loss cursor %d (%s) — same as the ordinary order",
			got, time.UnixMilli(got).UTC(), want, preLoss.UTC())
	}
}

// TestActor_ChannelLost_OvertakenByAliveStillCoversTheGap is the shape
// where the notice itself is created only AFTER a post-loss alive already
// advanced the producer's cursor. The anchor is the alive before that
// latest one — here the last healthy alive before the loss — so the
// recovery still reaches back past the loss instant; the loss instant
// is only ever a fallback for a producer with no cursor at all.
func TestActor_ChannelLost_OvertakenByAliveStillCoversTheGap(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	a, hits := steadyActor(t, newFakeManagerOps(), now.Add(-10*time.Second))
	healthy := now.Add(-6 * time.Second)
	if err := a.systemAliveReceived(aliveAt(healthy), true); err != nil {
		t.Fatal(err)
	}

	lost := now.Add(-4 * time.Second)
	// The overtaking alive: processed with the producer still up.
	if err := a.systemAliveReceived(aliveAt(now.Add(-1*time.Second)), true); err != nil {
		t.Fatal(err)
	}
	if a.recoveryState != types.CompletedRecoveryState || a.isFlaggedDown() {
		t.Fatalf("overtaking alive must look healthy: state=%v down=%v", a.recoveryState, a.isFlaggedDown())
	}
	// The loss notice arrives late, stamped with when it really happened.
	lossAt(t, a, lost)
	if !a.isFlaggedDown() {
		t.Fatal("late loss notice must still flag the producer down")
	}

	if err := a.systemAliveReceived(aliveAt(now), true); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 2)
	got := hits.lastAfterMillis.Load()
	if got > lost.UnixMilli() {
		t.Fatalf("recovery after= %d (%s) is past the loss instant %d (%s) — the overtaking alive hid the gap",
			got, time.UnixMilli(got).UTC(), lost.UnixMilli(), lost.UTC())
	}
	if got != healthy.UnixMilli() {
		t.Fatalf("recovery after= %d (%s), want the alive before the latest, %d (%s)",
			got, time.UnixMilli(got).UTC(), healthy.UnixMilli(), healthy.UTC())
	}
	if !a.recoveryFloor.Equal(healthy) {
		t.Fatalf("floor = %v after starting the covering recovery, want kept until it completes (%v)", a.recoveryFloor, healthy)
	}
	if err := a.snapshotRecoveryFinished(a.currentRecovery.recoveryID); err != nil {
		t.Fatal(err)
	}
	if !a.recoveryFloor.IsZero() {
		t.Fatalf("floor = %v after the covering recovery completed, want cleared", a.recoveryFloor)
	}
}

// TestActor_ChannelLost_DuringRecoveryRestartsFromItsCursor: a recovery
// in flight when the channel was lost lost its replayed messages too;
// the loss interrupts it and the next alive starts a fresh one that
// reaches back at least to the interrupted request's own cursor. The
// hazard it pins: the interrupted request was issued from a one-shot
// explicit rewind, which recording its PostRecovery result consumed, so
// a naive restart would recompute the cursor as the (later) last-alive
// cursor and skip the rewound span for good.
func TestActor_ChannelLost_DuringRecoveryRestartsFromItsCursor(t *testing.T) {
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, newFakeManagerOps())
	now := time.Now().Truncate(time.Millisecond)

	// Steady state with the alive cursor at T1.
	if err := a.systemAliveReceived(aliveAt(now.Add(-30*time.Second)), true); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 1)
	if err := a.snapshotRecoveryFinished(a.currentRecovery.recoveryID); err != nil {
		t.Fatal(err)
	}
	cursorT1 := now.Add(-15 * time.Second)
	if err := a.systemAliveReceived(aliveAt(cursorT1), true); err != nil {
		t.Fatal(err)
	}

	// An explicit rewind to T0 < T1, then a recovery that uses it.
	rewind := now.Add(-20 * time.Second)
	if err := a.pm.SetProducerRecoveryFromTimestamp(t.Context(), a.producerID, rewind); err != nil {
		t.Fatal(err)
	}
	if err := a.systemAliveReceived(aliveAt(now.Add(-10*time.Second)), false); err != nil { // subscribed=false → recovery
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 2)
	if got := hits.lastAfterMillis.Load(); got != rewind.UnixMilli() {
		t.Fatalf("rewound recovery after= %d, want %d", got, rewind.UnixMilli())
	}
	first := a.currentRecovery.recoveryID

	// Record the PostRecovery result the way the actor loop would; that
	// consumes the one-shot rewind, so the producer's cursor is T1 again.
	deadline := time.Now().Add(2 * time.Second)
	for {
		prod, err := a.pm.GetProducer(t.Context(), a.producerID)
		if err != nil {
			t.Fatal(err)
		}
		if prod.TimestampForRecovery().Equal(cursorT1) {
			break
		}
		select {
		case ev := <-a.inbox:
			a.dispatch(ev)
		default:
			time.Sleep(5 * time.Millisecond)
		}
		if time.Now().After(deadline) {
			t.Fatalf("PostRecovery result never recorded: cursor still %v", prod.TimestampForRecovery())
		}
	}

	lossAt(t, a, now.Add(-5*time.Second))
	if a.recoveryState != types.InterruptedRecoveryState {
		t.Fatalf("state = %v, want Interrupted", a.recoveryState)
	}
	if err := a.systemAliveReceived(aliveAt(now), true); err != nil {
		t.Fatal(err)
	}
	if a.recoveryState != types.StartedRecoveryState || a.currentRecovery.recoveryID == first {
		t.Fatalf("expected a NEW snapshot recovery after the loss, state=%v req=%d (first %d)", a.recoveryState, a.currentRecovery.recoveryID, first)
	}
	waitRecoverHits(t, hits, 3)
	if got := hits.lastAfterMillis.Load(); got != rewind.UnixMilli() {
		t.Fatalf("restarted recovery after= %d (%s), want the interrupted request's own cursor %d (%s), not the later alive cursor %d",
			got, time.UnixMilli(got).UTC(), rewind.UnixMilli(), rewind.UTC(), cursorT1.UnixMilli())
	}
}

// TestActor_ChannelLost_AppliedBeforeCoalescedAlive: the loss and the
// first alive on the new channel can land in the same inbox drain; the
// loss is applied first so that alive starts recovery immediately.
func TestActor_ChannelLost_AppliedBeforeCoalescedAlive(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	a, hits := steadyActor(t, newFakeManagerOps(), now.Add(-10*time.Second))
	preLoss := now.Add(-4 * time.Second)
	if err := a.systemAliveReceived(aliveAt(preLoss), true); err != nil {
		t.Fatal(err)
	}

	a.notePendingLoss(now.Add(-2 * time.Second))
	a.enqueueAlive(evAlive{timestamp: aliveAt(now), isSubscribed: true, messageInterest: types.SystemAliveOnly})
	a.dispatch(evAliveNudge{})

	if a.recoveryState != types.StartedRecoveryState {
		t.Fatalf("alive drained together with a channel loss must start recovery, state = %v", a.recoveryState)
	}
	waitRecoverHits(t, hits, 2)
	if got := hits.lastAfterMillis.Load(); got != preLoss.UnixMilli() {
		t.Fatalf("recovery after= %d, want pre-loss alive %d", got, preLoss.UnixMilli())
	}
}

// TestActor_ChannelLost_NudgeDroppedTickApplies: with a full inbox the
// nudge is dropped but the notice survives; the next tick applies it.
func TestActor_ChannelLost_NudgeDroppedTickApplies(t *testing.T) {
	a, _ := steadyActor(t, newFakeManagerOps(), time.Now())

	for a.send(evMsgProcessingEnded{}) { // non-blocking: stops once the inbox is full
	}
	a.enqueueChannelLost(time.Now()) // nudge dropped: inbox full
	if a.pendingLossAt.Load() == 0 {
		t.Fatal("notice must be recorded regardless of the nudge")
	}
	a.dispatch(evTick{now: time.Now(), inactivityArmed: false})
	if !a.isFlaggedDown() || a.downReason != types.ConnectionDownProducerDownReason {
		t.Fatalf("tick must apply the pending loss: down=%v reason=%v", a.isFlaggedDown(), a.downReason)
	}
	if a.pendingLossAt.Load() != 0 {
		t.Fatal("notice must be consumed once applied")
	}
}

// TestActor_ChannelLost_EarliestLossWins: two losses coalesced before the
// actor ran keep the EARLIER instant.
func TestActor_ChannelLost_EarliestLossWins(t *testing.T) {
	a, _ := steadyActor(t, newFakeManagerOps(), time.Now())
	early := time.Now().Add(-5 * time.Second)
	a.notePendingLoss(time.Now())
	a.notePendingLoss(early)
	a.notePendingLoss(time.Now())
	if got := time.Unix(0, a.pendingLossAt.Load()); !got.Equal(early) {
		t.Fatalf("pending = %v, want the earliest %v", got, early)
	}
	a.dispatch(evChannelLossNudge{})
	if !a.recoveryFloor.Equal(early) {
		t.Fatalf("floor = %v, want %v", a.recoveryFloor, early)
	}
}

// TestActor_ChannelLost_ProducerManagerErrorKeepsTheNoticeAndRetries:
// when the producer manager cannot answer, the reaction is deferred —
// the floor is already in place, coalesced alives are held back on both
// the nudge and the tick path — and a later dispatch, once the manager
// answers, applies it and lets the held alive start the recovery.
func TestActor_ChannelLost_ProducerManagerErrorKeepsTheNoticeAndRetries(t *testing.T) {
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActorForProducer(t, srv, newFakeManagerOps(), 999) // unknown producer → pm errors
	lost := time.Now().Add(-3 * time.Second)

	a.notePendingLoss(lost)
	a.enqueueAlive(evAlive{timestamp: aliveAt(time.Now()), isSubscribed: true, messageInterest: types.SystemAliveOnly})
	a.dispatch(evAliveNudge{})
	if a.pendingLossAt.Load() == 0 {
		t.Fatal("notice must stay pending when the producer manager errors")
	}
	if !a.recoveryFloor.Equal(lost) {
		t.Fatalf("floor = %v, want lowered to the loss (%v) even while the reaction is deferred", a.recoveryFloor, lost)
	}
	if a.pendingSystemAlive.Load() == nil {
		t.Fatal("alive must stay coalesced while the channel loss is pending")
	}
	a.dispatch(evTick{now: time.Now(), inactivityArmed: false})
	if a.pendingSystemAlive.Load() == nil {
		t.Fatal("tick must not process the alive either while the loss is pending")
	}

	// The producer manager answers again (the actor now serves a
	// catalogued producer): the retry applies the loss and releases the
	// alive, which starts the recovery — from the floor.
	a.producerID = 1
	a.dispatch(evTick{now: time.Now(), inactivityArmed: false})
	if a.pendingLossAt.Load() != 0 {
		t.Fatal("retry must consume the notice once the producer manager answers")
	}
	if !a.isFlaggedDown() || a.downReason != types.ConnectionDownProducerDownReason {
		t.Fatalf("retry must flag the producer down: down=%v reason=%v", a.isFlaggedDown(), a.downReason)
	}
	if a.pendingSystemAlive.Load() != nil {
		t.Fatal("held alive must be released by the successful retry")
	}
	if a.recoveryState != types.StartedRecoveryState {
		t.Fatalf("released alive should start a recovery, state = %v", a.recoveryState)
	}
	waitRecoverHits(t, hits, 1)
}

// TestActor_ChannelLost_DisabledProducerConsumesTheNotice: a disabled
// producer has no recovery to run; the notice is consumed (one-shot) and
// the producer is not flagged down.
func TestActor_ChannelLost_DisabledProducerConsumesTheNotice(t *testing.T) {
	fake := newFakeManagerOps()
	a, _ := steadyActor(t, fake, time.Now())
	if err := a.pm.SetProducerState(t.Context(), a.producerID, false); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	emittedBefore := len(fake.emittedMsgs)
	fake.mu.Unlock()

	lossAt(t, a, time.Now())

	if a.pendingLossAt.Load() != 0 {
		t.Fatal("notice must be consumed for a disabled producer")
	}
	if a.downReason == types.ConnectionDownProducerDownReason {
		t.Fatal("disabled producer must not be flagged down for a channel loss")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.emittedMsgs) != emittedBefore {
		t.Fatalf("disabled producer emitted %d status message(s) on channel loss", len(fake.emittedMsgs)-emittedBefore)
	}
}

// --- Manager fan-out ---

// openedManagerWithActors returns an OPEN manager (real producer manager
// against fixtureSrv: producer 1 = live, 2 = prematch, 3 = live|prematch)
// with an actor spawned for each given producer, all up.
func openedManagerWithActors(t *testing.T, ids ...int) (*Manager, *producer.Manager) {
	m, pm, _ := openedManagerWithActorsAndHits(t, ids...)
	return m, pm
}

func openedManagerWithActorsAndHits(t *testing.T, ids ...int) (*Manager, *producer.Manager, *recoveryHits) {
	t.Helper()
	srv, hits := fixtureSrv(t)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	cfg := &minimalCfg{apiURL: u.Host, token: "tok"}
	apiClient := api.New(cfg)
	apiClient.SetHTTPClient(&http.Client{
		Transport: &rewriteTransport{target: srv.URL, base: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}},
		Timeout:   2 * time.Second,
	})
	pm := producer.NewManager(cfg, apiClient, newDiscardLogger())
	if err := pm.Open(t.Context()); err != nil {
		t.Fatal(err)
	}
	m := NewManager(cfg, pm, apiClient, newDiscardLogger(), 0)
	if _, err := m.Open(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	for _, id := range ids {
		if m.findOrSpawn(id) == nil {
			t.Fatalf("no actor for producer %d", id)
		}
		if id <= 3 {
			if err := pm.SetProducerDown(id, false); err != nil {
				t.Fatal(err)
			}
		}
	}
	return m, pm, hits
}

func actorOf(t *testing.T, m *Manager, id int) *recoveryActor {
	t.Helper()
	m.actorsMu.RLock()
	defer m.actorsMu.RUnlock()
	a, ok := m.actors[id]
	if !ok {
		t.Fatalf("no actor %d", id)
	}
	return a
}

func waitDown(t *testing.T, pm *producer.Manager, id int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		down, err := pm.IsProducerDown(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if down {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("producer %d not flagged down", id)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestManager_OnFeedChannelLost_FansOutToEveryKnownActor drives the one
// line of production wiring between the session and the actors: an OPEN
// manager with actors for every producer signals all of them for an
// all-interest session (a connection drop closes such a session's
// channel), each ends up flagged down with the connection-down reason,
// and each carries the loss instant.
func TestManager_OnFeedChannelLost_FansOutToEveryKnownActor(t *testing.T) {
	m, pm := openedManagerWithActors(t, 1, 2, 3)
	lost := time.Now().Add(-time.Second)

	m.OnFeedChannelLost(uuid.New(), types.AllMessageInterest, lost)
	_ = lost
	if m.ChannelLossCount() != 1 {
		t.Fatalf("ChannelLossCount = %d, want 1", m.ChannelLossCount())
	}
	for _, id := range []int{1, 2, 3} {
		waitDown(t, pm, id)
	}
	// (The instant itself is pinned by the actor tests through
	// enqueueChannelLost; actor fields are not read here — the actors
	// are running.)
}

// TestManager_OnFeedChannelLost_ScopedToTheLostSessionsInterest: a
// LiveOnly session losing its channel could not have been receiving the
// prematch-only producer, so that producer keeps its state (and its
// in-flight event recoveries); the live and mixed producers are flagged.
// The negative assertion is on the actor's own pending notice, which
// OnFeedChannelLost sets synchronously — no sleep involved.
func TestManager_OnFeedChannelLost_ScopedToTheLostSessionsInterest(t *testing.T) {
	m, pm := openedManagerWithActors(t, 1, 2, 3)

	m.OnFeedChannelLost(uuid.New(), types.LiveOnlyMessageInterest, time.Now())
	if a := actorOf(t, m, 2); a.pendingLossAt.Load() != 0 {
		t.Fatal("prematch-only producer 2 was signalled for a LiveOnly session's loss")
	}
	waitDown(t, pm, 1) // live
	waitDown(t, pm, 3) // live|prematch
	if down, _ := pm.IsProducerDown(t.Context(), 2); down {
		t.Fatal("prematch-only producer 2 flagged down for a LiveOnly session's loss")
	}
}

// TestManager_OnFeedChannelLost_AliveSessionLossFlagsEveryProducer: the
// alive session's channel dies with every connection drop and is never
// parked behind a reader, so its loss is the promptest drop signal — and
// counts for every producer.
func TestManager_OnFeedChannelLost_AliveSessionLossFlagsEveryProducer(t *testing.T) {
	m, pm := openedManagerWithActors(t, 1, 2, 3)
	m.OnFeedChannelLost(uuid.New(), types.SystemAliveOnly, time.Now())
	for _, id := range []int{1, 2, 3} {
		waitDown(t, pm, id)
	}
}

// TestManager_OnFeedChannelLost_UnreadableProducerIsFlaggedAnyway: when
// the catalog cannot say what scope a producer has, the manager signals
// it rather than risk skipping a gap (the fault that dropped AMQP often
// makes the catalog unreadable too).
func TestManager_OnFeedChannelLost_UnreadableProducerIsFlaggedAnyway(t *testing.T) {
	m, _ := openedManagerWithActors(t, 1, 999)
	m.OnFeedChannelLost(uuid.New(), types.LiveOnlyMessageInterest, time.Now())
	// The actor's reaction fails (the catalog cannot describe 999) and
	// re-stores the notice, so the atomic pending slot settles non-zero;
	// it is the only actor state safe to read while the actor runs.
	a := actorOf(t, m, 999)
	deadline := time.Now().Add(2 * time.Second)
	for a.pendingLossAt.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("producer 999 (unreadable from the catalog) was not signalled")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestManager_OnFeedChannelLost_BeforeOpenIsNoop(t *testing.T) {
	m := newTestManager(t)
	m.OnFeedChannelLost(uuid.New(), types.AllMessageInterest, time.Now()) // must not panic on a never-opened manager
	if m.ChannelLossCount() != 1 {
		t.Fatalf("ChannelLossCount = %d, want 1", m.ChannelLossCount())
	}
}

func TestProducerDownReason_ConnectionDownMapsToStatusReason(t *testing.T) {
	if got := types.ConnectionDownProducerDownReason.ToProducerStatusReason(); got != types.ConnectionDownProducerStatusReason {
		t.Fatalf("ToProducerStatusReason = %v, want ConnectionDown", got)
	}
}

// --- Recovery is held back until the lost queue is re-bound ---

// TestActor_ChannelLost_RecoveryWaitsForRebind: while a lost session in
// the producer's scope has no queue, an alive must NOT issue the snapshot
// recovery — the replay would be published into nothing and its
// snapshot_complete lost with it. The producer stays flagged down; once
// the rebind is reported the recovery starts, from the floor.
func TestActor_ChannelLost_RecoveryWaitsForRebind(t *testing.T) {
	fake := newFakeManagerOps()
	now := time.Now().Truncate(time.Millisecond)
	a, hits := steadyActor(t, fake, now.Add(-10*time.Second))
	preLoss := now.Add(-4 * time.Second)
	if err := a.systemAliveReceived(aliveAt(preLoss), true); err != nil {
		t.Fatal(err)
	}

	fake.pendingRebind.Store(true) // the lost session has not re-bound
	lossAt(t, a, now.Add(-2*time.Second))
	if err := a.systemAliveReceived(aliveAt(now), true); err != nil {
		t.Fatal(err)
	}
	if a.recoveryState == types.StartedRecoveryState {
		t.Fatal("recovery must not start while the lost queue is not re-bound")
	}
	if !a.deferredRecovery || !a.isFlaggedDown() {
		t.Fatalf("expected deferred=true and producer down, got deferred=%v down=%v", a.deferredRecovery, a.isFlaggedDown())
	}
	time.Sleep(30 * time.Millisecond)
	if got := hits.recover.Load(); got != 1 {
		t.Fatalf("recovery POSTs = %d while rebind pending, want 1 (only the initial)", got)
	}
	// A tick while still pending changes nothing.
	a.dispatch(evTick{now: time.Now(), inactivityArmed: false})
	if a.recoveryState == types.StartedRecoveryState {
		t.Fatal("tick must not start the recovery while the rebind is pending")
	}

	fake.pendingRebind.Store(false)
	a.dispatch(evChannelRebound{})
	if a.recoveryState != types.StartedRecoveryState || a.deferredRecovery {
		t.Fatalf("rebind must start the deferred recovery: state=%v deferred=%v", a.recoveryState, a.deferredRecovery)
	}
	waitRecoverHits(t, hits, 2)
	if got := hits.lastAfterMillis.Load(); got != preLoss.UnixMilli() {
		t.Fatalf("deferred recovery after= %d, want the pre-loss cursor %d", got, preLoss.UnixMilli())
	}
}

// TestActor_ChannelLost_DeferredRecoveryResumesOnTick: the rebound nudge
// is lossy; the tick is the fallback.
func TestActor_ChannelLost_DeferredRecoveryResumesOnTick(t *testing.T) {
	fake := newFakeManagerOps()
	a, hits := steadyActor(t, fake, time.Now())
	fake.pendingRebind.Store(true)
	lossAt(t, a, time.Now())
	if err := a.systemAliveReceived(aliveAt(time.Now()), true); err != nil {
		t.Fatal(err)
	}
	if !a.deferredRecovery {
		t.Fatal("expected the recovery to be deferred")
	}
	fake.pendingRebind.Store(false)
	a.dispatch(evTick{now: time.Now(), inactivityArmed: false})
	if a.recoveryState != types.StartedRecoveryState {
		t.Fatalf("tick must resume the deferred recovery, state = %v", a.recoveryState)
	}
	waitRecoverHits(t, hits, 2)
}

// TestManager_RebindPending_TracksLostSessionsByScope pins the manager's
// bookkeeping: a lost LiveOnly session holds back the live and mixed
// producers only; a restore or a gone releases it; unknown sessions are
// ignored; the alive-only session holds back everyone.
func TestManager_RebindPending_TracksLostSessionsByScope(t *testing.T) {
	m, _ := openedManagerWithActors(t, 1, 2, 3)
	if m.rebindPending(1) {
		t.Fatal("nothing lost yet, nothing pending")
	}
	live := uuid.New()
	m.OnFeedChannelLost(live, types.LiveOnlyMessageInterest, time.Now())
	if !m.rebindPending(1) || !m.rebindPending(3) || m.rebindPending(2) {
		t.Fatalf("LiveOnly loss: pending 1=%v 2=%v 3=%v, want true/false/true", m.rebindPending(1), m.rebindPending(2), m.rebindPending(3))
	}
	m.OnFeedChannelRestored(uuid.New()) // unknown session: no effect
	if !m.rebindPending(1) {
		t.Fatal("restoring an unknown session must not release the lost one")
	}
	m.OnFeedChannelRestored(live)
	if m.rebindPending(1) || m.rebindPending(3) {
		t.Fatal("restored session must release its producers")
	}

	alive := uuid.New()
	m.OnFeedChannelLost(alive, types.SystemAliveOnly, time.Now())
	for _, id := range []int{1, 2, 3} {
		if !m.rebindPending(id) {
			t.Fatalf("alive-session loss must hold back producer %d", id)
		}
	}
	m.OnFeedSessionGone(alive)
	for _, id := range []int{1, 2, 3} {
		if m.rebindPending(id) {
			t.Fatalf("a session that is gone must not hold back producer %d", id)
		}
	}
}

// TestManager_ChannelRestored_NudgesDeferredActors: end to end through
// the real manager and a running actor, observed only through the
// recovery API — a loss flags the producer down; its next alive issues
// no recovery request while the lost session has no queue; the restore
// lets the request go out.
func TestManager_ChannelRestored_NudgesDeferredActors(t *testing.T) {
	m, pm, hits := openedManagerWithActorsAndHits(t, 1)
	sess := uuid.New()
	m.OnFeedChannelLost(sess, types.LiveOnlyMessageInterest, time.Now())
	waitDown(t, pm, 1)

	m.OnAliveReceived(1, aliveAt(time.Now()), true, types.SystemAliveOnly)
	time.Sleep(150 * time.Millisecond) // ample time for a wrongly issued POST to land
	if got := hits.recover.Load(); got != 0 {
		t.Fatalf("recovery POSTs = %d while the lost session had no queue, want 0", got)
	}

	m.OnFeedChannelRestored(sess)
	waitRecoverHits(t, hits, 1)
}

// --- Anchor: the alive before the last one ---

// TestActor_ChannelLost_AnchorsBeforeTheLatestAlive: with two healthy
// alives A1 < A2 before the loss, the recovery after the loss starts at
// A1, not A2. A2 arrives on the alive session's own channel and may have
// been generated after the broker deleted the consumer's queue yet been
// processed before the loss was seen, so it is not trusted; A1 is a full
// alive interval older.
func TestActor_ChannelLost_AnchorsBeforeTheLatestAlive(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	a, hits := steadyActor(t, newFakeManagerOps(), now.Add(-30*time.Second))
	a1 := now.Add(-12 * time.Second)
	a2 := now.Add(-6 * time.Second)
	for _, at := range []time.Time{a1, a2} {
		if err := a.systemAliveReceived(aliveAt(at), true); err != nil {
			t.Fatal(err)
		}
	}

	lossAt(t, a, now.Add(-3*time.Second))
	if err := a.systemAliveReceived(aliveAt(now), true); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 2)
	if got := hits.lastAfterMillis.Load(); got != a1.UnixMilli() {
		t.Fatalf("recovery after= %d (%s), want the alive BEFORE the latest, %d (%s); got the latest? %v",
			got, time.UnixMilli(got).UTC(), a1.UnixMilli(), a1.UTC(), got == a2.UnixMilli())
	}
}

// TestActor_FloorDischargedByFullRecovery: a recovery issued with a zero
// cursor ("everything the producer has") covers any floor and clears it.
func TestActor_FloorDischargedByFullRecovery(t *testing.T) {
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, newFakeManagerOps())
	// First alive on a producer with no cursor: the initial recovery is
	// issued with a zero cursor.
	if err := a.systemAliveReceived(aliveAt(time.Now()), true); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 1)
	if !a.currentRecovery.recoverFrom.IsZero() {
		t.Fatalf("initial recovery cursor = %v, want zero (everything)", a.currentRecovery.recoverFrom)
	}
	a.recoveryFloor = time.Now().Add(-time.Minute) // a loss floor raised meanwhile
	if err := a.snapshotRecoveryFinished(a.currentRecovery.recoveryID); err != nil {
		t.Fatal(err)
	}
	if !a.recoveryFloor.IsZero() {
		t.Fatalf("floor = %v after a full recovery completed, want cleared", a.recoveryFloor)
	}
}

// --- Floor completeness: every interrupt folds, every cursor shape ---

// TestActor_ChannelLost_InterruptedWhileAlreadyDownStillFolds: the
// subscribed=false path transitions straight to Interrupted when the
// producer is already flagged down, bypassing producerDown. That
// transition must fold the interrupted request's cursor into the floor
// too, or the restart — which recomputes the cursor from
// lastValidAliveGen, advanced by alives that arrived meanwhile — lands
// after a rewind the interrupted request had already consumed, and the
// span in between is never recovered.
func TestActor_ChannelLost_InterruptedWhileAlreadyDownStillFolds(t *testing.T) {
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, newFakeManagerOps())
	now := time.Now().Truncate(time.Millisecond)

	// Up and recovered, cursor at C1.
	if err := a.systemAliveReceived(aliveAt(now.Add(-40*time.Second)), true); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 1)
	if err := a.snapshotRecoveryFinished(a.currentRecovery.recoveryID); err != nil {
		t.Fatal(err)
	}
	if err := a.systemAliveReceived(aliveAt(now.Add(-30*time.Second)), true); err != nil {
		t.Fatal(err)
	}

	// An explicit rewind to T0 drives a recovery through subscribed=false
	// (which also flags the producer down).
	rewind := now.Add(-25 * time.Second)
	if err := a.pm.SetProducerRecoveryFromTimestamp(t.Context(), a.producerID, rewind); err != nil {
		t.Fatal(err)
	}
	if err := a.systemAliveReceived(aliveAt(now.Add(-20*time.Second)), false); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 2)
	if got := hits.lastAfterMillis.Load(); got != rewind.UnixMilli() {
		t.Fatalf("rewound recovery after= %d, want %d", got, rewind.UnixMilli())
	}
	// Record the API result: the one-shot rewind is consumed.
	deadline := time.Now().Add(2 * time.Second)
	for {
		prod, err := a.pm.GetProducer(t.Context(), a.producerID)
		if err != nil {
			t.Fatal(err)
		}
		if !prod.TimestampForRecovery().Equal(rewind) {
			break
		}
		select {
		case ev := <-a.inbox:
			a.dispatch(ev)
		default:
			time.Sleep(2 * time.Millisecond)
		}
		if time.Now().After(deadline) {
			t.Fatal("PostRecovery result never recorded")
		}
	}

	// Healthy alives during the recovery advance lastValidAliveGen well
	// past the rewind — that is what the restart would otherwise use.
	healthy := now.Add(-10 * time.Second)
	if err := a.systemAliveReceived(aliveAt(healthy), true); err != nil {
		t.Fatal(err)
	}
	if !a.lastValidAliveGen.After(rewind) {
		t.Fatalf("test setup: lastValidAliveGen %v must be after the rewind %v", a.lastValidAliveGen, rewind)
	}

	// A second subscribed=false alive while still down: producerDown is
	// skipped and the state goes straight to Interrupted.
	if err := a.systemAliveReceived(aliveAt(now.Add(-8*time.Second)), false); err != nil {
		t.Fatal(err)
	}
	if a.recoveryState != types.InterruptedRecoveryState {
		t.Fatalf("state = %v, want Interrupted", a.recoveryState)
	}
	if !a.recoveryFloor.Equal(rewind) {
		t.Fatalf("floor = %v after the direct Interrupted transition, want the interrupted request's cursor %v", a.recoveryFloor, rewind)
	}

	// Its completion restarts it; the restart must not land after the
	// rewind the interrupted request had used.
	if err := a.snapshotRecoveryFinished(a.currentRecovery.recoveryID); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 3)
	got := hits.lastAfterMillis.Load()
	if got == 0 || got > rewind.UnixMilli() {
		t.Fatalf("restart after= %d (%s), want at or before the rewind %d (%s) — it would otherwise start at the later alive cursor %d",
			got, time.UnixMilli(got).UTC(), rewind.UnixMilli(), rewind.UTC(), healthy.UnixMilli())
	}
}

// TestActor_ChannelLost_FloorBeatsTheInitialSnapshotWindow: with
// WithInitialSnapshotTime set, a producer with no cursor asks for that
// window — which must not hide a loss older than it.
func TestActor_ChannelLost_FloorBeatsTheInitialSnapshotWindow(t *testing.T) {
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	a := newActorWithSnapshotWindow(t, srv, 30*time.Second)
	lost := time.Now().Add(-5 * time.Minute).Truncate(time.Millisecond)

	a.enqueueChannelLost(lost) // no cursor yet: the anchor is the loss
	a.dispatch(evChannelLossNudge{})
	if err := a.systemAliveReceived(aliveAt(time.Now()), true); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 1)
	if got := hits.lastAfterMillis.Load(); got != lost.UnixMilli() {
		t.Fatalf("recovery after= %d (%s), want the loss %d (%s) — the %v initial-snapshot window must not hide an older loss",
			got, time.UnixMilli(got).UTC(), lost.UnixMilli(), lost.UTC(), 30*time.Second)
	}
}

// TestActor_FullHistoryRecoveryInterrupted_RestartsFullHistory: a
// recovery for the producer's whole history (zero cursor, the default
// first snapshot) that a loss interrupts must restart as full history
// too — no timestamp floor can express "everything".
func TestActor_FullHistoryRecoveryInterrupted_RestartsFullHistory(t *testing.T) {
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, newFakeManagerOps())

	// First alive on a producer with no cursor → full-history request.
	if err := a.systemAliveReceived(aliveAt(time.Now().Add(-time.Minute)), true); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 1)
	if hits.lastAfterMillis.Load() != 0 {
		t.Fatalf("initial request carried after= %d, want none (full history)", hits.lastAfterMillis.Load())
	}

	lossAt(t, a, time.Now())
	if a.recoveryState != types.InterruptedRecoveryState {
		t.Fatalf("state = %v, want Interrupted", a.recoveryState)
	}
	if !a.recoveryFloorFull {
		t.Fatal("interrupting a full-history recovery must record a full-history floor")
	}

	if err := a.snapshotRecoveryFinished(a.currentRecovery.recoveryID); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 2)
	if got := hits.lastAfterMillis.Load(); got != 0 {
		t.Fatalf("restart after= %d, want none — a full-history recovery must restart as full history", got)
	}
	// Completing the full restart discharges the floor.
	if err := a.snapshotRecoveryFinished(a.currentRecovery.recoveryID); err != nil {
		t.Fatal(err)
	}
	if a.recoveryFloorFull || !a.recoveryFloor.IsZero() {
		t.Fatalf("floor not discharged by the completed full recovery: full=%v floor=%v", a.recoveryFloorFull, a.recoveryFloor)
	}
}
