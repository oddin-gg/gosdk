package recovery

import (
	"github.com/google/uuid"
	"github.com/oddin-gg/gosdk/protocols"
	"time"
)

// DummyManager ...
type DummyManager struct {
}

// OnMessageProcessingStarted ...
func (d DummyManager) OnMessageProcessingStarted(sessionID uuid.UUID, producerID uint, timestamp time.Time) {
	return
}

// OnMessageProcessingEnded ...
func (d DummyManager) OnMessageProcessingEnded(sessionID uuid.UUID, producerID uint, timestamp time.Time) {
	return
}

// OnAliveReceived ...
func (d DummyManager) OnAliveReceived(producerID uint, timestamp protocols.MessageTimestamp, isSubscribed bool, messageInterest protocols.MessageInterest) {
	return
}

// OnSnapshotCompleteReceived ...
func (d DummyManager) OnSnapshotCompleteReceived(producerID uint, requestID uint, messageInterest protocols.MessageInterest) {
	return
}
