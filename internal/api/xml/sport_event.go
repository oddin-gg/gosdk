package xml

import (
	"github.com/oddin-gg/gosdk/internal/utils"
)

// SportEvent represents our Match
type SportEvent struct {
	ID           string               `xml:"id,attr"`
	RefID        *string              `xml:"ref_id,attr,omitempty"`
	Name         string               `xml:"name,attr"`
	Scheduled    *utils.DateTime      `xml:"scheduled,attr,omitempty"`
	ScheduledEnd *utils.DateTime      `xml:"scheduled_end,attr,omitempty"`
	LiveOdds     LiveOdds             `xml:"liveodds,attr,omitempty"`
	Status       SportEventStatusType `xml:"status,attr,omitempty"`
	// Elements
	Tournament   Tournament             `xml:"tournament,omitempty"`
	Competitors  *SportEventCompetitors `xml:"competitors,omitempty"`
	ReferenceIDs *ReferenceIDs          `xml:"reference_ids,omitempty"`
	ExtraInfo    *ExtraInfoWrapper      `xml:"extra_info,omitempty"`
}

// ReferenceIDs is the XML wrapper for an event's reference-id list,
// surfaced on the public Match as a name→value map. Forward-ported
// from main commit fcc3c0d (PR #38).
type ReferenceIDs struct {
	ReferenceID []ReferenceID `xml:"reference_id"`
}

// ReferenceID is a single (name, value) entry inside ReferenceIDs.
type ReferenceID struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

// LiveOdds ...
type LiveOdds string

// List of LiveOdds
const (
	LiveOddsNotAvailable LiveOdds = "not_available"
	LiveOddsBooked       LiveOdds = "booked"
	LiveOddsBookable     LiveOdds = "bookable"
	LiveOddsBuyable      LiveOdds = "buyable"
)

// ExtraInfoWrapper ...
type ExtraInfoWrapper struct {
	List []ExtraInfo `xml:"info"`
}

// ExtraInfo ...
type ExtraInfo struct {
	Key   string `xml:"key,attr"`
	Value string `xml:"value,attr"`
}

// SportEventStatusType ...
type SportEventStatusType string

// List of SportEventStatusType
const (
	SportEventStatusTypeNotStarted  SportEventStatusType = "not_started"
	SportEventStatusTypeLive        SportEventStatusType = "live"
	SportEventStatusTypeEnded       SportEventStatusType = "ended"
	SportEventStatusTypeClosed      SportEventStatusType = "closed"
	SportEventStatusTypeCancelled   SportEventStatusType = "cancelled"
	SportEventStatusTypeDelayed     SportEventStatusType = "delayed"
	SportEventStatusTypeInterrupted SportEventStatusType = "interrupted"
	SportEventStatusTypeSuspended   SportEventStatusType = "suspended"
	SportEventStatusTypePostponed   SportEventStatusType = "postponed"
	SportEventStatusTypeAbandoned   SportEventStatusType = "abandoned"
)

// SportEventCompetitors ...
type SportEventCompetitors struct {
	Competitor []TeamCompetitor `xml:"competitor,omitempty"`
}
