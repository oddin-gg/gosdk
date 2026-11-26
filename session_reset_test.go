package gosdk

import (
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/oddin-gg/gosdk/internal/recovery"
	"github.com/oddin-gg/gosdk/types"
)

// resetRecorder is a recovery processor that keeps the handle a session
// registers and the reset-done reports it makes.
type resetRecorder struct {
	sessionHookRecorder
	rmu        sync.Mutex
	registered map[uuid.UUID]recovery.Resetter
	reports    []resetReport
}

type resetReport struct {
	session  uuid.UUID
	number   uint64
	replaced bool
}

func (r *resetRecorder) RegisterSessionResetter(s uuid.UUID, h recovery.Resetter) {
	r.rmu.Lock()
	defer r.rmu.Unlock()
	if r.registered == nil {
		r.registered = make(map[uuid.UUID]recovery.Resetter)
	}
	r.registered[s] = h
}

func (r *resetRecorder) OnFeedChannelResetDone(s uuid.UUID, number uint64, replaced bool) {
	r.rmu.Lock()
	defer r.rmu.Unlock()
	r.reports = append(r.reports, resetReport{s, number, replaced})
}

// A live session gives the recovery processor its own consumer as the handle
// to reset it through, before the consumer starts; a replay session gives none.
func TestSession_Open_RegistersItsConsumerAsTheResetHandle(t *testing.T) {
	for _, isReplay := range []bool{false, true} {
		rec := &resetRecorder{}
		id, err := openFailingWith(t, rec, isReplay, types.AllMessageInterest)
		if err == nil {
			t.Fatal("Open succeeded without a broker")
		}
		rec.rmu.Lock()
		h, ok := rec.registered[id]
		rec.rmu.Unlock()
		if ok == isReplay {
			t.Fatalf("replay=%v: handle registered = %v", isReplay, ok)
		}
		if !isReplay && h == nil {
			t.Fatal("a nil handle was registered")
		}
	}
}

// What the session's consumer reports done reaches the processor under the
// session's id, with the number the reset was given; here the consumer was never
// opened, so nothing was replaced.
func TestSession_ConsumerResetDoneReachesTheProcessorUnderTheSessionID(t *testing.T) {
	rec := &resetRecorder{}
	id, err := openFailingWith(t, rec, false, types.AllMessageInterest)
	if err == nil {
		t.Fatal("Open succeeded without a broker")
	}
	rec.rmu.Lock()
	h := rec.registered[id]
	rec.rmu.Unlock()
	h.Reset(21)

	rec.rmu.Lock()
	defer rec.rmu.Unlock()
	if len(rec.reports) != 1 || rec.reports[0] != (resetReport{id, 21, false}) {
		t.Fatalf("reports = %+v, want session %s number 21 not replaced", rec.reports, id)
	}
}
