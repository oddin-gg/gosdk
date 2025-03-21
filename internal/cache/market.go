package cache

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oddin-gg/gosdk/internal/api"
	data "github.com/oddin-gg/gosdk/internal/api/xml"
	"github.com/oddin-gg/gosdk/internal/utils"
	"github.com/oddin-gg/gosdk/protocols"
	"github.com/patrickmn/go-cache"
	"github.com/pkg/errors"
)

// CompositeKey ...
type CompositeKey struct {
	MarketID uint
	Variant  *string
}

// MarketDescriptionCache ...
type MarketDescriptionCache struct {
	apiClient     *api.Client
	mux           sync.Mutex
	loadedLocales map[protocols.Locale]struct{}
	internalCache *cache.Cache
}

// LocalizedMarketDescriptions ...
func (m *MarketDescriptionCache) LocalizedMarketDescriptions(locale protocols.Locale) (map[CompositeKey]*LocalizedMarketDescription, error) {
	m.mux.Lock()
	_, ok := m.loadedLocales[locale]
	m.mux.Unlock()

	if !ok {
		err := m.loadAndCacheAllItems([]protocols.Locale{locale})
		if err != nil {
			return nil, err
		}
	}

	items := m.internalCache.Items()
	result := make(map[CompositeKey]*LocalizedMarketDescription, len(items))
	for key, value := range items {
		mds := value.Object.(*LocalizedMarketDescription)
		_, ok := mds.name[locale]
		if !ok {
			continue
		}

		cpKey, err := m.makeCompositeKey(key)
		if err != nil {
			return nil, err
		}

		result[cpKey] = mds
	}

	return result, nil
}

// MarketDescriptionByID returns LocalizedMarketDescription from cache. Error is returned when entity is not found
func (m *MarketDescriptionCache) MarketDescriptionByID(
	marketID uint,
	variant *string,
	locales []protocols.Locale,
) (*LocalizedMarketDescription, error) {
	var missingLocales []protocols.Locale
	key := m.makeStringKey(marketID, variant)
	item, _ := m.internalCache.Get(key)
	result, ok := item.(*LocalizedMarketDescription)
	if ok {
		for i := range locales {
			locale := locales[i]
			result.mux.Lock()
			_, ok := result.name[locale]
			result.mux.Unlock()
			if !ok {
				missingLocales = append(missingLocales, locale)
			}
		}
	} else {
		missingLocales = locales
	}

	if len(missingLocales) != 0 {
		err := m.loadAndCacheItem(&marketID, variant, missingLocales)
		if err != nil {
			return nil, err
		}

		item, found := m.internalCache.Get(key)
		if !found {
			return nil, errors.Errorf("item missing key=%q", key)
		}

		result, ok = item.(*LocalizedMarketDescription)
		if !ok {
			return nil, errors.Errorf("unknown item type: %T with key=%q", item, key)
		}
	}

	return result, nil
}

// MarketDescriptionByKey ...
// Deprecated: do not use this function, there is no load when missing
func (m *MarketDescriptionCache) MarketDescriptionByKey(key CompositeKey) (*LocalizedMarketDescription, error) {
	strKey := m.makeStringKey(key.MarketID, key.Variant)
	item, ok := m.internalCache.Get(strKey)
	if !ok {
		return nil, errors.Errorf("no market description found for %s", strKey)
	}

	result, ok := item.(*LocalizedMarketDescription)
	if !ok {
		return nil, errors.Errorf("failed to convert market description")
	}

	return result, nil
}

// ClearCacheItem ...
func (m *MarketDescriptionCache) ClearCacheItem(marketID uint, variant *string) {
	key := m.makeStringKey(marketID, variant)
	m.internalCache.Delete(key)
}

func (m *MarketDescriptionCache) loadAndCacheAllItems(locales []protocols.Locale) error {
	return m.loadAndCacheItem(nil, nil, locales)
}

func (m *MarketDescriptionCache) loadAndCacheItem(
	marketID *uint,
	variant *string,
	locales []protocols.Locale,
) error {
	m.mux.Lock()
	defer m.mux.Unlock()

	for _, locale := range locales {

		var descriptions []data.MarketDescription
		var err error
		if marketID != nil && variant != nil && utils.IsMarketVariantWithDynamicOutcomes(*variant) {
			descriptions, err = m.apiClient.FetchMarketDescriptionsWithDynamicOutcomes(*marketID, *variant, locale)
			if err != nil {
				return err
			}
		} else {
			// fetch all descriptions
			descriptions, err = m.apiClient.FetchMarketDescriptions(locale)
			if err != nil {
				return err
			}
		}

		for k := range descriptions {
			description := descriptions[k]
			err := m.refreshOrInsertItem(description, locale)
			if err != nil {
				return err
			}
		}
		m.loadedLocales[locale] = struct{}{}
	}

	return nil
}

func (m *MarketDescriptionCache) refreshOrInsertItem(description data.MarketDescription, locale protocols.Locale) error {
	key := m.makeStringKey(description.ID, description.Variant)
	item, ok := m.internalCache.Get(key)
	var dsc *LocalizedMarketDescription
	if !ok {
		if description.Outcomes == nil {
			return errors.Errorf("missing outcomes in %v", description)
		}

		outcomes := make(map[string]*LocalizedOutcomeDescription)
		for _, outcome := range description.Outcomes.Outcome {
			outcomes[outcome.ID] = &LocalizedOutcomeDescription{
				refID:       outcome.RefID,
				name:        make(map[protocols.Locale]string),
				description: make(map[protocols.Locale]string),
			}
		}
		dsc = &LocalizedMarketDescription{
			refID:                  description.RefID,
			IncludesOutcomesOfType: description.IncludesOutcomesOfType,
			OutcomeType:            description.OutcomeType,
			outcomes:               outcomes,
			name:                   make(map[protocols.Locale]string),
		}
	} else {
		dsc, ok = item.(*LocalizedMarketDescription)
		if !ok {
			return errors.Errorf("failed to convert market description")
		}
	}

	dsc.mux.Lock()
	defer dsc.mux.Unlock()

	for _, outcome := range description.Outcomes.Outcome {
		localizedOutcome, ok := dsc.outcomes[outcome.ID]
		if !ok {
			return errors.Errorf("missing outcome in cache %s", outcome.ID)
		}

		localizedOutcome.name[locale] = outcome.Name
		localizedOutcome.refID = outcome.RefID

		if outcome.Description != nil {
			localizedOutcome.description[locale] = *outcome.Description
		}
	}

	dsc.name[locale] = description.Name

	var specifiers []protocols.Specifier
	if description.Specifiers != nil {
		for _, specifier := range description.Specifiers.Specifier {
			specifiers = append(specifiers, specifierImpl{
				name: specifier.Name,
				kind: specifier.Type,
			})
		}
	}
	if len(specifiers) != 0 {
		dsc.specifiers = specifiers
	}

	m.internalCache.Set(key, dsc, 0)
	return nil
}

func (m *MarketDescriptionCache) makeStringKey(marketID uint, variant *string) string {
	var keyPart string
	if variant != nil {
		keyPart = *variant
	} else {
		keyPart = "*"
	}

	return fmt.Sprintf("%d-%s", marketID, keyPart)
}

func (m *MarketDescriptionCache) makeCompositeKey(key string) (CompositeKey, error) {
	var ck CompositeKey

	if len(key) == 0 {
		return ck, errors.New("empty string")
	}

	parts := strings.Split(key, "-")
	if len(parts) != 2 {
		return ck, errors.Errorf("malformed key %s", key)
	}

	id, err := strconv.Atoi(parts[0])
	if err != nil {
		return ck, err
	}

	ck.MarketID = uint(id)
	if parts[1] != "*" {
		ck.Variant = &parts[1]
	}

	return ck, nil
}

func newMarketDescriptionCache(client *api.Client) *MarketDescriptionCache {
	return &MarketDescriptionCache{
		loadedLocales: make(map[protocols.Locale]struct{}),
		internalCache: cache.New(24*time.Hour, 1*time.Hour),
		apiClient:     client,
	}
}

// LocalizedMarketDescription ...
type LocalizedMarketDescription struct {
	refID                  *uint
	IncludesOutcomesOfType *string
	OutcomeType            *string
	outcomes               map[string]*LocalizedOutcomeDescription
	specifiers             []protocols.Specifier
	name                   map[protocols.Locale]string
	// locks outcomes and name properties
	mux sync.Mutex
}

// LocalizedOutcomeDescription ...
type LocalizedOutcomeDescription struct {
	refID       *uint
	name        map[protocols.Locale]string
	description map[protocols.Locale]string
	mux         sync.Mutex
}

type specifierImpl struct {
	name string
	kind string
}

func (s specifierImpl) Name() string {
	return s.name
}

func (s specifierImpl) Type() string {
	return s.kind
}

type outcomeDescriptionImpl struct {
	id                          string
	localizedOutcomeDescription *LocalizedOutcomeDescription
}

func (o outcomeDescriptionImpl) ID() string {
	return o.id
}

func (o outcomeDescriptionImpl) RefID() *uint {
	return o.localizedOutcomeDescription.refID
}

func (o outcomeDescriptionImpl) LocalizedName(locale protocols.Locale) *string {
	o.localizedOutcomeDescription.mux.Lock()
	defer o.localizedOutcomeDescription.mux.Unlock()

	name, ok := o.localizedOutcomeDescription.name[locale]
	if !ok {
		return nil
	}

	return &name
}

func (o outcomeDescriptionImpl) Description(locale protocols.Locale) *string {
	o.localizedOutcomeDescription.mux.Lock()
	defer o.localizedOutcomeDescription.mux.Unlock()

	description, ok := o.localizedOutcomeDescription.description[locale]
	if !ok {
		return nil
	}

	return &description
}

type marketDescriptionImpl struct {
	id                     uint
	includesOutcomesOfType *string
	outcomeType            *string
	variant                *string
	marketDescriptionCache *MarketDescriptionCache
	locales                []protocols.Locale
}

func (m marketDescriptionImpl) ID() (uint, error) {
	return m.id, nil
}

func (m marketDescriptionImpl) RefID() (*uint, error) {
	item, err := m.marketDescriptionCache.MarketDescriptionByID(m.id, m.variant, m.locales)
	if err != nil {
		return nil, err
	}

	return item.refID, nil
}

func (m marketDescriptionImpl) LocalizedName(locale protocols.Locale) (*string, error) {
	item, err := m.marketDescriptionCache.MarketDescriptionByID(m.id, m.variant, m.locales)
	if err != nil {
		return nil, err
	}

	item.mux.Lock()
	defer item.mux.Unlock()

	name, ok := item.name[locale]
	if !ok {
		return nil, errors.Errorf("missing locale %s", locale)
	}

	return &name, nil
}

// IncludesOutcomesOfType return optional value of includesOutcomesOfType property. For more info about
// includesOutcomesOfType property check xsd schema.
func (m marketDescriptionImpl) IncludesOutcomesOfType() *string {
	return m.includesOutcomesOfType
}

// OutcomeType return optional value of outcomeType property. For more info about outcomeType property
// check xsd schema.
func (m marketDescriptionImpl) OutcomeType() *string {
	return m.outcomeType
}

func (m marketDescriptionImpl) Outcomes() ([]protocols.OutcomeDescription, error) {
	item, err := m.marketDescriptionCache.MarketDescriptionByID(m.id, m.variant, m.locales)
	if err != nil {
		return nil, err
	}

	item.mux.Lock()
	defer item.mux.Unlock()

	outcomes := make([]protocols.OutcomeDescription, 0, len(item.outcomes))
	for key := range item.outcomes {
		it := item.outcomes[key]
		outcome := outcomeDescriptionImpl{
			id:                          key,
			localizedOutcomeDescription: it,
		}
		outcomes = append(outcomes, outcome)
	}

	return outcomes, nil
}

func (m marketDescriptionImpl) Variant() (*string, error) {
	return m.variant, nil
}

func (m marketDescriptionImpl) Specifiers() ([]protocols.Specifier, error) {
	item, err := m.marketDescriptionCache.MarketDescriptionByID(m.id, m.variant, m.locales)
	if err != nil {
		return nil, err
	}

	return item.specifiers, nil
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
