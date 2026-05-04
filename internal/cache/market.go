package cache

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/oddin-gg/gosdk/internal/api"
	data "github.com/oddin-gg/gosdk/internal/api/xml"
	"github.com/oddin-gg/gosdk/internal/utils"
	"github.com/oddin-gg/gosdk/types"
)

// CompositeKey identifies a market description: marketID + optional variant.
type CompositeKey struct {
	MarketID uint
	Variant  *string
}

// String renders the key for logs/diagnostics.
func (k CompositeKey) String() string {
	v := "*"
	if k.Variant != nil {
		v = *k.Variant
	}
	return fmt.Sprintf("%d-%s", k.MarketID, v)
}

func variantKey(marketID uint, variant *string) CompositeKey {
	if variant == nil {
		return CompositeKey{MarketID: marketID}
	}
	v := *variant
	return CompositeKey{MarketID: marketID, Variant: &v}
}

// MarketDescriptionCache stores market descriptions per (marketID, variant)
// composite key. Each entry holds per-locale name/outcome data.
//
// Phase 3 rewrite: replaces patrickmn/go-cache with a sync.RWMutex-protected
// map. Each LocalizedMarketDescription has its own mu covering every field.
type MarketDescriptionCache struct {
	apiClient *api.Client

	mu            sync.RWMutex
	loadedLocales map[types.Locale]struct{}
	descriptions  map[CompositeKey]*LocalizedMarketDescription

	loadMu sync.Mutex // serializes concurrent API loads
}

// LocalizedMarketDescriptions returns every cached description that contains
// data for the given locale, fetching the locale's full catalog if not yet
// loaded.
func (m *MarketDescriptionCache) LocalizedMarketDescriptions(ctx context.Context, locale types.Locale) (map[CompositeKey]*LocalizedMarketDescription, error) {
	if !m.localeLoaded(locale) {
		if err := m.loadAll(ctx, []types.Locale{locale}); err != nil {
			return nil, err
		}
	}

	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[CompositeKey]*LocalizedMarketDescription, len(m.descriptions))
	for k, v := range m.descriptions {
		if v.hasLocale(locale) {
			out[k] = v
		}
	}
	return out, nil
}

// MarketDescriptionByID returns the description for (marketID, variant),
// loading missing locales as needed.
func (m *MarketDescriptionCache) MarketDescriptionByID(
	ctx context.Context,
	marketID uint,
	variant *string,
	locales []types.Locale,
) (*LocalizedMarketDescription, error) {
	key := variantKey(marketID, variant)

	m.mu.RLock()
	entry := m.descriptions[key]
	m.mu.RUnlock()

	missing := locales
	if entry != nil {
		missing = entry.missingLocales(locales)
	}
	if len(missing) > 0 {
		if err := m.loadOne(ctx, &marketID, variant, missing); err != nil {
			return nil, err
		}
		m.mu.RLock()
		entry = m.descriptions[key]
		m.mu.RUnlock()
		if entry == nil {
			return nil, fmt.Errorf("market description not found: %s", key)
		}
	}
	return entry, nil
}

// ClearCacheItem evicts a single description.
func (m *MarketDescriptionCache) ClearCacheItem(marketID uint, variant *string) {
	m.mu.Lock()
	delete(m.descriptions, variantKey(marketID, variant))
	m.mu.Unlock()
}

// Purge clears the entire cache.
func (m *MarketDescriptionCache) Purge() {
	m.mu.Lock()
	m.descriptions = make(map[CompositeKey]*LocalizedMarketDescription)
	m.loadedLocales = make(map[types.Locale]struct{})
	m.mu.Unlock()
}

func (m *MarketDescriptionCache) localeLoaded(locale types.Locale) bool {
	m.mu.RLock()
	_, ok := m.loadedLocales[locale]
	m.mu.RUnlock()
	return ok
}

func (m *MarketDescriptionCache) loadAll(ctx context.Context, locales []types.Locale) error {
	return m.loadOne(ctx, nil, nil, locales)
}

func (m *MarketDescriptionCache) loadOne(ctx context.Context, marketID *uint, variant *string, locales []types.Locale) error {
	m.loadMu.Lock()
	defer m.loadMu.Unlock()

	for _, locale := range locales {
		var (
			descriptions []data.MarketDescription
			err          error
		)
		if marketID != nil && variant != nil && utils.IsMarketVariantWithDynamicOutcomes(*variant) {
			descriptions, err = m.apiClient.FetchMarketDescriptionsWithDynamicOutcomes(ctx, *marketID, *variant, locale)
		} else {
			descriptions, err = m.apiClient.FetchMarketDescriptions(ctx, locale)
		}
		if err != nil {
			return err
		}

		for k := range descriptions {
			if err := m.upsert(descriptions[k], locale); err != nil {
				return err
			}
		}

		// Only the bulk fetch counts as fully loading the locale. A single-id
		// dynamic-variant fetch covers exactly that key.
		if marketID == nil {
			m.mu.Lock()
			m.loadedLocales[locale] = struct{}{}
			m.mu.Unlock()
		}
	}
	return nil
}

func (m *MarketDescriptionCache) upsert(description data.MarketDescription, locale types.Locale) error {
	key := variantKey(description.ID, description.Variant)

	m.mu.Lock()
	entry, ok := m.descriptions[key]
	if !ok {
		if description.Outcomes == nil {
			m.mu.Unlock()
			return fmt.Errorf("missing outcomes in %v", description)
		}
		outcomes := make(map[string]*LocalizedOutcomeDescription, len(description.Outcomes.Outcome))
		for _, o := range description.Outcomes.Outcome {
			outcomes[o.ID] = &LocalizedOutcomeDescription{
				name:        make(map[types.Locale]string),
				description: make(map[types.Locale]string),
			}
		}
		entry = &LocalizedMarketDescription{
			id:                     description.ID,
			variant:                description.Variant,
			IncludesOutcomesOfType: description.IncludesOutcomesOfType,
			OutcomeType:            description.OutcomeType,
			outcomes:               outcomes,
			name:                   make(map[types.Locale]string),
			groups:                 strings.Split(description.Groups, "|"),
		}
		m.descriptions[key] = entry
	}
	m.mu.Unlock()

	entry.merge(description, locale)
	return nil
}

func newMarketDescriptionCache(client *api.Client) *MarketDescriptionCache {
	return &MarketDescriptionCache{
		apiClient:     client,
		loadedLocales: make(map[types.Locale]struct{}),
		descriptions:  make(map[CompositeKey]*LocalizedMarketDescription),
	}
}

// LocalizedMarketDescription stores per-(market, variant) description data
// across multiple locales. mu guards all fields.
type LocalizedMarketDescription struct {
	mu sync.RWMutex

	id                     uint
	variant                *string
	IncludesOutcomesOfType *string
	OutcomeType            *string
	outcomes               map[string]*LocalizedOutcomeDescription
	specifiers             []types.Specifier
	name                   map[types.Locale]string
	groups                 []string
}

func (d *LocalizedMarketDescription) hasLocale(locale types.Locale) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	_, ok := d.name[locale]
	return ok
}

func (d *LocalizedMarketDescription) missingLocales(locales []types.Locale) []types.Locale {
	d.mu.RLock()
	defer d.mu.RUnlock()
	var missing []types.Locale
	for _, l := range locales {
		if _, ok := d.name[l]; !ok {
			missing = append(missing, l)
		}
	}
	return missing
}

func (d *LocalizedMarketDescription) merge(description data.MarketDescription, locale types.Locale) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if description.Outcomes != nil {
		for _, outcome := range description.Outcomes.Outcome {
			lo, ok := d.outcomes[outcome.ID]
			if !ok {
				// New outcome on a fresh fetch — add it.
				lo = &LocalizedOutcomeDescription{
					name:        make(map[types.Locale]string),
					description: make(map[types.Locale]string),
				}
				d.outcomes[outcome.ID] = lo
			}
			lo.mu.Lock()
			lo.name[locale] = outcome.Name
			if outcome.Description != nil {
				lo.description[locale] = *outcome.Description
			}
			lo.mu.Unlock()
		}
	}
	d.name[locale] = description.Name

	if description.Specifiers != nil {
		var specifiers []types.Specifier
		for _, s := range description.Specifiers.Specifier {
			specifiers = append(specifiers, types.Specifier{Name: s.Name, Type: s.Type})
		}
		if len(specifiers) > 0 {
			d.specifiers = specifiers
		}
	}
}

// Snapshot projects the cached entry into a types.MarketDescription
// value (data-copy under the entry's read lock).
func (d *LocalizedMarketDescription) Snapshot() types.MarketDescription {
	d.mu.RLock()
	defer d.mu.RUnlock()

	names := make(map[types.Locale]string, len(d.name))
	for k, v := range d.name {
		names[k] = v
	}

	outcomes := make([]types.OutcomeDescription, 0, len(d.outcomes))
	for id, oc := range d.outcomes {
		oc.mu.RLock()
		ocNames := make(map[types.Locale]string, len(oc.name))
		for k, v := range oc.name {
			ocNames[k] = v
		}
		ocDesc := make(map[types.Locale]string, len(oc.description))
		for k, v := range oc.description {
			ocDesc[k] = v
		}
		oc.mu.RUnlock()
		outcomes = append(outcomes, types.OutcomeDescription{
			ID:           id,
			Names:        ocNames,
			Descriptions: ocDesc,
		})
	}

	specifiers := make([]types.Specifier, len(d.specifiers))
	copy(specifiers, d.specifiers)

	groups := make([]string, len(d.groups))
	copy(groups, d.groups)

	return types.MarketDescription{
		ID:                     d.id,
		Names:                  names,
		Variant:                d.variant,
		IncludesOutcomesOfType: d.IncludesOutcomesOfType,
		OutcomeType:            d.OutcomeType,
		Outcomes:               outcomes,
		Specifiers:             specifiers,
		Groups:                 groups,
	}
}

// LocalizedOutcomeDescription holds per-locale outcome data.
type LocalizedOutcomeDescription struct {
	mu          sync.RWMutex
	name        map[types.Locale]string
	description map[types.Locale]string
}

// LocalizedName returns the cached outcome name for a locale.
func (l *LocalizedOutcomeDescription) LocalizedName(locale types.Locale) *string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	v, ok := l.name[locale]
	if !ok {
		return nil
	}
	return &v
}
