package cache

import (
	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/protocols"
	log "github.com/sirupsen/logrus"
)

// Manager ...
type Manager struct {
	MarketDescriptionCache     *MarketDescriptionCache
	CompetitorCache            *CompetitorCache
	SportDataCache             *SportCache
	FixtureCache               *FixtureCache
	TournamentCache            *TournamentCache
	MatchCache                 *MatchCache
	MatchStatusCache           *MatchStatusCache
	LocalizedStaticMatchStatus *LocalizedStaticDataCache
	PlayersCache               *PlayersCache
	logger                     *log.Entry
	MarketVoidReasonsCache     *MarketVoidReasonsCache
}

// OnFeedMessageReceived ...
func (m Manager) OnFeedMessageReceived(feedMessage *protocols.FeedMessage) {
	idMessage, ok := feedMessage.Message.(protocols.IDMessage)
	if !ok {
		return
	}

	id, err := protocols.ParseURN(idMessage.GetEventID())
	if err != nil {
		m.logger.Errorf("failed to parse id %s", idMessage.GetEventID())
		return
	}

	m.FixtureCache.OnFeedMessage(*id, feedMessage)
	m.MatchCache.OnFeedMessage(*id, feedMessage)
	m.TournamentCache.OnFeedMessage(*id, feedMessage)
	m.MatchStatusCache.OnFeedMessage(*id, feedMessage)
}

// Close ...
func (m Manager) Close() {
	m.LocalizedStaticMatchStatus.Close()
}

// NewManager ...
func NewManager(client *api.Client, oddsFeedConfiguration protocols.OddsFeedConfiguration, logger *log.Entry) *Manager {
	manager := &Manager{
		MarketDescriptionCache: newMarketDescriptionCache(client),
		CompetitorCache:        newCompetitorCache(client),
		SportDataCache:         newSportDataCache(client, logger),
		FixtureCache:           newFixtureCache(client),
		TournamentCache:        newTournamentCache(client, logger),
		MatchCache:             newMatchCache(client, logger),
		MatchStatusCache:       newMatchStatusCache(client, oddsFeedConfiguration, logger),
		MarketVoidReasonsCache: newMarketVoidReasonsCache(client),
		PlayersCache:           newPlayersCache(client),

		LocalizedStaticMatchStatus: newLocalizedStaticDataCache(oddsFeedConfiguration, func(locale protocols.Locale) ([]protocols.StaticData, error) {
			data, err := client.FetchMatchStatusDescriptions(locale)
			if err != nil {
				return nil, err
			}

			result := make([]protocols.StaticData, len(data))
			for i := range data {
				result[i] = data[i]
			}

			return result, nil
		}),
		logger: logger,
	}

	return manager
}
