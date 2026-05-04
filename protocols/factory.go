package protocols

import "context"

// EntityFactory ...
//
// Phase 6 reshape: BuildPlayer takes ctx and returns the populated
// value struct (or an error). Other Build* methods will follow as each
// entity is reshaped from interface-with-lazy-loads to value type.
type EntityFactory interface {
	BuildTournaments(tournamentIDs []URN, sportID URN, locales []Locale) []Tournament
	BuildTournament(id URN, sportID URN, locales []Locale) Tournament
	BuildSports(locales []Locale) ([]Sport, error)
	BuildSport(id URN, locales []Locale) Sport
	BuildCompetitors(competitorIDs []URN, locales []Locale) []Competitor
	BuildCompetitor(id URN, locales []Locale) Competitor
	BuildPlayer(ctx context.Context, id URN, locale Locale) (*Player, error)
	BuildFixture(ctx context.Context, id URN, locale Locale) (*Fixture, error)
	BuildMatchStatus(id URN, locales []Locale) MatchStatus
	BuildMatches(ids []URN, locales []Locale) []Match
	BuildMatch(id URN, locales []Locale, sportID *URN) Match
}
