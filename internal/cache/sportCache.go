package cache

import (
	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/api/xml"
	"github.com/oddin-gg/gosdk/protocols"
	"github.com/patrickmn/go-cache"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"sync"
)

// SportCache ...
type SportCache struct {
	apiClient     *api.Client
	internalCache *cache.Cache
	loadedLocales map[protocols.Locale]struct{}
	mux           sync.Mutex
	logger        *log.Logger
}

// OnAPIResponse ...
func (s *SportCache) OnAPIResponse(apiResponse protocols.Response) {
	if apiResponse.Locale == nil || apiResponse.Data == nil {
		return
	}

	result := make(map[string]*xml.Sport)
	switch data := apiResponse.Data.(type) {
	case *xml.TournamentScheduleResponse:
		result[data.Tournament.ID] = &data.Tournament.Sport
	case *xml.TournamentResponse:
		result[data.Tournament.ID] = &data.Tournament.Sport
	}

	err := s.handleTournamentData(*apiResponse.Locale, result)
	if err != nil {
		s.logger.WithError(err).Errorf("failed to process api response %s", apiResponse)
	}
}

// Sport ...
func (s *SportCache) Sport(id protocols.URN, locales []protocols.Locale) (*LocalizedSport, error) {
	item, _ := s.internalCache.Get(id.ToString())
	result, ok := item.(*LocalizedSport)

	var missingLocales []protocols.Locale
	if !ok {
		missingLocales = locales
	} else {
		missingLocales = s.findMissingLocales(locales)
		if len(missingLocales) != 0 {
			err := s.loadAndCacheItems(missingLocales)
			if err != nil {
				return nil, err
			}
		}
	}

	if len(missingLocales) != 0 {
		err := s.loadAndCacheItems(missingLocales)
		if err != nil {
			return nil, err
		}
		item, _ = s.internalCache.Get(id.ToString())
		result, ok = item.(*LocalizedSport)
		if !ok {
			return nil, errors.New("item missing")
		}
	}

	return result, nil
}

// Sports ...
func (s *SportCache) Sports(locales []protocols.Locale) ([]protocols.URN, error) {
	missingLocales := s.findMissingLocales(locales)
	if len(missingLocales) != 0 {
		err := s.loadAndCacheItems(missingLocales)
		if err != nil {
			return nil, err
		}
	}

	items := s.internalCache.Items()
	result := make([]protocols.URN, len(items))
	index := 0
	for _, value := range items {
		obj := value.Object
		item, ok := obj.(*LocalizedSport)
		if !ok {
			return nil, errors.New("wrong item type in sports cache")
		}
		result[index] = item.id
		index++
	}

	return result, nil
}

// SportTournaments ...
func (s *SportCache) SportTournaments(sportID protocols.URN, locale protocols.Locale) ([]protocols.URN, error) {
	item, _ := s.internalCache.Get(sportID.ToString())
	result, ok := item.(*LocalizedSport)
	if ok && len(result.tournamentIDs) != 0 {
		return result.makeTournamentIDsList(), nil
	}

	tournaments, err := s.apiClient.FetchTournaments(sportID, locale)
	if err != nil {
		return nil, err
	}

	tournamentIDs := make([]protocols.URN, len(tournaments))
	for i := range tournaments {
		id, err := protocols.ParseURN(tournaments[i].ID)
		if err != nil {
			return nil, err
		}
		tournamentIDs[i] = *id
		err = s.refreshOrInsertItem(sportID, locale, nil, id)
		if err != nil {
			return nil, err
		}
	}

	return tournamentIDs, nil
}

func (s *SportCache) findMissingLocales(locales []protocols.Locale) []protocols.Locale {
	var missingLocales []protocols.Locale
	for i := range locales {
		locale := locales[i]
		s.mux.Lock()
		_, ok := s.loadedLocales[locale]
		s.mux.Unlock()

		if !ok {
			missingLocales = append(missingLocales, locale)
		}
	}

	return missingLocales
}

func (s *SportCache) loadAndCacheItems(locales []protocols.Locale) error {
	for i := range locales {
		locale := locales[i]
		data, err := s.apiClient.FetchSports(locale)
		if err != nil {
			return err
		}

		for k := range data {
			sport := data[k]
			id, err := protocols.ParseURN(sport.ID)
			if err != nil {
				return err
			}

			err = s.refreshOrInsertItem(*id, locale, &sport, nil)
			if err != nil {
				return err
			}
		}

		s.mux.Lock()
		s.loadedLocales[locale] = struct{}{}
		s.mux.Unlock()
	}

	return nil
}

func (s *SportCache) refreshOrInsertItem(id protocols.URN, locale protocols.Locale, sport *xml.Sport, tournamentID *protocols.URN) error {
	item, _ := s.internalCache.Get(id.ToString())
	result, ok := item.(*LocalizedSport)
	if !ok {
		result = &LocalizedSport{
			id:            id,
			name:          make(map[protocols.Locale]string),
			tournamentIDs: make(map[protocols.URN]struct{}),
			abbreviation:  make(map[protocols.Locale]string),
		}
	}

	result.mux.Lock()
	defer result.mux.Unlock()

	if sport != nil {
		result.name[locale] = sport.Name
		result.abbreviation[locale] = sport.Abbreviation
		result.iconPath = sport.IconPath

		if sport.RefID != nil {
			refID, err := protocols.ParseURN(*sport.RefID)
			if err != nil {
				return err
			}
			result.refID = refID
		}
	}

	if tournamentID != nil {
		result.tournamentIDs[*tournamentID] = struct{}{}
	}

	s.internalCache.Set(id.ToString(), result, 0)
	return nil
}

func (s *SportCache) handleTournamentData(locale protocols.Locale, tournamentData map[string]*xml.Sport) error {
	for key, value := range tournamentData {
		tournamentID, err := protocols.ParseURN(key)
		if err != nil {
			return err
		}

		sportID, err := protocols.ParseURN(value.ID)
		if err != nil {
			return err
		}

		err = s.refreshOrInsertItem(*sportID, locale, tournamentData[key], tournamentID)
	}

	return nil
}

func newSportDataCache(client *api.Client, logger *log.Logger) *SportCache {
	sportDataCache := &SportCache{
		apiClient:     client,
		internalCache: cache.New(cache.NoExpiration, cache.NoExpiration),
		logger:        logger,
		loadedLocales: make(map[protocols.Locale]struct{}),
	}

	client.SubscribeWithAPIObserver(sportDataCache)

	return sportDataCache
}

// LocalizedSport ...
type LocalizedSport struct {
	id            protocols.URN
	tournamentIDs map[protocols.URN]struct{}
	name          map[protocols.Locale]string
	abbreviation  map[protocols.Locale]string
	iconPath      *string
	mux           sync.Mutex
	refID         *protocols.URN
}

func (l *LocalizedSport) makeTournamentIDsList() []protocols.URN {
	l.mux.Lock()
	defer l.mux.Unlock()

	result := make([]protocols.URN, len(l.tournamentIDs))
	index := 0
	for key := range l.tournamentIDs {
		result[index] = key
		index++
	}

	return result
}

type sportImpl struct {
	id             protocols.URN
	sportDataCache *SportCache
	entityFactory  protocols.EntityFactory
	locales        []protocols.Locale
}

func (s sportImpl) IconPath() (*string, error) {
	item, err := s.sportDataCache.Sport(s.id, s.locales)
	if err != nil {
		return nil, err
	}

	return item.iconPath, nil
}

func (s sportImpl) ID() protocols.URN {
	return s.id
}

func (s sportImpl) RefID() (*protocols.URN, error) {
	item, err := s.sportDataCache.Sport(s.id, s.locales)
	if err != nil {
		return nil, err
	}

	return item.refID, nil
}

func (s sportImpl) Names() (map[protocols.Locale]string, error) {
	item, err := s.sportDataCache.Sport(s.id, s.locales)
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

func (s sportImpl) LocalizedName(locale protocols.Locale) (*string, error) {
	item, err := s.sportDataCache.Sport(s.id, s.locales)
	if err != nil {
		return nil, err
	}

	item.mux.Lock()
	defer item.mux.Unlock()

	result, ok := item.name[locale]
	if !ok {
		return nil, errors.Errorf("missing locale %s", locale)
	}

	return &result, nil
}

func (s sportImpl) LocalizedAbbreviation(locale protocols.Locale) (*string, error) {
	item, err := s.sportDataCache.Sport(s.id, s.locales)
	if err != nil {
		return nil, err
	}

	item.mux.Lock()
	defer item.mux.Unlock()

	result, ok := item.abbreviation[locale]
	if !ok {
		return nil, errors.Errorf("missing locale %s", locale)
	}

	return &result, nil
}

func (s sportImpl) Tournaments() ([]protocols.Tournament, error) {
	item, err := s.sportDataCache.Sport(s.id, s.locales)
	if err != nil {
		return nil, err
	}

	tournamentIDs := item.makeTournamentIDsList()
	if len(tournamentIDs) == 0 {
		tournamentIDs, err = s.sportDataCache.SportTournaments(s.id, s.locales[0])
		if err != nil {
			return nil, err
		}
	}

	return s.entityFactory.BuildTournaments(tournamentIDs, s.id, s.locales), nil
}

// NewSport ...
func NewSport(id protocols.URN, dataCache *SportCache, entityFactory protocols.EntityFactory, locales []protocols.Locale) protocols.Sport {
	return &sportImpl{
		id:             id,
		sportDataCache: dataCache,
		entityFactory:  entityFactory,
		locales:        locales,
	}
}
