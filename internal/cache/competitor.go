package cache

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/api/xml"
	"github.com/oddin-gg/gosdk/protocols"
	"github.com/patrickmn/go-cache"
	log "github.com/sirupsen/logrus"
)

// TeamWrapper ...
type TeamWrapper interface {
	GetID() string
	// Deprecated: do not use this method, it will be removed in future
	GetRefID() *string
	GetName() string
	GetAbbreviation() string
}

type TeamWithPlayers interface {
	TeamWrapper
	GetPlayers() []xml.Player
}

// CompetitorCache ...
type CompetitorCache struct {
	apiClient     *api.Client
	internalCache *cache.Cache
	iconCache     *cache.Cache
	logger        *log.Entry
}

// OnAPIResponse ...
func (c *CompetitorCache) OnAPIResponse(apiResponse protocols.Response) {
	if apiResponse.Locale == nil || apiResponse.Data == nil {
		return
	}

	var result []TeamWrapper
	switch data := apiResponse.Data.(type) {
	case *xml.FixtureResponse:
		for i := range data.Fixture.Competitors.Competitor {
			result = append(result, data.Fixture.Competitors.Competitor[i])
		}
	case *xml.MatchSummaryResponse:
		for i := range data.SportEvent.Competitors.Competitor {
			result = append(result, data.SportEvent.Competitors.Competitor[i])
		}

	case *xml.ScheduleResponse:
		for _, event := range data.SportEvents {
			for i := range event.Competitors.Competitor {
				competitor := event.Competitors.Competitor[i]
				result = append(result, competitor)
			}
		}

	case *xml.TournamentsResponse:
		for _, event := range data.Tournaments {
			for i := range event.Competitors.Competitor {
				competitor := event.Competitors.Competitor[i]
				result = append(result, competitor)
			}
		}

	case *xml.TournamentResponse:
		for i := range data.Competitors.Competitor {
			result = append(result, data.Competitors.Competitor[i])
		}
	}

	if len(result) == 0 {
		return
	}

	err := c.handleTeamData(*apiResponse.Locale, result)
	if err != nil {
		c.logger.WithError(err).Errorf("failed to process api data %v", apiResponse)
	}
}

// Competitor ...
func (c *CompetitorCache) Competitor(id protocols.URN, locales []protocols.Locale) (*LocalizedCompetitor, error) {
	item, _ := c.internalCache.Get(id.ToString())
	result, ok := item.(*LocalizedCompetitor)

	var toFetchLocales []protocols.Locale
	if ok {
		loadedLocales := result.loadedLocales()
		for i := range locales {
			locale := locales[i]
			_, ok := loadedLocales[locale]

			if !ok {
				toFetchLocales = append(toFetchLocales, locale)
			}
		}
	} else {
		toFetchLocales = locales
	}

	if len(toFetchLocales) != 0 {
		return c.loadAndCacheItem(id, toFetchLocales)
	}

	return result, nil
}

// ClearCacheItem ...
func (c *CompetitorCache) ClearCacheItem(id protocols.URN) {
	c.internalCache.Delete(id.ToString())
	c.iconCache.Delete(id.ToString())
}

// CompetitorIcon ...
func (c *CompetitorCache) CompetitorIcon(id protocols.URN, locale protocols.Locale) (*string, error) {
	icon, ok := c.iconCache.Get(id.ToString())
	if ok {
		return icon.(*string), nil
	}

	data, err := c.apiClient.FetchCompetitorProfile(id, locale)
	if err != nil {
		return nil, err
	}

	c.iconCache.Set(id.ToString(), data.IconPath, 0)
	return data.IconPath, nil
}

func (c *CompetitorCache) handleTeamData(locale protocols.Locale, teams []TeamWrapper) error {
	for i := range teams {
		team := teams[i]
		id, err := protocols.ParseURN(team.GetID())
		if err != nil {
			return err
		}

		err = c.refreshOrInsertItem(*id, locale, team)
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *CompetitorCache) refreshOrInsertItem(id protocols.URN, locale protocols.Locale, team TeamWrapper) error {
	item, _ := c.internalCache.Get(id.ToString())
	result, ok := item.(*LocalizedCompetitor)
	if !ok {
		var refID *protocols.URN
		var err error
		if team.GetRefID() != nil {
			refID, err = protocols.ParseURN(*team.GetRefID())
		}

		if err != nil {
			return err
		}

		result = &LocalizedCompetitor{
			id:           id,
			refID:        refID,
			name:         make(map[protocols.Locale]string),
			abbreviation: make(map[protocols.Locale]string),
		}
	}

	result.mux.Lock()
	defer result.mux.Unlock()

	result.name[locale] = team.GetName()
	result.abbreviation[locale] = team.GetAbbreviation()
	if teamWithPlayers, ok := team.(TeamWithPlayers); ok {
		players := teamWithPlayers.GetPlayers()

		playerURNs := make([]protocols.URN, 0, len(players))
		for _, p := range players {
			playerURN, err := protocols.ParseURN(p.ID)
			if err != nil {
				return fmt.Errorf("parsing URN when refreshing players: %w", err)
			}

			playerURNs = append(playerURNs, *playerURN)
		}

		result.players = playerURNs
	}

	c.internalCache.Set(id.ToString(), result, 0)

	return nil
}

func (c *CompetitorCache) loadAndCacheItem(id protocols.URN, locales []protocols.Locale) (*LocalizedCompetitor, error) {
	for i := range locales {
		locale := locales[i]
		data, err := c.apiClient.FetchCompetitorProfileWithPlayers(id, locale)
		if err != nil {
			return nil, err
		}

		// Set icon if needed
		c.iconCache.Set(id.ToString(), data.Competitor.IconPath, 0)

		err = c.refreshOrInsertItem(id, locale, data)
		if err != nil {
			return nil, err
		}
	}

	item, _ := c.internalCache.Get(id.ToString())
	result, ok := item.(*LocalizedCompetitor)
	if !ok {
		return nil, errors.New("item missing")
	}

	return result, nil
}

func newCompetitorCache(client *api.Client, logger *log.Entry) *CompetitorCache {
	competitorCache := &CompetitorCache{
		apiClient:     client,
		internalCache: cache.New(24*time.Hour, 1*time.Hour),
		iconCache:     cache.New(24*time.Hour, 1*time.Hour),
		logger:        logger,
	}

	client.SubscribeWithAPIObserver(competitorCache)
	return competitorCache
}

// LocalizedCompetitor ...
type LocalizedCompetitor struct {
	id           protocols.URN
	refID        *protocols.URN
	name         map[protocols.Locale]string
	abbreviation map[protocols.Locale]string
	players      []protocols.URN
	mux          sync.Mutex
}

func (l *LocalizedCompetitor) loadedLocales() map[protocols.Locale]struct{} {
	l.mux.Lock()
	defer l.mux.Unlock()

	result := make(map[protocols.Locale]struct{})

	for key := range l.name {
		result[key] = struct{}{}
	}

	for key := range l.abbreviation {
		result[key] = struct{}{}
	}

	return result
}

func (l *LocalizedCompetitor) LocalizedName(locale protocols.Locale) (*string, error) {
	l.mux.Lock()
	defer l.mux.Unlock()

	result, ok := l.name[locale]
	if !ok {
		return nil, fmt.Errorf("missing locale %s", locale)
	}

	return &result, nil
}

type competitorImpl struct {
	id              protocols.URN
	competitorCache *CompetitorCache
	entityFactory   protocols.EntityFactory
	locales         []protocols.Locale
}

func (c competitorImpl) IconPath() (*string, error) {
	if len(c.locales) == 0 {
		return nil, errors.New("missing locales")
	}

	item, err := c.competitorCache.CompetitorIcon(c.id, c.locales[0])
	if err != nil {
		return nil, err
	}

	return item, nil
}

func (c competitorImpl) ID() protocols.URN {
	return c.id
}

// Deprecated: do not use this method, it will be removed in future
func (c competitorImpl) RefID() (*protocols.URN, error) {
	item, err := c.competitorCache.Competitor(c.id, c.locales)
	if err != nil {
		return nil, err
	}

	return item.refID, nil
}

func (c competitorImpl) Names() (map[protocols.Locale]string, error) {
	item, err := c.competitorCache.Competitor(c.id, c.locales)
	if err != nil {
		return nil, err
	}

	item.mux.Lock()
	defer item.mux.Unlock()

	// Return copy of map
	result := make(map[protocols.Locale]string, len(item.name))
	for key, value := range item.name {
		result[key] = value
	}

	return result, nil
}

func (c competitorImpl) LocalizedName(locale protocols.Locale) (*string, error) {
	item, err := c.competitorCache.Competitor(c.id, c.locales)
	if err != nil {
		return nil, err
	}

	return item.LocalizedName(locale)
}

func (c competitorImpl) Abbreviations() (map[protocols.Locale]string, error) {
	item, err := c.competitorCache.Competitor(c.id, c.locales)
	if err != nil {
		return nil, err
	}

	item.mux.Lock()
	defer item.mux.Unlock()

	// Return copy of map
	result := make(map[protocols.Locale]string, len(item.abbreviation))
	for key, value := range item.abbreviation {
		result[key] = value
	}

	return result, nil
}

func (c competitorImpl) LocalizedAbbreviation(locale protocols.Locale) (*string, error) {
	item, err := c.competitorCache.Competitor(c.id, c.locales)
	if err != nil {
		return nil, err
	}

	item.mux.Lock()
	defer item.mux.Unlock()

	result, ok := item.abbreviation[locale]
	if !ok {
		return nil, fmt.Errorf("missing locale %s", locale)
	}

	return &result, nil
}

func (c competitorImpl) Players() (map[protocols.Locale][]protocols.Player, error) {
	item, err := c.competitorCache.Competitor(c.id, c.locales)
	if err != nil {
		return nil, err
	}

	// If the competitor does not contain any players, try loading them.
	if len(item.players) == 0 {
		_, err := c.competitorCache.loadAndCacheItem(c.id, c.locales)
		if err != nil {
			return nil, fmt.Errorf("loading players into cache: %w", err)
		}
	}

	playersPerLocale := make(map[protocols.Locale][]protocols.Player, len(c.locales))
	for _, locale := range c.locales {
		players := make([]protocols.Player, 0, len(item.players))
		for _, playerURN := range item.players {
			player := c.entityFactory.BuildPlayer(playerURN, locale)
			players = append(players, player)
		}
		playersPerLocale[locale] = players
	}

	return playersPerLocale, nil
}

func (c competitorImpl) LocalizedPlayers(locale protocols.Locale) ([]protocols.Player, error) {
	item, err := c.competitorCache.Competitor(c.id, c.locales)
	if err != nil {
		return nil, err
	}

	// If the competitor does not contain any players, try loading them.
	if len(item.players) == 0 {
		_, err := c.competitorCache.loadAndCacheItem(c.id, c.locales)
		if err != nil {
			return nil, fmt.Errorf("loading players into cache: %w", err)
		}
	}

	players := make([]protocols.Player, 0, len(item.players))
	for _, playerURN := range item.players {
		player := c.entityFactory.BuildPlayer(playerURN, locale)
		players = append(players, player)
	}

	return players, nil
}

type teamCompetitorImpl struct {
	qualifier  *string
	competitor protocols.Competitor
}

func (t teamCompetitorImpl) IconPath() (*string, error) {
	return t.competitor.IconPath()
}

func (t teamCompetitorImpl) ID() protocols.URN {
	return t.competitor.ID()
}

func (t teamCompetitorImpl) RefID() (*protocols.URN, error) {
	return t.competitor.RefID()
}

func (t teamCompetitorImpl) Names() (map[protocols.Locale]string, error) {
	return t.competitor.Names()
}

func (t teamCompetitorImpl) LocalizedName(locale protocols.Locale) (*string, error) {
	return t.competitor.LocalizedName(locale)
}

func (t teamCompetitorImpl) Abbreviations() (map[protocols.Locale]string, error) {
	return t.competitor.Abbreviations()
}

func (t teamCompetitorImpl) LocalizedAbbreviation(locale protocols.Locale) (*string, error) {
	return t.competitor.LocalizedAbbreviation(locale)
}

func (t teamCompetitorImpl) Players() (map[protocols.Locale][]protocols.Player, error) {
	return t.competitor.Players()
}

func (t teamCompetitorImpl) LocalizedPlayers(locale protocols.Locale) ([]protocols.Player, error) {
	return t.competitor.LocalizedPlayers(locale)
}

func (t teamCompetitorImpl) Qualifier() *string {
	return t.qualifier
}

// NewCompetitor ...
func NewCompetitor(id protocols.URN, competitorCache *CompetitorCache, entityFactory protocols.EntityFactory, locales []protocols.Locale) protocols.Competitor {
	return &competitorImpl{
		id:              id,
		competitorCache: competitorCache,
		entityFactory:   entityFactory,
		locales:         locales,
	}
}
