package protocols

import (
	"context"
	"time"
)

// SportsInfoManager ...
//
// All "fetch"-style methods take a context.Context for cancellation. The
// in-memory cache invalidation methods (Clear*) do not — they are pure-state
// operations.
type SportsInfoManager interface {
	Sports(ctx context.Context) ([]Sport, error)
	LocalizedSports(ctx context.Context, locale Locale) ([]Sport, error)

	ActiveTournaments(ctx context.Context) ([]Tournament, error)
	LocalizedActiveTournaments(ctx context.Context, locale Locale) ([]Tournament, error)

	SportActiveTournaments(ctx context.Context, sportName string) ([]Tournament, error)
	LocalizedSportActiveTournaments(ctx context.Context, sportName string, locale Locale) ([]Tournament, error)

	MatchesFor(ctx context.Context, date time.Time) ([]Match, error)
	LocalizedMatchesFor(ctx context.Context, date time.Time, locale Locale) ([]Match, error)

	LiveMatches(ctx context.Context) ([]Match, error)
	LocalizedLiveMatches(ctx context.Context, locale Locale) ([]Match, error)

	Match(ctx context.Context, id URN) (Match, error)
	LocalizedMatch(ctx context.Context, id URN, locale Locale) (Match, error)

	Competitor(ctx context.Context, id URN) (Competitor, error)
	LocalizedCompetitor(ctx context.Context, id URN, locale Locale) (Competitor, error)

	FixtureChanges(ctx context.Context, after time.Time) ([]FixtureChange, error)
	LocalizedFixtureChanges(ctx context.Context, locale Locale, after time.Time) ([]FixtureChange, error)

	ListOfMatches(ctx context.Context, startIndex uint, limit uint) ([]Match, error)
	LocalizedListOfMatches(ctx context.Context, startIndex uint, limit uint, locale Locale) ([]Match, error)

	AvailableTournaments(ctx context.Context, sportID URN) ([]Tournament, error)
	LocalizedAvailableTournaments(ctx context.Context, sportID URN, locale Locale) ([]Tournament, error)

	ClearMatch(id URN)
	ClearTournament(id URN)
	ClearCompetitor(id URN)
}
