package factory

import (
	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	"github.com/oddin-gg/gosdk/protocols"
	"math"
)

type outcomeOddsImpl struct {
	id          uint
	refID       *uint
	probability *float32
	marketData  protocols.MarketData
	locale      protocols.Locale
	active      bool
	odds        *float32
}

func (o outcomeOddsImpl) ID() uint {
	return o.id
}

func (o outcomeOddsImpl) RefID() *uint {
	return o.refID
}

func (o outcomeOddsImpl) Name() (*string, error) {
	return o.marketData.OutcomeName(o.id, o.locale)
}

func (o outcomeOddsImpl) LocalizedName(locale protocols.Locale) (*string, error) {
	return o.marketData.OutcomeName(o.id, locale)
}

func (o outcomeOddsImpl) IsActive() bool {
	return o.active
}

func (o outcomeOddsImpl) Probability() *float32 {
	return o.probability
}

func (o outcomeOddsImpl) Odds(displayType protocols.OddsDisplayType) *float32 {
	switch displayType {
	case protocols.AmericanOddsDisplayType:
		return o.convertToAmericanOdds(o.odds)
	default:
		return o.odds
	}
}

func (o outcomeOddsImpl) convertToAmericanOdds(odds *float32) *float32 {
	if odds == nil || math.IsNaN(float64(*odds)) {
		return odds
	}

	switch {
	case *odds == 1.0:
		return nil
	case *odds >= 2.0:
		result := *odds - 100.0
		return &result
	default:
		result := -100 / (*odds - 1)
		return &result
	}
}

type outcomeSettlementImpl struct {
	id         uint
	refID      *uint
	marketData protocols.MarketData
	locale     protocols.Locale
	result     *feedXML.OutcomeResult
}

func (o outcomeSettlementImpl) ID() uint {
	return o.id
}

func (o outcomeSettlementImpl) RefID() *uint {
	return o.refID
}

func (o outcomeSettlementImpl) Name() (*string, error) {
	return o.marketData.OutcomeName(o.id, o.locale)
}

func (o outcomeSettlementImpl) LocalizedName(locale protocols.Locale) (*string, error) {
	return o.marketData.OutcomeName(o.id, locale)
}

func (o outcomeSettlementImpl) OutcomeResult() protocols.OutcomeResult {
	switch *o.result {
	case feedXML.OutcomeResultLost:
		return protocols.LostOutcomeResult
	case feedXML.OutcomeResultWon:
		return protocols.WonOutcomeResult
	case feedXML.OutcomeResultUndecidedYet:
		return protocols.UndecidedYetOutcomeResult
	default:
		return protocols.UnknownOutcomeResult
	}
}

type marketWithOddsImpl struct {
	id               uint
	refID            *uint
	specifiers       map[string]string
	marketData       protocols.MarketData
	locale           protocols.Locale
	favourite        *bool
	outcomeOdds      []protocols.OutcomeOdds
	feedMarketStatus *feedXML.MarketStatus
}

func (m marketWithOddsImpl) ID() uint {
	return m.id
}

func (m marketWithOddsImpl) RefID() *uint {
	return m.refID
}

func (m marketWithOddsImpl) Specifiers() map[string]string {
	return m.specifiers
}

func (m marketWithOddsImpl) Name() (*string, error) {
	return m.marketData.MarketName(m.locale)
}

func (m marketWithOddsImpl) LocalizedName(locale protocols.Locale) (*string, error) {
	return m.marketData.MarketName(locale)
}

func (m marketWithOddsImpl) Status() protocols.MarketStatus {
	return ConvertFeedMarketStatus(m.feedMarketStatus)
}

func (m marketWithOddsImpl) OutcomeOdds() []protocols.OutcomeOdds {
	return m.outcomeOdds
}

func (m marketWithOddsImpl) IsFavourite() *bool {
	return m.favourite
}

// ConvertFeedMarketStatus ...
func ConvertFeedMarketStatus(marketStatus *feedXML.MarketStatus) protocols.MarketStatus {
	switch *marketStatus {
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

type marketWithSettlementImpl struct {
	id                 uint
	refID              *uint
	specifiers         map[string]string
	marketData         protocols.MarketData
	locale             protocols.Locale
	outcomeSettlements []protocols.OutcomeSettlement
}

func (m marketWithSettlementImpl) ID() uint {
	return m.id
}

func (m marketWithSettlementImpl) RefID() *uint {
	return m.refID
}

func (m marketWithSettlementImpl) Specifiers() map[string]string {
	return m.specifiers
}

func (m marketWithSettlementImpl) Name() (*string, error) {
	return m.marketData.MarketName(m.locale)
}

func (m marketWithSettlementImpl) LocalizedName(locale protocols.Locale) (*string, error) {
	return m.marketData.MarketName(locale)
}

func (m marketWithSettlementImpl) OutcomeSettlements() []protocols.OutcomeSettlement {
	return m.outcomeSettlements
}

type marketCancelImpl struct {
	id         uint
	refID      *uint
	specifiers map[string]string
	marketData protocols.MarketData
	locale     protocols.Locale
}

func (m marketCancelImpl) ID() uint {
	return m.id
}

func (m marketCancelImpl) RefID() *uint {
	return m.refID
}

func (m marketCancelImpl) Specifiers() map[string]string {
	return m.specifiers
}

func (m marketCancelImpl) Name() (*string, error) {
	return m.marketData.MarketName(m.locale)
}

func (m marketCancelImpl) LocalizedName(locale protocols.Locale) (*string, error) {
	return m.marketData.MarketName(locale)
}
