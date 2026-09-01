package sport

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/cache"
	"github.com/oddin-gg/gosdk/internal/config"
	"github.com/oddin-gg/gosdk/internal/factory"
	"github.com/oddin-gg/gosdk/types"
)

type fixtureChangeImpl struct {
	id          types.URN
	updatedTime time.Time
}

func (f fixtureChangeImpl) SportEventID() types.URN {
	return f.id
}

func (f fixtureChangeImpl) UpdateTime() time.Time {
	return f.updatedTime
}

// Manager exposes sport/entity reads over the cache + EntityFactory.
//
// Public methods take ctx and propagate it through the EntityFactory and
// cache loaders to the API client — the cache layer's singleflight loads
// are ctx-aware (each waiter bounds its own wait; the shared fetch
// detaches to the owner lifetime). The Phase-3 rewrite that introduced
// this propagation has landed.
type Manager struct {
	entityFactory         *factory.EntityFactory
	apiClient             *api.Client
	oddsFeedConfiguration config.Config
	cacheManager          *cache.Manager
}

// Sports ...
func (m *Manager) Sports(ctx context.Context) ([]types.Sport, error) {
	return m.LocalizedSports(ctx, m.oddsFeedConfiguration.DefaultLocale())
}

// LocalizedSports ...
func (m *Manager) LocalizedSports(ctx context.Context, locale types.Locale) ([]types.Sport, error) {
	return m.entityFactory.BuildSports(ctx, []types.Locale{locale})
}

// ActiveTournaments ...
func (m *Manager) ActiveTournaments(ctx context.Context) ([]types.Tournament, error) {
	return m.LocalizedActiveTournaments(ctx, m.oddsFeedConfiguration.DefaultLocale())
}

// LocalizedActiveTournaments ...
func (m *Manager) LocalizedActiveTournaments(ctx context.Context, locale types.Locale) ([]types.Tournament, error) {
	return m.MultiLocalizedActiveTournaments(ctx, []types.Locale{locale})
}

// MultiLocalizedActiveTournaments preloads every supplied locale into
// the per-tournament cache so callers passing multiple locales (e.g.
// for a multi-language UI) avoid the round-trip-per-locale cost the
// single-locale variant forces.
func (m *Manager) MultiLocalizedActiveTournaments(ctx context.Context, locales []types.Locale) ([]types.Tournament, error) {
	if len(locales) == 0 {
		locales = []types.Locale{m.oddsFeedConfiguration.DefaultLocale()}
	}
	// LocalizedSports populates the sport's Names map for the *first*
	// locale. We use it to discover the tournament IDs; per-locale
	// detail is populated below by BuildTournaments(locales).
	sports, err := m.LocalizedSports(ctx, locales[0])
	if err != nil {
		return nil, err
	}

	var result []types.Tournament
	for _, sport := range sports {
		tournaments, err := m.entityFactory.BuildTournaments(ctx, sport.TournamentIDs, sport.ID, locales)
		if err != nil {
			return nil, err
		}
		result = append(result, tournaments...)
	}

	return result, nil
}

// SportActiveTournaments ...
func (m *Manager) SportActiveTournaments(ctx context.Context, sportName string) ([]types.Tournament, error) {
	return m.LocalizedSportActiveTournaments(ctx, sportName, m.oddsFeedConfiguration.DefaultLocale())
}

// LocalizedSportActiveTournaments ...
func (m *Manager) LocalizedSportActiveTournaments(ctx context.Context, sportName string, locale types.Locale) ([]types.Tournament, error) {
	return m.MultiLocalizedSportActiveTournaments(ctx, sportName, []types.Locale{locale})
}

// MultiLocalizedSportActiveTournaments resolves sportName against
// EVERY supplied locale's catalog (not just the default), then
// preloads every locale in the returned tournaments. Mirrors
// Java/.NET getActiveTournaments(sportName, locale) where the
// requested locale drives the name lookup itself.
//
// Pre-fix this code searched the *default*-locale catalog regardless
// of the locale parameter, so non-default-locale lookups failed
// unless the sport's default-locale name happened to be cached.
func (m *Manager) MultiLocalizedSportActiveTournaments(ctx context.Context, sportName string, locales []types.Locale) ([]types.Tournament, error) {
	if len(locales) == 0 {
		locales = []types.Locale{m.oddsFeedConfiguration.DefaultLocale()}
	}
	// Search each requested locale's catalog. The first match wins.
	// LocalizedSports populates that locale's Names so sport.Name(loc)
	// is guaranteed non-empty after a successful fetch.
	for _, locale := range locales {
		sports, err := m.LocalizedSports(ctx, locale)
		if err != nil {
			return nil, err
		}
		for _, sport := range sports {
			name, ok := sport.Name(locale).Get()
			if !ok || name == "" {
				continue
			}
			if strings.EqualFold(name, sportName) {
				return m.entityFactory.BuildTournaments(ctx, sport.TournamentIDs, sport.ID, locales)
			}
		}
	}

	return nil, fmt.Errorf("cannot find any sport with given name %s in locales %v", sportName, locales)
}

// MatchesFor ...
func (m *Manager) MatchesFor(ctx context.Context, date time.Time) ([]types.Match, error) {
	return m.LocalizedMatchesFor(ctx, date, m.oddsFeedConfiguration.DefaultLocale())
}

// LocalizedMatchesFor ...
func (m *Manager) LocalizedMatchesFor(ctx context.Context, date time.Time, locale types.Locale) ([]types.Match, error) {
	return m.MultiLocalizedMatchesFor(ctx, date, []types.Locale{locale})
}

// MultiLocalizedMatchesFor preloads every supplied locale into each
// returned match.
func (m *Manager) MultiLocalizedMatchesFor(ctx context.Context, date time.Time, locales []types.Locale) ([]types.Match, error) {
	if len(locales) == 0 {
		locales = []types.Locale{m.oddsFeedConfiguration.DefaultLocale()}
	}
	// FetchMatches's per-call locale only drives the API response
	// content for the index call. Per-match detail (which is what
	// the caller actually iterates) is built below with the full
	// locale set via BuildMatch(locales).
	data, err := m.apiClient.FetchMatches(ctx, date, locales[0])
	if err != nil {
		return nil, err
	}

	result := make([]types.Match, 0, len(data))
	for i := range data {
		id, err := types.ParseURN(data[i].ID)
		if err != nil {
			return nil, err
		}
		match, err := m.entityFactory.BuildMatch(ctx, *id, locales, nil)
		if err != nil {
			return nil, err
		}
		result = append(result, *match)
	}

	return result, nil
}

// LiveMatches ...
func (m *Manager) LiveMatches(ctx context.Context) ([]types.Match, error) {
	return m.LocalizedLiveMatches(ctx, m.oddsFeedConfiguration.DefaultLocale())
}

// LocalizedLiveMatches ...
func (m *Manager) LocalizedLiveMatches(ctx context.Context, locale types.Locale) ([]types.Match, error) {
	return m.MultiLocalizedLiveMatches(ctx, []types.Locale{locale})
}

// MultiLocalizedLiveMatches preloads every supplied locale into each
// returned match.
func (m *Manager) MultiLocalizedLiveMatches(ctx context.Context, locales []types.Locale) ([]types.Match, error) {
	if len(locales) == 0 {
		locales = []types.Locale{m.oddsFeedConfiguration.DefaultLocale()}
	}
	data, err := m.apiClient.FetchLiveMatches(ctx, locales[0])
	if err != nil {
		return nil, err
	}

	result := make([]types.Match, 0, len(data))
	for i := range data {
		id, err := types.ParseURN(data[i].ID)
		if err != nil {
			return nil, err
		}
		match, err := m.entityFactory.BuildMatch(ctx, *id, locales, nil)
		if err != nil {
			return nil, err
		}
		result = append(result, *match)
	}

	return result, nil
}

// Match ...
func (m *Manager) Match(ctx context.Context, id types.URN) (types.Match, error) {
	return m.LocalizedMatch(ctx, id, m.oddsFeedConfiguration.DefaultLocale())
}

// LocalizedMatch ...
func (m *Manager) LocalizedMatch(ctx context.Context, id types.URN, locale types.Locale) (types.Match, error) {
	return m.MultiLocalizedMatch(ctx, id, []types.Locale{locale})
}

// MultiLocalizedMatch preloads the match's Names map for every supplied
// locale. Single-call multi-locale fetch matches Java/.NET behaviour.
func (m *Manager) MultiLocalizedMatch(ctx context.Context, id types.URN, locales []types.Locale) (types.Match, error) {
	if len(locales) == 0 {
		locales = []types.Locale{m.oddsFeedConfiguration.DefaultLocale()}
	}
	match, err := m.entityFactory.BuildMatch(ctx, id, locales, nil)
	if err != nil {
		return types.Match{}, err
	}
	return *match, nil
}

// Competitor ...
func (m *Manager) Competitor(ctx context.Context, id types.URN) (types.Competitor, error) {
	return m.LocalizedCompetitor(ctx, id, m.oddsFeedConfiguration.DefaultLocale())
}

// LocalizedCompetitor ...
func (m *Manager) LocalizedCompetitor(ctx context.Context, id types.URN, locale types.Locale) (types.Competitor, error) {
	return m.MultiLocalizedCompetitor(ctx, id, []types.Locale{locale})
}

// MultiLocalizedCompetitor preloads the competitor's Names map for
// every supplied locale.
func (m *Manager) MultiLocalizedCompetitor(ctx context.Context, id types.URN, locales []types.Locale) (types.Competitor, error) {
	if len(locales) == 0 {
		locales = []types.Locale{m.oddsFeedConfiguration.DefaultLocale()}
	}
	c, err := m.entityFactory.BuildCompetitor(ctx, id, locales)
	if err != nil {
		return types.Competitor{}, err
	}
	return *c, nil
}

// FixtureChanges ...
func (m *Manager) FixtureChanges(ctx context.Context, after time.Time) ([]types.FixtureChange, error) {
	return m.LocalizedFixtureChanges(ctx, m.oddsFeedConfiguration.DefaultLocale(), after)
}

// LocalizedFixtureChanges ...
func (m *Manager) LocalizedFixtureChanges(ctx context.Context, locale types.Locale, after time.Time) ([]types.FixtureChange, error) {
	return m.MultiLocalizedFixtureChanges(ctx, []types.Locale{locale}, after)
}

// MultiLocalizedFixtureChanges fetches the fixture-change index. The
// returned FixtureChange entries don't carry per-locale text — they're
// ID + timestamp — so the locale slice is honored at the API layer
// only (the first locale is used for the request) and stored for any
// future per-fixture detail call the caller might make. Multi-locale
// kept for symmetry with the rest of the manager surface.
func (m *Manager) MultiLocalizedFixtureChanges(ctx context.Context, locales []types.Locale, after time.Time) ([]types.FixtureChange, error) {
	if len(locales) == 0 {
		locales = []types.Locale{m.oddsFeedConfiguration.DefaultLocale()}
	}
	data, err := m.apiClient.FetchFixtureChanges(ctx, locales[0], after)
	if err != nil {
		return nil, err
	}

	result := make([]types.FixtureChange, len(data))
	for i := range data {
		fixtureChange := data[i]
		id, err := types.ParseURN(fixtureChange.SportEventID)
		if err != nil {
			return nil, err
		}

		result[i] = &fixtureChangeImpl{
			id:          *id,
			updatedTime: (time.Time)(fixtureChange.UpdatedAt),
		}
	}

	return result, nil
}

// ListOfMatches ...
func (m *Manager) ListOfMatches(ctx context.Context, startIndex int, limit int) ([]types.Match, error) {
	return m.LocalizedListOfMatches(ctx, startIndex, limit, m.oddsFeedConfiguration.DefaultLocale())
}

// LocalizedListOfMatches ...
func (m *Manager) LocalizedListOfMatches(ctx context.Context, startIndex int, limit int, locale types.Locale) ([]types.Match, error) {
	return m.MultiLocalizedListOfMatches(ctx, startIndex, limit, []types.Locale{locale})
}

// MultiLocalizedListOfMatches preloads every supplied locale into each
// returned match.
func (m *Manager) MultiLocalizedListOfMatches(ctx context.Context, startIndex int, limit int, locales []types.Locale) ([]types.Match, error) {
	switch {
	case limit > 1000:
		return nil, fmt.Errorf("max limit is 1000")
	case limit < 1:
		return nil, fmt.Errorf("min limit is 1")
	case startIndex < 0:
		// Fail locally with a deterministic error rather than sending
		// ?start=-1 upstream (startIndex went from uint to int in v2).
		return nil, fmt.Errorf("start index must be >= 0, got %d", startIndex)
	}
	if len(locales) == 0 {
		locales = []types.Locale{m.oddsFeedConfiguration.DefaultLocale()}
	}

	data, err := m.apiClient.FetchSchedule(ctx, startIndex, limit, locales[0])
	if err != nil {
		return nil, err
	}

	result := make([]types.Match, 0, len(data))
	for i := range data {
		id, err := types.ParseURN(data[i].ID)
		if err != nil {
			return nil, err
		}
		match, err := m.entityFactory.BuildMatch(ctx, *id, locales, nil)
		if err != nil {
			return nil, err
		}
		result = append(result, *match)
	}

	return result, nil
}

// AvailableTournaments ...
func (m *Manager) AvailableTournaments(ctx context.Context, sportID types.URN) ([]types.Tournament, error) {
	return m.LocalizedAvailableTournaments(ctx, sportID, m.oddsFeedConfiguration.DefaultLocale())
}

// LocalizedAvailableTournaments ...
func (m *Manager) LocalizedAvailableTournaments(ctx context.Context, sportID types.URN, locale types.Locale) ([]types.Tournament, error) {
	return m.MultiLocalizedAvailableTournaments(ctx, sportID, []types.Locale{locale})
}

// MultiLocalizedAvailableTournaments preloads every supplied locale
// into each returned tournament.
func (m *Manager) MultiLocalizedAvailableTournaments(ctx context.Context, sportID types.URN, locales []types.Locale) ([]types.Tournament, error) {
	if len(locales) == 0 {
		locales = []types.Locale{m.oddsFeedConfiguration.DefaultLocale()}
	}
	data, err := m.apiClient.FetchTournaments(ctx, sportID, locales[0])
	if err != nil {
		return nil, err
	}

	result := make([]types.Tournament, 0, len(data))
	for i := range data {
		id, err := types.ParseURN(data[i].ID)
		if err != nil {
			return nil, err
		}
		t, err := m.entityFactory.BuildTournament(ctx, *id, sportID, locales)
		if err != nil {
			return nil, err
		}
		result = append(result, *t)
	}

	return result, nil
}

// Tournament returns a single tournament by URN in the default locale.
func (m *Manager) Tournament(ctx context.Context, id types.URN) (types.Tournament, error) {
	return m.LocalizedTournament(ctx, id, m.oddsFeedConfiguration.DefaultLocale())
}

// LocalizedTournament returns a single tournament in the given locale.
func (m *Manager) LocalizedTournament(ctx context.Context, id types.URN, locale types.Locale) (types.Tournament, error) {
	return m.MultiLocalizedTournament(ctx, id, []types.Locale{locale})
}

// MultiLocalizedTournament returns a single tournament with every
// supplied locale preloaded. The sport that owns the tournament is
// inferred from the cache (the tournament-info API endpoint includes
// the sport URN), so callers only need the tournament URN — matching
// the documented migration path "client.Tournament(ctx, urn) for
// each urn in sport.TournamentIDs".
func (m *Manager) MultiLocalizedTournament(ctx context.Context, id types.URN, locales []types.Locale) (types.Tournament, error) {
	if len(locales) == 0 {
		locales = []types.Locale{m.oddsFeedConfiguration.DefaultLocale()}
	}
	sportID, err := m.cacheManager.TournamentCache.SportIDFor(ctx, id, locales)
	if err != nil {
		return types.Tournament{}, err
	}
	t, err := m.entityFactory.BuildTournament(ctx, id, sportID, locales)
	if err != nil {
		return types.Tournament{}, err
	}
	return *t, nil
}

// Sport returns the catalog Sport for the given URN in the configured
// default locale. For a specific locale or multiple locales use
// LocalizedSport.
func (m *Manager) Sport(ctx context.Context, id types.URN) (types.Sport, error) {
	return m.LocalizedSport(ctx, id, m.oddsFeedConfiguration.DefaultLocale())
}

// LocalizedSport returns the Sport for id in the requested locale.
func (m *Manager) LocalizedSport(ctx context.Context, id types.URN, locale types.Locale) (types.Sport, error) {
	return m.MultiLocalizedSport(ctx, id, []types.Locale{locale})
}

// MultiLocalizedSport returns a Sport whose Names/Abbreviations maps
// are populated for every supplied locale. Pre-fix, the public-facing
// Client.Sport(...locales) preloaded each locale into the cache but
// then returned a single-locale-built Sport — callers passing multiple
// locales received Names with only the primary locale set.
// MultiLocalizedSport plumbs the full locale slice through to the
// cache so the returned struct mirrors Match/Tournament/Competitor.
func (m *Manager) MultiLocalizedSport(ctx context.Context, id types.URN, locales []types.Locale) (types.Sport, error) {
	if len(locales) == 0 {
		locales = []types.Locale{m.oddsFeedConfiguration.DefaultLocale()}
	}
	s, err := m.entityFactory.BuildSport(ctx, id, locales)
	if err != nil {
		return types.Sport{}, err
	}
	return *s, nil
}

// MultiLocalizedSports returns the sports catalog with every supplied
// locale preloaded into each Sport's Names/Abbreviations map.
func (m *Manager) MultiLocalizedSports(ctx context.Context, locales []types.Locale) ([]types.Sport, error) {
	if len(locales) == 0 {
		locales = []types.Locale{m.oddsFeedConfiguration.DefaultLocale()}
	}
	return m.entityFactory.BuildSports(ctx, locales)
}

// Player returns the cached Player snapshot for id in the configured
// default locale.
func (m *Manager) Player(ctx context.Context, id types.URN) (types.Player, error) {
	return m.LocalizedPlayer(ctx, id, m.oddsFeedConfiguration.DefaultLocale())
}

// LocalizedPlayer returns the Player snapshot in the requested locale.
func (m *Manager) LocalizedPlayer(ctx context.Context, id types.URN, locale types.Locale) (types.Player, error) {
	p, err := m.entityFactory.BuildPlayer(ctx, id, locale)
	if err != nil {
		return types.Player{}, err
	}
	return *p, nil
}

// MultiLocalizedPlayer preloads the cache for every supplied locale
// and returns the Player snapshot in the primary locale. Note: unlike
// Sport/Match/Tournament/Competitor — which carry per-locale Names
// maps — types.Player is currently single-locale-per-entry (Locale
// field, not Names map). Multi-locale callers therefore get a primary-
// locale snapshot; calling LocalizedPlayer with another locale serves
// from the now-warm cache without an extra round-trip. A future
// types.Player reshape can populate the result with merged Names too.
func (m *Manager) MultiLocalizedPlayer(ctx context.Context, id types.URN, locales []types.Locale) (types.Player, error) {
	if len(locales) == 0 {
		locales = []types.Locale{m.oddsFeedConfiguration.DefaultLocale()}
	}
	primary := locales[0]
	for _, l := range locales[1:] {
		if _, err := m.entityFactory.BuildPlayer(ctx, id, l); err != nil {
			return types.Player{}, err
		}
	}
	p, err := m.entityFactory.BuildPlayer(ctx, id, primary)
	if err != nil {
		return types.Player{}, err
	}
	return *p, nil
}

// ClearMatch evicts every cache entry for a match URN: the match
// summary, its fixture, and its match status. Matches Java
// SportsInfoManager.clearMatch and .NET ISportDataProvider.
// DeleteMatchFromCache, both of which invalidate all three caches —
// pre-v2.24 only the match summary was cleared, leaving fixture and
// status entries stale.
//
// The three clears are sequential, not atomic — a cross-cache
// transaction would couple three independent tombstone locks (each
// also taken by admission/OnAdmit/StoreSide paths) for no practical
// gain. Consequence, documented on the public interface: a Match
// built concurrently with this call can pair a freshly reloaded
// summary with the previous fixture or status snapshot. Each cache's
// own clear-vs-in-flight-load tombstone still guarantees that once
// this returns, no pre-clear load re-admits stale data anywhere.
func (m *Manager) ClearMatch(id types.URN) {
	m.cacheManager.MatchCache.ClearCacheItem(id)
	m.cacheManager.FixtureCache.ClearCacheItem(id)
	m.cacheManager.MatchStatusCache.ClearCacheItem(id)
}

// ClearFixture evicts a fixture entry. Companion to ClearMatch when
// only the fixture portion needs invalidation.
func (m *Manager) ClearFixture(id types.URN) {
	m.cacheManager.FixtureCache.ClearCacheItem(id)
}

// ClearMatchStatus evicts a match-status entry. Companion to
// ClearMatch when only the live status snapshot needs invalidation.
func (m *Manager) ClearMatchStatus(id types.URN) {
	m.cacheManager.MatchStatusCache.ClearCacheItem(id)
}

// ClearTournament ...
func (m *Manager) ClearTournament(id types.URN) {
	m.cacheManager.TournamentCache.ClearCacheItem(id)
}

// ClearCompetitor ...
func (m *Manager) ClearCompetitor(id types.URN) {
	m.cacheManager.CompetitorCache.ClearCacheItem(id)
}

// ClearPlayer evicts every cached locale entry for a player ID.
func (m *Manager) ClearPlayer(id types.URN) {
	m.cacheManager.PlayersCache.ClearByID(id.ToString())
}

// ClearSport evicts a sport entry from the catalog cache.
func (m *Manager) ClearSport(id types.URN) {
	m.cacheManager.SportDataCache.Clear(id)
}

// NewManager ...
func NewManager(entityFactory *factory.EntityFactory, apiClient *api.Client, cacheManager *cache.Manager, oddsFeedConfiguration config.Config) *Manager {
	return &Manager{
		entityFactory:         entityFactory,
		apiClient:             apiClient,
		cacheManager:          cacheManager,
		oddsFeedConfiguration: oddsFeedConfiguration,
	}
}
