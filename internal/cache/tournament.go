package cache

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/oddin-gg/gosdk/internal/api"
	apiXML "github.com/oddin-gg/gosdk/internal/api/xml"
	"github.com/oddin-gg/gosdk/internal/cache/lru"
	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	"github.com/oddin-gg/gosdk/protocols"
	log "github.com/oddin-gg/gosdk/internal/log"
)

// TournamentWrapper is the small interface implemented by the various API XML
// types that carry tournament metadata.
type TournamentWrapper interface {
	GetID() string
	GetStartDate() *time.Time
	GetEndDate() *time.Time
	GetSportID() string
	GetScheduledTime() *time.Time
	GetScheduledEndTime() *time.Time
	GetName() string
	GetAbbreviation() string
	GetRiskTier() int
	GetCategory() *apiXML.Category
}

// TournamentExtendedWrapper extends TournamentWrapper with the optional
// competitor list.
type TournamentExtendedWrapper interface {
	TournamentWrapper
	GetCompetitors() []apiXML.Team
}

// TournamentCache stores tournament data per (URN, locale).
type TournamentCache struct {
	apiClient *api.Client
	logger    *log.Logger
	lru       *lru.EventCache[protocols.URN, protocols.Locale, *LocalizedTournament]

	iconMu sync.RWMutex
	icons  map[protocols.URN]*string
}

// LocalizedTournament holds tournament data; mu guards every field.
type LocalizedTournament struct {
	mu sync.RWMutex

	id protocols.URN

	startDate        *time.Time
	endDate          *time.Time
	sportID          protocols.URN
	scheduledTime    *time.Time
	scheduledEndTime *time.Time
	riskTier         int
	category         *apiXML.Category
	competitorIDs    map[protocols.URN]struct{}

	name         map[protocols.Locale]string
	abbreviation map[protocols.Locale]string
}

// Locales implements lru.LocalizedEntry.
func (l *LocalizedTournament) Locales() []protocols.Locale {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]protocols.Locale, 0, len(l.name))
	for locale := range l.name {
		out = append(out, locale)
	}
	return out
}

func (l *LocalizedTournament) localizedName(locale protocols.Locale) (*string, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	v, ok := l.name[locale]
	if !ok {
		return nil, fmt.Errorf("missing locale %s", locale)
	}
	return &v, nil
}

func (l *LocalizedTournament) localizedAbbreviation(locale protocols.Locale) (*string, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	v, ok := l.abbreviation[locale]
	if !ok {
		return nil, fmt.Errorf("missing locale %s", locale)
	}
	return &v, nil
}

func (l *LocalizedTournament) snapshot() (start, end, sched, schedEnd *time.Time, riskTier int, cat *apiXML.Category) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.startDate, l.endDate, l.scheduledTime, l.scheduledEndTime, l.riskTier, l.category
}

func (l *LocalizedTournament) competitorIDList() []protocols.URN {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]protocols.URN, 0, len(l.competitorIDs))
	for k := range l.competitorIDs {
		out = append(out, k)
	}
	return out
}

// merge folds a TournamentWrapper payload into the entry under mu.
func (l *LocalizedTournament) merge(locale protocols.Locale, t TournamentWrapper) error {
	sportID, err := protocols.ParseURN(t.GetSportID())
	if err != nil {
		return err
	}

	var competitorURNs []protocols.URN
	if ext, ok := t.(TournamentExtendedWrapper); ok {
		comps := ext.GetCompetitors()
		competitorURNs = make([]protocols.URN, 0, len(comps))
		for _, c := range comps {
			urn, err := protocols.ParseURN(c.GetID())
			if err != nil {
				return err
			}
			competitorURNs = append(competitorURNs, *urn)
		}
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.id = ifZeroURN(l.id, urnFromString(t.GetID()))
	l.startDate = t.GetStartDate()
	l.endDate = t.GetEndDate()
	l.sportID = *sportID
	l.scheduledTime = t.GetScheduledTime()
	l.scheduledEndTime = t.GetScheduledEndTime()
	l.riskTier = t.GetRiskTier()
	l.category = t.GetCategory()
	l.name[locale] = t.GetName()
	l.abbreviation[locale] = t.GetAbbreviation()
	if competitorURNs != nil {
		l.competitorIDs = make(map[protocols.URN]struct{}, len(competitorURNs))
		for _, urn := range competitorURNs {
			l.competitorIDs[urn] = struct{}{}
		}
	}
	return nil
}

// ifZeroURN returns `prefer` if `current` is the zero URN, else `current`.
func ifZeroURN(current, prefer protocols.URN) protocols.URN {
	if current == (protocols.URN{}) {
		return prefer
	}
	return current
}

// urnFromString parses, ignoring errors (used as a defensive fallback).
func urnFromString(s string) protocols.URN {
	u, err := protocols.ParseURN(s)
	if err != nil || u == nil {
		return protocols.URN{}
	}
	return *u
}

// Tournament returns a populated LocalizedTournament.
func (t *TournamentCache) Tournament(ctx context.Context, id protocols.URN, locales []protocols.Locale) (*LocalizedTournament, error) {
	v, _, err := t.lru.Get(ctx, id, locales)
	if err != nil {
		return nil, err
	}
	return v, nil
}

// TournamentCompetitors returns the competitor URN list for the tournament.
// If the entry was populated by a non-Tournament-info API path it may not
// have the competitor list yet; in that case we force a fresh fetch.
func (t *TournamentCache) TournamentCompetitors(ctx context.Context, id protocols.URN, locale protocols.Locale) ([]protocols.URN, error) {
	v, err := t.Tournament(ctx, id, []protocols.Locale{locale})
	if err != nil {
		return nil, err
	}
	urns := v.competitorIDList()
	if len(urns) > 0 {
		return urns, nil
	}
	// Force re-fetch via the FetchTournament path which carries competitors.
	t.lru.Clear(id)
	v, err = t.Tournament(ctx, id, []protocols.Locale{locale})
	if err != nil {
		return nil, err
	}
	return v.competitorIDList(), nil
}

// TournamentIcon returns the cached icon path, fetching if needed.
func (t *TournamentCache) TournamentIcon(ctx context.Context, id protocols.URN, locale protocols.Locale) (*string, error) {
	t.iconMu.RLock()
	if v, ok := t.icons[id]; ok {
		t.iconMu.RUnlock()
		return v, nil
	}
	t.iconMu.RUnlock()

	data, err := t.apiClient.FetchTournament(ctx, id, locale)
	if err != nil {
		return nil, err
	}
	t.iconMu.Lock()
	t.icons[id] = data.IconPath
	t.iconMu.Unlock()
	return data.IconPath, nil
}

// OnFeedMessage clears the cache for tournament-typed FixtureChange messages.
func (t *TournamentCache) OnFeedMessage(id protocols.URN, feedMessage *protocols.FeedMessage) {
	if feedMessage.Message == nil {
		return
	}
	msg, ok := feedMessage.Message.(*feedXML.FixtureChange)
	if !ok || id.Type != "tournament" {
		return
	}
	parsed, err := protocols.ParseURN(msg.EventID)
	if err != nil || parsed == nil {
		t.logger.WithError(err).Errorf("failed to convert urn %s", msg.EventID)
		return
	}
	t.ClearCacheItem(*parsed)
}

// ClearCacheItem is the public invalidation hook.
func (t *TournamentCache) ClearCacheItem(id protocols.URN) {
	t.lru.Clear(id)
	t.iconMu.Lock()
	delete(t.icons, id)
	t.iconMu.Unlock()
}

func newTournamentCache(client *api.Client, logger *log.Logger) *TournamentCache {
	tc := &TournamentCache{
		apiClient: client,
		logger:    logger,
		icons:     make(map[protocols.URN]*string),
	}
	tc.lru = lru.NewEventCache[protocols.URN, protocols.Locale, *LocalizedTournament](
		lru.Config{},
		func(
			ctx context.Context,
			id protocols.URN,
			missing []protocols.Locale,
			existing *LocalizedTournament,
			hasExisting bool,
		) (*LocalizedTournament, error) {
			var entry *LocalizedTournament
			if hasExisting {
				entry = existing
			} else {
				entry = &LocalizedTournament{
					id:            id,
					name:          make(map[protocols.Locale]string),
					abbreviation:  make(map[protocols.Locale]string),
					competitorIDs: make(map[protocols.URN]struct{}),
				}
			}
			for _, locale := range missing {
				data, err := client.FetchTournament(ctx, id, locale)
				if err != nil {
					return nil, err
				}
				tc.iconMu.Lock()
				tc.icons[id] = data.IconPath
				tc.iconMu.Unlock()
				if err := entry.merge(locale, data); err != nil {
					return nil, err
				}
			}
			return entry, nil
		},
	)
	return tc
}

// tournamentImpl implements protocols.Tournament.
type tournamentImpl struct {
	id              protocols.URN
	sportID         protocols.URN
	tournamentCache *TournamentCache
	entityFactory   protocols.EntityFactory
	locales         []protocols.Locale
}

func (t tournamentImpl) IconPath() (*string, error) {
	if len(t.locales) == 0 {
		return nil, errors.New("missing locales")
	}
	return t.tournamentCache.TournamentIcon(context.Background(), t.id, t.locales[0])
}

func (t tournamentImpl) ID() protocols.URN { return t.id }

func (t tournamentImpl) LocalizedAbbreviation(locale protocols.Locale) (*string, error) {
	item, err := t.tournamentCache.Tournament(context.Background(), t.id, []protocols.Locale{locale})
	if err != nil {
		return nil, err
	}
	return item.localizedAbbreviation(locale)
}

func (t tournamentImpl) LocalizedName(locale protocols.Locale) (*string, error) {
	item, err := t.tournamentCache.Tournament(context.Background(), t.id, []protocols.Locale{locale})
	if err != nil {
		return nil, err
	}
	return item.localizedName(locale)
}

func (t tournamentImpl) SportID() (*protocols.URN, error) { return &t.sportID, nil }

func (t tournamentImpl) ScheduledTime() (*time.Time, error) {
	item, err := t.tournamentCache.Tournament(context.Background(), t.id, t.locales)
	if err != nil {
		return nil, err
	}
	_, _, sched, _, _, _ := item.snapshot()
	return sched, nil
}

func (t tournamentImpl) ScheduledEndTime() (*time.Time, error) {
	item, err := t.tournamentCache.Tournament(context.Background(), t.id, t.locales)
	if err != nil {
		return nil, err
	}
	_, _, _, schedEnd, _, _ := item.snapshot()
	return schedEnd, nil
}

func (t tournamentImpl) LiveOddsAvailability() (*protocols.LiveOddsAvailability, error) {
	available := protocols.NotAvailableLiveOddsAvailability
	return &available, nil
}

func (t tournamentImpl) Sport() protocols.SportSummary {
	return t.entityFactory.BuildSport(t.sportID, t.locales)
}

func (t tournamentImpl) Competitors() ([]protocols.Competitor, error) {
	item, err := t.tournamentCache.Tournament(context.Background(), t.id, t.locales)
	if err != nil {
		return nil, err
	}
	urns := item.competitorIDList()
	if len(urns) == 0 {
		urns, err = t.tournamentCache.TournamentCompetitors(context.Background(), t.id, t.locales[0])
		if err != nil {
			return nil, err
		}
	}
	return t.entityFactory.BuildCompetitors(urns, t.locales), nil
}

func (t tournamentImpl) StartDate() (*time.Time, error) {
	item, err := t.tournamentCache.Tournament(context.Background(), t.id, t.locales)
	if err != nil {
		return nil, err
	}
	start, _, _, _, _, _ := item.snapshot()
	return start, nil
}

func (t tournamentImpl) EndDate() (*time.Time, error) {
	item, err := t.tournamentCache.Tournament(context.Background(), t.id, t.locales)
	if err != nil {
		return nil, err
	}
	_, end, _, _, _, _ := item.snapshot()
	return end, nil
}

func (t tournamentImpl) RiskTier() (int, error) {
	item, err := t.tournamentCache.Tournament(context.Background(), t.id, t.locales)
	if err != nil {
		return 0, err
	}
	_, _, _, _, riskTier, _ := item.snapshot()
	return riskTier, nil
}

type categoryImpl struct {
	id          string
	name        string
	countryCode *string
}

func (c *categoryImpl) ID() string           { return c.id }
func (c *categoryImpl) Name() string         { return c.name }
func (c *categoryImpl) CountryCode() *string { return c.countryCode }

func (t tournamentImpl) Category() (protocols.Category, error) {
	item, err := t.tournamentCache.Tournament(context.Background(), t.id, t.locales)
	if err != nil {
		return nil, err
	}
	_, _, _, _, _, cat := item.snapshot()
	if cat == nil {
		return nil, errors.New("category not found")
	}
	return &categoryImpl{
		id:          cat.ID,
		name:        cat.Name,
		countryCode: cat.CountryCode,
	}, nil
}

// NewTournament ...
func NewTournament(id protocols.URN, sportID protocols.URN, tournamentCache *TournamentCache, entityFactory protocols.EntityFactory, locales []protocols.Locale) protocols.Tournament {
	return &tournamentImpl{
		id:              id,
		sportID:         sportID,
		tournamentCache: tournamentCache,
		entityFactory:   entityFactory,
		locales:         locales,
	}
}
