package xml

import (
	"encoding/xml"
	"github.com/oddin-gg/gosdk/internal/utils"
	"time"
)

// SportTournamentInfoResponse ...
type SportTournamentInfoResponse struct {
	XMLName     xml.Name            `xml:"tournament_info"`
	GeneratedAt utils.DateTime      `xml:"generated_at,attr"`
	Tournament  TournamentExtended  `xml:"tournament"`
	Competitors *CompetitorsWrapper `xml:"competitors,omitempty"`
}

// SportTournamentsResponse ...
type SportTournamentsResponse struct {
	XMLName     xml.Name       `xml:"sport_tournaments"`
	GeneratedAt utils.DateTime `xml:"generated_at,attr"`
	Sport       Sport          `xml:"sport"`
	Tournaments *Tournaments   `xml:"tournaments,omitempty"`
}

// Tournaments ...
type Tournaments struct {
	Tournament []Tournament `xml:"tournament,omitempty"`
}

// TournamentsResponse ...
type TournamentsResponse struct {
	XMLName     xml.Name             `xml:"tournaments"`
	GeneratedAt utils.DateTime       `xml:"generated_at,attr"`
	Tournaments []TournamentExtended `xml:"tournament"`
}

// Tournament ...
type Tournament struct {
	XMLName          xml.Name           `xml:"tournament"`
	ID               string             `xml:"id,attr"`
	RefID            *string            `xml:"ref_id,attr,omitempty"`
	Name             string             `xml:"name,attr"`
	Scheduled        *utils.DateTime    `xml:"scheduled,attr,omitempty"`
	ScheduledEnd     *utils.DateTime    `xml:"scheduled_end,attr,omitempty"`
	TournamentLength []TournamentLength `xml:"tournament_length,omitempty"`
	Sport            Sport              `xml:"sport"`
	Abbreviation     string             `xml:"abbreviation,attr"`
}

// GetName ...
func (t Tournament) GetName() string {
	return t.Name
}

// GetAbbreviation ...
func (t Tournament) GetAbbreviation() string {
	return t.Abbreviation
}

// GetID ...
func (t Tournament) GetID() string {
	return t.ID
}

// GetRefID ...
func (t Tournament) GetRefID() *string {
	return t.RefID
}

// GetSportID ...
func (t Tournament) GetSportID() string {
	return t.Sport.ID
}

// GetScheduledTime ...
func (t Tournament) GetScheduledTime() *time.Time {
	return (*time.Time)(t.ScheduledEnd)
}

// GetScheduledEndTime ...
func (t Tournament) GetScheduledEndTime() *time.Time {
	return (*time.Time)(t.ScheduledEnd)
}

// GetStartDate ...
func (t Tournament) GetStartDate() *time.Time {
	if len(t.TournamentLength) == 0 {
		return nil
	}

	return (*time.Time)(&t.TournamentLength[0].StartDate)
}

// GetEndDate ...
func (t Tournament) GetEndDate() *time.Time {
	if len(t.TournamentLength) == 0 {
		return nil
	}

	return (*time.Time)(&t.TournamentLength[0].EndDate)
}

// TournamentLength ...
type TournamentLength struct {
	StartDate utils.Date `xml:"start_date,attr,omitempty"`
	EndDate   utils.Date `xml:"end_date,attr,omitempty"`
}

// Category ...
type Category struct {
	ID          string `xml:"id,attr"`
	Name        string `xml:"name,attr"`
	CountryCode string `xml:"country_code,attr,omitempty"`
}

// TournamentExtended ...
type TournamentExtended struct {
	Tournament
	Competitors *CompetitorsWrapper `xml:"competitors,omitempty"`
	IconPath    *string             `xml:"icon_path,attr,omitempty"`
}

// GetCompetitors ...
func (t TournamentExtended) GetCompetitors() []Team {
	if t.Competitors == nil {
		return []Team{}
	}

	return t.Competitors.Competitor
}

// CompetitorsWrapper ...
type CompetitorsWrapper struct {
	Competitor []Team `xml:"competitor,omitempty"`
}

// TournamentResponse ...
type TournamentResponse struct {
	XMLName     xml.Name            `xml:"tournament_info"`
	GeneratedAt utils.DateTime      `xml:"generated_at,attr"`
	Tournament  TournamentExtended  `xml:"tournament"`
	Competitors *CompetitorsWrapper `xml:"competitors,omitempty"`
}

// TournamentScheduleResponse ...
type TournamentScheduleResponse struct {
	XMLName     xml.Name           `xml:"tournament_schedule"`
	GeneratedAt utils.DateTime     `xml:"generated_at,attr"`
	Tournament  TournamentExtended `xml:"tournament"`
	SportEvents Events             `xml:"sport_events"`
}

// Events ...
type Events struct {
	List []SportEvent `xml:"sport_event"`
}
