package gosdk

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/oddin-gg/gosdk/internal/feed"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

// sessionHookEvent is one call of the recovery processor that registers a
// client session.
type sessionHookEvent struct {
	kind     string // "opened" or "gone"
	session  uuid.UUID
	interest types.MessageInterest
}

// sessionHookRecorder keeps the order of the registration hooks; every
// other hook is a no-op.
type sessionHookRecorder struct {
	mu     sync.Mutex
	events []sessionHookEvent
}

func (r *sessionHookRecorder) OnClientSessionOpened(s uuid.UUID, mi types.MessageInterest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, sessionHookEvent{"opened", s, mi})
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

func (*sessionHookRecorder) OnFeedChannelLost(uuid.UUID, types.MessageInterest, time.Time) {}
func (*sessionHookRecorder) OnFeedChannelRestored(uuid.UUID)                               {}
func (*sessionHookRecorder) OnRecoveryMessageReceived(int, int)                            {}
func (*sessionHookRecorder) OnMessageProcessingStarted(uuid.UUID, int, time.Time)          {}
func (*sessionHookRecorder) OnMessageProcessingEnded(uuid.UUID, int, time.Time)            {}
func (*sessionHookRecorder) OnAliveReceived(int, types.MessageTimestamp, bool, types.MessageInterest) {
}
func (*sessionHookRecorder) OnSnapshotCompleteReceived(context.Context, int, int, types.MessageInterest) error {
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
		want := []sessionHookEvent{{"opened", id, interest}, {kind: "gone", session: id}}
		got := rec.snapshot()
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("%s: hook calls = %+v, want %+v", interest, got, want)
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
	if len(got) == 0 || got[0] != (sessionHookEvent{"opened", id, types.SystemAliveOnly}) {
		t.Fatalf("hook calls = %+v, want the alive-only session reported as opened", got)
	}
}
