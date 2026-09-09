package recovery

import (
	"crypto/tls"
	"net/http"
	"net/url"
	"testing"
	"time"

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
// producer down the moment the loss is reported, so the next system
// alive starts a recovery from the cursor BEFORE the loss.

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

// TestActor_ChannelLost_RecoversFromTheCursorBeforeTheLoss is the core
// contract: after a channel loss the producer is flagged down with the
// connection-down reason, the consumer sees that transition, and the
// next alive POSTs a snapshot recovery whose `after=` cursor is the LAST
// ALIVE BEFORE THE LOSS — not the alive that arrived after it.
func TestActor_ChannelLost_RecoversFromTheCursorBeforeTheLoss(t *testing.T) {
	fake := newFakeManagerOps()
	now := time.Now().Truncate(time.Millisecond)
	a, hits := steadyActor(t, fake, now.Add(-10*time.Second))

	// Steady state: a healthy alive advances the recovery cursor and
	// starts nothing.
	preLoss := now.Add(-4 * time.Second)
	if err := a.systemAliveReceived(aliveAt(preLoss), true); err != nil {
		t.Fatal(err)
	}
	if a.recoveryState != types.CompletedRecoveryState {
		t.Fatalf("steady alive changed state to %v", a.recoveryState)
	}

	// The consumer channel is lost (a blip far shorter than MaxInactivity
	// — the alive interval check alone would never notice).
	a.enqueueChannelLost()
	a.dispatch(evChannelLossNudge{})

	if !a.isFlaggedDown() {
		t.Fatal("producer should be flagged down after a channel loss")
	}
	if a.downReason != types.ConnectionDownProducerDownReason {
		t.Fatalf("down reason = %v, want ConnectionDown", a.downReason)
	}
	if a.pendingChannelLoss.Load() {
		t.Fatal("pending flag must be cleared once applied")
	}
	fake.mu.Lock()
	last := fake.emittedMsgs[len(fake.emittedMsgs)-1]
	fake.mu.Unlock()
	if last.ProducerStatus == nil || !last.ProducerStatus.IsDown() ||
		last.ProducerStatus.ProducerStatusReason() != types.ConnectionDownProducerStatusReason {
		t.Fatalf("last emitted status = %+v, want down with ConnectionDown reason", last.ProducerStatus)
	}

	// The first alive after the rebind starts the snapshot recovery —
	// from the cursor before the loss, so the gap is covered.
	if err := a.systemAliveReceived(aliveAt(now), true); err != nil {
		t.Fatal(err)
	}
	if a.recoveryState != types.StartedRecoveryState {
		t.Fatalf("post-loss alive should start a snapshot recovery, state = %v", a.recoveryState)
	}
	waitRecoverHits(t, hits, 2)
	if got, want := hits.lastAfterMillis.Load(), preLoss.UnixMilli(); got != want {
		t.Fatalf("recovery after= %d (%s), want the pre-loss alive %d (%s) — the gap would not be covered",
			got, time.UnixMilli(got).UTC(), want, preLoss.UTC())
	}
}

// TestActor_ChannelLost_DuringRecoveryRestartsIt: a recovery that was in
// flight when the channel was lost lost its replayed messages too; the
// loss interrupts it and the next alive starts a fresh one.
func TestActor_ChannelLost_DuringRecoveryRestartsIt(t *testing.T) {
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, newFakeManagerOps())
	if err := a.systemAliveReceived(aliveAt(time.Now()), true); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 1)
	first := a.currentRecovery.recoveryID

	a.pendingChannelLoss.Store(true)
	a.dispatch(evTick{now: time.Now(), inactivityArmed: false})

	if a.recoveryState != types.InterruptedRecoveryState {
		t.Fatalf("state = %v, want Interrupted", a.recoveryState)
	}
	if err := a.systemAliveReceived(aliveAt(time.Now()), true); err != nil {
		t.Fatal(err)
	}
	if a.recoveryState != types.StartedRecoveryState || a.currentRecovery.recoveryID == first {
		t.Fatalf("expected a NEW snapshot recovery after the loss, state=%v req=%d (first %d)", a.recoveryState, a.currentRecovery.recoveryID, first)
	}
	waitRecoverHits(t, hits, 2)
}

// TestActor_ChannelLost_AppliedBeforeCoalescedAlive: the loss and the
// first alive on the new channel can land in the same inbox drain; the
// loss must be applied first so that alive starts recovery rather than
// being read as "all is well" and advancing the cursor past the gap.
func TestActor_ChannelLost_AppliedBeforeCoalescedAlive(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	a, hits := steadyActor(t, newFakeManagerOps(), now.Add(-10*time.Second))
	preLoss := now.Add(-4 * time.Second)
	if err := a.systemAliveReceived(aliveAt(preLoss), true); err != nil {
		t.Fatal(err)
	}

	a.pendingChannelLoss.Store(true)
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
// nudge is dropped but the flag survives; the next tick applies it.
func TestActor_ChannelLost_NudgeDroppedTickApplies(t *testing.T) {
	a, _ := steadyActor(t, newFakeManagerOps(), time.Now())

	for a.send(evMsgProcessingEnded{}) { // non-blocking: stops once the inbox is full
	}
	a.enqueueChannelLost() // nudge dropped: inbox full
	if !a.pendingChannelLoss.Load() {
		t.Fatal("flag must be set regardless of the nudge")
	}
	a.dispatch(evTick{now: time.Now(), inactivityArmed: false})
	if !a.isFlaggedDown() || a.downReason != types.ConnectionDownProducerDownReason {
		t.Fatalf("tick must apply the pending loss: down=%v reason=%v", a.isFlaggedDown(), a.downReason)
	}
	if a.pendingChannelLoss.Load() {
		t.Fatal("flag must be consumed once applied")
	}
}

// TestActor_ChannelLost_ProducerManagerErrorKeepsTheNotice: when the
// producer manager cannot answer, the reaction is deferred, not dropped.
func TestActor_ChannelLost_ProducerManagerErrorKeepsTheNotice(t *testing.T) {
	srv, _ := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActorForProducer(t, srv, newFakeManagerOps(), 999) // unknown producer → pm errors

	a.pendingChannelLoss.Store(true)
	a.dispatch(evChannelLossNudge{})
	if !a.pendingChannelLoss.Load() {
		t.Fatal("notice must stay pending when the producer manager errors")
	}
}

// TestManager_OnFeedChannelLost_FansOutToEveryKnownActor drives the one
// line of production wiring between the client and the actors: an OPEN
// manager with two producer actors signals both, and each ends up
// flagged down with the connection-down reason.
func TestManager_OnFeedChannelLost_FansOutToEveryKnownActor(t *testing.T) {
	srv, _ := fixtureSrv(t)
	defer srv.Close()
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
	defer m.Close()

	// Two known producers: their actors exist because the feed has been
	// heard from. Bring both to steady state (recovered, up).
	actors := map[int]*recoveryActor{}
	for _, id := range []int{1, 2} {
		a := m.findOrSpawn(id)
		if a == nil {
			t.Fatalf("no actor for producer %d", id)
		}
		actors[id] = a
	}
	for id, a := range actors {
		if err := pm.SetProducerDown(id, false); err != nil {
			t.Fatal(err)
		}
		_ = a // steady: up, not recovering
	}

	m.OnFeedChannelLost()
	if m.ChannelLossCount() != 1 {
		t.Fatalf("ChannelLossCount = %d, want 1", m.ChannelLossCount())
	}

	deadline := time.Now().Add(3 * time.Second)
	for id := range actors {
		for {
			down, err := pm.IsProducerDown(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if down {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("producer %d not flagged down after OnFeedChannelLost", id)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

func TestManager_OnFeedChannelLost_BeforeOpenIsNoop(t *testing.T) {
	m := newTestManager(t)
	m.OnFeedChannelLost() // must not panic on a never-opened manager
	if m.ChannelLossCount() != 1 {
		t.Fatalf("ChannelLossCount = %d, want 1", m.ChannelLossCount())
	}
}

func TestProducerDownReason_ConnectionDownMapsToStatusReason(t *testing.T) {
	if got := types.ConnectionDownProducerDownReason.ToProducerStatusReason(); got != types.ConnectionDownProducerStatusReason {
		t.Fatalf("ToProducerStatusReason = %v, want ConnectionDown", got)
	}
}
