package cache

import (
	"fmt"
	"sync"
	"time"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/api/xml"
	"github.com/oddin-gg/gosdk/protocols"
	"github.com/patrickmn/go-cache"
	log "github.com/sirupsen/logrus"
)

// PlayerCacheKey represent cache key
type PlayerCacheKey struct {
	PlayerID string
	Locale   protocols.Locale
}

type PlayersCache struct {
	internalCache *cache.Cache
	apiClient     *api.Client
	mux           sync.Mutex
	logger        *log.Entry
}

// OnAPIResponse ...
func (c *PlayersCache) OnAPIResponse(apiResponse protocols.Response) {
	if apiResponse.Locale == nil || apiResponse.Data == nil {
		return
	}

	players := make([]xml.Player, 0)
	if data, ok := apiResponse.Data.(*xml.CompetitorResponse); ok {
		players = append(players, data.Players...)
	}

	if len(players) == 0 {
		return
	}

	err := c.handlePlayerData(*apiResponse.Locale, players)
	if err != nil {
		c.logger.WithError(err).Errorf("failed to process api data %v", apiResponse)
	}
}

// GetPlayer returns cached LocalizedPlayer if is in cache, if it is not then the team is fetched via api and stored in cache.
// If Player does not exist then ErrItemNotFoundInCache error is returned
func (c *PlayersCache) GetPlayer(id PlayerCacheKey) (*LocalizedPlayer, error) {
	players, err := c.GetPlayers([]PlayerCacheKey{id})
	switch {
	case err != nil:
		return nil, fmt.Errorf("get player from cache failed: %w", err)
	case len(players) == 0:
		return nil, fmt.Errorf("player %s not found: %w", id, ErrItemNotFoundInCache)
	case len(players) > 1:
		return nil, fmt.Errorf("get player from cache failed - more than one player found for id: %s", id)
	}

	player, found := players[id]
	if !found {
		return nil, fmt.Errorf("get player from cache - player found for id %q in player hash map: %w", id, ErrItemNotFoundInCache)
	}
	return &player, nil
}

// GetPlayers returns map of cached LocalizedPlayer if they are in cache, if any Player is missing then it is fetched via
// api and stored in cache.
func (c *PlayersCache) GetPlayers(ids []PlayerCacheKey) (map[PlayerCacheKey]LocalizedPlayer, error) {
	resultPlayers, missingPlayersIDs := c.getPlayersFromCache(ids)
	if len(missingPlayersIDs) == 0 {
		return resultPlayers, nil
	}

	// run just one api fetch
	c.mux.Lock()
	defer c.mux.Unlock()

	resultPlayers, missingPlayersIDs = c.getPlayersFromCache(ids)
	if len(missingPlayersIDs) == 0 {
		return resultPlayers, nil
	}

	dbPlayers, err := c.fetchPlayersFromAPI(missingPlayersIDs)
	if err != nil {
		return nil, fmt.Errorf("GetPlayers failed: %w", err)
	}

	for key, playerProfile := range dbPlayers {
		convertedPlayer := LocalizedPlayer{
			ID:            playerProfile.Player.ID,
			LocalizedName: playerProfile.Player.Name,
			FullName:      playerProfile.Player.FullName,
			SportID:       playerProfile.Player.SportID,
			locale:        key.Locale,
		}
		c.setPlayer(key, convertedPlayer)
	}

	resultPlayers, missingPlayersIDs = c.getPlayersFromCache(ids)
	if len(missingPlayersIDs) == 0 {
		return resultPlayers, nil
	}

	return nil, fmt.Errorf("get player from cache - some players %v not found in db: %w", missingPlayersIDs, ErrItemNotFoundInCache)
}

func (c *PlayersCache) handlePlayerData(locale protocols.Locale, players []xml.Player) error {
	for i := range players {
		err := c.refreshOrInsertItem(players[i], locale)
		if err != nil {
			return fmt.Errorf("refreshing or inserting player: %w", err)
		}
	}

	return nil
}

func (c *PlayersCache) refreshOrInsertItem(player xml.Player, locale protocols.Locale) error {
	key := PlayerCacheKey{PlayerID: player.ID, Locale: locale}

	result, ok := c.getPlayer(key)
	if !ok {
		result = LocalizedPlayer{}
	}

	result.ID = player.ID
	result.LocalizedName = player.Name
	result.FullName = player.FullName
	result.SportID = player.SportID
	result.locale = locale

	c.setPlayer(key, result)

	return nil
}

func (c *PlayersCache) getPlayersFromCache(
	ids []PlayerCacheKey,
) (map[PlayerCacheKey]LocalizedPlayer, []PlayerCacheKey) {
	var missingPlayersIDs []PlayerCacheKey
	foundPlayers := make(map[PlayerCacheKey]LocalizedPlayer, len(ids))

	for _, id := range ids {
		if res, found := c.getPlayer(id); found {
			foundPlayers[id] = res
		} else {
			missingPlayersIDs = append(missingPlayersIDs, id)
		}
	}
	return foundPlayers, missingPlayersIDs
}

func (c *PlayersCache) fetchPlayersFromAPI(keys []PlayerCacheKey) (map[PlayerCacheKey]xml.PlayerProfile, error) {
	res := make(map[PlayerCacheKey]xml.PlayerProfile, len(keys))

	for _, key := range keys {
		data, err := c.apiClient.FetchPlayerProfile(key.PlayerID, key.Locale)

		if err != nil {
			return nil, fmt.Errorf("fetch player profiles failed: %w", err)
		}
		if data == nil {
			continue
		}
		res[key] = *data
	}

	return res, nil
}

func (c *PlayersCache) key(id PlayerCacheKey) string {
	return id.PlayerID + ":" + string(id.Locale)
}

func (c *PlayersCache) getPlayer(id PlayerCacheKey) (LocalizedPlayer, bool) {
	res, found := c.internalCache.Get(c.key(id))
	if !found {
		return LocalizedPlayer{}, found
	}
	return res.(LocalizedPlayer), found
}

func (c *PlayersCache) setPlayer(id PlayerCacheKey, obj LocalizedPlayer) {
	c.internalCache.Set(c.key(id), obj, cache.DefaultExpiration)
}

func newPlayersCache(apiClient *api.Client, logger *log.Entry) *PlayersCache {
	playersCache := &PlayersCache{
		internalCache: cache.New(12*time.Hour, 1*time.Hour),
		apiClient:     apiClient,
		logger:        logger,
	}

	apiClient.SubscribeWithAPIObserver(playersCache)

	return playersCache
}

type LocalizedPlayer struct {
	ID            string
	LocalizedName string
	FullName      string
	SportID       string
	locale        protocols.Locale
}

type playerImpl struct {
	key         PlayerCacheKey
	playerCache *PlayersCache
}

func (p playerImpl) ID() (string, error) {
	item, err := p.playerCache.GetPlayer(p.key)
	if err != nil {
		return "", fmt.Errorf("getting player from cache: %w", err)
	}

	return item.ID, nil
}

func (p playerImpl) LocalizedName() (string, error) {
	item, err := p.playerCache.GetPlayer(p.key)
	if err != nil {
		return "", fmt.Errorf("getting player from cache: %w", err)
	}

	return item.LocalizedName, nil
}

func (p playerImpl) FullName() (string, error) {
	item, err := p.playerCache.GetPlayer(p.key)
	if err != nil {
		return "", fmt.Errorf("getting player from cache: %w", err)
	}

	return item.FullName, nil
}

func (p playerImpl) SportID() (string, error) {
	item, err := p.playerCache.GetPlayer(p.key)
	if err != nil {
		return "", fmt.Errorf("getting player from cache: %w", err)
	}

	return item.SportID, nil
}

// NewPlayer ...
func NewPlayer(id protocols.URN, playerCache *PlayersCache, locale protocols.Locale) protocols.Player {
	key := PlayerCacheKey{PlayerID: id.ToString(), Locale: locale}

	return &playerImpl{
		key:         key,
		playerCache: playerCache,
	}
}
