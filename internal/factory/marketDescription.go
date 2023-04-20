package factory

import (
	"github.com/oddin-gg/gosdk/internal/cache"
	"github.com/oddin-gg/gosdk/protocols"
	"github.com/pkg/errors"
)

// MarketDescriptionFactory ...
type MarketDescriptionFactory struct {
	marketDescriptionCache *cache.MarketDescriptionCache
	marketVoidReasonsCache *cache.MarketVoidReasonsCache
	playerCache            *cache.PlayersCache
	competitorCache        *cache.CompetitorCache
}

// MarketDescriptionByIdAndSpecifiers returns market description from cache based on marketID, specifiers and locales
func (m MarketDescriptionFactory) MarketDescriptionByIdAndSpecifiers(
	marketID uint,
	specifiers map[string]string,
	locales []protocols.Locale,
) (protocols.MarketDescription, error) {
	var variant *string
	specifier, ok := specifiers["variant"]
	if ok {
		variant = &specifier
	}

	return m.MarketDescriptionByIdAndVariant(marketID, variant, locales)
}

// MarketDescriptionByIdAndVariant returns market description from cache based on marketID, optional market variant
// and locales
func (m MarketDescriptionFactory) MarketDescriptionByIdAndVariant(
	marketID uint,
	variant *string,
	locales []protocols.Locale,
) (protocols.MarketDescription, error) {
	mds, err := m.marketDescriptionCache.MarketDescriptionByID(marketID, variant, locales)
	if err != nil {
		return nil, errors.Wrap(err, "get market description by id failed")
	}
	if mds == nil {
		return nil, errors.New("get market description by id failed - cannot be nil")
	}

	return cache.NewMarketDescription(marketID, mds.IncludesOutcomesOfType, mds.OutcomeType, variant, m.marketDescriptionCache, locales), nil
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
	marketDescriptions, err := m.marketDescriptionCache.LocalizedMarketDescriptions(locale)
	if err != nil {
		return nil, err
	}

	result := make([]protocols.MarketDescription, 0, len(marketDescriptions))
	for key, value := range marketDescriptions {
		description := cache.NewMarketDescription(
			key.MarketID,
			value.IncludesOutcomesOfType,
			value.OutcomeType,
			key.Variant,
			m.marketDescriptionCache,
			[]protocols.Locale{locale},
		)
		result = append(result, description)
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
