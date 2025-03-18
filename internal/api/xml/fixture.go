package xml

import (
	"encoding/xml"

	"github.com/oddin-gg/gosdk/internal/utils"
)

const (
	ExtraInfoSportFormatKey = "sport_format"
)

// FixtureResponse ...
type FixtureResponse struct {
	XMLName     xml.Name       `xml:"fixtures_fixture"`
	GeneratedAt utils.DateTime `xml:"generated_at,attr"`
	Fixture     Fixture        `xml:"fixture"`
}

// Fixture ...
type Fixture struct {
	SportEvent
	StartTime  *utils.DateTime    `xml:"start_time,attr,omitempty"`
	TVChannels *TvChannelsWrapper `xml:"tv_channels,omitempty"`
}

// TvChannelsWrapper ...
type TvChannelsWrapper struct {
	List []TVChannel `xml:"tv_channel"`
}

// TVChannel ...
type TVChannel struct {
	Name      string         `xml:"name,attr"`
	Language  string         `xml:"language,attr"`
	StartTime utils.DateTime `xml:"start_time,attr,omitempty"`
	StreamURL string         `xml:"stream_url,attr,omitempty"`
}
