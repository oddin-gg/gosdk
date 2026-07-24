package types

import (
	"time"
)

// BookmakerDetail describes the calling tenant — the bookmaker id,
// AMQP virtual host the SDK should connect to, and access expiry.
//
// The behavioural surface that fetches this (formerly
// types.WhoAmIManager) lives in the gosdk root package as of v2.25
// (unexported since v1.0.0); types/ is data-shape-only.
type BookmakerDetail interface {
	ExpireAt() time.Time
	BookmakerID() int
	VirtualHost() string
}
