package cache

import (
	"sync"
	"time"

	"github.com/oddin-gg/gosdk/internal/api"
	apiXML "github.com/oddin-gg/gosdk/internal/api/xml"
	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	"github.com/oddin-gg/gosdk/internal/utils"
	"github.com/oddin-gg/gosdk/protocols"
	"github.com/patrickmn/go-cache"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

// MatchCache ...
type MatchCache struct {
	apiClient     *api.Client
	internalCache *cache.Cache
	logger        *log.Logger
}

// OnAPIResponse ...
func (m *MatchCache) OnAPIResponse(apiResponse protocols.Response) {
	if apiResponse.Locale == nil || apiResponse.Data == nil {
		return
	}

	var events []apiXML.SportEvent
	switch data := apiResponse.Data.(type) {
	case *apiXML.FixtureResponse:
		events = append(events, data.Fixture.SportEvent)
	case *apiXML.ScheduleResponse:
		events = data.SportEvents
	case *apiXML.TournamentScheduleResponse:
		events = data.SportEvents.List
	}

	if len(events) == 0 {
		return
	}

	err := m.handleMatchData(*apiResponse.Locale, events)
	if err != nil {
		m.logger.WithError(err).Errorf("failed to process api response %s", apiResponse)
	}
}

// OnFeedMessage ...
func (m *MatchCache) OnFeedMessage(id protocols.URN, feedMessage *protocols.FeedMessage) {
	if feedMessage.Message == nil {
		return
	}

	_, ok := feedMessage.Message.(*feedXML.FixtureChange)
	if !ok || id.Type != "match" {
		return
	}

	m.ClearCacheItem(id)
}

// ClearCacheItem ...
func (m *MatchCache) ClearCacheItem(id protocols.URN) {
	m.internalCache.Delete(id.ToString())
}

// Match ...
func (m *MatchCache) Match(id protocols.URN, locales []protocols.Locale) (*LocalizedMatch, error) {
	item, _ := m.internalCache.Get(id.ToString())
	result, ok := item.(*LocalizedMatch)

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
		err := m.loadAndCacheItem(id, locales)
		if err != nil {
			return nil, err
		}

		item, _ = m.internalCache.Get(id.ToString())
		result, ok = item.(*LocalizedMatch)
		if !ok {
			return nil, errors.New("item missing")
		}
	}

	return result, nil
}

func (m *MatchCache) loadAndCacheItem(id protocols.URN, locales []protocols.Locale) error {
	for i := range locales {
		locale := locales[i]
		data, err := m.apiClient.FetchMatchSummary(id, locale)
		if err != nil {
			return err
		}

		err = m.refreshOrInsertItem(id, locale, data.SportEvent)
		if err != nil {
			return err
		}
	}

	return nil
}

func (m *MatchCache) handleMatchData(locale protocols.Locale, matches []apiXML.SportEvent) error {
	for i := range matches {
		id, err := protocols.ParseURN(matches[i].ID)
		if err != nil {
			return err
		}

		err = m.refreshOrInsertItem(*id, locale, matches[i])
		if err != nil {
			return err
		}
	}

	return nil
}

func (m *MatchCache) refreshOrInsertItem(id protocols.URN, locale protocols.Locale, match apiXML.SportEvent) error {
	var homeTeamID *string
	var homeTeamQualifier *string

	var awayTeamID *string
	var awayTeamQualifier *string

	if match.Competitors != nil && len(match.Competitors.Competitor) == 2 {
		homeTeamID = &match.Competitors.Competitor[0].ID
		homeTeamQualifier = &match.Competitors.Competitor[0].Qualifier

		awayTeamID = &match.Competitors.Competitor[1].ID
		awayTeamQualifier = &match.Competitors.Competitor[1].Qualifier
	}

	item, _ := m.internalCache.Get(id.ToString())
	result, ok := item.(*LocalizedMatch)

	if !ok {
		refID, err := m.unwrapURN(match.RefID)
		if err != nil {
			return err
		}

		result = &LocalizedMatch{
			id:    id,
			refID: refID,
			name:  make(map[protocols.Locale]string),
		}
	}

	tournamentID, err := m.unwrapURN(&match.Tournament.ID)
	if err != nil {
		return err
	}
	result.tournamentID = *tournamentID

	sportID, err := m.unwrapURN(&match.Tournament.Sport.ID)
	if err != nil {
		return err
	}
	result.sportID = *sportID

	homeTeamURN, err := m.unwrapURN(homeTeamID)
	if err != nil {
		return err
	}
	result.homeTeamID = homeTeamURN

	awayTeamURN, err := m.unwrapURN(awayTeamID)
	if err != nil {
		return err
	}
	result.awayTeamID = awayTeamURN

	result.homeTeamQualifier = homeTeamQualifier
	result.awayTeamQualifier = awayTeamQualifier

	var liveOdds protocols.LiveOddsAvailability
	switch match.LiveOdds {
	case apiXML.LiveOddsNotAvailable:
		liveOdds = protocols.NotAvailableLiveOddsAvailability
	default:
		liveOdds = protocols.AvailableLiveOddsAvailability
	}

	result.liveOddsAvailability = &liveOdds

	scheduledTime, err := m.unwrapTime(match.Scheduled)
	if err != nil {
		return err
	}
	result.scheduledTime = scheduledTime

	scheduledEndTime, err := m.unwrapTime(match.ScheduledEnd)
	if err != nil {
		return err
	}
	result.scheduledEndTime = scheduledEndTime

	result.mux.Lock()
	defer result.mux.Unlock()

	result.name[locale] = match.Name

	m.internalCache.Set(id.ToString(), result, 0)

	return nil
}

func (m *MatchCache) unwrapURN(id *string) (*protocols.URN, error) {
	if id == nil {
		return nil, nil
	}

	return protocols.ParseURN(*id)
}

func (m *MatchCache) unwrapTime(dateTime *utils.DateTime) (*time.Time, error) {
	if dateTime == nil {
		return nil, nil
	}

	parsed := (time.Time)(*dateTime)
	return &parsed, nil
}

func newMatchCache(client *api.Client, logger *log.Logger) *MatchCache {
	matchCache := &MatchCache{
		apiClient:     client,
		internalCache: cache.New(12*time.Hour, 10*time.Minute),
		logger:        logger,
	}

	client.SubscribeWithAPIObserver(matchCache)

	return matchCache
}

// LocalizedMatch ...
type LocalizedMatch struct {
	id                   protocols.URN
	refID                *protocols.URN
	scheduledTime        *time.Time
	scheduledEndTime     *time.Time
	sportID              protocols.URN
	tournamentID         protocols.URN
	homeTeamID           *protocols.URN
	awayTeamID           *protocols.URN
	homeTeamQualifier    *string
	awayTeamQualifier    *string
	liveOddsAvailability *protocols.LiveOddsAvailability
	name                 map[protocols.Locale]string
	mux                  sync.Mutex
}

type matchImpl struct {
	id            protocols.URN
	localSportID  *protocols.URN
	matchCache    *MatchCache
	entityFactory protocols.EntityFactory
	locales       []protocols.Locale
}

func (m matchImpl) ID() protocols.URN {
	return m.id
}

func (m matchImpl) RefID() (*protocols.URN, error) {
	item, err := m.matchCache.Match(m.id, m.locales)
	if err != nil {
		return nil, err
	}

	return item.refID, nil
}

func (m matchImpl) LocalizedName(locale protocols.Locale) (*string, error) {
	item, err := m.matchCache.Match(m.id, m.locales)
	if err != nil {
		return nil, err
	}

	item.mux.Lock()
	defer item.mux.Unlock()

	name, ok := item.name[locale]
	if !ok {
		return nil, errors.Errorf("missing locale %s", locale)
	}

	return &name, nil
}

func (m matchImpl) SportID() (*protocols.URN, error) {
	if m.localSportID != nil {
		return m.localSportID, nil
	}

	item, err := m.matchCache.Match(m.id, m.locales)
	if err != nil {
		return nil, err
	}

	m.localSportID = &item.sportID
	return m.localSportID, nil
}

func (m matchImpl) ScheduledTime() (*time.Time, error) {
	item, err := m.matchCache.Match(m.id, m.locales)
	if err != nil {
		return nil, err
	}

	return item.scheduledTime, nil
}

func (m matchImpl) ScheduledEndTime() (*time.Time, error) {
	item, err := m.matchCache.Match(m.id, m.locales)
	if err != nil {
		return nil, err
	}

	return item.scheduledEndTime, nil
}

func (m matchImpl) LiveOddsAvailability() (*protocols.LiveOddsAvailability, error) {
	item, err := m.matchCache.Match(m.id, m.locales)
	if err != nil {
		return nil, err
	}

	return item.liveOddsAvailability, nil
}

func (m matchImpl) Competitors() ([]protocols.Competitor, error) {
	home, err := m.HomeCompetitor()
	if err != nil {
		return nil, err
	}

	away, err := m.AwayCompetitor()
	if err != nil {
		return nil, err
	}

	return []protocols.Competitor{home, away}, nil
}

func (m matchImpl) Status() protocols.MatchStatus {
	return m.entityFactory.BuildMatchStatus(m.id, m.locales)
}

func (m matchImpl) Tournament() (protocols.Tournament, error) {
	sportID, err := m.SportID()
	if err != nil {
		return nil, err
	}

	item, err := m.matchCache.Match(m.id, m.locales)
	if err != nil {
		return nil, err
	}

	return m.entityFactory.BuildTournament(item.tournamentID, *sportID, m.locales), nil
}

func (m matchImpl) HomeCompetitor() (protocols.TeamCompetitor, error) {
	item, err := m.matchCache.Match(m.id, m.locales)
	if err != nil {
		return nil, err
	}

	if item.homeTeamID == nil {
		return nil, errors.New("missing home team id")
	}

	competitor := m.entityFactory.BuildCompetitor(*item.homeTeamID, m.locales)
	return teamCompetitorImpl{
		qualifier:  item.homeTeamQualifier,
		competitor: competitor,
	}, nil
}

func (m matchImpl) AwayCompetitor() (protocols.TeamCompetitor, error) {
	item, err := m.matchCache.Match(m.id, m.locales)
	if err != nil {
		return nil, err
	}

	if item.homeTeamID == nil {
		return nil, errors.New("missing home team id")
	}

	competitor := m.entityFactory.BuildCompetitor(*item.awayTeamID, m.locales)
	return teamCompetitorImpl{
		qualifier:  item.awayTeamQualifier,
		competitor: competitor,
	}, nil
}

func (m matchImpl) Fixture() protocols.Fixture {
	return m.entityFactory.BuildFixture(m.id, m.locales)
}

// NewMatch ...
func NewMatch(id protocols.URN, sportID *protocols.URN, matchCache *MatchCache, entityFactory protocols.EntityFactory, locales []protocols.Locale) protocols.Match {
	return &matchImpl{
		id:            id,
		localSportID:  sportID,
		matchCache:    matchCache,
		entityFactory: entityFactory,
		locales:       locales,
	}
}
