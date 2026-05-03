package protocols

import (
	"context"
	"time"
)

// ProducerScope ...
type ProducerScope int

// ProducerScopes
const (
	LiveProducerScope     ProducerScope = 1
	PrematchProducerScope ProducerScope = 2
)

// RecoveryInfo ...
type RecoveryInfo interface {
	After() time.Time
	Timestamp() time.Time
	RequestID() uint
	Successful() bool
	NodeID() *int
}

// Producer ...
type Producer interface {
	ID() uint
	Name() string
	Description() string
	LastMessageTimestamp() time.Time
	IsAvailable() bool
	IsEnabled() bool
	IsFlaggedDown() bool
	APIEndpoint() string
	ProducerScopes() []ProducerScope
	LastProcessedMessageGenTimestamp() time.Time
	ProcessingQueDelay() time.Duration
	TimestampForRecovery() time.Time
	StatefulRecoveryWindowInMinutes() uint
	RecoveryInfo() *RecoveryInfo
}

// ProducerManager ...
type ProducerManager interface {
	AvailableProducers(ctx context.Context) (map[uint]Producer, error)
	ActiveProducers(ctx context.Context) (map[uint]Producer, error)
	ActiveProducersInScope(ctx context.Context, scope ProducerScope) (map[uint]Producer, error)
	GetProducer(ctx context.Context, id uint) (Producer, error)
	SetProducerState(ctx context.Context, id uint, enabled bool) error
	SetProducerRecoveryFromTimestamp(ctx context.Context, producerID uint, timestamp time.Time) error
	IsProducerEnabled(ctx context.Context, id uint) (bool, error)
	IsProducerDown(ctx context.Context, id uint) (bool, error)
}
