package factory

import (
	"github.com/oddin-gg/gosdk/internal/cache"
	"github.com/oddin-gg/gosdk/protocols"
)

// MarketDescriptionFactory ...
type MarketDescriptionFactory struct {
	marketDescriptionCache *cache.MarketDescriptionCache
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
func NewMarketDescriptionFactory(marketDescriptionCache *cache.MarketDescriptionCache) *MarketDescriptionFactory {
	return &MarketDescriptionFactory{
		marketDescriptionCache: marketDescriptionCache,
	}
}
