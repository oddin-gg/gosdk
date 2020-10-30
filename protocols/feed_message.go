package protocols

// BasicFeedMessage ...
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

// QueueMessage ...
type QueueMessage struct {
	RawFeedMessage    *RawFeedMessage
	FeedMessage       *FeedMessage
	UnparsableMessage UnparsableMessage
}

// SessionMessage ...
type SessionMessage struct {
	RawFeedMessage    *RawFeedMessage
	Message           interface{}
	UnparsableMessage UnparsableMessage
}
