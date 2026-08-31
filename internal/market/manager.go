package market

import (
	"context"

	"github.com/oddin-gg/gosdk/internal/cache"
	"github.com/oddin-gg/gosdk/internal/config"
	"github.com/oddin-gg/gosdk/internal/factory"
	"github.com/oddin-gg/gosdk/types"
)

// Manager exposes the market-description catalog. ctx flows through to
// the underlying cache loaders.
type Manager struct {
	oddsFeedConfiguration    config.Config
	marketDescriptionFactory *factory.MarketDescriptionFactory
	cacheManager             *cache.Manager
}

// MarketDescriptions ...
func (m Manager) MarketDescriptions(ctx context.Context) ([]types.MarketDescription, error) {
	return m.LocalizedMarketDescriptions(ctx, m.oddsFeedConfiguration.DefaultLocale())
}

// LocalizedMarketDescriptionByIDAndVariant returns the description in the
// supplied locales. Variants caches per-locale; passing multiple locales
// fills the cache for all of them.
func (m Manager) LocalizedMarketDescriptionByIDAndVariant(
	ctx context.Context,
	marketID int,
	variant types.Optional[string],
	locales ...types.Locale,
) (*types.MarketDescription, error) {
	if len(locales) == 0 {
		locales = []types.Locale{m.oddsFeedConfiguration.DefaultLocale()}
	}
	return m.marketDescriptionFactory.MarketDescriptionByIDAndVariant(ctx, marketID, variant, locales)
}

// LocalizedMarketDescriptions ...
func (m Manager) LocalizedMarketDescriptions(ctx context.Context, locale types.Locale) ([]types.MarketDescription, error) {
	return m.marketDescriptionFactory.MarketDescriptions(ctx, locale)
}

// MultiLocalizedMarketDescriptions preloads every supplied locale
// into the description cache and returns the catalog snapshotted
// against the primary (first) locale. Each entry's Names +
// outcome Names/Descriptions maps include every supplied locale —
// callers can then read e.g. desc.Names[en] and desc.Names[ru]
// without refetching.
func (m Manager) MultiLocalizedMarketDescriptions(ctx context.Context, locales []types.Locale) ([]types.MarketDescription, error) {
	if len(locales) == 0 {
		locales = []types.Locale{m.oddsFeedConfiguration.DefaultLocale()}
	}
	return m.marketDescriptionFactory.MultiMarketDescriptions(ctx, locales)
}

// ClearMarketDescription evicts a single cached market description.
func (m Manager) ClearMarketDescription(marketID int, variant types.Optional[string]) {
	m.cacheManager.MarketDescriptionCache.ClearCacheItem(marketID, variant)
}

// ClearMarketVoidReasons evicts the void-reasons catalog. The next
// call to MarketVoidReasons() will refetch.
func (m Manager) ClearMarketVoidReasons() {
	m.cacheManager.MarketVoidReasonsCache.Clear()
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
func NewManager(cacheManager *cache.Manager, marketDescriptionFactory *factory.MarketDescriptionFactory, oddsFeedConfiguration config.Config) *Manager {
	return &Manager{
		oddsFeedConfiguration:    oddsFeedConfiguration,
		marketDescriptionFactory: marketDescriptionFactory,
		cacheManager:             cacheManager,
	}
}
