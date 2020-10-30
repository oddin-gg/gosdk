package protocols

// MarketData ...
type MarketData interface {
	MarketName(locale Locale) (*string, error)
	OutcomeName(id uint, locale Locale) (*string, error)
}

// Market ...
type Market interface {
	ID() uint
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
}

// OutcomeDescription ...
type OutcomeDescription interface {
	ID() uint
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
	RefID() (*uint, error)
	LocalizedName(locale Locale) (*string, error)
	Outcomes() ([]OutcomeDescription, error)
	Variant() (*string, error)
	Specifiers() ([]Specifier, error)
}

// MarketDescriptionManager ...
type MarketDescriptionManager interface {
	MarketDescriptions() ([]MarketDescription, error)
	LocalizedMarketDescriptions(locale Locale) ([]MarketDescription, error)
	ClearMarketDescription(marketID uint, variant *string)
}
