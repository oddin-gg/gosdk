package xml

import (
	"encoding/xml"

	"github.com/oddin-gg/gosdk/internal/utils"
)

// FixtureChangesResponse ...
type FixtureChangesResponse struct {
	XMLName     xml.Name        `xml:"fixture_changes"`
	GeneratedAt utils.DateTime  `xml:"generated_at,attr"`
	Changes     []FixtureChange `xml:"fixture_change,omitempty"`
}

// FixtureChange ...
type FixtureChange struct {
	SportEventID string `xml:"sport_event_id,attr"`
	// Deprecated: do not use this property, it will be removed in future
	SportEventRefID *string        `xml:"sport_event_ref_id,attr,omitempty"`
	UpdatedAt       utils.DateTime `xml:"update_time,attr"`
}
