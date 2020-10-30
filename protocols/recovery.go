package protocols

import "time"
import "github.com/google/uuid"

// RecoveryMessage ...
type RecoveryMessage struct {
	ProducerStatus       ProducerStatus
	EventRecoveryMessage EventRecoveryMessage
}

// RecoveryManager ...
type RecoveryManager interface {
	InitiateEventOddsMessagesRecovery(producerID uint, eventID URN) (uint, error)
	InitiateEventStatefulMessagesRecovery(producerID uint, eventID URN) (uint, error)
}

// RecoveryMessageProcessor ...
type RecoveryMessageProcessor interface {
	OnMessageProcessingStarted(sessionID uuid.UUID, producerID uint, timestamp time.Time)
	OnMessageProcessingEnded(sessionID uuid.UUID, producerID uint, timestamp time.Time)
	OnAliveReceived(producerID uint, timestamp MessageTimestamp, isSubscribed bool, messageInterest MessageInterest)
	OnSnapshotCompleteReceived(producerID uint, requestID uint, messageInterest MessageInterest)
}

// RecoveryState ...
type RecoveryState int

// RecoveryStates
const (
	DefaultRecoveryState     RecoveryState = 0
	NotStartedRecoveryState  RecoveryState = 1
	StartedRecoveryState     RecoveryState = 2
	CompletedRecoveryState   RecoveryState = 3
	InterruptedRecoveryState RecoveryState = 4
	ErrorRecoveryState       RecoveryState = 5
)

// ProducerStatusReason ...
type ProducerStatusReason int

// ProducerStatusReasons
const (
	ErrorProducerStatusReason                          ProducerStatusReason = 0
	FirstRecoveryCompletedProducerStatusReason         ProducerStatusReason = 1
	ProcessingQueueDelayStabilizedProducerStatusReason ProducerStatusReason = 2
	ReturnedFromInactivityProducerStatusReason         ProducerStatusReason = 3
	AliveIntervalViolationProducerStatusReason         ProducerStatusReason = 4
	ProcessingQueueDelayViolationProducerStatusReason  ProducerStatusReason = 5
	OtherProducerStatusReason                          ProducerStatusReason = 6
)

// ProducerDownReason ...
type ProducerDownReason int

// ProducerDownReasons
const (
	DefaultProducerDownReason                       ProducerDownReason = 0
	AliveInternalViolationProducerDownReason        ProducerDownReason = 1
	ProcessingQueueDelayViolationProducerDownReason ProducerDownReason = 2
	OtherProducerDownReason                         ProducerDownReason = 6
)

// ToProducerStatusReason ...
func (p ProducerDownReason) ToProducerStatusReason() ProducerStatusReason {
	switch p {
	case AliveInternalViolationProducerDownReason:
		return AliveIntervalViolationProducerStatusReason
	case ProcessingQueueDelayViolationProducerDownReason:
		return ProcessingQueueDelayViolationProducerStatusReason
	case OtherProducerDownReason:
		return OtherProducerStatusReason
	default:
		return ErrorProducerStatusReason
	}
}

// ProducerUpReason ...
type ProducerUpReason int

// ProducerUpReasons
const (
	DefaultProducerUpReason                        ProducerUpReason = 0
	FirstRecoveryCompletedProducerUpReason         ProducerUpReason = 1
	ProcessingQueueDelayStabilizedProducerUpReason ProducerUpReason = 2
	ReturnedFromInactivityProducerUpReason         ProducerUpReason = 3
)

// ToProducerStatusReason ...
func (p ProducerUpReason) ToProducerStatusReason() ProducerStatusReason {
	switch p {
	case FirstRecoveryCompletedProducerUpReason:
		return FirstRecoveryCompletedProducerStatusReason
	case ProcessingQueueDelayStabilizedProducerUpReason:
		return ProcessingQueueDelayStabilizedProducerStatusReason
	case ReturnedFromInactivityProducerUpReason:
		return ReturnedFromInactivityProducerStatusReason
	default:
		return ErrorProducerStatusReason
	}
}
