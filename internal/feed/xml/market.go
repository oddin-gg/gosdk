package xml

import (
	"encoding/xml"
)

// MarketAttributes ...
type MarketAttributes struct {
	ID                 uint    `xml:"id,attr"`
	RefID              *uint   `xml:"ref_id,attr,omitempty"`
	Specifiers         *string `xml:"specifiers,attr,omitempty"`
	ExtendedSpecifiers *string `xml:"extended_specifiers,attr,omitempty"`
}

// MarketWithoutOutcome ...
type MarketWithoutOutcome struct {
	MarketAttributes
	VoidReason       int     `xml:"void_reason,attr,omitempty"`
	VoidReasonID     *uint   `xml:"void_reason_id,attr,omitempty"`
	VoidReasonParams *string `xml:"void_reason_params,attr,omitempty"`
}

// MarketWithOutcome ...
type MarketWithOutcome struct {
	XMLName xml.Name `xml:"market"`
	MarketAttributes
	Favourite *bool         `xml:"favourite,attr,omitempty"`
	Status    *MarketStatus `xml:"status,attr,omitempty"`
	Outcomes  []Outcome     `xml:"outcome"`
}

// MarketStatus ...
type MarketStatus int

// List of MarketStatus
const (
	MarketStatusActive     MarketStatus = 1
	MarketStatusDeactived  MarketStatus = 0
	MarketStatusSuspended  MarketStatus = -1
	MarketStatusHandedOver MarketStatus = -2
	MarketStatusSettled    MarketStatus = -3
	MarketStatusCancelled  MarketStatus = -4
	MarketStatusDefault    MarketStatus = -50
)

// OutcomeResult ...
type OutcomeResult int

// List of OutcomeResult
const (
	OutcomeResultLost         OutcomeResult = 0
	OutcomeResultWon          OutcomeResult = 1
	OutcomeResultUndecidedYet OutcomeResult = -1
)

// Outcome ...
type Outcome struct {
	XMLName xml.Name `xml:"outcome"`
	ID      uint     `xml:"id,attr"`
	RefID   *uint    `xml:"ref_id,attr,omitempty"`
	// Odds change outcome fields
	Odds          *float32 `xml:"odds,attr,omitempty"`
	Probabilities *float32 `xml:"probabilities,attr,omitempty"`
	Active        *uint    `xml:"active,attr"`
	// Settlement outcome fields
	Result     *OutcomeResult `xml:"result,attr,omitempty"`
	VoidFactor *float32       `xml:"void_factor,attr,omitempty"`
}
