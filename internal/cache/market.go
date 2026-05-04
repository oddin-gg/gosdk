package cache

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/oddin-gg/gosdk/internal/api"
	data "github.com/oddin-gg/gosdk/internal/api/xml"
	"github.com/oddin-gg/gosdk/internal/utils"
	"github.com/oddin-gg/gosdk/protocols"
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
	loadedLocales map[protocols.Locale]struct{}
	descriptions  map[CompositeKey]*LocalizedMarketDescription

	loadMu sync.Mutex // serializes concurrent API loads
}

// LocalizedMarketDescriptions returns every cached description that contains
// data for the given locale, fetching the locale's full catalog if not yet
// loaded.
func (m *MarketDescriptionCache) LocalizedMarketDescriptions(ctx context.Context, locale protocols.Locale) (map[CompositeKey]*LocalizedMarketDescription, error) {
	if !m.localeLoaded(locale) {
		if err := m.loadAll(ctx, []protocols.Locale{locale}); err != nil {
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
	locales []protocols.Locale,
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
	m.loadedLocales = make(map[protocols.Locale]struct{})
	m.mu.Unlock()
}

func (m *MarketDescriptionCache) localeLoaded(locale protocols.Locale) bool {
	m.mu.RLock()
	_, ok := m.loadedLocales[locale]
	m.mu.RUnlock()
	return ok
}

func (m *MarketDescriptionCache) loadAll(ctx context.Context, locales []protocols.Locale) error {
	return m.loadOne(ctx, nil, nil, locales)
}

func (m *MarketDescriptionCache) loadOne(ctx context.Context, marketID *uint, variant *string, locales []protocols.Locale) error {
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

func (m *MarketDescriptionCache) upsert(description data.MarketDescription, locale protocols.Locale) error {
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
				name:        make(map[protocols.Locale]string),
				description: make(map[protocols.Locale]string),
			}
		}
		entry = &LocalizedMarketDescription{
			refID:                  description.RefID,
			IncludesOutcomesOfType: description.IncludesOutcomesOfType,
			OutcomeType:            description.OutcomeType,
			outcomes:               outcomes,
			name:                   make(map[protocols.Locale]string),
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
		loadedLocales: make(map[protocols.Locale]struct{}),
		descriptions:  make(map[CompositeKey]*LocalizedMarketDescription),
	}
}

// LocalizedMarketDescription stores per-(market, variant) description data
// across multiple locales. mu guards all fields.
type LocalizedMarketDescription struct {
	mu sync.RWMutex

	refID                  *uint
	IncludesOutcomesOfType *string
	OutcomeType            *string
	outcomes               map[string]*LocalizedOutcomeDescription
	specifiers             []protocols.Specifier
	name                   map[protocols.Locale]string
	groups                 []string
}

func (d *LocalizedMarketDescription) hasLocale(locale protocols.Locale) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	_, ok := d.name[locale]
	return ok
}

func (d *LocalizedMarketDescription) missingLocales(locales []protocols.Locale) []protocols.Locale {
	d.mu.RLock()
	defer d.mu.RUnlock()
	var missing []protocols.Locale
	for _, l := range locales {
		if _, ok := d.name[l]; !ok {
			missing = append(missing, l)
		}
	}
	return missing
}

func (d *LocalizedMarketDescription) merge(description data.MarketDescription, locale protocols.Locale) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if description.Outcomes != nil {
		for _, outcome := range description.Outcomes.Outcome {
			lo, ok := d.outcomes[outcome.ID]
			if !ok {
				// New outcome on a fresh fetch — add it.
				lo = &LocalizedOutcomeDescription{
					name:        make(map[protocols.Locale]string),
					description: make(map[protocols.Locale]string),
				}
				d.outcomes[outcome.ID] = lo
			}
			lo.mu.Lock()
			lo.name[locale] = outcome.Name
			lo.refID = outcome.RefID
			if outcome.Description != nil {
				lo.description[locale] = *outcome.Description
			}
			lo.mu.Unlock()
		}
	}
	d.name[locale] = description.Name

	if description.Specifiers != nil {
		var specifiers []protocols.Specifier
		for _, s := range description.Specifiers.Specifier {
			specifiers = append(specifiers, specifierImpl{name: s.Name, kind: s.Type})
		}
		if len(specifiers) > 0 {
			d.specifiers = specifiers
		}
	}
}

func (d *LocalizedMarketDescription) localizedName(locale protocols.Locale) (*string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	v, ok := d.name[locale]
	if !ok {
		return nil, fmt.Errorf("missing locale %s", locale)
	}
	return &v, nil
}

func (d *LocalizedMarketDescription) outcomeList() []protocols.OutcomeDescription {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]protocols.OutcomeDescription, 0, len(d.outcomes))
	for id, oc := range d.outcomes {
		out = append(out, outcomeDescriptionImpl{id: id, localizedOutcomeDescription: oc})
	}
	return out
}

func (d *LocalizedMarketDescription) specifierList() []protocols.Specifier {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]protocols.Specifier, len(d.specifiers))
	copy(out, d.specifiers)
	return out
}

func (d *LocalizedMarketDescription) groupList() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]string, len(d.groups))
	copy(out, d.groups)
	return out
}

func (d *LocalizedMarketDescription) refIDValue() *uint {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.refID
}

// LocalizedOutcomeDescription holds per-locale outcome data.
type LocalizedOutcomeDescription struct {
	mu          sync.RWMutex
	refID       *uint
	name        map[protocols.Locale]string
	description map[protocols.Locale]string
}

type specifierImpl struct {
	name string
	kind string
}

func (s specifierImpl) Name() string { return s.name }
func (s specifierImpl) Type() string { return s.kind }

type outcomeDescriptionImpl struct {
	id                          string
	localizedOutcomeDescription *LocalizedOutcomeDescription
}

func (o outcomeDescriptionImpl) ID() string { return o.id }

// Deprecated: do not use this property, it will be removed in future
func (o outcomeDescriptionImpl) RefID() *uint {
	o.localizedOutcomeDescription.mu.RLock()
	defer o.localizedOutcomeDescription.mu.RUnlock()
	return o.localizedOutcomeDescription.refID
}

func (o outcomeDescriptionImpl) LocalizedName(locale protocols.Locale) *string {
	o.localizedOutcomeDescription.mu.RLock()
	defer o.localizedOutcomeDescription.mu.RUnlock()
	v, ok := o.localizedOutcomeDescription.name[locale]
	if !ok {
		return nil
	}
	return &v
}

func (o outcomeDescriptionImpl) Description(locale protocols.Locale) *string {
	o.localizedOutcomeDescription.mu.RLock()
	defer o.localizedOutcomeDescription.mu.RUnlock()
	v, ok := o.localizedOutcomeDescription.description[locale]
	if !ok {
		return nil
	}
	return &v
}

type marketDescriptionImpl struct {
	id                     uint
	includesOutcomesOfType *string
	outcomeType            *string
	variant                *string
	marketDescriptionCache *MarketDescriptionCache
	locales                []protocols.Locale
}

func (m marketDescriptionImpl) ID() (uint, error) { return m.id, nil }

// Deprecated: do not use this method, it will be removed in future
func (m marketDescriptionImpl) RefID() (*uint, error) {
	item, err := m.marketDescriptionCache.MarketDescriptionByID(context.Background(), m.id, m.variant, m.locales)
	if err != nil {
		return nil, err
	}
	return item.refIDValue(), nil
}

func (m marketDescriptionImpl) LocalizedName(locale protocols.Locale) (*string, error) {
	item, err := m.marketDescriptionCache.MarketDescriptionByID(context.Background(), m.id, m.variant, m.locales)
	if err != nil {
		return nil, err
	}
	return item.localizedName(locale)
}

func (m marketDescriptionImpl) IncludesOutcomesOfType() *string { return m.includesOutcomesOfType }
func (m marketDescriptionImpl) OutcomeType() *string            { return m.outcomeType }

func (m marketDescriptionImpl) Outcomes() ([]protocols.OutcomeDescription, error) {
	item, err := m.marketDescriptionCache.MarketDescriptionByID(context.Background(), m.id, m.variant, m.locales)
	if err != nil {
		return nil, err
	}
	return item.outcomeList(), nil
}

func (m marketDescriptionImpl) Variant() (*string, error) { return m.variant, nil }

func (m marketDescriptionImpl) Specifiers() ([]protocols.Specifier, error) {
	item, err := m.marketDescriptionCache.MarketDescriptionByID(context.Background(), m.id, m.variant, m.locales)
	if err != nil {
		return nil, err
	}
	return item.specifierList(), nil
}

func (m marketDescriptionImpl) Groups() ([]string, error) {
	item, err := m.marketDescriptionCache.MarketDescriptionByID(context.Background(), m.id, m.variant, m.locales)
	if err != nil {
		return nil, err
	}
	return item.groupList(), nil
}

// NewMarketDescription ...
func NewMarketDescription(
	id uint,
	includesOutcomesOfType *string,
	outcomeType *string,
	variant *string,
	marketDescriptionCache *MarketDescriptionCache,
	locales []protocols.Locale,
) protocols.MarketDescription {
	return &marketDescriptionImpl{
		id:                     id,
		includesOutcomesOfType: includesOutcomesOfType,
		outcomeType:            outcomeType,
		variant:                variant,
		marketDescriptionCache: marketDescriptionCache,
		locales:                locales,
	}
}
