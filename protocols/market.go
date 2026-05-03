package protocols

import "context"

// MarketData ...
type MarketData interface {
	MarketName(locale Locale) (*string, error)
	OutcomeName(id string, locale Locale) (*string, error)
}

// Market ...
type Market interface {
	ID() uint
	// Deprecated: do not use this method, it will be removed in future
	RefID() *uint
	Specifiers() map[string]string
	Name() (*string, error)
	LocalizedName(locale Locale) (*string, error)
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

// MarketWithOdds ...
type MarketWithOdds interface {
	Market
	Status() MarketStatus
	OutcomeOdds() []OutcomeOdds
	IsFavourite() *bool
}

// MarketWithSettlement ...
type MarketWithSettlement interface {
	Market
	OutcomeSettlements() []OutcomeSettlement
}

// MarketCancel ...
type MarketCancel interface {
	Market
	VoidReasonID() *uint
	VoidReasonParams() *string
}

// OutcomeDescription ...
type OutcomeDescription interface {
	ID() string
	// Deprecated: do not use this method, it will be removed in future
	RefID() *uint
	LocalizedName(locale Locale) *string
	Description(locale Locale) *string
}

// Specifier ...
type Specifier interface {
	Name() string
	Type() string
}

// MarketDescription ...
type MarketDescription interface {
	ID() (uint, error)
	// Deprecated: do not use this method, it will be removed in future
	RefID() (*uint, error)
	LocalizedName(locale Locale) (*string, error)
	IncludesOutcomesOfType() *string
	OutcomeType() *string
	Outcomes() ([]OutcomeDescription, error)
	Variant() (*string, error)
	Specifiers() ([]Specifier, error)
	Groups() ([]string, error)
}

// MarketVoidReason ...
type MarketVoidReason interface {
	ID() uint
	Name() string
	Description() *string
	Template() *string
	Params() []string
}

// MarketDescriptionManager ...
//
// I/O-bearing methods take context.Context. ClearMarketDescription is
// pure-state cache invalidation and does not.
type MarketDescriptionManager interface {
	MarketDescriptions(ctx context.Context) ([]MarketDescription, error)
	MarketDescriptionByIDAndVariant(ctx context.Context, marketID uint, variant *string) (MarketDescription, error)
	LocalizedMarketDescriptions(ctx context.Context, locale Locale) ([]MarketDescription, error)
	ClearMarketDescription(marketID uint, variant *string)
	MarketVoidReasons(ctx context.Context) ([]MarketVoidReason, error)
	ReloadMarketVoidReasons(ctx context.Context) ([]MarketVoidReason, error)
}

const (
	MarketGroupPlayerProps = "player_props"
)
