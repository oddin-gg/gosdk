package types

import (
	"slices"
	"time"
)

// ProducerScope identifies the market phase a producer serves. It is a
// typed string whose values match the feed's wire representation, so a
// scope is self-describing in logs and JSON and maps trivially from the
// producer catalog. A producer may serve more than one scope (see
// Producer.ProducerScopes and the ProducerHasScope helper).
type ProducerScope string

// ProducerScopes
const (
	LiveProducerScope     ProducerScope = "live"
	PrematchProducerScope ProducerScope = "prematch"
)

// RecoveryInfo ...
//
// v2.29 reshape: NodeID() migrated from *int to Optional[int].
type RecoveryInfo interface {
	After() time.Time
	Timestamp() time.Time
	RequestID() int
	Successful() bool
	NodeID() Optional[int]
}

// Producer ...
type Producer interface {
	ID() int
	Name() string
	Description() string
	LastMessageTimestamp() time.Time
	IsAvailable() bool
	IsEnabled() bool
	IsFlaggedDown() bool
	APIEndpoint() string
	// ProducerScopes returns every market phase this producer serves.
	ProducerScopes() []ProducerScope
	LastProcessedMessageGenTimestamp() time.Time
	ProcessingQueDelay() time.Duration
	TimestampForRecovery() time.Time
	StatefulRecoveryWindowInMinutes() int
	RecoveryInfo() *RecoveryInfo
}

// ProducerHasScope reports whether the producer serves the given scope.
// It is a free function rather than a Producer method on purpose: the
// result is fully derivable from ProducerScopes(), so adding it to the
// interface would force every consumer mock / fake of Producer to
// implement it for no benefit. Consumers who want the membership check
// call this; the SDK uses it internally too.
//
// A nil producer has no scopes — fail closed rather than panic, matching
// MessageInterest.IsProducerInterested's nil handling.
func ProducerHasScope(p Producer, scope ProducerScope) bool {
	if p == nil {
		return false
	}
	return slices.Contains(p.ProducerScopes(), scope)
}

// ProducerStatus is the producer-side payload published on
// RecoveryEvents() when a producer transitions up / down. v2.33
// relocated from types/message.go (the interface is a producer-state
// concept, not a message concept).
type ProducerStatus interface {
	Message
	IsDown() bool
	IsDelayed() bool
	ProducerStatusReason() ProducerStatusReason
}

// RecoveryState is the per-producer recovery state-machine label —
// distinct from RecoveryRequestStatus (per-request terminal state).
// v2.33 relocated from types/recovery.go.
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

// ProducerStatusReason is the reason behind a producer status
// transition (paired with a Up/Down direction). v2.33 relocated
// from types/recovery.go.
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

// ProducerDownReason narrows the reason for a producer-down
// transition. v2.33 relocated from types/recovery.go.
type ProducerDownReason int

// ProducerDownReasons
const (
	DefaultProducerDownReason                       ProducerDownReason = 0
	AliveInternalViolationProducerDownReason        ProducerDownReason = 1
	ProcessingQueueDelayViolationProducerDownReason ProducerDownReason = 2
	OtherProducerDownReason                         ProducerDownReason = 6
)

// ToProducerStatusReason maps a producer-down reason to the broader
// ProducerStatusReason space.
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

// ProducerUpReason narrows the reason for a producer-up transition.
// v2.33 relocated from types/recovery.go.
type ProducerUpReason int

// ProducerUpReasons
const (
	DefaultProducerUpReason                        ProducerUpReason = 0
	FirstRecoveryCompletedProducerUpReason         ProducerUpReason = 1
	ProcessingQueueDelayStabilizedProducerUpReason ProducerUpReason = 2
	ReturnedFromInactivityProducerUpReason         ProducerUpReason = 3
)

// ToProducerStatusReason maps a producer-up reason to the broader
// ProducerStatusReason space.
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
