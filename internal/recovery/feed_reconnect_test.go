package recovery

import (
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/types"
)

// These tests pin the reaction to a feed reconnect. The subscription
// queues are exclusive and auto-delete, so a connection drop loses every
// message published until rebind; only a snapshot recovery can close the
// gap, and the alive-based checks miss any drop shorter than
// MaxInactivity. The actor therefore flags the producer down on
// reconnect so the next system alive starts a recovery.

func aliveAt(t time.Time) types.MessageTimestamp {
	return types.MessageTimestamp{Created: t, Sent: t, Received: t, Published: t}
}

// steadyActor returns an actor whose first snapshot recovery has
// completed: producer up, state Completed — the state a healthy feed
// sits in between recoveries.
func steadyActor(t *testing.T, fake *fakeManagerOps) (*recoveryActor, *recoveryHits) {
	t.Helper()
	srv, hits := fixtureSrv(t)
	t.Cleanup(srv.Close)
	a := newWiredActor(t, srv, fake)
	if err := a.systemAliveReceived(aliveAt(time.Now()), true); err != nil {
		t.Fatalf("first alive: %v", err)
	}
	if a.recoveryState != types.StartedRecoveryState {
		t.Fatalf("first alive should start the initial snapshot recovery, state = %v", a.recoveryState)
	}
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

func TestActor_FeedReconnect_FlagsProducerDownAndNextAliveRecovers(t *testing.T) {
	fake := newFakeManagerOps()
	a, hits := steadyActor(t, fake)
	waitRecoverHits(t, hits, 1)

	// A healthy alive in steady state starts nothing.
	if err := a.systemAliveReceived(aliveAt(time.Now()), true); err != nil {
		t.Fatal(err)
	}
	if a.recoveryState != types.CompletedRecoveryState {
		t.Fatalf("steady alive changed state to %v", a.recoveryState)
	}

	// The connection dropped and came back (shorter than MaxInactivity —
	// the alive interval check alone would never notice).
	a.pendingFeedReconnect.Store(true)
	a.dispatch(evFeedReconnectNudge{})

	if !a.isFlaggedDown() {
		t.Fatal("producer should be flagged down after a feed reconnect")
	}
	if a.downReason != types.ConnectionDownProducerDownReason {
		t.Fatalf("down reason = %v, want ConnectionDown", a.downReason)
	}
	if a.pendingFeedReconnect.Load() {
		t.Fatal("pending flag must be cleared once applied")
	}
	// The consumer sees the transition with the connection-down reason.
	fake.mu.Lock()
	last := fake.emittedMsgs[len(fake.emittedMsgs)-1]
	fake.mu.Unlock()
	if last.ProducerStatus == nil || !last.ProducerStatus.IsDown() ||
		last.ProducerStatus.ProducerStatusReason() != types.ConnectionDownProducerStatusReason {
		t.Fatalf("last emitted status = %+v, want down with ConnectionDown reason", last.ProducerStatus)
	}

	// The first post-reconnect alive starts the snapshot recovery.
	if err := a.systemAliveReceived(aliveAt(time.Now()), true); err != nil {
		t.Fatal(err)
	}
	if a.recoveryState != types.StartedRecoveryState {
		t.Fatalf("post-reconnect alive should start a snapshot recovery, state = %v", a.recoveryState)
	}
	waitRecoverHits(t, hits, 2)
}

// TestActor_FeedReconnect_DuringRecoveryRestartsIt: a recovery that was
// in flight when the connection dropped lost its replayed messages too;
// the reconnect interrupts it and the next alive starts a fresh one.
func TestActor_FeedReconnect_DuringRecoveryRestartsIt(t *testing.T) {
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, newFakeManagerOps())
	if err := a.systemAliveReceived(aliveAt(time.Now()), true); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 1)
	first := a.currentRecovery.recoveryID

	a.pendingFeedReconnect.Store(true)
	a.dispatch(evTick{now: time.Now(), inactivityArmed: false})

	if a.recoveryState != types.InterruptedRecoveryState {
		t.Fatalf("state = %v, want Interrupted", a.recoveryState)
	}
	if err := a.systemAliveReceived(aliveAt(time.Now()), true); err != nil {
		t.Fatal(err)
	}
	if a.recoveryState != types.StartedRecoveryState || a.currentRecovery.recoveryID == first {
		t.Fatalf("expected a NEW snapshot recovery after reconnect, state=%v req=%d (first %d)", a.recoveryState, a.currentRecovery.recoveryID, first)
	}
	waitRecoverHits(t, hits, 2)
}

// TestActor_FeedReconnect_AppliedBeforeCoalescedAlive: the reconnect
// and the first post-reconnect alive can land in the same inbox drain;
// the reconnect must be applied first so that alive starts recovery
// rather than being read as "all is well".
func TestActor_FeedReconnect_AppliedBeforeCoalescedAlive(t *testing.T) {
	a, hits := steadyActor(t, newFakeManagerOps())
	waitRecoverHits(t, hits, 1)

	a.pendingFeedReconnect.Store(true)
	a.enqueueAlive(evAlive{timestamp: aliveAt(time.Now()), isSubscribed: true, messageInterest: types.SystemAliveOnly})
	a.dispatch(evAliveNudge{})

	if a.recoveryState != types.StartedRecoveryState {
		t.Fatalf("alive drained together with a reconnect must start recovery, state = %v", a.recoveryState)
	}
	waitRecoverHits(t, hits, 2)
}

// TestActor_FeedReconnect_NudgeDroppedTickApplies: with a full inbox the
// nudge is dropped but the flag survives; the next tick applies it.
func TestActor_FeedReconnect_NudgeDroppedTickApplies(t *testing.T) {
	a, hits := steadyActor(t, newFakeManagerOps())
	waitRecoverHits(t, hits, 1)

	for len(a.inbox) < cap(a.inbox) {
		a.inbox <- evMsgProcessingEnded{}
	}
	a.enqueueFeedReconnected() // nudge dropped: inbox full
	if !a.pendingFeedReconnect.Load() {
		t.Fatal("flag must be set regardless of the nudge")
	}
	a.dispatch(evTick{now: time.Now(), inactivityArmed: false})
	if !a.isFlaggedDown() || a.downReason != types.ConnectionDownProducerDownReason {
		t.Fatalf("tick must apply the pending reconnect: down=%v reason=%v", a.isFlaggedDown(), a.downReason)
	}
}

func TestManager_OnFeedReconnected_BeforeOpenIsNoop(t *testing.T) {
	m := newTestManager(t)
	m.OnFeedReconnected() // must not panic on a never-opened manager
	if m.FeedReconnectCount() != 1 {
		t.Fatalf("FeedReconnectCount = %d, want 1", m.FeedReconnectCount())
	}
}

func TestProducerDownReason_ConnectionDownMapsToStatusReason(t *testing.T) {
	if got := types.ConnectionDownProducerDownReason.ToProducerStatusReason(); got != types.ConnectionDownProducerStatusReason {
		t.Fatalf("ToProducerStatusReason = %v, want ConnectionDown", got)
	}
}
