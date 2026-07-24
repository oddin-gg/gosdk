package xml

import (
	"encoding/xml"
	"time"
)

// FixtureChangeType ...
type FixtureChangeType int

// List of FixtureChangeType
//
// Wire values match the upstream feed protocol; the gap at 4 is the
// FORMAT change-type (mapped to types.OtherChangeFixtureChangeType to
// match javasdk's `OFChangeType.FORMAT → OTHER_CHANGE` and netcoresdk's
// `4 => FixtureChangeType.FORMAT`). Pre-fix: 4 was missing from this
// list and the factory's mapper, so wire `change_type=4` arrived as
// FixtureChangeTypeUnknown — silent feature gap vs Java/.NET.
const (
	FixtureChangeTypeUnknown   FixtureChangeType = 0
	FixtureChangeTypeNew       FixtureChangeType = 1
	FixtureChangeTypeDateTime  FixtureChangeType = 2
	FixtureChangeTypeCancelled FixtureChangeType = 3
	FixtureChangeTypeFormat    FixtureChangeType = 4
	FixtureChangeTypeCoverage  FixtureChangeType = 5
	FixtureChangeTypeStreamURL FixtureChangeType = 106
)

// FixtureChange ...
type FixtureChange struct {
	MessageWithTimestamp
	XMLName    xml.Name          `xml:"fixture_change"`
	EventID    string            `xml:"event_id,attr"`
	EventRefID *string           `xml:"event_ref_id,attr,omitempty"`
	ProductID  int               `xml:"product,attr"`
	ChangeType FixtureChangeType `xml:"change_type,attr,omitempty"`
	RequestID  *int              `xml:"request_id,attr,omitempty"`
}

// GetEventID ...
func (f FixtureChange) GetEventID() string {
	return f.EventID
}

// Product ...
func (f FixtureChange) Product() int {
	return f.ProductID
}

// Timestamp ...
func (f FixtureChange) Timestamp() time.Time {
	return (time.Time)(f.MessageWithTimestamp.Timestamp)
}
