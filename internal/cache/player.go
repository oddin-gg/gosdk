package cache

import (
	"context"
	"fmt"
	"sync"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/api/xml"
	"github.com/oddin-gg/gosdk/protocols"
	log "github.com/sirupsen/logrus"
)

// PlayerCacheKey represent cache key
type PlayerCacheKey struct {
	PlayerID string
	Locale   protocols.Locale
}

// PlayersCache stores player profiles keyed by (playerID, locale).
//
// Phase 3 rewrite: replaces patrickmn/go-cache with a sync.RWMutex map.
// Player data is flat per (id, locale) — no per-locale subfields, so the
// EventCache-with-fill-in primitive isn't a fit; a simple map with a
// per-key load mutex is enough. Loader takes ctx; previously synthesized
// context.Background() at the boundary.
type PlayersCache struct {
	apiClient *api.Client
	logger    *log.Entry

	mu      sync.RWMutex
	players map[PlayerCacheKey]LocalizedPlayer
	loadMu  sync.Mutex // serializes API fetches across concurrent GetPlayers callers
}

// LocalizedPlayer is the cached player profile.
type LocalizedPlayer struct {
	ID            string
	LocalizedName string
	FullName      string
	SportID       string
	locale        protocols.Locale
}

// GetPlayer returns a single cached LocalizedPlayer, fetching if missing.
func (c *PlayersCache) GetPlayer(ctx context.Context, id PlayerCacheKey) (*LocalizedPlayer, error) {
	players, err := c.GetPlayers(ctx, []PlayerCacheKey{id})
	if err != nil {
		return nil, fmt.Errorf("get player from cache failed: %w", err)
	}
	p, ok := players[id]
	if !ok {
		return nil, fmt.Errorf("player %s not found: %w", id, ErrItemNotFoundInCache)
	}
	return &p, nil
}

// GetPlayers returns a map of cached LocalizedPlayer values, fetching any
// missing ones from the API. Concurrent callers serialize on loadMu so
// duplicate fetches for the same key are avoided.
func (c *PlayersCache) GetPlayers(ctx context.Context, ids []PlayerCacheKey) (map[PlayerCacheKey]LocalizedPlayer, error) {
	result, missing := c.snapshot(ids)
	if len(missing) == 0 {
		return result, nil
	}

	c.loadMu.Lock()
	defer c.loadMu.Unlock()

	// Re-snapshot inside the load lock — another goroutine may have just filled.
	result, missing = c.snapshot(ids)
	if len(missing) == 0 {
		return result, nil
	}

	for _, key := range missing {
		data, err := c.apiClient.FetchPlayerProfile(ctx, key.PlayerID, key.Locale)
		if err != nil {
			return nil, fmt.Errorf("fetch player profile %s/%s: %w", key.PlayerID, key.Locale, err)
		}
		if data == nil {
			continue
		}
		c.set(key, LocalizedPlayer{
			ID:            data.Player.ID,
			LocalizedName: data.Player.Name,
			FullName:      data.Player.FullName,
			SportID:       data.Player.SportID,
			locale:        key.Locale,
		})
	}

	result, missing = c.snapshot(ids)
	if len(missing) == 0 {
		return result, nil
	}
	return nil, fmt.Errorf("get player from cache - some players %v not found: %w", missing, ErrItemNotFoundInCache)
}

// Clear evicts the cache entry for the given (id, locale).
func (c *PlayersCache) Clear(id PlayerCacheKey) {
	c.mu.Lock()
	delete(c.players, id)
	c.mu.Unlock()
}

// ClearByID evicts every entry for the player ID across all locales.
func (c *PlayersCache) ClearByID(playerID string) {
	c.mu.Lock()
	for k := range c.players {
		if k.PlayerID == playerID {
			delete(c.players, k)
		}
	}
	c.mu.Unlock()
}

// Purge clears the entire cache.
func (c *PlayersCache) Purge() {
	c.mu.Lock()
	c.players = make(map[PlayerCacheKey]LocalizedPlayer)
	c.mu.Unlock()
}

func (c *PlayersCache) snapshot(ids []PlayerCacheKey) (map[PlayerCacheKey]LocalizedPlayer, []PlayerCacheKey) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	found := make(map[PlayerCacheKey]LocalizedPlayer, len(ids))
	var missing []PlayerCacheKey
	for _, id := range ids {
		if v, ok := c.players[id]; ok {
			found[id] = v
		} else {
			missing = append(missing, id)
		}
	}
	return found, missing
}

func (c *PlayersCache) set(id PlayerCacheKey, p LocalizedPlayer) {
	c.mu.Lock()
	c.players[id] = p
	c.mu.Unlock()
}

// MergePlayers folds an XML.Player slice into the cache (used by code paths
// that already fetched a parent entity and want to pre-populate players).
func (c *PlayersCache) MergePlayers(locale protocols.Locale, players []xml.Player) {
	for _, p := range players {
		key := PlayerCacheKey{PlayerID: p.ID, Locale: locale}
		c.set(key, LocalizedPlayer{
			ID:            p.ID,
			LocalizedName: p.Name,
			FullName:      p.FullName,
			SportID:       p.SportID,
			locale:        locale,
		})
	}
}

func newPlayersCache(apiClient *api.Client, logger *log.Entry) *PlayersCache {
	return &PlayersCache{
		apiClient: apiClient,
		logger:    logger,
		players:   make(map[PlayerCacheKey]LocalizedPlayer),
	}
}

// playerImpl satisfies protocols.Player.
type playerImpl struct {
	key         PlayerCacheKey
	playerCache *PlayersCache
}

func (p playerImpl) ID() string { return p.key.PlayerID }

func (p playerImpl) LocalizedName() (string, error) {
	item, err := p.playerCache.GetPlayer(context.Background(), p.key)
	if err != nil {
		return "", fmt.Errorf("getting player from cache: %w", err)
	}
	return item.LocalizedName, nil
}

func (p playerImpl) FullName() (string, error) {
	item, err := p.playerCache.GetPlayer(context.Background(), p.key)
	if err != nil {
		return "", fmt.Errorf("getting player from cache: %w", err)
	}
	return item.FullName, nil
}

func (p playerImpl) SportID() (string, error) {
	item, err := p.playerCache.GetPlayer(context.Background(), p.key)
	if err != nil {
		return "", fmt.Errorf("getting player from cache: %w", err)
	}
	return item.SportID, nil
}

// NewPlayer ...
func NewPlayer(id protocols.URN, playerCache *PlayersCache, locale protocols.Locale) protocols.Player {
	return &playerImpl{
		key:         PlayerCacheKey{PlayerID: id.ToString(), Locale: locale},
		playerCache: playerCache,
	}
}
