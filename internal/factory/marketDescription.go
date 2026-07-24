package factory

import (
	"cmp"
	"context"
	"fmt"
	"slices"

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
	marketID int,
	specifiers map[string]string,
	locales []types.Locale,
) (*types.MarketDescription, error) {
	variant := types.None[string]()
	if specifier, ok := specifiers["variant"]; ok {
		variant = types.Some(specifier)
	}
	return m.MarketDescriptionByIDAndVariant(ctx, marketID, variant, locales)
}

// MarketDescriptionByIDAndVariant returns the cached market description
// by (marketID, variant, locales). Always returns a populated value or
// an error.
func (m MarketDescriptionFactory) MarketDescriptionByIDAndVariant(
	ctx context.Context,
	marketID int,
	variant types.Optional[string],
	locales []types.Locale,
) (*types.MarketDescription, error) {
	mds, err := m.marketDescriptionCache.MarketDescriptionByID(ctx, marketID, variant, locales)
	if err != nil {
		return nil, fmt.Errorf("market description %d/%v locales=%v: %w", marketID, variant, locales, err)
	}
	if mds == nil {
		// Cache returned (nil, nil) — should not happen, but wrap
		// ErrItemNotFoundInCache so consumers can errors.Is.
		return nil, fmt.Errorf("market description %d/%v locales=%v: cache returned nil with no error: %w", marketID, variant, locales, cache.ErrItemNotFoundInCache)
	}
	desc := mds.Snapshot()
	return &desc, nil
}

// MarketVoidReasons returns the void-reasons catalog.
func (m MarketDescriptionFactory) MarketVoidReasons(ctx context.Context) ([]types.MarketVoidReason, error) {
	data, err := m.marketVoidReasonsCache.MarketVoidReasons(ctx)
	if err != nil {
		return nil, fmt.Errorf("market void reasons: %w", err)
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
		return nil, fmt.Errorf("reload market void reasons: %w", err)
	}
	return m.MarketVoidReasons(ctx)
}

// MarketDescriptions returns every market description for the locale.
func (m MarketDescriptionFactory) MarketDescriptions(ctx context.Context, locale types.Locale) ([]types.MarketDescription, error) {
	mds, err := m.marketDescriptionCache.LocalizedMarketDescriptions(ctx, locale)
	if err != nil {
		return nil, fmt.Errorf("market descriptions locale %s: %w", locale, err)
	}
	result := make([]types.MarketDescription, 0, len(mds))
	for _, value := range mds {
		result = append(result, value.Snapshot())
	}
	sortMarketDescriptions(result)
	return result, nil
}

// sortMarketDescriptions orders the catalog canonically by
// (ID, Variant): the cache hands entries back in map-iteration order,
// so identical calls otherwise reshuffle the public listing between an
// uncached and a cached read.
func sortMarketDescriptions(mds []types.MarketDescription) {
	slices.SortFunc(mds, func(a, b types.MarketDescription) int {
		if c := cmp.Compare(a.ID, b.ID); c != 0 {
			return c
		}
		return cmp.Compare(a.Variant.ValueOr(""), b.Variant.ValueOr(""))
	})
}

// MultiMarketDescriptions preloads every supplied locale into the
// description cache and returns the catalog from the primary locale's
// view. Each returned description's Names + outcome Names/Descriptions
// maps include all supplied locales (the cache stores per-locale
// strings in shared entries; Snapshot() emits whatever locales the
// entry currently has).
func (m MarketDescriptionFactory) MultiMarketDescriptions(ctx context.Context, locales []types.Locale) ([]types.MarketDescription, error) {
	mds, err := m.marketDescriptionCache.MultiLocalizedMarketDescriptions(ctx, locales)
	if err != nil {
		return nil, fmt.Errorf("market descriptions locales=%v: %w", locales, err)
	}
	result := make([]types.MarketDescription, 0, len(mds))
	for _, value := range mds {
		result = append(result, value.Snapshot())
	}
	sortMarketDescriptions(result)
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
