package types

// UnderageStatus indicates whether a competitor or player is flagged
// as underage. Surfaced as Competitor.Underage and Player.Underage.
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
