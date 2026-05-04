package types

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// RecoveryMessage ...
type RecoveryMessage struct {
	ProducerStatus       ProducerStatus
	EventRecoveryMessage EventRecoveryMessage
}

// RecoveryManager ...
//
// Phase 6.1 reshape: returns the bare request id. The richer
// *gosdk.RecoveryHandle is constructed at the gosdk.Client layer; the
// types-level interface stays minimal so this package has no
// dependency on the public Client types.
type RecoveryManager interface {
	InitiateEventOddsMessagesRecovery(ctx context.Context, producerID uint, eventID URN) (uint, error)
	InitiateEventStatefulMessagesRecovery(ctx context.Context, producerID uint, eventID URN) (uint, error)
}

// RecoveryRequestStatus is the terminal state of a per-request recovery.
type RecoveryRequestStatus int

const (
	// RecoveryStatusPending is the initial state — the API request was
	// accepted but no SnapshotComplete has arrived yet.
	RecoveryStatusPending RecoveryRequestStatus = iota
	// RecoveryStatusCompleted: SnapshotComplete arrived; recovery succeeded.
	RecoveryStatusCompleted
	// RecoveryStatusFailed: a downstream error or the producer was
	// flagged-down before recovery completed.
	RecoveryStatusFailed
	// RecoveryStatusTimedOut: the configured MaxRecoveryExecution elapsed
	// before the corresponding SnapshotComplete arrived.
	RecoveryStatusTimedOut
)

// String returns a stable human-readable label.
func (s RecoveryRequestStatus) String() string {
	switch s {
	case RecoveryStatusPending:
		return "pending"
	case RecoveryStatusCompleted:
		return "completed"
	case RecoveryStatusFailed:
		return "failed"
	case RecoveryStatusTimedOut:
		return "timed_out"
	default:
		return "unknown"
	}
}

// RecoveryResult is the outcome of a single recovery request.
type RecoveryResult struct {
	RequestID uint
	ProducerID uint
	EventID   URN
	Status    RecoveryRequestStatus
	Err       error
	StartedAt time.Time
	EndedAt   time.Time
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
