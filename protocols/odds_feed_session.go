package protocols

import (
	"github.com/google/uuid"
)

// SessionMessageDelivery ...
type SessionMessageDelivery <-chan SessionMessage

// OddsFeedSession ...
type OddsFeedSession interface {
	ID() uuid.UUID
	RespCh() SessionMessageDelivery
}

// OddsFeedSessionBuilder ...
type OddsFeedSessionBuilder interface {
	SetMessageInterest(messageInterest MessageInterest) OddsFeedSessionBuilder
	SetSpecificEventsOnly(specificEvents []URN) OddsFeedSessionBuilder
	SetSpecificEventOnly(specificEventOnly URN) OddsFeedSessionBuilder
	Build() (SessionMessageDelivery, error)
	BuildReplay() (SessionMessageDelivery, error)
}
