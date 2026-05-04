package cache

import (
	"context"
	"sync"
	"time"

	"github.com/oddin-gg/gosdk/internal/api"
	apiXML "github.com/oddin-gg/gosdk/internal/api/xml"
	"github.com/oddin-gg/gosdk/internal/cache/lru"
	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	"github.com/oddin-gg/gosdk/types"
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
	lru       *lru.EventCache[types.URN, types.Locale, *LocalizedTournament]

	iconMu sync.RWMutex
	icons  map[types.URN]*string
}

// LocalizedTournament holds tournament data; mu guards every field.
type LocalizedTournament struct {
	mu sync.RWMutex

	id types.URN

	startDate        *time.Time
	endDate          *time.Time
	sportID          types.URN
	scheduledTime    *time.Time
	scheduledEndTime *time.Time
	riskTier         int
	category         *apiXML.Category
	competitorIDs    map[types.URN]struct{}

	name         map[types.Locale]string
	abbreviation map[types.Locale]string
}

// Locales implements lru.LocalizedEntry.
func (l *LocalizedTournament) Locales() []types.Locale {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]types.Locale, 0, len(l.name))
	for locale := range l.name {
		out = append(out, locale)
	}
	return out
}

func (l *LocalizedTournament) competitorIDList() []types.URN {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]types.URN, 0, len(l.competitorIDs))
	for k := range l.competitorIDs {
		out = append(out, k)
	}
	return out
}

// merge folds a TournamentWrapper payload into the entry under mu.
func (l *LocalizedTournament) merge(locale types.Locale, t TournamentWrapper) error {
	sportID, err := types.ParseURN(t.GetSportID())
	if err != nil {
		return err
	}

	var competitorURNs []types.URN
	if ext, ok := t.(TournamentExtendedWrapper); ok {
		comps := ext.GetCompetitors()
		competitorURNs = make([]types.URN, 0, len(comps))
		for _, c := range comps {
			urn, err := types.ParseURN(c.GetID())
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
		l.competitorIDs = make(map[types.URN]struct{}, len(competitorURNs))
		for _, urn := range competitorURNs {
			l.competitorIDs[urn] = struct{}{}
		}
	}
	return nil
}

// ifZeroURN returns `prefer` if `current` is the zero URN, else `current`.
func ifZeroURN(current, prefer types.URN) types.URN {
	if current == (types.URN{}) {
		return prefer
	}
	return current
}

// urnFromString parses, ignoring errors (used as a defensive fallback).
func urnFromString(s string) types.URN {
	u, err := types.ParseURN(s)
	if err != nil || u == nil {
		return types.URN{}
	}
	return *u
}

// Tournament returns a populated LocalizedTournament.
func (t *TournamentCache) Tournament(ctx context.Context, id types.URN, locales []types.Locale) (*LocalizedTournament, error) {
	v, _, err := t.lru.Get(ctx, id, locales)
	if err != nil {
		return nil, err
	}
	return v, nil
}

// TournamentCompetitors returns the competitor URN list for the tournament.
// If the entry was populated by a non-Tournament-info API path it may not
// have the competitor list yet; in that case we force a fresh fetch.
func (t *TournamentCache) TournamentCompetitors(ctx context.Context, id types.URN, locale types.Locale) ([]types.URN, error) {
	v, err := t.Tournament(ctx, id, []types.Locale{locale})
	if err != nil {
		return nil, err
	}
	urns := v.competitorIDList()
	if len(urns) > 0 {
		return urns, nil
	}
	// Force re-fetch via the FetchTournament path which carries competitors.
	t.lru.Clear(id)
	v, err = t.Tournament(ctx, id, []types.Locale{locale})
	if err != nil {
		return nil, err
	}
	return v.competitorIDList(), nil
}

// TournamentIcon returns the cached icon path, fetching if needed.
func (t *TournamentCache) TournamentIcon(ctx context.Context, id types.URN, locale types.Locale) (*string, error) {
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
func (t *TournamentCache) OnFeedMessage(id types.URN, feedMessage *types.FeedMessage) {
	if feedMessage.Message == nil {
		return
	}
	msg, ok := feedMessage.Message.(*feedXML.FixtureChange)
	if !ok || id.Type != "tournament" {
		return
	}
	parsed, err := types.ParseURN(msg.EventID)
	if err != nil || parsed == nil {
		t.logger.WithError(err).Errorf("failed to convert urn %s", msg.EventID)
		return
	}
	t.ClearCacheItem(*parsed)
}

// ClearCacheItem is the public invalidation hook.
func (t *TournamentCache) ClearCacheItem(id types.URN) {
	t.lru.Clear(id)
	t.iconMu.Lock()
	delete(t.icons, id)
	t.iconMu.Unlock()
}

func newTournamentCache(client *api.Client, logger *log.Logger) *TournamentCache {
	tc := &TournamentCache{
		apiClient: client,
		logger:    logger,
		icons:     make(map[types.URN]*string),
	}
	tc.lru = lru.NewEventCache[types.URN, types.Locale, *LocalizedTournament](
		lru.Config{},
		func(
			ctx context.Context,
			id types.URN,
			missing []types.Locale,
			existing *LocalizedTournament,
			hasExisting bool,
		) (*LocalizedTournament, error) {
			var entry *LocalizedTournament
			if hasExisting {
				entry = existing
			} else {
				entry = &LocalizedTournament{
					id:            id,
					name:          make(map[types.Locale]string),
					abbreviation:  make(map[types.Locale]string),
					competitorIDs: make(map[types.URN]struct{}),
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

// tournamentSnapshot projects the cached entry into a
// types.Tournament value. Resolves the embedded sport summary
// through the entity factory; competitor URNs are kept as URNs (lazy
// resolution per call site).
func (l *LocalizedTournament) tournamentSnapshot(
	ctx context.Context,
	icon *string,
	sportSummary types.SportSummary,
) types.Tournament {
	l.mu.RLock()
	defer l.mu.RUnlock()
	names := make(map[types.Locale]string, len(l.name))
	for k, v := range l.name {
		names[k] = v
	}
	abbr := make(map[types.Locale]string, len(l.abbreviation))
	for k, v := range l.abbreviation {
		abbr[k] = v
	}
	competitorIDs := make([]types.URN, 0, len(l.competitorIDs))
	for k := range l.competitorIDs {
		competitorIDs = append(competitorIDs, k)
	}
	var category *types.Category
	if l.category != nil {
		category = &types.Category{
			ID:          l.category.ID,
			Name:        l.category.Name,
			CountryCode: l.category.CountryCode,
		}
	}
	return types.Tournament{
		ID:               l.id,
		Names:            names,
		Abbreviations:    abbr,
		StartDate:        cloneTime(l.startDate),
		EndDate:          cloneTime(l.endDate),
		ScheduledTime:    cloneTime(l.scheduledTime),
		ScheduledEndTime: cloneTime(l.scheduledEndTime),
		IconPath:         icon,
		RiskTier:         l.riskTier,
		Category:         category,
		Sport:            sportSummary,
		CompetitorIDs:    competitorIDs,
	}
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := *t
	return &v
}

// BuildTournament resolves a Tournament snapshot from the cache for the
// given locales. The embedded SportSummary is fetched through the entity
// factory; competitor URNs are kept lazy.
func BuildTournament(
	ctx context.Context,
	tc *TournamentCache,
	factory types.EntityFactory,
	id types.URN,
	sportID types.URN,
	locales []types.Locale,
) (*types.Tournament, error) {
	item, err := tc.Tournament(ctx, id, locales)
	if err != nil {
		return nil, err
	}
	if len(item.competitorIDList()) == 0 && len(locales) > 0 {
		// Force a fetch that surfaces competitor URNs (the bare
		// /info path may not include them).
		if _, err := tc.TournamentCompetitors(ctx, id, locales[0]); err != nil {
			return nil, err
		}
		item, err = tc.Tournament(ctx, id, locales)
		if err != nil {
			return nil, err
		}
	}
	var icon *string
	if len(locales) > 0 {
		icon, err = tc.TournamentIcon(ctx, id, locales[0])
		if err != nil {
			return nil, err
		}
	}
	var sportSummary types.SportSummary
	if sport, err := factory.BuildSport(ctx, sportID, locales); err == nil && sport != nil {
		sportSummary = sport.SportSummary
	} else {
		sportSummary = types.SportSummary{ID: sportID}
	}
	tournament := item.tournamentSnapshot(ctx, icon, sportSummary)
	return &tournament, nil
}
