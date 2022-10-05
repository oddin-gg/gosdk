package xml

// PeriodScore ...
type PeriodScore struct {
	Type            string  `xml:"type,attr"`
	Number          uint    `xml:"number,attr"`
	MatchStatusCode uint    `xml:"match_status_code,attr"`
	HomeScore       float64 `xml:"home_score,attr"`
	AwayScore       float64 `xml:"away_score,attr"`
	RoundsPeriodScore
	KillsPeriodsScore
	GoalsPeriodsScore
}

// RoundsPeriodScore ...
type RoundsPeriodScore struct {
	HomeWonRounds *uint32 `xml:"home_won_rounds,attr,omitempty"`
	AwayWonRounds *uint32 `xml:"away_won_rounds,attr,omitempty"`
}

// KillsPeriodsScore ...
type KillsPeriodsScore struct {
	HomeKills *int32 `xml:"home_kills,attr,omitempty"`
	AwayKills *int32 `xml:"away_kills,attr,omitempty"`
}

// GoalsPeriodsScore ...
type GoalsPeriodsScore struct {
	HomeGoals *uint32 `xml:"home_goals,attr,omitempty"`
	AwayGoals *uint32 `xml:"away_goals,attr,omitempty"`
}

// SportEventStatus ...
type SportEventStatus struct {
	WinnerID            *string       `xml:"winner_id,attr,omitempty"`
	HomeScore           float64       `xml:"home_score,attr"`
	AwayScore           float64       `xml:"away_score,attr"`
	Status              int           `xml:"status,attr"`
	MatchStatus         uint          `xml:"match_status,attr"`
	PeriodScores        *PeriodScores `xml:"period_scores,omitempty"`
	ScoreboardAvailable bool          `xml:"scoreboard_available,attr"`
	Scoreboard          *Scoreboard   `xml:"scoreboard,omitempty"`
}

// PeriodScores ...
type PeriodScores struct {
	List []*PeriodScore `xml:"period_score"`
}

// Scoreboard ...
type Scoreboard struct {
	CurrentCTTeam        *uint32 `xml:"current_ct_team,attr,omitempty"`
	HomeWonRounds        *uint32 `xml:"home_won_rounds,attr,omitempty"`
	AwayWonRounds        *uint32 `xml:"away_won_rounds,attr,omitempty"`
	CurrentRound         *uint32 `xml:"current_round,attr,omitempty"`
	HomeKills            *int32  `xml:"home_kills,attr,omitempty"`
	AwayKills            *int32  `xml:"away_kills,attr,omitempty"`
	HomeDestroyedTurrets *int32  `xml:"home_destroyed_turrets,attr,omitempty"`
	AwayDestroyedTurrets *int32  `xml:"away_destroyed_turrets,attr,omitempty"`
	HomeDestroyedTowers  *int32  `xml:"home_destroyed_towers,attr,omitempty"`
	AwayDestroyedTowers  *int32  `xml:"away_destroyed_towers,attr,omitempty"`
	HomeGold             *uint32 `xml:"home_gold,attr,omitempty"`
	AwayGold             *uint32 `xml:"away_gold,attr,omitempty"`
	HomeGoals            *uint32 `xml:"home_goals,attr,omitempty"`
	AwayGoals            *uint32 `xml:"away_goals,attr,omitempty"`
	Time                 *uint32 `xml:"time,attr,omitempty"`
	GameTime             *uint32 `xml:"game_time,attr,omitempty"`
	CurrentDefenderTeam  *uint32 `xml:"current_def_team,attr,omitempty"`
}
