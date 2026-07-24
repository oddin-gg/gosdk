package xml

import "encoding/xml"

// ReplayEvent ...
type ReplayEvent struct {
	ID       string `xml:"id,attr"`
	RefID    string `xml:"ref_id,attr,omitempty"`
	Position string `xml:"position,attr"`
}

// ReplayResponse ...
type ReplayResponse struct {
	XMLName     xml.Name      `xml:"replay_set_content"`
	SportEvents []ReplayEvent `xml:"replay_event"`
}

// ReplayStatusResponse mirrors the .NET / Java GET /replay/status payload:
//
//	<player_status status="..."/>
//
// The status is an opaque string set by the replay engine (typically one
// of "playing", "stopped", "paused" — but treated as text for forward
// compatibility).
type ReplayStatusResponse struct {
	XMLName xml.Name `xml:"player_status"`
	Status  string   `xml:"status,attr"`
}
