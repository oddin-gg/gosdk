package types

import "context"

// MarketData is the internal lookup shim used by message-construction
// code to resolve market and outcome names against the description
// cache. Not consumer-facing; consumers see resolved names on the
// Market / OutcomeOdds value structs.
type MarketData interface {
	MarketName(locale Locale) (*string, error)
	OutcomeName(id string, locale Locale) (*string, error)
}

// Market is the base market shape carried inside messages.
//
// Phase 6.1 reshape: replaces the previous Market interface with a
// value struct. Name is resolved at message-construction time in the
// SDK's default locale (callers needing a different locale go through
// Client.MarketDescription with the configured market id + specifiers).
type Market struct {
	ID         uint
	Specifiers map[string]string
	Name       string
}

// MarketStatus ...
type MarketStatus int

// MarketStatuses
const (
	ActiveMarketStatus      MarketStatus = 1
	SuspendedMarketStatus   MarketStatus = 2
	DeactivatedMarketStatus MarketStatus = 3
	SettledMarketStatus     MarketStatus = 4
	CancelledMarketStatus   MarketStatus = 5
	HandedOverMarketStatus  MarketStatus = 6
	UnknownMarketStatus     MarketStatus = 0
)

// MarketWithOdds is a market with live odds attached.
type MarketWithOdds struct {
	Market
	Status      MarketStatus
	IsFavourite *bool
	OutcomeOdds []OutcomeOdds
}

// MarketWithSettlement is a market with settlement outcomes attached.
type MarketWithSettlement struct {
	Market
	OutcomeSettlements []OutcomeSettlement
}

// MarketCancel is a market in a BetCancel message — carries the void
// reason for the cancellation.
type MarketCancel struct {
	Market
	VoidReasonID     *uint
	VoidReasonParams *string
}

// OutcomeDescription is a static-catalog outcome description, populated
// across one or more locales.
type OutcomeDescription struct {
	ID           string
	Names        map[Locale]string
	Descriptions map[Locale]string
}

// LocalizedName returns the localized name, or nil if the locale wasn't
// loaded.
func (o OutcomeDescription) LocalizedName(locale Locale) *string {
	if v, ok := o.Names[locale]; ok {
		return &v
	}
	return nil
}

// Description returns the localized description, or nil.
func (o OutcomeDescription) Description(locale Locale) *string {
	if v, ok := o.Descriptions[locale]; ok {
		return &v
	}
	return nil
}

// Specifier is a typed parameter on a market description (e.g. "score=1:1").
type Specifier struct {
	Name string
	Type string
}

// MarketDescription is a static-catalog market description, populated
// across one or more locales.
type MarketDescription struct {
	ID                     uint
	Names                  map[Locale]string
	Variant                *string
	IncludesOutcomesOfType *string
	OutcomeType            *string
	Outcomes               []OutcomeDescription
	Specifiers             []Specifier
	Groups                 []string
}

// LocalizedName returns the localized market description name, or nil
// if the locale wasn't loaded.
func (m MarketDescription) LocalizedName(locale Locale) *string {
	if v, ok := m.Names[locale]; ok {
		return &v
	}
	return nil
}

// MarketVoidReason is a void-reasons catalog entry.
type MarketVoidReason struct {
	ID          uint
	Name        string
	Description *string
	Template    *string
	Params      []string
}

// MarketDescriptionManager ...
//
// Phase 6.1 reshape: returns value-typed MarketDescription /
// MarketVoidReason directly (the previous interfaces with lazy-load
// accessors are gone).
type MarketDescriptionManager interface {
	MarketDescriptions(ctx context.Context) ([]MarketDescription, error)
	MarketDescriptionByIDAndVariant(ctx context.Context, marketID uint, variant *string) (*MarketDescription, error)
	LocalizedMarketDescriptions(ctx context.Context, locale Locale) ([]MarketDescription, error)
	ClearMarketDescription(marketID uint, variant *string)
	MarketVoidReasons(ctx context.Context) ([]MarketVoidReason, error)
	ReloadMarketVoidReasons(ctx context.Context) ([]MarketVoidReason, error)
}

const (
	MarketGroupPlayerProps = "player_props"
)
