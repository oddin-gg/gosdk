package xml

import (
	"encoding/xml"
	"time"
)

// SnapshotComplete ...
type SnapshotComplete struct {
	MessageWithTimestamp
	XMLName   xml.Name `xml:"snapshot_complete"`
	ProductID int      `xml:"product,attr"`
	RequestID int      `xml:"request_id,attr"`
}

// Product ...
func (s SnapshotComplete) Product() int {
	return s.ProductID
}

// Timestamp ...
func (s SnapshotComplete) Timestamp() time.Time {
	return (time.Time)(s.MessageWithTimestamp.Timestamp)
}
