package xml

import (
	"encoding/xml"
	"time"
)

// OddsChange ...
type OddsChange struct {
	MessageWithTimestamp
	XMLName xml.Name `xml:"odds_change"`
	EventID string   `xml:"event_id,attr"`
	// Deprecated: do not use this property, it will be removed in future
	EventRefID       *string           `xml:"event_ref_id,attr,omitempty"`
	ProductID        uint              `xml:"product,attr"`
	SportEventStatus *SportEventStatus `xml:"sport_event_status,omitempty"`
	Odds             Odds              `xml:"odds"`
	RequestID        *uint             `xml:"request_id,attr,omitempty"`
}

// GetEventID ...
func (o OddsChange) GetEventID() string {
	return o.EventID
}

// Product ...
func (o OddsChange) Product() uint {
	return o.ProductID
}

// Timestamp ...
func (o OddsChange) Timestamp() time.Time {
	return (time.Time)(o.MessageWithTimestamp.Timestamp)
}

// Odds ...
type Odds struct {
	Markets []*MarketWithOutcome `xml:"market"`
}
