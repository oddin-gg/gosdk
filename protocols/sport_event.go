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

// SportEvent ...
type SportEvent interface {
	ID() URN
	RefID() (*URN, error)
	LocalizedName(locale Locale) (*string, error)
	SportID() (*URN, error)
	ScheduledTime() (*time.Time, error)
	ScheduledEndTime() (*time.Time, error)
	LiveOddsAvailability() (*LiveOddsAvailability, error)
}

// PeriodScore ...
type PeriodScore interface {
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
}

// Scoreboard ...
type Scoreboard interface {
	CurrentCTTeam() *uint32
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
}

// Competition ...
type Competition interface {
	SportEvent
	Competitors() ([]Competitor, error)
}

// TvChannel ...
type TvChannel interface {
	Name() string
	Language() string
	StreamURL() string
}

// Fixture ...
type Fixture interface {
	StartTime() (*time.Time, error)
	ExtraInfo() (map[string]string, error)
	TvChannels() ([]TvChannel, error)
}

// Match ...
type Match interface {
	Competition
	Status() MatchStatus
	Tournament() (Tournament, error)
	HomeCompetitor() (TeamCompetitor, error)
	AwayCompetitor() (TeamCompetitor, error)
	Fixture() Fixture
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
}

// SportSummary ...
type SportSummary interface {
	ID() URN
	RefID() (*URN, error)
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
	SportEventRefID() *URN
	UpdateTime() time.Time
}
