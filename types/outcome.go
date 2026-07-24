package types

import "math"

// OddsDisplayType ...
type OddsDisplayType int

// OddsDisplayTypes
const (
	DecimalOddsDisplayType  OddsDisplayType = 1
	AmericanOddsDisplayType OddsDisplayType = 2
)

// VoidFactor describes the proportional refund applied when a market
// or outcome is voided after settlement.
type VoidFactor float64

// VoidFactor values carried on settlement messages.
const (
	// VoidFactorRefundHalf refunds 50% of the stake.
	VoidFactorRefundHalf VoidFactor = 0.5
	// VoidFactorRefundFull refunds 100% of the stake.
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
// interfaces with a value struct. Names is populated at message-
// construction time for every locale in `WithPreloadLocales(...)`
// (and the default locale).
//
// v2.x reshape: parallel accessors `LocalizedName(locale) *string` (nil
// on miss) and `Name(locale) string` (empty on miss) collapsed into a
// single `Name(locale) Optional[string]`. `.ValueOr("")` keeps the
// always-string ergonomics; `.Get()` detects "not preloaded".
type Outcome struct {
	ID    string
	Names map[Locale]string
}

// Name returns the outcome name for locale, or None when the locale
// wasn't preloaded at message-decode time.
func (o Outcome) Name(locale Locale) Optional[string] {
	if v, ok := o.Names[locale]; ok {
		return Some(v)
	}
	return None[string]()
}

// OutcomeOdds is an outcome carrying live odds.
//
// v2.27 reshape: Probability and DecimalOdds migrated from *float32
// to Optional[float32] for value semantics + alias-free snapshots.
// See types.Optional for full rationale.
type OutcomeOdds struct {
	Outcome
	IsActive bool
	// Probability is the implied probability of the outcome
	// (None when not reported by the feed).
	Probability Optional[float32]
	// DecimalOdds is the raw decimal-format odds value
	// (None when not reported).
	DecimalOdds Optional[float32]
}

// Odds returns the odds in the requested display type, computed from
// DecimalOdds. Result is None when no odds are reported, and also
// when the American conversion would produce a non-meaningful value
// (decimal == 1.0).
func (o OutcomeOdds) Odds(displayType OddsDisplayType) Optional[float32] {
	switch displayType {
	case AmericanOddsDisplayType:
		return convertToAmericanOdds(o.DecimalOdds)
	default:
		return o.DecimalOdds
	}
}

// convertToAmericanOdds converts decimal odds to American (moneyline)
// format. Values with no meaningful moneyline yield None: NaN and ±Inf,
// zero/negative odds, and everything <= 1.0 (at or below break-even —
// the formulas would divide by zero or fabricate a sign). decimal >=
// 2.0 → (decimal-1)*100 (positive moneyline); 1.0 < decimal < 2.0 →
// -100/(decimal-1) (negative moneyline).
//
// The pre-v2.32 implementation returned `decimal - 100` on the >=2.0
// branch — a transcription bug that made decimal 3.0 yield -97
// instead of +200. The bug was locked in by a test that asserted the
// wrong formula. Fixed to match the .NET SDK and the standard
// definition (https://en.wikipedia.org/wiki/Odds#Moneyline_odds):
//
//	decimal 2.0 → +100   (1-to-1 payout, even money)
//	decimal 2.5 → +150
//	decimal 3.0 → +200
//	decimal 1.5 → -200   (must risk 200 to win 100)
func convertToAmericanOdds(odds Optional[float32]) Optional[float32] {
	v, ok := odds.Get()
	if !ok {
		return None[float32]()
	}
	// Domain guard: decimal odds are meaningful only for FINITE values
	// strictly above 1.0 (a 1.0 price has no payout; values in (0,1]
	// and 0 are malformed upstream data — pre-fix decimal 0 mapped to a
	// plausible-looking +100 and 0.5 to +200; infinities passed
	// through). Outside the domain: None.
	if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
		return None[float32]()
	}
	switch {
	case v <= 1.0:
		return None[float32]()
	case v >= 2.0:
		return Some((v - 1) * 100)
	default:
		return Some(-100 / (v - 1))
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
//
// v2.27 reshape: VoidFactor migrated from *VoidFactor to
// Optional[VoidFactor].
type OutcomeSettlement struct {
	Outcome
	OutcomeResult OutcomeResult
	VoidFactor    Optional[VoidFactor]
}
