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
func (d DummyManager) OnMessageProcessingStarted(sessionID uuid.UUID, producerID int, timestamp time.Time) {
}

// OnMessageProcessingEnded ...
func (d DummyManager) OnMessageProcessingEnded(sessionID uuid.UUID, producerID int, timestamp time.Time) {
}

// OnAliveReceived ...
func (d DummyManager) OnAliveReceived(producerID int, timestamp types.MessageTimestamp, isSubscribed bool, messageInterest types.MessageInterest) {
}

// OnSnapshotCompleteReceived ...
func (d DummyManager) OnSnapshotCompleteReceived(ctx context.Context, producerID int, requestID int, messageInterest types.MessageInterest) error {
	return nil
}
