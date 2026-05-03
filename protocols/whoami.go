package protocols

import (
	"context"
	"time"
)

// BookmakerDetail ...
type BookmakerDetail interface {
	ExpireAt() time.Time
	BookmakerID() uint
	VirtualHost() string
}

// WhoAmIManager ...
type WhoAmIManager interface {
	BookmakerDetails(ctx context.Context) (BookmakerDetail, error)
}
