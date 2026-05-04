package factory

import (
	"strings"

	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	"github.com/oddin-gg/gosdk/protocols"
	log "github.com/oddin-gg/gosdk/internal/log"
)

// MarketFactory builds value-typed market snapshots from feed XML.
type MarketFactory struct {
	marketDataFactory *MarketDataFactory
	locales           []protocols.Locale
	logger            *log.Logger
}

// BuildMarket ...
func (m MarketFactory) BuildMarket(event interface{}, market *feedXML.MarketAttributes) protocols.Market {
	specs := m.extractSpecifiers(market.Specifiers)
	md := m.marketDataFactory.BuildMarketData(event, market.ID, specs)
	return protocols.Market{
		ID:         market.ID,
		Specifiers: specs,
		Name:       resolveMarketName(md, m.locales[0]),
	}
}

// BuildMarketWithOdds ...
func (m MarketFactory) BuildMarketWithOdds(event interface{}, market *feedXML.MarketWithOutcome) protocols.MarketWithOdds {
	specs := m.extractSpecifiers(market.Specifiers)
	md := m.marketDataFactory.BuildMarketData(event, market.ID, specs)
	odds := make([]protocols.OutcomeOdds, len(market.Outcomes))
	for i := range market.Outcomes {
		odds[i] = m.buildOutcomeOdds(market.Outcomes[i], md, m.locales[0])
	}
	return protocols.MarketWithOdds{
		Market: protocols.Market{
			ID:         market.ID,
			Specifiers: specs,
			Name:       resolveMarketName(md, m.locales[0]),
		},
		Status:      ConvertFeedMarketStatus(market.Status),
		IsFavourite: market.Favourite,
		OutcomeOdds: odds,
	}
}

// BuildMarketWithSettlement ...
func (m MarketFactory) BuildMarketWithSettlement(event interface{}, market *feedXML.MarketWithOutcome) protocols.MarketWithSettlement {
	specs := m.extractSpecifiers(market.Specifiers)
	md := m.marketDataFactory.BuildMarketData(event, market.ID, specs)
	settlements := make([]protocols.OutcomeSettlement, len(market.Outcomes))
	for i := range market.Outcomes {
		settlements[i] = m.buildOutcomeSettlement(market.Outcomes[i], md, m.locales[0])
	}
	return protocols.MarketWithSettlement{
		Market: protocols.Market{
			ID:         market.ID,
			Specifiers: specs,
			Name:       resolveMarketName(md, m.locales[0]),
		},
		OutcomeSettlements: settlements,
	}
}

// BuildMarketCancel ...
func (m MarketFactory) BuildMarketCancel(event interface{}, market *feedXML.MarketWithoutOutcome) protocols.MarketCancel {
	specs := m.extractSpecifiers(market.Specifiers)
	md := m.marketDataFactory.BuildMarketData(event, market.ID, specs)
	return protocols.MarketCancel{
		Market: protocols.Market{
			ID:         market.ID,
			Specifiers: specs,
			Name:       resolveMarketName(md, m.locales[0]),
		},
		VoidReasonID:     market.VoidReasonID,
		VoidReasonParams: market.VoidReasonParams,
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
			continue
		}
		result[variant[0]] = variant[1]
	}
	return result
}

func (m MarketFactory) buildOutcomeOdds(outcome feedXML.Outcome, md protocols.MarketData, locale protocols.Locale) protocols.OutcomeOdds {
	active := outcome.Active != nil && *outcome.Active == 1
	return protocols.OutcomeOdds{
		Outcome: protocols.Outcome{
			ID:   outcome.ID,
			Name: resolveOutcomeName(md, outcome.ID, locale),
		},
		IsActive:    active,
		Probability: outcome.Probabilities,
		DecimalOdds: outcome.Odds,
	}
}

func (m MarketFactory) buildOutcomeSettlement(outcome feedXML.Outcome, md protocols.MarketData, locale protocols.Locale) protocols.OutcomeSettlement {
	var result protocols.OutcomeResult
	if outcome.Result != nil {
		switch *outcome.Result {
		case feedXML.OutcomeResultLost:
			result = protocols.LostOutcomeResult
		case feedXML.OutcomeResultWon:
			result = protocols.WonOutcomeResult
		case feedXML.OutcomeResultUndecidedYet:
			result = protocols.UndecidedYetOutcomeResult
		default:
			result = protocols.UnknownOutcomeResult
		}
	}

	var voidFactor *protocols.VoidFactor
	if outcome.VoidFactor != nil {
		switch *outcome.VoidFactor {
		case 0.5:
			v := protocols.VoidFactorRefundHalf
			voidFactor = &v
		case 1.0:
			v := protocols.VoidFactorRefundFull
			voidFactor = &v
		}
	}

	return protocols.OutcomeSettlement{
		Outcome: protocols.Outcome{
			ID:   outcome.ID,
			Name: resolveOutcomeName(md, outcome.ID, locale),
		},
		OutcomeResult: result,
		VoidFactor:    voidFactor,
	}
}

// resolveMarketName looks up the market name in the description cache,
// returning "" when unavailable. Errors are swallowed by design — the
// factory is on the AMQP hot path and a missing description shouldn't
// fail the entire message decode; consumers can fetch the description
// directly via Client.MarketDescription if needed.
func resolveMarketName(md protocols.MarketData, locale protocols.Locale) string {
	if md == nil {
		return ""
	}
	name, err := md.MarketName(locale)
	if err != nil || name == nil {
		return ""
	}
	return *name
}

func resolveOutcomeName(md protocols.MarketData, outcomeID string, locale protocols.Locale) string {
	if md == nil {
		return ""
	}
	name, err := md.OutcomeName(outcomeID, locale)
	if err != nil || name == nil {
		return ""
	}
	return *name
}

// NewMarketFactory ...
func NewMarketFactory(marketDataFactory *MarketDataFactory, locales []protocols.Locale, logger *log.Logger) *MarketFactory {
	return &MarketFactory{
		marketDataFactory: marketDataFactory,
		locales:           locales,
		logger:            logger,
	}
}

// ConvertFeedMarketStatus exposes the feed-status → public-status
// mapping for callers that build markets outside this factory.
func ConvertFeedMarketStatus(status *feedXML.MarketStatus) protocols.MarketStatus {
	if status == nil {
		return protocols.UnknownMarketStatus
	}
	switch *status {
	case feedXML.MarketStatusActive:
		return protocols.ActiveMarketStatus
	case feedXML.MarketStatusDeactived:
		return protocols.DeactivatedMarketStatus
	case feedXML.MarketStatusSuspended:
		return protocols.SuspendedMarketStatus
	case feedXML.MarketStatusHandedOver:
		return protocols.HandedOverMarketStatus
	case feedXML.MarketStatusSettled:
		return protocols.SettledMarketStatus
	case feedXML.MarketStatusCancelled:
		return protocols.CancelledMarketStatus
	default:
		return protocols.UnknownMarketStatus
	}
}

