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

// ExtraInfoWrapper ...
type ExtraInfoWrapper struct {
	List []ExtraInfo `xml:"info"`
}

// ExtraInfo ...
type ExtraInfo struct {
	Key   string `xml:"key,attr"`
	Value string `xml:"value,attr"`
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
