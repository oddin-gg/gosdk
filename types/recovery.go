package types

import (
	"time"
)

// RecoveryMessage is pipeline wiring between the recovery manager and
// the client's event pump. Exported for cross-package plumbing only —
// NOT part of the supported v1 API (consumers receive
// gosdk.RecoveryEvent); it may change or move under internal/ in any
// release.
type RecoveryMessage struct {
	ProducerStatus       ProducerStatus
	EventRecoveryMessage EventRecoveryMessage
}

// EventRecoveryMessage carries the per-event recovery completion
// payload published on RecoveryEvents() when a SnapshotComplete
// arrives for a request initiated via Client.RecoverEventOdds /
// RecoverEventStateful.
type EventRecoveryMessage interface {
	Message
	EventID() URN
	RequestID() int
}

// RecoveryRequestStatus is the terminal state of a per-request recovery.
type RecoveryRequestStatus int

const (
	// RecoveryStatusPending is the initial state — the API request was
	// accepted but no SnapshotComplete has arrived yet.
	RecoveryStatusPending RecoveryRequestStatus = iota
	// RecoveryStatusCompleted means SnapshotComplete arrived; recovery succeeded.
	RecoveryStatusCompleted
	// RecoveryStatusFailed means a downstream error or the producer was
	// flagged-down before recovery completed.
	RecoveryStatusFailed
	// RecoveryStatusTimedOut means the configured MaxRecoveryExecution
	// elapsed before the corresponding SnapshotComplete arrived.
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
	RequestID  int
	ProducerID int
	EventID    URN
	Status     RecoveryRequestStatus
	Err        error
	StartedAt  time.Time
	EndedAt    time.Time
}

// The session→recovery processing hooks (formerly
// types.RecoveryMessageProcessor) live unexported in the gosdk root
// package as of v1.0.0 — internal wiring, not consumer surface.
