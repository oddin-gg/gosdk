package cache

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/api/xml"
	"github.com/oddin-gg/gosdk/protocols"
	log "github.com/oddin-gg/gosdk/internal/log"
)

// SportCache caches the (small, bounded) sport list and per-sport tournament
// IDs that get filled in lazily as tournaments are looked up.
//
// Phase 3 rewrite: replaces patrickmn/go-cache + global mutex with a
// sync.RWMutex-protected map. No LRU; the sport list is small (≲50). Locale
// tracking is per-cache; once a locale is loaded the data covers every
// known sport for that locale.
type SportCache struct {
	apiClient *api.Client
	logger    *log.Logger

	mu            sync.RWMutex
	loadedLocales map[protocols.Locale]struct{}
	sports        map[protocols.URN]*LocalizedSport
}

// LocalizedSport holds per-sport data; mu guards every field.
type LocalizedSport struct {
	mu sync.RWMutex

	id            protocols.URN
	tournamentIDs map[protocols.URN]struct{}
	name          map[protocols.Locale]string
	abbreviation  map[protocols.Locale]string
	iconPath      *string
}

func (l *LocalizedSport) makeTournamentIDsList() []protocols.URN {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]protocols.URN, 0, len(l.tournamentIDs))
	for k := range l.tournamentIDs {
		out = append(out, k)
	}
	return out
}

// Sport returns a sport entry, loading missing locales as needed.
func (s *SportCache) Sport(ctx context.Context, id protocols.URN, locales []protocols.Locale) (*LocalizedSport, error) {
	if err := s.ensureLocalesLoaded(ctx, locales); err != nil {
		return nil, err
	}

	s.mu.RLock()
	entry, ok := s.sports[id]
	s.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("sport %s not found", id.ToString())
	}
	return entry, nil
}

// Sports returns the URN list, loading missing locales as needed.
func (s *SportCache) Sports(ctx context.Context, locales []protocols.Locale) ([]protocols.URN, error) {
	if err := s.ensureLocalesLoaded(ctx, locales); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]protocols.URN, 0, len(s.sports))
	for id := range s.sports {
		out = append(out, id)
	}
	return out, nil
}

// SportTournaments returns the tournament URN list for the sport, fetching
// from the API and merging into the cached entry.
func (s *SportCache) SportTournaments(ctx context.Context, sportID protocols.URN, locale protocols.Locale) ([]protocols.URN, error) {
	s.mu.RLock()
	entry, ok := s.sports[sportID]
	s.mu.RUnlock()

	if ok {
		ids := entry.makeTournamentIDsList()
		if len(ids) > 0 {
			return ids, nil
		}
	}

	tournaments, err := s.apiClient.FetchTournaments(ctx, sportID, locale)
	if err != nil {
		return nil, err
	}

	tournamentIDs := make([]protocols.URN, 0, len(tournaments))
	for i := range tournaments {
		id, err := protocols.ParseURN(tournaments[i].ID)
		if err != nil {
			return nil, err
		}
		tournamentIDs = append(tournamentIDs, *id)
		if err := s.recordTournament(sportID, *id); err != nil {
			return nil, err
		}
	}
	return tournamentIDs, nil
}

// ensureLocalesLoaded fetches the sport list for any locale not already loaded.
// Locale-load failure does NOT poison the cache (the locale stays unmarked).
func (s *SportCache) ensureLocalesLoaded(ctx context.Context, locales []protocols.Locale) error {
	missing := s.findMissingLocales(locales)
	if len(missing) == 0 {
		return nil
	}
	for _, locale := range missing {
		data, err := s.apiClient.FetchSports(ctx, locale)
		if err != nil {
			return err
		}
		for k := range data {
			sport := data[k]
			id, err := protocols.ParseURN(sport.ID)
			if err != nil {
				return err
			}
			if err := s.upsertSport(*id, locale, &sport); err != nil {
				return err
			}
		}
		s.markLocaleLoaded(locale)
	}
	return nil
}

func (s *SportCache) findMissingLocales(locales []protocols.Locale) []protocols.Locale {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var missing []protocols.Locale
	for _, l := range locales {
		if _, ok := s.loadedLocales[l]; !ok {
			missing = append(missing, l)
		}
	}
	return missing
}

func (s *SportCache) markLocaleLoaded(locale protocols.Locale) {
	s.mu.Lock()
	s.loadedLocales[locale] = struct{}{}
	s.mu.Unlock()
}

func (s *SportCache) upsertSport(id protocols.URN, locale protocols.Locale, sport *xml.Sport) error {
	s.mu.Lock()
	entry, ok := s.sports[id]
	if !ok {
		entry = &LocalizedSport{
			id:            id,
			tournamentIDs: make(map[protocols.URN]struct{}),
			name:          make(map[protocols.Locale]string),
			abbreviation:  make(map[protocols.Locale]string),
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

func (s *SportCache) recordTournament(sportID protocols.URN, tournamentID protocols.URN) error {
	s.mu.Lock()
	entry, ok := s.sports[sportID]
	if !ok {
		entry = &LocalizedSport{
			id:            sportID,
			tournamentIDs: make(map[protocols.URN]struct{}),
			name:          make(map[protocols.Locale]string),
			abbreviation:  make(map[protocols.Locale]string),
		}
		s.sports[sportID] = entry
	}
	s.mu.Unlock()

	entry.mu.Lock()
	entry.tournamentIDs[tournamentID] = struct{}{}
	entry.mu.Unlock()
	return nil
}

// Clear evicts a single sport.
func (s *SportCache) Clear(id protocols.URN) {
	s.mu.Lock()
	delete(s.sports, id)
	s.mu.Unlock()
}

// Purge clears the entire cache.
func (s *SportCache) Purge() {
	s.mu.Lock()
	s.sports = make(map[protocols.URN]*LocalizedSport)
	s.loadedLocales = make(map[protocols.Locale]struct{})
	s.mu.Unlock()
}

func newSportDataCache(client *api.Client, logger *log.Logger) *SportCache {
	return &SportCache{
		apiClient:     client,
		logger:        logger,
		loadedLocales: make(map[protocols.Locale]struct{}),
		sports:        make(map[protocols.URN]*LocalizedSport),
	}
}

// summarySnapshot projects the cached entry into a protocols.SportSummary
// value (data-copy under the entry's read lock).
func (l *LocalizedSport) summarySnapshot() protocols.SportSummary {
	l.mu.RLock()
	defer l.mu.RUnlock()
	names := make(map[protocols.Locale]string, len(l.name))
	for k, v := range l.name {
		names[k] = v
	}
	abbr := make(map[protocols.Locale]string, len(l.abbreviation))
	for k, v := range l.abbreviation {
		abbr[k] = v
	}
	var iconPath *string
	if l.iconPath != nil {
		v := *l.iconPath
		iconPath = &v
	}
	return protocols.SportSummary{
		ID:            l.id,
		Names:         names,
		Abbreviations: abbr,
		IconPath:      iconPath,
	}
}

// BuildSport resolves a Sport snapshot from the cache, fetching missing
// locales and tournament IDs as needed.
func BuildSport(ctx context.Context, sc *SportCache, id protocols.URN, locales []protocols.Locale) (*protocols.Sport, error) {
	item, err := sc.Sport(ctx, id, locales)
	if err != nil {
		return nil, err
	}
	tournamentIDs := item.makeTournamentIDsList()
	if len(tournamentIDs) == 0 && len(locales) > 0 {
		tournamentIDs, err = sc.SportTournaments(ctx, id, locales[0])
		if err != nil {
			return nil, err
		}
	}
	return &protocols.Sport{
		SportSummary:  item.summarySnapshot(),
		TournamentIDs: tournamentIDs,
	}, nil
}

// Compile-time check.
var _ = errors.New
