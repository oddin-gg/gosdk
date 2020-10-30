package xml

import (
	"encoding/xml"
	"github.com/oddin-gg/gosdk/protocols"
)

// MarketDescriptionResponse ...
type MarketDescriptionResponse struct {
	XMLName      xml.Name            `xml:"market_descriptions"`
	ResponseCode string              `xml:"response_code,attr"`
	Markets      []MarketDescription `xml:"market,omitempty"`
}

// Code ...
func (m MarketDescriptionResponse) Code() protocols.ResponseCode {
	return protocols.ResponseCode(m.ResponseCode)
}

// MarketDescription represents market type
type MarketDescription struct {
	ID         uint               `xml:"id,attr"`
	RefID      *uint              `xml:"ref_id,attr,omitempty"`
	Name       string             `xml:"name,attr"`
	Variant    *string            `xml:"variant,attr,omitempty"`
	Outcomes   *OutcomesWrapper   `xml:"outcomes"`
	Specifiers *SpecifiersWrapper `xml:"specifiers"`
}

// SpecifiersWrapper ...
type SpecifiersWrapper struct {
	Specifier []Specifier `xml:"specifier"`
}

// Specifier ...
type Specifier struct {
	Name        string `xml:"name,attr"`
	Type        string `xml:"type,attr"`
	Description string `xml:"description,attr,omitempty"`
}

// OutcomesWrapper ...
type OutcomesWrapper struct {
	Outcome []MarketDescriptionOutcome `xml:"outcome"`
}

// MarketDescriptionOutcome ...
type MarketDescriptionOutcome struct {
	ID          uint    `xml:"id,attr"`
	RefID       *uint   `xml:"ref_id,attr,omitempty"`
	Name        string  `xml:"name,attr"`
	Description *string `xml:"description,attr,omitempty"`
}
