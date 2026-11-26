package recovery

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/oddin-gg/gosdk/types"
)

// A session whose live messages stay three minutes old is reset: the manager asks
// for a recovery of the producer, replaces the session's channel once the API has
// accepted it, asks again once the transport has made the reset, and brings the
// producer up when the new recovery completes. The consumer here reports as a real
// one does: the reset is done after the old queue's snapshot_complete has come.
func TestManager_StaleSessionIsResetOnceItsRecoveryIsAcceptedAndRecoveredAgain(t *testing.T) {
	r := newRig(t)
	r.open()
	s1 := uuid.New()
	consumer := &fakeResetter{started: make(chan struct{}, 1)}
	r.mgr.RegisterSessionResetter(s1, consumer)
	r.openSession(s1, types.LiveOnlyMessageInterest)
	r.alive(liveID)
	requestedAt := r.clock.Now().Wall()
	first := r.waitSnapshots(1)[0]
	r.snapshotComplete(s1, liveID, first.requestID)
	eventually(t, "the live producer up", func() bool { s, ok := r.status(liveID); return ok && !s.IsDown() })

	// the messages are three minutes old by the producer's clock, for the window; they
	// are older than what the first recovery brought, so the session's checkpoint is
	// the time of that request
	for second := 1; second <= 61; second++ {
		r.clock.advance(time.Second)
		if second%10 == 0 {
			r.alive(liveID)
		}
		now := r.clock.Now()
		r.mgr.OnMessageProcessed(s1, liveID, now.Wall().Add(-3*time.Minute), now, 0)
		r.tick()
		r.sync()
	}
	asked := r.waitSnapshots(2)
	second := asked[1]
	if second.afterMillis != requestedAt.UnixMilli() {
		t.Fatalf("the net's request starts at %d, want the session's checkpoint %d", second.afterMillis, requestedAt.UnixMilli())
	}

	// the API accepted it: the session's channel is replaced
	select {
	case <-consumer.started:
	case <-time.After(3 * time.Second):
		t.Fatal("the session's channel was not reset")
	}
	numbers := consumer.asked()
	if len(numbers) != 1 {
		t.Fatalf("resets asked of the consumer: %v, want one", numbers)
	}
	eventually(t, "the producer down", func() bool { return r.isDown(liveID) })

	// the old queue's snapshot_complete comes while the transport resets: it is ignored
	r.snapshotComplete(s1, liveID, second.requestID)
	r.sync()
	if got := len(r.hits.snapshots()); got != 2 {
		t.Fatalf("recoveries asked for before the reset is done: %d, want 2", got)
	}
	if !r.isDown(liveID) {
		t.Fatal("the producer came up on a snapshot_complete of the queue being replaced")
	}

	// the channel is replaced: the same recovery is asked for again
	r.mgr.OnFeedChannelResetDone(s1, numbers[0], true)
	third := r.waitSnapshots(3)[2]
	if third.requestID == second.requestID {
		t.Fatal("the recovery was asked for again under the same request id")
	}
	if third.afterMillis != second.afterMillis {
		t.Fatalf("the second request starts at %d, want %d like the first", third.afterMillis, second.afterMillis)
	}

	// the new channel delivers what is current, and the recovery completes
	r.clock.advance(time.Second)
	r.processed(s1, liveID)
	r.tick()
	r.sync()
	r.snapshotComplete(s1, liveID, third.requestID)
	eventually(t, "the live producer up again", func() bool { s, ok := r.status(liveID); return ok && !s.IsDown() })
	if got := consumer.asked(); len(got) != 1 {
		t.Fatalf("resets asked of the consumer: %v, want one", got)
	}
	if got := len(r.hits.snapshots()); got != 3 {
		t.Fatalf("recoveries asked for: %d, want 3", got)
	}
}
