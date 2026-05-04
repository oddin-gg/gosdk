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
	"github.com/oddin-gg/gosdk/internal/utils"
	"github.com/oddin-gg/gosdk/protocols"
	log "github.com/oddin-gg/gosdk/internal/log"
)

// MatchCache stores match summaries per (URN, locale).
//
// Phase 3 rewrite: lru.EventCache primitive + per-entry mutex covering
// every field (was: partial-mutex with named fields racing). The
// previous OnAPIResponse cross-population pattern is removed — lazy
// loading + singleflight gives equivalent results with cleaner semantics.
type MatchCache struct {
	apiClient *api.Client
	logger    *log.Logger
	lru       *lru.EventCache[protocols.URN, protocols.Locale, *LocalizedMatch]
}

// LocalizedMatch is the cached representation of a match. All fields are
// guarded by mu. Locales() reports which locales currently have a name set.
type LocalizedMatch struct {
	mu sync.RWMutex

	id protocols.URN

	// Locale-independent fields (set on first load; later loads re-set them).
	scheduledTime        *time.Time
	scheduledEndTime     *time.Time
	sportID              protocols.URN
	tournamentID         protocols.URN
	competitors          []competitor
	liveOddsAvailability *protocols.LiveOddsAvailability
	sportFormat          protocols.SportFormat

	// Per-locale fields.
	name      map[protocols.Locale]string
	extraInfo map[protocols.Locale]map[string]string
}

type competitor struct {
	urn       protocols.URN
	qualifier string
}

// Locales implements lru.LocalizedEntry.
func (m *LocalizedMatch) Locales() []protocols.Locale {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]protocols.Locale, 0, len(m.name))
	for l := range m.name {
		out = append(out, l)
	}
	return out
}

// Accessors are pure-data reads under RLock — no I/O.

func (m *LocalizedMatch) ID() protocols.URN { return m.id }

func (m *LocalizedMatch) Name(locale protocols.Locale) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.name[locale]
	return v, ok
}

func (m *LocalizedMatch) ScheduledTime() *time.Time {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.scheduledTime
}

func (m *LocalizedMatch) ScheduledEndTime() *time.Time {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.scheduledEndTime
}

func (m *LocalizedMatch) SportID() protocols.URN {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sportID
}

func (m *LocalizedMatch) TournamentID() protocols.URN {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.tournamentID
}

func (m *LocalizedMatch) Competitors() []competitor {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]competitor, len(m.competitors))
	copy(out, m.competitors)
	return out
}

func (m *LocalizedMatch) LiveOddsAvailability() *protocols.LiveOddsAvailability {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.liveOddsAvailability
}

func (m *LocalizedMatch) SportFormat() protocols.SportFormat {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sportFormat
}

// ExtraInfo returns a copy of the locale's extra-info map (or nil).
func (m *LocalizedMatch) ExtraInfo(locale protocols.Locale) map[string]string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	src := m.extraInfo[locale]
	if src == nil {
		return nil
	}
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// Match returns a populated LocalizedMatch, fetching missing locales as needed.
func (m *MatchCache) Match(ctx context.Context, id protocols.URN, locales []protocols.Locale) (*LocalizedMatch, error) {
	v, _, err := m.lru.Get(ctx, id, locales)
	if err != nil {
		return nil, err
	}
	return v, nil
}

// OnFeedMessage clears the cache entry on a FixtureChange for a match.
func (m *MatchCache) OnFeedMessage(id protocols.URN, feedMessage *protocols.FeedMessage) {
	if feedMessage.Message == nil {
		return
	}
	if _, ok := feedMessage.Message.(*feedXML.FixtureChange); !ok || id.Type != "match" {
		return
	}
	m.ClearCacheItem(id)
}

// ClearCacheItem is the public invalidation hook.
func (m *MatchCache) ClearCacheItem(id protocols.URN) { m.lru.Clear(id) }

func newMatchCache(client *api.Client, logger *log.Logger) *MatchCache {
	mc := &MatchCache{apiClient: client, logger: logger}
	mc.lru = lru.NewEventCache[protocols.URN, protocols.Locale, *LocalizedMatch](
		lru.Config{},
		func(
			ctx context.Context,
			id protocols.URN,
			missing []protocols.Locale,
			existing *LocalizedMatch,
			hasExisting bool,
		) (*LocalizedMatch, error) {
			var entry *LocalizedMatch
			if hasExisting {
				entry = existing
			} else {
				entry = &LocalizedMatch{
					id:        id,
					name:      make(map[protocols.Locale]string),
					extraInfo: make(map[protocols.Locale]map[string]string),
				}
			}
			for _, locale := range missing {
				data, err := client.FetchMatchSummary(ctx, id, locale)
				if err != nil {
					return nil, err
				}
				if err := entry.merge(locale, data.SportEvent); err != nil {
					return nil, err
				}
			}
			return entry, nil
		},
	)
	return mc
}

// merge folds a freshly fetched match summary into the entry under mu.
func (m *LocalizedMatch) merge(locale protocols.Locale, match apiXML.SportEvent) error {
	tournamentID, err := unwrapURN(&match.Tournament.ID)
	if err != nil {
		return err
	}
	if tournamentID == nil {
		return fmt.Errorf("match %s has no tournament id", match.ID)
	}
	sportID, err := unwrapURN(&match.Tournament.Sport.ID)
	if err != nil {
		return err
	}
	if sportID == nil {
		return fmt.Errorf("match %s has no sport id", match.ID)
	}

	var sportFormat protocols.SportFormat = protocols.SportFormatClassic
	extraInfo := make(map[string]string)
	if match.ExtraInfo != nil && match.ExtraInfo.List != nil {
		for _, info := range match.ExtraInfo.List {
			if info.Key == apiXML.ExtraInfoSportFormatKey && len(info.Value) > 0 {
				switch info.Value {
				case protocols.SportFormatRace:
					sportFormat = protocols.SportFormatRace
				case protocols.SportFormatClassic:
					sportFormat = protocols.SportFormatClassic
				default:
					return fmt.Errorf("unknown sport format for match %s: %s", match.ID, info.Value)
				}
			}
			extraInfo[info.Key] = info.Value
		}
	}

	var competitors []competitor
	if match.Competitors != nil && len(match.Competitors.Competitor) > 0 {
		competitors = make([]competitor, 0, len(match.Competitors.Competitor))
		for _, c := range match.Competitors.Competitor {
			urn, err := protocols.ParseURN(c.ID)
			if err != nil {
				return err
			}
			if urn == nil {
				return fmt.Errorf("invalid or empty competitor urn: %s", c.ID)
			}
			competitors = append(competitors, competitor{urn: *urn, qualifier: c.Qualifier})
		}
	}

	var liveOdds protocols.LiveOddsAvailability
	switch match.LiveOdds {
	case apiXML.LiveOddsNotAvailable:
		liveOdds = protocols.NotAvailableLiveOddsAvailability
	default:
		liveOdds = protocols.AvailableLiveOddsAvailability
	}

	scheduledTime, err := unwrapTime(match.Scheduled)
	if err != nil {
		return err
	}
	scheduledEndTime, err := unwrapTime(match.ScheduledEnd)
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.sportID = *sportID
	m.tournamentID = *tournamentID
	m.competitors = competitors
	m.liveOddsAvailability = &liveOdds
	m.sportFormat = sportFormat
	m.scheduledTime = scheduledTime
	m.scheduledEndTime = scheduledEndTime
	m.name[locale] = match.Name
	m.extraInfo[locale] = extraInfo
	return nil
}

// snapshot projects the cached match entry into the field shapes used
// by protocols.Match (data-copy under the entry's read lock).
func (m *LocalizedMatch) snapshot() (
	names map[protocols.Locale]string,
	extraInfo map[protocols.Locale]map[string]string,
	scheduledTime, scheduledEndTime *time.Time,
	sportID, tournamentID protocols.URN,
	competitors []competitor,
	liveOddsAvailability protocols.LiveOddsAvailability,
	sportFormat protocols.SportFormat,
) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names = make(map[protocols.Locale]string, len(m.name))
	for k, v := range m.name {
		names[k] = v
	}
	extraInfo = make(map[protocols.Locale]map[string]string, len(m.extraInfo))
	for locale, src := range m.extraInfo {
		dst := make(map[string]string, len(src))
		for k, v := range src {
			dst[k] = v
		}
		extraInfo[locale] = dst
	}
	if m.scheduledTime != nil {
		t := *m.scheduledTime
		scheduledTime = &t
	}
	if m.scheduledEndTime != nil {
		t := *m.scheduledEndTime
		scheduledEndTime = &t
	}
	sportID = m.sportID
	tournamentID = m.tournamentID
	competitors = make([]competitor, len(m.competitors))
	copy(competitors, m.competitors)
	if m.liveOddsAvailability != nil {
		liveOddsAvailability = *m.liveOddsAvailability
	}
	sportFormat = m.sportFormat
	return
}

// BuildMatch resolves a *protocols.Match snapshot. Eagerly loads:
//   - the per-locale match summary (entry + name + extra-info)
//   - the tournament (with its embedded sport summary)
//   - per-competitor profiles (across the requested locales)
//   - home/away team competitors when the sport format is "classic"
//   - the fixture (in the primary locale)
//   - the live status snapshot (with localized status-code description)
//
// sportID overrides the cached sportID when non-nil — used by feed
// message decode where the routing key carries the sport.
func BuildMatch(
	ctx context.Context,
	mc *MatchCache,
	factory protocols.EntityFactory,
	id protocols.URN,
	sportID *protocols.URN,
	locales []protocols.Locale,
) (*protocols.Match, error) {
	if len(locales) == 0 {
		return nil, fmt.Errorf("BuildMatch: no locales supplied")
	}
	entry, err := mc.Match(ctx, id, locales)
	if err != nil {
		return nil, err
	}
	names, extraInfo, sched, schedEnd, cachedSport, tournID, comps, liveAvail, format := entry.snapshot()
	resolvedSport := cachedSport
	if sportID != nil {
		resolvedSport = *sportID
	}

	// Tournament (eager).
	tournament, err := factory.BuildTournament(ctx, tournID, resolvedSport, locales)
	if err != nil {
		return nil, fmt.Errorf("build tournament %s: %w", tournID.ToString(), err)
	}

	// Competitors (eager). For classic sports the home/away pair is
	// projected into TeamCompetitor pointers as well.
	competitors := make([]protocols.Competitor, 0, len(comps))
	for _, t := range comps {
		c, err := factory.BuildCompetitor(ctx, t.urn, locales)
		if err != nil {
			return nil, fmt.Errorf("build competitor %s: %w", t.urn.ToString(), err)
		}
		competitors = append(competitors, *c)
	}

	var home, away *protocols.TeamCompetitor
	if format == protocols.SportFormatClassic && len(comps) == 2 {
		hq := comps[0].qualifier
		aq := comps[1].qualifier
		h, err := factory.BuildTeamCompetitor(ctx, comps[0].urn, &hq, locales)
		if err != nil {
			return nil, err
		}
		home = h
		a, err := factory.BuildTeamCompetitor(ctx, comps[1].urn, &aq, locales)
		if err != nil {
			return nil, err
		}
		away = a
	}

	// Fixture (primary locale).
	fixture, err := factory.BuildFixture(ctx, id, locales[0])
	if err != nil {
		return nil, fmt.Errorf("build fixture %s: %w", id.ToString(), err)
	}

	// Status (cache-fed; FetchMatchSummary already populated it as part
	// of mc.Match above via the cache observer).
	status, err := factory.BuildMatchStatus(ctx, id, locales)
	if err != nil {
		return nil, fmt.Errorf("build match status %s: %w", id.ToString(), err)
	}

	return &protocols.Match{
		ID:                   id,
		Names:                names,
		SportID:              resolvedSport,
		ScheduledTime:        sched,
		ScheduledEndTime:     schedEnd,
		LiveOddsAvailability: liveAvail,
		SportFormat:          format,
		ExtraInfo:            extraInfo,
		Tournament:           *tournament,
		Competitors:          competitors,
		HomeCompetitor:       home,
		AwayCompetitor:       away,
		Fixture:              *fixture,
		Status:               *status,
	}, nil
}

// shared helpers used across this package's caches.

func unwrapURN(id *string) (*protocols.URN, error) {
	if id == nil {
		return nil, nil
	}
	return protocols.ParseURN(*id)
}

func unwrapTime(dateTime *utils.DateTime) (*time.Time, error) {
	if dateTime == nil {
		return nil, nil
	}
	t := (time.Time)(*dateTime)
	return &t, nil
}
