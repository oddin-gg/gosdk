package xml

import (
	"encoding/xml"
	"time"
)

// BetStop ...
type BetStop struct {
	XMLName xml.Name `xml:"bet_stop"`
	MessageAttributes
	Groups string       `xml:"groups,attr,omitempty"`
	Status MarketStatus `xml:"market_status,attr,omitempty"`
}

// GetEventID returns the event id the payload itself carries — the
// route/payload identity cross-check and feed-driven cache invalidation
// both key on this accessor; its absence silently exempted bet_stop
// from both (Codex P2).
func (b BetStop) GetEventID() string {
	return b.MessageAttributes.EventID
}

// Product ...
func (b BetStop) Product() int {
	return b.MessageAttributes.Product
}

// Timestamp ...
func (b BetStop) Timestamp() time.Time {
	return (time.Time)(b.MessageWithTimestamp.Timestamp)
}
