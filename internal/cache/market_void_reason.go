package cache

import (
	"context"
	"sync"

	"github.com/oddin-gg/gosdk/internal/api"
	data "github.com/oddin-gg/gosdk/internal/api/xml"
	"github.com/oddin-gg/gosdk/types"
)

// MarketVoidReasonsCache caches the singleton list of market void reasons.
//
// Phase 3 rewrite: replaces patrickmn/go-cache with a small sync.RWMutex-
// guarded slice. Single key, no locale; LRU/TTL adds nothing here. A failed
// load doesn't poison the cache (loaded resets to false on error).
type MarketVoidReasonsCache struct {
	apiClient *api.Client

	mu     sync.Mutex // guards loaded + voidReasons; serializes loads
	loaded bool
	void   []data.MarketVoidReasons
}

// MarketVoidReasons returns the cached list, fetching on first access.
func (m *MarketVoidReasonsCache) MarketVoidReasons(ctx context.Context) ([]data.MarketVoidReasons, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.loaded {
		return m.void, nil
	}
	v, err := m.apiClient.FetchMarketVoidReasons(ctx)
	if err != nil {
		// loaded stays false → next call retries. No poisoning.
		return nil, err
	}
	m.void = v
	m.loaded = true
	return m.void, nil
}

// ReloadMarketVoidReasons forces a refresh on next access.
func (m *MarketVoidReasonsCache) ReloadMarketVoidReasons(ctx context.Context) error {
	m.mu.Lock()
	m.loaded = false
	m.mu.Unlock()
	_, err := m.MarketVoidReasons(ctx)
	return err
}

// Clear marks the cache as un-loaded; next access will re-fetch.
func (m *MarketVoidReasonsCache) Clear() {
	m.mu.Lock()
	m.loaded = false
	m.void = nil
	m.mu.Unlock()
}

func newMarketVoidReasonsCache(client *api.Client) *MarketVoidReasonsCache {
	return &MarketVoidReasonsCache{apiClient: client}
}

// NewMarketVoidReason constructs a value-typed types.MarketVoidReason.
// Phase 6.1 reshape: returns the value struct directly (the
// marketVoidReasonImpl wrapper is gone).
func NewMarketVoidReason(
	id uint,
	name string,
	description *string,
	template *string,
	params []string,
) types.MarketVoidReason {
	return types.MarketVoidReason{
		ID:          id,
		Name:        name,
		Description: description,
		Template:    template,
		Params:      params,
	}
}
