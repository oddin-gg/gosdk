package gosdk

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/internal/recovery"
	"github.com/oddin-gg/gosdk/types"
)

// lossRecordingProcessor records OnFeedChannelLost calls; every other
// hook is a no-op.
type lossRecordingProcessor struct {
	mu      sync.Mutex
	session uuid.UUID
	calls   int
	// restored are the sessions whose queue was reported bound.
	restored []uuid.UUID
}

func (p *lossRecordingProcessor) OnFeedChannelLost(session uuid.UUID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.session = session
	p.calls++
}
func (p *lossRecordingProcessor) OnFeedChannelRestored(session uuid.UUID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.restored = append(p.restored, session)
}
func (*lossRecordingProcessor) OnFeedSessionGone(uuid.UUID) {}
func (*lossRecordingProcessor) OnClientSessionOpened(uuid.UUID, types.MessageInterest, recovery.Stamp) {
}
func (*lossRecordingProcessor) OnMessageProcessingStarted(uuid.UUID, int, recovery.Stamp)         {}
func (*lossRecordingProcessor) OnMessageProcessingEnded(uuid.UUID, int, time.Time)                {}
func (*lossRecordingProcessor) OnAliveReceived(int, types.MessageTimestamp, recovery.Stamp, bool) {}
func (*lossRecordingProcessor) OnSnapshotCompleteReceived(context.Context, uuid.UUID, int, int) error {
	return nil
}

// TestNewSession_ChannelLostHookDelegatesToRecovery pins the one line of
// wiring between a lost consumer queue and the recovery that closes its
// gap: a live session's consumer reports a channel loss to the recovery
// processor, under the session's id; a replay session's consumer reports
// nothing (historical traffic has no live gap to recover, and flagging
// live producers down for a replay queue rebind would force spurious
// snapshot recoveries).
func TestNewSession_ChannelLostHookDelegatesToRecovery(t *testing.T) {
	for _, tc := range []struct {
		name      string
		isReplay  bool
		wantCalls int
	}{
		{"live session delegates to the recovery processor", false, 1},
		{"replay session stays silent", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &lossRecordingProcessor{}
			s := newSession(nil, nil, nil, nil, rec, "ex", "od:sport:", tc.isReplay, log.New(nil), StrategyCatch, 0)
			impl, ok := s.(*oddsFeedSessionImpl)
			if !ok {
				t.Fatalf("newSession returned %T", s)
			}
			if got := impl.channelConsumer.ChannelLostHookInstalled(); got != (tc.wantCalls > 0) {
				t.Fatalf("hook installed = %v, want %v", got, tc.wantCalls > 0)
			}
			if r, g := impl.channelConsumer.ChannelLifecycleHooksInstalled(); r != (tc.wantCalls > 0) || g != (tc.wantCalls > 0) {
				t.Fatalf("restored/gone hooks installed = %v/%v, want %v", r, g, tc.wantCalls > 0)
			}
			impl.channelConsumer.ReportChannelLost(time.Now().Add(-time.Second))
			rec.mu.Lock()
			defer rec.mu.Unlock()
			if rec.calls != tc.wantCalls {
				t.Fatalf("recovery processor calls = %d, want %d", rec.calls, tc.wantCalls)
			}
			if tc.wantCalls == 1 && rec.session != impl.sessionID {
				t.Fatalf("hook reported session %s, want the session's own id %s", rec.session, impl.sessionID)
			}
		})
	}
}

// TestNewSession_ChannelBoundHookReportsTheSessionsQueueBoundToRecovery pins the other
// line of wiring: the consumer reports its first queue bound, from Open, and that
// reaches the recovery processor under the session's id. Without it a session
// registered with its queue not bound would never be reported bound, and nothing
// would be recovered for its producers. A replay session reports nothing.
func TestNewSession_ChannelBoundHookReportsTheSessionsQueueBoundToRecovery(t *testing.T) {
	for _, tc := range []struct {
		name     string
		isReplay bool
		want     int
	}{
		{"live session reports its queue bound", false, 1},
		{"replay session stays silent", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &lossRecordingProcessor{}
			s := newSession(nil, nil, nil, nil, rec, "ex", "od:sport:", tc.isReplay, log.New(nil), StrategyCatch, 0)
			impl := s.(*oddsFeedSessionImpl)
			impl.channelConsumer.ReportChannelBound()
			rec.mu.Lock()
			defer rec.mu.Unlock()
			if len(rec.restored) != tc.want {
				t.Fatalf("queue reported bound %d times, want %d", len(rec.restored), tc.want)
			}
			if tc.want == 1 && rec.restored[0] != impl.sessionID {
				t.Fatalf("reported session %s, want the session's own id %s", rec.restored[0], impl.sessionID)
			}
		})
	}
}
