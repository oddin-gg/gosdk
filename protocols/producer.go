package protocols

import "time"

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
	AvailableProducers() (map[uint]Producer, error)
	ActiveProducers() (map[uint]Producer, error)
	GetProducer(id uint) (Producer, error)
	SetProducerState(id uint, enabled bool) error
	SetProducerRecoveryFromTimestamp(producerID uint, timestamp time.Time) error
	IsProducerEnabled(id uint) (bool, error)
	IsProducerDown(id uint) (bool, error)
}
