package factory

import (
	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	"github.com/oddin-gg/gosdk/protocols"
	log "github.com/sirupsen/logrus"
	"strings"
)

// MarketFactory ...
type MarketFactory struct {
	marketDataFactory *MarketDataFactory
	locales           []protocols.Locale
	logger            *log.Logger
}

// BuildMarket ...
func (m MarketFactory) BuildMarket(event interface{}, market *feedXML.MarketAttributes) protocols.Market {
	specifiersMap := m.extractSpecifiers(market.Specifiers)
	marketData := m.marketDataFactory.BuildMarketData(event, market.ID, specifiersMap)
	return marketImpl{
		id:         market.ID,
		refID:      market.RefID,
		specifiers: specifiersMap,
		marketData: marketData,
		locale:     m.locales[0],
	}
}

// BuildMarketWithOdds ...
func (m MarketFactory) BuildMarketWithOdds(event interface{}, market *feedXML.MarketWithOutcome) protocols.MarketWithOdds {
	specifiersMap := m.extractSpecifiers(market.Specifiers)
	marketData := m.marketDataFactory.BuildMarketData(event, market.ID, specifiersMap)
	outcomeOdds := make([]protocols.OutcomeOdds, len(market.Outcomes))
	for i := range market.Outcomes {
		marketOutcome := market.Outcomes[i]
		outcomeOdds[i] = m.buildOutcomeOdds(marketOutcome, marketData, m.locales[0])
	}

	return marketWithOddsImpl{
		id:               market.ID,
		refID:            market.RefID,
		specifiers:       specifiersMap,
		marketData:       marketData,
		locale:           m.locales[0],
		favourite:        market.Favourite,
		outcomeOdds:      outcomeOdds,
		feedMarketStatus: market.Status,
	}
}

// BuildMarketWithSettlement ....
func (m MarketFactory) BuildMarketWithSettlement(event interface{}, market *feedXML.MarketWithOutcome) protocols.MarketWithSettlement {
	specifiersMap := m.extractSpecifiers(market.Specifiers)
	marketData := m.marketDataFactory.BuildMarketData(event, market.ID, specifiersMap)
	outcomeSettlements := make([]protocols.OutcomeSettlement, len(market.Outcomes))
	for i := range market.Outcomes {
		marketOutcome := market.Outcomes[i]
		outcomeSettlements[i] = m.buildOutcomeSettlement(marketOutcome, marketData, m.locales[0])
	}

	return marketWithSettlementImpl{
		id:                 market.ID,
		refID:              market.RefID,
		specifiers:         specifiersMap,
		marketData:         marketData,
		locale:             m.locales[0],
		outcomeSettlements: outcomeSettlements,
	}
}

// BuildMarketCancel ...
func (m MarketFactory) BuildMarketCancel(event interface{}, market *feedXML.MarketWithoutOutcome) protocols.MarketCancel {
	specifiersMap := m.extractSpecifiers(market.Specifiers)
	marketData := m.marketDataFactory.BuildMarketData(event, market.ID, specifiersMap)

	return marketCancelImpl{
		id:               market.ID,
		refID:            market.RefID,
		specifiers:       specifiersMap,
		marketData:       marketData,
		locale:           m.locales[0],
		voidReasonID:     market.VoidReasonID,
		voidReasonParams: market.VoidReasonParams,
	}
}

func (m MarketFactory) extractSpecifiers(specifiers *string) map[string]string {
	result := make(map[string]string)
	if specifiers == nil || len(*specifiers) == 0 {
		return result
	}

	parts := strings.Split(*specifiers, "|")
	for i, part := range parts {
		variant := strings.Split(part, "=")
		if len(variant) != 2 {
			m.logger.Warnf("bad specifier size %s", parts[i])
		}

		result[variant[0]] = variant[1]
	}

	return result
}

func (m MarketFactory) buildOutcomeOdds(outcome feedXML.Outcome, marketData protocols.MarketData, locale protocols.Locale) protocols.OutcomeOdds {
	var active bool
	if outcome.Active != nil && *outcome.Active == 1 {
		active = true
	}

	return outcomeOddsImpl{
		id:          outcome.ID,
		refID:       outcome.RefID,
		probability: outcome.Probabilities,
		marketData:  marketData,
		locale:      locale,
		active:      active,
		odds:        outcome.Odds,
	}
}

func (m MarketFactory) buildOutcomeSettlement(outcome feedXML.Outcome, marketData protocols.MarketData, locale protocols.Locale) protocols.OutcomeSettlement {
	return outcomeSettlementImpl{
		id:         outcome.ID,
		refID:      outcome.RefID,
		marketData: marketData,
		locale:     locale,
		result:     outcome.Result,
		voidFactor: outcome.VoidFactor,
	}
}

// NewMarketFactory ...
func NewMarketFactory(marketDataFactory *MarketDataFactory, locales []protocols.Locale, logger *log.Logger) *MarketFactory {
	return &MarketFactory{
		marketDataFactory: marketDataFactory,
		locales:           locales,
		logger:            logger,
	}
}
