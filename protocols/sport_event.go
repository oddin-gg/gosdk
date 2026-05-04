package protocols

import (
	"time"
)

// SideType ...
type SideType int

// SideTypes
const (
	HomeSideType SideType = 1
	AwaySideType SideType = 2
)

// LiveOddsAvailability ...
type LiveOddsAvailability string

// LiveOddsAvailabilities
const (
	NotAvailableLiveOddsAvailability LiveOddsAvailability = "not_available"
	AvailableLiveOddsAvailability    LiveOddsAvailability = "available"
)

type SportFormat string

const (
	SportFormatUnknown = "unknown"
	SportFormatClassic = "classic"
	SportFormatRace    = "race"
)

// SportEvent ...
type SportEvent interface {
	ID() URN
	LocalizedName(locale Locale) (*string, error)
	SportID() (*URN, error)
	ScheduledTime() (*time.Time, error)
	ScheduledEndTime() (*time.Time, error)
	LiveOddsAvailability() (*LiveOddsAvailability, error)
}

// PeriodScore ...
type PeriodScore interface {
	Type() string
	HomeScore() float64
	AwayScore() float64
	PeriodNumber() uint
	MatchStatusCode() uint
	HomeWonRounds() *uint32
	AwayWonRounds() *uint32
	HomeKills() *int32
	AwayKills() *int32
	HomeGoals() *uint32
	AwayGoals() *uint32
	HomePoints() *uint32
	AwayPoints() *uint32
	HomeRuns() *uint32
	AwayRuns() *uint32
	HomeWicketsFallen() *uint32
	AwayWicketsFallen() *uint32
	HomeOversPlayed() *uint32
	HomeBallsPlayed() *uint32
	AwayOversPlayed() *uint32
	AwayBallsPlayed() *uint32
	HomeWonCoinToss() *bool
}

// Scoreboard ...
type Scoreboard interface {
	CurrentCTTeam() *uint32
	CurrentDefenderTeam() *uint32
	HomeWonRounds() *uint32
	AwayWonRounds() *uint32
	CurrentRound() *uint32
	HomeKills() *int32
	AwayKills() *int32
	HomeDestroyedTurrets() *int32
	AwayDestroyedTurrets() *int32
	HomeGold() *uint32
	AwayGold() *uint32
	HomeDestroyedTowers() *int32
	AwayDestroyedTowers() *int32
	HomeGoals() *uint32
	AwayGoals() *uint32
	Time() *uint32
	GameTime() *uint32
	ElapsedTime() *uint32
	HomePoints() *uint32
	AwayPoints() *uint32
	RemainingGameTime() *uint32
	HomeRuns() *uint32
	AwayRuns() *uint32
	HomeWicketsFallen() *uint32
	AwayWicketsFallen() *uint32
	HomeOversPlayed() *uint32
	HomeBallsPlayed() *uint32
	AwayOversPlayed() *uint32
	AwayBallsPlayed() *uint32
	HomeWonCoinToss() *bool
	HomeBatting() *bool
	AwayBatting() *bool
	Inning() *uint32
	HomeGames() *uint32
	AwayGames() *uint32
}

// CompetitionStatus ...
type CompetitionStatus interface {
	WinnerID() (*URN, error)
	Status() (*EventStatus, error)
}

// MatchStatus ...
type MatchStatus interface {
	CompetitionStatus
	PeriodScores() ([]PeriodScore, error)
	MatchStatusID() (*uint, error)
	MatchStatus() (LocalizedStaticData, error)
	LocalizedMatchStatus(locale Locale) (LocalizedStaticData, error)
	HomeScore() (*float64, error)
	AwayScore() (*float64, error)
	IsScoreboardAvailable() (bool, error)
	Scoreboard() (Scoreboard, error)
	Statistics() (Statistics, error)
}

type Statistics interface {
	HomeYellowCards() *uint32
	AwayYellowCards() *uint32

	HomeRedCards() *uint32
	AwayRedCards() *uint32

	HomeYellowRedCards() *uint32
	AwayYellowRedCards() *uint32

	HomeCorners() *uint32
	AwayCorners() *uint32
}

// Competition ...
type Competition interface {
	SportEvent
	Competitors() ([]Competitor, error)
}

// TvChannel is a TV broadcast channel attached to a fixture, in one
// locale.
type TvChannel struct {
	Name      string
	Language  string
	StreamURL string
}

// Fixture is a pure-data snapshot of a sport-event fixture in one locale.
//
// Phase 6 reshape: replaces the previous Fixture interface (with lazy
// (value, error) accessors) with a value struct populated at construction.
// StartTime is a pointer because the upstream API can omit it; ExtraInfo
// and TvChannels are nil/empty when the fixture has no such data.
type Fixture struct {
	StartTime  *time.Time
	ExtraInfo  map[string]string
	TvChannels []TvChannel
	Locale     Locale
}

// Match ...
type Match interface {
	Competition
	Status() MatchStatus
	Tournament() (Tournament, error)
	HomeCompetitor() (TeamCompetitor, error)
	AwayCompetitor() (TeamCompetitor, error)
	Fixture() Fixture
	SportFormat() (SportFormat, error)
	ExtraInfo() (map[string]string, error)
}

// LongTermEvent ...
type LongTermEvent interface {
	SportEvent
	Sport() SportSummary
}

// Tournament ...
type Tournament interface {
	LongTermEvent
	Competitors() ([]Competitor, error)
	StartDate() (*time.Time, error)
	EndDate() (*time.Time, error)
	LocalizedAbbreviation(locale Locale) (*string, error)
	IconPath() (*string, error)
	RiskTier() (int, error)
	Category() (Category, error)
}

type Category interface {
	ID() string
	Name() string
	CountryCode() *string
}

// SportSummary ...
type SportSummary interface {
	ID() URN
	Names() (map[Locale]string, error)
	IconPath() (*string, error)
	LocalizedName(locale Locale) (*string, error)
	LocalizedAbbreviation(locale Locale) (*string, error)
}

// Sport ...
type Sport interface {
	SportSummary
	Tournaments() ([]Tournament, error)
}

// EventStatus ...
type EventStatus string

// EventStatuses
const (
	NotStartedEventStatus  EventStatus = "not_started"
	LiveEventStatus        EventStatus = "live"
	SuspendedEventStatus   EventStatus = "suspended"
	EndedEventStatus       EventStatus = "ended"
	FinishedEventStatus    EventStatus = "closed"
	CancelledEventStatus   EventStatus = "cancelled"
	AbandonedEventStatus   EventStatus = "abandoned"
	DelayedEventStatus     EventStatus = "delayed"
	UnknownEventStatus     EventStatus = "unknown"
	PostponedEventStatus   EventStatus = "postponed"
	InterruptedEventStatus EventStatus = "interrupted"
)

// FixtureChange ...
type FixtureChange interface {
	SportEventID() URN
	UpdateTime() time.Time
}
