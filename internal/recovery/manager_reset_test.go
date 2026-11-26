package recovery

import (
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/oddin-gg/gosdk/types"
)

// fakeResetter is a session's handle: it records the numbers it was asked to
// reset with, and can hold the reset the way a broker round trip does.
type fakeResetter struct {
	mu      sync.Mutex
	numbers []uint64
	hold    chan struct{}
	started chan struct{}
}

func (f *fakeResetter) Reset(number uint64) {
	f.mu.Lock()
	f.numbers = append(f.numbers, number)
	f.mu.Unlock()
	if f.started != nil {
		f.started <- struct{}{}
	}
	if f.hold != nil {
		<-f.hold
	}
}

func (f *fakeResetter) asked() []uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]uint64(nil), f.numbers...)
}

// The outbox's reset reaches the consumer of the session it names and no other,
// and it does not hold up the actor while the reset waits for the broker.
func TestManager_ResetRunsTheRightSessionsHandleOffTheActorsGoroutine(t *testing.T) {
	r := newRig(t)
	r.open()
	s1, s2 := uuid.New(), uuid.New()
	h1 := &fakeResetter{hold: make(chan struct{}), started: make(chan struct{}, 1)}
	h2 := &fakeResetter{}
	r.mgr.RegisterSessionResetter(s1, h1)
	r.mgr.RegisterSessionResetter(s2, h2)
	r.openSession(s1, types.AllMessageInterest)
	r.openSession(s2, types.AllMessageInterest)

	// the actor runs the outbox call: from a fact handled on its goroutine
	a := r.mgr.actor()
	called := make(chan struct{})
	a.setBeforeHandle(func(f fact) {
		if b, ok := f.(factBarrier); ok {
			a.reset(s1, 4)
			close(called)
			close(b.done)
		}
	})
	a.postEssential(factBarrier{make(chan struct{})})
	<-called
	select {
	case <-h1.started:
	case <-time.After(3 * time.Second):
		t.Fatal("the session's handle was not asked to reset")
	}
	// the held reset does not stop the actor taking facts
	a.setBeforeHandle(func(f fact) {
		if b, ok := f.(factBarrier); ok {
			close(b.done)
		}
	})
	r.sync()
	if got := h1.asked(); len(got) != 1 || got[0] != 4 {
		t.Fatalf("session 1 was asked %v, want [4]", got)
	}
	if got := h2.asked(); len(got) != 0 {
		t.Fatalf("session 2 was asked %v, want nothing", got)
	}
	close(h1.hold)
}

// A session with no handle cannot be reset: the reset ends at once, not replaced.
func TestManager_ResetOfASessionWithoutAHandleEndsNotReplaced(t *testing.T) {
	r := newRig(t)
	r.open()
	a := r.mgr.actor()
	got := make(chan factResetDone, 4)
	a.setBeforeHandle(func(f fact) {
		switch f := f.(type) {
		case factBarrier:
			close(f.done)
		case factResetDone:
			got <- f
		}
	})
	s := uuid.New()
	a.reset(s, 11)
	select {
	case f := <-got:
		if f.session != s || f.number != 11 || f.replaced {
			t.Fatalf("fact = %+v, want session %s number 11 not replaced", f, s)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no reset-done fact")
	}
}

// The consumer's done-report is posted to the actor as an essential fact, and a
// session that is gone drops its handle.
func TestManager_ResetDoneReachesTheActorAsAFactAndAGoneSessionDropsItsHandle(t *testing.T) {
	r := newRig(t)
	r.open()
	a := r.mgr.actor()
	got := make(chan factResetDone, 4)
	a.setBeforeHandle(func(f fact) {
		switch f := f.(type) {
		case factBarrier:
			close(f.done)
		case factResetDone:
			got <- f
		}
	})
	s := uuid.New()
	r.mgr.OnFeedChannelResetDone(s, 8, true)
	select {
	case f := <-got:
		if f != (factResetDone{session: s, number: 8, replaced: true}) {
			t.Fatalf("fact = %+v", f)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the done-report did not reach the actor")
	}

	r.mgr.RegisterSessionResetter(s, &fakeResetter{})
	if r.mgr.resetterOf(s) == nil {
		t.Fatal("the handle was not kept")
	}
	r.mgr.OnFeedSessionGone(s)
	if r.mgr.resetterOf(s) != nil {
		t.Fatal("a gone session keeps its handle")
	}
}
