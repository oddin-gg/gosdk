package gosdk

import (
	"context"
	"time"

	"github.com/oddin-gg/gosdk/types"
)

// This file holds the manager-shaped interfaces that describe what
// the SDK *does*. Pre-v2.25 they lived in the types/ package alongside
// the data types the SDK returns; the v2.25 reshape moved them here so
// types/ is pure data-shape definitions and gosdk owns the behavioural
// surface. The v1.0.0 pass unexported them: they are wiring seams
// between Client and the internal manager implementations — no public
// method takes or returns them, so exporting them only leaked
// gosdk.SportsInfoManager etc. into the consumer namespace against
// NEXT.md goal #3 ("flatten to a single Client"). The public surface
// is Client alone.
//
// Internal packages (internal/whoami, internal/replay,
// internal/sport, internal/market) return concrete *Manager values
// that satisfy these interfaces structurally — they don't import
// gosdk to avoid a cycle.

// whoAmIManager exposes the bookmaker-details probe.
type whoAmIManager interface {
	BookmakerDetails(ctx context.Context) (types.BookmakerDetail, error)
}

// replayManager exposes the replay-queue + playback control surface.
//
// Phase 6 reshape: ReplayList returns Match values directly (the
// previous SportEvent interface is gone — replay queues are populated
// from match URNs).
type replayManager interface {
	// ReplayList resolves every queued event to a Match (one
	// sports-info call per ID). Use ReplayEventIDs when only the URNs
	// are needed and the resolution cost is wasteful.
	ReplayList(ctx context.Context) ([]types.Match, error)
	// ReplayEventIDs returns the queued event URNs without resolving
	// them to Match values. Mirrors .NET's IReplayManager.GetEventsInQueue —
	// useful for "is this ID in the queue?" checks where building a
	// Match per entry would be unnecessary work.
	ReplayEventIDs(ctx context.Context) ([]types.URN, error)

	AddSportEventID(ctx context.Context, id types.URN) (bool, error)
	RemoveSportEventID(ctx context.Context, id types.URN) (bool, error)

	Play(ctx context.Context, params types.ReplayPlayParams) (bool, error)

	Stop(ctx context.Context) (bool, error)
	Clear(ctx context.Context) (bool, error)

	// Status reports the current replay-engine state. The returned
	// string is opaque (set by the engine, typically "playing" /
	// "stopped" / "paused").
	Status(ctx context.Context) (string, error)
}

// sportsInfoManager exposes the sports-catalog + match/competitor/
// player/tournament/fixture surface.
//
// All "fetch"-style methods take a context.Context for cancellation.
// The in-memory cache invalidation methods (Clear*) do not — they
// are pure-state operations.
type sportsInfoManager interface {
	Sports(ctx context.Context) ([]types.Sport, error)
	LocalizedSports(ctx context.Context, locale types.Locale) ([]types.Sport, error)
	// MultiLocalizedSports returns the catalog with every supplied
	// locale merged into each Sport's Names/Abbreviations map.
	MultiLocalizedSports(ctx context.Context, locales []types.Locale) ([]types.Sport, error)

	Sport(ctx context.Context, id types.URN) (types.Sport, error)
	LocalizedSport(ctx context.Context, id types.URN, locale types.Locale) (types.Sport, error)
	// MultiLocalizedSport returns a Sport with every supplied locale
	// merged into Names/Abbreviations. Pre-fix the public Sport(...)
	// preloaded the cache but returned a single-locale snapshot.
	MultiLocalizedSport(ctx context.Context, id types.URN, locales []types.Locale) (types.Sport, error)

	Player(ctx context.Context, id types.URN) (types.Player, error)
	LocalizedPlayer(ctx context.Context, id types.URN, locale types.Locale) (types.Player, error)
	// MultiLocalizedPlayer preloads the cache for every supplied
	// locale and returns the primary-locale snapshot. types.Player is
	// currently single-locale-per-entry — multi-locale callers can
	// follow up with LocalizedPlayer(other-locale) which serves from
	// the now-warm cache without an extra round-trip.
	MultiLocalizedPlayer(ctx context.Context, id types.URN, locales []types.Locale) (types.Player, error)

	ActiveTournaments(ctx context.Context) ([]types.Tournament, error)
	LocalizedActiveTournaments(ctx context.Context, locale types.Locale) ([]types.Tournament, error)
	// MultiLocalizedActiveTournaments preloads every supplied locale
	// in the cache for each returned tournament. Passing >1 locale
	// matches Java/.NET preload semantics and avoids the per-locale
	// re-fetch the single-locale variant forces.
	MultiLocalizedActiveTournaments(ctx context.Context, locales []types.Locale) ([]types.Tournament, error)

	SportActiveTournaments(ctx context.Context, sportName string) ([]types.Tournament, error)
	LocalizedSportActiveTournaments(ctx context.Context, sportName string, locale types.Locale) ([]types.Tournament, error)
	// MultiLocalizedSportActiveTournaments performs sport-name lookup
	// against every supplied locale (not just the default). Mirrors
	// Java/.NET getActiveTournaments(sportName, locale).
	MultiLocalizedSportActiveTournaments(ctx context.Context, sportName string, locales []types.Locale) ([]types.Tournament, error)

	MatchesFor(ctx context.Context, date time.Time) ([]types.Match, error)
	LocalizedMatchesFor(ctx context.Context, date time.Time, locale types.Locale) ([]types.Match, error)
	MultiLocalizedMatchesFor(ctx context.Context, date time.Time, locales []types.Locale) ([]types.Match, error)

	LiveMatches(ctx context.Context) ([]types.Match, error)
	LocalizedLiveMatches(ctx context.Context, locale types.Locale) ([]types.Match, error)
	MultiLocalizedLiveMatches(ctx context.Context, locales []types.Locale) ([]types.Match, error)

	Match(ctx context.Context, id types.URN) (types.Match, error)
	LocalizedMatch(ctx context.Context, id types.URN, locale types.Locale) (types.Match, error)
	MultiLocalizedMatch(ctx context.Context, id types.URN, locales []types.Locale) (types.Match, error)

	Competitor(ctx context.Context, id types.URN) (types.Competitor, error)
	LocalizedCompetitor(ctx context.Context, id types.URN, locale types.Locale) (types.Competitor, error)
	MultiLocalizedCompetitor(ctx context.Context, id types.URN, locales []types.Locale) (types.Competitor, error)

	FixtureChanges(ctx context.Context, after time.Time) ([]types.FixtureChange, error)
	LocalizedFixtureChanges(ctx context.Context, locale types.Locale, after time.Time) ([]types.FixtureChange, error)
	MultiLocalizedFixtureChanges(ctx context.Context, locales []types.Locale, after time.Time) ([]types.FixtureChange, error)

	ListOfMatches(ctx context.Context, startIndex int, limit int) ([]types.Match, error)
	LocalizedListOfMatches(ctx context.Context, startIndex int, limit int, locale types.Locale) ([]types.Match, error)
	MultiLocalizedListOfMatches(ctx context.Context, startIndex int, limit int, locales []types.Locale) ([]types.Match, error)

	AvailableTournaments(ctx context.Context, sportID types.URN) ([]types.Tournament, error)
	LocalizedAvailableTournaments(ctx context.Context, sportID types.URN, locale types.Locale) ([]types.Tournament, error)
	MultiLocalizedAvailableTournaments(ctx context.Context, sportID types.URN, locales []types.Locale) ([]types.Tournament, error)

	// Tournament returns a single tournament by URN. The sport that
	// owns the tournament is inferred from the cache, so callers
	// don't have to know it. Mirrors Java's
	// SportsInfoManager.getLongTermEvent(URN) and the documented
	// migration path "client.Tournament(ctx, urn) for each urn in
	// sport.TournamentIDs".
	Tournament(ctx context.Context, id types.URN) (types.Tournament, error)
	LocalizedTournament(ctx context.Context, id types.URN, locale types.Locale) (types.Tournament, error)
	MultiLocalizedTournament(ctx context.Context, id types.URN, locales []types.Locale) (types.Tournament, error)

	// ClearMatch invalidates every cache entry for a match URN: the
	// match summary, fixture, and live status. Mirrors Java
	// SportsInfoManager.clearMatch and .NET DeleteMatchFromCache.
	//
	// The three caches are cleared sequentially, not atomically: a
	// Match built concurrently with the call may combine a freshly
	// reloaded summary with the previous fixture or status snapshot
	// (each individually consistent). Once ClearMatch returns, all
	// three entries are invalidated and the next read of each
	// refetches.
	ClearMatch(id types.URN)
	// ClearFixture invalidates only the fixture cache entry.
	ClearFixture(id types.URN)
	// ClearMatchStatus invalidates only the match-status cache entry.
	ClearMatchStatus(id types.URN)
	ClearTournament(id types.URN)
	ClearCompetitor(id types.URN)
	ClearPlayer(id types.URN)
	ClearSport(id types.URN)
}

// marketDescriptionManager exposes the market-descriptions catalog
// and the void-reasons catalog.
//
// Phase 6.1 reshape: returns value-typed MarketDescription /
// MarketVoidReason directly (the previous interfaces with lazy-load
// accessors are gone).
type marketDescriptionManager interface {
	MarketDescriptions(ctx context.Context) ([]types.MarketDescription, error)
	MarketDescriptionByIDAndVariant(ctx context.Context, marketID int, variant types.Optional[string]) (*types.MarketDescription, error)
	// LocalizedMarketDescriptionByIDAndVariant returns a description
	// in the supplied locales (parity with .NET / Java); the first
	// locale is the primary, additional locales preload the cache.
	LocalizedMarketDescriptionByIDAndVariant(ctx context.Context, marketID int, variant types.Optional[string], locales ...types.Locale) (*types.MarketDescription, error)
	LocalizedMarketDescriptions(ctx context.Context, locale types.Locale) ([]types.MarketDescription, error)
	// MultiLocalizedMarketDescriptions preloads every supplied locale
	// into the description cache and returns the catalog with each
	// description's Names + outcomes populated for all of them.
	// Mirrors the multi-locale preload semantics that other manager
	// methods now expose; matches Java/.NET cache-population
	// behaviour.
	MultiLocalizedMarketDescriptions(ctx context.Context, locales []types.Locale) ([]types.MarketDescription, error)
	ClearMarketDescription(marketID int, variant types.Optional[string])
	MarketVoidReasons(ctx context.Context) ([]types.MarketVoidReason, error)
	ReloadMarketVoidReasons(ctx context.Context) ([]types.MarketVoidReason, error)
	// ClearMarketVoidReasons evicts the void-reasons catalog. The
	// next MarketVoidReasons call refetches from the API.
	ClearMarketVoidReasons()
}
