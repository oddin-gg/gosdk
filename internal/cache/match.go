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

// matchImpl implements protocols.Match. Accessors call MatchCache.Match
// with context.Background() because protocols.Match accessors don't take
// a ctx — Phase 6 reshapes accessors to be pure-data, eliminating these
// hidden lazy loads.
type matchImpl struct {
	id            protocols.URN
	localSportID  *protocols.URN
	matchCache    *MatchCache
	entityFactory protocols.EntityFactory
	locales       []protocols.Locale
}

func (m matchImpl) ID() protocols.URN { return m.id }

func (m matchImpl) LocalizedName(locale protocols.Locale) (*string, error) {
	item, err := m.matchCache.Match(context.Background(), m.id, m.locales)
	if err != nil {
		return nil, err
	}
	v, ok := item.Name(locale)
	if !ok {
		return nil, fmt.Errorf("missing locale %s", locale)
	}
	return &v, nil
}

func (m matchImpl) SportID() (*protocols.URN, error) {
	if m.localSportID != nil {
		return m.localSportID, nil
	}
	item, err := m.matchCache.Match(context.Background(), m.id, m.locales)
	if err != nil {
		return nil, err
	}
	id := item.SportID()
	m.localSportID = &id
	return m.localSportID, nil
}

func (m matchImpl) ScheduledTime() (*time.Time, error) {
	item, err := m.matchCache.Match(context.Background(), m.id, m.locales)
	if err != nil {
		return nil, err
	}
	return item.ScheduledTime(), nil
}

func (m matchImpl) ScheduledEndTime() (*time.Time, error) {
	item, err := m.matchCache.Match(context.Background(), m.id, m.locales)
	if err != nil {
		return nil, err
	}
	return item.ScheduledEndTime(), nil
}

func (m matchImpl) LiveOddsAvailability() (*protocols.LiveOddsAvailability, error) {
	item, err := m.matchCache.Match(context.Background(), m.id, m.locales)
	if err != nil {
		return nil, err
	}
	return item.LiveOddsAvailability(), nil
}

func (m matchImpl) Competitors() ([]protocols.Competitor, error) {
	item, err := m.matchCache.Match(context.Background(), m.id, m.locales)
	if err != nil {
		return nil, err
	}
	teams := item.Competitors()
	if len(teams) < 2 {
		return nil, fmt.Errorf("match %s has less than 2 competitors", m.id.ToString())
	}
	out := make([]protocols.Competitor, 0, len(teams))
	for _, t := range teams {
		t := t
		out = append(out, teamCompetitorImpl{
			qualifier:  &t.qualifier,
			competitor: m.entityFactory.BuildCompetitor(t.urn, m.locales),
		})
	}
	return out, nil
}

func (m matchImpl) SportFormat() (protocols.SportFormat, error) {
	item, err := m.matchCache.Match(context.Background(), m.id, m.locales)
	if err != nil {
		return protocols.SportFormatUnknown, err
	}
	return item.SportFormat(), nil
}

func (m matchImpl) ExtraInfo() (map[string]string, error) {
	item, err := m.matchCache.Match(context.Background(), m.id, m.locales)
	if err != nil {
		return nil, err
	}
	return item.ExtraInfo(m.locales[0]), nil
}

func (m matchImpl) Status() protocols.MatchStatus {
	return m.entityFactory.BuildMatchStatus(m.id, m.locales)
}

func (m matchImpl) Tournament() (protocols.Tournament, error) {
	sportID, err := m.SportID()
	if err != nil {
		return nil, err
	}
	item, err := m.matchCache.Match(context.Background(), m.id, m.locales)
	if err != nil {
		return nil, err
	}
	return m.entityFactory.BuildTournament(item.TournamentID(), *sportID, m.locales), nil
}

func (m matchImpl) homeAwayCompetitor(home bool) (protocols.TeamCompetitor, error) {
	item, err := m.matchCache.Match(context.Background(), m.id, m.locales)
	if err != nil {
		return nil, err
	}
	teams := item.Competitors()
	switch {
	case len(teams) < 2:
		return nil, fmt.Errorf("match %s has less than 2 competitors", m.id.ToString())
	case item.SportFormat() != protocols.SportFormatClassic:
		return nil, fmt.Errorf("match %s is not a classic sport format", m.id.ToString())
	case len(teams) > 2:
		return nil, fmt.Errorf("classic sport match %s has more than 2 competitors", m.id.ToString())
	}
	team := teams[0]
	if !home {
		team = teams[1]
	}
	return teamCompetitorImpl{
		qualifier:  &team.qualifier,
		competitor: m.entityFactory.BuildCompetitor(team.urn, m.locales),
	}, nil
}

func (m matchImpl) HomeCompetitor() (protocols.TeamCompetitor, error) { return m.homeAwayCompetitor(true) }
func (m matchImpl) AwayCompetitor() (protocols.TeamCompetitor, error) { return m.homeAwayCompetitor(false) }

// Fixture returns the fixture snapshot for this match in the default
// locale. On fetch error returns a zero-value Fixture (callers can
// inspect the err via FixtureWithError). The signature stays errorless
// to match the protocols.Match interface; the reshaped Fixture type is
// a value struct now.
func (m matchImpl) Fixture() protocols.Fixture {
	f, _ := m.entityFactory.BuildFixture(context.Background(), m.id, m.locales[0])
	if f == nil {
		return protocols.Fixture{}
	}
	return *f
}

// NewMatch ...
func NewMatch(id protocols.URN, sportID *protocols.URN, matchCache *MatchCache, entityFactory protocols.EntityFactory, locales []protocols.Locale) protocols.Match {
	return &matchImpl{
		id:            id,
		localSportID:  sportID,
		matchCache:    matchCache,
		entityFactory: entityFactory,
		locales:       locales,
	}
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
