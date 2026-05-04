package market

import (
	"context"

	"github.com/oddin-gg/gosdk/internal/cache"
	"github.com/oddin-gg/gosdk/internal/factory"
	"github.com/oddin-gg/gosdk/protocols"
)

// Manager ...
//
// The public interface is ctx-aware. The factory/cache it delegates to is
// not yet — those layers are rewritten in Phase 3 with full ctx propagation
// (cache loader signatures take ctx). For Phase 2 we accept a one-line
// context.Background() inside delegations that will be replaced.
type Manager struct {
	oddsFeedConfiguration    protocols.OddsFeedConfiguration
	marketDescriptionFactory *factory.MarketDescriptionFactory
	cacheManager             *cache.Manager
}

// MarketDescriptions ...
func (m Manager) MarketDescriptions(ctx context.Context) ([]protocols.MarketDescription, error) {
	return m.LocalizedMarketDescriptions(ctx, m.oddsFeedConfiguration.DefaultLocale())
}

// MarketDescriptionByIDAndVariant ...
func (m Manager) MarketDescriptionByIDAndVariant(
	ctx context.Context,
	marketID uint,
	variant *string,
) (*protocols.MarketDescription, error) {
	_ = ctx // Phase 3 plumbs through the cache loader.
	locale := []protocols.Locale{m.oddsFeedConfiguration.DefaultLocale()}
	return m.marketDescriptionFactory.MarketDescriptionByIDAndVariant(marketID, variant, locale)
}

// LocalizedMarketDescriptions ...
func (m Manager) LocalizedMarketDescriptions(ctx context.Context, locale protocols.Locale) ([]protocols.MarketDescription, error) {
	_ = ctx // Phase 3 plumbs through the cache loader.
	return m.marketDescriptionFactory.MarketDescriptions(locale)
}

// ClearMarketDescription ...
func (m Manager) ClearMarketDescription(marketID uint, variant *string) {
	m.cacheManager.MarketDescriptionCache.ClearCacheItem(marketID, variant)
}

// MarketVoidReasons ...
func (m Manager) MarketVoidReasons(ctx context.Context) ([]protocols.MarketVoidReason, error) {
	_ = ctx // Phase 3 plumbs through the cache loader.
	return m.marketDescriptionFactory.MarketVoidReasons()
}

// ReloadMarketVoidReasons ...
func (m Manager) ReloadMarketVoidReasons(ctx context.Context) ([]protocols.MarketVoidReason, error) {
	_ = ctx // Phase 3 plumbs through the cache loader.
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
