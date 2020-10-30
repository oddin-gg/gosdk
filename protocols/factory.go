package protocols

// EntityFactory ...
type EntityFactory interface {
	BuildTournaments(tournamentIDs []URN, sportID URN, locales []Locale) []Tournament
	BuildTournament(id URN, sportID URN, locales []Locale) Tournament
	BuildSports(locales []Locale) ([]Sport, error)
	BuildSport(id URN, locales []Locale) Sport
	BuildCompetitors(competitorIDs []URN, locales []Locale) []Competitor
	BuildCompetitor(id URN, locales []Locale) Competitor
	BuildFixture(id URN, locales []Locale) Fixture
	BuildMatchStatus(id URN, locales []Locale) MatchStatus
	BuildMatches(ids []URN, locales []Locale) []Match
	BuildMatch(id URN, locales []Locale, sportID *URN) Match
}
