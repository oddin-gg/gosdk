package protocols

// OddsDisplayType ...
type OddsDisplayType int

// OddsDisplayTypes
const (
	DecimalOddsDisplayType  OddsDisplayType = 1
	AmericanOddsDisplayType OddsDisplayType = 2
)

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
}
