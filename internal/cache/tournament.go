package cache

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/oddin-gg/gosdk/internal/api"
	apiXML "github.com/oddin-gg/gosdk/internal/api/xml"
	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	"github.com/oddin-gg/gosdk/protocols"
	"github.com/patrickmn/go-cache"
	log "github.com/sirupsen/logrus"
)

// TournamentWrapper ...
type TournamentWrapper interface {
	GetID() string
	// Deprecated: do not use this method, it will be removed in future
	GetRefID() *string
	GetStartDate() *time.Time
	GetEndDate() *time.Time
	GetSportID() string
	GetScheduledTime() *time.Time
	GetScheduledEndTime() *time.Time
	GetName() string
	GetAbbreviation() string
}

// TournamentExtendedWrapper ...
type TournamentExtendedWrapper interface {
	TournamentWrapper
	GetCompetitors() []apiXML.Team
}

// TournamentCache ...
type TournamentCache struct {
	apiClient     *api.Client
	internalCache *cache.Cache
	iconCache     *cache.Cache
	logger        *log.Entry
}

// OnFeedMessage ...
func (t *TournamentCache) OnFeedMessage(id protocols.URN, feedMessage *protocols.FeedMessage) {
	if feedMessage.Message == nil {
		return
	}

	message, ok := feedMessage.Message.(*feedXML.FixtureChange)
	switch {
	case !ok:
		fallthrough
	case id.Type != "tournament":
		return
	default:
		id, err := protocols.ParseURN(message.EventID)
		if err != nil {
			t.logger.WithError(err).Errorf("failed to convert urn %s", message.EventID)
		}

		t.ClearCacheItem(*id)
	}
}

// OnAPIResponse ...
func (t *TournamentCache) OnAPIResponse(apiResponse protocols.Response) {
	if apiResponse.Locale == nil || apiResponse.Data == nil {
		return
	}

	var result []TournamentWrapper
	switch data := apiResponse.Data.(type) {
	case *apiXML.FixtureResponse:
		result = append(result, data.Fixture.Tournament)
	case *apiXML.TournamentsResponse:
		for i := range data.Tournaments {
			tournament := data.Tournaments[i]
			result = append(result, tournament)
		}
	case *apiXML.MatchSummaryResponse:
		result = append(result, data.SportEvent.Tournament)
	case *apiXML.ScheduleResponse:
		for i := range data.SportEvents {
			tournament := data.SportEvents[i].Tournament
			result = append(result, tournament)
		}
	case *apiXML.TournamentScheduleResponse:
		result = append(result, data.Tournament)
	case *apiXML.SportTournamentsResponse:
		if data.Tournaments == nil {
			return
		}

		for i := range data.Tournaments.Tournament {
			tournament := data.Tournaments.Tournament[i]
			result = append(result, tournament)
		}
	}

	if len(result) == 0 {
		return
	}

	err := t.handleTournamentsData(*apiResponse.Locale, result)
	if err != nil {
		t.logger.WithError(err).Errorf("failed to precess api data %v", apiResponse)
	}
}

// ClearCacheItem ...
func (t *TournamentCache) ClearCacheItem(id protocols.URN) {
	t.internalCache.Delete(id.ToString())
}

// Tournament ...
func (t *TournamentCache) Tournament(id protocols.URN, locales []protocols.Locale) (*LocalizedTournament, error) {
	item, _ := t.internalCache.Get(id.ToString())
	result, ok := item.(*LocalizedTournament)

	var missingLocales []protocols.Locale
	if !ok {
		missingLocales = locales
	} else {
		for i := range locales {
			locale := locales[i]
			result.mux.Lock()
			_, ok := result.name[locale]
			result.mux.Unlock()

			if !ok {
				missingLocales = append(missingLocales, locale)
			}
		}
	}

	if len(missingLocales) != 0 {
		err := t.loadAndCacheItem(id, locales)
		if err != nil {
			return nil, err
		}

		item, _ = t.internalCache.Get(id.ToString())
		result, ok = item.(*LocalizedTournament)
		if !ok {
			return nil, errors.New("item missing")
		}
	}

	return result, nil
}

// TournamentCompetitors ...
func (t *TournamentCache) TournamentCompetitors(id protocols.URN, locale protocols.Locale) ([]protocols.URN, error) {
	item, _ := t.internalCache.Get(id.ToString())
	result, ok := item.(*LocalizedTournament)

	var competitorIDs map[protocols.URN]struct{}
	if ok && len(result.competitorIDs) != 0 {
		competitorIDs = result.competitorIDs
	} else {
		err := t.loadAndCacheItem(id, []protocols.Locale{locale})
		if err != nil {
			return nil, err
		}

		item, _ = t.internalCache.Get(id.ToString())
		result = item.(*LocalizedTournament)
		competitorIDs = result.competitorIDs
	}

	listIDs := make([]protocols.URN, len(competitorIDs))
	index := 0
	for key := range competitorIDs {
		listIDs[index] = key
		index++
	}

	return listIDs, nil
}

// TournamentIcon ...
func (t *TournamentCache) TournamentIcon(id protocols.URN, locale protocols.Locale) (*string, error) {
	icon, ok := t.iconCache.Get(id.ToString())
	if ok {
		return icon.(*string), nil
	}

	data, err := t.apiClient.FetchTournament(id, locale)
	if err != nil {
		return nil, err
	}

	t.iconCache.Set(id.ToString(), data.IconPath, 0)
	return data.IconPath, nil
}

func (t *TournamentCache) loadAndCacheItem(id protocols.URN, locales []protocols.Locale) error {
	for i := range locales {
		locale := locales[i]
		data, err := t.apiClient.FetchTournament(id, locale)
		if err != nil {
			return err
		}

		// Set icon to cache
		t.iconCache.Set(id.ToString(), data.IconPath, 0)

		err = t.refreshOrInsertItem(id, locale, data)
		if err != nil {
			return err
		}
	}

	return nil
}

func (t *TournamentCache) handleTournamentsData(locale protocols.Locale, tournaments []TournamentWrapper) error {
	for i := range tournaments {
		tournament := tournaments[i]
		id, err := protocols.ParseURN(tournament.GetID())
		if err != nil {
			return err
		}

		err = t.refreshOrInsertItem(*id, locale, tournament)
		if err != nil {
			return err
		}
	}

	return nil
}

func (t *TournamentCache) refreshOrInsertItem(id protocols.URN, locale protocols.Locale, tournament TournamentWrapper) error {
	item, _ := t.internalCache.Get(id.ToString())
	result, ok := item.(*LocalizedTournament)
	if !ok {
		sportID, err := protocols.ParseURN(tournament.GetSportID())
		if err != nil {
			return err
		}

		var refID *protocols.URN
		if tournament.GetRefID() != nil {
			refID, err = protocols.ParseURN(*tournament.GetRefID())
			if err != nil {
				return err
			}
		}

		result = &LocalizedTournament{
			id:               id,
			refID:            refID,
			startDate:        tournament.GetStartDate(),
			endDate:          tournament.GetEndDate(),
			sportID:          *sportID,
			scheduledTime:    tournament.GetScheduledTime(),
			scheduledEndTime: tournament.GetScheduledEndTime(),
			competitorIDs:    make(map[protocols.URN]struct{}),
			name:             make(map[protocols.Locale]string),
			abbreviation:     make(map[protocols.Locale]string),
		}
	}

	result.mux.Lock()
	defer result.mux.Unlock()

	result.name[locale] = tournament.GetName()
	result.abbreviation[locale] = tournament.GetAbbreviation()

	extendedTournament, ok := tournament.(TournamentExtendedWrapper)
	if ok {
		for _, team := range extendedTournament.GetCompetitors() {
			id, err := protocols.ParseURN(team.GetID())
			if err != nil {
				return err
			}
			result.competitorIDs[*id] = struct{}{}
		}
	}

	t.internalCache.Set(id.ToString(), result, 0)
	return nil
}

func newTournamentCache(client *api.Client, logger *log.Entry) *TournamentCache {
	tournamentCache := &TournamentCache{
		apiClient:     client,
		internalCache: cache.New(12*time.Hour, 10*time.Minute),
		iconCache:     cache.New(12*time.Hour, 10*time.Minute),
		logger:        logger,
	}

	client.SubscribeWithAPIObserver(tournamentCache)

	return tournamentCache
}

// LocalizedTournament ...
type LocalizedTournament struct {
	id               protocols.URN
	refID            *protocols.URN
	startDate        *time.Time
	endDate          *time.Time
	sportID          protocols.URN
	scheduledTime    *time.Time
	scheduledEndTime *time.Time
	name             map[protocols.Locale]string
	abbreviation     map[protocols.Locale]string
	competitorIDs    map[protocols.URN]struct{}

	mux sync.Mutex
}

type tournamentImpl struct {
	id              protocols.URN
	sportID         protocols.URN
	tournamentCache *TournamentCache
	entityFactory   protocols.EntityFactory
	locales         []protocols.Locale
}

func (t tournamentImpl) IconPath() (*string, error) {
	if len(t.locales) == 0 {
		return nil, fmt.Errorf("missing locales")
	}

	item, err := t.tournamentCache.TournamentIcon(t.id, t.locales[0])
	if err != nil {
		return nil, err
	}

	return item, nil
}

func (t tournamentImpl) ID() protocols.URN {
	return t.id
}

func (t tournamentImpl) RefID() (*protocols.URN, error) {
	item, err := t.tournamentCache.Tournament(t.id, t.locales)
	if err != nil {
		return nil, err
	}

	return item.refID, nil
}

func (t tournamentImpl) LocalizedAbbreviation(locale protocols.Locale) (*string, error) {
	item, err := t.tournamentCache.Tournament(t.id, []protocols.Locale{locale})
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

func (t tournamentImpl) LocalizedName(locale protocols.Locale) (*string, error) {
	item, err := t.tournamentCache.Tournament(t.id, []protocols.Locale{locale})
	if err != nil {
		return nil, err
	}

	item.mux.Lock()
	defer item.mux.Unlock()

	result, ok := item.name[locale]
	if !ok {
		return nil, fmt.Errorf("missing locale %s", locale)
	}

	return &result, nil
}

func (t tournamentImpl) SportID() (*protocols.URN, error) {
	return &t.sportID, nil
}

func (t tournamentImpl) ScheduledTime() (*time.Time, error) {
	item, err := t.tournamentCache.Tournament(t.id, t.locales)
	if err != nil {
		return nil, err
	}

	return item.scheduledTime, nil
}

func (t tournamentImpl) ScheduledEndTime() (*time.Time, error) {
	item, err := t.tournamentCache.Tournament(t.id, t.locales)
	if err != nil {
		return nil, err
	}

	return item.scheduledEndTime, nil
}

func (t tournamentImpl) LiveOddsAvailability() (*protocols.LiveOddsAvailability, error) {
	available := protocols.NotAvailableLiveOddsAvailability
	return &available, nil
}

func (t tournamentImpl) Sport() protocols.SportSummary {
	return t.entityFactory.BuildSport(t.sportID, t.locales)
}

func (t tournamentImpl) Competitors() ([]protocols.Competitor, error) {
	item, err := t.tournamentCache.Tournament(t.id, t.locales)
	if err != nil {
		return nil, err
	}

	var competitors []protocols.URN
	if len(item.competitorIDs) == 0 {
		competitors, err = t.tournamentCache.TournamentCompetitors(t.id, t.locales[0])
		if err != nil {
			return nil, err
		}
	} else {
		for key := range item.competitorIDs {
			competitors = append(competitors, key)
		}
	}

	return t.entityFactory.BuildCompetitors(competitors, t.locales), nil
}

func (t tournamentImpl) StartDate() (*time.Time, error) {
	item, err := t.tournamentCache.Tournament(t.id, t.locales)
	if err != nil {
		return nil, err
	}

	return item.startDate, nil
}

func (t tournamentImpl) EndDate() (*time.Time, error) {
	item, err := t.tournamentCache.Tournament(t.id, t.locales)
	if err != nil {
		return nil, err
	}

	return item.endDate, nil
}

// NewTournament ...
func NewTournament(id protocols.URN, sportID protocols.URN, tournamentCache *TournamentCache, entityFactory protocols.EntityFactory, locales []protocols.Locale) protocols.Tournament {
	return &tournamentImpl{
		id:              id,
		sportID:         sportID,
		tournamentCache: tournamentCache,
		entityFactory:   entityFactory,
		locales:         locales,
	}
}
