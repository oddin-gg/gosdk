// Package types holds the SDK's public data shapes: entity value
// structs (Match, Tournament, Competitor, Player, market/outcome
// types), feed-message payloads (SessionMessage variants), identifiers
// (URN, Locale, Environment, Region, ProducerScope, MessageInterest),
// and the Optional[T] container used for maybe-present values.
//
// Almost everything here is a pure-data value struct whose methods are
// plain field reads — no I/O, no errors. Three types remain interfaces:
// Producer, whose accessors DO read live, manager-owned state (enabled/
// flagged-down/timestamps track the catalog as it changes);
// BookmakerDetail; and FixtureChange, a legacy immutable interface whose
// accessors return fixed values (it is an entity interface, distinct from
// the FixtureChangeMessage feed payload — neither reads live state).
//
// Types marked "NOT part of the supported v1 API" (MarketData,
// FeedMessage, QueueMessage, RecoveryMessage, …) are exported only for
// cross-package plumbing inside the SDK and may change or move under
// internal/ in any release.
package types
