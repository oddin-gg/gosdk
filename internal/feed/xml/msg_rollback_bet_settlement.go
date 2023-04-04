package xml

import (
	"encoding/xml"
	"time"
)

// RollbackBetSettlement ...
type RollbackBetSettlement struct {
	MessageAttributes
	XMLName xml.Name                      `xml:"rollback_bet_settlement"`
	Markets []RollbackBetSettlementMarket `xml:"market"`
}

type RollbackBetSettlementMarket struct {
	XMLName xml.Name `xml:"market"`
	MarketAttributes
}

// GetEventID ...
func (o RollbackBetSettlement) GetEventID() string {
	return o.MessageAttributes.EventID
}

// Product ...
func (o RollbackBetSettlement) Product() uint {
	return o.MessageAttributes.Product
}

// Timestamp ...
func (o RollbackBetSettlement) Timestamp() time.Time {
	return (time.Time)(o.MessageAttributes.MessageWithTimestamp.Timestamp)
}
