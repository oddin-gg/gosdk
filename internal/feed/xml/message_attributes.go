package xml

import (
	"github.com/oddin-gg/gosdk/internal/utils"
)

// MessageWithTimestamp ...
type MessageWithTimestamp struct {
	Timestamp utils.Timestamp `xml:"timestamp,attr"`
}

// MessageAttributes ...
type MessageAttributes struct {
	MessageWithTimestamp
	Product uint   `xml:"product,attr"`
	EventID string `xml:"event_id,attr"`
	// Deprecated: do not use this property, it will be removed in future
	EventRefID *string `xml:"event_ref_id,attr,omitempty"`
	RequestID  *uint   `xml:"request_id,attr,omitempty"`
}
