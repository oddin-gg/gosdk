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
	"github.com/oddin-gg/gosdk/internal/utils"
	"github.com/oddin-gg/gosdk/types"
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
	lru       *lru.EventCache[types.URN, types.Locale, *LocalizedMatch]
}

// LocalizedMatch is the cached representation of a match. All fields are
// guarded by mu. Locales() reports which locales currently have a name set.
type LocalizedMatch struct {
	mu sync.RWMutex

	id types.URN

	// Locale-independent fields (set on first load; later loads re-set them).
	scheduledTime        *time.Time
	scheduledEndTime     *time.Time
	sportID              types.URN
	tournamentID         types.URN
	competitors          []competitor
	liveOddsAvailability *types.LiveOddsAvailability
	sportFormat          types.SportFormat

	// Per-locale fields.
	name      map[types.Locale]string
	extraInfo map[types.Locale]map[string]string

	// Locale-independent (the API returns the same reference-id set
	// across locales). Populated from SportEvent.ReferenceIDs;
	// forward-ported from main commit fcc3c0d (PR #38).
	referenceIDs map[string]string
}

type competitor struct {
	urn       types.URN
	qualifier string
}

// Locales implements lru.LocalizedEntry.
func (m *LocalizedMatch) Locales() []types.Locale {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]types.Locale, 0, len(m.name))
	for l := range m.name {
		out = append(out, l)
	}
	return out
}

// cloneForUpdate returns a copy the loader can merge into without
// mutating the live cached entry (copy-on-write): a failed later-locale
// fetch must not leave earlier locales of the SAME load visible to
// concurrent readers — the load/admit transaction admits the clone only
// after every locale and the coverage validation succeed. Top-level maps
// are copied one level deep; inner maps, slices, and pointers are safe
// to alias because merge() only ever REPLACES them wholesale.
func (m *LocalizedMatch) cloneForUpdate() *LocalizedMatch {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c := &LocalizedMatch{
		id:                   m.id,
		scheduledTime:        m.scheduledTime,
		scheduledEndTime:     m.scheduledEndTime,
		sportID:              m.sportID,
		tournamentID:         m.tournamentID,
		competitors:          m.competitors,
		liveOddsAvailability: m.liveOddsAvailability,
		sportFormat:          m.sportFormat,
		name:                 make(map[types.Locale]string, len(m.name)+1),
		extraInfo:            make(map[types.Locale]map[string]string, len(m.extraInfo)+1),
		referenceIDs:         m.referenceIDs,
	}
	for k, v := range m.name {
		c.name[k] = v
	}
	for k, v := range m.extraInfo {
		c.extraInfo[k] = v
	}
	return c
}

// Accessors are pure-data reads under RLock — no I/O.

func (m *LocalizedMatch) ID() types.URN { return m.id }

func (m *LocalizedMatch) Name(locale types.Locale) (string, bool) {
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

func (m *LocalizedMatch) SportID() types.URN {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sportID
}

func (m *LocalizedMatch) TournamentID() types.URN {
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

func (m *LocalizedMatch) LiveOddsAvailability() *types.LiveOddsAvailability {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.liveOddsAvailability
}

func (m *LocalizedMatch) SportFormat() types.SportFormat {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sportFormat
}

// ExtraInfo returns a copy of the locale's extra-info map (or nil).
func (m *LocalizedMatch) ExtraInfo(locale types.Locale) map[string]string {
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
func (m *MatchCache) Match(ctx context.Context, id types.URN, locales []types.Locale) (*LocalizedMatch, error) {
	v, _, err := m.lru.Get(ctx, id, locales)
	if err != nil {
		return nil, notFoundIfAbsent(err)
	}
	return v, nil
}

// OnFeedMessage clears the cache entry on a FixtureChange for a match.
func (m *MatchCache) OnFeedMessage(id types.URN, feedMessage *types.FeedMessage) {
	if feedMessage.Message == nil {
		return
	}
	if _, ok := feedMessage.Message.(*feedXML.FixtureChange); !ok || id.Type != "match" {
		return
	}
	m.ClearCacheItem(id)
}

// ClearCacheItem is the public invalidation hook.
func (m *MatchCache) ClearCacheItem(id types.URN) { m.lru.Clear(id) }

func newMatchCache(lifeCtx context.Context, client *api.Client, logger *log.Logger) *MatchCache {
	mc := &MatchCache{apiClient: client, logger: logger}
	mc.lru = lru.NewEventCache[types.URN, types.Locale, *LocalizedMatch](
		lru.Config{Lifetime: lifeCtx},
		func(
			ctx context.Context,
			id types.URN,
			missing []types.Locale,
			existing *LocalizedMatch,
			hasExisting bool,
		) (*LocalizedMatch, error) {
			var entry *LocalizedMatch
			if hasExisting {
				// Copy-on-write: merge into a clone so a failed
				// later-locale fetch can't leave this load's earlier
				// locales visible on the live cached entry (admitted
				// only after coverage validation).
				entry = existing.cloneForUpdate()
			} else {
				entry = &LocalizedMatch{
					id:        id,
					name:      make(map[types.Locale]string),
					extraInfo: make(map[types.Locale]map[string]string),
				}
			}
			for _, locale := range missing {
				data, err := client.FetchMatchSummary(ctx, id, locale)
				if err != nil {
					return nil, fmt.Errorf("fetch match summary %s/%s: %w", id.ToString(), locale, err)
				}
				if err := entry.merge(locale, data.SportEvent); err != nil {
					return nil, fmt.Errorf("merge match %s locale %s: %w", id.ToString(), locale, err)
				}
			}
			return entry, nil
		},
	)
	return mc
}

// merge folds a freshly fetched match summary into the entry under mu.
func (m *LocalizedMatch) merge(locale types.Locale, match apiXML.SportEvent) error {
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

	// An absent sport_format key keeps the legacy default (classic) —
	// the upstream catalog omits it for head-to-head sports.
	var sportFormat types.SportFormat = types.SportFormatClassic
	extraInfo := make(map[string]string)
	if match.ExtraInfo != nil && match.ExtraInfo.List != nil {
		for _, info := range match.ExtraInfo.List {
			if info.Key == apiXML.ExtraInfoSportFormatKey && len(info.Value) > 0 {
				switch info.Value {
				case types.SportFormatRace:
					sportFormat = types.SportFormatRace
				case types.SportFormatClassic:
					sportFormat = types.SportFormatClassic
				default:
					// Forward-compat: a newly introduced upstream format
					// must NOT abort match construction (pre-fix this
					// returned an error, breaking Client.Match, the
					// match-list queries, and feed enrichment for every
					// match carrying the new value — feed delivery then
					// continued with Event()==nil). Public model defines
					// SportFormatUnknown for exactly this; the raw value
					// stays readable via ExtraInfo["sport_format"].
					sportFormat = types.SportFormatUnknown
				}
			}
			extraInfo[info.Key] = info.Value
		}
	}

	var competitors []competitor
	if match.Competitors != nil && len(match.Competitors.Competitor) > 0 {
		competitors = make([]competitor, 0, len(match.Competitors.Competitor))
		for _, c := range match.Competitors.Competitor {
			urn, err := types.ParseURN(c.ID)
			if err != nil {
				return err
			}
			if urn == nil {
				return fmt.Errorf("invalid or empty competitor urn: %s", c.ID)
			}
			competitors = append(competitors, competitor{urn: *urn, qualifier: c.Qualifier})
		}
	}

	var liveOdds types.LiveOddsAvailability
	switch match.LiveOdds {
	case apiXML.LiveOddsBooked, apiXML.LiveOddsBookable, apiXML.LiveOddsBuyable:
		liveOdds = types.AvailableLiveOddsAvailability
	case apiXML.LiveOddsNotAvailable:
		liveOdds = types.NotAvailableLiveOddsAvailability
	default:
		// Fail CLOSED: an absent attribute (""), a malformed value, or
		// a future enum value the SDK doesn't know must not report live
		// odds as available — pre-fix the default branch did exactly
		// that, and consumers could enable live behavior for events
		// with no confirmed availability. The known wire values are
		// enumerated above; anything else is conservatively
		// not-available until the upstream schema (and this mapping)
		// says otherwise.
		liveOdds = types.NotAvailableLiveOddsAvailability
	}

	scheduledTime, err := unwrapTime(match.Scheduled)
	if err != nil {
		return err
	}
	scheduledEndTime, err := unwrapTime(match.ScheduledEnd)
	if err != nil {
		return err
	}

	// Reference IDs (locale-independent). Built outside the lock so a
	// nil ReferenceIDs payload doesn't clobber a previously-populated
	// map; only refresh when the API actually returned the block.
	var refIDs map[string]string
	if match.ReferenceIDs != nil {
		refIDs = make(map[string]string, len(match.ReferenceIDs.ReferenceID))
		for _, ref := range match.ReferenceIDs.ReferenceID {
			refIDs[ref.Name] = ref.Value
		}
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
	if refIDs != nil {
		m.referenceIDs = refIDs
	}
	return nil
}

// snapshot projects the cached match entry into the field shapes used
// by types.Match (data-copy under the entry's read lock).
func (m *LocalizedMatch) snapshot() (
	names map[types.Locale]string,
	extraInfo map[types.Locale]map[string]string,
	scheduledTime, scheduledEndTime *time.Time,
	sportID, tournamentID types.URN,
	competitors []competitor,
	liveOddsAvailability types.LiveOddsAvailability,
	sportFormat types.SportFormat,
	referenceIDs map[string]string,
) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names = make(map[types.Locale]string, len(m.name))
	for k, v := range m.name {
		names[k] = v
	}
	extraInfo = make(map[types.Locale]map[string]string, len(m.extraInfo))
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
	if m.referenceIDs != nil {
		// Decouple from the cache so caller mutations on the returned
		// map don't leak into the cached entry.
		referenceIDs = make(map[string]string, len(m.referenceIDs))
		for k, v := range m.referenceIDs {
			referenceIDs[k] = v
		}
	}
	return
}

// BuildMatch resolves a *types.Match snapshot. Eagerly loads:
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
	factory entityFactory,
	id types.URN,
	sportID *types.URN,
	locales []types.Locale,
) (*types.Match, error) {
	if len(locales) == 0 {
		return nil, fmt.Errorf("BuildMatch: no locales supplied")
	}
	entry, err := mc.Match(ctx, id, locales)
	if err != nil {
		return nil, err
	}
	names, extraInfo, sched, schedEnd, cachedSport, tournID, comps, liveAvail, format, refIDs := entry.snapshot()
	// The cached sport comes from the API summary and is AUTHORITATIVE;
	// the sportID argument comes from a feed message's ROUTING KEY.
	// Pre-fix the route value replaced the cached sport without
	// comparison — a mis-routed (or malicious) delivery could relabel a
	// known match under a conflicting sport identity. The route value
	// is used only when the cache carries no sport yet; a conflict
	// keeps the cached identity and logs.
	resolvedSport := cachedSport
	if sportID != nil {
		switch {
		case cachedSport == (types.URN{}):
			resolvedSport = *sportID
		case *sportID != cachedSport:
			if mc.logger != nil {
				mc.logger.WithField("match_id", id.ToString()).
					WithField("cached_sport", cachedSport.ToString()).
					WithField("route_sport", sportID.ToString()).
					Warn("cache: routing-key sport conflicts with cached sport; keeping cached identity")
			}
		}
	}

	// Tournament (eager).
	tournament, err := factory.BuildTournament(ctx, tournID, resolvedSport, locales)
	if err != nil {
		return nil, fmt.Errorf("build tournament %s: %w", tournID.ToString(), err)
	}

	// Competitors (eager). For classic sports the home/away pair is
	// projected into TeamCompetitor pointers as well.
	competitors := make([]types.Competitor, 0, len(comps))
	for _, t := range comps {
		c, err := factory.BuildCompetitor(ctx, t.urn, locales)
		if err != nil {
			return nil, fmt.Errorf("build competitor %s: %w", t.urn.ToString(), err)
		}
		competitors = append(competitors, *c)
	}

	// Classic-format home/away assignment is keyed on the per-competitor
	// `qualifier` ("home"/"away"), NOT on slice position. The API does
	// not guarantee ordering: an [away, home] response would silently
	// produce swapped competitors under the previous index-based code.
	// If neither qualifier is recognized, leave home/away nil rather
	// than guessing — downstream consumers tolerate the missing pointers.
	var home, away *types.TeamCompetitor
	if format == types.SportFormatClassic && len(comps) == 2 {
		var homeIdx, awayIdx = -1, -1
		for i, c := range comps {
			switch c.qualifier {
			case "home":
				homeIdx = i
			case "away":
				awayIdx = i
			}
		}
		if homeIdx >= 0 && awayIdx >= 0 {
			hq := comps[homeIdx].qualifier
			aq := comps[awayIdx].qualifier
			h, err := factory.BuildTeamCompetitor(ctx, comps[homeIdx].urn, &hq, locales)
			if err != nil {
				return nil, err
			}
			home = h
			a, err := factory.BuildTeamCompetitor(ctx, comps[awayIdx].urn, &aq, locales)
			if err != nil {
				return nil, err
			}
			away = a
		}
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

	return &types.Match{
		ID:                   id,
		Names:                names,
		SportID:              resolvedSport,
		ScheduledTime:        sched,
		ScheduledEndTime:     schedEnd,
		LiveOddsAvailability: liveAvail,
		SportFormat:          format,
		ExtraInfo:            extraInfo,
		ReferenceIDs:         refIDs,
		Tournament:           *tournament,
		Competitors:          competitors,
		HomeCompetitor:       home,
		AwayCompetitor:       away,
		Fixture:              *fixture,
		Status:               *status,
	}, nil
}

// shared helpers used across this package's caches.

func unwrapURN(id *string) (*types.URN, error) {
	if id == nil {
		return nil, nil
	}
	return types.ParseURN(*id)
}

// clonePtr returns a fresh *T whose pointee equals *p, or nil when
// p is nil. Used by Build* helpers to defensively decouple returned
// snapshots from cache-owned pointer fields — a caller mutating the
// pointee of a snapshot must not corrupt the cache's value, and a
// second read must return the cache's current state, not the
// caller-mutated value.
func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// cloneStringMap returns a shallow copy of m, or nil when m is nil.
// Strings are immutable in Go so a shallow copy fully decouples
// caller and cache.
func cloneStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func unwrapTime(dateTime *utils.DateTime) (*time.Time, error) {
	if dateTime == nil {
		return nil, nil
	}
	t := (time.Time)(*dateTime)
	return &t, nil
}
