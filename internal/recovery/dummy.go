package recovery

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/oddin-gg/gosdk/types"
)

// DummyManager ...
type DummyManager struct {
}

// OnMessageProcessingStarted ...
func (d DummyManager) OnMessageProcessingStarted(sessionID uuid.UUID, producerID int, taken Stamp) {
}

// OnMessageProcessingEnded ...
func (d DummyManager) OnMessageProcessingEnded(sessionID uuid.UUID, producerID int, timestamp time.Time) {
}

// OnAliveReceived ...
func (d DummyManager) OnAliveReceived(producerID int, timestamp types.MessageTimestamp, received Stamp, isSubscribed bool) {
}

// OnSnapshotCompleteReceived ...
func (d DummyManager) OnSnapshotCompleteReceived(ctx context.Context, session uuid.UUID, producerID int, requestID int) error {
	return nil
}

// OnMessageProcessed ...
func (d DummyManager) OnMessageProcessed(uuid.UUID, int, time.Time, Stamp, int) {}

// OnSessionAlive ...
func (d DummyManager) OnSessionAlive(uuid.UUID, int, time.Time, Stamp, bool, bool) {
}

// OnFeedChannelLost ...
func (d DummyManager) OnFeedChannelLost(uuid.UUID) {}

func (d DummyManager) OnFeedChannelRestored(uuid.UUID) {}

// RegisterSessionResetter ...
func (d DummyManager) RegisterSessionResetter(uuid.UUID, Resetter) {}

// OnFeedChannelResetDone ...
func (d DummyManager) OnFeedChannelResetDone(uuid.UUID, uint64, bool) {}

func (d DummyManager) OnFeedSessionGone(uuid.UUID) {}

func (d DummyManager) OnClientSessionOpened(uuid.UUID, types.MessageInterest, Stamp) {}
