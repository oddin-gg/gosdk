package xml

import "encoding/xml"

// CompetitorResponse ...
type CompetitorResponse struct {
	XMLName    xml.Name     `xml:"competitor_profile"`
	Competitor TeamExtended `xml:"competitor"`
	Players    []Player     `xml:"players>player,omitempty"`
}

// GetID ...
func (cr CompetitorResponse) GetID() string {
	return cr.Competitor.ID
}

// GetName ...
func (cr CompetitorResponse) GetName() string {
	return cr.Competitor.Name
}

// GetAbbreviation ...
func (cr CompetitorResponse) GetAbbreviation() string {
	return cr.Competitor.Abbreviation
}

// GetRefID ...
// Deprecated: do not use this method, it will be removed in future
func (cr CompetitorResponse) GetRefID() *string {
	return cr.Competitor.RefID
}

func (cr CompetitorResponse) GetPlayers() []Player {
	return cr.Players
}

// Team ...
type Team struct {
	ID string `xml:"id,attr"`
	// Deprecated: do not use this property, it will be removed in future
	RefID        *string `xml:"ref_id,attr,omitempty"`
	Name         string  `xml:"name,attr"`
	Abbreviation string  `xml:"abbreviation,attr"`
	Country      string  `xml:"country,attr,omitempty"`
	CountryCode  string  `xml:"country_code,attr,omitempty"`
	Virtual      bool    `xml:"virtual,attr,omitempty"`
}

// GetID ...
func (t Team) GetID() string {
	return t.ID
}

// GetName ...
func (t Team) GetName() string {
	return t.Name
}

// GetAbbreviation ...
func (t Team) GetAbbreviation() string {
	return t.Abbreviation
}

// GetRefID ...
// Deprecated: do not use this method, it will be removed in future
func (t Team) GetRefID() *string {
	return t.RefID
}

// TeamExtended ...
type TeamExtended struct {
	Team
	Sports     []Sport    `xml:"sport,omitempty"`
	Categories []Category `xml:"category,omitempty"`
	IconPath   *string    `xml:"icon_path,attr,omitempty"`
}

// TeamCompetitor ...
type TeamCompetitor struct {
	Team
	Qualifier string `xml:"qualifier,attr,omitempty"`
}
