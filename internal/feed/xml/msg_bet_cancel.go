package xml

import (
	"encoding/xml"
	"time"
)

// BetCancel ...
type BetCancel struct {
	XMLName xml.Name `xml:"bet_cancel"`
	MessageAttributes
	StartTime *uint                   `xml:"start_time,attr,omitempty"`
	EndTime   *uint                   `xml:"end_time,attr,omitempty"`
	Markets   []*MarketWithoutOutcome `xml:"market"`
}

// Product ...
func (b BetCancel) Product() uint {
	return b.MessageAttributes.Product
}

// Timestamp ...
func (b BetCancel) Timestamp() time.Time {
	return (time.Time)(b.MessageWithTimestamp.Timestamp)
}
