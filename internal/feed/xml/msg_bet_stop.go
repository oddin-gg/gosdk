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

// Product ...
func (b BetStop) Product() uint {
	return b.MessageAttributes.Product
}

// Timestamp ...
func (b BetStop) Timestamp() time.Time {
	return (time.Time)(b.MessageWithTimestamp.Timestamp)
}
