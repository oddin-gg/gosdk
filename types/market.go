package types

import "context"

// MarketData is the internal lookup shim used by message-construction
// code to resolve market and outcome names against the description
// cache. Not consumer-facing; consumers see resolved names on the
// Market / OutcomeOdds value structs.
//
// NOT part of the supported v1 API: exported only because internal
// packages exchange it across package boundaries. It may change or
// move under internal/ in any release without notice — do not
// implement or depend on it.
type MarketData interface {
	MarketName(ctx context.Context, locale Locale) (*string, error)
	OutcomeName(ctx context.Context, id string, locale Locale) (*string, error)
}

// Market is the base market shape carried inside messages.
//
// Phase 6.1 reshape: replaces the previous Market interface with a
// value struct. Names is populated at message-construction time for
// every locale in `WithPreloadLocales(...)` (and the default locale)
// WHOSE NAME RESOLVED — a locale the description catalog cannot supply
// is omitted from the map rather than mapped to an empty string, so
// Name(locale) reports None for it.
//
// v2.x reshape: the parallel accessors `LocalizedName(locale) *string`
// (nil on miss) and `Name(locale) string` (empty on miss) collapsed
// into a single `Name(locale) Optional[string]`. `.ValueOr("")` keeps
// the always-string ergonomics; `.Get()` detects "not preloaded".
type Market struct {
	ID         int
	Specifiers map[string]string
	// Names is the per-locale market name map: an entry per requested
	// locale (default + WithPreloadLocales) whose name resolved from
	// the description catalog. A locale the catalog cannot supply is
	// OMITTED (never an empty-string entry) — including, in the
	// degenerate case, the default locale.
	Names map[Locale]string
}

// Name returns the localized market name, or None when the locale
// wasn't preloaded at message-decode time. To get a market name in a
// locale not in WithPreloadLocales, the consumer must call
// Client.MarketDescription(ctx, id, variant, locale) first to prime
// the cache — but that fills the description cache, not previously-
// decoded messages (per NEXT.md §7).
func (m Market) Name(locale Locale) Optional[string] {
	if v, ok := m.Names[locale]; ok {
		return Some(v)
	}
	return None[string]()
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
//
// v2.27 reshape: IsFavourite migrated from *bool to Optional[bool].
type MarketWithOdds struct {
	Market
	Status      MarketStatus
	IsFavourite Optional[bool]
	OutcomeOdds []OutcomeOdds
}

// MarketWithSettlement is a market with settlement outcomes attached.
//
// v2.x reshape: VoidReasonID + VoidReasonParams added for parity with
// netcoresdk's IMarketWithSettlement (which inherits them from
// IMarketCancel). The wire schema carries void_reason_id /
// void_reason_params on bet_settlement markets; pre-fix the Go
// decoder ignored them. None when the upstream feed didn't include
// the attribute. The deprecated single-int `void_reason` attribute
// is intentionally not exposed — javasdk marks it @Deprecated and
// returns null in MarketWithSettlementImpl, origin/main Go never
// exposed it, and the modern ID + Params fields cover the same data.
type MarketWithSettlement struct {
	Market
	OutcomeSettlements []OutcomeSettlement
	VoidReasonID       Optional[int]
	VoidReasonParams   Optional[string]
}

// MarketCancel is a market in a BetCancel message — carries the void
// reason for the cancellation.
//
// v2.27 reshape: VoidReasonID and VoidReasonParams migrated from
// *uint / *string to Optional[int] / Optional[string].
type MarketCancel struct {
	Market
	VoidReasonID     Optional[int]
	VoidReasonParams Optional[string]
}

// OutcomeDescription is a static-catalog outcome description, populated
// across one or more locales.
type OutcomeDescription struct {
	ID           string
	Names        map[Locale]string
	Descriptions map[Locale]string
}

// LocalizedName returns the localized name, or None if the locale
// wasn't loaded.
//
// v2.x reshape: migrated from `*string` (nil on miss) to
// `Optional[string]` for consistency with the rest of the SDK's
// "maybe-loaded" idiom. `.ValueOr("")` reproduces the previous
// non-nullable convenience.
func (o OutcomeDescription) LocalizedName(locale Locale) Optional[string] {
	if v, ok := o.Names[locale]; ok {
		return Some(v)
	}
	return None[string]()
}

// Description returns the localized description, or None.
//
// v2.x reshape: migrated from `*string` to `Optional[string]`.
func (o OutcomeDescription) Description(locale Locale) Optional[string] {
	if v, ok := o.Descriptions[locale]; ok {
		return Some(v)
	}
	return None[string]()
}

// Specifier is a typed parameter on a market description (e.g. "score=1:1").
type Specifier struct {
	Name string
	Type string
}

// MarketDescription is a static-catalog market description, populated
// across one or more locales.
//
// v2.28 reshape: Variant / IncludesOutcomesOfType / OutcomeType
// migrated from *string to Optional[string]. Closes the snapshot
// pointer-aliasing path the v2.25 clonePtr fix worked around.
type MarketDescription struct {
	ID                     int
	Names                  map[Locale]string
	Variant                Optional[string]
	IncludesOutcomesOfType Optional[string]
	OutcomeType            Optional[string]
	Outcomes               []OutcomeDescription
	Specifiers             []Specifier
	Groups                 []string
}

// LocalizedName returns the localized market description name, or
// None if the locale wasn't loaded.
//
// v2.x reshape: migrated from `*string` to `Optional[string]`.
func (m MarketDescription) LocalizedName(locale Locale) Optional[string] {
	if v, ok := m.Names[locale]; ok {
		return Some(v)
	}
	return None[string]()
}

// MarketVoidReason is a void-reasons catalog entry.
//
// v2.28 reshape: Description / Template migrated from *string to
// Optional[string].
type MarketVoidReason struct {
	ID          int
	Name        string
	Description Optional[string]
	Template    Optional[string]
	Params      []string
}

// (The market-description behavioural interface lives unexported in
// the gosdk root package as of v2.25 — types/ is data-shape-only.)

// Market group identifiers carried in MarketDescription.Groups.
const (
	// MarketGroupPlayerProps marks markets whose specifiers reference
	// individual players (e.g., "first goal scorer", "player to score").
	// Triggers per-player name resolution at message-construction time.
	MarketGroupPlayerProps = "player_props"
)
