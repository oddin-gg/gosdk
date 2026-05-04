package factory

import (
	"context"

	"github.com/oddin-gg/gosdk/internal/cache"
	"github.com/oddin-gg/gosdk/protocols"
)

// EntityFactory ...
type EntityFactory struct {
	cacheManager *cache.Manager
}

// BuildTournaments resolves a slice of Tournament snapshots.
func (e *EntityFactory) BuildTournaments(ctx context.Context, ids []protocols.URN, sportID protocols.URN, locales []protocols.Locale) ([]protocols.Tournament, error) {
	result := make([]protocols.Tournament, 0, len(ids))
	for _, id := range ids {
		t, err := cache.BuildTournament(ctx, e.cacheManager.TournamentCache, e, id, sportID, locales)
		if err != nil {
			return nil, err
		}
		result = append(result, *t)
	}
	return result, nil
}

// BuildTournament resolves a single Tournament snapshot.
func (e *EntityFactory) BuildTournament(ctx context.Context, id protocols.URN, sportID protocols.URN, locales []protocols.Locale) (*protocols.Tournament, error) {
	return cache.BuildTournament(ctx, e.cacheManager.TournamentCache, e, id, sportID, locales)
}

// BuildSports resolves the catalog of Sport snapshots for the given
// locales. Each entry is a populated value; tournament IDs are filled
// in but tournaments themselves are not eagerly resolved.
func (e *EntityFactory) BuildSports(ctx context.Context, locales []protocols.Locale) ([]protocols.Sport, error) {
	sportIDs, err := e.cacheManager.SportDataCache.Sports(ctx, locales)
	if err != nil {
		return nil, err
	}
	result := make([]protocols.Sport, 0, len(sportIDs))
	for _, id := range sportIDs {
		s, err := cache.BuildSport(ctx, e.cacheManager.SportDataCache, id, locales)
		if err != nil {
			return nil, err
		}
		result = append(result, *s)
	}
	return result, nil
}

// BuildSport resolves a single Sport snapshot.
func (e *EntityFactory) BuildSport(ctx context.Context, id protocols.URN, locales []protocols.Locale) (*protocols.Sport, error) {
	return cache.BuildSport(ctx, e.cacheManager.SportDataCache, id, locales)
}

// BuildCompetitors resolves a slice of Competitor snapshots.
func (e *EntityFactory) BuildCompetitors(ctx context.Context, ids []protocols.URN, locales []protocols.Locale) ([]protocols.Competitor, error) {
	result := make([]protocols.Competitor, 0, len(ids))
	for _, id := range ids {
		c, err := cache.BuildCompetitor(ctx, e.cacheManager.CompetitorCache, e, id, locales)
		if err != nil {
			return nil, err
		}
		result = append(result, *c)
	}
	return result, nil
}

// BuildCompetitor resolves a single Competitor snapshot.
func (e *EntityFactory) BuildCompetitor(ctx context.Context, id protocols.URN, locales []protocols.Locale) (*protocols.Competitor, error) {
	return cache.BuildCompetitor(ctx, e.cacheManager.CompetitorCache, e, id, locales)
}

// BuildTeamCompetitor resolves a TeamCompetitor (Competitor + qualifier).
func (e *EntityFactory) BuildTeamCompetitor(ctx context.Context, id protocols.URN, qualifier *string, locales []protocols.Locale) (*protocols.TeamCompetitor, error) {
	return cache.BuildTeamCompetitor(ctx, e.cacheManager.CompetitorCache, e, id, qualifier, locales)
}

// BuildPlayer resolves a Player snapshot from the cache, fetching if
// missing. Returns a populated value or an error from the underlying
// fetch — never returns nil with nil error.
func (e *EntityFactory) BuildPlayer(ctx context.Context, id protocols.URN, locale protocols.Locale) (*protocols.Player, error) {
	return cache.BuildPlayer(ctx, e.cacheManager.PlayersCache, id, locale)
}

// BuildFixture resolves a per-locale Fixture snapshot from the cache,
// fetching if missing. Returns a populated value or an error.
func (e *EntityFactory) BuildFixture(ctx context.Context, id protocols.URN, locale protocols.Locale) (*protocols.Fixture, error) {
	return cache.BuildFixture(ctx, e.cacheManager.FixtureCache, id, locale)
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
