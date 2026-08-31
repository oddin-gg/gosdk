package cache

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/cache/lru"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

// PlayerCacheKey identifies a (player, locale) entry in the cache.
type PlayerCacheKey struct {
	PlayerID string
	Locale   types.Locale
}

// String renders the key as "<playerID>/<locale>" for log + error messages.
// Without this, fmt.Errorf("%v", key) emits the raw struct dump
// "{od:player:42 en}" which is hard to grep for.
func (k PlayerCacheKey) String() string { return k.PlayerID + "/" + string(k.Locale) }

// playersCacheSize bounds the player cache. Player profiles form a long
// tail (one entry per (playerID, locale), fed by competitor rosters), so
// the cap is above the per-event default; eviction is safe — a missing
// player is re-fetched on next access.
const playersCacheSize = 10000

// PlayersCache stores types.Player snapshots keyed by (id, locale).
//
// Player data is flat per (id, locale) — no per-locale subfields, so the
// EventCache fill-in primitive isn't a fit; a bounded expirable LRU with
// singleflight-deduplicated loads is enough. Previously a plain map:
// one entry per (player, locale) ever seen, never evicted — one of the
// unbounded-growth paths behind the "SDK cache taking too much space"
// consumer report.
//
// Phase 6 reshape: now stores types.Player (value struct) directly
// instead of an internal LocalizedPlayer wrapper.
type PlayersCache struct {
	apiClient *api.Client
	logger    *log.Logger
	// lifetime is the detach root for singleflight profile loads —
	// cancelled by Manager.Close so no fetch outlives the owner.
	lifetime context.Context

	players *lru.TTL[PlayerCacheKey, types.Player]
	// loadGroup deduplicates concurrent fetches of the same
	// PlayerCacheKey (one in-flight FetchPlayerProfile call per
	// key). Pre-v2.24 a global loadMu serialised every miss across
	// every key, so unrelated player IDs / locales couldn't load
	// concurrently — a 4-locale preload of N players ran 4N
	// sequential HTTP calls instead of N parallel batches.
	loadGroup singleflight.Group

	// clearMu + clearedIDsAt + purgedAt: the clear-vs-in-flight-load
	// tombstone (same invariant as lru.EventCache). A loader snapshots
	// time.Now() before fetching; the store — mutually exclusive with
	// Clear under clearMu — is skipped when a Clear for THAT player id
	// (or a Purge) landed after the snapshot, so Clear/ClearByID cannot
	// be silently undone by a fetch that started before them. Keyed by
	// PlayerID rather than (id, locale): ClearByID must also suppress
	// in-flight loads for locales not yet present in the cache, and
	// over-suppressing a sibling locale of the SAME player costs one
	// refetch while never disturbing other players. The map is pruned
	// once entries are older than any possible in-flight fetch.
	clearMu      sync.Mutex
	clearedIDsAt map[string]time.Time
	purgedAt     time.Time

	// flightGen is folded into every singleflight key. Tombstones stop
	// a pre-clear flight from REPOPULATING the cache, but without a
	// generation a caller arriving AFTER the clear could still JOIN
	// that flight and be handed its pre-clear result directly.
	// Advancing the generation on every Clear/ClearByID/Purge detaches
	// all future callers from all in-flight loads (deliberately
	// coarse: clears are rare, and the cost of an early detach is one
	// duplicate fetch — never staleness).
	flightGen atomic.Uint64
}

// playerTombstonePruneLen / MaxAge bound the tombstone map (see the
// matching constants on the other caches).
const (
	playerTombstonePruneLen = 1024
	playerTombstoneMaxAge   = 2 * time.Minute
)

// storeIfNotCleared admits a loaded player unless a Clear for its id
// (or a Purge) landed after the load started. Returns false when the
// store was suppressed.
func (c *PlayersCache) storeIfNotCleared(key PlayerCacheKey, p types.Player, loadStarted time.Time) bool {
	c.clearMu.Lock()
	defer c.clearMu.Unlock()
	if c.clearedIDsAt[key.PlayerID].After(loadStarted) || c.purgedAt.After(loadStarted) {
		return false
	}
	c.players.Add(key, p)
	return true
}

// markCleared records the per-player tombstone; callers must follow
// with the actual removal while still holding clearMu.
func (c *PlayersCache) markCleared(playerID string) {
	c.flightGen.Add(1) // detach future callers from in-flight loads
	now := time.Now()
	c.clearedIDsAt[playerID] = now
	if len(c.clearedIDsAt) > playerTombstonePruneLen {
		cutoff := now.Add(-playerTombstoneMaxAge)
		for k, t := range c.clearedIDsAt {
			if t.Before(cutoff) {
				delete(c.clearedIDsAt, k)
			}
		}
	}
}

// GetPlayer returns a single cached Player, fetching if missing.
func (c *PlayersCache) GetPlayer(ctx context.Context, id PlayerCacheKey) (*types.Player, error) {
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

// playerLoadConcurrency bounds the per-call fan-out of GetPlayers'
// missing-key loads (and the per-locale player resolution in the
// competitor snapshot). Cold rosters are the hot case: a 2×10-player
// match across 3 locales is ~60 profile fetches, and loading them one
// at a time made BuildMatch's cold path scale linearly with roster
// size. Cross-caller parallelism already existed via singleflight;
// this bounds the WITHIN-call batch.
const playerLoadConcurrency = 8

// GetPlayers returns a map of cached Player values, fetching any missing
// ones from the API. Missing keys within one call load as a bounded
// parallel batch (playerLoadConcurrency); concurrent callers requesting
// different (PlayerID, Locale) keys load in parallel; concurrent
// callers requesting the SAME key share a single in-flight HTTP call
// via lru.LoadCoalesced — the shared fetch runs under WithoutCancel so
// a short-deadline leader can't fail every coalesced follower with ITS
// cancellation, and each caller's own ctx still bounds its wait.
func (c *PlayersCache) GetPlayers(ctx context.Context, ids []PlayerCacheKey) (map[PlayerCacheKey]types.Player, error) {
	result, missing := c.snapshot(ids)
	if len(missing) == 0 {
		return result, nil
	}

	// Fan out the misses. The first failure cancels the group's ctx so
	// sibling waiters stop early (their coalesced fetches, detached by
	// design, are shared with other callers and finish on their own);
	// duplicate keys in the input coalesce on the singleflight.
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(playerLoadConcurrency)
	var resultMu sync.Mutex
	for _, key := range missing {
		g.Go(func() error {
			// Fast-path: another goroutine may have populated this key
			// between the snapshot above and now. Re-check before paying
			// the singleflight join cost.
			if p, ok := c.lookup(key); ok {
				resultMu.Lock()
				result[key] = p
				resultMu.Unlock()
				return nil
			}
			p, err := c.loadPlayer(gctx, key)
			if err != nil {
				// A by-id 404 is definitive absence — classify it as
				// ErrItemNotFound (the ErrItemNotFound docs name Player)
				// while keeping the APIError in the chain.
				return notFoundIfAbsent(err)
			}
			// Use the flight's return value directly rather than relying
			// on a cache re-read: when a Clear raced the load, the store
			// was suppressed (tombstone) but the freshly-fetched player is
			// still the correct answer for THIS caller. Zero value =
			// upstream had no such player.
			if p != (types.Player{}) {
				resultMu.Lock()
				result[key] = p
				resultMu.Unlock()
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	// Success = every requested key resolved. Compare against the map,
	// not len(ids): duplicate keys in the input collapse to one map
	// entry, and a length comparison mis-reported [key, key] as a
	// failure with an EMPTY not-found list.
	var notFound []PlayerCacheKey
	for _, id := range ids {
		if _, ok := result[id]; !ok {
			notFound = append(notFound, id)
		}
	}
	if len(notFound) == 0 {
		return result, nil
	}
	return nil, fmt.Errorf("get player from cache - some players %v not found: %w", notFound, ErrItemNotFoundInCache)
}

// loadPlayer runs the singleflight-coalesced profile fetch for one
// key, re-registering whenever a Clear/Purge invalidates the flight
// generation mid-registration (errStaleFlight). The zero Player means
// upstream had no such player.
func (c *PlayersCache) loadPlayer(ctx context.Context, key PlayerCacheKey) (types.Player, error) {
	for {
		gen := c.flightGen.Load()
		p, err := lru.LoadCoalesced(ctx, c.lifetime, &c.loadGroup, c.flightKey(gen, key), func(loadCtx context.Context) (types.Player, error) {
			// Stale-generation guard — see errStaleFlight for the
			// register-vs-clear window this closes.
			if c.flightGen.Load() != gen {
				return types.Player{}, errStaleFlight
			}
			// Snapshot the flight's start BEFORE fetching: the store
			// below is suppressed if a Clear lands after this point
			// (see clearMu/clearedIDsAt).
			loadStarted := time.Now()
			// Re-check under the singleflight gate — the leader of a
			// duplicate-call group may already have stored the value
			// before the follower entered the closure.
			if p, ok := c.lookup(key); ok {
				return p, nil
			}
			data, err := c.apiClient.FetchPlayerProfile(loadCtx, key.PlayerID, key.Locale)
			if err != nil {
				return types.Player{}, fmt.Errorf("fetch player profile %s/%s: %w", key.PlayerID, key.Locale, err)
			}
			if data == nil {
				return types.Player{}, nil
			}
			p := types.Player{
				ID:       data.Player.ID,
				Name:     data.Player.Name,
				FullName: data.Player.FullName,
				SportID:  data.Player.SportID,
				Locale:   key.Locale,
			}
			// Caller still receives p; a suppressed store just means
			// the next read refetches (post-clear freshness wins).
			c.storeIfNotCleared(key, p, loadStarted)
			return p, nil
		})
		if errors.Is(err, errStaleFlight) {
			continue // re-register under the fresh generation
		}
		return p, err
	}
}

// lookup is the single-key snapshot helper used by the singleflight
// closure to short-circuit before issuing an HTTP call.
func (c *PlayersCache) lookup(id PlayerCacheKey) (types.Player, bool) {
	return c.players.Get(id)
}

// Clear evicts the cache entry for the given (id, locale). The
// tombstone guarantees an in-flight load that started before this call
// cannot re-admit its result afterwards.
func (c *PlayersCache) Clear(id PlayerCacheKey) {
	c.clearMu.Lock()
	c.markCleared(id.PlayerID)
	c.players.Remove(id)
	c.clearMu.Unlock()
}

// ClearByID evicts every entry for the player ID across all locales.
func (c *PlayersCache) ClearByID(playerID string) {
	c.clearMu.Lock()
	c.markCleared(playerID)
	for _, k := range c.players.Keys() {
		if k.PlayerID == playerID {
			c.players.Remove(k)
		}
	}
	c.clearMu.Unlock()
}

// Purge clears the entire cache.
func (c *PlayersCache) Purge() {
	c.clearMu.Lock()
	c.flightGen.Add(1) // detach future callers from in-flight loads
	c.purgedAt = time.Now()
	c.clearedIDsAt = make(map[string]time.Time)
	c.players.Purge()
	c.clearMu.Unlock()
}

func (c *PlayersCache) snapshot(ids []PlayerCacheKey) (map[PlayerCacheKey]types.Player, []PlayerCacheKey) {
	found := make(map[PlayerCacheKey]types.Player, len(ids))
	var missing []PlayerCacheKey
	for _, id := range ids {
		if v, ok := c.players.Get(id); ok {
			found[id] = v
		} else {
			missing = append(missing, id)
		}
	}
	return found, missing
}

func newPlayersCache(lifeCtx context.Context, apiClient *api.Client, logger *log.Logger) *PlayersCache {
	return &PlayersCache{
		apiClient:    apiClient,
		logger:       logger,
		lifetime:     lifeCtx,
		players:      lru.NewTTL[PlayerCacheKey, types.Player](playersCacheSize, nil, lru.DefaultEventCacheTTL),
		clearedIDsAt: make(map[string]time.Time),
	}
}

// BuildPlayer is a convenience constructor used by entity factories. It
// resolves the (id, locale) snapshot from the cache, fetching if missing,
// and returns the populated value. Errors propagate from the underlying
// fetch.
func BuildPlayer(ctx context.Context, c *PlayersCache, id types.URN, locale types.Locale) (*types.Player, error) {
	return c.GetPlayer(ctx, PlayerCacheKey{PlayerID: id.ToString(), Locale: locale})
}

// flightKey builds the generation-prefixed singleflight key (see
// flightGen). The generation is passed in, not read here: the caller
// must capture it ONCE and re-check it inside the flight closure, so a
// Clear landing between capture and registration is detected
// (errStaleFlight) instead of silently splitting the flight.
func (c *PlayersCache) flightKey(gen uint64, key PlayerCacheKey) string {
	return fmt.Sprintf("%d|%s", gen, key.String())
}
