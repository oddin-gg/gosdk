package xml

import "encoding/xml"

// ReplayEvent ...
type ReplayEvent struct {
	ID string `xml:"id,attr"`
	// Deprecated: do not use this property, it will be removed in future
	RefID    string `xml:"ref_id,attr,omitempty"`
	Position string `xml:"position,attr"`
}

// ReplayResponse ...
type ReplayResponse struct {
	XMLName     xml.Name      `xml:"replay_set_content"`
	SportEvents []ReplayEvent `xml:"replay_event"`
}
