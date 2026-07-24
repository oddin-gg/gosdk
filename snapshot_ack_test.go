package gosdk

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	"github.com/oddin-gg/gosdk/types"
)

// fakeSnapshotProcessor is a recoveryMessageProcessor whose
// OnSnapshotCompleteReceived returns a configurable error, to drive the
// session's ack-only-when-admitted branch.
type fakeSnapshotProcessor struct {
	err   error
	calls atomic.Int32
}

func (f *fakeSnapshotProcessor) OnMessageProcessingStarted(uuid.UUID, int, time.Time) {}
func (f *fakeSnapshotProcessor) OnMessageProcessingEnded(uuid.UUID, int, time.Time)   {}
func (f *fakeSnapshotProcessor) OnAliveReceived(int, types.MessageTimestamp, bool, types.MessageInterest) {
}
func (f *fakeSnapshotProcessor) OnSnapshotCompleteReceived(context.Context, int, int, types.MessageInterest) error {
	f.calls.Add(1)
	return f.err
}

// TestSession_SnapshotComplete_AckOnlyWhenAdmitted pins the High-finding
// fix: a snapshot_complete delivery is acked ONLY when the recovery
// actor admits it. If admission fails (ctx cancelled / recovery manager
// shutting down), the delivery must stay UNACKED so the broker
// redelivers — a dropped-then-acked completion would strand recovery
// until MaxRecoveryExecution.
func TestSession_SnapshotComplete_AckOnlyWhenAdmitted(t *testing.T) {
	newFM := func() *types.FeedMessage {
		return &types.FeedMessage{
			BasicFeedMessage: types.BasicFeedMessage{
				Timestamp: types.MessageTimestamp{Created: time.Now()},
			},
			Message: &feedXML.SnapshotComplete{ProductID: 1, RequestID: 42},
		}
	}

	t.Run("admitted -> acked", func(t *testing.T) {
		proc := &fakeSnapshotProcessor{err: nil}
		o := &oddsFeedSessionImpl{
			cacheManager:             &spyCacheNotifier{},
			recoveryMessageProcessor: proc,
			logger:                   discardLogger(),
			msgCh:                    make(chan sessionEnvelope, 1),
			sessionID:                uuid.New(),
		}
		var acked atomic.Int32
		o.processFeedMessage(t.Context(), newFM(), types.AllMessageInterest, func() { acked.Add(1) }, false, nil)
		if proc.calls.Load() != 1 {
			t.Fatalf("OnSnapshotCompleteReceived calls = %d, want 1", proc.calls.Load())
		}
		if acked.Load() != 1 {
			t.Fatalf("admitted snapshot_complete acks = %d, want 1", acked.Load())
		}
	})

	t.Run("not admitted -> not acked", func(t *testing.T) {
		proc := &fakeSnapshotProcessor{err: errors.New("recovery manager closed")}
		o := &oddsFeedSessionImpl{
			cacheManager:             &spyCacheNotifier{},
			recoveryMessageProcessor: proc,
			logger:                   discardLogger(),
			msgCh:                    make(chan sessionEnvelope, 1),
			sessionID:                uuid.New(),
		}
		var acked atomic.Int32
		o.processFeedMessage(t.Context(), newFM(), types.AllMessageInterest, func() { acked.Add(1) }, false, nil)
		if proc.calls.Load() != 1 {
			t.Fatalf("OnSnapshotCompleteReceived calls = %d, want 1", proc.calls.Load())
		}
		if acked.Load() != 0 {
			t.Fatalf("un-admitted snapshot_complete acks = %d, want 0 (must be redelivered)", acked.Load())
		}
	})
}
