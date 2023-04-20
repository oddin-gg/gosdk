package xml

import (
	"encoding/xml"
	"time"
)

// BetSettlement ...
type BetSettlement struct {
	MessageWithTimestamp
	XMLName xml.Name `xml:"bet_settlement"`
	EventID string   `xml:"event_id,attr"`
	// Deprecated: do not use this property, it will be removed in future
	EventRefID *string        `xml:"event_ref_id,attr,omitempty"`
	ProductID  uint           `xml:"product,attr"`
	Markets    MarketsWrapper `xml:"outcomes"`
	RequestID  *uint          `xml:"request_id,attr,omitempty"`
}

// Product ...
func (b BetSettlement) Product() uint {
	return b.ProductID
}

// Timestamp ...
func (b BetSettlement) Timestamp() time.Time {
	return (time.Time)(b.MessageWithTimestamp.Timestamp)
}

// MarketsWrapper ...
type MarketsWrapper struct {
	Markets []*MarketWithOutcome `xml:"market"`
}
