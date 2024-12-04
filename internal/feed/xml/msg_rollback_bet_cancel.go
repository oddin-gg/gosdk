package xml

import (
	"encoding/xml"
	"time"
)

// RollbackBetCancel ...
type RollbackBetCancel struct {
	MessageAttributes
	XMLName   xml.Name                  `xml:"rollback_bet_cancel"`
	StartTime *uint                     `xml:"start_time,attr,omitempty"`
	EndTime   *uint                     `xml:"end_time,attr,omitempty"`
	Markets   []RollbackBetCancelMarket `xml:"market"`
}

type RollbackBetCancelMarket struct {
	XMLName xml.Name `xml:"market"`
	MarketAttributes
}

// GetEventID ...
func (o RollbackBetCancel) GetEventID() string {
	return o.MessageAttributes.EventID
}

// Product ...
func (o RollbackBetCancel) Product() uint {
	return o.MessageAttributes.Product
}

// Timestamp ...
func (o RollbackBetCancel) Timestamp() time.Time {
	return (time.Time)(o.MessageAttributes.MessageWithTimestamp.Timestamp)
}
