package types

// SessionMessageDelivery is the channel type subscriptions deliver
// SessionMessage values on. Kept as a type alias so consumers can
// reference a single name when wiring channel handoffs.
type SessionMessageDelivery <-chan SessionMessage
