package gosdk

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/oddin-gg/gosdk/internal/feed"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/internal/recovery"
	"github.com/oddin-gg/gosdk/types"
)

// sessionHookEvent is one call of the recovery processor that registers a
// client session.
type sessionHookEvent struct {
	kind     string // "opened" or "gone"
	session  uuid.UUID
	interest types.MessageInterest
	// at is the reading of the clock an "opened" call carried.
	at recovery.Stamp
}

// sessionHookRecorder keeps the order of the registration hooks; every
// other hook is a no-op.
type sessionHookRecorder struct {
	mu     sync.Mutex
	events []sessionHookEvent
}

func (r *sessionHookRecorder) OnClientSessionOpened(s uuid.UUID, mi types.MessageInterest, at recovery.Stamp) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, sessionHookEvent{kind: "opened", session: s, interest: mi, at: at})
}

func (r *sessionHookRecorder) OnFeedSessionGone(s uuid.UUID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, sessionHookEvent{kind: "gone", session: s})
}

func (r *sessionHookRecorder) snapshot() []sessionHookEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]sessionHookEvent(nil), r.events...)
}

func (*sessionHookRecorder) OnFeedChannelLost(uuid.UUID)                               {}
func (*sessionHookRecorder) OnFeedChannelRestored(uuid.UUID)                           {}
func (*sessionHookRecorder) OnMessageProcessingStarted(uuid.UUID, int, recovery.Stamp) {}
func (*sessionHookRecorder) OnMessageProcessingEnded(uuid.UUID, int, time.Time)        {}
func (*sessionHookRecorder) OnAliveReceived(int, types.MessageTimestamp, recovery.Stamp, bool) {
}
func (*sessionHookRecorder) OnSnapshotCompleteReceived(context.Context, uuid.UUID, int, int) error {
	return nil
}

// openFailing opens a session whose channel consumer cannot get a channel
// (the context is already done, so no broker is needed) and returns the
// session id with the error of Open.
func openFailing(t *testing.T, rec *sessionHookRecorder, isReplay bool, interest types.MessageInterest) (uuid.UUID, error) {
	t.Helper()
	logger := log.New(nil)
	s := newSession(feed.NewClient(nil, nil, logger), nil, nil, nil, rec, "ex", "od:sport:", isReplay, logger, StrategyCatch, 0)
	impl, ok := s.(*oddsFeedSessionImpl)
	if !ok {
		t.Fatalf("newSession returned %T", s)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return impl.sessionID, s.Open(ctx, []string{string(interest)}, &interest, false)
}

// A live session registers itself, with its interest and own id, before
// its consumer starts, and takes the registration back when the consumer
// cannot open: a registration left behind would count a session that
// never receives anything. The consumer cannot open here, so the order of
// the calls is what pins both.
func TestSession_Open_RegistersTheClientSessionBeforeTheConsumerAndRemovesItOnFailure(t *testing.T) {
	for _, interest := range []types.MessageInterest{
		types.LiveOnlyMessageInterest, types.PrematchOnlyMessageInterest, types.AllMessageInterest,
	} {
		rec := &sessionHookRecorder{}
		id, err := openFailing(t, rec, false, interest)
		if err == nil {
			t.Fatalf("%s: Open succeeded without a broker", interest)
		}
		got := rec.snapshot()
		if len(got) != 2 || got[0].kind != "opened" || got[0].session != id || got[0].interest != interest ||
			got[1].kind != "gone" || got[1].session != id {
			t.Fatalf("%s: hook calls = %+v, want opened then gone of session %s", interest, got, id)
		}
	}
}

// A replay session is not part of the live feed: nothing is registered,
// and nothing to remove when its consumer fails.
func TestSession_Open_ReplaySessionIsNotRegistered(t *testing.T) {
	rec := &sessionHookRecorder{}
	if _, err := openFailing(t, rec, true, types.AllMessageInterest); err == nil {
		t.Fatal("Open succeeded without a broker")
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("a replay session called the registration hooks: %+v", got)
	}
}

// The SDK's own alive-only session passes through the same hook; it is the
// recovery manager that does not register it.
func TestSession_Open_AliveOnlySessionCallsTheHook(t *testing.T) {
	rec := &sessionHookRecorder{}
	id, err := openFailing(t, rec, false, types.SystemAliveOnly)
	if err == nil {
		t.Fatal("Open succeeded without a broker")
	}
	got := rec.snapshot()
	if len(got) == 0 || got[0].kind != "opened" || got[0].session != id || got[0].interest != types.SystemAliveOnly {
		t.Fatalf("hook calls = %+v, want the alive-only session reported as opened", got)
	}
}

// The reading of the clock a session's registration carries is taken where the
// session registers, before its consumer binds a queue, not when the fact is
// handled later: one reading, the session's own clock's.
func TestSession_Open_RegistrationCarriesAReadingTakenBeforeTheConsumerOpens(t *testing.T) {
	rec := &sessionHookRecorder{}
	logger := log.New(nil)
	interest := types.LiveOnlyMessageInterest
	s := newSession(feed.NewClient(nil, nil, logger), nil, nil, nil, rec, "ex", "od:sport:", false, logger, StrategyCatch, 0)
	impl, ok := s.(*oddsFeedSessionImpl)
	if !ok {
		t.Fatalf("newSession returned %T", s)
	}
	clk := &stepClock{}
	impl.clock = clk
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Open(ctx, []string{string(interest)}, &interest, false); err == nil {
		t.Fatal("Open succeeded without a broker")
	}
	got := rec.snapshot()
	if len(got) == 0 || got[0].kind != "opened" {
		t.Fatalf("hook calls = %+v, want the registration first", got)
	}
	if got[0].at.Mono() != time.Millisecond {
		t.Fatalf("the registration carries the reading %v, want the clock's first (1ms)", got[0].at.Mono())
	}
	if clk.reads() != 1 {
		t.Fatalf("the session read the clock %d times to register, want once", clk.reads())
	}
}
