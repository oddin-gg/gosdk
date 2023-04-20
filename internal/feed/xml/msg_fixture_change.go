package xml

import (
	"encoding/xml"
	"time"
)

// FixtureChangeType ...
type FixtureChangeType uint

// List of FixtureChangeType
const (
	FixtureChangeTypeUnknown   FixtureChangeType = 0
	FixtureChangeTypeNew       FixtureChangeType = 1
	FixtureChangeTypeDateTime  FixtureChangeType = 2
	FixtureChangeTypeCancelled FixtureChangeType = 3
	FixtureChangeTypeCoverage  FixtureChangeType = 5
	FixtureChangeTypeStreamURL FixtureChangeType = 106
)

// FixtureChange ...
type FixtureChange struct {
	MessageWithTimestamp
	XMLName xml.Name `xml:"fixture_change"`
	EventID string   `xml:"event_id,attr"`
	// Deprecated: do not use this property, it will be removed in future
	EventRefID *string           `xml:"event_ref_id,attr,omitempty"`
	ProductID  uint              `xml:"product,attr"`
	ChangeType FixtureChangeType `xml:"change_type,attr,omitempty"`
	RequestID  *uint             `xml:"request_id,attr,omitempty"`
}

// GetEventID ...
func (f FixtureChange) GetEventID() string {
	return f.EventID
}

// Product ...
func (f FixtureChange) Product() uint {
	return f.ProductID
}

// Timestamp ...
func (f FixtureChange) Timestamp() time.Time {
	return (time.Time)(f.MessageWithTimestamp.Timestamp)
}
