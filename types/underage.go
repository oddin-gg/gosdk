package types

// UnderageStatus indicates whether a competitor is flagged as
// involving underage participants. Surfaced as Competitor.Underage
// (Player carries no underage field — the upstream player profile does
// not report one).
type UnderageStatus int

// UnderageStatus values.
const (
	// UnderageUnknown means the bookmaker hasn't reported a status.
	UnderageUnknown UnderageStatus = -1
	// UnderageNo means the participant is confirmed to be of age.
	UnderageNo UnderageStatus = 0
	// UnderageYes means the participant is confirmed to be underage.
	UnderageYes UnderageStatus = 1
)
