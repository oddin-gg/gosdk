package factory

import (
	"context"
	"errors"
	"fmt"

	"github.com/oddin-gg/gosdk/internal/cache"
	"github.com/oddin-gg/gosdk/protocols"
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
	marketID uint,
	specifiers map[string]string,
	locales []protocols.Locale,
) (*protocols.MarketDescription, error) {
	var variant *string
	if specifier, ok := specifiers["variant"]; ok {
		variant = &specifier
	}
	return m.MarketDescriptionByIDAndVariant(marketID, variant, locales)
}

// MarketDescriptionByIDAndVariant returns the cached market description
// by (marketID, variant, locales). Always returns a populated value or
// an error.
func (m MarketDescriptionFactory) MarketDescriptionByIDAndVariant(
	marketID uint,
	variant *string,
	locales []protocols.Locale,
) (*protocols.MarketDescription, error) {
	mds, err := m.marketDescriptionCache.MarketDescriptionByID(context.Background(), marketID, variant, locales)
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
func (m MarketDescriptionFactory) MarketVoidReasons() ([]protocols.MarketVoidReason, error) {
	data, err := m.marketVoidReasonsCache.MarketVoidReasons(context.Background())
	if err != nil {
		return nil, err
	}
	result := make([]protocols.MarketVoidReason, 0, len(data))
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
func (m MarketDescriptionFactory) ReloadMarketVoidReasons() ([]protocols.MarketVoidReason, error) {
	if err := m.marketVoidReasonsCache.ReloadMarketVoidReasons(context.Background()); err != nil {
		return nil, err
	}
	return m.MarketVoidReasons()
}

// MarketDescriptions returns every market description for the locale.
func (m MarketDescriptionFactory) MarketDescriptions(locale protocols.Locale) ([]protocols.MarketDescription, error) {
	mds, err := m.marketDescriptionCache.LocalizedMarketDescriptions(context.Background(), locale)
	if err != nil {
		return nil, err
	}
	result := make([]protocols.MarketDescription, 0, len(mds))
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
