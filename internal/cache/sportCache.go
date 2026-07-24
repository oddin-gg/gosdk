package cache

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/api/xml"
	"github.com/oddin-gg/gosdk/internal/cache/lru"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

// SportCache caches the (small, bounded) sport list and per-sport tournament
// IDs that get filled in lazily as tournaments are looked up.
//
// Phase 3 rewrite: replaces patrickmn/go-cache + global mutex with a
// sync.RWMutex-protected map. No LRU; the sport list is small (≲50). Locale
// tracking is per-cache; once a locale is loaded the data covers every
// known sport for that locale.
//
// Concurrent-load safety (v2.24): per-locale singleflight so N
// concurrent callers for the same locale share one FetchSports
// round-trip instead of fanning out into N parallel API calls and N
// parallel upsertSport runs.
type SportCache struct {
	apiClient *api.Client
	logger    *log.Logger
	// lifetime is the detach root for singleflight catalog loads —
	// cancelled by Manager.Close so no fetch outlives the owner.
	lifetime context.Context

	mu            sync.RWMutex
	loadedLocales map[types.Locale]struct{}
	sports        map[types.URN]*LocalizedSport

	// Clear-vs-in-flight-load protection, mirroring the market cache:
	//   - clearedAt (per sport) + purgedAt suppress stores from loads
	//     that began before the invalidation — per key, so clearing
	//     sport A never suppresses the rest of an in-flight catalog
	//     response (which would yield an empty Sports() with no error);
	//   - lastClearAt (max of any clear/purge) gates ONLY the
	//     loadedLocales mark, forcing the bulk refetch that restores
	//     the cleared sport;
	//   - flightGen is folded into the singleflight key so a caller
	//     arriving AFTER a clear never joins a pre-clear flight.
	clearedAt   map[types.URN]time.Time
	lastClearAt time.Time
	purgedAt    time.Time
	flightGen   atomic.Uint64

	sf singleflight.Group
}

// sportTombstonePruneLen / MaxAge bound the tombstone map (see the
// matching constants on the other caches).
const (
	sportTombstonePruneLen = 1024
	sportTombstoneMaxAge   = 2 * time.Minute
)

// storeSuppressed reports whether a store for id from a load that began
// at loadStarted must be skipped (a Clear for id, or a Purge, landed
// after the load started). Caller must hold s.mu (read or write).
func (s *SportCache) storeSuppressedLocked(id types.URN, loadStarted time.Time) bool {
	return s.clearedAt[id].After(loadStarted) || s.purgedAt.After(loadStarted)
}

// LocalizedSport holds per-sport data; mu guards every field.
//
// tournamentsLoaded distinguishes "this sport genuinely has zero
// tournaments" (true, tournamentIDs empty) from "tournaments not yet
// fetched" (false). Pre-fix the cache only had `tournamentIDs` and
// SportTournaments / BuildSport keyed off `len(...) > 0`, so a sport
// with a legitimate empty result re-fetched on every call. Mirrors
// the LocalizedTournament.competitorsLoaded pattern (see
// internal/cache/tournament.go).
type LocalizedSport struct {
	mu sync.RWMutex

	id                types.URN
	tournamentIDs     map[types.URN]struct{}
	tournamentsLoaded bool
	name              map[types.Locale]string
	abbreviation      map[types.Locale]string
	iconPath          *string
}

func (l *LocalizedSport) makeTournamentIDsList() []types.URN {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]types.URN, 0, len(l.tournamentIDs))
	for k := range l.tournamentIDs {
		out = append(out, k)
	}
	sortURNs(out) // deterministic public ordering (see sortURNs)
	return out
}

func (l *LocalizedSport) tournamentsAreLoaded() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.tournamentsLoaded
}

// missingLocales reports which requested locales this sport does NOT
// cover. The sport-list catalog is per-locale global: two asymmetric
// locales both mark loaded, but a sport present in only one keeps a gap
// here — the caller turns that into ErrSportLocaleIncomplete (by-id) or
// filters the sport from bulk views, never a partially-localized result.
func (l *LocalizedSport) missingLocales(locales []types.Locale) []types.Locale {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var missing []types.Locale
	for _, loc := range locales {
		if _, ok := l.name[loc]; !ok {
			missing = append(missing, loc)
		}
	}
	return missing
}

func (l *LocalizedSport) markTournamentsLoaded() {
	l.mu.Lock()
	l.tournamentsLoaded = true
	l.mu.Unlock()
}

// Sport returns a sport entry, loading missing locales as needed.
//
// Returns an error wrapping ErrItemNotFoundInCache when the catalog
// load succeeds but the requested id isn't present (lets consumers
// errors.Is to distinguish "API said no such sport" from
// "catalog-load failed").
func (s *SportCache) Sport(ctx context.Context, id types.URN, locales []types.Locale) (*LocalizedSport, error) {
	if err := s.ensureLocalesLoaded(ctx, locales); err != nil {
		return nil, err
	}

	s.mu.RLock()
	entry, ok := s.sports[id]
	s.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("sport %s not found in catalog (locales=%v): %w", id.ToString(), locales, ErrItemNotFoundInCache)
	}
	// Per-sport locale coverage: loadedLocales is global to the catalog,
	// so a sport present in only some of the requested locales is served
	// with a name/abbreviation gap the exported Sport promises to fill.
	// Surface it as a typed, non-retryable error instead of a silently
	// partial object.
	if still := entry.missingLocales(locales); len(still) > 0 {
		return nil, fmt.Errorf("sport %s: locales %v unavailable in upstream catalog: %w", id.ToString(), still, ErrSportLocaleIncomplete)
	}
	return entry, nil
}

// Sports returns the URN list, loading missing locales as needed. Only
// sports that carry EVERY requested locale are returned — a sport present
// in a subset of the requested locales would otherwise appear with a
// name/abbreviation gap the exported Sport promises to fill (the
// loadedLocales set is global to the catalog, not per sport). Mirrors
// MultiLocalizedMarketDescriptions' all-locales intersection contract;
// fetch a filtered-out sport by id to get the typed
// ErrSportLocaleIncomplete diagnosing the gap.
func (s *SportCache) Sports(ctx context.Context, locales []types.Locale) ([]types.URN, error) {
	if err := s.ensureLocalesLoaded(ctx, locales); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]types.URN, 0, len(s.sports))
	for id, entry := range s.sports {
		if len(entry.missingLocales(locales)) > 0 {
			continue
		}
		out = append(out, id)
	}
	sortURNs(out) // deterministic public ordering (see sortURNs)
	return out, nil
}

// SportTournaments returns the tournament URN list for the sport, fetching
// from the API and merging into the cached entry.
//
// Empty-result handling: a sport that genuinely has zero tournaments is
// served from cache after the first successful fetch (tournamentsLoaded=
// true on the entry). Pre-fix every call re-fetched because the empty
// list was indistinguishable from "not yet loaded".
func (s *SportCache) SportTournaments(ctx context.Context, sportID types.URN, locale types.Locale) ([]types.URN, error) {
	s.mu.RLock()
	entry, ok := s.sports[sportID]
	s.mu.RUnlock()

	if ok && entry.tournamentsAreLoaded() {
		return entry.makeTournamentIDsList(), nil
	}

	loadStarted := time.Now()
	tournaments, err := s.apiClient.FetchTournaments(ctx, sportID, locale)
	if err != nil {
		return nil, fmt.Errorf("fetch tournaments for sport %s/%s: %w", sportID.ToString(), locale, err)
	}

	// Stage-then-commit: parse and validate the COMPLETE response
	// before touching cache state. Pre-fix each tournament was recorded
	// as it parsed, so a malformed id mid-response aborted AFTER
	// committing the earlier rows — a retry then merged the
	// authoritative list ON TOP of the partial commit, and rows the
	// failed response contained but the new one doesn't lingered in
	// SportTournaments/Sports views until explicit invalidation.
	tournamentIDs := make([]types.URN, 0, len(tournaments))
	for i := range tournaments {
		id, err := types.ParseURN(tournaments[i].ID)
		if err != nil {
			return nil, fmt.Errorf("sport %s tournaments: parse tournament id %q: %w", sportID.ToString(), tournaments[i].ID, err)
		}
		tournamentIDs = append(tournamentIDs, *id)
	}
	for _, id := range tournamentIDs {
		if err := s.recordTournament(sportID, id, loadStarted); err != nil {
			return nil, fmt.Errorf("sport %s record tournament %s: %w", sportID.ToString(), id.ToString(), err)
		}
	}
	// Mark tournamentsLoaded even when the list was empty — parity
	// with LocalizedTournament.competitorsLoaded; prevents repeated
	// API calls for sports that genuinely have zero tournaments.
	// Use ensureSportEntry so the marker survives even on first
	// access for a sport never previously loaded via Sports() — the
	// non-empty path already creates the entry inside recordTournament,
	// but the empty path bypasses that loop. Suppressed when an
	// invalidation raced this fetch: recreating the cleared sport as a
	// stub would resurrect it into Sports() views. The caller still
	// gets the freshly fetched list.
	if entry := s.ensureSportEntry(sportID, loadStarted); entry != nil {
		entry.markTournamentsLoaded()
	}
	return tournamentIDs, nil
}

// ensureSportEntry returns the existing entry for sportID or creates a
// minimal stub (no name/abbreviation — those land on the next Sports()
// catalog refresh). Used by SportTournaments to attach
// tournamentsLoaded=true even when the upstream result was empty and
// recordTournament's create-on-miss path didn't fire.
// Returns nil when the store was suppressed by a racing invalidation.
func (s *SportCache) ensureSportEntry(sportID types.URN, loadStarted time.Time) *LocalizedSport {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.storeSuppressedLocked(sportID, loadStarted) {
		return nil
	}
	entry, ok := s.sports[sportID]
	if !ok {
		entry = &LocalizedSport{
			id:            sportID,
			tournamentIDs: make(map[types.URN]struct{}),
			name:          make(map[types.Locale]string),
			abbreviation:  make(map[types.Locale]string),
		}
		s.sports[sportID] = entry
	}
	return entry
}

// ensureLocalesLoaded fetches the sport list for any locale not already loaded.
// Locale-load failure does NOT poison the cache (the locale stays unmarked).
//
// Concurrent callers for the same locale are coalesced through
// lru.LoadCoalesced (DoChan + WithoutCancel): only one FetchSports
// round-trip per locale runs at a time; each caller's ctx independently
// bounds its wait, and a short-deadline first caller can't cancel the
// load for later waiters. The loader re-checks loadedLocales inside the
// critical region so a coalesced caller doesn't re-run a fetch a peer
// just completed.
func (s *SportCache) ensureLocalesLoaded(ctx context.Context, locales []types.Locale) error {
	missing := s.findMissingLocales(locales)
	if len(missing) == 0 {
		return nil
	}
	for _, locale := range missing {
		// Re-check ctx between locales so a multi-locale call cancelled
		// mid-iteration doesn't keep starting detached fetches.
		if err := ctx.Err(); err != nil {
			return err
		}
		loc := locale
		for {
			gen := s.flightGen.Load()
			flightKey := fmt.Sprintf("%d|%s", gen, loc)
			_, err := lru.LoadCoalesced(ctx, s.lifetime, &s.sf, flightKey, func(loadCtx context.Context) (struct{}, error) {
				// Stale-generation guard — see errStaleFlight for the
				// register-vs-clear window this closes.
				if s.flightGen.Load() != gen {
					return struct{}{}, errStaleFlight
				}
				// Snapshot the flight's start BEFORE fetching: stores and
				// the locale mark below are suppressed if an invalidation
				// lands after this point.
				loadStarted := time.Now()
				// Double-check: peer goroutine may already have loaded.
				s.mu.RLock()
				_, alreadyLoaded := s.loadedLocales[loc]
				s.mu.RUnlock()
				if alreadyLoaded {
					return struct{}{}, nil
				}

				data, err := s.apiClient.FetchSports(loadCtx, loc)
				if err != nil {
					return struct{}{}, fmt.Errorf("load sport catalog locale %s: %w", loc, err)
				}
				// Stage-then-commit: validate the COMPLETE response
				// before touching cache state — pre-fix each sport was
				// upserted as it parsed, so a malformed id mid-response
				// aborted after committing earlier rows; the retry's
				// authoritative catalog then merged on top and phantom
				// rows lingered until explicit invalidation.
				ids := make([]types.URN, 0, len(data))
				for k := range data {
					id, err := types.ParseURN(data[k].ID)
					if err != nil {
						return struct{}{}, fmt.Errorf("load sport catalog locale %s: parse sport id %q: %w", loc, data[k].ID, err)
					}
					ids = append(ids, *id)
				}
				for k := range data {
					if err := s.upsertSport(ids[k], loc, &data[k], loadStarted); err != nil {
						return struct{}{}, fmt.Errorf("load sport catalog locale %s: upsert %s: %w", loc, ids[k].ToString(), err)
					}
				}
				s.markLocaleLoaded(loc, loadStarted)
				return struct{}{}, nil
			})
			if errors.Is(err, errStaleFlight) {
				continue // re-register under the fresh generation
			}
			if err != nil {
				return err
			}
			break
		}
	}
	return nil
}

func (s *SportCache) findMissingLocales(locales []types.Locale) []types.Locale {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var missing []types.Locale
	for _, l := range locales {
		if _, ok := s.loadedLocales[l]; !ok {
			missing = append(missing, l)
		}
	}
	return missing
}

// markLocaleLoaded records the locale as loaded unless any
// invalidation landed after the load began — the cleared sport's row
// was suppressed, so the locale must stay unmarked to force the bulk
// refetch that restores it.
func (s *SportCache) markLocaleLoaded(locale types.Locale, loadStarted time.Time) {
	s.mu.Lock()
	if !s.lastClearAt.After(loadStarted) {
		s.loadedLocales[locale] = struct{}{}
	}
	s.mu.Unlock()
}

func (s *SportCache) upsertSport(id types.URN, locale types.Locale, sport *xml.Sport, loadStarted time.Time) error {
	s.mu.Lock()
	if s.storeSuppressedLocked(id, loadStarted) {
		// A Clear for this sport (or a Purge) landed after the load
		// started: skip only this row — the next read refetches.
		s.mu.Unlock()
		return nil
	}
	entry, ok := s.sports[id]
	if !ok {
		entry = &LocalizedSport{
			id:            id,
			tournamentIDs: make(map[types.URN]struct{}),
			name:          make(map[types.Locale]string),
			abbreviation:  make(map[types.Locale]string),
		}
		s.sports[id] = entry
	}
	s.mu.Unlock()

	entry.mu.Lock()
	defer entry.mu.Unlock()
	entry.name[locale] = sport.Name
	entry.abbreviation[locale] = sport.Abbreviation
	entry.iconPath = sport.IconPath
	return nil
}

func (s *SportCache) recordTournament(sportID types.URN, tournamentID types.URN, loadStarted time.Time) error {
	s.mu.Lock()
	if s.storeSuppressedLocked(sportID, loadStarted) {
		s.mu.Unlock()
		return nil
	}
	entry, ok := s.sports[sportID]
	if !ok {
		entry = &LocalizedSport{
			id:            sportID,
			tournamentIDs: make(map[types.URN]struct{}),
			name:          make(map[types.Locale]string),
			abbreviation:  make(map[types.Locale]string),
		}
		s.sports[sportID] = entry
	}
	s.mu.Unlock()

	entry.mu.Lock()
	entry.tournamentIDs[tournamentID] = struct{}{}
	entry.mu.Unlock()
	return nil
}

// Clear evicts a single sport AND invalidates every loaded-locale
// marker. Without the locale invalidation, a subsequent Sport(ctx,id)
// would see "sport not found" and skip the refetch (because the locale
// is still marked loaded), and Sports(ctx) would return an incomplete
// catalog. Java/.NET force the next access to refetch the catalog
// after a clear; we mirror that. The next request causes one bulk
// FetchSports per locale — bounded and acceptable.
func (s *SportCache) Clear(id types.URN) {
	s.mu.Lock()
	s.flightGen.Add(1) // detach future callers from in-flight loads
	now := time.Now()
	s.clearedAt[id] = now
	s.lastClearAt = now
	if len(s.clearedAt) > sportTombstonePruneLen {
		cutoff := now.Add(-sportTombstoneMaxAge)
		for k, t := range s.clearedAt {
			if t.Before(cutoff) {
				delete(s.clearedAt, k)
			}
		}
	}
	delete(s.sports, id)
	s.loadedLocales = make(map[types.Locale]struct{})
	s.mu.Unlock()
}

// Purge clears the entire cache.
func (s *SportCache) Purge() {
	s.mu.Lock()
	s.flightGen.Add(1) // detach future callers from in-flight loads
	now := time.Now()
	s.purgedAt = now
	s.lastClearAt = now
	s.clearedAt = make(map[types.URN]time.Time)
	s.sports = make(map[types.URN]*LocalizedSport)
	s.loadedLocales = make(map[types.Locale]struct{})
	s.mu.Unlock()
}

func newSportDataCache(lifeCtx context.Context, client *api.Client, logger *log.Logger) *SportCache {
	return &SportCache{
		apiClient:     client,
		logger:        logger,
		lifetime:      lifeCtx,
		loadedLocales: make(map[types.Locale]struct{}),
		sports:        make(map[types.URN]*LocalizedSport),
		clearedAt:     make(map[types.URN]time.Time),
	}
}

// summarySnapshot projects the cached entry into a types.SportSummary
// value (data-copy under the entry's read lock).
func (l *LocalizedSport) summarySnapshot() types.SportSummary {
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
	return types.SportSummary{
		ID:            l.id,
		Names:         names,
		Abbreviations: abbr,
		// FromPtr copies the pointee — the previous local-copy
		// dance (clonePtr-style) is no longer needed.
		IconPath: types.FromPtr(l.iconPath),
	}
}

// BuildSport resolves a Sport snapshot from the cache, fetching missing
// locales and tournament IDs as needed. The first call for a sport
// triggers a tournament fetch even when the resulting list is empty;
// subsequent calls return the cached (possibly-empty) list without
// re-fetching, gated by tournamentsAreLoaded().
func BuildSport(ctx context.Context, sc *SportCache, id types.URN, locales []types.Locale) (*types.Sport, error) {
	item, err := sc.Sport(ctx, id, locales)
	if err != nil {
		return nil, err
	}
	tournamentIDs := item.makeTournamentIDsList()
	if !item.tournamentsAreLoaded() && len(locales) > 0 {
		tournamentIDs, err = sc.SportTournaments(ctx, id, locales[0])
		if err != nil {
			return nil, err
		}
	}
	return &types.Sport{
		SportSummary:  item.summarySnapshot(),
		TournamentIDs: tournamentIDs,
	}, nil
}

// Compile-time check.
var _ = errors.New
