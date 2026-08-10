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
	apiXML "github.com/oddin-gg/gosdk/internal/api/xml"
	"github.com/oddin-gg/gosdk/internal/cache/lru"
	"github.com/oddin-gg/gosdk/internal/config"
	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

// MatchStatusCache stores per-event status snapshots, fed by both AMQP
// OddsChange messages (live updates) and API MatchSummary responses (initial
// load + on-demand fetch).
//
// Phase 3 rewrite: replaces patrickmn/go-cache with a sync.RWMutex map.
// Updates use copy-on-write semantics: refresh* builds a fresh
// *LocalizedMatchStatus, copies the prior entry's fields into it, mutates
// the copy, and atomic-swaps it into the map. Readers holding a pointer
// see a stable snapshot — no partial-update tears.
//
// Phase 6 reshape: cache stores value-typed PeriodScore/Scoreboard/
// Statistics fields directly. BuildMatchStatus projects the entry into a
// *types.MatchStatus value with the localized status-code description
// resolved at construction.
//
// Concurrent-miss safety (v2.24): MatchStatus uses a singleflight.Group
// keyed on URN so N concurrent goroutines for the same id share a
// single FetchMatchSummary round-trip. Hot under recovery and snapshot
// where many subscriptions ask for the same event status simultaneously.
//
// Bounded storage: entries live in an expirable LRU (same size/TTL
// defaults as the other per-event caches) instead of a plain map. The
// cache is fed by EVERY OddsChange, so an unbounded map accumulated one
// entry per match the process ever saw live — the dominant contributor
// to the "SDK cache taking too much space" consumer report. Eviction is
// safe: a status evicted for an active match is re-fetched from the
// match summary on the next MatchStatus() call and re-fed by the next
// OddsChange.
type MatchStatusCache struct {
	apiClient             *api.Client
	logger                *log.Logger
	oddsFeedConfiguration config.Config
	// lifetime is the detach root for singleflight summary loads —
	// cancelled by Manager.Close so no fetch outlives the owner.
	lifetime context.Context

	// mu serializes the copy-on-write merges in refreshOrInsert* (the
	// LRU itself is thread-safe, but snapshot+merge+store must be
	// atomic per id so concurrent feed/API refreshes can't drop
	// updates — see refreshOrInsertFeedItem). It also guards the
	// clear tombstone below.
	mu      sync.Mutex
	entries *lru.TTL[types.URN, *LocalizedMatchStatus]

	// clearedAt (per key) + purgedAt implement the clear-vs-in-flight-
	// load tombstone for the API path: every api.Response carries the
	// START time of the fetch that produced it (Response.StartedAt), so
	// the observer-driven refreshOrInsertAPIItem can skip the store
	// when a ClearCacheItem for THAT id (or a Purge) landed after the
	// fetch began — regardless of which code path initiated the fetch
	// (this cache's loader or the match cache's summary load). Per-key
	// tombstones keep unrelated ids unaffected: clearing match A never
	// suppresses an in-flight load for match B. The map is pruned once
	// entries are older than any possible in-flight fetch. The FEED
	// path (refreshOrInsertFeedItem) stays unguarded on purpose — live
	// data is newer than any clear by definition.
	clearedAt map[types.URN]time.Time
	purgedAt  time.Time

	// flightGen is folded into the singleflight key. Tombstones stop a
	// pre-clear flight from repopulating the cache, but without a
	// generation a caller arriving AFTER the clear could still JOIN
	// that flight — and receive its transient not-found instead of
	// initiating a fresh fetch. Advancing on every ClearCacheItem/Purge
	// detaches future callers from all in-flight loads.
	flightGen atomic.Uint64

	sf singleflight.Group
}

// clearTombstonePruneLen is the clearedAt size beyond which
// ClearCacheItem amortizes a prune of expired tombstones.
const clearTombstonePruneLen = 1024

// clearTombstoneMaxAge bounds how long a tombstone can matter: no
// in-flight fetch outlives the HTTP client timeout; one extra minute of
// slack covers scheduling.
const clearTombstoneMaxAge = 2 * time.Minute

// maxStatusClearRetries bounds how many times MatchStatus restarts a
// load whose store was suppressed by a mid-flight ClearCacheItem/Purge
// (mirrors lru.maxClearRetries). Only a sustained storm of clears for
// the same id, each landing within one load window, can exhaust it.
const maxStatusClearRetries = 3

// LocalizedMatchStatus is the cache entry. Fields are value-typed and
// immutable per snapshot — refreshOrInsert* builds a fresh copy and
// atomic-swaps it into the map.
type LocalizedMatchStatus struct {
	// Freshness ordering (see refreshOrInsertFeedItem /
	// refreshOrInsertAPIItem): feedMsgTS is the upstream message
	// timestamp of the newest APPLIED feed update; feedAppliedAt is the
	// local wall-clock instant it was applied (comparable with
	// api.Response.StartedAt, which is also local wall clock).
	feedMsgTS     time.Time
	feedAppliedAt time.Time

	winnerID              *types.URN
	status                types.EventStatus
	periodScores          []types.PeriodScore
	matchStatusID         *int
	homeScore             *float64
	awayScore             *float64
	isScoreboardAvailable bool
	scoreboard            *types.Scoreboard
	statistics            *types.Statistics
}

// OnFeedMessage ...
func (m *MatchStatusCache) OnFeedMessage(id types.URN, feedMessage *types.FeedMessage) {
	if feedMessage.Message == nil {
		return
	}
	message, ok := feedMessage.Message.(*feedXML.OddsChange)
	if !ok || message.SportEventStatus == nil {
		return
	}
	m.refreshOrInsertFeedItem(id, message.SportEventStatus, feedMessage.Timestamp.Created)
}

// OnAPIResponse ...
//
// Failures here are observer-callback paths: the SDK consumer never
// sees an error directly, but the cache fails to populate so the
// next MatchStatus() call returns the wrapped ErrItemNotFoundInCache
// (see MatchStatus singleflight loader). Logging WithError ensures
// the underlying cause is recoverable from logs.
func (m *MatchStatusCache) OnAPIResponse(apiResponse api.Response) {
	msg, ok := apiResponse.Data.(*apiXML.MatchSummaryResponse)
	if !ok {
		return
	}
	id, err := types.ParseURN(msg.SportEvent.ID)
	if err != nil {
		m.logger.WithError(err).Errorf("OnAPIResponse: parse match urn %q", msg.SportEvent.ID)
		return
	}
	if err := m.refreshOrInsertAPIItem(*id, msg.SportEventStatus, apiResponse.StartedAt); err != nil {
		m.logger.WithError(err).Errorf("OnAPIResponse: refresh match status %s", id.ToString())
	}
}

// ClearCacheItem ...
func (m *MatchStatusCache) ClearCacheItem(id types.URN) {
	m.mu.Lock()
	m.flightGen.Add(1) // detach future callers from in-flight loads
	now := time.Now()
	m.clearedAt[id] = now
	if len(m.clearedAt) > clearTombstonePruneLen {
		cutoff := now.Add(-clearTombstoneMaxAge)
		for k, t := range m.clearedAt {
			if t.Before(cutoff) {
				delete(m.clearedAt, k)
			}
		}
	}
	m.entries.Remove(id)
	m.mu.Unlock()
}

// Purge clears the entire cache.
func (m *MatchStatusCache) Purge() {
	m.mu.Lock()
	m.flightGen.Add(1) // detach future callers from in-flight loads
	m.purgedAt = time.Now()
	m.clearedAt = make(map[types.URN]time.Time)
	m.entries.Purge()
	m.mu.Unlock()
}

// MatchStatus returns a cached status, fetching from the API on miss.
// The fetch triggers OnAPIResponse via the api.Client observer hook,
// which populates the cache; we then re-read.
//
// Concurrent misses for the same id are coalesced through singleflight
// — only one FetchMatchSummary round-trip runs per (id) at a time. Each
// caller's ctx independently bounds its wait via lru.LoadCoalesced;
// the shared fetch runs under WithoutCancel so a short-deadline first
// caller can't cancel the load for later waiters.
func (m *MatchStatusCache) MatchStatus(ctx context.Context, id types.URN) (*LocalizedMatchStatus, error) {
	if entry, ok := m.lookup(id); ok {
		return entry, nil
	}

	for clearRetries := 0; ; {
		gen := m.flightGen.Load()
		flightKey := fmt.Sprintf("%d|%s", gen, id.ToString())
		entry, err := lru.LoadCoalesced(ctx, m.lifetime, &m.sf, flightKey, func(loadCtx context.Context) (*LocalizedMatchStatus, error) {
			// Stale-generation guard — see errStaleFlight for the
			// register-vs-clear window this closes.
			if m.flightGen.Load() != gen {
				return nil, errStaleFlight
			}
			// Re-check inside the singleflight critical region in case a
			// peer goroutine already populated the cache.
			if entry, ok := m.lookup(id); ok {
				return entry, nil
			}
			// started lower-bounds api.Response.StartedAt: any tombstone
			// recorded AFTER the fetch began (i.e. one that suppressed
			// THIS flight's store) is necessarily after it. No other
			// local bookkeeping is needed — the observer checks
			// StartedAt against the per-id tombstone itself.
			started := time.Now()
			if _, err := m.apiClient.FetchMatchSummary(loadCtx, id, m.oddsFeedConfiguration.DefaultLocale()); err != nil {
				// A definitive upstream 404 maps to the exported
				// not-found sentinel (contract parity with the match/
				// tournament/competitor/player caches); everything else
				// passes through as a retryable fetch failure.
				return nil, notFoundIfAbsent(fmt.Errorf("fetch match summary %s: %w", id.ToString(), err))
			}
			entry, ok := m.lookup(id)
			if !ok {
				// Distinguish WHY a successful fetch didn't populate.
				// A ClearCacheItem/Purge landing mid-flight suppresses
				// the observer's store (per-id tombstone) — a transient
				// invalidation race, not upstream absence. Surface it as
				// the retryable sentinel so the caller restarts from the
				// now-empty entry (mirrors lru.EventCache's
				// errClearedDuringLoad handling) instead of handing every
				// coalesced waiter the definitive ErrItemNotFoundInCache.
				m.mu.Lock()
				cleared := m.clearedAt[id].After(started) || m.purgedAt.After(started)
				m.mu.Unlock()
				if cleared {
					return nil, errClearedDuringLoad
				}
				// API succeeded but OnAPIResponse didn't populate — either
				// the response shape mismatched or the observer dropped
				// the response. Wrap ErrItemNotFoundInCache so consumers
				// can distinguish from a fetch error.
				return nil, fmt.Errorf("match status %s: cache miss after FetchMatchSummary (observer dropped response): %w", id.ToString(), ErrItemNotFoundInCache)
			}
			return entry, nil
		})
		switch {
		case errors.Is(err, errStaleFlight):
			continue // re-register under the fresh generation
		case errors.Is(err, errClearedDuringLoad):
			if clearRetries >= maxStatusClearRetries {
				// Sustained clear storm — bail with the transient error
				// (deliberately NOT the not-found sentinel) rather than
				// livelock.
				return nil, fmt.Errorf("match status %s: %w", id.ToString(), errClearedDuringLoad)
			}
			clearRetries++
			continue // retry from the now-empty entry
		}
		return entry, err
	}
}

func (m *MatchStatusCache) lookup(id types.URN) (*LocalizedMatchStatus, bool) {
	return m.entries.Get(id)
}

// shallowClone returns a fresh struct with all fields copied from src,
// or a zero-value if src is nil.
func (m *MatchStatusCache) shallowClone(src *LocalizedMatchStatus) *LocalizedMatchStatus {
	if src == nil {
		return &LocalizedMatchStatus{}
	}
	c := *src
	return &c
}

func (m *MatchStatusCache) refreshOrInsertFeedItem(id types.URN, data *feedXML.SportEventStatus, msgTS time.Time) {
	// Hold the write lock across the snapshot+merge+store. Pre-fix the
	// snapshot was taken under RLock and the store under Lock, so two
	// concurrent feed messages for the same id could each clone the
	// same prev, apply different fields, and race on the store —
	// silently dropping one update.
	m.mu.Lock()
	defer m.mu.Unlock()

	prev, _ := m.entries.Get(id)
	// Feed-vs-feed ordering: AMQP guarantees order per queue, but
	// multiple subscriptions (and delayed recovery traffic) feed this
	// one cache — an older message must not overwrite a newer applied
	// update. Enforced only when both timestamps are present; messages
	// without an upstream timestamp keep the previous last-write-wins.
	if prev != nil && !msgTS.IsZero() && !prev.feedMsgTS.IsZero() && msgTS.Before(prev.feedMsgTS) {
		return
	}
	result := m.shallowClone(prev)
	if !msgTS.IsZero() {
		result.feedMsgTS = msgTS
	}
	result.feedAppliedAt = time.Now()
	// Presence-preserving merge: only fields the payload SUPPLIED
	// overwrite prior state (the XML scalars are pointers — see the
	// feedXML.SportEventStatus note). A partial update no longer erases
	// real scores/status with decoded zero values.
	if data.Status != nil {
		result.status = m.fromFeedEventStatus(*data.Status)
	}
	// winner_id: the feed carries it on final/decided statuses only —
	// assign when present and parseable, keep the previous value when
	// absent (an in-play update without a winner must not erase one).
	// Pre-fix this merge dropped the attribute entirely, so a live
	// consumer's MatchStatus.WinnerID stayed nil (or stale) until an
	// API summary refetch happened to overwrite the cache entry.
	if data.WinnerID != nil {
		if w, err := types.ParseURN(*data.WinnerID); err == nil {
			result.winnerID = w
		} else {
			m.logger.WithError(err).Warnf("feed match status %s: unparsable winner_id %q kept previous", id.ToString(), *data.WinnerID)
		}
	}
	if data.PeriodScores != nil {
		result.periodScores = m.mapFeedPeriodScores(data.PeriodScores.List)
	}
	if data.MatchStatus != nil {
		ms := *data.MatchStatus
		result.matchStatusID = &ms
	}
	if data.HomeScore != nil {
		v := *data.HomeScore
		result.homeScore = &v
	}
	if data.AwayScore != nil {
		v := *data.AwayScore
		result.awayScore = &v
	}
	if data.ScoreboardAvailable != nil {
		result.isScoreboardAvailable = *data.ScoreboardAvailable
	}
	if data.Scoreboard != nil {
		sb := makeFeedScoreboard(data.Scoreboard)
		result.scoreboard = &sb
	}
	if data.Statistics != nil {
		s := makeFeedStatistics(data.Statistics)
		result.statistics = &s
	}

	m.entries.Add(id, result)
}

func (m *MatchStatusCache) refreshOrInsertAPIItem(id types.URN, data apiXML.SportEventStatus, fetchStarted time.Time) error {
	var winnerID *types.URN
	if data.WinnerID != nil {
		var err error
		winnerID, err = types.ParseURN(*data.WinnerID)
		if err != nil {
			return err
		}
	}

	// Hold the write lock across the snapshot+merge+store so a concurrent
	// feed-side refresh for the same id can't lose updates (see
	// refreshOrInsertFeedItem for the same reasoning).
	m.mu.Lock()
	defer m.mu.Unlock()

	// Per-id clear tombstone: skip the store when a ClearCacheItem for
	// THIS id (or a Purge) landed after the summary fetch began — the
	// data may predate the invalidation. fetchStarted comes from
	// api.Response.StartedAt, so the guard holds no matter which code
	// path (this cache, the match cache, ...) initiated the fetch.
	if m.clearedAt[id].After(fetchStarted) || m.purgedAt.After(fetchStarted) {
		return nil
	}

	prev, _ := m.entries.Get(id)
	// Freshness ordering: a summary fetch that STARTED before a feed
	// update was applied carries data at least as old as that update —
	// applying it would roll live status/scores/winner back. StartedAt
	// and feedAppliedAt are both local wall clock, so the comparison is
	// sound. (Pre-fix only explicit clears were guarded.)
	if prev != nil && !prev.feedAppliedAt.IsZero() && fetchStarted.Before(prev.feedAppliedAt) {
		return nil
	}
	result := m.shallowClone(prev)
	// Presence-preserving merge — same contract as the feed path.
	if data.Status != "" {
		result.status = m.fromAPI(data.Status)
	}
	if data.WinnerID != nil {
		// Absent winner_id keeps the previous value (an in-play summary
		// without a winner must not erase a known one).
		result.winnerID = winnerID
	}
	if data.MatchStatusCode != nil {
		ms := *data.MatchStatusCode
		result.matchStatusID = &ms
	}
	if data.HomeScore != nil {
		v := *data.HomeScore
		result.homeScore = &v
	}
	if data.AwayScore != nil {
		v := *data.AwayScore
		result.awayScore = &v
	}
	if data.PeriodScores != nil {
		result.periodScores = m.mapAPIPeriodScores(data.PeriodScores.List)
	}
	if data.ScoreboardAvailable != nil {
		result.isScoreboardAvailable = *data.ScoreboardAvailable
	}
	if data.Scoreboard != nil {
		sb := makeAPIScoreboard(data.Scoreboard)
		result.scoreboard = &sb
	}

	m.entries.Add(id, result)
	return nil
}

// --- mapping helpers ---
//
// XML decode types (apiXML/feedXML) keep the *T idiom for upstream
// compatibility; conversion to types.Optional[T] happens here at the
// XML→types boundary via types.FromPtr. FromPtr COPIES the pointee,
// so the resulting Optional values are fully decoupled from the
// XML structs that the decoder may later reuse or release.

func (m *MatchStatusCache) mapAPIPeriodScores(periodScores []*apiXML.PeriodScore) []types.PeriodScore {
	result := make([]types.PeriodScore, len(periodScores))
	for i := range periodScores {
		ps := periodScores[i]
		result[i] = types.PeriodScore{
			Type:              ps.Type,
			HomeScore:         ps.HomeScore,
			AwayScore:         ps.AwayScore,
			PeriodNumber:      ps.Number,
			MatchStatusCode:   ps.MatchStatusCode,
			HomeWonRounds:     types.FromPtr(ps.HomeWonRounds),
			AwayWonRounds:     types.FromPtr(ps.AwayWonRounds),
			HomeKills:         types.FromPtr(ps.HomeKills),
			AwayKills:         types.FromPtr(ps.AwayKills),
			HomeGoals:         types.FromPtr(ps.HomeGoals),
			AwayGoals:         types.FromPtr(ps.AwayGoals),
			HomePoints:        types.FromPtr(ps.HomePoints),
			AwayPoints:        types.FromPtr(ps.AwayPoints),
			HomeGames:         types.FromPtr(ps.HomeGames),
			AwayGames:         types.FromPtr(ps.AwayGames),
			HomeRuns:          types.FromPtr(ps.HomeRuns),
			AwayRuns:          types.FromPtr(ps.AwayRuns),
			HomeWicketsFallen: types.FromPtr(ps.HomeWicketsFallen),
			AwayWicketsFallen: types.FromPtr(ps.AwayWicketsFallen),
			HomeOversPlayed:   types.FromPtr(ps.HomeOversPlayed),
			HomeBallsPlayed:   types.FromPtr(ps.HomeBallsPlayed),
			AwayOversPlayed:   types.FromPtr(ps.AwayOversPlayed),
			AwayBallsPlayed:   types.FromPtr(ps.AwayBallsPlayed),
			HomeWonCoinToss:   types.FromPtr(ps.HomeWonCoinToss),
		}
	}
	return result
}

func (m *MatchStatusCache) mapFeedPeriodScores(periodScores []*feedXML.PeriodScore) []types.PeriodScore {
	result := make([]types.PeriodScore, len(periodScores))
	for i := range periodScores {
		ps := periodScores[i]
		result[i] = types.PeriodScore{
			Type:              ps.Type,
			HomeScore:         ps.HomeScore,
			AwayScore:         ps.AwayScore,
			PeriodNumber:      ps.Number,
			MatchStatusCode:   ps.MatchStatusCode,
			HomeWonRounds:     types.FromPtr(ps.HomeWonRounds),
			AwayWonRounds:     types.FromPtr(ps.AwayWonRounds),
			HomeKills:         types.FromPtr(ps.HomeKills),
			AwayKills:         types.FromPtr(ps.AwayKills),
			HomeGoals:         types.FromPtr(ps.HomeGoals),
			AwayGoals:         types.FromPtr(ps.AwayGoals),
			HomePoints:        types.FromPtr(ps.HomePoints),
			AwayPoints:        types.FromPtr(ps.AwayPoints),
			HomeGames:         types.FromPtr(ps.HomeGames),
			AwayGames:         types.FromPtr(ps.AwayGames),
			HomeRuns:          types.FromPtr(ps.HomeRuns),
			AwayRuns:          types.FromPtr(ps.AwayRuns),
			HomeWicketsFallen: types.FromPtr(ps.HomeWicketsFallen),
			AwayWicketsFallen: types.FromPtr(ps.AwayWicketsFallen),
			HomeOversPlayed:   types.FromPtr(ps.HomeOversPlayed),
			HomeBallsPlayed:   types.FromPtr(ps.HomeBallsPlayed),
			AwayOversPlayed:   types.FromPtr(ps.AwayOversPlayed),
			AwayBallsPlayed:   types.FromPtr(ps.AwayBallsPlayed),
			HomeWonCoinToss:   types.FromPtr(ps.HomeWonCoinToss),
		}
	}
	return result
}

func makeFeedScoreboard(s *feedXML.Scoreboard) types.Scoreboard {
	return types.Scoreboard{
		CurrentCTTeam:        types.FromPtr(s.CurrentCTTeam),
		CurrentDefenderTeam:  types.FromPtr(s.CurrentDefenderTeam),
		HomeWonRounds:        types.FromPtr(s.HomeWonRounds),
		AwayWonRounds:        types.FromPtr(s.AwayWonRounds),
		CurrentRound:         types.FromPtr(s.CurrentRound),
		HomeKills:            types.FromPtr(s.HomeKills),
		AwayKills:            types.FromPtr(s.AwayKills),
		HomeDestroyedTurrets: types.FromPtr(s.HomeDestroyedTurrets),
		AwayDestroyedTurrets: types.FromPtr(s.AwayDestroyedTurrets),
		HomeGold:             types.FromPtr(s.HomeGold),
		AwayGold:             types.FromPtr(s.AwayGold),
		HomeDestroyedTowers:  types.FromPtr(s.HomeDestroyedTowers),
		AwayDestroyedTowers:  types.FromPtr(s.AwayDestroyedTowers),
		HomeGoals:            types.FromPtr(s.HomeGoals),
		AwayGoals:            types.FromPtr(s.AwayGoals),
		Time:                 types.FromPtr(s.Time),
		GameTime:             types.FromPtr(s.GameTime),
		ElapsedTime:          types.FromPtr(s.ElapsedTime),
		HomePoints:           types.FromPtr(s.HomePoints),
		AwayPoints:           types.FromPtr(s.AwayPoints),
		RemainingGameTime:    types.FromPtr(s.RemainingGameTime),
		HomeRuns:             types.FromPtr(s.HomeRuns),
		AwayRuns:             types.FromPtr(s.AwayRuns),
		HomeWicketsFallen:    types.FromPtr(s.HomeWicketsFallen),
		AwayWicketsFallen:    types.FromPtr(s.AwayWicketsFallen),
		HomeOversPlayed:      types.FromPtr(s.HomeOversPlayed),
		HomeBallsPlayed:      types.FromPtr(s.HomeBallsPlayed),
		AwayOversPlayed:      types.FromPtr(s.AwayOversPlayed),
		AwayBallsPlayed:      types.FromPtr(s.AwayBallsPlayed),
		HomeWonCoinToss:      types.FromPtr(s.HomeWonCoinToss),
		HomeBatting:          types.FromPtr(s.HomeBatting),
		AwayBatting:          types.FromPtr(s.AwayBatting),
		Inning:               types.FromPtr(s.Inning),
		HomeGames:            types.FromPtr(s.HomeGames),
		AwayGames:            types.FromPtr(s.AwayGames),
	}
}

func makeAPIScoreboard(s *apiXML.Scoreboard) types.Scoreboard {
	return types.Scoreboard{
		CurrentCTTeam:        types.FromPtr(s.CurrentCTTeam),
		CurrentDefenderTeam:  types.FromPtr(s.CurrentDefenderTeam),
		HomeWonRounds:        types.FromPtr(s.HomeWonRounds),
		AwayWonRounds:        types.FromPtr(s.AwayWonRounds),
		CurrentRound:         types.FromPtr(s.CurrentRound),
		HomeKills:            types.FromPtr(s.HomeKills),
		AwayKills:            types.FromPtr(s.AwayKills),
		HomeDestroyedTurrets: types.FromPtr(s.HomeDestroyedTurrets),
		AwayDestroyedTurrets: types.FromPtr(s.AwayDestroyedTurrets),
		HomeGold:             types.FromPtr(s.HomeGold),
		AwayGold:             types.FromPtr(s.AwayGold),
		HomeDestroyedTowers:  types.FromPtr(s.HomeDestroyedTowers),
		AwayDestroyedTowers:  types.FromPtr(s.AwayDestroyedTowers),
		HomeGoals:            types.FromPtr(s.HomeGoals),
		AwayGoals:            types.FromPtr(s.AwayGoals),
		Time:                 types.FromPtr(s.Time),
		GameTime:             types.FromPtr(s.GameTime),
		ElapsedTime:          types.FromPtr(s.ElapsedTime),
		HomePoints:           types.FromPtr(s.HomePoints),
		AwayPoints:           types.FromPtr(s.AwayPoints),
		RemainingGameTime:    types.FromPtr(s.RemainingGameTime),
		HomeRuns:             types.FromPtr(s.HomeRuns),
		AwayRuns:             types.FromPtr(s.AwayRuns),
		HomeWicketsFallen:    types.FromPtr(s.HomeWicketsFallen),
		AwayWicketsFallen:    types.FromPtr(s.AwayWicketsFallen),
		HomeOversPlayed:      types.FromPtr(s.HomeOversPlayed),
		HomeBallsPlayed:      types.FromPtr(s.HomeBallsPlayed),
		AwayOversPlayed:      types.FromPtr(s.AwayOversPlayed),
		AwayBallsPlayed:      types.FromPtr(s.AwayBallsPlayed),
		HomeWonCoinToss:      types.FromPtr(s.HomeWonCoinToss),
		HomeBatting:          types.FromPtr(s.HomeBatting),
		AwayBatting:          types.FromPtr(s.AwayBatting),
		Inning:               types.FromPtr(s.Inning),
		HomeGames:            types.FromPtr(s.HomeGames),
		AwayGames:            types.FromPtr(s.AwayGames),
	}
}

func makeFeedStatistics(stats *feedXML.Statistics) types.Statistics {
	if stats == nil {
		return types.Statistics{}
	}
	return types.Statistics{
		HomeYellowCards:    types.FromPtr(stats.YellowCards.ResolveHome()),
		AwayYellowCards:    types.FromPtr(stats.YellowCards.ResolveAway()),
		HomeRedCards:       types.FromPtr(stats.RedCards.ResolveHome()),
		AwayRedCards:       types.FromPtr(stats.RedCards.ResolveAway()),
		HomeYellowRedCards: types.FromPtr(stats.YellowRedCards.ResolveHome()),
		AwayYellowRedCards: types.FromPtr(stats.YellowRedCards.ResolveAway()),
		HomeCorners:        types.FromPtr(stats.Corners.ResolveHome()),
		AwayCorners:        types.FromPtr(stats.Corners.ResolveAway()),
	}
}

func (m *MatchStatusCache) fromFeedEventStatus(status int) types.EventStatus {
	switch status {
	case 0:
		return types.NotStartedEventStatus
	case 1:
		return types.LiveEventStatus
	case 2:
		return types.SuspendedEventStatus
	case 3:
		return types.EndedEventStatus
	case 4:
		return types.FinishedEventStatus
	case 5:
		return types.CancelledEventStatus
	default:
		return types.UnknownEventStatus
	}
}

func (m *MatchStatusCache) fromAPI(status apiXML.SportEventStatusType) types.EventStatus {
	switch s := types.EventStatus(status); s {
	case types.NotStartedEventStatus,
		types.LiveEventStatus,
		types.SuspendedEventStatus,
		types.EndedEventStatus,
		types.FinishedEventStatus,
		types.CancelledEventStatus,
		types.AbandonedEventStatus,
		types.DelayedEventStatus,
		types.PostponedEventStatus,
		types.InterruptedEventStatus:
		return s
	default:
		return types.UnknownEventStatus
	}
}

func newMatchStatusCache(lifeCtx context.Context, client *api.Client, oddsFeedConfiguration config.Config, logger *log.Logger) *MatchStatusCache {
	c := &MatchStatusCache{
		apiClient:             client,
		lifetime:              lifeCtx,
		oddsFeedConfiguration: oddsFeedConfiguration,
		logger:                logger,
		entries: lru.NewTTL[types.URN, *LocalizedMatchStatus](
			lru.DefaultEventCacheSize, nil, lru.DefaultEventCacheTTL),
		clearedAt: make(map[types.URN]time.Time),
	}
	client.SubscribeWithAPIObserver(c)
	return c
}

// BuildMatchStatus resolves a *types.MatchStatus snapshot. Fetches
// from the API if the status isn't yet cached. The localized status-code
// description is resolved through the static-data cache for the supplied
// locales (primary locale = locales[0]).
//
// Cache aliasing — two-layer decoupling:
//
//   - OUTER pointers (WinnerID, Scoreboard, Statistics): cloned via
//     clonePtr so the snapshot's *T points at a fresh struct. Caller
//     can do `*status.Scoreboard = Scoreboard{}` without affecting
//     the cache.
//
//   - INNER fields on Scoreboard / Statistics / PeriodScore: now
//     value-style Optional[T] (v2.26). Caller can do
//     `status.Scoreboard.HomeKills = Some(999)` or even wholesale-
//     replace the Scoreboard pointee without leaking into the cache.
//
//   - MatchStatusID / HomeScore / AwayScore: now Optional[T] directly
//     on the snapshot — value semantics, no aliasing.
//
// Together these close the inner-pointer aliasing class flagged in
// the v2.25 review.
func BuildMatchStatus(
	ctx context.Context,
	cache *MatchStatusCache,
	staticCache *LocalizedStaticDataCache,
	id types.URN,
	locales []types.Locale,
) (*types.MatchStatus, error) {
	entry, err := cache.MatchStatus(ctx, id)
	if err != nil {
		return nil, err
	}
	out := &types.MatchStatus{
		WinnerID:              clonePtr(entry.winnerID),
		Status:                entry.status,
		MatchStatusID:         types.FromPtr(entry.matchStatusID),
		HomeScore:             types.FromPtr(entry.homeScore),
		AwayScore:             types.FromPtr(entry.awayScore),
		IsScoreboardAvailable: entry.isScoreboardAvailable,
		PeriodScores:          append([]types.PeriodScore(nil), entry.periodScores...),
		Scoreboard:            clonePtr(entry.scoreboard),
		Statistics:            clonePtr(entry.statistics),
	}
	if entry.matchStatusID != nil && staticCache != nil {
		desc, err := staticCache.LocalizedItem(ctx, *entry.matchStatusID, locales)
		switch {
		case err == nil:
			out.StatusDescription = &desc
		case errors.Is(err, ErrItemNotFoundInCache):
			// The status-code description is genuinely absent from the
			// upstream catalog — leave StatusDescription nil (documented
			// optional). Only THIS outcome is tolerated.
		default:
			// Context cancellation or a transport/API failure must NOT be
			// silently converted to "no description": that made a
			// transient/canceled load indistinguishable from a genuinely
			// unknown status code. Propagate with match/status/locale
			// context.
			return nil, fmt.Errorf("match status %s: resolve status-code %d description (locales=%v): %w",
				id.ToString(), *entry.matchStatusID, locales, err)
		}
	}
	return out, nil
}
