package protocols

import "time"

// SportsInfoManager ...
type SportsInfoManager interface {
	Sports() ([]Sport, error)
	LocalizedSports(locale Locale) ([]Sport, error)

	ActiveTournaments() ([]Tournament, error)
	LocalizedActiveTournaments(locale Locale) ([]Tournament, error)

	SportActiveTournaments(sportName string) ([]Tournament, error)
	LocalizedSportActiveTournaments(sportName string, locale Locale) ([]Tournament, error)

	MatchesFor(date time.Time) ([]Match, error)
	LocalizedMatchesFor(date time.Time, locale Locale) ([]Match, error)

	LiveMatches() ([]Match, error)
	LocalizedLiveMatches(locale Locale) ([]Match, error)

	Match(id URN) (Match, error)
	LocalizedMatch(id URN, locale Locale) (Match, error)

	Competitor(id URN) (Competitor, error)
	LocalizedCompetitor(id URN, locale Locale) (Competitor, error)

	FixtureChanges(after time.Time) ([]FixtureChange, error)
	LocalizedFixtureChanges(locale Locale, after time.Time) ([]FixtureChange, error)

	ListOfMatches(startIndex uint, limit uint) ([]Match, error)
	LocalizedListOfMatches(startIndex uint, limit uint, locale Locale) ([]Match, error)

	AvailableTournaments(sportID URN) ([]Tournament, error)
	LocalizedAvailableTournaments(sportID URN, locale Locale) ([]Tournament, error)

	ClearMatch(id URN)
	ClearTournament(id URN)
	ClearCompetitor(id URN)
}
