package xml

import (
	"github.com/oddin-gg/gosdk/internal/utils"
)

// SportEvent represents our Match
type SportEvent struct {
	ID string `xml:"id,attr"`
	// Deprecated: do not use this property, it will be removed in future
	RefID        *string              `xml:"ref_id,attr,omitempty"`
	Name         string               `xml:"name,attr"`
	Scheduled    *utils.DateTime      `xml:"scheduled,attr,omitempty"`
	ScheduledEnd *utils.DateTime      `xml:"scheduled_end,attr,omitempty"`
	LiveOdds     LiveOdds             `xml:"liveodds,attr,omitempty"`
	Status       SportEventStatusType `xml:"status,attr,omitempty"`
	// Elements
	Tournament  Tournament             `xml:"tournament,omitempty"`
	Competitors *SportEventCompetitors `xml:"competitors,omitempty"`
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
