package market

import (
	"context"

	"github.com/oddin-gg/gosdk/internal/cache"
	"github.com/oddin-gg/gosdk/internal/factory"
	"github.com/oddin-gg/gosdk/types"
)

// Manager exposes the market-description catalog. ctx flows through to
// the underlying cache loaders.
type Manager struct {
	oddsFeedConfiguration    types.OddsFeedConfiguration
	marketDescriptionFactory *factory.MarketDescriptionFactory
	cacheManager             *cache.Manager
}

// MarketDescriptions ...
func (m Manager) MarketDescriptions(ctx context.Context) ([]types.MarketDescription, error) {
	return m.LocalizedMarketDescriptions(ctx, m.oddsFeedConfiguration.DefaultLocale())
}

// MarketDescriptionByIDAndVariant ...
func (m Manager) MarketDescriptionByIDAndVariant(
	ctx context.Context,
	marketID uint,
	variant *string,
) (*types.MarketDescription, error) {
	locale := []types.Locale{m.oddsFeedConfiguration.DefaultLocale()}
	return m.marketDescriptionFactory.MarketDescriptionByIDAndVariant(ctx, marketID, variant, locale)
}

// LocalizedMarketDescriptions ...
func (m Manager) LocalizedMarketDescriptions(ctx context.Context, locale types.Locale) ([]types.MarketDescription, error) {
	return m.marketDescriptionFactory.MarketDescriptions(ctx, locale)
}

// ClearMarketDescription ...
func (m Manager) ClearMarketDescription(marketID uint, variant *string) {
	m.cacheManager.MarketDescriptionCache.ClearCacheItem(marketID, variant)
}

// MarketVoidReasons ...
func (m Manager) MarketVoidReasons(ctx context.Context) ([]types.MarketVoidReason, error) {
	return m.marketDescriptionFactory.MarketVoidReasons(ctx)
}

// ReloadMarketVoidReasons ...
func (m Manager) ReloadMarketVoidReasons(ctx context.Context) ([]types.MarketVoidReason, error) {
	return m.marketDescriptionFactory.ReloadMarketVoidReasons(ctx)
}

// NewManager ...
func NewManager(cacheManager *cache.Manager, marketDescriptionFactory *factory.MarketDescriptionFactory, oddsFeedConfiguration types.OddsFeedConfiguration) *Manager {
	return &Manager{
		oddsFeedConfiguration:    oddsFeedConfiguration,
		marketDescriptionFactory: marketDescriptionFactory,
		cacheManager:             cacheManager,
	}
}
