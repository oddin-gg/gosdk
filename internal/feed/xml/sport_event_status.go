package xml

// PeriodScore ...
type PeriodScore struct {
	Type            string  `xml:"type,attr"`
	Number          int     `xml:"number,attr"`
	MatchStatusCode int     `xml:"match_status_code,attr"`
	HomeScore       float64 `xml:"home_score,attr"`
	AwayScore       float64 `xml:"away_score,attr"`
	RoundsPeriodScore
	KillsPeriodsScore
	GoalsPeriodsScore
	PointsPeriodScore
	GamesPeriodScore
	RunPeriodsScore
}

// RoundsPeriodScore ...
type RoundsPeriodScore struct {
	HomeWonRounds *int `xml:"home_won_rounds,attr,omitempty"`
	AwayWonRounds *int `xml:"away_won_rounds,attr,omitempty"`
}

// PointsPeriodScore ...
type PointsPeriodScore struct {
	HomePoints *int `xml:"home_points,attr,omitempty"`
	AwayPoints *int `xml:"away_points,attr,omitempty"`
}

// GamesPeriodScore carries the games won inside one period. Set-based classic
// sports (tennis, volleyball) report them per set, alongside the period's
// home_score/away_score, which is the running sets-won tally.
type GamesPeriodScore struct {
	HomeGames *int `xml:"home_games,attr,omitempty"`
	AwayGames *int `xml:"away_games,attr,omitempty"`
}

// KillsPeriodsScore ...
type KillsPeriodsScore struct {
	HomeKills *int32 `xml:"home_kills,attr,omitempty"`
	AwayKills *int32 `xml:"away_kills,attr,omitempty"`
}

// GoalsPeriodsScore ...
type GoalsPeriodsScore struct {
	HomeGoals *int `xml:"home_goals,attr,omitempty"`
	AwayGoals *int `xml:"away_goals,attr,omitempty"`
}

// RunPeriodsScore ...
type RunPeriodsScore struct {
	HomeRuns          *int  `xml:"home_runs,attr,omitempty"`
	AwayRuns          *int  `xml:"away_runs,attr,omitempty"`
	HomeWicketsFallen *int  `xml:"home_wickets_fallen,attr,omitempty"`
	AwayWicketsFallen *int  `xml:"away_wickets_fallen,attr,omitempty"`
	HomeOversPlayed   *int  `xml:"home_overs_played,attr,omitempty"`
	HomeBallsPlayed   *int  `xml:"home_balls_played,attr,omitempty"`
	AwayOversPlayed   *int  `xml:"away_overs_played,attr,omitempty"`
	AwayBallsPlayed   *int  `xml:"away_balls_played,attr,omitempty"`
	HomeWonCoinToss   *bool `xml:"home_won_coin_toss,attr,omitempty"`
}

// SportEventStatus ...
// SportEventStatus — scalar attributes are POINTERS so absence is
// distinguishable from zero: plain fields decoded missing attributes as
// 0/false, and partial live updates then ERASED real scores / status
// codes (and were exposed publicly as Some(0)). The cache merge
// overwrites only fields the payload actually supplied.
type SportEventStatus struct {
	WinnerID            *string       `xml:"winner_id,attr,omitempty"`
	HomeScore           *float64      `xml:"home_score,attr,omitempty"`
	AwayScore           *float64      `xml:"away_score,attr,omitempty"`
	Status              *int          `xml:"status,attr,omitempty"`
	MatchStatus         *int          `xml:"match_status,attr,omitempty"`
	PeriodScores        *PeriodScores `xml:"period_scores,omitempty"`
	ScoreboardAvailable *bool         `xml:"scoreboard_available,attr,omitempty"`
	Scoreboard          *Scoreboard   `xml:"scoreboard,omitempty"`
	Statistics          *Statistics   `xml:"statistics,omitempty"`
}

// PeriodScores ...
type PeriodScores struct {
	List []*PeriodScore `xml:"period_score"`
}

// Scoreboard ...
type Scoreboard struct {
	CurrentCTTeam        *int   `xml:"current_ct_team,attr,omitempty"`
	HomeWonRounds        *int   `xml:"home_won_rounds,attr,omitempty"`
	AwayWonRounds        *int   `xml:"away_won_rounds,attr,omitempty"`
	CurrentRound         *int   `xml:"current_round,attr,omitempty"`
	HomeKills            *int32 `xml:"home_kills,attr,omitempty"`
	AwayKills            *int32 `xml:"away_kills,attr,omitempty"`
	HomeDestroyedTurrets *int32 `xml:"home_destroyed_turrets,attr,omitempty"`
	AwayDestroyedTurrets *int32 `xml:"away_destroyed_turrets,attr,omitempty"`
	HomeDestroyedTowers  *int32 `xml:"home_destroyed_towers,attr,omitempty"`
	AwayDestroyedTowers  *int32 `xml:"away_destroyed_towers,attr,omitempty"`
	HomeGold             *int   `xml:"home_gold,attr,omitempty"`
	AwayGold             *int   `xml:"away_gold,attr,omitempty"`
	HomeGoals            *int   `xml:"home_goals,attr,omitempty"`
	AwayGoals            *int   `xml:"away_goals,attr,omitempty"`
	Time                 *int   `xml:"time,attr,omitempty"`
	GameTime             *int   `xml:"game_time,attr,omitempty"`
	ElapsedTime          *int   `xml:"elapsed_time,attr,omitempty"`
	CurrentDefenderTeam  *int   `xml:"current_def_team,attr,omitempty"`

	// VirtualBasketballScoreboard
	HomePoints        *int `xml:"home_points,attr,omitempty"`
	AwayPoints        *int `xml:"away_points,attr,omitempty"`
	RemainingGameTime *int `xml:"remaining_game_time,attr,omitempty"`

	// RushCricketScoreboard
	HomeRuns          *int  `xml:"home_runs,attr,omitempty"`
	AwayRuns          *int  `xml:"away_runs,attr,omitempty"`
	HomeWicketsFallen *int  `xml:"home_wickets_fallen,attr,omitempty"`
	AwayWicketsFallen *int  `xml:"away_wickets_fallen,attr,omitempty"`
	HomeOversPlayed   *int  `xml:"home_overs_played,attr,omitempty"`
	HomeBallsPlayed   *int  `xml:"home_balls_played,attr,omitempty"`
	AwayOversPlayed   *int  `xml:"away_overs_played,attr,omitempty"`
	AwayBallsPlayed   *int  `xml:"away_balls_played,attr,omitempty"`
	HomeWonCoinToss   *bool `xml:"home_won_coin_toss,attr,omitempty"`
	HomeBatting       *bool `xml:"home_batting,attr,omitempty"`
	AwayBatting       *bool `xml:"away_batting,attr,omitempty"`
	Inning            *int  `xml:"inning,attr,omitempty"`

	// TableTennisScoreboard
	HomeGames *int `xml:"home_games,attr,omitempty"`
	AwayGames *int `xml:"away_games,attr,omitempty"`
}
