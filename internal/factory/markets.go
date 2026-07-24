package factory

import (
	"context"
	"strings"

	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

// MarketFactory builds value-typed market snapshots from feed XML.
//
// Per NEXT.md §7: at message-decode time the factory enriches each
// market with descriptions for *every* configured locale (default +
// preload set). Consumers then use market.Name(locale) for O(1)
// in-memory lookups across any of those locales — returns
// Optional[string] (Some when the locale is preloaded, None
// otherwise).
type MarketFactory struct {
	marketDataFactory *MarketDataFactory
	locales           []types.Locale
	logger            *log.Logger
}

// BuildMarket ...
func (m MarketFactory) BuildMarket(ctx context.Context, event interface{}, market *feedXML.MarketAttributes) types.Market {
	specs := m.extractSpecifiers(market.Specifiers)
	md := m.marketDataFactory.BuildMarketData(event, market.ID, specs)
	return types.Market{
		ID:         market.ID,
		Specifiers: specs,
		Names:      m.resolveMarketNames(ctx, md),
	}
}

// BuildMarketWithOdds ...
func (m MarketFactory) BuildMarketWithOdds(ctx context.Context, event interface{}, market *feedXML.MarketWithOutcome) types.MarketWithOdds {
	specs := m.extractSpecifiers(market.Specifiers)
	md := m.marketDataFactory.BuildMarketData(event, market.ID, specs)
	odds := make([]types.OutcomeOdds, len(market.Outcomes))
	for i := range market.Outcomes {
		odds[i] = m.buildOutcomeOdds(ctx, market.Outcomes[i], md)
	}
	return types.MarketWithOdds{
		Market: types.Market{
			ID:         market.ID,
			Specifiers: specs,
			Names:      m.resolveMarketNames(ctx, md),
		},
		Status:      ConvertFeedMarketStatus(market.Status),
		IsFavourite: types.FromPtr(market.Favourite),
		OutcomeOdds: odds,
	}
}

// BuildMarketWithSettlement ...
func (m MarketFactory) BuildMarketWithSettlement(ctx context.Context, event interface{}, market *feedXML.MarketWithOutcome) types.MarketWithSettlement {
	specs := m.extractSpecifiers(market.Specifiers)
	md := m.marketDataFactory.BuildMarketData(event, market.ID, specs)
	settlements := make([]types.OutcomeSettlement, len(market.Outcomes))
	for i := range market.Outcomes {
		settlements[i] = m.buildOutcomeSettlement(ctx, market.Outcomes[i], md)
	}
	return types.MarketWithSettlement{
		Market: types.Market{
			ID:         market.ID,
			Specifiers: specs,
			Names:      m.resolveMarketNames(ctx, md),
		},
		OutcomeSettlements: settlements,
		VoidReasonID:       types.FromPtr(market.VoidReasonID),
		VoidReasonParams:   types.FromPtr(market.VoidReasonParams),
	}
}

// BuildMarketCancel ...
func (m MarketFactory) BuildMarketCancel(ctx context.Context, event interface{}, market *feedXML.MarketWithoutOutcome) types.MarketCancel {
	specs := m.extractSpecifiers(market.Specifiers)
	md := m.marketDataFactory.BuildMarketData(event, market.ID, specs)
	return types.MarketCancel{
		Market: types.Market{
			ID:         market.ID,
			Specifiers: specs,
			Names:      m.resolveMarketNames(ctx, md),
		},
		VoidReasonID:     types.FromPtr(market.VoidReasonID),
		VoidReasonParams: types.FromPtr(market.VoidReasonParams),
	}
}

func (m MarketFactory) extractSpecifiers(specifiers *string) map[string]string {
	result := make(map[string]string)
	if specifiers == nil || len(*specifiers) == 0 {
		return result
	}
	parts := strings.Split(*specifiers, "|")
	for _, part := range parts {
		// strings.Cut splits on the FIRST '=' so a specifier value
		// containing '=' (e.g. an opaque base64-ish payload from a
		// future protocol revision) is preserved verbatim. Pre-v2.24
		// strings.Split(part, "=") produced a >2 slice and the
		// length check rejected the whole specifier.
		key, value, ok := strings.Cut(part, "=")
		if !ok || key == "" {
			m.logger.Warnf("bad specifier %s", part)
			continue
		}
		result[key] = value
	}
	return result
}

func (m MarketFactory) buildOutcomeOdds(ctx context.Context, outcome feedXML.Outcome, md types.MarketData) types.OutcomeOdds {
	active := outcome.Active != nil && *outcome.Active == 1
	return types.OutcomeOdds{
		Outcome: types.Outcome{
			ID:    outcome.ID,
			Names: m.resolveOutcomeNames(ctx, md, outcome.ID),
		},
		IsActive:    active,
		Probability: types.FromPtr(outcome.Probabilities),
		DecimalOdds: types.FromPtr(outcome.Odds),
	}
}

func (m MarketFactory) buildOutcomeSettlement(ctx context.Context, outcome feedXML.Outcome, md types.MarketData) types.OutcomeSettlement {
	var result types.OutcomeResult
	if outcome.Result != nil {
		switch *outcome.Result {
		case feedXML.OutcomeResultLost:
			result = types.LostOutcomeResult
		case feedXML.OutcomeResultWon:
			result = types.WonOutcomeResult
		case feedXML.OutcomeResultUndecidedYet:
			result = types.UndecidedYetOutcomeResult
		default:
			result = types.UnknownOutcomeResult
		}
	}

	voidFactor := types.None[types.VoidFactor]()
	if outcome.VoidFactor != nil {
		switch *outcome.VoidFactor {
		case 0.5:
			voidFactor = types.Some(types.VoidFactorRefundHalf)
		case 1.0:
			voidFactor = types.Some(types.VoidFactorRefundFull)
		}
	}

	return types.OutcomeSettlement{
		Outcome: types.Outcome{
			ID:    outcome.ID,
			Names: m.resolveOutcomeNames(ctx, md, outcome.ID),
		},
		OutcomeResult: result,
		VoidFactor:    voidFactor,
	}
}

// resolveMarketNames fans out across every configured locale and
// returns a per-locale name map. Locales that fail to resolve are
// omitted from the map → market.Name(loc) returns None for them
// (consumer can prime the cache via Client.MarketDescription).
// Loaded-but-empty names ARE preserved (the map carries an empty
// string entry) so market.Name(loc) returns Some("") — distinct
// from None per the Optional[string] contract.
func (m MarketFactory) resolveMarketNames(ctx context.Context, md types.MarketData) map[types.Locale]string {
	names := make(map[types.Locale]string, len(m.locales))
	for _, l := range m.locales {
		if name, ok := resolveMarketName(ctx, md, l); ok {
			names[l] = name
		}
	}
	return names
}

// resolveOutcomeNames is the per-outcome equivalent of
// resolveMarketNames. Loaded-but-empty names are preserved as
// Some("") (the map gets the empty entry).
func (m MarketFactory) resolveOutcomeNames(ctx context.Context, md types.MarketData, outcomeID string) map[types.Locale]string {
	names := make(map[types.Locale]string, len(m.locales))
	for _, l := range m.locales {
		if name, ok := resolveOutcomeName(ctx, md, outcomeID, l); ok {
			names[l] = name
		}
	}
	return names
}

// resolveMarketName looks up the market name in the description cache.
// Returns (name, true) when the cache returned a value (including
// loaded-but-empty), (zero, false) when no description was available
// or the lookup errored. Errors are swallowed by design — the
// factory is on the AMQP hot path and a missing description shouldn't
// fail the entire message decode; consumers can fetch the description
// directly via Client.MarketDescription if needed.
func resolveMarketName(ctx context.Context, md types.MarketData, locale types.Locale) (string, bool) {
	if md == nil {
		return "", false
	}
	name, err := md.MarketName(ctx, locale)
	if err != nil || name == nil {
		return "", false
	}
	return *name, true
}

func resolveOutcomeName(ctx context.Context, md types.MarketData, outcomeID string, locale types.Locale) (string, bool) {
	if md == nil {
		return "", false
	}
	name, err := md.OutcomeName(ctx, outcomeID, locale)
	if err != nil || name == nil {
		return "", false
	}
	return *name, true
}

// NewMarketFactory ...
func NewMarketFactory(marketDataFactory *MarketDataFactory, locales []types.Locale, logger *log.Logger) *MarketFactory {
	return &MarketFactory{
		marketDataFactory: marketDataFactory,
		locales:           locales,
		logger:            logger,
	}
}

// ConvertFeedMarketStatus exposes the feed-status → public-status
// mapping for callers that build markets outside this factory.
func ConvertFeedMarketStatus(status *feedXML.MarketStatus) types.MarketStatus {
	if status == nil {
		return types.UnknownMarketStatus
	}
	switch *status {
	case feedXML.MarketStatusActive:
		return types.ActiveMarketStatus
	case feedXML.MarketStatusDeactived:
		return types.DeactivatedMarketStatus
	case feedXML.MarketStatusSuspended:
		return types.SuspendedMarketStatus
	case feedXML.MarketStatusHandedOver:
		return types.HandedOverMarketStatus
	case feedXML.MarketStatusSettled:
		return types.SettledMarketStatus
	case feedXML.MarketStatusCancelled:
		return types.CancelledMarketStatus
	default:
		return types.UnknownMarketStatus
	}
}
