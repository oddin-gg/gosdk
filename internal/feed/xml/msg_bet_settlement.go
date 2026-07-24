package xml

import (
	"encoding/xml"
	"time"
)

// BetSettlement ...
type BetSettlement struct {
	MessageWithTimestamp
	XMLName    xml.Name       `xml:"bet_settlement"`
	EventID    string         `xml:"event_id,attr"`
	EventRefID *string        `xml:"event_ref_id,attr,omitempty"`
	ProductID  int            `xml:"product,attr"`
	Markets    MarketsWrapper `xml:"outcomes"`
	RequestID  *int           `xml:"request_id,attr,omitempty"`
}

// GetEventID returns the event id the payload itself carries — the
// route/payload identity cross-check and feed-driven cache invalidation
// both key on this accessor; its absence silently exempted
// bet_settlement from both (Codex P2).
func (b BetSettlement) GetEventID() string {
	return b.EventID
}

// Product ...
func (b BetSettlement) Product() int {
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
