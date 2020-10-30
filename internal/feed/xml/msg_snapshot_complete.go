package xml

import (
	"encoding/xml"
	"time"
)

// SnapshotComplete ...
type SnapshotComplete struct {
	MessageWithTimestamp
	XMLName   xml.Name `xml:"snapshot_complete"`
	ProductID uint     `xml:"product,attr"`
	RequestID uint     `xml:"request_id,attr"`
}

// Product ...
func (s SnapshotComplete) Product() uint {
	return s.ProductID
}

// Timestamp ...
func (s SnapshotComplete) Timestamp() time.Time {
	return (time.Time)(s.MessageWithTimestamp.Timestamp)
}
