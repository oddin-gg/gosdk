package factory

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/oddin-gg/gosdk/internal/cache"
	"github.com/oddin-gg/gosdk/internal/config"
	"github.com/oddin-gg/gosdk/types"
)

// MarketDataFactory ...
type MarketDataFactory struct {
	oddsFeedConfiguration    config.Config
	marketDescriptionFactory *MarketDescriptionFactory
}

// BuildMarketData returns the name resolver for ONE market of ONE
// message. The value memoizes its description-cache reads, so it must
// not be reused across messages (a later message must observe a catalog
// refresh); the market factory builds a fresh one per market it emits.
func (m MarketDataFactory) BuildMarketData(event interface{}, marketID int, specifiers map[string]string) types.MarketData {
	return &marketDataImpl{
		marketID:                 marketID,
		specifiers:               specifiers,
		marketDescriptionFactory: m.marketDescriptionFactory,
		event:                    event,
	}
}

// NewMarketDataFactory ...
func NewMarketDataFactory(oddsFeedConfiguration config.Config, marketDescriptionFactory *MarketDescriptionFactory) *MarketDataFactory {
	return &MarketDataFactory{
		oddsFeedConfiguration:    oddsFeedConfiguration,
		marketDescriptionFactory: marketDescriptionFactory,
	}
}

type marketDataImpl struct {
	marketID                 int
	specifiers               map[string]string
	marketDescriptionFactory *MarketDescriptionFactory
	event                    interface{}

	// descriptions memoizes the cache entry per requested locale shape
	// for the lifetime of this value (one market of one message). Name
	// resolution asks for the description once per market name per
	// locale and once per OUTCOME per locale; the cache lookup behind
	// each ask walks every outcome of the entry for its locale-coverage
	// check (twice), so per outcome it was O(N) mutex traffic and per
	// market O(N²) — the second half of the fix after the Snapshot()
	// copy. With the memo the lookup runs once per (locale, canonical)
	// shape per market; everything after it is a map read.
	//
	// Errors are memoized too: an unknown market failed the same way for
	// every outcome × locale, each attempt a fresh cache miss.
	mu           sync.Mutex
	descriptions []descriptionMemo
}

// descriptionMemo is one memoized description lookup. canonical marks
// the OutcomeName shape, which requests EnLocale alongside locale (see
// OutcomeName); MarketName requests locale alone.
type descriptionMemo struct {
	locale    types.Locale
	canonical bool
	entry     *cache.LocalizedMarketDescription
	err       error
}

// description returns the live cache entry for this market covering
// locale (and EnLocale too when canonical), memoized per shape.
func (m *marketDataImpl) description(ctx context.Context, locale types.Locale, canonical bool) (*cache.LocalizedMarketDescription, error) {
	// EnLocale alongside EnLocale is the plain shape.
	canonical = canonical && locale != types.EnLocale

	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.descriptions {
		if d := &m.descriptions[i]; d.locale == locale && d.canonical == canonical {
			return d.entry, d.err
		}
	}
	locales := []types.Locale{locale}
	if canonical {
		locales = append(locales, types.EnLocale)
	}
	entry, err := m.marketDescriptionFactory.localizedMarketDescription(ctx, m.marketID, m.specifiers, locales)
	m.descriptions = append(m.descriptions, descriptionMemo{locale: locale, canonical: canonical, entry: entry, err: err})
	return entry, err
}

func (m *marketDataImpl) OutcomeName(ctx context.Context, outcomeID string, locale types.Locale) (*string, error) {
	// Request EnLocale alongside the caller's locale: the English
	// catalog label is the canonical outcome identity used to recognise
	// the home/away placeholder outcomes locale-independently (see
	// makeOutcomeName). Matching on the LOCALIZED label (pre-fix) only
	// ever worked for English — a ru/de catalog returned its generic
	// translated label instead of the localized competitor name.
	marketDescription, err := m.description(ctx, locale, true)
	if err != nil {
		return nil, err
	}

	// Direct by-id read off the live entry, in ONE lock scope: the
	// localized name, the canonical (English) label the substitution
	// keys on, whether the entry knows the outcome at all, and the
	// market's outcome_type. Exists tells the dynamic branch below apart
	// from a known outcome the catalog does not name in this locale (→
	// None), and reading it together with outcome_type keeps a
	// concurrent catalog merge from mixing two revisions into one
	// answer. Before this fix it scanned the Outcomes slice of a full
	// Snapshot() copy — consistent, but at the cost of the copy.
	read := marketDescription.ReadOutcomeName(outcomeID, locale, types.EnLocale)

	var outcomeName *string
	var canonicalName types.Optional[string]
	if read.Exists {
		if read.NameOK {
			name := read.Name
			outcomeName = &name
		}
		if read.CanonicalOK {
			canonicalName = types.Some(read.Canonical)
		}
	}

	// market with dynamic outcomes can have also non-dynamic outcome, that's reason why outcome with outcomeID exists at first
	if ot, ok := read.OutcomeType.Get(); !read.Exists && ok {
		switch outcomeType(ot) {
		case playerOutcomeType:
			player, err := m.marketDescriptionFactory.playerCache.GetPlayer(ctx, cache.PlayerCacheKey{PlayerID: outcomeID, Locale: locale})
			if err != nil {
				return nil, fmt.Errorf("derivation of outcome name for dynamic player outcome failed for id [%s]: %w", outcomeID, err)
			}
			outcomeName = &player.Name

		case competitorOutcomeType:
			urn, err := types.ParseURN(outcomeID)
			if err != nil {
				return nil, fmt.Errorf("unsupported competitor id in outcome %s: %w", outcomeID, err)
			}
			competitor, err := m.marketDescriptionFactory.competitorCache.Competitor(ctx, *urn, []types.Locale{locale})
			if err != nil {
				return nil, fmt.Errorf("derivation of outcome name for dynamic player outcome failed for id [%s]: %w", outcomeID, err)
			}

			name, err := competitor.LocalizedName(locale)
			if err != nil {
				// LocalizedName already wraps with competitor URN +
				// locale + ErrLocaleNotLoaded; just add the
				// outcome-id breadcrumb here.
				return nil, fmt.Errorf("dynamic competitor outcome name %s: %w", outcomeID, err)
			}
			outcomeName = name

		default:
			return nil, fmt.Errorf("unsupported outcome type [%s]", ot)
		}
	}

	return m.makeOutcomeName(outcomeName, canonicalName, locale)
}

func (m *marketDataImpl) MarketName(ctx context.Context, locale types.Locale) (*string, error) {
	marketDescription, err := m.description(ctx, locale, false)
	if err != nil {
		return nil, err
	}

	name, ok := marketDescription.Name(locale)
	if !ok {
		return nil, fmt.Errorf("missing locale %s for market %d", locale, m.marketID)
	}

	return m.makeMarketName(ctx, marketDescription, name, locale)
}

// makeOutcomeName substitutes the home/away placeholder outcomes with
// the event's localized competitor names. The placeholder is recognised
// by the outcome's CANONICAL (English catalog) label — the outcome's
// only locale-independent identity in the catalog — so the substitution
// works for every requested locale. Pre-fix the check compared the
// LOCALIZED label against "home"/"away", which only matched in English:
// a ru/de consumer got the catalog's generic translated label instead
// of the team name. canonicalName falls back to the localized label
// when the en name isn't loaded (defensive; OutcomeName requests en
// explicitly), which preserves the English behaviour exactly.
func (m *marketDataImpl) makeOutcomeName(outcomeName *string, canonicalName types.Optional[string], locale types.Locale) (*string, error) {
	if outcomeName == nil {
		return nil, nil
	}

	canonical := canonicalName.ValueOr(*outcomeName)
	match, isMatch := m.event.(types.Match)

	switch {
	case canonical == "home" && isMatch && match.HomeCompetitor != nil:
		// Substitute only when the competitor name exists in this
		// locale. Pre-fix a miss stored the ValueOr("") empty string,
		// so outcome.Name(locale) returned a bogus Some("") —
		// indistinguishable from a legitimately empty name. A nil
		// return makes the resolve layer skip the locale, so the
		// accessor honestly reports None.
		if name, ok := match.HomeCompetitor.Name(locale).Get(); ok {
			return &name, nil
		}
		return nil, nil
	case canonical == "away" && isMatch && match.AwayCompetitor != nil:
		if name, ok := match.AwayCompetitor.Name(locale).Get(); ok {
			return &name, nil
		}
		return nil, nil
	default:
		return outcomeName, nil
	}
}

// makeMarketName fills the "{specifier}" placeholders of the catalog
// template from the market's specifiers. marketDescription is the entry
// MarketName already fetched — before this fix it re-fetched (and
// re-copied) the description just to read Groups.
func (m *marketDataImpl) makeMarketName(ctx context.Context, marketDescription *cache.LocalizedMarketDescription, marketName string, locale types.Locale) (*string, error) {
	if len(m.specifiers) == 0 {
		return &marketName, nil
	}

	match, isMatch := m.event.(types.Match)
	isPropsMarket := marketDescription.HasGroup(types.MarketGroupPlayerProps)

	template := marketName
	for key, value := range m.specifiers {
		key = "{" + key + "}"
		if !strings.Contains(template, key) {
			continue
		}

		switch {
		case value == "home" && isMatch && match.HomeCompetitor != nil:
			// A locale miss must not substitute "" into the template —
			// that produced names with silent holes, stored as a bogus
			// Some(""). Returning nil skips this locale entirely; the
			// accessor reports None (see makeOutcomeName).
			name, ok := match.HomeCompetitor.Name(locale).Get()
			if !ok {
				return nil, nil
			}
			value = name
		case value == "away" && isMatch && match.AwayCompetitor != nil:
			name, ok := match.AwayCompetitor.Name(locale).Get()
			if !ok {
				return nil, nil
			}
			value = name
		}

		// handle props markets
		if isPropsMarket {
			if name, ok := m.getPropsName(ctx, value, locale); ok {
				value = name
			}
		}

		template = strings.ReplaceAll(template, key, value)
	}

	return &template, nil
}

// getPropsName resolves a player-props specifier value (a player URN)
// to the player's localized name. Callers gate on the market carrying
// the player_props group.
func (m *marketDataImpl) getPropsName(ctx context.Context, entityID string, locale types.Locale) (string, bool) {
	urn, err := types.ParseURN(entityID)
	if err != nil {
		return "", false
	}

	//nolint:gocritic // for simpler extension
	switch urn.Type {
	case string(types.PlayerEventType):
		player, err := m.marketDescriptionFactory.playerCache.GetPlayer(
			ctx,
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
