package cache

import (
	"context"
	"errors"
	"sync"

	"github.com/oddin-gg/gosdk/internal/api"
	apiXML "github.com/oddin-gg/gosdk/internal/api/xml"
	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	"github.com/oddin-gg/gosdk/protocols"
	log "github.com/oddin-gg/gosdk/internal/log"
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
// *protocols.MatchStatus value with the localized status-code description
// resolved at construction.
type MatchStatusCache struct {
	apiClient             *api.Client
	logger                *log.Logger
	oddsFeedConfiguration protocols.OddsFeedConfiguration

	mu      sync.RWMutex
	entries map[protocols.URN]*LocalizedMatchStatus
}

// LocalizedMatchStatus is the cache entry. Fields are value-typed and
// immutable per snapshot — refreshOrInsert* builds a fresh copy and
// atomic-swaps it into the map.
type LocalizedMatchStatus struct {
	winnerID              *protocols.URN
	status                protocols.EventStatus
	periodScores          []protocols.PeriodScore
	matchStatusID         *uint
	homeScore             float64
	awayScore             float64
	isScoreboardAvailable bool
	scoreboard            *protocols.Scoreboard
	statistics            *protocols.Statistics
}

// OnFeedMessage ...
func (m *MatchStatusCache) OnFeedMessage(id protocols.URN, feedMessage *protocols.FeedMessage) {
	if feedMessage.Message == nil {
		return
	}
	message, ok := feedMessage.Message.(*feedXML.OddsChange)
	if !ok || message.SportEventStatus == nil {
		return
	}
	m.refreshOrInsertFeedItem(id, message.SportEventStatus)
}

// OnAPIResponse ...
func (m *MatchStatusCache) OnAPIResponse(apiResponse protocols.Response) {
	msg, ok := apiResponse.Data.(*apiXML.MatchSummaryResponse)
	if !ok {
		return
	}
	id, err := protocols.ParseURN(msg.SportEvent.ID)
	if err != nil {
		m.logger.WithError(err).Errorf("failed to parse urn %s", msg.SportEvent.ID)
		return
	}
	if err := m.refreshOrInsertAPIItem(*id, msg.SportEventStatus); err != nil {
		m.logger.WithError(err).Errorf("failed to refresh api item %v", *id)
	}
}

// ClearCacheItem ...
func (m *MatchStatusCache) ClearCacheItem(id protocols.URN) {
	m.mu.Lock()
	delete(m.entries, id)
	m.mu.Unlock()
}

// Purge clears the entire cache.
func (m *MatchStatusCache) Purge() {
	m.mu.Lock()
	m.entries = make(map[protocols.URN]*LocalizedMatchStatus)
	m.mu.Unlock()
}

// MatchStatus returns a cached status, fetching from the API on miss.
// The fetch triggers OnAPIResponse via the api.Client observer hook,
// which populates the cache; we then re-read.
func (m *MatchStatusCache) MatchStatus(ctx context.Context, id protocols.URN) (*LocalizedMatchStatus, error) {
	m.mu.RLock()
	entry, ok := m.entries[id]
	m.mu.RUnlock()
	if ok {
		return entry, nil
	}

	if _, err := m.apiClient.FetchMatchSummary(ctx, id, m.oddsFeedConfiguration.DefaultLocale()); err != nil {
		return nil, err
	}

	m.mu.RLock()
	entry, ok = m.entries[id]
	m.mu.RUnlock()
	if !ok {
		return nil, errors.New("item missing")
	}
	return entry, nil
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

func (m *MatchStatusCache) refreshOrInsertFeedItem(id protocols.URN, data *feedXML.SportEventStatus) {
	m.mu.RLock()
	prev := m.entries[id]
	m.mu.RUnlock()

	result := m.shallowClone(prev)
	result.status = m.fromFeedEventStatus(data.Status)
	if data.PeriodScores != nil {
		result.periodScores = m.mapFeedPeriodScores(data.PeriodScores.List)
	}
	result.matchStatusID = &data.MatchStatus
	result.homeScore = data.HomeScore
	result.awayScore = data.AwayScore
	result.isScoreboardAvailable = data.ScoreboardAvailable
	if data.Scoreboard != nil {
		sb := makeFeedScoreboard(data.Scoreboard)
		result.scoreboard = &sb
	}
	if data.Statistics != nil {
		s := makeFeedStatistics(data.Statistics)
		result.statistics = &s
	}

	m.mu.Lock()
	m.entries[id] = result
	m.mu.Unlock()
}

func (m *MatchStatusCache) refreshOrInsertAPIItem(id protocols.URN, data apiXML.SportEventStatus) error {
	var winnerID *protocols.URN
	if data.WinnerID != nil {
		var err error
		winnerID, err = protocols.ParseURN(*data.WinnerID)
		if err != nil {
			return err
		}
	}

	m.mu.RLock()
	prev := m.entries[id]
	m.mu.RUnlock()

	result := m.shallowClone(prev)
	result.status = m.fromAPI(data.Status)
	result.winnerID = winnerID
	result.matchStatusID = &data.MatchStatusCode
	result.homeScore = data.HomeScore
	result.awayScore = data.AwayScore
	if data.PeriodScores != nil {
		result.periodScores = m.mapAPIPeriodScores(data.PeriodScores.List)
	}
	result.isScoreboardAvailable = data.ScoreboardAvailable
	if data.Scoreboard != nil {
		sb := makeAPIScoreboard(data.Scoreboard)
		result.scoreboard = &sb
	}

	m.mu.Lock()
	m.entries[id] = result
	m.mu.Unlock()
	return nil
}

// --- mapping helpers ---

func (m *MatchStatusCache) mapAPIPeriodScores(periodScores []*apiXML.PeriodScore) []protocols.PeriodScore {
	result := make([]protocols.PeriodScore, len(periodScores))
	for i := range periodScores {
		ps := periodScores[i]
		result[i] = protocols.PeriodScore{
			Type:              ps.Type,
			HomeScore:         ps.HomeScore,
			AwayScore:         ps.AwayScore,
			PeriodNumber:      ps.Number,
			MatchStatusCode:   ps.MatchStatusCode,
			HomeWonRounds:     ps.HomeWonRounds,
			AwayWonRounds:     ps.AwayWonRounds,
			HomeKills:         ps.HomeKills,
			AwayKills:         ps.AwayKills,
			HomeGoals:         ps.HomeGoals,
			AwayGoals:         ps.AwayGoals,
			HomePoints:        ps.HomePoints,
			AwayPoints:        ps.AwayPoints,
			HomeRuns:          ps.HomeRuns,
			AwayRuns:          ps.AwayRuns,
			HomeWicketsFallen: ps.HomeWicketsFallen,
			AwayWicketsFallen: ps.AwayWicketsFallen,
			HomeOversPlayed:   ps.HomeOversPlayed,
			HomeBallsPlayed:   ps.HomeBallsPlayed,
			AwayOversPlayed:   ps.AwayOversPlayed,
			AwayBallsPlayed:   ps.AwayBallsPlayed,
			HomeWonCoinToss:   ps.HomeWonCoinToss,
		}
	}
	return result
}

func (m *MatchStatusCache) mapFeedPeriodScores(periodScores []*feedXML.PeriodScore) []protocols.PeriodScore {
	result := make([]protocols.PeriodScore, len(periodScores))
	for i := range periodScores {
		ps := periodScores[i]
		result[i] = protocols.PeriodScore{
			Type:              ps.Type,
			HomeScore:         ps.HomeScore,
			AwayScore:         ps.AwayScore,
			PeriodNumber:      ps.Number,
			MatchStatusCode:   ps.MatchStatusCode,
			HomeWonRounds:     ps.HomeWonRounds,
			AwayWonRounds:     ps.AwayWonRounds,
			HomeKills:         ps.HomeKills,
			AwayKills:         ps.AwayKills,
			HomeGoals:         ps.HomeGoals,
			AwayGoals:         ps.AwayGoals,
			HomePoints:        ps.HomePoints,
			AwayPoints:        ps.AwayPoints,
			HomeRuns:          ps.HomeRuns,
			AwayRuns:          ps.AwayRuns,
			HomeWicketsFallen: ps.HomeWicketsFallen,
			AwayWicketsFallen: ps.AwayWicketsFallen,
			HomeOversPlayed:   ps.HomeOversPlayed,
			HomeBallsPlayed:   ps.HomeBallsPlayed,
			AwayOversPlayed:   ps.AwayOversPlayed,
			AwayBallsPlayed:   ps.AwayBallsPlayed,
			HomeWonCoinToss:   ps.HomeWonCoinToss,
		}
	}
	return result
}

func makeFeedScoreboard(s *feedXML.Scoreboard) protocols.Scoreboard {
	return protocols.Scoreboard{
		CurrentCTTeam:        s.CurrentCTTeam,
		CurrentDefenderTeam:  s.CurrentDefenderTeam,
		HomeWonRounds:        s.HomeWonRounds,
		AwayWonRounds:        s.AwayWonRounds,
		CurrentRound:         s.CurrentRound,
		HomeKills:            s.HomeKills,
		AwayKills:            s.AwayKills,
		HomeDestroyedTurrets: s.HomeDestroyedTurrets,
		AwayDestroyedTurrets: s.AwayDestroyedTurrets,
		HomeGold:             s.HomeGold,
		AwayGold:             s.AwayGold,
		HomeDestroyedTowers:  s.HomeDestroyedTowers,
		AwayDestroyedTowers:  s.AwayDestroyedTowers,
		HomeGoals:            s.HomeGoals,
		AwayGoals:            s.AwayGoals,
		Time:                 s.Time,
		GameTime:             s.GameTime,
		ElapsedTime:          s.ElapsedTime,
		HomePoints:           s.HomePoints,
		AwayPoints:           s.AwayPoints,
		RemainingGameTime:    s.RemainingGameTime,
		HomeRuns:             s.HomeRuns,
		AwayRuns:             s.AwayRuns,
		HomeWicketsFallen:    s.HomeWicketsFallen,
		AwayWicketsFallen:    s.AwayWicketsFallen,
		HomeOversPlayed:      s.HomeOversPlayed,
		HomeBallsPlayed:      s.HomeBallsPlayed,
		AwayOversPlayed:      s.AwayOversPlayed,
		AwayBallsPlayed:      s.AwayBallsPlayed,
		HomeWonCoinToss:      s.HomeWonCoinToss,
		HomeBatting:          s.HomeBatting,
		AwayBatting:          s.AwayBatting,
		Inning:               s.Inning,
		HomeGames:            s.HomeGames,
		AwayGames:            s.AwayGames,
	}
}

func makeAPIScoreboard(s *apiXML.Scoreboard) protocols.Scoreboard {
	return protocols.Scoreboard{
		CurrentCTTeam:        s.CurrentCTTeam,
		CurrentDefenderTeam:  s.CurrentDefenderTeam,
		HomeWonRounds:        s.HomeWonRounds,
		AwayWonRounds:        s.AwayWonRounds,
		CurrentRound:         s.CurrentRound,
		HomeKills:            s.HomeKills,
		AwayKills:            s.AwayKills,
		HomeDestroyedTurrets: s.HomeDestroyedTurrets,
		AwayDestroyedTurrets: s.AwayDestroyedTurrets,
		HomeGold:             s.HomeGold,
		AwayGold:             s.AwayGold,
		HomeDestroyedTowers:  s.HomeDestroyedTowers,
		AwayDestroyedTowers:  s.AwayDestroyedTowers,
		HomeGoals:            s.HomeGoals,
		AwayGoals:            s.AwayGoals,
		Time:                 s.Time,
		GameTime:             s.GameTime,
		ElapsedTime:          s.ElapsedTime,
		HomePoints:           s.HomePoints,
		AwayPoints:           s.AwayPoints,
		RemainingGameTime:    s.RemainingGameTime,
		HomeRuns:             s.HomeRuns,
		AwayRuns:             s.AwayRuns,
		HomeWicketsFallen:    s.HomeWicketsFallen,
		AwayWicketsFallen:    s.AwayWicketsFallen,
		HomeOversPlayed:      s.HomeOversPlayed,
		HomeBallsPlayed:      s.HomeBallsPlayed,
		AwayOversPlayed:      s.AwayOversPlayed,
		AwayBallsPlayed:      s.AwayBallsPlayed,
		HomeWonCoinToss:      s.HomeWonCoinToss,
		HomeBatting:          s.HomeBatting,
		AwayBatting:          s.AwayBatting,
		Inning:               s.Inning,
		HomeGames:            s.HomeGames,
		AwayGames:            s.AwayGames,
	}
}

func makeFeedStatistics(stats *feedXML.Statistics) protocols.Statistics {
	if stats == nil {
		return protocols.Statistics{}
	}
	return protocols.Statistics{
		HomeYellowCards:    stats.YellowCards.ResolveHome(),
		AwayYellowCards:    stats.YellowCards.ResolveAway(),
		HomeRedCards:       stats.RedCards.ResolveHome(),
		AwayRedCards:       stats.RedCards.ResolveAway(),
		HomeYellowRedCards: stats.YellowRedCards.ResolveHome(),
		AwayYellowRedCards: stats.YellowRedCards.ResolveAway(),
		HomeCorners:        stats.Corners.ResolveHome(),
		AwayCorners:        stats.Corners.ResolveAway(),
	}
}

func (m *MatchStatusCache) fromFeedEventStatus(status int) protocols.EventStatus {
	switch status {
	case 0:
		return protocols.NotStartedEventStatus
	case 1:
		return protocols.LiveEventStatus
	case 2:
		return protocols.SuspendedEventStatus
	case 3:
		return protocols.EndedEventStatus
	case 4:
		return protocols.FinishedEventStatus
	case 5:
		return protocols.CancelledEventStatus
	default:
		return protocols.UnknownEventStatus
	}
}

func (m *MatchStatusCache) fromAPI(status apiXML.SportEventStatusType) protocols.EventStatus {
	switch s := protocols.EventStatus(status); s {
	case protocols.NotStartedEventStatus,
		protocols.LiveEventStatus,
		protocols.SuspendedEventStatus,
		protocols.EndedEventStatus,
		protocols.FinishedEventStatus,
		protocols.CancelledEventStatus,
		protocols.AbandonedEventStatus,
		protocols.DelayedEventStatus,
		protocols.PostponedEventStatus,
		protocols.InterruptedEventStatus:
		return s
	default:
		return protocols.UnknownEventStatus
	}
}

func newMatchStatusCache(client *api.Client, oddsFeedConfiguration protocols.OddsFeedConfiguration, logger *log.Logger) *MatchStatusCache {
	c := &MatchStatusCache{
		apiClient:             client,
		oddsFeedConfiguration: oddsFeedConfiguration,
		logger:                logger,
		entries:               make(map[protocols.URN]*LocalizedMatchStatus),
	}
	client.SubscribeWithAPIObserver(c)
	return c
}

// BuildMatchStatus resolves a *protocols.MatchStatus snapshot. Fetches
// from the API if the status isn't yet cached. The localized status-code
// description is resolved through the static-data cache for the supplied
// locales (primary locale = locales[0]).
func BuildMatchStatus(
	ctx context.Context,
	cache *MatchStatusCache,
	staticCache *LocalizedStaticDataCache,
	id protocols.URN,
	locales []protocols.Locale,
) (*protocols.MatchStatus, error) {
	entry, err := cache.MatchStatus(ctx, id)
	if err != nil {
		return nil, err
	}
	out := &protocols.MatchStatus{
		WinnerID:              entry.winnerID,
		Status:                entry.status,
		MatchStatusID:         entry.matchStatusID,
		IsScoreboardAvailable: entry.isScoreboardAvailable,
		PeriodScores:          append([]protocols.PeriodScore(nil), entry.periodScores...),
		Scoreboard:            entry.scoreboard,
		Statistics:            entry.statistics,
	}
	hs := entry.homeScore
	as := entry.awayScore
	out.HomeScore = &hs
	out.AwayScore = &as
	if entry.matchStatusID != nil && staticCache != nil {
		desc, err := staticCache.LocalizedItem(*entry.matchStatusID, locales)
		if err == nil {
			out.StatusDescription = &desc
		}
	}
	return out, nil
}
