package cache

import (
	"context"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/types"
	log "github.com/oddin-gg/gosdk/internal/log"
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
	logger                     *log.Logger
	MarketVoidReasonsCache     *MarketVoidReasonsCache
}

// OnFeedMessageReceived ...
func (m Manager) OnFeedMessageReceived(feedMessage *types.FeedMessage) {
	idMessage, ok := feedMessage.Message.(types.IDMessage)
	if !ok {
		return
	}

	id, err := types.ParseURN(idMessage.GetEventID())
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
func NewManager(client *api.Client, oddsFeedConfiguration types.OddsFeedConfiguration, logger *log.Logger) *Manager {
	manager := &Manager{
		MarketDescriptionCache: newMarketDescriptionCache(client),
		CompetitorCache:        newCompetitorCache(client, logger),
		SportDataCache:         newSportDataCache(client, logger),
		FixtureCache:           newFixtureCache(client),
		TournamentCache:        newTournamentCache(client, logger),
		MatchCache:             newMatchCache(client, logger),
		MatchStatusCache:       newMatchStatusCache(client, oddsFeedConfiguration, logger),
		MarketVoidReasonsCache: newMarketVoidReasonsCache(client),
		PlayersCache:           newPlayersCache(client, logger),

		LocalizedStaticMatchStatus: newLocalizedStaticDataCache(oddsFeedConfiguration, func(locale types.Locale) ([]types.StaticData, error) {
			data, err := client.FetchMatchStatusDescriptions(context.Background(), locale)
			if err != nil {
				return nil, err
			}

			result := make([]types.StaticData, len(data))
			for i := range data {
				d := data[i].GetDescription()
				result[i] = types.StaticData{
					ID:          data[i].GetID(),
					Description: d,
				}
			}

			return result, nil
		}),
		logger: logger,
	}

	return manager
}
