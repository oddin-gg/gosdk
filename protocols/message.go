package protocols

import "time"

// RoutingKeyInfo ...
type RoutingKeyInfo struct {
	FullRoutingKey     string
	SportID            *URN
	EventID            *URN
	IsSystemRoutingKey bool
}

// BasicMessage ...
type BasicMessage interface {
	Product() uint
	Timestamp() time.Time
}

// IDMessage ...
type IDMessage interface {
	GetEventID() string
}

// Message ...
type Message interface {
	Producer() Producer
	Timestamp() MessageTimestamp
}

// MessageTimestamp ...
type MessageTimestamp struct {
	Created   time.Time
	Sent      time.Time
	Received  time.Time
	Published time.Time
}

// RequestMessage ...
type RequestMessage interface {
	Message
	RequestID() *uint
	RawMessage() []byte
}

// UnparsableMessage ...
type UnparsableMessage interface {
	Message
	EventMessage
	RawMessage() []byte
}

// EventMessage ...
type EventMessage interface {
	Event() interface{}
}

// OddsChange ...
type OddsChange interface {
	RequestMessage
	EventMessage
	Markets() []MarketWithOdds
}

// BetStop ...
type BetStop interface {
	RequestMessage
	EventMessage
	isBetStop()
}

// BetSettlement ...
type BetSettlement interface {
	RequestMessage
	EventMessage
	Markets() []MarketWithSettlement
}

// BetCancel ...
type BetCancel interface {
	RequestMessage
	EventMessage
	Markets() []MarketCancel
}

// FixtureChangeType ...
type FixtureChangeType int

// FixtureChangeTypes
const (
	NewFixtureChangeType         FixtureChangeType = 1
	TimeUpdateChangeType         FixtureChangeType = 2
	CancelledFixtureChangeType   FixtureChangeType = 3
	OtherChangeFixtureChangeType FixtureChangeType = 4
	CoverageFixtureChangeType    FixtureChangeType = 5
	StreamURLFixtureChangeType   FixtureChangeType = 6
	UnknownFixtureChangeType     FixtureChangeType = 0
)

// FixtureChangeMessage ...
type FixtureChangeMessage interface {
	RequestMessage
	EventMessage
	ChangeType() FixtureChangeType
}

// ProducerStatus ...
type ProducerStatus interface {
	Message
	IsDown() bool
	IsDelayed() bool
	ProducerStatusReason() ProducerStatusReason
}

// RollbackBetSettlement ...
type RollbackBetSettlement interface {
	RequestMessage
	EventMessage
	RolledBackSettledMarkets() []Market
}

// RollbackBetCancel ...
type RollbackBetCancel interface {
	RequestMessage
	EventMessage
	RolledBackCanceledMarkets() []Market
}
