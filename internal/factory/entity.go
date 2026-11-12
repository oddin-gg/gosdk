package factory

import (
	"context"

	"github.com/oddin-gg/gosdk/internal/cache"
	"github.com/oddin-gg/gosdk/types"
)

// EntityFactory ...
type EntityFactory struct {
	cacheManager *cache.Manager
}

// BuildTournaments resolves a slice of Tournament snapshots.
func (e *EntityFactory) BuildTournaments(ctx context.Context, ids []types.URN, sportID types.URN, locales []types.Locale) ([]types.Tournament, error) {
	result := make([]types.Tournament, 0, len(ids))
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
func (e *EntityFactory) BuildTournament(ctx context.Context, id types.URN, sportID types.URN, locales []types.Locale) (*types.Tournament, error) {
	return cache.BuildTournament(ctx, e.cacheManager.TournamentCache, e, id, sportID, locales)
}

// BuildSports resolves the catalog of Sport snapshots for the given
// locales. Each entry is a populated value; tournament IDs are filled
// in but tournaments themselves are not eagerly resolved.
func (e *EntityFactory) BuildSports(ctx context.Context, locales []types.Locale) ([]types.Sport, error) {
	sportIDs, err := e.cacheManager.SportDataCache.Sports(ctx, locales)
	if err != nil {
		return nil, err
	}
	result := make([]types.Sport, 0, len(sportIDs))
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
func (e *EntityFactory) BuildSport(ctx context.Context, id types.URN, locales []types.Locale) (*types.Sport, error) {
	return cache.BuildSport(ctx, e.cacheManager.SportDataCache, id, locales)
}

// BuildCompetitors resolves a slice of Competitor snapshots.
func (e *EntityFactory) BuildCompetitors(ctx context.Context, ids []types.URN, locales []types.Locale) ([]types.Competitor, error) {
	result := make([]types.Competitor, 0, len(ids))
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
func (e *EntityFactory) BuildCompetitor(ctx context.Context, id types.URN, locales []types.Locale) (*types.Competitor, error) {
	return cache.BuildCompetitor(ctx, e.cacheManager.CompetitorCache, e, id, locales)
}

// BuildTeamCompetitor resolves a TeamCompetitor (Competitor + qualifier).
func (e *EntityFactory) BuildTeamCompetitor(ctx context.Context, id types.URN, qualifier *string, locales []types.Locale) (*types.TeamCompetitor, error) {
	return cache.BuildTeamCompetitor(ctx, e.cacheManager.CompetitorCache, e, id, qualifier, locales)
}

// BuildPlayer resolves a Player snapshot from the cache, fetching if
// missing. Returns a populated value or an error from the underlying
// fetch — never returns nil with nil error.
func (e *EntityFactory) BuildPlayer(ctx context.Context, id types.URN, locale types.Locale) (*types.Player, error) {
	return cache.BuildPlayer(ctx, e.cacheManager.PlayersCache, id, locale)
}

// BuildFixture resolves a per-locale Fixture snapshot from the cache,
// fetching if missing. Returns a populated value or an error.
func (e *EntityFactory) BuildFixture(ctx context.Context, id types.URN, locale types.Locale) (*types.Fixture, error) {
	return cache.BuildFixture(ctx, e.cacheManager.FixtureCache, id, locale)
}

// BuildMatchStatus resolves a *types.MatchStatus snapshot. Fetches
// from the API via the cache observer if the entry is missing.
func (e *EntityFactory) BuildMatchStatus(ctx context.Context, id types.URN, locales []types.Locale) (*types.MatchStatus, error) {
	return cache.BuildMatchStatus(ctx, e.cacheManager.MatchStatusCache, e.cacheManager.LocalizedStaticMatchStatus, id, locales)
}

// BuildMatches resolves a slice of Match snapshots.
func (e *EntityFactory) BuildMatches(ctx context.Context, ids []types.URN, locales []types.Locale) ([]types.Match, error) {
	result := make([]types.Match, 0, len(ids))
	for _, id := range ids {
		m, err := cache.BuildMatch(ctx, e.cacheManager.MatchCache, e, id, nil, locales)
		if err != nil {
			return nil, err
		}
		result = append(result, *m)
	}
	return result, nil
}

// BuildMatch resolves a single Match snapshot. sportID overrides the
// cached sport when non-nil (used by feed-message decode where the
// routing key carries the sport).
func (e *EntityFactory) BuildMatch(ctx context.Context, id types.URN, locales []types.Locale, sportID *types.URN) (*types.Match, error) {
	return cache.BuildMatch(ctx, e.cacheManager.MatchCache, e, id, sportID, locales)
}

// NewEntityFactory ...
func NewEntityFactory(cacheManager *cache.Manager) *EntityFactory {
	return &EntityFactory{
		cacheManager: cacheManager,
	}
}
