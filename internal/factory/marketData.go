package factory

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/oddin-gg/gosdk/internal/cache"
	"github.com/oddin-gg/gosdk/internal/config"
	"github.com/oddin-gg/gosdk/types"
)

// MarketDataFactory ...
type MarketDataFactory struct {
	oddsFeedConfiguration    config.Config
	marketDescriptionFactory *MarketDescriptionFactory
}

// BuildMarketData ...
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
}

func (m marketDataImpl) OutcomeName(ctx context.Context, outcomeID string, locale types.Locale) (*string, error) {
	// Request EnLocale alongside the caller's locale: the English
	// catalog label is the canonical outcome identity used to recognise
	// the home/away placeholder outcomes locale-independently (see
	// makeOutcomeName). Matching on the LOCALIZED label (pre-fix) only
	// ever worked for English — a ru/de catalog returned its generic
	// translated label instead of the localized competitor name.
	locales := []types.Locale{locale}
	if locale != types.EnLocale {
		locales = append(locales, types.EnLocale)
	}
	marketDescription, err := m.marketDescriptionFactory.MarketDescriptionByIDAndSpecifiers(ctx, m.marketID, m.specifiers, locales)
	if err != nil {
		return nil, err
	}

	found := false
	var outcomeName *string
	var canonicalName types.Optional[string]
	for _, outcome := range marketDescription.Outcomes {
		if outcome.ID == outcomeID {
			if v, ok := outcome.LocalizedName(locale).Get(); ok {
				outcomeName = &v
			}
			canonicalName = outcome.LocalizedName(types.EnLocale)
			found = true
			break
		}
	}

	// market with dynamic outcomes can have also non-dynamic outcome, that's reason why outcome with outcomeID exists at first
	if ot, ok := marketDescription.OutcomeType.Get(); !found && ok {
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

func (m marketDataImpl) MarketName(ctx context.Context, locale types.Locale) (*string, error) {
	marketDescription, err := m.marketDescriptionFactory.MarketDescriptionByIDAndSpecifiers(ctx, m.marketID, m.specifiers, []types.Locale{locale})
	if err != nil {
		return nil, err
	}

	name, ok := marketDescription.LocalizedName(locale).Get()
	if !ok {
		return nil, fmt.Errorf("missing locale %s for market %d", locale, m.marketID)
	}

	return m.makeMarketName(ctx, name, locale)
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
func (m marketDataImpl) makeOutcomeName(outcomeName *string, canonicalName types.Optional[string], locale types.Locale) (*string, error) {
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

func (m marketDataImpl) makeMarketName(ctx context.Context, marketName string, locale types.Locale) (*string, error) {
	if len(m.specifiers) == 0 {
		return &marketName, nil
	}

	match, isMatch := m.event.(types.Match)
	marketDescription, err := m.marketDescriptionFactory.MarketDescriptionByIDAndSpecifiers(ctx, m.marketID, m.specifiers, []types.Locale{locale})
	if err != nil {
		return nil, err
	}
	groups := marketDescription.Groups

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
		if name, isPropsMarket := m.getPropsName(ctx, value, groups, locale); isPropsMarket {
			value = name
		}

		template = strings.ReplaceAll(template, key, value)
	}

	return &template, nil
}

func (m marketDataImpl) getPropsName(ctx context.Context, entityID string, groups []string, locale types.Locale) (string, bool) {
	if !slices.Contains(groups, types.MarketGroupPlayerProps) {
		return "", false
	}

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
