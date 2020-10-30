package protocols

import "net/url"

// ConnectionDownMessage ...
type ConnectionDownMessage interface {
	IsDown() bool
}

// ProducerStatusChangeMessage ...
type ProducerStatusChangeMessage interface {
	ProducerStatus() ProducerStatus
}

// EventRecoveryMessage ...
type EventRecoveryMessage interface {
	Message
	EventID() URN
	RequestID() uint
}

// RawAPIData ...
type RawAPIData interface {
	URL() *url.URL
	Data() interface{}
}
