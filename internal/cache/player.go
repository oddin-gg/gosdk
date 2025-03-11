package cache

import (
	"fmt"
	"sync"
	"time"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/api/xml"
	"github.com/oddin-gg/gosdk/protocols"
	"github.com/patrickmn/go-cache"
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
		return nil, fmt.Errorf("get player from cache - player found for id: %s in player hash map: %w", id, ErrItemNotFoundInCache)
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

func newPlayersCache(apiClient *api.Client) *PlayersCache {
	return &PlayersCache{
		internalCache: cache.New(12*time.Hour, 1*time.Hour),
		apiClient:     apiClient,
	}
}

type LocalizedPlayer struct {
	ID            string
	LocalizedName string
	locale        protocols.Locale
}
