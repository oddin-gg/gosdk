package cache

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/oddin-gg/gosdk/internal/api"
	data "github.com/oddin-gg/gosdk/internal/api/xml"
	"github.com/oddin-gg/gosdk/internal/cache/lru"
	"github.com/oddin-gg/gosdk/types"
)

// MarketVoidReasonsCache caches the singleton list of market void reasons.
//
// Phase 3 rewrite: replaces patrickmn/go-cache with a small mutex-guarded
// slice. Single key, no locale; LRU/TTL adds nothing here. A failed
// load doesn't poison the cache (loaded resets to false on error).
//
// Cold loads coalesce through singleflight and the mutex is NEVER held
// across I/O — pre-fix the cold HTTP fetch ran under the mutex, so a
// second caller with a short ctx blocked on a lock acquisition that is
// not ctx-aware and could return (stale-fetched data, nil) tens of
// seconds after its own deadline expired. Every waiter now selects on
// its own ctx (lru.LoadCoalesced) while the shared fetch runs detached
// under the cache lifetime.
type MarketVoidReasonsCache struct {
	apiClient *api.Client
	// lifetime is the detach root for coalesced loads — cancelled by
	// Manager.Close so no fetch outlives the owner.
	lifetime context.Context

	mu     sync.RWMutex // guards loaded/void/clearedAt (state only — never held across I/O)
	loaded bool
	void   []data.MarketVoidReasons
	// clearedAt suppresses stores from loads that began before a
	// Clear/Reload — without it a pre-clear flight could re-admit the
	// invalidated list. loaded stays false on a suppressed store, so
	// the next call refetches.
	clearedAt time.Time

	// flightGen is folded into the singleflight key and bumped by
	// Clear/Reload. Without it a caller arriving strictly AFTER a Clear
	// returned would JOIN a still-running pre-clear flight (same constant
	// key) and receive the pre-clear list directly from the flight's
	// return value — the tombstone only blocks the STORE, not the shared
	// return — violating the "next call refetches" invalidation contract.
	// A post-clear caller now uses a new key, so it starts its own fetch.
	flightGen atomic.Uint64

	sf singleflight.Group
}

// MarketVoidReasons returns the cached list, fetching on first access.
func (m *MarketVoidReasonsCache) MarketVoidReasons(ctx context.Context) ([]data.MarketVoidReasons, error) {
	m.mu.RLock()
	if m.loaded {
		v := m.void
		m.mu.RUnlock()
		return v, nil
	}
	m.mu.RUnlock()

	flightKey := fmt.Sprintf("void_reasons|%d", m.flightGen.Load())

	return lru.LoadCoalesced(ctx, m.lifetime, &m.sf, flightKey, func(loadCtx context.Context) ([]data.MarketVoidReasons, error) {
		// Re-check under the flight — a peer may have loaded already.
		m.mu.RLock()
		if m.loaded {
			v := m.void
			m.mu.RUnlock()
			return v, nil
		}
		m.mu.RUnlock()

		loadStarted := time.Now()
		v, err := m.apiClient.FetchMarketVoidReasons(loadCtx)
		if err != nil {
			// loaded stays false → next call retries. No poisoning.
			return nil, err
		}
		m.mu.Lock()
		if !m.clearedAt.After(loadStarted) {
			m.void = v
			m.loaded = true
		}
		m.mu.Unlock()
		// The caller still gets the freshly fetched list even when a
		// racing Clear suppressed the store.
		return v, nil
	})
}

// ReloadMarketVoidReasons forces a refresh on next access. Stamps the
// tombstone so an in-flight pre-reload fetch cannot re-mark the cache
// loaded with the old list (the reload may still JOIN that flight and
// return the old list once; the suppressed store keeps loaded=false, so
// the following call fetches fresh).
func (m *MarketVoidReasonsCache) ReloadMarketVoidReasons(ctx context.Context) error {
	m.flightGen.Add(1) // detach post-reload callers from any pre-reload flight
	m.mu.Lock()
	m.loaded = false
	m.clearedAt = time.Now()
	m.mu.Unlock()
	_, err := m.MarketVoidReasons(ctx)
	return err
}

// Clear marks the cache as un-loaded; next access will re-fetch. The
// tombstone stops in-flight loads from re-admitting the pre-clear list,
// and the flight-generation bump stops a post-clear caller from joining
// (and being served by) a still-running pre-clear flight.
func (m *MarketVoidReasonsCache) Clear() {
	m.flightGen.Add(1)
	m.mu.Lock()
	m.loaded = false
	m.void = nil
	m.clearedAt = time.Now()
	m.mu.Unlock()
}

func newMarketVoidReasonsCache(lifeCtx context.Context, client *api.Client) *MarketVoidReasonsCache {
	return &MarketVoidReasonsCache{apiClient: client, lifetime: lifeCtx}
}

// NewMarketVoidReason constructs a value-typed types.MarketVoidReason.
// Phase 6.1 reshape: returns the value struct directly (the
// marketVoidReasonImpl wrapper is gone). v2.28: description and
// template arguments are still *string at this internal boundary
// (their source is XML decode); conversion to Optional[string]
// happens at the constructor.
func NewMarketVoidReason(
	id int,
	name string,
	description *string,
	template *string,
	params []string,
) types.MarketVoidReason {
	return types.MarketVoidReason{
		ID:          id,
		Name:        name,
		Description: types.FromPtr(description),
		Template:    types.FromPtr(template),
		Params:      params,
	}
}
