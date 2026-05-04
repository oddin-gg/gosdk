package sport

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/cache"
	"github.com/oddin-gg/gosdk/internal/factory"
	"github.com/oddin-gg/gosdk/protocols"
)

type fixtureChangeImpl struct {
	id          protocols.URN
	updatedTime time.Time
}

func (f fixtureChangeImpl) SportEventID() protocols.URN {
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
	oddsFeedConfiguration protocols.OddsFeedConfiguration
	cacheManager          *cache.Manager
}

// Sports ...
func (m *Manager) Sports(ctx context.Context) ([]protocols.Sport, error) {
	return m.LocalizedSports(ctx, m.oddsFeedConfiguration.DefaultLocale())
}

// LocalizedSports ...
func (m *Manager) LocalizedSports(ctx context.Context, locale protocols.Locale) ([]protocols.Sport, error) {
	return m.entityFactory.BuildSports(ctx, []protocols.Locale{locale})
}

// ActiveTournaments ...
func (m *Manager) ActiveTournaments(ctx context.Context) ([]protocols.Tournament, error) {
	return m.LocalizedActiveTournaments(ctx, m.oddsFeedConfiguration.DefaultLocale())
}

// LocalizedActiveTournaments ...
func (m *Manager) LocalizedActiveTournaments(ctx context.Context, locale protocols.Locale) ([]protocols.Tournament, error) {
	sports, err := m.LocalizedSports(ctx, locale)
	if err != nil {
		return nil, err
	}

	var result []protocols.Tournament
	for _, sport := range sports {
		tournaments := m.entityFactory.BuildTournaments(sport.TournamentIDs, sport.ID, []protocols.Locale{locale})
		result = append(result, tournaments...)
	}

	return result, nil
}

// SportActiveTournaments ...
func (m *Manager) SportActiveTournaments(ctx context.Context, sportName string) ([]protocols.Tournament, error) {
	return m.LocalizedSportActiveTournaments(ctx, sportName, m.oddsFeedConfiguration.DefaultLocale())
}

// LocalizedSportActiveTournaments ...
func (m *Manager) LocalizedSportActiveTournaments(ctx context.Context, sportName string, locale protocols.Locale) ([]protocols.Tournament, error) {
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
			return m.entityFactory.BuildTournaments(sport.TournamentIDs, sport.ID, []protocols.Locale{locale}), nil
		}
	}

	return nil, fmt.Errorf("cannot find any sport with given name %s or locale %s combination", sportName, locale)
}

// MatchesFor ...
func (m *Manager) MatchesFor(ctx context.Context, date time.Time) ([]protocols.Match, error) {
	return m.LocalizedMatchesFor(ctx, date, m.oddsFeedConfiguration.DefaultLocale())
}

// LocalizedMatchesFor ...
func (m *Manager) LocalizedMatchesFor(ctx context.Context, date time.Time, locale protocols.Locale) ([]protocols.Match, error) {
	data, err := m.apiClient.FetchMatches(ctx, date, locale)
	if err != nil {
		return nil, err
	}

	result := make([]protocols.Match, len(data))
	for i := range data {
		id, err := protocols.ParseURN(data[i].ID)
		if err != nil {
			return nil, err
		}
		result[i] = m.entityFactory.BuildMatch(*id, []protocols.Locale{locale}, nil)
	}

	return result, nil
}

// LiveMatches ...
func (m *Manager) LiveMatches(ctx context.Context) ([]protocols.Match, error) {
	return m.LocalizedLiveMatches(ctx, m.oddsFeedConfiguration.DefaultLocale())
}

// LocalizedLiveMatches ...
func (m *Manager) LocalizedLiveMatches(ctx context.Context, locale protocols.Locale) ([]protocols.Match, error) {
	data, err := m.apiClient.FetchLiveMatches(ctx, locale)
	if err != nil {
		return nil, err
	}

	result := make([]protocols.Match, len(data))
	for i := range data {
		id, err := protocols.ParseURN(data[i].ID)
		if err != nil {
			return nil, err
		}
		result[i] = m.entityFactory.BuildMatch(*id, []protocols.Locale{locale}, nil)
	}

	return result, nil
}

// Match ...
func (m *Manager) Match(ctx context.Context, id protocols.URN) (protocols.Match, error) {
	return m.LocalizedMatch(ctx, id, m.oddsFeedConfiguration.DefaultLocale())
}

// LocalizedMatch ...
func (m *Manager) LocalizedMatch(ctx context.Context, id protocols.URN, locale protocols.Locale) (protocols.Match, error) {
	_ = ctx
	return m.entityFactory.BuildMatch(id, []protocols.Locale{locale}, nil), nil
}

// Competitor ...
func (m *Manager) Competitor(ctx context.Context, id protocols.URN) (protocols.Competitor, error) {
	return m.LocalizedCompetitor(ctx, id, m.oddsFeedConfiguration.DefaultLocale())
}

// LocalizedCompetitor ...
func (m *Manager) LocalizedCompetitor(ctx context.Context, id protocols.URN, locale protocols.Locale) (protocols.Competitor, error) {
	c, err := m.entityFactory.BuildCompetitor(ctx, id, []protocols.Locale{locale})
	if err != nil {
		return protocols.Competitor{}, err
	}
	return *c, nil
}

// FixtureChanges ...
func (m *Manager) FixtureChanges(ctx context.Context, after time.Time) ([]protocols.FixtureChange, error) {
	return m.LocalizedFixtureChanges(ctx, m.oddsFeedConfiguration.DefaultLocale(), after)
}

// LocalizedFixtureChanges ...
func (m *Manager) LocalizedFixtureChanges(ctx context.Context, locale protocols.Locale, after time.Time) ([]protocols.FixtureChange, error) {
	data, err := m.apiClient.FetchFixtureChanges(ctx, locale, after)
	if err != nil {
		return nil, err
	}

	result := make([]protocols.FixtureChange, len(data))
	for i := range data {
		fixtureChange := data[i]
		id, err := protocols.ParseURN(fixtureChange.SportEventID)
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
func (m *Manager) ListOfMatches(ctx context.Context, startIndex uint, limit uint) ([]protocols.Match, error) {
	return m.LocalizedListOfMatches(ctx, startIndex, limit, m.oddsFeedConfiguration.DefaultLocale())
}

// LocalizedListOfMatches ...
func (m *Manager) LocalizedListOfMatches(ctx context.Context, startIndex uint, limit uint, locale protocols.Locale) ([]protocols.Match, error) {
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

	result := make([]protocols.Match, len(data))
	for i := range data {
		id, err := protocols.ParseURN(data[i].ID)
		if err != nil {
			return nil, err
		}
		result[i] = m.entityFactory.BuildMatch(*id, []protocols.Locale{locale}, nil)
	}

	return result, nil
}

// AvailableTournaments ...
func (m *Manager) AvailableTournaments(ctx context.Context, sportID protocols.URN) ([]protocols.Tournament, error) {
	return m.LocalizedAvailableTournaments(ctx, sportID, m.oddsFeedConfiguration.DefaultLocale())
}

// LocalizedAvailableTournaments ...
func (m *Manager) LocalizedAvailableTournaments(ctx context.Context, sportID protocols.URN, locale protocols.Locale) ([]protocols.Tournament, error) {
	data, err := m.apiClient.FetchTournaments(ctx, sportID, locale)
	if err != nil {
		return nil, err
	}

	result := make([]protocols.Tournament, len(data))
	for i := range data {
		id, err := protocols.ParseURN(data[i].ID)
		if err != nil {
			return nil, err
		}
		result[i] = m.entityFactory.BuildTournament(*id, sportID, []protocols.Locale{locale})
	}

	return result, nil
}

// ClearMatch ...
func (m *Manager) ClearMatch(id protocols.URN) {
	m.cacheManager.MatchCache.ClearCacheItem(id)
}

// ClearTournament ...
func (m *Manager) ClearTournament(id protocols.URN) {
	m.cacheManager.TournamentCache.ClearCacheItem(id)
}

// ClearCompetitor ...
func (m *Manager) ClearCompetitor(id protocols.URN) {
	m.cacheManager.CompetitorCache.ClearCacheItem(id)
}

// NewManager ...
func NewManager(entityFactory *factory.EntityFactory, apiClient *api.Client, cacheManager *cache.Manager, oddsFeedConfiguration protocols.OddsFeedConfiguration) *Manager {
	return &Manager{
		entityFactory:         entityFactory,
		apiClient:             apiClient,
		cacheManager:          cacheManager,
		oddsFeedConfiguration: oddsFeedConfiguration,
	}
}
