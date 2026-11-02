package recovery

import (
	"errors"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/types"
)

func producerStatusEvents(f *fakeManagerOps) []types.ProducerStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []types.ProducerStatus
	for _, m := range f.emittedMsgs {
		if m.ProducerStatus != nil {
			out = append(out, m.ProducerStatus)
		}
	}
	return out
}

// TestActor_DelayedFlagClearsOnTickAndIsEmitted: the producer comes up
// right after its first recovery while its last processed message is old
// (the resume point), so the status is emitted delayed=true. Nothing used
// to clear the flag: the reason does not change while the producer stays
// up, so neither the event stream nor ProducerStatus() ever said it was
// current again. The tick now re-evaluates the timing and publishes a
// status when only the flag flipped — once, not per tick.
func TestActor_DelayedFlagClearsOnTickAndIsEmitted(t *testing.T) {
	fake := newFakeManagerOps()
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, fake)

	now := time.Now().Truncate(time.Millisecond)
	// An old processed cursor: the producer opens delayed.
	a.onMessageProcessingEnded(now.Add(-time.Hour))
	if err := a.systemAliveReceived(aliveAt(now), true); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 1)
	if err := a.snapshotRecoveryFinished(a.currentRecovery.recoveryID); err != nil {
		t.Fatal(err)
	}

	evs := producerStatusEvents(fake)
	if len(evs) == 0 {
		t.Fatal("no producer status emitted by the recovery completion")
	}
	last := evs[len(evs)-1]
	if last.IsDown() || !last.IsDelayed() {
		t.Fatalf("after open: down=%v delayed=%v, want up+delayed", last.IsDown(), last.IsDelayed())
	}
	reason := last.ProducerStatusReason()
	before := len(evs)

	// Still delayed: a tick must not emit anything.
	a.onTick(now.Add(time.Second), false)
	if got := len(producerStatusEvents(fake)); got != before {
		t.Fatalf("tick with an unchanged flag emitted %d status event(s)", got-before)
	}

	// The feed catches up.
	a.onMessageProcessingEnded(now.Add(2 * time.Second))
	a.onTick(now.Add(3*time.Second), false)

	evs = producerStatusEvents(fake)
	if len(evs) != before+1 {
		t.Fatalf("status events = %d, want %d (one for the flag flipping)", len(evs), before+1)
	}
	got := evs[len(evs)-1]
	if got.IsDelayed() || got.IsDown() {
		t.Fatalf("refreshed status: down=%v delayed=%v, want up+current", got.IsDown(), got.IsDelayed())
	}
	if got.ProducerStatusReason() != reason {
		t.Fatalf("refreshed status reason = %v, want unchanged %v", got.ProducerStatusReason(), reason)
	}
	if cur := a.currentStatus(); cur == nil || cur.IsDelayed() {
		t.Fatalf("ProducerStatus snapshot still delayed: %+v", cur)
	}

	// No flapping: further ticks with the flag unchanged stay silent.
	for i := range 5 {
		a.onMessageProcessingEnded(now.Add(time.Duration(4+i) * time.Second))
		a.onTick(now.Add(time.Duration(4+i)*time.Second), false)
	}
	if got := len(producerStatusEvents(fake)); got != before+1 {
		t.Fatalf("status events after more ticks = %d, want %d", got, before+1)
	}

	// And it flips back when the feed really falls behind again.
	a.onTick(now.Add(10*time.Minute), false)
	evs = producerStatusEvents(fake)
	if len(evs) != before+2 || !evs[len(evs)-1].IsDelayed() {
		t.Fatalf("falling behind again: events=%d lastDelayed=%v", len(evs), evs[len(evs)-1].IsDelayed())
	}
	if !a.currentStatus().IsDelayed() {
		t.Fatal("ProducerStatus snapshot did not turn delayed again")
	}
}

// TestActor_DelayedFlagRefreshWaitsForFirstStatus: before the first status
// exists the tick has nothing to refresh (the reason-change path owns the
// first emission).
func TestActor_DelayedFlagRefreshWaitsForFirstStatus(t *testing.T) {
	fake := newFakeManagerOps()
	srv, _ := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, fake)

	a.onTick(time.Now(), false)
	if got := len(producerStatusEvents(fake)); got != 0 {
		t.Fatalf("tick before any status emitted %d event(s)", got)
	}
}

// delayedOpenActor returns an actor that came up right after its first
// recovery with the flag published as delayed=true (the processed cursor is
// an hour old), plus the status events emitted so far.
func delayedOpenActor(t *testing.T, fake *fakeManagerOps) (*recoveryActor, time.Time) {
	t.Helper()
	srv, hits := fixtureSrv(t)
	t.Cleanup(srv.Close)
	a := newWiredActor(t, srv, fake)
	now := time.Now().Truncate(time.Millisecond)
	a.onMessageProcessingEnded(now.Add(-time.Hour))
	if err := a.systemAliveReceived(aliveAt(now), true); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 1)
	if err := a.snapshotRecoveryFinished(a.currentRecovery.recoveryID); err != nil {
		t.Fatal(err)
	}
	evs := producerStatusEvents(fake)
	if len(evs) == 0 || !evs[len(evs)-1].IsDelayed() {
		t.Fatal("setup: expected a delayed status after the open")
	}
	return a, now
}

// A disabled producer publishes nothing: no event, and the snapshot
// ProducerStatus() returns stays as it was, however the timing moves.
func TestActor_DelayedFlagRefresh_DisabledProducerPublishesNothing(t *testing.T) {
	fake := newFakeManagerOps()
	a, now := delayedOpenActor(t, fake)
	before := len(producerStatusEvents(fake))
	snapBefore := a.currentStatus()

	if err := a.pm.SetProducerState(t.Context(), a.producerID, false); err != nil {
		t.Fatal(err)
	}
	a.onMessageProcessingEnded(now.Add(2 * time.Second)) // the flag would flip
	a.refreshDelayedFlag(now.Add(3 * time.Second))

	if got := len(producerStatusEvents(fake)); got != before {
		t.Fatalf("disabled producer emitted %d status event(s)", got-before)
	}
	if a.currentStatus() != snapBefore || !a.currentStatus().IsDelayed() {
		t.Fatal("disabled producer's status snapshot changed")
	}
}

// A timing lookup that fails must not be read as "delayed": the published
// flag is current (false) and a failing lookup would flip it to true.
// Nothing is published and the snapshot keeps its flag.
func TestActor_DelayedFlagRefresh_FailingTimingLookupPublishesNothing(t *testing.T) {
	fake := newFakeManagerOps()
	a, now := delayedOpenActor(t, fake)

	// The feed catches up: published delayed=false.
	a.onMessageProcessingEnded(now.Add(2 * time.Second))
	a.refreshDelayedFlag(now.Add(3 * time.Second))
	if a.currentStatus().IsDelayed() {
		t.Fatal("setup: flag did not clear")
	}
	before := len(producerStatusEvents(fake))
	snapBefore := a.currentStatus()

	a.lastProcessedFn = func() (time.Time, error) { return time.Time{}, errors.New("lookup failed") }
	a.refreshDelayedFlag(now.Add(4 * time.Second))
	if got := len(producerStatusEvents(fake)); got != before {
		t.Fatalf("a failing lookup published %d status event(s)", got-before)
	}
	if a.currentStatus() != snapBefore || a.currentStatus().IsDelayed() {
		t.Fatal("a failing lookup changed the status snapshot")
	}

	// A lookup that works again publishes the real change.
	a.lastProcessedFn = nil
	a.refreshDelayedFlag(now.Add(10 * time.Minute)) // processing now stands 10 minutes behind
	if got := len(producerStatusEvents(fake)); got != before+1 || !a.currentStatus().IsDelayed() {
		t.Fatalf("after the lookup recovered: events %d (want %d), delayed=%v", got, before+1, a.currentStatus().IsDelayed())
	}
}
