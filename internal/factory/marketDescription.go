package factory

import (
	"context"
	"errors"
	"fmt"

	"github.com/oddin-gg/gosdk/internal/cache"
	"github.com/oddin-gg/gosdk/types"
)

// MarketDescriptionFactory ...
type MarketDescriptionFactory struct {
	marketDescriptionCache *cache.MarketDescriptionCache
	marketVoidReasonsCache *cache.MarketVoidReasonsCache
	playerCache            *cache.PlayersCache
	competitorCache        *cache.CompetitorCache
}

// MarketDescriptionByIDAndSpecifiers returns the cached market
// description by marketID, specifiers, and locales.
func (m MarketDescriptionFactory) MarketDescriptionByIDAndSpecifiers(
	ctx context.Context,
	marketID uint,
	specifiers map[string]string,
	locales []types.Locale,
) (*types.MarketDescription, error) {
	var variant *string
	if specifier, ok := specifiers["variant"]; ok {
		variant = &specifier
	}
	return m.MarketDescriptionByIDAndVariant(ctx, marketID, variant, locales)
}

// MarketDescriptionByIDAndVariant returns the cached market description
// by (marketID, variant, locales). Always returns a populated value or
// an error.
func (m MarketDescriptionFactory) MarketDescriptionByIDAndVariant(
	ctx context.Context,
	marketID uint,
	variant *string,
	locales []types.Locale,
) (*types.MarketDescription, error) {
	mds, err := m.marketDescriptionCache.MarketDescriptionByID(ctx, marketID, variant, locales)
	if err != nil {
		return nil, fmt.Errorf("get market description by id failed: %w", err)
	}
	if mds == nil {
		return nil, errors.New("get market description by id failed - cannot be nil")
	}
	desc := mds.Snapshot()
	return &desc, nil
}

// MarketVoidReasons returns the void-reasons catalog.
func (m MarketDescriptionFactory) MarketVoidReasons(ctx context.Context) ([]types.MarketVoidReason, error) {
	data, err := m.marketVoidReasonsCache.MarketVoidReasons(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]types.MarketVoidReason, 0, len(data))
	for _, d := range data {
		params := make([]string, len(d.VoidReasonParams))
		for i, p := range d.VoidReasonParams {
			params[i] = p.Name
		}
		result = append(result, cache.NewMarketVoidReason(
			d.ID,
			d.Name,
			d.Description,
			d.Template,
			params,
		))
	}
	return result, nil
}

// ReloadMarketVoidReasons forces a refresh and returns the new list.
func (m MarketDescriptionFactory) ReloadMarketVoidReasons(ctx context.Context) ([]types.MarketVoidReason, error) {
	if err := m.marketVoidReasonsCache.ReloadMarketVoidReasons(ctx); err != nil {
		return nil, err
	}
	return m.MarketVoidReasons(ctx)
}

// MarketDescriptions returns every market description for the locale.
func (m MarketDescriptionFactory) MarketDescriptions(ctx context.Context, locale types.Locale) ([]types.MarketDescription, error) {
	mds, err := m.marketDescriptionCache.LocalizedMarketDescriptions(ctx, locale)
	if err != nil {
		return nil, err
	}
	result := make([]types.MarketDescription, 0, len(mds))
	for _, value := range mds {
		result = append(result, value.Snapshot())
	}
	return result, nil
}

// NewMarketDescriptionFactory ...
func NewMarketDescriptionFactory(
	marketDescriptionCache *cache.MarketDescriptionCache,
	marketVoidReasonsCache *cache.MarketVoidReasonsCache,
	playerCache *cache.PlayersCache,
	competitorCache *cache.CompetitorCache,
) *MarketDescriptionFactory {
	return &MarketDescriptionFactory{
		marketDescriptionCache: marketDescriptionCache,
		marketVoidReasonsCache: marketVoidReasonsCache,
		playerCache:            playerCache,
		competitorCache:        competitorCache,
	}
}
