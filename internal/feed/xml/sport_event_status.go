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
	PointsPeriodScore
	RunPeriodsScore
}

// RoundsPeriodScore ...
type RoundsPeriodScore struct {
	HomeWonRounds *uint32 `xml:"home_won_rounds,attr,omitempty"`
	AwayWonRounds *uint32 `xml:"away_won_rounds,attr,omitempty"`
}

// PointsPeriodScore ...
type PointsPeriodScore struct {
	HomePoints *uint32 `xml:"home_points,attr,omitempty"`
	AwayPoints *uint32 `xml:"away_points,attr,omitempty"`
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

// RunPeriodsScore ...
type RunPeriodsScore struct {
	HomeRuns          *uint32 `xml:"home_runs,attr,omitempty"`
	AwayRuns          *uint32 `xml:"away_runs,attr,omitempty"`
	HomeWicketsFallen *uint32 `xml:"home_wickets_fallen,attr,omitempty"`
	AwayWicketsFallen *uint32 `xml:"away_wickets_fallen,attr,omitempty"`
	HomeOversPlayed   *uint32 `xml:"home_overs_played,attr,omitempty"`
	HomeBallsPlayed   *uint32 `xml:"home_balls_played,attr,omitempty"`
	AwayOversPlayed   *uint32 `xml:"away_overs_played,attr,omitempty"`
	AwayBallsPlayed   *uint32 `xml:"away_balls_played,attr,omitempty"`
	HomeWonCoinToss   *bool   `xml:"home_won_coin_toss,attr,omitempty"`
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

	// VirtualBasketballScoreboard
	HomePoints        *uint32 `xml:"home_points,attr,omitempty"`
	AwayPoints        *uint32 `xml:"away_points,attr,omitempty"`
	RemainingGameTime *uint32 `xml:"remaining_game_time,attr,omitempty"`

	// RushCricketScoreboard
	HomeRuns          *uint32 `xml:"home_runs,attr,omitempty"`
	AwayRuns          *uint32 `xml:"away_runs,attr,omitempty"`
	HomeWicketsFallen *uint32 `xml:"home_wickets_fallen,attr,omitempty"`
	AwayWicketsFallen *uint32 `xml:"away_wickets_fallen,attr,omitempty"`
	HomeOversPlayed   *uint32 `xml:"home_overs_played,attr,omitempty"`
	HomeBallsPlayed   *uint32 `xml:"home_balls_played,attr,omitempty"`
	AwayOversPlayed   *uint32 `xml:"away_overs_played,attr,omitempty"`
	AwayBallsPlayed   *uint32 `xml:"away_balls_played,attr,omitempty"`
	HomeWonCoinToss   *bool   `xml:"home_won_coin_toss,attr,omitempty"`
	HomeBatting       *bool   `xml:"home_batting,attr,omitempty"`
	AwayBatting       *bool   `xml:"away_batting,attr,omitempty"`
	Inning            *uint32 `xml:"inning,attr,omitempty"`

	// TableTennisScoreboard
	HomeGames *uint32 `xml:"home_games,attr,omitempty"`
	AwayGames *uint32 `xml:"away_games,attr,omitempty"`
}
