package xml

import "encoding/xml"

// CompetitorResponse ...
type CompetitorResponse struct {
	XMLName    xml.Name     `xml:"competitor_profile"`
	Competitor TeamExtended `xml:"competitor"`
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
