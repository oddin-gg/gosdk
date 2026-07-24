package xml

import (
	"encoding/xml"
)

// MarketAttributes ...
type MarketAttributes struct {
	ID                 int     `xml:"id,attr"`
	RefID              *int    `xml:"ref_id,attr,omitempty"`
	Specifiers         *string `xml:"specifiers,attr,omitempty"`
	ExtendedSpecifiers *string `xml:"extended_specifiers,attr,omitempty"`
}

// MarketWithoutOutcome ...
//
// VoidReasonID + VoidReasonParams are the modern void-metadata fields
// used by public consumers. The legacy `void_reason` int attribute is
// deliberately not decoded: javasdk marks it @Deprecated (see
// MarketCancel/MarketWithSettlement in javasdk's Market.kt — Java's
// MarketWithSettlementImpl returns null for the deprecated accessor),
// origin/main Go never exposed it, and the modern fields cover the
// same data more precisely.
type MarketWithoutOutcome struct {
	MarketAttributes
	VoidReasonID     *int    `xml:"void_reason_id,attr,omitempty"`
	VoidReasonParams *string `xml:"void_reason_params,attr,omitempty"`
}

// MarketWithOutcome ...
//
// VoidReason* attributes are present on bet_settlement markets in
// addition to bet_cancel markets (the wire schema carries them on
// both — see netcoresdk's betSettlementMarket and bet_cancel_market
// XML types). Pre-fix bet_settlement decoded only the outcomes; the
// public types.MarketWithSettlement therefore had no void-metadata
// surface — a parity gap vs .NET. Now decoded and surfaced via
// VoidReasonID + VoidReasonParams on types.MarketWithSettlement.
// The legacy single-int void_reason attribute is intentionally not
// decoded (deprecated in Java/.NET, never exposed in origin/main Go).
type MarketWithOutcome struct {
	XMLName xml.Name `xml:"market"`
	MarketAttributes
	Favourite        *bool         `xml:"favourite,attr,omitempty"`
	Status           *MarketStatus `xml:"status,attr,omitempty"`
	VoidReasonID     *int          `xml:"void_reason_id,attr,omitempty"`
	VoidReasonParams *string       `xml:"void_reason_params,attr,omitempty"`
	Outcomes         []Outcome     `xml:"outcome"`
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
	ID      string   `xml:"id,attr"`
	RefID   *int     `xml:"ref_id,attr,omitempty"`
	// Odds change outcome fields
	Odds          *float32 `xml:"odds,attr,omitempty"`
	Probabilities *float32 `xml:"probabilities,attr,omitempty"`
	Active        *int     `xml:"active,attr"`
	// Settlement outcome fields
	Result     *OutcomeResult `xml:"result,attr,omitempty"`
	VoidFactor *float32       `xml:"void_factor,attr,omitempty"`
}
