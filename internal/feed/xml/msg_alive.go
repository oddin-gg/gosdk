package xml

import (
	"encoding/xml"
	"time"
)

// Alive ...
//
// Subscribed is a POINTER so an ABSENT attribute is distinguishable from
// an explicit subscribed="0": a missing attribute silently decoding to
// 0 was interpreted as "not subscribed" and could mark the producer down
// / alter recovery state (Codex P2). Validation rejects a nil (absent)
// or out-of-range value before any state change.
type Alive struct {
	MessageWithTimestamp
	XMLName    xml.Name `xml:"alive"`
	ProductID  int      `xml:"product,attr"`
	Subscribed *int     `xml:"subscribed,attr"`
}

// Product ...
func (a Alive) Product() int {
	return a.ProductID
}

// Timestamp ...
func (a Alive) Timestamp() time.Time {
	return (time.Time)(a.MessageWithTimestamp.Timestamp)
}
