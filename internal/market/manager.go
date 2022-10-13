package market

import (
	"github.com/oddin-gg/gosdk/internal/cache"
	"github.com/oddin-gg/gosdk/internal/factory"
	"github.com/oddin-gg/gosdk/protocols"
)

// Manager ...
type Manager struct {
	oddsFeedConfiguration    protocols.OddsFeedConfiguration
	marketDescriptionFactory *factory.MarketDescriptionFactory
	cacheManager             *cache.Manager
}

// MarketDescriptions ...
func (m Manager) MarketDescriptions() ([]protocols.MarketDescription, error) {
	return m.LocalizedMarketDescriptions(m.oddsFeedConfiguration.DefaultLocale())
}

// LocalizedMarketDescriptions ...
func (m Manager) LocalizedMarketDescriptions(locale protocols.Locale) ([]protocols.MarketDescription, error) {
	return m.marketDescriptionFactory.MarketDescriptions(locale)
}

// ClearMarketDescription ...
func (m Manager) ClearMarketDescription(marketID uint, variant *string) {
	m.cacheManager.MarketDescriptionCache.ClearCacheItem(marketID, variant)
}

// MarketVoidReasons ...
func (m Manager) MarketVoidReasons() ([]protocols.MarketVoidReason, error) {
	return m.marketDescriptionFactory.MarketVoidReasons()
}

// ReloadMarketVoidReasons ...
func (m Manager) ReloadMarketVoidReasons() ([]protocols.MarketVoidReason, error) {
	return m.marketDescriptionFactory.ReloadMarketVoidReasons()
}

// NewManager ...
func NewManager(cacheManager *cache.Manager, marketDescriptionFactory *factory.MarketDescriptionFactory, oddsFeedConfiguration protocols.OddsFeedConfiguration) *Manager {
	return &Manager{
		oddsFeedConfiguration:    oddsFeedConfiguration,
		marketDescriptionFactory: marketDescriptionFactory,
		cacheManager:             cacheManager,
	}
}
