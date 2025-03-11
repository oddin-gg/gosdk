package sport

import (
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
	refID       *protocols.URN
	updatedTime time.Time
}

// Deprecated: do not use this method, it will be removed in future
func (f fixtureChangeImpl) SportEventRefID() *protocols.URN {
	return f.refID
}

func (f fixtureChangeImpl) SportEventID() protocols.URN {
	return f.id
}

func (f fixtureChangeImpl) UpdateTime() time.Time {
	return f.updatedTime
}

// Manager ...
type Manager struct {
	entityFactory         *factory.EntityFactory
	apiClient             *api.Client
	oddsFeedConfiguration protocols.OddsFeedConfiguration
	cacheManager          *cache.Manager
}

// Sports ...
func (m *Manager) Sports() ([]protocols.Sport, error) {
	return m.LocalizedSports(m.oddsFeedConfiguration.DefaultLocale())
}

// LocalizedSports ...
func (m *Manager) LocalizedSports(locale protocols.Locale) ([]protocols.Sport, error) {
	return m.entityFactory.BuildSports([]protocols.Locale{locale})
}

// ActiveTournaments ...
func (m *Manager) ActiveTournaments() ([]protocols.Tournament, error) {
	return m.LocalizedActiveTournaments(m.oddsFeedConfiguration.DefaultLocale())
}

// LocalizedActiveTournaments ...
func (m *Manager) LocalizedActiveTournaments(locale protocols.Locale) ([]protocols.Tournament, error) {
	sports, err := m.LocalizedSports(locale)
	if err != nil {
		return nil, err
	}

	var result []protocols.Tournament
	for _, sport := range sports {
		tournaments, err := sport.Tournaments()
		if err != nil {
			return nil, err
		}
		result = append(result, tournaments...)
	}

	return result, nil
}

// SportActiveTournaments ...
func (m *Manager) SportActiveTournaments(sportName string) ([]protocols.Tournament, error) {
	return m.LocalizedSportActiveTournaments(sportName, m.oddsFeedConfiguration.DefaultLocale())
}

// LocalizedSportActiveTournaments ...
func (m *Manager) LocalizedSportActiveTournaments(sportName string, locale protocols.Locale) ([]protocols.Tournament, error) {
	sports, err := m.Sports()
	if err != nil {
		return nil, err
	}

	for _, sport := range sports {
		name, err := sport.LocalizedName(locale)
		if err != nil {
			return nil, err
		}

		if strings.EqualFold(*name, sportName) {
			return sport.Tournaments()
		}
	}

	return nil, fmt.Errorf("cannot find any sport with given name %s or locale %s combination", sportName, locale)
}

// MatchesFor ...
func (m *Manager) MatchesFor(date time.Time) ([]protocols.Match, error) {
	return m.LocalizedMatchesFor(date, m.oddsFeedConfiguration.DefaultLocale())
}

// LocalizedMatchesFor ...
func (m *Manager) LocalizedMatchesFor(date time.Time, locale protocols.Locale) ([]protocols.Match, error) {
	data, err := m.apiClient.FetchMatches(date, locale)
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
func (m *Manager) LiveMatches() ([]protocols.Match, error) {
	return m.LocalizedLiveMatches(m.oddsFeedConfiguration.DefaultLocale())
}

// LocalizedLiveMatches ...
func (m *Manager) LocalizedLiveMatches(locale protocols.Locale) ([]protocols.Match, error) {
	data, err := m.apiClient.FetchLiveMatches(locale)
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
func (m *Manager) Match(id protocols.URN) (protocols.Match, error) {
	return m.LocalizedMatch(id, m.oddsFeedConfiguration.DefaultLocale())
}

// LocalizedMatch ...
func (m *Manager) LocalizedMatch(id protocols.URN, locale protocols.Locale) (protocols.Match, error) {
	return m.entityFactory.BuildMatch(id, []protocols.Locale{locale}, nil), nil
}

// Competitor ...
func (m *Manager) Competitor(id protocols.URN) (protocols.Competitor, error) {
	return m.LocalizedCompetitor(id, m.oddsFeedConfiguration.DefaultLocale())
}

// LocalizedCompetitor ...
func (m *Manager) LocalizedCompetitor(id protocols.URN, locale protocols.Locale) (protocols.Competitor, error) {
	return m.entityFactory.BuildCompetitor(id, []protocols.Locale{locale}), nil
}

// FixtureChanges ...
func (m *Manager) FixtureChanges(after time.Time) ([]protocols.FixtureChange, error) {
	return m.LocalizedFixtureChanges(m.oddsFeedConfiguration.DefaultLocale(), after)
}

// LocalizedFixtureChanges ...
func (m *Manager) LocalizedFixtureChanges(locale protocols.Locale, after time.Time) ([]protocols.FixtureChange, error) {
	data, err := m.apiClient.FetchFixtureChanges(locale, after)
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

		var refID *protocols.URN
		if fixtureChange.SportEventRefID != nil {
			refID, err = protocols.ParseURN(*fixtureChange.SportEventRefID)
			if err != nil {
				return nil, err
			}
		}

		result[i] = &fixtureChangeImpl{
			id:          *id,
			refID:       refID,
			updatedTime: (time.Time)(fixtureChange.UpdatedAt),
		}
	}

	return result, nil
}

// ListOfMatches ...
func (m *Manager) ListOfMatches(startIndex uint, limit uint) ([]protocols.Match, error) {
	return m.LocalizedListOfMatches(startIndex, limit, m.oddsFeedConfiguration.DefaultLocale())
}

// LocalizedListOfMatches ...
func (m *Manager) LocalizedListOfMatches(startIndex uint, limit uint, locale protocols.Locale) ([]protocols.Match, error) {
	switch {
	case limit > 1000:
		return nil, fmt.Errorf("max limit is 1000")
	case limit < 1:
		return nil, fmt.Errorf("min limit is 1")
	}

	data, err := m.apiClient.FetchSchedule(startIndex, limit, locale)
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
func (m *Manager) AvailableTournaments(sportID protocols.URN) ([]protocols.Tournament, error) {
	return m.LocalizedAvailableTournaments(sportID, m.oddsFeedConfiguration.DefaultLocale())
}

// LocalizedAvailableTournaments ...
func (m *Manager) LocalizedAvailableTournaments(sportID protocols.URN, locale protocols.Locale) ([]protocols.Tournament, error) {
	data, err := m.apiClient.FetchTournaments(sportID, locale)
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
