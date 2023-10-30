package cache

import (
	"errors"
	"time"

	"github.com/oddin-gg/gosdk/internal/api"
	apiXML "github.com/oddin-gg/gosdk/internal/api/xml"
	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	"github.com/oddin-gg/gosdk/protocols"
	"github.com/patrickmn/go-cache"
	log "github.com/sirupsen/logrus"
)

var _ protocols.PeriodScore = (*periodScoreImpl)(nil)

type periodScoreImpl struct {
	periodType      string
	homeScore       float64
	awayScore       float64
	periodNumber    uint
	matchStatusCode uint
	homeWonRounds   *uint32
	awayWonRounds   *uint32
	homeKills       *int32
	awayKills       *int32
	homeGoals       *uint32
	awayGoals       *uint32
	homePoints      *uint32
	awayPoints      *uint32

	// RushCricket
	homeRuns          *uint32
	awayRuns          *uint32
	homeWicketsFallen *uint32
	awayWicketsFallen *uint32
	homeOversPlayed   *uint32
	homeBallsPlayed   *uint32
	awayOversPlayed   *uint32
	awayBallsPlayed   *uint32
	homeWonCoinToss   *bool
}

func (p periodScoreImpl) Type() string {
	return p.periodType
}

func (p periodScoreImpl) HomeGoals() *uint32 {
	return p.homeGoals
}

func (p periodScoreImpl) AwayGoals() *uint32 {
	return p.awayGoals
}

func (p periodScoreImpl) HomeScore() float64 {
	return p.homeScore
}

func (p periodScoreImpl) AwayScore() float64 {
	return p.awayScore
}

func (p periodScoreImpl) PeriodNumber() uint {
	return p.periodNumber
}

func (p periodScoreImpl) MatchStatusCode() uint {
	return p.matchStatusCode
}

func (p periodScoreImpl) HomeWonRounds() *uint32 {
	return p.homeWonRounds
}

func (p periodScoreImpl) AwayWonRounds() *uint32 {
	return p.awayWonRounds
}

func (p periodScoreImpl) HomeKills() *int32 {
	return p.homeKills
}

func (p periodScoreImpl) AwayKills() *int32 {
	return p.awayKills
}

func (p periodScoreImpl) HomePoints() *uint32 {
	return p.homePoints
}

func (p periodScoreImpl) AwayPoints() *uint32 {
	return p.awayPoints
}

func (p periodScoreImpl) HomeRuns() *uint32 {
	return p.homeRuns
}

func (p periodScoreImpl) AwayRuns() *uint32 {
	return p.awayRuns
}

func (p periodScoreImpl) HomeWicketsFallen() *uint32 {
	return p.homeWicketsFallen
}

func (p periodScoreImpl) AwayWicketsFallen() *uint32 {
	return p.awayWicketsFallen
}

func (p periodScoreImpl) HomeOversPlayed() *uint32 {
	return p.homeOversPlayed
}

func (p periodScoreImpl) AwayOversPlayed() *uint32 {
	return p.awayOversPlayed
}

func (p periodScoreImpl) HomeBallsPlayed() *uint32 {
	return p.homeBallsPlayed
}

func (p periodScoreImpl) AwayBallsPlayed() *uint32 {
	return p.awayBallsPlayed
}

func (p periodScoreImpl) HomeWonCoinToss() *bool {
	return p.homeWonCoinToss
}

type scoreboardImpl struct {
	currentCTTeam        *uint32
	homeWonRounds        *uint32
	awayWonRounds        *uint32
	currentRound         *uint32
	homeKills            *int32
	awayKills            *int32
	homeDestroyedTurrets *int32
	awayDestroyedTurrets *int32
	homeGold             *uint32
	awayGold             *uint32
	homeDestroyedTowers  *int32
	awayDestroyedTowers  *int32
	homeGoals            *uint32
	awayGoals            *uint32
	time                 *uint32
	gameTime             *uint32
	currentDefenderTeam  *uint32

	// VirtualBasketballScoreboard
	homePoints        *uint32
	awayPoints        *uint32
	remainingGameTime *uint32

	// RushCricketScoreboard
	homeRuns          *uint32
	awayRuns          *uint32
	homeWicketsFallen *uint32
	awayWicketsFallen *uint32
	homeOversPlayed   *uint32
	homeBallsPlayed   *uint32
	awayOversPlayed   *uint32
	awayBallsPlayed   *uint32
	homeWonCoinToss   *bool
	homeBatting       *bool
	awayBatting       *bool
	inning            *uint32
}

func (s scoreboardImpl) CurrentCTTeam() *uint32 {
	return s.currentCTTeam
}

func (s scoreboardImpl) CurrentDefenderTeam() *uint32 {
	return s.currentDefenderTeam
}

func (s scoreboardImpl) HomePoints() *uint32 {
	return s.homePoints
}

func (s scoreboardImpl) AwayPoints() *uint32 {
	return s.awayPoints
}

func (s scoreboardImpl) RemainingGameTime() *uint32 {
	return s.remainingGameTime
}

func (s scoreboardImpl) HomeWonRounds() *uint32 {
	return s.homeWonRounds
}

func (s scoreboardImpl) AwayWonRounds() *uint32 {
	return s.awayWonRounds
}

func (s scoreboardImpl) CurrentRound() *uint32 {
	return s.currentRound
}

func (s scoreboardImpl) HomeKills() *int32 {
	return s.homeKills
}

func (s scoreboardImpl) AwayKills() *int32 {
	return s.awayKills
}

func (s scoreboardImpl) HomeDestroyedTurrets() *int32 {
	return s.homeDestroyedTurrets
}

func (s scoreboardImpl) AwayDestroyedTurrets() *int32 {
	return s.awayDestroyedTurrets
}

func (s scoreboardImpl) HomeGold() *uint32 {
	return s.homeGold
}

func (s scoreboardImpl) AwayGold() *uint32 {
	return s.awayGold
}

func (s scoreboardImpl) HomeDestroyedTowers() *int32 {
	return s.homeDestroyedTowers
}

func (s scoreboardImpl) AwayDestroyedTowers() *int32 {
	return s.awayDestroyedTowers
}

func (s scoreboardImpl) HomeGoals() *uint32 {
	return s.homeGoals
}

func (s scoreboardImpl) AwayGoals() *uint32 {
	return s.awayGoals
}

func (s scoreboardImpl) Time() *uint32 {
	return s.time
}

func (s scoreboardImpl) GameTime() *uint32 {
	return s.gameTime
}

func (s scoreboardImpl) HomeRuns() *uint32 {
	return s.homeRuns
}

func (s scoreboardImpl) AwayRuns() *uint32 {
	return s.awayRuns
}

func (s scoreboardImpl) HomeWicketsFallen() *uint32 {
	return s.homeWicketsFallen
}

func (s scoreboardImpl) AwayWicketsFallen() *uint32 {
	return s.awayWicketsFallen
}

func (s scoreboardImpl) HomeOversPlayed() *uint32 {
	return s.homeOversPlayed
}

func (s scoreboardImpl) HomeBallsPlayed() *uint32 {
	return s.homeBallsPlayed
}

func (s scoreboardImpl) AwayOversPlayed() *uint32 {
	return s.awayOversPlayed
}

func (s scoreboardImpl) AwayBallsPlayed() *uint32 {
	return s.awayBallsPlayed
}

func (s scoreboardImpl) HomeWonCoinToss() *bool {
	return s.homeWonCoinToss
}

func (s scoreboardImpl) HomeBatting() *bool {
	return s.homeBatting
}

func (s scoreboardImpl) AwayBatting() *bool {
	return s.awayBatting
}

func (s scoreboardImpl) Inning() *uint32 {
	return s.inning
}

// MatchStatusCache ...
type MatchStatusCache struct {
	apiClient             *api.Client
	internalCache         *cache.Cache
	logger                *log.Entry
	oddsFeedConfiguration protocols.OddsFeedConfiguration
}

// OnFeedMessage ...
func (m MatchStatusCache) OnFeedMessage(id protocols.URN, feedMessage *protocols.FeedMessage) {
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
func (m MatchStatusCache) OnAPIResponse(apiResponse protocols.Response) {
	switch msg := apiResponse.Data.(type) {
	case *apiXML.MatchSummaryResponse:
		id, err := protocols.ParseURN(msg.SportEvent.ID)
		if err != nil {
			m.logger.WithError(err).Errorf("failed to parse urn %s", msg.SportEvent.ID)
		}

		err = m.refreshOrInsertAPIItem(*id, msg.SportEventStatus)
		if err != nil {
			m.logger.WithError(err).Errorf("failed to refresh api item %v", *id)
		}
	}
}

// ClearCacheItem ...
func (m MatchStatusCache) ClearCacheItem(id protocols.URN) {
	m.internalCache.Delete(id.ToString())
}

// MatchStatus ...
func (m MatchStatusCache) MatchStatus(id protocols.URN) (*LocalizedMatchStatus, error) {
	item, _ := m.internalCache.Get(id.ToString())
	result, ok := item.(*LocalizedMatchStatus)
	if ok {
		return result, nil
	}

	// This will trigger OnAPIResponse callback
	_, err := m.apiClient.FetchMatchSummary(id, m.oddsFeedConfiguration.DefaultLocale())
	if err != nil {
		return nil, err
	}

	item, _ = m.internalCache.Get(id.ToString())
	result, ok = item.(*LocalizedMatchStatus)
	if !ok {
		return nil, errors.New("item missing")
	}

	return result, nil
}

func (m MatchStatusCache) refreshOrInsertFeedItem(id protocols.URN, data *feedXML.SportEventStatus) {
	item, _ := m.internalCache.Get(id.ToString())
	result, ok := item.(*LocalizedMatchStatus)

	if !ok {
		result = &LocalizedMatchStatus{}
	}

	result.status = m.fromFeedEventStatus(data.Status)

	if data.PeriodScores != nil {
		result.periodScores = m.mapFeedPeriodScores(data.PeriodScores.List)
	}

	result.matchStatusID = &data.MatchStatus
	result.homeScore = data.HomeScore
	result.awayScore = data.AwayScore
	result.isScoreboardAvailable = data.ScoreboardAvailable
	if data.Scoreboard != nil {
		result.scoreboard = m.makeFeedScoreboard(data.Scoreboard)
	}

	m.internalCache.Set(id.ToString(), result, 0)
}

func (m MatchStatusCache) refreshOrInsertAPIItem(id protocols.URN, data apiXML.SportEventStatus) error {
	item, _ := m.internalCache.Get(id.ToString())
	result, ok := item.(*LocalizedMatchStatus)

	if !ok {
		result = &LocalizedMatchStatus{}
	}

	var winnerID *protocols.URN
	var err error
	if data.WinnerID != nil {
		winnerID, err = protocols.ParseURN(*data.WinnerID)
		if err != nil {
			return err
		}
	}

	result.status = m.fromAPI(data.Status)
	result.winnerID = winnerID
	result.matchStatusID = &data.MatchStatusCode
	result.homeScore = data.HomeScore
	result.awayScore = data.AwayScore
	result.periodScores = m.mapAPIPeriodScores(data.PeriodScores.List)
	result.isScoreboardAvailable = data.ScoreboardAvailable
	if data.Scoreboard != nil {
		result.scoreboard = m.makeAPIScoreboard(data.Scoreboard)
	}

	m.internalCache.Set(id.ToString(), result, 0)
	return nil
}

func (m MatchStatusCache) mapAPIPeriodScores(periodScores []*apiXML.PeriodScore) []protocols.PeriodScore {
	result := make([]protocols.PeriodScore, len(periodScores))
	for i := range periodScores {
		periodScore := periodScores[i]
		result[i] = periodScoreImpl{
			periodType:        periodScore.Type,
			homeScore:         periodScore.HomeScore,
			awayScore:         periodScore.AwayScore,
			periodNumber:      periodScore.Number,
			matchStatusCode:   periodScore.MatchStatusCode,
			homeWonRounds:     periodScore.HomeWonRounds,
			awayWonRounds:     periodScore.AwayWonRounds,
			homeKills:         periodScore.HomeKills,
			awayKills:         periodScore.AwayKills,
			homeGoals:         periodScore.HomeGoals,
			awayGoals:         periodScore.AwayGoals,
			homePoints:        periodScore.HomePoints,
			awayPoints:        periodScore.AwayPoints,
			homeRuns:          periodScore.HomeRuns,
			awayRuns:          periodScore.AwayRuns,
			homeWicketsFallen: periodScore.HomeWicketsFallen,
			awayWicketsFallen: periodScore.AwayWicketsFallen,
			homeOversPlayed:   periodScore.HomeOversPlayed,
			homeBallsPlayed:   periodScore.HomeBallsPlayed,
			awayOversPlayed:   periodScore.AwayOversPlayed,
			awayBallsPlayed:   periodScore.AwayBallsPlayed,
			homeWonCoinToss:   periodScore.HomeWonCoinToss,
		}
	}

	return result
}

func (m MatchStatusCache) mapFeedPeriodScores(periodScores []*feedXML.PeriodScore) []protocols.PeriodScore {
	result := make([]protocols.PeriodScore, len(periodScores))
	for i := range periodScores {
		periodScore := periodScores[i]
		result[i] = periodScoreImpl{
			periodType:        periodScore.Type,
			homeScore:         periodScore.HomeScore,
			awayScore:         periodScore.AwayScore,
			periodNumber:      periodScore.Number,
			matchStatusCode:   periodScore.MatchStatusCode,
			homeWonRounds:     periodScore.HomeWonRounds,
			awayWonRounds:     periodScore.AwayWonRounds,
			homeKills:         periodScore.HomeKills,
			awayKills:         periodScore.AwayKills,
			homeGoals:         periodScore.HomeGoals,
			awayGoals:         periodScore.AwayGoals,
			homePoints:        periodScore.HomePoints,
			awayPoints:        periodScore.AwayPoints,
			homeRuns:          periodScore.HomeRuns,
			awayRuns:          periodScore.AwayRuns,
			homeWicketsFallen: periodScore.HomeWicketsFallen,
			awayWicketsFallen: periodScore.AwayWicketsFallen,
			homeOversPlayed:   periodScore.HomeOversPlayed,
			homeBallsPlayed:   periodScore.HomeBallsPlayed,
			awayOversPlayed:   periodScore.AwayOversPlayed,
			awayBallsPlayed:   periodScore.AwayBallsPlayed,
			homeWonCoinToss:   periodScore.HomeWonCoinToss,
		}
	}

	return result
}

func (m MatchStatusCache) makeFeedScoreboard(scoreboard *feedXML.Scoreboard) protocols.Scoreboard {
	return &scoreboardImpl{
		currentCTTeam:        scoreboard.CurrentCTTeam,
		homeWonRounds:        scoreboard.HomeWonRounds,
		awayWonRounds:        scoreboard.AwayWonRounds,
		currentRound:         scoreboard.CurrentRound,
		homeKills:            scoreboard.HomeKills,
		awayKills:            scoreboard.AwayKills,
		homeDestroyedTurrets: scoreboard.HomeDestroyedTurrets,
		awayDestroyedTurrets: scoreboard.AwayDestroyedTurrets,
		homeGold:             scoreboard.HomeGold,
		awayGold:             scoreboard.AwayGold,
		homeDestroyedTowers:  scoreboard.HomeDestroyedTowers,
		awayDestroyedTowers:  scoreboard.AwayDestroyedTowers,
		homeGoals:            scoreboard.HomeGoals,
		awayGoals:            scoreboard.AwayGoals,
		time:                 scoreboard.Time,
		gameTime:             scoreboard.GameTime,
		currentDefenderTeam:  scoreboard.CurrentDefenderTeam,
		homePoints:           scoreboard.HomePoints,
		awayPoints:           scoreboard.AwayPoints,
		remainingGameTime:    scoreboard.RemainingGameTime,
		homeRuns:             scoreboard.HomeRuns,
		awayRuns:             scoreboard.AwayRuns,
		homeWicketsFallen:    scoreboard.HomeWicketsFallen,
		awayWicketsFallen:    scoreboard.AwayWicketsFallen,
		homeOversPlayed:      scoreboard.HomeOversPlayed,
		homeBallsPlayed:      scoreboard.HomeBallsPlayed,
		awayOversPlayed:      scoreboard.AwayOversPlayed,
		awayBallsPlayed:      scoreboard.AwayBallsPlayed,
		homeWonCoinToss:      scoreboard.HomeWonCoinToss,
		homeBatting:          scoreboard.HomeBatting,
		awayBatting:          scoreboard.AwayBatting,
		inning:               scoreboard.Inning,
	}
}

func (m MatchStatusCache) makeAPIScoreboard(scoreboard *apiXML.Scoreboard) protocols.Scoreboard {
	return &scoreboardImpl{
		currentCTTeam:        scoreboard.CurrentCTTeam,
		homeWonRounds:        scoreboard.HomeWonRounds,
		awayWonRounds:        scoreboard.AwayWonRounds,
		currentRound:         scoreboard.CurrentRound,
		homeKills:            scoreboard.HomeKills,
		awayKills:            scoreboard.AwayKills,
		homeDestroyedTurrets: scoreboard.HomeDestroyedTurrets,
		awayDestroyedTurrets: scoreboard.AwayDestroyedTurrets,
		homeGold:             scoreboard.HomeGold,
		awayGold:             scoreboard.AwayGold,
		homeDestroyedTowers:  scoreboard.HomeDestroyedTowers,
		awayDestroyedTowers:  scoreboard.AwayDestroyedTowers,
		homeGoals:            scoreboard.HomeGoals,
		awayGoals:            scoreboard.AwayGoals,
		time:                 scoreboard.Time,
		gameTime:             scoreboard.GameTime,
		currentDefenderTeam:  scoreboard.CurrentDefenderTeam,
		homePoints:           scoreboard.HomePoints,
		awayPoints:           scoreboard.AwayPoints,
		remainingGameTime:    scoreboard.RemainingGameTime,
		homeRuns:             scoreboard.HomeRuns,
		awayRuns:             scoreboard.AwayRuns,
		homeWicketsFallen:    scoreboard.HomeWicketsFallen,
		awayWicketsFallen:    scoreboard.AwayWicketsFallen,
		homeOversPlayed:      scoreboard.HomeOversPlayed,
		homeBallsPlayed:      scoreboard.HomeBallsPlayed,
		awayOversPlayed:      scoreboard.AwayOversPlayed,
		awayBallsPlayed:      scoreboard.AwayBallsPlayed,
		homeWonCoinToss:      scoreboard.HomeWonCoinToss,
		homeBatting:          scoreboard.HomeBatting,
		awayBatting:          scoreboard.AwayBatting,
		inning:               scoreboard.Inning,
	}
}

func (m MatchStatusCache) fromFeedEventStatus(status int) protocols.EventStatus {
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

func (m MatchStatusCache) fromAPI(status apiXML.SportEventStatusType) protocols.EventStatus {
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

func newMatchStatusCache(client *api.Client, oddsFeedConfiguration protocols.OddsFeedConfiguration, logger *log.Entry) *MatchStatusCache {
	matchStatusCache := &MatchStatusCache{
		apiClient:             client,
		oddsFeedConfiguration: oddsFeedConfiguration,
		// Don't delete item => wait for match to expire
		internalCache: cache.New(20*time.Minute, 1*time.Minute),
		logger:        logger,
	}

	client.SubscribeWithAPIObserver(matchStatusCache)

	return matchStatusCache
}

// LocalizedMatchStatus ...
type LocalizedMatchStatus struct {
	winnerID              *protocols.URN
	status                protocols.EventStatus
	periodScores          []protocols.PeriodScore
	matchStatusID         *uint
	homeScore             float64
	awayScore             float64
	isScoreboardAvailable bool
	scoreboard            protocols.Scoreboard
}

type matchStatusImpl struct {
	sportEventID                    protocols.URN
	matchStatusCache                *MatchStatusCache
	localizedStaticMatchStatusCache *LocalizedStaticDataCache
	locales                         []protocols.Locale
}

func (m matchStatusImpl) WinnerID() (*protocols.URN, error) {
	item, err := m.matchStatusCache.MatchStatus(m.sportEventID)
	if err != nil {
		return nil, err
	}

	return item.winnerID, nil
}

func (m matchStatusImpl) Status() (*protocols.EventStatus, error) {
	item, err := m.matchStatusCache.MatchStatus(m.sportEventID)
	if err != nil {
		return nil, err
	}

	return &item.status, nil
}

func (m matchStatusImpl) PeriodScores() ([]protocols.PeriodScore, error) {
	item, err := m.matchStatusCache.MatchStatus(m.sportEventID)
	if err != nil {
		return nil, err
	}

	return item.periodScores, nil
}

func (m matchStatusImpl) MatchStatusID() (*uint, error) {
	item, err := m.matchStatusCache.MatchStatus(m.sportEventID)
	if err != nil {
		return nil, err
	}

	return item.matchStatusID, nil
}

func (m matchStatusImpl) MatchStatus() (protocols.LocalizedStaticData, error) {
	status, err := m.MatchStatusID()
	if err != nil {
		return nil, err
	}

	return m.localizedStaticMatchStatusCache.LocalizedItem(*status, m.locales)
}

func (m matchStatusImpl) LocalizedMatchStatus(locale protocols.Locale) (protocols.LocalizedStaticData, error) {
	status, err := m.MatchStatusID()
	if err != nil {
		return nil, err
	}

	return m.localizedStaticMatchStatusCache.LocalizedItem(*status, []protocols.Locale{locale})
}

func (m matchStatusImpl) HomeScore() (*float64, error) {
	item, err := m.matchStatusCache.MatchStatus(m.sportEventID)
	if err != nil {
		return nil, err
	}

	return &item.homeScore, nil
}

func (m matchStatusImpl) AwayScore() (*float64, error) {
	item, err := m.matchStatusCache.MatchStatus(m.sportEventID)
	if err != nil {
		return nil, err
	}

	return &item.awayScore, nil
}

func (m matchStatusImpl) IsScoreboardAvailable() (bool, error) {
	item, err := m.matchStatusCache.MatchStatus(m.sportEventID)
	if err != nil {
		return false, err
	}

	return item.isScoreboardAvailable, nil
}

func (m matchStatusImpl) Scoreboard() (protocols.Scoreboard, error) {
	item, err := m.matchStatusCache.MatchStatus(m.sportEventID)
	if err != nil {
		return nil, err
	}

	return item.scoreboard, nil
}

// NewMatchStatus ...
func NewMatchStatus(sportEventID protocols.URN, matchStatusCache *MatchStatusCache, localizedStaticMatchStatusCache *LocalizedStaticDataCache, locales []protocols.Locale) protocols.MatchStatus {
	return &matchStatusImpl{
		sportEventID:                    sportEventID,
		matchStatusCache:                matchStatusCache,
		localizedStaticMatchStatusCache: localizedStaticMatchStatusCache,
		locales:                         locales,
	}
}
