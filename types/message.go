package types

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
	Product() int
	Timestamp() time.Time
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
//
// v2.30 reshape: RequestID() migrated from *int to Optional[int].
// The interface is embedded into OddsChange, BetStop, BetSettlement,
// BetCancel, FixtureChangeMessage, RollbackBetSettlement, and
// RollbackBetCancel — every public message type carrying a request
// id. None means the upstream feed did not include a request id
// (typical for non-recovery-correlated messages).
type RequestMessage interface {
	Message
	RequestID() Optional[int]
	RawMessage() []byte
}

// UnparsableMessage ...
type UnparsableMessage interface {
	Message
	WithEvent
	RawMessage() []byte
}

// WithEvent is the per-message accessor that returns the event
// entity (Match / Tournament / nil) the feed message addresses.
//
// v2.25 rename: formerly named `EventMessage`. The name was repurposed
// for the new top-level union struct (the envelope shape on
// SessionMessage). `WithEvent` describes what this interface
// actually does — every concrete message type that carries an
// associated event entity embeds it.
//
// The Event() return type stays interface{} for now; consumers
// type-assert to the VALUE types types.Match or types.Tournament
// (the factories store values, not pointers — a *Match assertion
// always fails). nil when the message carries no resolvable event.
// Tightening to a typed union is a separate reshape.
type WithEvent interface {
	Event() interface{}
}

// EventMessage is the top-level tagged union of every parsed feed
// message type. Exactly one field is non-nil per envelope; consumers
// branch with simple `if env.OddsChange != nil` checks instead of
// type-switching on an `interface{}`.
//
// v2.25 reshape: replaces the prior
// `SessionMessage.Message interface{}` field. The interface{} form
// required type-switching on every consumer call site and risked
// silently mishandling unexpected concrete types. The union is
// IDE-discoverable (every variant shows up in autocomplete),
// panic-free (no failed type assertion), and embeds cleanly into
// SessionMessage so the consumer surface reads as
// `for env := range sub.Messages() { if env.OddsChange != nil … }`.
//
// Producers (internal/factory + session) populate exactly one field
// per envelope based on the built message's interface type.
type EventMessage struct {
	OddsChange            OddsChange
	BetStop               BetStop
	BetSettlement         BetSettlement
	BetCancel             BetCancel
	FixtureChange         FixtureChangeMessage
	RollbackBetSettlement RollbackBetSettlement
	RollbackBetCancel     RollbackBetCancel
}

// OddsChange ...
type OddsChange interface {
	RequestMessage
	WithEvent
	Markets() []MarketWithOdds
}

// BetStopMarker is the embeddable token that distinguishes BetStop
// from other RequestMessage+WithEvent shapes (BetCancel,
// FixtureChange, etc.) in a type-switch. BetStop has no payload of
// its own — every other Request*Message type adds a Markets() / time
// / change-type method that distinguishes it; without a marker, a
// type-switch case for BetStop would match any structurally-equivalent
// shape.
//
// The marker is public so concrete impls outside the types package
// (e.g. internal/factory.betStopImpl) can compose it; the satisfying
// method `isBetStop()` is unexported so only the marker struct
// implements it — preventing accidental "looks like a BetStop"
// matches by unrelated value types.
type BetStopMarker struct{}

// isBetStop is the sealed-interface marker. The `unused` linter
// (staticcheck U1000) cannot trace its consumption through interface
// satisfaction in other packages — concrete impls embed BetStopMarker
// and the session loop's type-switch dispatches via types.BetStop —
// so the //lint:ignore directive is intentional. golangci-lint
// honors the same staticcheck directive, so a separate
// //nolint:unused is unnecessary (and would itself be flagged by
// nolintlint as a redundant directive).
//
//lint:ignore U1000 satisfies types.BetStop via interface dispatch (used by session.go + internal/factory)
func (BetStopMarker) isBetStop() {}

// BetStop ...
//
// Concrete impls satisfy this interface by embedding BetStopMarker.
// The unexported isBetStop() method makes BetStop structurally
// distinct from BetCancel / FixtureChange / Rollback* (each of which
// has its own distinguishing method).
type BetStop interface {
	RequestMessage
	WithEvent
	isBetStop()
}

// BetSettlement ...
type BetSettlement interface {
	RequestMessage
	WithEvent
	Markets() []MarketWithSettlement
}

// BetCancel ...
type BetCancel interface {
	RequestMessage
	WithEvent
	StartTime() *time.Time
	EndTime() *time.Time
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
	WithEvent
	ChangeType() FixtureChangeType
}

// RollbackBetSettlement ...
type RollbackBetSettlement interface {
	RequestMessage
	WithEvent
	RolledBackSettledMarkets() []Market
}

// RollbackBetCancel ...
type RollbackBetCancel interface {
	RequestMessage
	WithEvent
	StartTime() *time.Time
	EndTime() *time.Time
	RolledBackCanceledMarkets() []Market
}
