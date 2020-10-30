package protocols

import (
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
	BookmakerDetails() (BookmakerDetail, error)
}
