package cache

import (
	"context"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/api/xml"
	"github.com/oddin-gg/gosdk/internal/cache/lru"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

// TeamWrapper is the small interface implemented by every API XML type that
// carries enough fields to populate a competitor entry.
type TeamWrapper interface {
	GetID() string
	GetName() string
	GetAbbreviation() string
	GetUnderage() string
}

// TeamWithPlayers is the optional extension when the API XML also lists players.
type TeamWithPlayers interface {
	TeamWrapper
	GetPlayers() []xml.Player
}

// CompetitorCache stores competitor profiles per (URN, locale).
//
// Phase 3 rewrite: lru.EventCache + per-entry mutex; the icon path lives in
// its own concurrent map (icons are URN-keyed and locale-independent in
// practice). The previous OnAPIResponse cross-population is removed — lazy
// loading + singleflight gives the equivalent.
//
// v2.x reshape: icons migrated from `map[URN]*string` guarded by a global
// RWMutex to `sync.Map`. Pre-fix every concurrent loader for a different
// competitor URN briefly serialized on the same `iconMu` write inside its
// singleflight body, defeating the per-key load isolation. sync.Map's
// internal sharding lets unrelated URNs proceed in parallel without
// contending on a single write lock.
type CompetitorCache struct {
	apiClient *api.Client
	logger    *log.Logger
	lru       *lru.EventCache[types.URN, types.Locale, *LocalizedCompetitor]

	// icons stores types.URN → *string (nil means "fetched, no icon").
	// Use loadIcon / storeIcon / deleteIcon helpers for type-safe access.
	icons sync.Map
}

// loadIcon returns (icon, true) if the URN has a recorded icon (which may
// itself be nil for a competitor with no icon path), or (nil, false)
// when no profile fetch has populated the URN yet.
func (c *CompetitorCache) loadIcon(id types.URN) (*string, bool) {
	v, ok := c.icons.Load(id)
	if !ok {
		return nil, false
	}
	if v == nil {
		return nil, true
	}
	return v.(*string), true
}

func (c *CompetitorCache) storeIcon(id types.URN, icon *string) {
	c.icons.Store(id, icon)
}

func (c *CompetitorCache) deleteIcon(id types.URN) {
	c.icons.Delete(id)
}

// LocalizedCompetitor carries per-locale name/abbreviation plus
// locale-independent metadata (players, underage).
//
// playersLoaded distinguishes "this competitor genuinely has no
// players" (true, players==nil/empty) from "we haven't yet fetched
// the with-players profile" (false). Pre-v2.24, a competitor with
// zero players was indistinguishable from an unfetched one and got
// re-fetched on every BuildCompetitor call.
type LocalizedCompetitor struct {
	mu sync.RWMutex

	id types.URN

	name         map[types.Locale]string
	abbreviation map[types.Locale]string

	underage      *types.UnderageStatus
	players       []types.URN
	playersLoaded bool

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
func (l *LocalizedCompetitor) stageIcon(icon *string) {
	l.mu.Lock()
	l.stagedIcon, l.stagedIconSet = icon, true
	l.mu.Unlock()
}

// takeStagedIcon returns and clears the staged icon, if any.
func (l *LocalizedCompetitor) takeStagedIcon() (*string, bool) {
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
func (l *LocalizedCompetitor) Locales() []types.Locale {
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
// are copied one level deep; slices and pointers are safe to alias
// because merge() only ever REPLACES them wholesale. Staged-icon fields
// start zeroed: staging belongs to the in-flight load, and the source's
// staged icon (if any) was already consumed at its own admission.
func (l *LocalizedCompetitor) cloneForUpdate() *LocalizedCompetitor {
	l.mu.RLock()
	defer l.mu.RUnlock()
	c := &LocalizedCompetitor{
		id:            l.id,
		name:          make(map[types.Locale]string, len(l.name)+1),
		abbreviation:  make(map[types.Locale]string, len(l.abbreviation)+1),
		underage:      l.underage,
		players:       l.players,
		playersLoaded: l.playersLoaded,
	}
	for k, v := range l.name {
		c.name[k] = v
	}
	for k, v := range l.abbreviation {
		c.abbreviation[k] = v
	}
	return c
}

// LocalizedName returns the localized name or an error if the locale is not loaded.
// Wraps ErrLocaleNotLoaded so consumers can errors.Is the cause.
func (l *LocalizedCompetitor) LocalizedName(locale types.Locale) (*string, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	v, ok := l.name[locale]
	if !ok {
		return nil, fmt.Errorf("competitor %s locale %s: %w", l.id.ToString(), locale, ErrLocaleNotLoaded)
	}
	return &v, nil
}

func (l *LocalizedCompetitor) playersAreLoaded() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.playersLoaded
}

// merge folds a TeamWrapper payload into the entry under mu.
//
// The TeamWithPlayers branch always marks playersLoaded=true on
// successful parse — even when the players slice is empty — so a
// genuinely playerless competitor doesn't trigger re-fetch on every
// subsequent BuildCompetitor call. For non-TeamWithPlayers payloads
// (locale-only loads), playersLoaded stays unchanged.
func (l *LocalizedCompetitor) merge(locale types.Locale, team TeamWrapper) error {
	var underage *types.UnderageStatus
	if u := team.GetUnderage(); u != "" {
		var parsed types.UnderageStatus
		switch u {
		case "0":
			parsed = types.UnderageNo
		case "1":
			parsed = types.UnderageYes
		default:
			parsed = types.UnderageUnknown
		}
		underage = &parsed
	}

	var (
		playerURNs   []types.URN
		hasPlayerSet bool
	)
	if twp, ok := team.(TeamWithPlayers); ok {
		hasPlayerSet = true
		players := twp.GetPlayers()
		playerURNs = make([]types.URN, 0, len(players))
		for _, p := range players {
			urn, err := types.ParseURN(p.ID)
			if err != nil {
				return fmt.Errorf("parsing URN when refreshing players: %w", err)
			}
			playerURNs = append(playerURNs, *urn)
		}
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.name[locale] = team.GetName()
	l.abbreviation[locale] = team.GetAbbreviation()
	if underage != nil {
		l.underage = underage
	}
	if hasPlayerSet {
		l.players = playerURNs
		l.playersLoaded = true
	}
	return nil
}

// Competitor returns a populated LocalizedCompetitor.
func (c *CompetitorCache) Competitor(ctx context.Context, id types.URN, locales []types.Locale) (*LocalizedCompetitor, error) {
	v, _, err := c.lru.Get(ctx, id, locales)
	if err != nil {
		return nil, notFoundIfAbsent(err)
	}
	return v, nil
}

// reloadCompetitor forces a fresh fetch (used when callers want to refresh
// players or underage that may not have been populated by an earlier
// non-profile API path).
func (c *CompetitorCache) reloadCompetitor(ctx context.Context, id types.URN, locales []types.Locale) (*LocalizedCompetitor, error) {
	c.lru.Clear(id)
	return c.Competitor(ctx, id, locales)
}

// CompetitorIcon returns the icon path, fetching the competitor profile if needed.
func (c *CompetitorCache) CompetitorIcon(ctx context.Context, id types.URN, locale types.Locale) (*string, error) {
	if v, ok := c.loadIcon(id); ok {
		return v, nil
	}

	fetchStarted := time.Now()
	data, err := c.apiClient.FetchCompetitorProfile(ctx, id, locale)
	if err != nil {
		return nil, fmt.Errorf("fetch competitor icon %s/%s: %w", id.ToString(), locale, err)
	}

	// Store under the parent entry's clear-tombstone lock so a
	// ClearCacheItem racing this fetch isn't undone for the icon —
	// StoreSide is atomic with Clear, no check-to-write window.
	// The caller still gets the fresh value.
	c.lru.StoreSide(id, fetchStarted, func() {
		c.storeIcon(id, data.IconPath)
	})
	return data.IconPath, nil
}

// ClearCacheItem removes both the entry and its icon.
func (c *CompetitorCache) ClearCacheItem(id types.URN) {
	c.lru.Clear(id)
	c.deleteIcon(id)
}

func newCompetitorCache(lifeCtx context.Context, client *api.Client, logger *log.Logger) *CompetitorCache {
	cc := &CompetitorCache{
		apiClient: client,
		logger:    logger,
	}
	cc.lru = lru.NewEventCache[types.URN, types.Locale, *LocalizedCompetitor](
		lru.Config{
			Lifetime: lifeCtx,
			// Evict the icon alongside the entry. Icons are populated on
			// every loader call but were previously removed only by the
			// explicit ClearCacheItem — LRU/TTL eviction left them behind
			// forever, an unbounded side-map leak.
			OnEvict: func(key any, _ any) {
				if id, ok := key.(types.URN); ok {
					cc.deleteIcon(id)
				}
			},
			// Commit the load-staged icon ONLY when the parent entry is
			// admitted (atomic with Clear, under the tombstone lock) — a
			// load that fails after staging must not leave an orphaned
			// icon no eviction hook can reach.
			OnAdmit: func(key any, value any) {
				id, okKey := key.(types.URN)
				entry, okVal := value.(*LocalizedCompetitor)
				if !okKey || !okVal {
					return
				}
				if icon, staged := entry.takeStagedIcon(); staged {
					cc.storeIcon(id, icon)
				}
			},
		},
		func(
			ctx context.Context,
			id types.URN,
			missing []types.Locale,
			existing *LocalizedCompetitor,
			hasExisting bool,
		) (*LocalizedCompetitor, error) {
			var entry *LocalizedCompetitor
			if hasExisting {
				// Copy-on-write: merge into a clone so a failed
				// later-locale fetch can't leave this load's earlier
				// locales visible on the live cached entry (admitted
				// only after coverage validation).
				entry = existing.cloneForUpdate()
			} else {
				entry = &LocalizedCompetitor{
					id:           id,
					name:         make(map[types.Locale]string),
					abbreviation: make(map[types.Locale]string),
				}
			}
			for _, locale := range missing {
				data, err := client.FetchCompetitorProfileWithPlayers(ctx, id, locale)
				if err != nil {
					return nil, fmt.Errorf("fetch competitor profile %s/%s: %w", id.ToString(), locale, err)
				}
				// STAGE the icon; the OnAdmit hook commits it iff this
				// load's entry is admitted. Committing here — before the
				// remaining locales, coverage validation, and admission —
				// left an orphaned icon behind whenever a later step
				// failed (and a racing Clear couldn't suppress it).
				entry.stageIcon(data.Competitor.IconPath)
				if err := entry.merge(locale, data); err != nil {
					return nil, fmt.Errorf("merge competitor %s locale %s: %w", id.ToString(), locale, err)
				}
			}
			return entry, nil
		},
	)
	return cc
}

// snapshot projects the cached entry into a types.Competitor value
// (data-copy under the entry's read lock). Resolves players for each
// locale via the supplied factory; returns an error if any player fetch
// fails.
func (l *LocalizedCompetitor) snapshot(
	ctx context.Context,
	icon *string,
	factory entityFactory,
	locales []types.Locale,
) (*types.Competitor, error) {
	l.mu.RLock()
	names := make(map[types.Locale]string, len(l.name))
	for k, v := range l.name {
		names[k] = v
	}
	abbr := make(map[types.Locale]string, len(l.abbreviation))
	for k, v := range l.abbreviation {
		abbr[k] = v
	}
	playerURNs := append([]types.URN(nil), l.players...)
	underage := types.UnderageUnknown
	if l.underage != nil {
		underage = *l.underage
	}
	l.mu.RUnlock()

	// Resolve the roster as a bounded parallel batch: sequential
	// (locale × player) resolution made a cold competitor build scale
	// linearly with roster size (a 10-player roster in 3 locales was 30
	// serial round-trips). Each goroutine writes its own index of its
	// own bucket, so no two goroutines share a slot; the player cache's
	// singleflight still dedups concurrent fetches of one profile.
	players := make(map[types.Locale][]types.Player, len(locales))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(playerLoadConcurrency)
	for _, locale := range locales {
		bucket := make([]types.Player, len(playerURNs))
		players[locale] = bucket
		for i, urn := range playerURNs {
			g.Go(func() error {
				p, err := factory.BuildPlayer(gctx, urn, locale)
				if err != nil {
					return fmt.Errorf("build player %s/%s: %w", urn.ToString(), locale, err)
				}
				bucket[i] = *p
				return nil
			})
		}
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	return &types.Competitor{
		ID:            l.id,
		Names:         names,
		Abbreviations: abbr,
		IconPath:      types.FromPtr(icon),
		Underage:      underage,
		Players:       players,
	}, nil
}

// BuildCompetitor resolves a Competitor snapshot from the cache for the
// given locales. Player URNs on the entry are eagerly resolved into
// populated Player snapshots per locale; the cache fetches are
// deduplicated through the player cache's load mutex.
//
// If the cached entry is missing players (the API path that populated
// it didn't include them), this falls back to reloadCompetitor to force
// a profile-with-players fetch.
func BuildCompetitor(
	ctx context.Context,
	cc *CompetitorCache,
	factory entityFactory,
	id types.URN,
	locales []types.Locale,
) (*types.Competitor, error) {
	item, err := cc.Competitor(ctx, id, locales)
	if err != nil {
		return nil, err
	}
	if !item.playersAreLoaded() {
		item, err = cc.reloadCompetitor(ctx, id, locales)
		if err != nil {
			return nil, fmt.Errorf("loading competitor profile: %w", err)
		}
	}
	var icon *string
	if len(locales) > 0 {
		icon, err = cc.CompetitorIcon(ctx, id, locales[0])
		if err != nil {
			return nil, err
		}
	}
	return item.snapshot(ctx, icon, factory, locales)
}

// BuildTeamCompetitor adds a side qualifier to a Competitor snapshot.
func BuildTeamCompetitor(
	ctx context.Context,
	cc *CompetitorCache,
	factory entityFactory,
	id types.URN,
	qualifier *string,
	locales []types.Locale,
) (*types.TeamCompetitor, error) {
	c, err := BuildCompetitor(ctx, cc, factory, id, locales)
	if err != nil {
		return nil, err
	}
	return &types.TeamCompetitor{
		Competitor: *c,
		Qualifier:  types.FromPtr(qualifier),
	}, nil
}
