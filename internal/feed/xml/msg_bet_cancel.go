package xml

import (
	"encoding/xml"
	"time"
)

// BetCancel ...
type BetCancel struct {
	XMLName xml.Name `xml:"bet_cancel"`
	MessageAttributes
	StartTime *int64                  `xml:"start_time,attr,omitempty"`
	EndTime   *int64                  `xml:"end_time,attr,omitempty"`
	Markets   []*MarketWithoutOutcome `xml:"market"`
}

// GetEventID returns the event id the payload itself carries — the
// route/payload identity cross-check and feed-driven cache invalidation
// both key on this accessor; its absence silently exempted bet_cancel
// from both (Codex P2).
func (b BetCancel) GetEventID() string {
	return b.MessageAttributes.EventID
}

// Product ...
func (b BetCancel) Product() int {
	return b.MessageAttributes.Product
}

// Timestamp ...
func (b BetCancel) Timestamp() time.Time {
	return (time.Time)(b.MessageWithTimestamp.Timestamp)
}
