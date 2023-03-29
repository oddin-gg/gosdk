package protocols

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

// Outcome ...
type Outcome interface {
	ID() uint
	RefID() *uint
	Name() (*string, error)
	LocalizedName(locale Locale) (*string, error)
}

// OutcomeProbabilities ...
type OutcomeProbabilities interface {
	Outcome
	IsActive() bool
	Probability() *float32
}

// OutcomeOdds ...
type OutcomeOdds interface {
	OutcomeProbabilities
	Odds(displayType OddsDisplayType) *float32
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

// OutcomeSettlement ...
type OutcomeSettlement interface {
	Outcome
	OutcomeResult() OutcomeResult
	VoidFactor() *VoidFactor
}
