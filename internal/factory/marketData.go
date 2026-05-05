package factory

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/oddin-gg/gosdk/internal/cache"
	"github.com/oddin-gg/gosdk/types"
)

// MarketDataFactory ...
type MarketDataFactory struct {
	oddsFeedConfiguration    types.OddsFeedConfiguration
	marketDescriptionFactory *MarketDescriptionFactory
}

// BuildMarketData ...
func (m MarketDataFactory) BuildMarketData(event interface{}, marketID uint, specifiers map[string]string) types.MarketData {
	return &marketDataImpl{
		marketID:                 marketID,
		specifiers:               specifiers,
		marketDescriptionFactory: m.marketDescriptionFactory,
		event:                    event,
	}
}

// NewMarketDataFactory ...
func NewMarketDataFactory(oddsFeedConfiguration types.OddsFeedConfiguration, marketDescriptionFactory *MarketDescriptionFactory) *MarketDataFactory {
	return &MarketDataFactory{
		oddsFeedConfiguration:    oddsFeedConfiguration,
		marketDescriptionFactory: marketDescriptionFactory,
	}
}

type marketDataImpl struct {
	marketID                 uint
	specifiers               map[string]string
	marketDescriptionFactory *MarketDescriptionFactory
	event                    interface{}
}

func (m marketDataImpl) OutcomeName(ctx context.Context, outcomeID string, locale types.Locale) (*string, error) {
	marketDescription, err := m.marketDescriptionFactory.MarketDescriptionByIDAndSpecifiers(ctx, m.marketID, m.specifiers, []types.Locale{locale})
	if err != nil {
		return nil, err
	}

	found := false
	var outcomeName *string
	for _, outcome := range marketDescription.Outcomes {
		if outcome.ID == outcomeID {
			outcomeName = outcome.LocalizedName(locale)
			found = true
			break
		}
	}

	// market with dynamic outcomes can have also non-dynamic outcome, that's reason why outcome with outcomeID exists at first
	if !found && marketDescription.OutcomeType != nil {
		switch outcomeType(*marketDescription.OutcomeType) {
		case playerOutcomeType:
			player, err := m.marketDescriptionFactory.playerCache.GetPlayer(ctx, cache.PlayerCacheKey{PlayerID: outcomeID, Locale: locale})
			if err != nil {
				return nil, fmt.Errorf("derivation of outcome name for dynamic player outcome failed for id [%s]: %w", outcomeID, err)
			}
			outcomeName = &player.Name

		case competitorOutcomeType:
			urn, err := types.ParseURN(outcomeID)
			if err != nil {
				return nil, fmt.Errorf("unsupported competitor id in outcome %s: %w", outcomeID, err)
			}
			competitor, err := m.marketDescriptionFactory.competitorCache.Competitor(ctx, *urn, []types.Locale{locale})
			if err != nil {
				return nil, fmt.Errorf("derivation of outcome name for dynamic player outcome failed for id [%s]: %w", outcomeID, err)
			}

			name, err := competitor.LocalizedName(locale)
			if err != nil {
				return nil, fmt.Errorf("missing locale %s: %w", locale, err)
			}
			outcomeName = name

		default:
			return nil, fmt.Errorf("unsupported outcome type [%s]", *marketDescription.OutcomeType)
		}
	}

	return m.makeOutcomeName(outcomeName, locale)
}

func (m marketDataImpl) MarketName(ctx context.Context, locale types.Locale) (*string, error) {
	marketDescription, err := m.marketDescriptionFactory.MarketDescriptionByIDAndSpecifiers(ctx, m.marketID, m.specifiers, []types.Locale{locale})
	if err != nil {
		return nil, err
	}

	name := marketDescription.LocalizedName(locale)
	if name == nil {
		return nil, fmt.Errorf("missing locale %s for market %d", locale, m.marketID)
	}

	return m.makeMarketName(ctx, *name, locale)
}

func (m marketDataImpl) makeOutcomeName(outcomeName *string, locale types.Locale) (*string, error) {
	if outcomeName == nil {
		return nil, nil
	}

	match, isMatch := m.event.(types.Match)

	switch {
	// @TODO this broke with different locale - need to use ID
	case *outcomeName == "home" && isMatch && match.HomeCompetitor != nil:
		name := match.HomeCompetitor.Name(locale)
		return &name, nil
		// @TODO this broke with different locale - need to use ID
	case *outcomeName == "away" && isMatch && match.AwayCompetitor != nil:
		name := match.AwayCompetitor.Name(locale)
		return &name, nil
	default:
		return outcomeName, nil
	}
}

func (m marketDataImpl) makeMarketName(ctx context.Context, marketName string, locale types.Locale) (*string, error) {
	if len(m.specifiers) == 0 {
		return &marketName, nil
	}

	match, isMatch := m.event.(types.Match)
	marketDescription, err := m.marketDescriptionFactory.MarketDescriptionByIDAndSpecifiers(ctx, m.marketID, m.specifiers, []types.Locale{locale})
	if err != nil {
		return nil, err
	}
	groups := marketDescription.Groups

	template := marketName
	for key, value := range m.specifiers {
		key = "{" + key + "}"
		if !strings.Contains(template, key) {
			continue
		}

		switch {
		case value == "home" && isMatch && match.HomeCompetitor != nil:
			value = match.HomeCompetitor.Name(locale)
		case value == "away" && isMatch && match.AwayCompetitor != nil:
			value = match.AwayCompetitor.Name(locale)
		}

		// handle props markets
		if name, isPropsMarket := m.getPropsName(ctx, value, groups, locale); isPropsMarket {
			value = name
		}

		template = strings.ReplaceAll(template, key, value)
	}

	return &template, nil
}

func (m marketDataImpl) getPropsName(ctx context.Context, entityID string, groups []string, locale types.Locale) (string, bool) {
	if !slices.Contains(groups, types.MarketGroupPlayerProps) {
		return "", false
	}

	urn, err := types.ParseURN(entityID)
	if err != nil {
		return "", false
	}

	//nolint:gocritic // for simpler extension
	switch urn.Type {
	case string(types.PlayerEventType):
		player, err := m.marketDescriptionFactory.playerCache.GetPlayer(
			ctx,
			cache.PlayerCacheKey{
				PlayerID: entityID,
				Locale:   locale,
			},
		)
		if err != nil {
			return "", false
		}
		return player.Name, true
	}
	return "", false
}
