package sport

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/cache"
	"github.com/oddin-gg/gosdk/internal/factory"
	"github.com/oddin-gg/gosdk/types"
)

type fixtureChangeImpl struct {
	id          types.URN
	updatedTime time.Time
}

func (f fixtureChangeImpl) SportEventID() types.URN {
	return f.id
}

func (f fixtureChangeImpl) UpdateTime() time.Time {
	return f.updatedTime
}

// Manager ...
//
// Public methods take ctx and propagate it to the API client. The
// EntityFactory / cache layer underneath is rewritten in Phase 3 with full
// ctx propagation; for now its loaders are ctx-unaware and we accept that
// asymmetry until Phase 3 lands.
type Manager struct {
	entityFactory         *factory.EntityFactory
	apiClient             *api.Client
	oddsFeedConfiguration types.OddsFeedConfiguration
	cacheManager          *cache.Manager
}

// Sports ...
func (m *Manager) Sports(ctx context.Context) ([]types.Sport, error) {
	return m.LocalizedSports(ctx, m.oddsFeedConfiguration.DefaultLocale())
}

// LocalizedSports ...
func (m *Manager) LocalizedSports(ctx context.Context, locale types.Locale) ([]types.Sport, error) {
	return m.entityFactory.BuildSports(ctx, []types.Locale{locale})
}

// ActiveTournaments ...
func (m *Manager) ActiveTournaments(ctx context.Context) ([]types.Tournament, error) {
	return m.LocalizedActiveTournaments(ctx, m.oddsFeedConfiguration.DefaultLocale())
}

// LocalizedActiveTournaments ...
func (m *Manager) LocalizedActiveTournaments(ctx context.Context, locale types.Locale) ([]types.Tournament, error) {
	sports, err := m.LocalizedSports(ctx, locale)
	if err != nil {
		return nil, err
	}

	var result []types.Tournament
	for _, sport := range sports {
		tournaments, err := m.entityFactory.BuildTournaments(ctx, sport.TournamentIDs, sport.ID, []types.Locale{locale})
		if err != nil {
			return nil, err
		}
		result = append(result, tournaments...)
	}

	return result, nil
}

// SportActiveTournaments ...
func (m *Manager) SportActiveTournaments(ctx context.Context, sportName string) ([]types.Tournament, error) {
	return m.LocalizedSportActiveTournaments(ctx, sportName, m.oddsFeedConfiguration.DefaultLocale())
}

// LocalizedSportActiveTournaments ...
func (m *Manager) LocalizedSportActiveTournaments(ctx context.Context, sportName string, locale types.Locale) ([]types.Tournament, error) {
	sports, err := m.Sports(ctx)
	if err != nil {
		return nil, err
	}

	for _, sport := range sports {
		name := sport.Name(locale)
		if name == "" {
			continue
		}
		if strings.EqualFold(name, sportName) {
			return m.entityFactory.BuildTournaments(ctx, sport.TournamentIDs, sport.ID, []types.Locale{locale})
		}
	}

	return nil, fmt.Errorf("cannot find any sport with given name %s or locale %s combination", sportName, locale)
}

// MatchesFor ...
func (m *Manager) MatchesFor(ctx context.Context, date time.Time) ([]types.Match, error) {
	return m.LocalizedMatchesFor(ctx, date, m.oddsFeedConfiguration.DefaultLocale())
}

// LocalizedMatchesFor ...
func (m *Manager) LocalizedMatchesFor(ctx context.Context, date time.Time, locale types.Locale) ([]types.Match, error) {
	data, err := m.apiClient.FetchMatches(ctx, date, locale)
	if err != nil {
		return nil, err
	}

	result := make([]types.Match, 0, len(data))
	for i := range data {
		id, err := types.ParseURN(data[i].ID)
		if err != nil {
			return nil, err
		}
		match, err := m.entityFactory.BuildMatch(ctx, *id, []types.Locale{locale}, nil)
		if err != nil {
			return nil, err
		}
		result = append(result, *match)
	}

	return result, nil
}

// LiveMatches ...
func (m *Manager) LiveMatches(ctx context.Context) ([]types.Match, error) {
	return m.LocalizedLiveMatches(ctx, m.oddsFeedConfiguration.DefaultLocale())
}

// LocalizedLiveMatches ...
func (m *Manager) LocalizedLiveMatches(ctx context.Context, locale types.Locale) ([]types.Match, error) {
	data, err := m.apiClient.FetchLiveMatches(ctx, locale)
	if err != nil {
		return nil, err
	}

	result := make([]types.Match, 0, len(data))
	for i := range data {
		id, err := types.ParseURN(data[i].ID)
		if err != nil {
			return nil, err
		}
		match, err := m.entityFactory.BuildMatch(ctx, *id, []types.Locale{locale}, nil)
		if err != nil {
			return nil, err
		}
		result = append(result, *match)
	}

	return result, nil
}

// Match ...
func (m *Manager) Match(ctx context.Context, id types.URN) (types.Match, error) {
	return m.LocalizedMatch(ctx, id, m.oddsFeedConfiguration.DefaultLocale())
}

// LocalizedMatch ...
func (m *Manager) LocalizedMatch(ctx context.Context, id types.URN, locale types.Locale) (types.Match, error) {
	match, err := m.entityFactory.BuildMatch(ctx, id, []types.Locale{locale}, nil)
	if err != nil {
		return types.Match{}, err
	}
	return *match, nil
}

// Competitor ...
func (m *Manager) Competitor(ctx context.Context, id types.URN) (types.Competitor, error) {
	return m.LocalizedCompetitor(ctx, id, m.oddsFeedConfiguration.DefaultLocale())
}

// LocalizedCompetitor ...
func (m *Manager) LocalizedCompetitor(ctx context.Context, id types.URN, locale types.Locale) (types.Competitor, error) {
	c, err := m.entityFactory.BuildCompetitor(ctx, id, []types.Locale{locale})
	if err != nil {
		return types.Competitor{}, err
	}
	return *c, nil
}

// FixtureChanges ...
func (m *Manager) FixtureChanges(ctx context.Context, after time.Time) ([]types.FixtureChange, error) {
	return m.LocalizedFixtureChanges(ctx, m.oddsFeedConfiguration.DefaultLocale(), after)
}

// LocalizedFixtureChanges ...
func (m *Manager) LocalizedFixtureChanges(ctx context.Context, locale types.Locale, after time.Time) ([]types.FixtureChange, error) {
	data, err := m.apiClient.FetchFixtureChanges(ctx, locale, after)
	if err != nil {
		return nil, err
	}

	result := make([]types.FixtureChange, len(data))
	for i := range data {
		fixtureChange := data[i]
		id, err := types.ParseURN(fixtureChange.SportEventID)
		if err != nil {
			return nil, err
		}

		result[i] = &fixtureChangeImpl{
			id:          *id,
			updatedTime: (time.Time)(fixtureChange.UpdatedAt),
		}
	}

	return result, nil
}

// ListOfMatches ...
func (m *Manager) ListOfMatches(ctx context.Context, startIndex uint, limit uint) ([]types.Match, error) {
	return m.LocalizedListOfMatches(ctx, startIndex, limit, m.oddsFeedConfiguration.DefaultLocale())
}

// LocalizedListOfMatches ...
func (m *Manager) LocalizedListOfMatches(ctx context.Context, startIndex uint, limit uint, locale types.Locale) ([]types.Match, error) {
	switch {
	case limit > 1000:
		return nil, fmt.Errorf("max limit is 1000")
	case limit < 1:
		return nil, fmt.Errorf("min limit is 1")
	}

	data, err := m.apiClient.FetchSchedule(ctx, startIndex, limit, locale)
	if err != nil {
		return nil, err
	}

	result := make([]types.Match, 0, len(data))
	for i := range data {
		id, err := types.ParseURN(data[i].ID)
		if err != nil {
			return nil, err
		}
		match, err := m.entityFactory.BuildMatch(ctx, *id, []types.Locale{locale}, nil)
		if err != nil {
			return nil, err
		}
		result = append(result, *match)
	}

	return result, nil
}

// AvailableTournaments ...
func (m *Manager) AvailableTournaments(ctx context.Context, sportID types.URN) ([]types.Tournament, error) {
	return m.LocalizedAvailableTournaments(ctx, sportID, m.oddsFeedConfiguration.DefaultLocale())
}

// LocalizedAvailableTournaments ...
func (m *Manager) LocalizedAvailableTournaments(ctx context.Context, sportID types.URN, locale types.Locale) ([]types.Tournament, error) {
	data, err := m.apiClient.FetchTournaments(ctx, sportID, locale)
	if err != nil {
		return nil, err
	}

	result := make([]types.Tournament, 0, len(data))
	for i := range data {
		id, err := types.ParseURN(data[i].ID)
		if err != nil {
			return nil, err
		}
		t, err := m.entityFactory.BuildTournament(ctx, *id, sportID, []types.Locale{locale})
		if err != nil {
			return nil, err
		}
		result = append(result, *t)
	}

	return result, nil
}

// ClearMatch ...
func (m *Manager) ClearMatch(id types.URN) {
	m.cacheManager.MatchCache.ClearCacheItem(id)
}

// ClearTournament ...
func (m *Manager) ClearTournament(id types.URN) {
	m.cacheManager.TournamentCache.ClearCacheItem(id)
}

// ClearCompetitor ...
func (m *Manager) ClearCompetitor(id types.URN) {
	m.cacheManager.CompetitorCache.ClearCacheItem(id)
}

// NewManager ...
func NewManager(entityFactory *factory.EntityFactory, apiClient *api.Client, cacheManager *cache.Manager, oddsFeedConfiguration types.OddsFeedConfiguration) *Manager {
	return &Manager{
		entityFactory:         entityFactory,
		apiClient:             apiClient,
		cacheManager:          cacheManager,
		oddsFeedConfiguration: oddsFeedConfiguration,
	}
}
