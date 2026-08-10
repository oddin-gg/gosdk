package cache

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/oddin-gg/gosdk/internal/api"
	apiXML "github.com/oddin-gg/gosdk/internal/api/xml"
	"github.com/oddin-gg/gosdk/internal/cache/lru"
	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
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
	GetReferenceIDs() *apiXML.ReferenceIDs
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
	// competitorsLoaded distinguishes "extended payload merged" from
	// "legitimately empty competitor list". Pre-v2.23, BuildTournament
	// used `len(competitorIDs) == 0` as a "not yet loaded" signal,
	// triggering a clear-and-refetch on every build for tournaments
	// that genuinely have zero competitors. The flag is set by merge()
	// only when the payload was a TournamentExtendedWrapper (which
	// carries the competitor list); other paths leave it false so
	// BuildTournament still forces a /competitors fetch on first use.
	competitorsLoaded bool

	name         map[types.Locale]string
	abbreviation map[types.Locale]string

	// Locale-independent — the API returns the same set across locales.
	referenceIDs map[string]string

	// stagedIcon holds the icon path gathered by the loader while the
	// load is still in flight. It is committed to the cache's icon map
	// only by the OnAdmit hook — i.e. only when the parent entry is
	// actually admitted — so a failed later-locale fetch, a coverage
	// failure, or a racing Clear can't leave an orphaned icon behind
	// (no parent entry means no eviction hook would ever clean it up).
	stagedIcon    *string
	stagedIconSet bool
}

// stageIcon records a load-gathered icon for commit-on-admission.
func (l *LocalizedTournament) stageIcon(icon *string) {
	l.mu.Lock()
	l.stagedIcon, l.stagedIconSet = icon, true
	l.mu.Unlock()
}

// takeStagedIcon returns and clears the staged icon, if any.
func (l *LocalizedTournament) takeStagedIcon() (*string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.stagedIconSet {
		return nil, false
	}
	icon := l.stagedIcon
	l.stagedIcon, l.stagedIconSet = nil, false
	return icon, true
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

// cloneForUpdate returns a copy the loader can merge into without
// mutating the live cached entry (copy-on-write): a failed later-locale
// fetch must not leave earlier locales of the SAME load visible to
// concurrent readers — the load/admit transaction admits the clone only
// after every locale and the coverage validation succeed. Top-level maps
// are copied one level deep; pointers are safe to alias because merge()
// only ever REPLACES them wholesale (competitorIDs and referenceIDs
// included — a payload swaps in a fresh map). Staged-icon fields start
// zeroed: staging belongs to the in-flight load, and the source's staged icon
// (if any) was already consumed at its own admission.
func (l *LocalizedTournament) cloneForUpdate() *LocalizedTournament {
	l.mu.RLock()
	defer l.mu.RUnlock()
	c := &LocalizedTournament{
		id:                l.id,
		startDate:         l.startDate,
		endDate:           l.endDate,
		sportID:           l.sportID,
		scheduledTime:     l.scheduledTime,
		scheduledEndTime:  l.scheduledEndTime,
		riskTier:          l.riskTier,
		category:          l.category,
		competitorIDs:     make(map[types.URN]struct{}, len(l.competitorIDs)),
		competitorsLoaded: l.competitorsLoaded,
		name:              make(map[types.Locale]string, len(l.name)+1),
		abbreviation:      make(map[types.Locale]string, len(l.abbreviation)+1),
		referenceIDs:      l.referenceIDs,
	}
	for k := range l.competitorIDs {
		c.competitorIDs[k] = struct{}{}
	}
	for k, v := range l.name {
		c.name[k] = v
	}
	for k, v := range l.abbreviation {
		c.abbreviation[k] = v
	}
	return c
}

func (l *LocalizedTournament) competitorIDList() []types.URN {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]types.URN, 0, len(l.competitorIDs))
	for k := range l.competitorIDs {
		out = append(out, k)
	}
	sortURNs(out) // deterministic public ordering (see sortURNs)
	return out
}

// merge folds a TournamentWrapper payload into the entry under mu.
func (l *LocalizedTournament) merge(locale types.Locale, t TournamentWrapper) error {
	sportID, err := types.ParseURN(t.GetSportID())
	if err != nil {
		return fmt.Errorf("tournament %s: parse sport id %q: %w", t.GetID(), t.GetSportID(), err)
	}

	var (
		competitorURNs  []types.URN
		extendedPayload bool
	)
	if ext, ok := t.(TournamentExtendedWrapper); ok {
		extendedPayload = true
		comps := ext.GetCompetitors()
		competitorURNs = make([]types.URN, 0, len(comps))
		for _, c := range comps {
			urn, err := types.ParseURN(c.GetID())
			if err != nil {
				return fmt.Errorf("tournament %s: parse competitor id %q: %w", t.GetID(), c.GetID(), err)
			}
			competitorURNs = append(competitorURNs, *urn)
		}
	}

	// The block is optional on the wire, so an absent one means "nothing
	// new", not "cleared" — refresh only when the payload sent it.
	var refIDs map[string]string
	if block := t.GetReferenceIDs(); block != nil {
		refIDs = make(map[string]string, len(block.ReferenceID))
		for _, ref := range block.ReferenceID {
			refIDs[ref.Name] = ref.Value
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
	if refIDs != nil {
		l.referenceIDs = refIDs
	}
	if extendedPayload {
		// Replace the competitor set (the API is authoritative) AND
		// flag the tournament as competitor-loaded so BuildTournament
		// doesn't trigger a clear-and-refetch on tournaments that
		// legitimately have no competitors.
		l.competitorIDs = make(map[types.URN]struct{}, len(competitorURNs))
		for _, urn := range competitorURNs {
			l.competitorIDs[urn] = struct{}{}
		}
		l.competitorsLoaded = true
	}
	return nil
}

// competitorsAreLoaded reports whether merge() has folded in an
// extended-tournament payload (the only payload shape that carries
// the competitor list). Used by BuildTournament to decide whether
// to force a /competitors fetch — pre-v2.23 this branch keyed on
// `len(competitorIDList()) == 0`, false-negativing every tournament
// that genuinely has zero competitors.
func (l *LocalizedTournament) competitorsAreLoaded() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.competitorsLoaded
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
		return nil, notFoundIfAbsent(err)
	}
	return v, nil
}

// SportIDFor returns the cached sportID for the given tournament,
// loading it via the supplied locales if necessary. Used by the
// public Client.Tournament(ctx, id, ...) helper which only has the
// tournament URN and needs the sportID to populate the embedded
// SportSummary on the returned Tournament value.
func (t *TournamentCache) SportIDFor(ctx context.Context, id types.URN, locales []types.Locale) (types.URN, error) {
	item, err := t.Tournament(ctx, id, locales)
	if err != nil {
		return types.URN{}, err
	}
	item.mu.RLock()
	defer item.mu.RUnlock()
	return item.sportID, nil
}

// TournamentCompetitors returns the competitor URN list for the tournament.
// If the entry was populated by a non-Tournament-info API path it may not
// have the competitor list yet; in that case we force a fresh fetch.
//
// Empty-result handling: a tournament that genuinely has zero competitors
// returns the empty slice from cache once competitorsAreLoaded()=true.
// Pre-fix `len(urns) > 0` was the gate, so a tournament with a real
// empty competitor list re-fetched on every call.
func (t *TournamentCache) TournamentCompetitors(ctx context.Context, id types.URN, locale types.Locale) ([]types.URN, error) {
	v, err := t.Tournament(ctx, id, []types.Locale{locale})
	if err != nil {
		return nil, err
	}
	if v.competitorsAreLoaded() {
		return v.competitorIDList(), nil
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

	fetchStarted := time.Now()
	data, err := t.apiClient.FetchTournament(ctx, id, locale)
	if err != nil {
		return nil, err
	}
	// Store under the parent entry's clear-tombstone lock: a
	// ClearCacheItem that landed after this fetch began must not have
	// its icon resurrected by us — StoreSide is atomic with Clear, so
	// there is no check-to-write window for a Clear to slip through.
	// The caller still gets the fresh value.
	t.lru.StoreSide(id, fetchStarted, func() {
		t.iconMu.Lock()
		t.icons[id] = data.IconPath
		t.iconMu.Unlock()
	})
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
	if err != nil {
		t.logger.WithError(err).Errorf("OnFeedMessage(tournament): parse event id %q", msg.EventID)
		return
	}
	if parsed == nil {
		t.logger.Errorf("OnFeedMessage(tournament): empty event id in FixtureChange routing key")
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

func newTournamentCache(lifeCtx context.Context, client *api.Client, logger *log.Logger) *TournamentCache {
	tc := &TournamentCache{
		apiClient: client,
		logger:    logger,
		icons:     make(map[types.URN]*string),
	}
	tc.lru = lru.NewEventCache[types.URN, types.Locale, *LocalizedTournament](
		lru.Config{
			Lifetime: lifeCtx,
			// Evict the icon alongside the entry — LRU/TTL eviction
			// previously left icon side-map entries behind forever.
			OnEvict: func(key any, _ any) {
				if id, ok := key.(types.URN); ok {
					tc.iconMu.Lock()
					delete(tc.icons, id)
					tc.iconMu.Unlock()
				}
			},
			// Commit the load-staged icon ONLY when the parent entry is
			// admitted (atomic with Clear, under the tombstone lock) — a
			// load that fails after staging must not leave an orphaned
			// icon no eviction hook can reach.
			OnAdmit: func(key any, value any) {
				id, okKey := key.(types.URN)
				entry, okVal := value.(*LocalizedTournament)
				if !okKey || !okVal {
					return
				}
				if icon, staged := entry.takeStagedIcon(); staged {
					tc.iconMu.Lock()
					tc.icons[id] = icon
					tc.iconMu.Unlock()
				}
			},
		},
		func(
			ctx context.Context,
			id types.URN,
			missing []types.Locale,
			existing *LocalizedTournament,
			hasExisting bool,
		) (*LocalizedTournament, error) {
			var entry *LocalizedTournament
			if hasExisting {
				// Copy-on-write: merge into a clone so a failed
				// later-locale fetch can't leave this load's earlier
				// locales visible on the live cached entry (admitted
				// only after coverage validation).
				entry = existing.cloneForUpdate()
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
					return nil, fmt.Errorf("fetch tournament %s/%s: %w", id.ToString(), locale, err)
				}
				// STAGE the icon; the OnAdmit hook commits it iff this
				// load's entry is admitted. Committing here — before the
				// remaining locales, coverage validation, and admission —
				// left an orphaned icon behind whenever a later step
				// failed (and a racing Clear couldn't suppress it).
				entry.stageIcon(data.IconPath)
				if err := entry.merge(locale, data); err != nil {
					return nil, fmt.Errorf("merge tournament %s locale %s: %w", id.ToString(), locale, err)
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
	sortURNs(competitorIDs) // deterministic public ordering (see sortURNs)
	// nil stays nil: "never sent" differs from "sent empty".
	var referenceIDs map[string]string
	if l.referenceIDs != nil {
		referenceIDs = make(map[string]string, len(l.referenceIDs))
		for k, v := range l.referenceIDs {
			referenceIDs[k] = v
		}
	}
	var category *types.Category
	if l.category != nil {
		category = &types.Category{
			ID:   l.category.ID,
			Name: l.category.Name,
			// types.FromPtr copies the pointee — pre-v2.32 the
			// snapshot aliased the cache's *string CountryCode.
			CountryCode: types.FromPtr(l.category.CountryCode),
			IconPath:    types.FromPtr(l.category.IconPath),
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
		IconPath:         types.FromPtr(icon),
		RiskTier:         l.riskTier,
		Category:         category,
		Sport:            sportSummary,
		CompetitorIDs:    competitorIDs,
		ReferenceIDs:     referenceIDs,
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
	factory entityFactory,
	id types.URN,
	sportID types.URN,
	locales []types.Locale,
) (*types.Tournament, error) {
	item, err := tc.Tournament(ctx, id, locales)
	if err != nil {
		return nil, err
	}
	if !item.competitorsAreLoaded() && len(locales) > 0 {
		// Force a fetch that surfaces competitor URNs — the
		// /tournaments/{id}/info path returns a non-extended payload
		// without the competitor list. Once an extended payload has
		// been merged we don't refetch (even if the list is empty —
		// some tournaments legitimately have zero competitors and
		// the pre-v2.23 `len == 0` gate refetched them on every build).
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
	// Resolve the sport identity. The cached tournament sport comes from
	// the API summary and is AUTHORITATIVE; the sportID argument is a feed
	// message's ROUTING-KEY value. Mirror the match path (BuildMatch): use
	// the route value only when the cache carries no sport yet, and on a
	// conflict keep the cached identity and log — a mis-routed or malicious
	// delivery must not relabel a known tournament (API data for
	// tournament A) under an unrelated route-selected sport B.
	resolvedSport := sportID
	if cached := item.sportID; cached != (types.URN{}) {
		if cached != sportID && tc.logger != nil {
			tc.logger.WithField("tournament_id", id.ToString()).
				WithField("cached_sport", cached.ToString()).
				WithField("route_sport", sportID.ToString()).
				Warn("cache: routing-key sport conflicts with cached tournament sport; keeping cached identity")
		}
		resolvedSport = cached
	}

	// Build the embedded SportSummary. Pre-fix this branch silently
	// swallowed the BuildSport error and returned a stub
	// SportSummary{ID: sportID} with empty Names — consumers saw a
	// tournament with no sport label and no diagnostic. Surface the
	// error so the caller can react (the tournament cache already
	// has id/locales context to wrap it).
	sport, err := factory.BuildSport(ctx, resolvedSport, locales)
	if err != nil {
		return nil, fmt.Errorf("build tournament %s: resolve sport %s: %w", id.ToString(), resolvedSport.ToString(), err)
	}
	sportSummary := sport.SportSummary
	tournament := item.tournamentSnapshot(ctx, icon, sportSummary)
	return &tournament, nil
}
