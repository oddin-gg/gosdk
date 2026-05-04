package types

import "math"

// OddsDisplayType ...
type OddsDisplayType int

// OddsDisplayTypes
const (
	DecimalOddsDisplayType  OddsDisplayType = 1
	AmericanOddsDisplayType OddsDisplayType = 2
)

// VoidFactor ...
type VoidFactor float64

const (
	VoidFactorRefundHalf VoidFactor = 0.5
	VoidFactorRefundFull VoidFactor = 1.0
)

// String representation of the type
func (v VoidFactor) String() string {
	switch v {
	case VoidFactorRefundFull:
		return "REFUND_FULL"
	case VoidFactorRefundHalf:
		return "REFUND_HALF"
	default:
		return ""
	}
}

// Outcome is the base outcome shape carried inside markets.
//
// Phase 6.1 reshape: replaces the previous Outcome/OutcomeProbabilities
// interfaces with a value struct. Name is resolved at message-construction
// time in the SDK's default locale (callers needing a different locale go
// through Client.MarketDescription).
type Outcome struct {
	ID   string
	Name string
}

// OutcomeOdds is an outcome carrying live odds.
type OutcomeOdds struct {
	Outcome
	IsActive    bool
	Probability *float32
	// DecimalOdds is the raw decimal-format odds value (nil when missing).
	DecimalOdds *float32
}

// Odds returns the odds in the requested display type, computed from
// DecimalOdds. Result is nil when no odds are reported.
func (o OutcomeOdds) Odds(displayType OddsDisplayType) *float32 {
	switch displayType {
	case AmericanOddsDisplayType:
		return convertToAmericanOdds(o.DecimalOdds)
	default:
		return o.DecimalOdds
	}
}

func convertToAmericanOdds(odds *float32) *float32 {
	if odds == nil || math.IsNaN(float64(*odds)) {
		return odds
	}
	switch {
	case *odds == 1.0:
		return nil
	case *odds >= 2.0:
		result := *odds - 100.0
		return &result
	default:
		result := -100 / (*odds - 1)
		return &result
	}
}

// OutcomeResult ...
type OutcomeResult int

// OutcomeResults
const (
	LostOutcomeResult         OutcomeResult = 1
	WonOutcomeResult          OutcomeResult = 2
	UndecidedYetOutcomeResult OutcomeResult = 3
	UnknownOutcomeResult      OutcomeResult = 0
)

// OutcomeSettlement is an outcome carrying its settlement result.
type OutcomeSettlement struct {
	Outcome
	OutcomeResult OutcomeResult
	VoidFactor    *VoidFactor
}
