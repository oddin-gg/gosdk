package types

// BasicFeedMessage is the shared base of the feed-pipeline envelopes.
// The RawFeedMessage side (extended-data reporting) is supported
// consumer surface; FeedMessage and QueueMessage below are PIPELINE
// WIRING — exported only for cross-package plumbing, NOT part of the
// supported v1 API, and may change or move under internal/ in any
// release. Consumers interact with SessionMessage and its variants.
type BasicFeedMessage struct {
	RawMessage []byte
	RoutingKey *RoutingKeyInfo
	Timestamp  MessageTimestamp
}

// FeedMessage ...
type FeedMessage struct {
	BasicFeedMessage
	Message BasicMessage
}

// RawFeedMessage ...
type RawFeedMessage struct {
	BasicFeedMessage
	Message         interface{}
	MessageInterest MessageInterest
}

// QueueMessage is pipeline wiring — see the BasicFeedMessage note:
// exported for cross-package plumbing only, NOT supported v1 API.
type QueueMessage struct {
	RawFeedMessage    *RawFeedMessage
	FeedMessage       *FeedMessage
	UnparsableMessage UnparsableMessage
}

// SessionMessage is the envelope a Subscription delivers per parsed
// AMQP delivery. Exactly one of:
//
//   - one of the embedded EventMessage variants (OddsChange, BetStop,
//     BetSettlement, BetCancel, FixtureChange, RollbackBetSettlement,
//     RollbackBetCancel),
//   - UnparsableMessage (the SDK couldn't decode the body),
//   - RawFeedMessage (only when WithExtendedDataReporting(true)),
//
// is meaningful per envelope.
//
// Extended-data expansion: with WithExtendedDataReporting(true), ONE
// decodable AMQP delivery becomes TWO envelopes on Messages(), in
// order — first an envelope carrying only RawFeedMessage (all variant
// fields nil), then the envelope carrying the parsed variant or
// UnparsableMessage (RawFeedMessage nil). Never both in one envelope.
// Deliveries the session drops (alive traffic, disabled/out-of-scope
// producer) emit only the raw envelope; bodies that fail XML decode
// emit only an UnparsableMessage envelope (raw is built for decodable
// bodies only). Consumers counting or correlating deliveries must not
// count RawFeedMessage envelopes as separate deliveries. With
// reporting OFF: at most one envelope per delivery — deliveries the
// session intentionally drops (alive traffic, disabled/out-of-scope
// producer) produce ZERO envelopes.
//
// Consumers branch with simple nil checks:
//
//	for env := range sub.Messages() {
//	    switch {
//	    case env.OddsChange != nil:    handle(env.OddsChange)
//	    case env.BetSettlement != nil: handle(env.BetSettlement)
//	    case env.UnparsableMessage != nil: ...
//	    }
//	}
//
// v2.25 reshape: replaced the prior `Message interface{}` field with
// the embedded EventMessage union — IDE-discoverable, panic-free
// (no failed type assertion), and forces consumers to enumerate
// message variants instead of falling through a default branch.
type SessionMessage struct {
	RawFeedMessage *RawFeedMessage
	EventMessage
	UnparsableMessage UnparsableMessage
}
