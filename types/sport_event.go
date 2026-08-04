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

// SportFormat distinguishes between scoring shapes (classic head-to-head
// vs race-style) so consumers can branch their UI/scoring logic.
type SportFormat string

// Recognised SportFormat values.
const (
	// SportFormatUnknown is the zero/unset value or an unrecognised format.
	SportFormatUnknown = "unknown"
	// SportFormatClassic is a head-to-head sport (most match-based formats).
	SportFormatClassic = "classic"
	// SportFormatRace is a multi-competitor race (place/podium semantics).
	SportFormatRace = "race"
)

// (SportEvent interface removed in Phase 6 reshape — entities like
// Match and Tournament now expose ID, Names, ScheduledTime, etc. as
// fields/helpers directly.)

// PeriodScore is a pure-data per-period scoreline.
//
// v2.26 reshape: optional sport-specific fields migrated from `*T` to
// Optional[T] so a snapshot copy of PeriodScore is fully independent
// from cache-owned state. Pre-migration, a caller mutating
// `*ps.HomeKills = 999` mutated the cache for every other reader of
// the same Scoreboard pointer; with Optional[int32] the value lives
// inline and can never be aliased.
//
// Reading: `if v, ok := ps.HomeKills.Get(); ok { ... }` (or
// `ps.HomeKills.ValueOr(0)` for default-zero reads). Migration helpers
// `Optional.Ptr()` and `FromPtr` bridge to/from the `*T` idiom.
type PeriodScore struct {
	Type            string
	PeriodNumber    int
	MatchStatusCode int
	HomeScore       float64
	AwayScore       float64

	HomeWonRounds Optional[int]
	AwayWonRounds Optional[int]

	HomeKills Optional[int32]
	AwayKills Optional[int32]

	HomeGoals Optional[int]
	AwayGoals Optional[int]

	HomePoints Optional[int]
	AwayPoints Optional[int]

	HomeGames Optional[int]
	AwayGames Optional[int]

	HomeRuns          Optional[int]
	AwayRuns          Optional[int]
	HomeWicketsFallen Optional[int]
	AwayWicketsFallen Optional[int]
	HomeOversPlayed   Optional[int]
	HomeBallsPlayed   Optional[int]
	AwayOversPlayed   Optional[int]
	AwayBallsPlayed   Optional[int]
	HomeWonCoinToss   Optional[bool]
}

// Scoreboard is a pure-data live scoreboard for an event.
//
// v2.26 reshape: optional fields migrated from `*T` to Optional[T] —
// see PeriodScore for rationale. Closes the inner-pointer aliasing
// finding from the v2.25 review.
type Scoreboard struct {
	CurrentCTTeam       Optional[int]
	CurrentDefenderTeam Optional[int]
	HomeWonRounds       Optional[int]
	AwayWonRounds       Optional[int]
	CurrentRound        Optional[int]

	HomeKills            Optional[int32]
	AwayKills            Optional[int32]
	HomeDestroyedTurrets Optional[int32]
	AwayDestroyedTurrets Optional[int32]
	HomeGold             Optional[int]
	AwayGold             Optional[int]
	HomeDestroyedTowers  Optional[int32]
	AwayDestroyedTowers  Optional[int32]

	HomeGoals Optional[int]
	AwayGoals Optional[int]

	Time              Optional[int]
	GameTime          Optional[int]
	ElapsedTime       Optional[int]
	RemainingGameTime Optional[int]

	HomePoints Optional[int]
	AwayPoints Optional[int]

	HomeRuns          Optional[int]
	AwayRuns          Optional[int]
	HomeWicketsFallen Optional[int]
	AwayWicketsFallen Optional[int]
	HomeOversPlayed   Optional[int]
	HomeBallsPlayed   Optional[int]
	AwayOversPlayed   Optional[int]
	AwayBallsPlayed   Optional[int]
	HomeWonCoinToss   Optional[bool]
	HomeBatting       Optional[bool]
	AwayBatting       Optional[bool]
	Inning            Optional[int]

	HomeGames Optional[int]
	AwayGames Optional[int]
}

// Statistics is a pure-data per-event statistics snapshot.
//
// v2.26 reshape: see Scoreboard / PeriodScore for rationale.
type Statistics struct {
	HomeYellowCards    Optional[int]
	AwayYellowCards    Optional[int]
	HomeRedCards       Optional[int]
	AwayRedCards       Optional[int]
	HomeYellowRedCards Optional[int]
	AwayYellowRedCards Optional[int]
	HomeCorners        Optional[int]
	AwayCorners        Optional[int]
}

// MatchStatus is a pure-data live status snapshot for a match.
//
// Phase 6 reshape: replaces the previous MatchStatus interface (with
// (value, error) lazy accessors) with a value struct populated at
// construction. StatusDescription / StatusDescriptions carry the
// localized status-code description from the static catalog.
//
// v2.26 reshape: numeric / boolean optionals migrated from `*T` to
// Optional[T] (MatchStatusID, HomeScore, AwayScore). Structural
// pointer fields (Scoreboard, Statistics, StatusDescription) stay as
// `*T` because they reference larger structs that are wholesale-
// optional and pointer-shared by the cache builder; their *interior*
// fields are now value-style, so a shallow copy of a Scoreboard
// pointee is decoupled from the cache.
type MatchStatus struct {
	WinnerID              *URN
	Status                EventStatus
	MatchStatusID         Optional[int]
	HomeScore             Optional[float64]
	AwayScore             Optional[float64]
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

	// ReferenceIDs is the upstream API's `reference_ids` block as a
	// flat name→value map (locale-independent — the API returns the
	// same set across locales). Forward-ported from main commit
	// fcc3c0d (PR #38). Nil when the API returned no reference_ids
	// for this match.
	ReferenceIDs map[string]string

	Tournament     Tournament
	Competitors    []Competitor
	HomeCompetitor *TeamCompetitor // nil for non-classic sport formats
	AwayCompetitor *TeamCompetitor // nil for non-classic sport formats
	Fixture        Fixture
	Status         MatchStatus
}

// Name returns the localized match name, or None if the match wasn't
// loaded for that locale. Use `.ValueOr("")` for the always-string
// semantics or `.Get()` to detect the not-loaded case explicitly.
func (m Match) Name(locale Locale) Optional[string] {
	if v, ok := m.Names[locale]; ok {
		return Some(v)
	}
	return None[string]()
}

// ExtraInfoFor returns the extra-info map for the given locale, or nil.
func (m Match) ExtraInfoFor(locale Locale) map[string]string {
	return m.ExtraInfo[locale]
}

// Category is a pure-data tournament category (e.g., a country grouping
// for a sport).
//
// v2.32 reshape: CountryCode migrated from *string to Optional[string].
// Missed in the v2.28 string-pointer cluster — the Tournament builder
// previously copied the cache's *string pointer directly into the
// snapshot, leaving the same caller-mutation aliasing the rest of
// v2.28 closed for sibling fields.
type Category struct {
	ID          string
	Name        string
	CountryCode Optional[string]
	// IconPath is the category icon location, when the API supplies
	// one (main-branch parity: feat(category) PR #43).
	IconPath Optional[string]
}

// Tournament is a pure-data snapshot of a tournament populated across
// one or more locales.
//
// Phase 6 reshape: replaces the previous Tournament interface (with
// (value, error) lazy accessors) with a value struct populated at
// construction. Sport carries the sport summary; CompetitorIDs lets
// callers resolve competitors lazily through the SDK.
//
// v2.28 reshape: IconPath migrated from *string to Optional[string].
// Time pointers (StartDate / EndDate / ScheduledTime / ScheduledEndTime)
// stay as *time.Time per Go convention.
type Tournament struct {
	ID               URN
	Names            map[Locale]string
	Abbreviations    map[Locale]string
	StartDate        *time.Time
	EndDate          *time.Time
	ScheduledTime    *time.Time
	ScheduledEndTime *time.Time
	IconPath         Optional[string]
	RiskTier         int
	Category         *Category
	Sport            SportSummary
	CompetitorIDs    []URN
}

// Name returns the localized name, or None if the tournament wasn't
// loaded for that locale.
func (t Tournament) Name(locale Locale) Optional[string] {
	if v, ok := t.Names[locale]; ok {
		return Some(v)
	}
	return None[string]()
}

// Abbreviation returns the localized abbreviation, or None if not loaded.
func (t Tournament) Abbreviation(locale Locale) Optional[string] {
	if v, ok := t.Abbreviations[locale]; ok {
		return Some(v)
	}
	return None[string]()
}

// SportSummary is a pure-data snapshot of a sport's per-locale labels.
//
// Phase 6 reshape: replaces the previous SportSummary interface (with
// (value, error) accessors) with a value struct populated at
// construction. Names and Abbreviations carry every locale that was
// loaded for this sport.
//
// v2.28 reshape: IconPath migrated from *string to Optional[string].
type SportSummary struct {
	ID            URN
	Names         map[Locale]string
	Abbreviations map[Locale]string
	IconPath      Optional[string]
}

// Name returns the localized name for the given locale, or None if the
// sport hasn't been loaded for that locale.
func (s SportSummary) Name(locale Locale) Optional[string] {
	if v, ok := s.Names[locale]; ok {
		return Some(v)
	}
	return None[string]()
}

// Abbreviation returns the localized abbreviation for the given locale,
// or None if not loaded.
func (s SportSummary) Abbreviation(locale Locale) Optional[string] {
	if v, ok := s.Abbreviations[locale]; ok {
		return Some(v)
	}
	return None[string]()
}

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
