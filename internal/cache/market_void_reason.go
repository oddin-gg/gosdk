package cache

import (
	"errors"
	"time"

	"github.com/oddin-gg/gosdk/internal/api"
	data "github.com/oddin-gg/gosdk/internal/api/xml"
	"github.com/oddin-gg/gosdk/protocols"
	"github.com/patrickmn/go-cache"
)

const MarketVoidReasonCacheKey = "market_void_reasons"

// MarketVoidReasonsCache ...
type MarketVoidReasonsCache struct {
	apiClient     *api.Client
	internalCache *cache.Cache
}

// MarketVoidReasons ...
func (m *MarketVoidReasonsCache) MarketVoidReasons() ([]data.MarketVoidReasons, error) {
	d, ok := m.internalCache.Get(MarketVoidReasonCacheKey)
	if !ok {
		if err := m.loadAndCacheItem(); err != nil {
			return nil, err
		}
		d, ok = m.internalCache.Get(MarketVoidReasonCacheKey)
	}

	if !ok {
		return nil, errors.New("unable to load market void reasons")
	}

	return d.([]data.MarketVoidReasons), nil
}

// ReloadMarketVoidReasons ...
func (m *MarketVoidReasonsCache) ReloadMarketVoidReasons() error {
	return m.loadAndCacheItem()
}

func (m *MarketVoidReasonsCache) loadAndCacheItem() error {
	voidReasons, err := m.apiClient.FetchMarketVoidReasons()
	if err != nil {
		return err
	}
	m.internalCache.Set(MarketVoidReasonCacheKey, voidReasons, 0)
	return nil
}

func newMarketVoidReasonsCache(client *api.Client) *MarketVoidReasonsCache {
	return &MarketVoidReasonsCache{
		internalCache: cache.New(24*time.Hour, 1*time.Hour),
		apiClient:     client,
	}
}

type marketVoidReasonImpl struct {
	id          uint
	name        string
	description *string
	template    *string
	params      []string
}

func (m marketVoidReasonImpl) ID() uint {
	return m.id
}

func (m marketVoidReasonImpl) Name() string {
	return m.name
}

func (m marketVoidReasonImpl) Description() *string {
	return m.description
}

func (m marketVoidReasonImpl) Template() *string {
	return m.template
}

func (m marketVoidReasonImpl) Params() []string {
	return m.params
}

// NewMarketVoidReason ...
func NewMarketVoidReason(
	id uint,
	name string,
	description *string,
	template *string,
	params []string,
) protocols.MarketVoidReason {
	return &marketVoidReasonImpl{
		id:          id,
		name:        name,
		description: description,
		template:    template,
		params:      params,
	}
}
