package cache

import (
	"context"
	"fmt"
	"sync"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/api/xml"
	"github.com/oddin-gg/gosdk/internal/cache/lru"
	"github.com/oddin-gg/gosdk/types"
	log "github.com/oddin-gg/gosdk/internal/log"
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
// its own simple map (icons are URN-keyed and locale-independent in practice).
// The previous OnAPIResponse cross-population is removed — lazy loading +
// singleflight gives the equivalent.
type CompetitorCache struct {
	apiClient *api.Client
	logger    *log.Logger
	lru       *lru.EventCache[types.URN, types.Locale, *LocalizedCompetitor]

	iconMu sync.RWMutex
	icons  map[types.URN]*string
}

// LocalizedCompetitor carries per-locale name/abbreviation plus
// locale-independent metadata (players, underage).
type LocalizedCompetitor struct {
	mu sync.RWMutex

	id types.URN

	name         map[types.Locale]string
	abbreviation map[types.Locale]string

	underage *types.UnderageStatus
	players  []types.URN
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

// LocalizedName returns the localized name or an error if the locale is not loaded.
func (l *LocalizedCompetitor) LocalizedName(locale types.Locale) (*string, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	v, ok := l.name[locale]
	if !ok {
		return nil, fmt.Errorf("missing locale %s", locale)
	}
	return &v, nil
}

func (l *LocalizedCompetitor) playerURNs() []types.URN {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]types.URN, len(l.players))
	copy(out, l.players)
	return out
}

// merge folds a TeamWrapper payload into the entry under mu.
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

	var playerURNs []types.URN
	if twp, ok := team.(TeamWithPlayers); ok {
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
	if playerURNs != nil {
		l.players = playerURNs
	}
	return nil
}

// Competitor returns a populated LocalizedCompetitor.
func (c *CompetitorCache) Competitor(ctx context.Context, id types.URN, locales []types.Locale) (*LocalizedCompetitor, error) {
	v, _, err := c.lru.Get(ctx, id, locales)
	if err != nil {
		return nil, err
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
	c.iconMu.RLock()
	if v, ok := c.icons[id]; ok {
		c.iconMu.RUnlock()
		return v, nil
	}
	c.iconMu.RUnlock()

	data, err := c.apiClient.FetchCompetitorProfile(ctx, id, locale)
	if err != nil {
		return nil, err
	}

	c.iconMu.Lock()
	c.icons[id] = data.IconPath
	c.iconMu.Unlock()
	return data.IconPath, nil
}

// ClearCacheItem removes both the entry and its icon.
func (c *CompetitorCache) ClearCacheItem(id types.URN) {
	c.lru.Clear(id)
	c.iconMu.Lock()
	delete(c.icons, id)
	c.iconMu.Unlock()
}

func newCompetitorCache(client *api.Client, logger *log.Logger) *CompetitorCache {
	cc := &CompetitorCache{
		apiClient: client,
		logger:    logger,
		icons:     make(map[types.URN]*string),
	}
	cc.lru = lru.NewEventCache[types.URN, types.Locale, *LocalizedCompetitor](
		lru.Config{},
		func(
			ctx context.Context,
			id types.URN,
			missing []types.Locale,
			existing *LocalizedCompetitor,
			hasExisting bool,
		) (*LocalizedCompetitor, error) {
			var entry *LocalizedCompetitor
			if hasExisting {
				entry = existing
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
					return nil, err
				}
				cc.iconMu.Lock()
				cc.icons[id] = data.Competitor.IconPath
				cc.iconMu.Unlock()
				if err := entry.merge(locale, data); err != nil {
					return nil, err
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
	factory types.EntityFactory,
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

	players := map[types.Locale][]types.Player{}
	for _, locale := range locales {
		bucket := make([]types.Player, 0, len(playerURNs))
		for _, urn := range playerURNs {
			p, err := factory.BuildPlayer(ctx, urn, locale)
			if err != nil {
				return nil, fmt.Errorf("build player %s/%s: %w", urn.ToString(), locale, err)
			}
			bucket = append(bucket, *p)
		}
		players[locale] = bucket
	}

	return &types.Competitor{
		ID:            l.id,
		Names:         names,
		Abbreviations: abbr,
		IconPath:      icon,
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
	factory types.EntityFactory,
	id types.URN,
	locales []types.Locale,
) (*types.Competitor, error) {
	item, err := cc.Competitor(ctx, id, locales)
	if err != nil {
		return nil, err
	}
	if len(item.playerURNs()) == 0 {
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
	factory types.EntityFactory,
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
		Qualifier:  qualifier,
	}, nil
}
