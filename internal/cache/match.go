package cache

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/oddin-gg/gosdk/internal/api"
	apiXML "github.com/oddin-gg/gosdk/internal/api/xml"
	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	"github.com/oddin-gg/gosdk/internal/utils"
	"github.com/oddin-gg/gosdk/protocols"
	"github.com/patrickmn/go-cache"
	log "github.com/sirupsen/logrus"
)

// MatchCache ...
type MatchCache struct {
	apiClient     *api.Client
	internalCache *cache.Cache
	logger        *log.Entry
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
		m.logger.WithError(err).Errorf("failed to process api response %v", apiResponse)
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

	result.sportFormat = protocols.SportFormatClassic
	result.extraInfo = make(map[string]string)
	if match.ExtraInfo.List != nil {
		for _, info := range match.ExtraInfo.List {
			if info.Key == apiXML.ExtraInfoSportFormatKey && len(info.Value) > 0 {
				switch info.Value {
				case protocols.SportFormatRace:
					result.sportFormat = protocols.SportFormatRace
				case protocols.SportFormatClassic:
					result.sportFormat = protocols.SportFormatClassic
				default:
					return fmt.Errorf("unknown sport format for match %s: %s", match.ID, info.Value)
				}
			}
			result.extraInfo[info.Key] = info.Value
		}
	}

	var competitors []competitor
	if match.Competitors != nil && len(match.Competitors.Competitor) > 0 {
		for _, c := range match.Competitors.Competitor {
			urn, err := protocols.ParseURN(c.ID)
			switch {
			case err != nil:
				return err
			case urn == nil:
				return fmt.Errorf("invalid or empty urn: %s", c.ID)
			}

			competitors = append(competitors, competitor{
				urn:       *urn,
				qualifier: c.Qualifier,
			})
		}
	}
	result.competitors = competitors

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

func newMatchCache(client *api.Client, logger *log.Entry) *MatchCache {
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
	competitors          []competitor
	liveOddsAvailability *protocols.LiveOddsAvailability
	name                 map[protocols.Locale]string
	sportFormat          protocols.SportFormat
	extraInfo            map[string]string

	mux sync.Mutex
}

type competitor struct {
	urn       protocols.URN
	qualifier string
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

// Deprecated: do not use this method, it will be removed in future
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
		return nil, fmt.Errorf("missing locale %s", locale)
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
	item, err := m.matchCache.Match(m.id, m.locales)
	if err != nil {
		return nil, err
	}

	if len(item.competitors) < 2 {
		return nil, fmt.Errorf("match %s has less than 2 competitors", m.id.ToString())
	}

	competitors := make([]protocols.Competitor, 0, len(item.competitors))
	for _, team := range item.competitors {
		competitors = append(competitors, teamCompetitorImpl{
			qualifier:  &team.qualifier,
			competitor: m.entityFactory.BuildCompetitor(team.urn, m.locales),
		})
	}

	return competitors, nil
}

func (m matchImpl) SportFormat() (protocols.SportFormat, error) {
	item, err := m.matchCache.Match(m.id, m.locales)
	if err != nil {
		return protocols.SportFormatUnknown, err
	}

	return item.sportFormat, nil
}

func (m matchImpl) ExtraInfo() (map[string]string, error) {
	item, err := m.matchCache.Match(m.id, m.locales)
	if err != nil {
		return nil, err
	}

	return item.extraInfo, nil
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

	var team competitor

	switch {
	case len(item.competitors) < 2:
		return nil, fmt.Errorf("match %s has less than 2 competitors", m.id.ToString())
	case item.sportFormat != protocols.SportFormatClassic:
		return nil, fmt.Errorf("match %s is not classic sport format", m.id.ToString())
	case len(item.competitors) > 2:
		return nil, fmt.Errorf("classic sport match %s has more than 2 competitors", m.id.ToString())
	default:
		team = item.competitors[0]
	}

	return teamCompetitorImpl{
		qualifier:  &team.qualifier,
		competitor: m.entityFactory.BuildCompetitor(team.urn, m.locales),
	}, nil
}

func (m matchImpl) AwayCompetitor() (protocols.TeamCompetitor, error) {
	item, err := m.matchCache.Match(m.id, m.locales)
	if err != nil {
		return nil, err
	}

	var team competitor

	switch {
	case len(item.competitors) < 2:
		return nil, fmt.Errorf("match %s has less than 2 competitors", m.id.ToString())
	case item.sportFormat != protocols.SportFormatClassic:
		return nil, fmt.Errorf("match %s is not classic sport format", m.id.ToString())
	case len(item.competitors) > 2:
		return nil, fmt.Errorf("classic sport match %s has more than 2 competitors", m.id.ToString())
	default:
		team = item.competitors[1]
	}

	return teamCompetitorImpl{
		qualifier:  &team.qualifier,
		competitor: m.entityFactory.BuildCompetitor(team.urn, m.locales),
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
