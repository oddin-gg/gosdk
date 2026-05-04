package types

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

// (SportEvent interface removed in Phase 6 reshape — entities like
// Match and Tournament now expose ID, Names, ScheduledTime, etc. as
// fields/helpers directly.)

// PeriodScore is a pure-data per-period scoreline.
//
// Phase 6 reshape: replaces the previous PeriodScore interface (a
// 22-method getter list) with a value struct. Optional sport-specific
// fields are pointers — nil means "not reported."
type PeriodScore struct {
	Type            string
	PeriodNumber    uint
	MatchStatusCode uint
	HomeScore       float64
	AwayScore       float64

	HomeWonRounds *uint32
	AwayWonRounds *uint32

	HomeKills *int32
	AwayKills *int32

	HomeGoals *uint32
	AwayGoals *uint32

	HomePoints *uint32
	AwayPoints *uint32

	HomeRuns          *uint32
	AwayRuns          *uint32
	HomeWicketsFallen *uint32
	AwayWicketsFallen *uint32
	HomeOversPlayed   *uint32
	HomeBallsPlayed   *uint32
	AwayOversPlayed   *uint32
	AwayBallsPlayed   *uint32
	HomeWonCoinToss   *bool
}

// Scoreboard is a pure-data live scoreboard for an event.
//
// Phase 6 reshape: replaces the Scoreboard interface (a 35-method
// getter list) with a value struct. Optional sport-specific fields
// are pointers — nil means "not reported."
type Scoreboard struct {
	CurrentCTTeam       *uint32
	CurrentDefenderTeam *uint32
	HomeWonRounds       *uint32
	AwayWonRounds       *uint32
	CurrentRound        *uint32

	HomeKills            *int32
	AwayKills            *int32
	HomeDestroyedTurrets *int32
	AwayDestroyedTurrets *int32
	HomeGold             *uint32
	AwayGold             *uint32
	HomeDestroyedTowers  *int32
	AwayDestroyedTowers  *int32

	HomeGoals *uint32
	AwayGoals *uint32

	Time              *uint32
	GameTime          *uint32
	ElapsedTime       *uint32
	RemainingGameTime *uint32

	HomePoints *uint32
	AwayPoints *uint32

	HomeRuns          *uint32
	AwayRuns          *uint32
	HomeWicketsFallen *uint32
	AwayWicketsFallen *uint32
	HomeOversPlayed   *uint32
	HomeBallsPlayed   *uint32
	AwayOversPlayed   *uint32
	AwayBallsPlayed   *uint32
	HomeWonCoinToss   *bool
	HomeBatting       *bool
	AwayBatting       *bool
	Inning            *uint32

	HomeGames *uint32
	AwayGames *uint32
}

// Statistics is a pure-data per-event statistics snapshot.
type Statistics struct {
	HomeYellowCards    *uint32
	AwayYellowCards    *uint32
	HomeRedCards       *uint32
	AwayRedCards       *uint32
	HomeYellowRedCards *uint32
	AwayYellowRedCards *uint32
	HomeCorners        *uint32
	AwayCorners        *uint32
}

// MatchStatus is a pure-data live status snapshot for a match.
//
// Phase 6 reshape: replaces the previous MatchStatus interface (with
// (value, error) lazy accessors) with a value struct populated at
// construction. StatusDescription / StatusDescriptions carry the
// localized status-code description from the static catalog.
type MatchStatus struct {
	WinnerID              *URN
	Status                EventStatus
	MatchStatusID         *uint
	HomeScore             *float64
	AwayScore             *float64
	IsScoreboardAvailable bool
	PeriodScores          []PeriodScore
	Scoreboard            *Scoreboard
	Statistics            *Statistics

	// StatusDescription is the localized status-code description in the
	// primary locale this snapshot was constructed for. Nil when the
	// match has no MatchStatusID or the static catalog wasn't loaded.
	StatusDescription *LocalizedStaticData
}

// (Competition interface removed in Phase 6 reshape — Match exposes
// Competitors as a field directly.)

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

// Match is a pure-data snapshot of a match populated across one or
// more locales.
//
// Phase 6 reshape: replaces the previous Match interface (with 11
// (value, error) lazy accessors) with a value struct populated at
// construction. Linked entities (Tournament, Competitors, MatchStatus,
// Fixture) are eager-loaded as direct fields. Per-locale name and
// extra info are exposed as maps with helper methods.
type Match struct {
	ID                   URN
	Names                map[Locale]string
	SportID              URN
	ScheduledTime        *time.Time
	ScheduledEndTime     *time.Time
	LiveOddsAvailability LiveOddsAvailability
	SportFormat          SportFormat
	ExtraInfo            map[Locale]map[string]string

	Tournament     Tournament
	Competitors    []Competitor
	HomeCompetitor *TeamCompetitor // nil for non-classic sport formats
	AwayCompetitor *TeamCompetitor // nil for non-classic sport formats
	Fixture        Fixture
	Status         MatchStatus
}

// Name returns the localized match name, or "" if not loaded.
func (m Match) Name(locale Locale) string { return m.Names[locale] }

// ExtraInfoFor returns the extra-info map for the given locale, or nil.
func (m Match) ExtraInfoFor(locale Locale) map[string]string {
	return m.ExtraInfo[locale]
}

// Category is a pure-data tournament category (e.g., a country grouping
// for a sport).
type Category struct {
	ID          string
	Name        string
	CountryCode *string
}

// Tournament is a pure-data snapshot of a tournament populated across
// one or more locales.
//
// Phase 6 reshape: replaces the previous Tournament interface (with
// (value, error) lazy accessors) with a value struct populated at
// construction. Sport carries the sport summary; CompetitorIDs lets
// callers resolve competitors lazily through the SDK.
type Tournament struct {
	ID               URN
	Names            map[Locale]string
	Abbreviations    map[Locale]string
	StartDate        *time.Time
	EndDate          *time.Time
	ScheduledTime    *time.Time
	ScheduledEndTime *time.Time
	IconPath         *string
	RiskTier         int
	Category         *Category
	Sport            SportSummary
	CompetitorIDs    []URN
}

// Name returns the localized name, or "" if not loaded.
func (t Tournament) Name(locale Locale) string { return t.Names[locale] }

// Abbreviation returns the localized abbreviation, or "" if not loaded.
func (t Tournament) Abbreviation(locale Locale) string { return t.Abbreviations[locale] }

// SportSummary is a pure-data snapshot of a sport's per-locale labels.
//
// Phase 6 reshape: replaces the previous SportSummary interface (with
// (value, error) accessors) with a value struct populated at
// construction. Names and Abbreviations carry every locale that was
// loaded for this sport.
type SportSummary struct {
	ID            URN
	Names         map[Locale]string
	Abbreviations map[Locale]string
	IconPath      *string
}

// Name returns the localized name for the given locale, or "" if the
// sport hasn't been loaded for that locale.
func (s SportSummary) Name(locale Locale) string { return s.Names[locale] }

// Abbreviation returns the localized abbreviation for the given locale,
// or "" if not loaded.
func (s SportSummary) Abbreviation(locale Locale) string { return s.Abbreviations[locale] }

// Sport extends SportSummary with the URNs of tournaments under this
// sport. Tournaments are not eagerly resolved to keep Sport cheap to
// construct; callers pass the URNs to Client.Tournament(...) when they
// want a populated Tournament value.
type Sport struct {
	SportSummary
	TournamentIDs []URN
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
