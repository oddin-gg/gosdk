package xml

import (
	"encoding/xml"
	"time"
)

// Alive ...
type Alive struct {
	MessageWithTimestamp
	XMLName    xml.Name `xml:"alive"`
	ProductID  uint     `xml:"product,attr"`
	Subscribed uint     `xml:"subscribed,attr"`
}

// Product ...
func (a Alive) Product() uint {
	return a.ProductID
}

// Timestamp ...
func (a Alive) Timestamp() time.Time {
	return (time.Time)(a.MessageWithTimestamp.Timestamp)
}
