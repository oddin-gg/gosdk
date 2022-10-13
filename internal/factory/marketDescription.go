package factory

import (
	"github.com/oddin-gg/gosdk/internal/cache"
	"github.com/oddin-gg/gosdk/protocols"
)

// MarketDescriptionFactory ...
type MarketDescriptionFactory struct {
	marketDescriptionCache *cache.MarketDescriptionCache
	marketVoidReasonsCache *cache.MarketVoidReasonsCache
}

// MarketDescriptionByID ...
func (m MarketDescriptionFactory) MarketDescriptionByID(marketID uint, specifiers map[string]string, locales []protocols.Locale) protocols.MarketDescription {
	var variant *string
	specifier, ok := specifiers["variant"]
	if ok {
		variant = &specifier
	}

	return cache.NewMarketDescription(marketID, variant, m.marketDescriptionCache, locales)
}

// MarketVoidReasons ...
func (m MarketDescriptionFactory) MarketVoidReasons() ([]protocols.MarketVoidReason, error) {
	data, err := m.marketVoidReasonsCache.MarketVoidReasons()
	if err != nil {
		return nil, err
	}

	result := make([]protocols.MarketVoidReason, len(data))
	for i, d := range data {

		params := make([]string, len(d.VoidReasonParams))
		for i, p := range d.VoidReasonParams {
			params[i] = p.Name
		}

		description := cache.NewMarketVoidReason(
			d.ID,
			d.Name,
			d.Description,
			d.Template,
			params,
		)
		result[i] = description
	}

	return result, nil
}

// ReloadMarketVoidReasons ...
func (m MarketDescriptionFactory) ReloadMarketVoidReasons() ([]protocols.MarketVoidReason, error) {
	if err := m.marketVoidReasonsCache.ReloadMarketVoidReasons(); err != nil {
		return nil, err
	}

	return m.MarketVoidReasons()
}

// MarketDescriptions ...
func (m MarketDescriptionFactory) MarketDescriptions(locale protocols.Locale) ([]protocols.MarketDescription, error) {
	keys, err := m.marketDescriptionCache.LocalizedMarketDescriptions(locale)
	if err != nil {
		return nil, err
	}

	result := make([]protocols.MarketDescription, len(keys))
	for i, key := range keys {
		description := cache.NewMarketDescription(
			key.MarketID,
			key.Variant,
			m.marketDescriptionCache,
			[]protocols.Locale{locale},
		)
		result[i] = description
	}

	return result, nil
}

// NewMarketDescriptionFactory ...
func NewMarketDescriptionFactory(
	marketDescriptionCache *cache.MarketDescriptionCache,
	marketVoidReasonsCache *cache.MarketVoidReasonsCache,
) *MarketDescriptionFactory {
	return &MarketDescriptionFactory{
		marketDescriptionCache: marketDescriptionCache,
		marketVoidReasonsCache: marketVoidReasonsCache,
	}
}
