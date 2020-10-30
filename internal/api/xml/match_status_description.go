package xml

import (
	"encoding/xml"
	"github.com/oddin-gg/gosdk/protocols"
)

// MatchStatusDescriptionResponse ...
type MatchStatusDescriptionResponse struct {
	XMLName      xml.Name      `xml:"match_status_descriptions"`
	ResponseCode string        `xml:"response_code,attr"`
	MatchStatus  []MatchStatus `xml:"match_status,omitempty"`
}

// Code ...
func (m MatchStatusDescriptionResponse) Code() protocols.ResponseCode {
	return protocols.ResponseCode(m.ResponseCode)
}

// MatchStatus ...
type MatchStatus struct {
	ID          uint    `xml:"id,attr"`
	Description *string `xml:"description,attr"`
}

// GetID ...
func (m MatchStatus) GetID() uint {
	return m.ID
}

// GetDescription ...
func (m MatchStatus) GetDescription() *string {
	return m.Description
}
