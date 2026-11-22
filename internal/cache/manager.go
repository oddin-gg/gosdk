package cache

import (
	"context"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/cache/lru"
	"github.com/oddin-gg/gosdk/internal/config"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
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

	// life is the one lifetime owner of every cache: its context is the
	// detach root of their singleflight loads, and every store commits
	// inside it. Close ends it, so no detached fetch (including
	// construction-time preloads) outlives the manager and no cache
	// changes once Close has returned — NEXT.md §8: failed construction
	// must not leave background work running.
	life *lru.Lifetime
}

// OnFeedMessageReceived ...
func (m Manager) OnFeedMessageReceived(feedMessage *types.FeedMessage) {
	idm, ok := feedMessage.Message.(idMessage)
	if !ok {
		return
	}

	id, err := types.ParseURN(idm.GetEventID())
	if err != nil {
		m.logger.WithError(err).Errorf("OnFeedMessage: parse event id %q", idm.GetEventID())
		return
	}

	m.FixtureCache.OnFeedMessage(*id, feedMessage)
	m.MatchCache.OnFeedMessage(*id, feedMessage)
	m.TournamentCache.OnFeedMessage(*id, feedMessage)
	m.MatchStatusCache.OnFeedMessage(*id, feedMessage)
}

// Close is CloseCtx with an unbounded join. Prefer CloseCtx from paths
// that carry a shutdown budget.
func (m Manager) Close() {
	m.CloseCtx(context.Background())
}

// CloseCtx tears the caches down with the refresh-goroutine join
// BOUNDED by ctx. The localized-static refresh performs synchronous
// fetch I/O; with a cancellation-ignoring custom transport an
// unbounded join could wedge root shutdown forever (c.closed never
// published, every later Close timing out). Reports whether the join
// completed.
func (m Manager) CloseCtx(ctx context.Context) bool {
	// End the shared lifetime FIRST: in-flight detached loads abort (their
	// HTTP requests are cancelled), and every commit that is running has
	// finished when End returns; a flight that ends later stores nothing.
	// End waits for no flight, only for those in-memory commits.
	m.life.End()
	return m.LocalizedStaticMatchStatus.CloseCtx(ctx)
}

// NewManager constructs the cache manager. ctx becomes the lifecycle
// root for caches that run periodic-refresh goroutines (e.g. the
// localized static data cache) and the detach root for every cache's
// singleflight loads. The caches outlive ctx's cancellation-by-deadline
// only in the sense that Close() is the canonical shutdown signal: the
// manager derives its own lifetime ctx via WithoutCancel(ctx) +
// WithCancel, and Close cancels it — aborting any in-flight detached
// load so no fetch outlives the manager.
//
// preloadLocales is the optional list of locales (in addition to the
// configured default) that static-catalog caches should fetch eagerly
// and keep refreshed.
func NewManager(ctx context.Context, client *api.Client, oddsFeedConfiguration config.Config, logger *log.Logger, preloadLocales []types.Locale) *Manager {
	life := lru.NewLifetime(ctx)
	manager := &Manager{
		MarketDescriptionCache: newMarketDescriptionCache(life, client, logger),
		CompetitorCache:        newCompetitorCache(life, client, logger),
		SportDataCache:         newSportDataCache(life, client, logger),
		FixtureCache:           newFixtureCache(life, client),
		TournamentCache:        newTournamentCache(life, client, logger),
		MatchCache:             newMatchCache(life, client, logger),
		MatchStatusCache:       newMatchStatusCache(life, client, oddsFeedConfiguration, logger),
		MarketVoidReasonsCache: newMarketVoidReasonsCache(life, client),
		PlayersCache:           newPlayersCache(life, client, logger),
		life:                   life,

		LocalizedStaticMatchStatus: newLocalizedStaticDataCache(life, oddsFeedConfiguration, logger, preloadLocales, func(ctx context.Context, locale types.Locale) ([]types.StaticData, error) { //nolint:contextcheck // deliberate detach: the refresh runs under the cache lifetime, not a caller's context
			data, err := client.FetchMatchStatusDescriptions(ctx, locale)
			if err != nil {
				return nil, err
			}

			result := make([]types.StaticData, len(data))
			for i := range data {
				result[i] = types.StaticData{
					ID: data[i].GetID(),
					// data[i] is the API XML type — its
					// GetDescription returns *string. Bridge to
					// Optional[string] at the boundary.
					Description: types.FromPtr(data[i].GetDescription()),
				}
			}

			return result, nil
		}),
		logger: logger,
	}

	return manager
}
