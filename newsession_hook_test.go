package gosdk

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

// lossRecordingProcessor records OnFeedChannelLost calls; every other
// hook is a no-op.
type lossRecordingProcessor struct {
	mu    sync.Mutex
	calls []struct {
		interest types.MessageInterest
		lostAt   time.Time
	}
}

func (p *lossRecordingProcessor) OnFeedChannelLost(mi types.MessageInterest, lostAt time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, struct {
		interest types.MessageInterest
		lostAt   time.Time
	}{mi, lostAt})
}
func (*lossRecordingProcessor) OnMessageProcessingStarted(uuid.UUID, int, time.Time) {}
func (*lossRecordingProcessor) OnMessageProcessingEnded(uuid.UUID, int, time.Time)   {}
func (*lossRecordingProcessor) OnAliveReceived(int, types.MessageTimestamp, bool, types.MessageInterest) {
}
func (*lossRecordingProcessor) OnSnapshotCompleteReceived(context.Context, int, int, types.MessageInterest) error {
	return nil
}

// TestNewSession_ChannelLostHookDelegatesToRecovery pins the one line of
// wiring between a lost consumer queue and the recovery that closes its
// gap: a live session's consumer reports a channel loss to the recovery
// processor, with the loss instant; a replay session's consumer reports
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
			lost := time.Now().Add(-time.Second)
			impl.channelConsumer.ReportChannelLost(lost)
			rec.mu.Lock()
			defer rec.mu.Unlock()
			if len(rec.calls) != tc.wantCalls {
				t.Fatalf("recovery processor calls = %d, want %d", len(rec.calls), tc.wantCalls)
			}
			if tc.wantCalls == 1 && !rec.calls[0].lostAt.Equal(lost) {
				t.Fatalf("lostAt = %v, want %v passed through unchanged", rec.calls[0].lostAt, lost)
			}
		})
	}
}
