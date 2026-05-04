package recovery

import (
	"time"

	"github.com/google/uuid"
	"github.com/oddin-gg/gosdk/types"
)

// DummyManager ...
type DummyManager struct {
}

// OnMessageProcessingStarted ...
func (d DummyManager) OnMessageProcessingStarted(sessionID uuid.UUID, producerID uint, timestamp time.Time) {
}

// OnMessageProcessingEnded ...
func (d DummyManager) OnMessageProcessingEnded(sessionID uuid.UUID, producerID uint, timestamp time.Time) {
}

// OnAliveReceived ...
func (d DummyManager) OnAliveReceived(producerID uint, timestamp types.MessageTimestamp, isSubscribed bool, messageInterest types.MessageInterest) {
}

// OnSnapshotCompleteReceived ...
func (d DummyManager) OnSnapshotCompleteReceived(producerID uint, requestID uint, messageInterest types.MessageInterest) {
}
