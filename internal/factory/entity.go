package factory

import (
	"github.com/oddin-gg/gosdk/internal/cache"
	"github.com/oddin-gg/gosdk/protocols"
)

// EntityFactory ...
type EntityFactory struct {
	cacheManager *cache.Manager
}

// BuildTournaments ...
func (e *EntityFactory) BuildTournaments(tournamentIDs []protocols.URN, sportID protocols.URN, locales []protocols.Locale) []protocols.Tournament {
	result := make([]protocols.Tournament, len(tournamentIDs))
	for i := range tournamentIDs {
		id := tournamentIDs[i]
		result[i] = cache.NewTournament(
			id,
			sportID,
			e.cacheManager.TournamentCache,
			e,
			locales,
		)
	}

	return result
}

// BuildTournament ...
func (e *EntityFactory) BuildTournament(id protocols.URN, sportID protocols.URN, locales []protocols.Locale) protocols.Tournament {
	return cache.NewTournament(
		id,
		sportID,
		e.cacheManager.TournamentCache,
		e,
		locales,
	)
}

// BuildSports ...
func (e *EntityFactory) BuildSports(locales []protocols.Locale) ([]protocols.Sport, error) {
	localizedSportIDs, err := e.cacheManager.SportDataCache.Sports(locales)
	if err != nil {
		return nil, err
	}

	result := make([]protocols.Sport, len(localizedSportIDs))
	for i := range localizedSportIDs {
		id := localizedSportIDs[i]
		result[i] = cache.NewSport(id, e.cacheManager.SportDataCache, e, locales)
	}

	return result, nil
}

// BuildSport ...
func (e *EntityFactory) BuildSport(id protocols.URN, locales []protocols.Locale) protocols.Sport {
	return cache.NewSport(id, e.cacheManager.SportDataCache, e, locales)
}

// BuildCompetitors ...
func (e *EntityFactory) BuildCompetitors(competitorIDs []protocols.URN, locales []protocols.Locale) []protocols.Competitor {
	result := make([]protocols.Competitor, len(competitorIDs))
	for i := range competitorIDs {
		id := competitorIDs[i]
		result[i] = cache.NewCompetitor(id, e.cacheManager.CompetitorCache, e, locales)
	}

	return result
}

// BuildCompetitor ...
func (e *EntityFactory) BuildCompetitor(id protocols.URN, locales []protocols.Locale) protocols.Competitor {
	return cache.NewCompetitor(id, e.cacheManager.CompetitorCache, e, locales)
}

// BuildPlayer ...
func (e *EntityFactory) BuildPlayer(id protocols.URN, locale protocols.Locale) protocols.Player {
	return cache.NewPlayer(id, e.cacheManager.PlayersCache, locale)
}

// BuildFixture ...
func (e *EntityFactory) BuildFixture(id protocols.URN, locales []protocols.Locale) protocols.Fixture {
	return cache.NewFixture(id, e.cacheManager.FixtureCache, locales)
}

// BuildMatchStatus ...
func (e *EntityFactory) BuildMatchStatus(id protocols.URN, locales []protocols.Locale) protocols.MatchStatus {
	return cache.NewMatchStatus(id, e.cacheManager.MatchStatusCache, e.cacheManager.LocalizedStaticMatchStatus, locales)
}

// BuildMatches ...
func (e *EntityFactory) BuildMatches(ids []protocols.URN, locales []protocols.Locale) []protocols.Match {
	result := make([]protocols.Match, len(ids))
	for i := range ids {
		id := ids[i]
		result[i] = cache.NewMatch(id, nil, e.cacheManager.MatchCache, e, locales)
	}

	return result
}

// BuildMatch ...
func (e *EntityFactory) BuildMatch(id protocols.URN, locales []protocols.Locale, sportID *protocols.URN) protocols.Match {
	return cache.NewMatch(id, sportID, e.cacheManager.MatchCache, e, locales)
}

// NewEntityFactory ...
func NewEntityFactory(cacheManager *cache.Manager) *EntityFactory {
	return &EntityFactory{
		cacheManager: cacheManager,
	}
}
