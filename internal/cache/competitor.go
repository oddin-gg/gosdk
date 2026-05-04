package cache

import (
	"context"
	"fmt"
	"sync"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/api/xml"
	"github.com/oddin-gg/gosdk/internal/cache/lru"
	"github.com/oddin-gg/gosdk/protocols"
	log "github.com/sirupsen/logrus"
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
	logger    *log.Entry
	lru       *lru.EventCache[protocols.URN, protocols.Locale, *LocalizedCompetitor]

	iconMu sync.RWMutex
	icons  map[protocols.URN]*string
}

// LocalizedCompetitor carries per-locale name/abbreviation plus
// locale-independent metadata (players, underage).
type LocalizedCompetitor struct {
	mu sync.RWMutex

	id protocols.URN

	name         map[protocols.Locale]string
	abbreviation map[protocols.Locale]string

	underage *protocols.UnderageStatus
	players  []protocols.URN
}

// Locales implements lru.LocalizedEntry.
func (l *LocalizedCompetitor) Locales() []protocols.Locale {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]protocols.Locale, 0, len(l.name))
	for locale := range l.name {
		out = append(out, locale)
	}
	return out
}

// LocalizedName returns the localized name or an error if the locale is not loaded.
func (l *LocalizedCompetitor) LocalizedName(locale protocols.Locale) (*string, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	v, ok := l.name[locale]
	if !ok {
		return nil, fmt.Errorf("missing locale %s", locale)
	}
	return &v, nil
}

func (l *LocalizedCompetitor) names() map[protocols.Locale]string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make(map[protocols.Locale]string, len(l.name))
	for k, v := range l.name {
		out[k] = v
	}
	return out
}

func (l *LocalizedCompetitor) abbreviations() map[protocols.Locale]string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make(map[protocols.Locale]string, len(l.abbreviation))
	for k, v := range l.abbreviation {
		out[k] = v
	}
	return out
}

func (l *LocalizedCompetitor) localizedAbbreviation(locale protocols.Locale) (*string, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	v, ok := l.abbreviation[locale]
	if !ok {
		return nil, fmt.Errorf("missing locale %s", locale)
	}
	return &v, nil
}

func (l *LocalizedCompetitor) getUnderage() *protocols.UnderageStatus {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.underage
}

func (l *LocalizedCompetitor) playerURNs() []protocols.URN {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]protocols.URN, len(l.players))
	copy(out, l.players)
	return out
}

// merge folds a TeamWrapper payload into the entry under mu.
func (l *LocalizedCompetitor) merge(locale protocols.Locale, team TeamWrapper) error {
	var underage *protocols.UnderageStatus
	if u := team.GetUnderage(); u != "" {
		var parsed protocols.UnderageStatus
		switch u {
		case "0":
			parsed = protocols.UnderageNo
		case "1":
			parsed = protocols.UnderageYes
		default:
			parsed = protocols.UnderageUnknown
		}
		underage = &parsed
	}

	var playerURNs []protocols.URN
	if twp, ok := team.(TeamWithPlayers); ok {
		players := twp.GetPlayers()
		playerURNs = make([]protocols.URN, 0, len(players))
		for _, p := range players {
			urn, err := protocols.ParseURN(p.ID)
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
func (c *CompetitorCache) Competitor(ctx context.Context, id protocols.URN, locales []protocols.Locale) (*LocalizedCompetitor, error) {
	v, _, err := c.lru.Get(ctx, id, locales)
	if err != nil {
		return nil, err
	}
	return v, nil
}

// reloadCompetitor forces a fresh fetch (used when callers want to refresh
// players or underage that may not have been populated by an earlier
// non-profile API path).
func (c *CompetitorCache) reloadCompetitor(ctx context.Context, id protocols.URN, locales []protocols.Locale) (*LocalizedCompetitor, error) {
	c.lru.Clear(id)
	return c.Competitor(ctx, id, locales)
}

// CompetitorIcon returns the icon path, fetching the competitor profile if needed.
func (c *CompetitorCache) CompetitorIcon(ctx context.Context, id protocols.URN, locale protocols.Locale) (*string, error) {
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
func (c *CompetitorCache) ClearCacheItem(id protocols.URN) {
	c.lru.Clear(id)
	c.iconMu.Lock()
	delete(c.icons, id)
	c.iconMu.Unlock()
}

func newCompetitorCache(client *api.Client, logger *log.Entry) *CompetitorCache {
	cc := &CompetitorCache{
		apiClient: client,
		logger:    logger,
		icons:     make(map[protocols.URN]*string),
	}
	cc.lru = lru.NewEventCache[protocols.URN, protocols.Locale, *LocalizedCompetitor](
		lru.Config{},
		func(
			ctx context.Context,
			id protocols.URN,
			missing []protocols.Locale,
			existing *LocalizedCompetitor,
			hasExisting bool,
		) (*LocalizedCompetitor, error) {
			var entry *LocalizedCompetitor
			if hasExisting {
				entry = existing
			} else {
				entry = &LocalizedCompetitor{
					id:           id,
					name:         make(map[protocols.Locale]string),
					abbreviation: make(map[protocols.Locale]string),
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

// competitorImpl implements protocols.Competitor.
type competitorImpl struct {
	id              protocols.URN
	competitorCache *CompetitorCache
	entityFactory   protocols.EntityFactory
	locales         []protocols.Locale
}

func (c competitorImpl) IconPath() (*string, error) {
	if len(c.locales) == 0 {
		return nil, fmt.Errorf("missing locales")
	}
	return c.competitorCache.CompetitorIcon(context.Background(), c.id, c.locales[0])
}

func (c competitorImpl) ID() protocols.URN { return c.id }

func (c competitorImpl) Names() (map[protocols.Locale]string, error) {
	item, err := c.competitorCache.Competitor(context.Background(), c.id, c.locales)
	if err != nil {
		return nil, err
	}
	return item.names(), nil
}

func (c competitorImpl) LocalizedName(locale protocols.Locale) (*string, error) {
	item, err := c.competitorCache.Competitor(context.Background(), c.id, c.locales)
	if err != nil {
		return nil, err
	}
	return item.LocalizedName(locale)
}

func (c competitorImpl) Abbreviations() (map[protocols.Locale]string, error) {
	item, err := c.competitorCache.Competitor(context.Background(), c.id, c.locales)
	if err != nil {
		return nil, err
	}
	return item.abbreviations(), nil
}

func (c competitorImpl) LocalizedAbbreviation(locale protocols.Locale) (*string, error) {
	item, err := c.competitorCache.Competitor(context.Background(), c.id, c.locales)
	if err != nil {
		return nil, err
	}
	return item.localizedAbbreviation(locale)
}

func (c competitorImpl) Players() (map[protocols.Locale][]protocols.Player, error) {
	item, err := c.competitorCache.Competitor(context.Background(), c.id, c.locales)
	if err != nil {
		return nil, err
	}
	urns := item.playerURNs()
	if len(urns) == 0 {
		// Re-fetch the profile-with-players to surface the player list
		// (a non-profile fetch may have populated the entry without players).
		item, err = c.competitorCache.reloadCompetitor(context.Background(), c.id, c.locales)
		if err != nil {
			return nil, fmt.Errorf("loading players into cache: %w", err)
		}
		urns = item.playerURNs()
	}
	out := make(map[protocols.Locale][]protocols.Player, len(c.locales))
	for _, locale := range c.locales {
		players := make([]protocols.Player, 0, len(urns))
		for _, urn := range urns {
			players = append(players, c.entityFactory.BuildPlayer(urn, locale))
		}
		out[locale] = players
	}
	return out, nil
}

func (c competitorImpl) LocalizedPlayers(locale protocols.Locale) ([]protocols.Player, error) {
	item, err := c.competitorCache.Competitor(context.Background(), c.id, c.locales)
	if err != nil {
		return nil, err
	}
	urns := item.playerURNs()
	if len(urns) == 0 {
		item, err = c.competitorCache.reloadCompetitor(context.Background(), c.id, c.locales)
		if err != nil {
			return nil, fmt.Errorf("loading players into cache: %w", err)
		}
		urns = item.playerURNs()
	}
	players := make([]protocols.Player, 0, len(urns))
	for _, urn := range urns {
		players = append(players, c.entityFactory.BuildPlayer(urn, locale))
	}
	return players, nil
}

func (c competitorImpl) Underage() (protocols.UnderageStatus, error) {
	item, err := c.competitorCache.Competitor(context.Background(), c.id, c.locales)
	if err != nil {
		return protocols.UnderageUnknown, err
	}
	underage := item.getUnderage()
	if underage == nil {
		// Underage may not have been populated by a non-profile API path.
		item, err = c.competitorCache.reloadCompetitor(context.Background(), c.id, c.locales)
		if err != nil {
			return protocols.UnderageUnknown, fmt.Errorf("loading competitor profile into cache: %w", err)
		}
		underage = item.getUnderage()
	}
	if underage == nil {
		return protocols.UnderageUnknown, nil
	}
	return *underage, nil
}

// teamCompetitorImpl decorates a Competitor with a "home"/"away" qualifier.
type teamCompetitorImpl struct {
	qualifier  *string
	competitor protocols.Competitor
}

func (t teamCompetitorImpl) IconPath() (*string, error) { return t.competitor.IconPath() }
func (t teamCompetitorImpl) ID() protocols.URN          { return t.competitor.ID() }
func (t teamCompetitorImpl) Names() (map[protocols.Locale]string, error) {
	return t.competitor.Names()
}
func (t teamCompetitorImpl) LocalizedName(locale protocols.Locale) (*string, error) {
	return t.competitor.LocalizedName(locale)
}
func (t teamCompetitorImpl) Abbreviations() (map[protocols.Locale]string, error) {
	return t.competitor.Abbreviations()
}
func (t teamCompetitorImpl) LocalizedAbbreviation(locale protocols.Locale) (*string, error) {
	return t.competitor.LocalizedAbbreviation(locale)
}
func (t teamCompetitorImpl) Players() (map[protocols.Locale][]protocols.Player, error) {
	return t.competitor.Players()
}
func (t teamCompetitorImpl) LocalizedPlayers(locale protocols.Locale) ([]protocols.Player, error) {
	return t.competitor.LocalizedPlayers(locale)
}
func (t teamCompetitorImpl) Qualifier() *string                       { return t.qualifier }
func (t teamCompetitorImpl) Underage() (protocols.UnderageStatus, error) { return t.competitor.Underage() }

// NewCompetitor ...
func NewCompetitor(id protocols.URN, competitorCache *CompetitorCache, entityFactory protocols.EntityFactory, locales []protocols.Locale) protocols.Competitor {
	return &competitorImpl{
		id:              id,
		competitorCache: competitorCache,
		entityFactory:   entityFactory,
		locales:         locales,
	}
}
