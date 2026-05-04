package factory

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/oddin-gg/gosdk/internal/cache"
	"github.com/oddin-gg/gosdk/protocols"
)

// MarketDataFactory ...
type MarketDataFactory struct {
	oddsFeedConfiguration    protocols.OddsFeedConfiguration
	marketDescriptionFactory *MarketDescriptionFactory
}

// BuildMarketData ...
func (m MarketDataFactory) BuildMarketData(event interface{}, marketID uint, specifiers map[string]string) protocols.MarketData {
	return &marketDataImpl{
		marketID:                 marketID,
		specifiers:               specifiers,
		marketDescriptionFactory: m.marketDescriptionFactory,
		event:                    event,
	}
}

// NewMarketDataFactory ...
func NewMarketDataFactory(oddsFeedConfiguration protocols.OddsFeedConfiguration, marketDescriptionFactory *MarketDescriptionFactory) *MarketDataFactory {
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

func (m marketDataImpl) OutcomeName(outcomeID string, locale protocols.Locale) (*string, error) {
	marketDescription, err := m.marketDescriptionFactory.MarketDescriptionByIDAndSpecifiers(m.marketID, m.specifiers, []protocols.Locale{locale})
	if err != nil {
		return nil, err
	}

	outcomes, err := marketDescription.Outcomes()
	if err != nil {
		return nil, err
	}

	found := false
	var outcomeName *string
	for _, outcome := range outcomes {
		if outcome.ID() == outcomeID {
			outcomeName = outcome.LocalizedName(locale)
			found = true
			break
		}
	}

	// market with dynamic outcomes can have also non-dynamic outcome, that's reason why outcome with outcomeID exists at first
	if !found && marketDescription.OutcomeType() != nil {
		switch outcomeType(*marketDescription.OutcomeType()) {
		case playerOutcomeType:
			player, err := m.marketDescriptionFactory.playerCache.GetPlayer(context.Background(), cache.PlayerCacheKey{PlayerID: outcomeID, Locale: locale})
			if err != nil {
				return nil, fmt.Errorf("derivation of outcome name for dynamic player outcome failed for id [%s]: %w", outcomeID, err)
			}
			outcomeName = &player.Name

		case competitorOutcomeType:
			urn, err := protocols.ParseURN(outcomeID)
			if err != nil {
				return nil, fmt.Errorf("unsupported competitor id in outcome %s: %w", outcomeID, err)
			}
			competitor, err := m.marketDescriptionFactory.competitorCache.Competitor(context.Background(), *urn, []protocols.Locale{locale})
			if err != nil {
				return nil, fmt.Errorf("derivation of outcome name for dynamic player outcome failed for id [%s]: %w", outcomeID, err)
			}

			name, err := competitor.LocalizedName(locale)
			if err != nil {
				return nil, fmt.Errorf("missing locale %s: %w", locale, err)
			}

			outcomeName = name

		default:
			return nil, fmt.Errorf("unsupported outcome type [%s]", *marketDescription.OutcomeType())
		}
	}

	return m.makeOutcomeName(outcomeName, locale)
}

func (m marketDataImpl) MarketName(locale protocols.Locale) (*string, error) {
	marketDescription, err := m.marketDescriptionFactory.MarketDescriptionByIDAndSpecifiers(m.marketID, m.specifiers, []protocols.Locale{locale})
	if err != nil {
		return nil, err
	}

	name, err := marketDescription.LocalizedName(locale)
	if err != nil {
		return nil, err
	}

	return m.makeMarketName(*name, locale)
}

func (m marketDataImpl) makeOutcomeName(outcomeName *string, locale protocols.Locale) (*string, error) {
	if outcomeName == nil {
		return nil, nil
	}

	match, isMatch := m.event.(protocols.Match)

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

func (m marketDataImpl) makeMarketName(marketName string, locale protocols.Locale) (*string, error) {
	if m.specifiers == nil || len(m.specifiers) == 0 {
		return &marketName, nil
	}

	match, isMatch := m.event.(protocols.Match)
	marketDescription, err := m.marketDescriptionFactory.MarketDescriptionByIDAndSpecifiers(m.marketID, m.specifiers, []protocols.Locale{locale})
	if err != nil {
		return nil, err
	}
	groups, err := marketDescription.Groups()
	if err != nil {
		return nil, err
	}

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
		if name, isPropsMarket := m.getPropsName(value, groups, locale); isPropsMarket {
			value = name
		}

		template = strings.ReplaceAll(template, key, value)
	}

	return &template, nil
}

func (m marketDataImpl) getPropsName(entityID string, groups []string, locale protocols.Locale) (string, bool) {
	if !slices.Contains(groups, protocols.MarketGroupPlayerProps) {
		return "", false
	}

	urn, err := protocols.ParseURN(entityID)
	if err != nil {
		return "", false
	}

	//nolint:gocritic // for simpler extension
	switch urn.Type {
	case string(protocols.PlayerEventType):
		player, err := m.marketDescriptionFactory.playerCache.GetPlayer(
			context.Background(),
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
