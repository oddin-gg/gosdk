package factory

import (
	"fmt"
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
			player, err := m.marketDescriptionFactory.playerCache.GetPlayer(cache.PlayerCacheKey{PlayerID: outcomeID, Locale: locale})
			if err != nil {
				return nil, fmt.Errorf("derivation of outcome name for dynamic player outcome failed for id [%s]: %w", outcomeID, err)
			}
			outcomeName = &player.LocalizedName

		case competitorOutcomeType:
			urn, err := protocols.ParseURN(outcomeID)
			if err != nil {
				return nil, fmt.Errorf("unsupported competitor id in outcome %s: %w", outcomeID, err)
			}
			competitor, err := m.marketDescriptionFactory.competitorCache.Competitor(*urn, []protocols.Locale{locale})
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
	case *outcomeName == "home" && isMatch:
		home, err := match.HomeCompetitor()
		if err != nil {
			return nil, err
		}
		return home.LocalizedName(locale)
		// @TODO this broke with different locale - need to use ID
	case *outcomeName == "away" && isMatch:
		away, err := match.AwayCompetitor()
		if err != nil {
			return nil, err
		}
		return away.LocalizedName(locale)
	default:
		return outcomeName, nil
	}
}

func (m marketDataImpl) makeMarketName(marketName string, locale protocols.Locale) (*string, error) {
	if m.specifiers == nil || len(m.specifiers) == 0 {
		return &marketName, nil
	}

	match, isMatch := m.event.(protocols.Match)

	template := marketName
	for key, value := range m.specifiers {
		key = "{" + key + "}"
		if !strings.Contains(template, key) {
			continue
		}

		switch {
		case value == "home" && isMatch:
			home, err := match.HomeCompetitor()
			if err != nil {
				return nil, err
			}
			name, err := home.LocalizedName(locale)
			if err != nil {
				return nil, err
			}
			value = *name
		case value == "away" && isMatch:
			away, err := match.AwayCompetitor()
			if err != nil {
				return nil, err
			}
			name, err := away.LocalizedName(locale)
			if err != nil {
				return nil, err
			}
			value = *name
		}

		template = strings.ReplaceAll(template, key, value)
	}

	return &template, nil
}
