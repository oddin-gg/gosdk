package api

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	data "github.com/oddin-gg/gosdk/internal/api/xml"
	"github.com/oddin-gg/gosdk/protocols"
)

const (
	apiVersion     = "v1"
	timeoutSeconds = 10
	timeLayout     = "2006-01-02"
	retryCount     = 3
	retryDelay     = 10 * time.Second
)

// Observer ...
type Observer interface {
	OnAPIResponse(apiResponse protocols.Response)
}

// Client ...
type Client struct {
	cfg        protocols.OddsFeedConfiguration
	msgCh      chan protocols.Response
	lock       sync.RWMutex
	observers  []Observer
	httpClient http.Client
	closed     bool
}

// FetchWhoAmI ...
func (c *Client) FetchWhoAmI() (*data.WhoAMI, error) {
	var resp data.WhoAMI
	err := c.fetchData("/users/whoami", &resp, nil)
	return &resp, err
}

// FetchProducers ...
func (c *Client) FetchProducers() ([]data.Producer, error) {
	var resp data.ProducersResponse
	err := c.fetchData("/descriptions/producers", &resp, nil)
	if err != nil {
		return nil, err
	}

	return resp.Producers, nil
}

// FetchSports ...
func (c *Client) FetchSports(locale protocols.Locale) ([]data.Sport, error) {
	var resp data.SportsResponse
	err := c.fetchData(fmt.Sprintf("/sports/%s/sports", locale), &resp, &locale)
	if err != nil {
		return nil, err
	}

	return resp.Sports, nil
}

// FetchMatchStatusDescriptions ...
func (c *Client) FetchMatchStatusDescriptions(locale protocols.Locale) ([]data.MatchStatus, error) {
	var resp data.MatchStatusDescriptionResponse
	err := c.fetchData(fmt.Sprintf("/descriptions/%s/match_status", locale), &resp, &locale)
	if err != nil {
		return nil, err
	}

	return resp.MatchStatus, nil
}

// FetchFixtureChanges ...
func (c *Client) FetchFixtureChanges(locale protocols.Locale, after time.Time) ([]data.FixtureChange, error) {
	path := fmt.Sprintf("/sports/%s/fixtures/changes", locale)

	if !after.IsZero() {
		path = fmt.Sprintf("%s?after=%d", path, after.UnixNano()/1e6)
	}

	var resp data.FixtureChangesResponse
	err := c.fetchData(path, &resp, &locale)
	if err != nil {
		return nil, err
	}

	return resp.Changes, nil
}

// FetchFixture ...
func (c *Client) FetchFixture(id protocols.URN, locale protocols.Locale) (*data.Fixture, error) {
	var resp data.FixtureResponse
	err := c.fetchData(fmt.Sprintf("/sports/%s/sport_events/%s/fixture", locale, id.ToString()), &resp, &locale)
	if err != nil {
		return nil, err
	}

	return &resp.Fixture, nil
}

// FetchSchedule ...
func (c *Client) FetchSchedule(startIndex, limit uint, locale protocols.Locale) ([]data.SportEvent, error) {
	var resp data.ScheduleResponse
	err := c.fetchData(fmt.Sprintf("/sports/%s/schedules/pre/schedule?start=%d&limit=%d", locale, startIndex, limit), &resp, &locale)
	if err != nil {
		return nil, err
	}

	return resp.SportEvents, nil
}

// FetchTournaments ...
func (c *Client) FetchTournaments(sportID protocols.URN, locale protocols.Locale) ([]data.Tournament, error) {
	var resp data.SportTournamentsResponse
	err := c.fetchData(fmt.Sprintf("/sports/%s/sports/%s/tournaments", locale, sportID.ToString()), &resp, &locale)
	if err != nil {
		return nil, err
	}

	if resp.Tournaments == nil {
		// sport has no tournaments
		return nil, nil
	}

	return resp.Tournaments.Tournament, nil
}

// FetchTournament ...
func (c *Client) FetchTournament(id protocols.URN, locale protocols.Locale) (*data.TournamentExtended, error) {
	var resp data.SportTournamentInfoResponse
	err := c.fetchData(fmt.Sprintf("/sports/%s/tournaments/%s/info", locale, id.ToString()), &resp, &locale)
	if err != nil {
		return nil, err
	}

	return &resp.Tournament, nil
}

// FetchCompetitorProfile ...
func (c *Client) FetchCompetitorProfile(id protocols.URN, locale protocols.Locale) (*data.TeamExtended, error) {
	resp, err := c.FetchCompetitorProfileWithPlayers(id, locale)
	if err != nil {
		return nil, err
	}

	return &resp.Competitor, nil
}

// FetchCompetitorProfileWithPlayers ...
func (c *Client) FetchCompetitorProfileWithPlayers(id protocols.URN, locale protocols.Locale) (*data.CompetitorResponse, error) {
	var resp data.CompetitorResponse
	err := c.fetchData(fmt.Sprintf("/sports/%s/competitors/%s/profile", locale, id.ToString()), &resp, &locale)
	if err != nil {
		return nil, err
	}

	return &resp, nil
}

// FetchMatchSummary ...
func (c *Client) FetchMatchSummary(id protocols.URN, locale protocols.Locale) (*data.MatchSummaryResponse, error) {
	var resp data.MatchSummaryResponse
	err := c.fetchData(fmt.Sprintf("/sports/%s/sport_events/%s/summary", locale, id.ToString()), &resp, &locale)
	if err != nil {
		return nil, err
	}

	return &resp, nil
}

// FetchLiveMatches ...
func (c *Client) FetchLiveMatches(locale protocols.Locale) ([]data.SportEvent, error) {
	var resp data.ScheduleResponse
	err := c.fetchData(fmt.Sprintf("/sports/%s/schedules/live/schedule", locale), &resp, &locale)
	if err != nil {
		return nil, err
	}

	return resp.SportEvents, nil
}

// FetchMatches ...
func (c *Client) FetchMatches(t time.Time, locale protocols.Locale) ([]data.SportEvent, error) {
	var resp data.ScheduleResponse
	err := c.fetchData(fmt.Sprintf("/sports/%s/schedules/%s/schedule", locale, t.Format(timeLayout)), &resp, &locale)
	if err != nil {
		return nil, err
	}

	return resp.SportEvents, nil
}

// FetchMarketDescriptions ...
func (c *Client) FetchMarketDescriptions(locale protocols.Locale) ([]data.MarketDescription, error) {
	var resp data.MarketDescriptionResponse
	err := c.fetchData(fmt.Sprintf("/descriptions/%s/markets", locale), &resp, &locale)
	if err != nil {
		return nil, err
	}

	return resp.Markets, nil
}

// FetchMarketDescriptionsWithDynamicOutcomes ...
func (c *Client) FetchMarketDescriptionsWithDynamicOutcomes(
	marketTypeID uint,
	marketVariant string,
	locale protocols.Locale,
) ([]data.MarketDescription, error) {
	var resp data.MarketDescriptionResponse
	err := c.fetchData(
		fmt.Sprintf("/descriptions/%s/markets/%d/variants/%s", locale, marketTypeID, marketVariant),
		&resp,
		&locale,
	)
	if err != nil {
		return nil, err
	}

	return resp.Markets, nil
}

// FetchMarketVoidReasons ...
func (c *Client) FetchMarketVoidReasons() ([]data.MarketVoidReasons, error) {
	var resp data.MarketVoidReasonsResponse
	if err := c.fetchData("/descriptions/void_reasons", &resp, nil); err != nil {
		return nil, err
	}
	return resp.VoidReasons, nil
}

// FetchPlayerProfile fetch player's profile
func (c *Client) FetchPlayerProfile(playerID string, locale protocols.Locale) (*data.PlayerProfile, error) {
	var resp data.PlayerProfile
	err := c.fetchData(fmt.Sprintf("/sports/%s/players/%s/profile", locale, playerID), &resp, &locale)
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

// PostEventStatefulRecovery ...
func (c *Client) PostEventStatefulRecovery(producerName string, eventID protocols.URN, requestID uint, nodeID *int) (bool, error) {
	path := fmt.Sprintf("/%s/stateful_messages/events/%s/initiate_request?request_id=%d", producerName, eventID.ToString(), requestID)
	if nodeID != nil {
		path = fmt.Sprintf("%s&node_id=%d", path, *nodeID)
	}

	res, err := c.do(http.MethodPost, path)
	if err != nil {
		return false, err
	}
	_ = res.Body.Close()

	return true, nil
}

// PostEventOddsRecovery ...
func (c *Client) PostEventOddsRecovery(producerName string, eventID protocols.URN, requestID uint, nodeID *int) (bool, error) {
	path := fmt.Sprintf("/%s/odds/events/%s/initiate_request?request_id=%d", producerName, eventID.ToString(), requestID)
	if nodeID != nil {
		path = fmt.Sprintf("%s&node_id=%d", path, *nodeID)
	}

	res, err := c.do(http.MethodPost, path)
	if err != nil {
		return false, err
	}
	_ = res.Body.Close()

	return true, nil
}

// PostRecovery ...
func (c *Client) PostRecovery(producerName string, requestID uint, nodeID *int, after time.Time) (bool, error) {
	path := fmt.Sprintf("/%s/recovery/initiate_request?request_id=%d", producerName, requestID)
	if nodeID != nil {
		path = fmt.Sprintf("%s&node_id=%d", path, *nodeID)
	}

	if !after.IsZero() {
		path = fmt.Sprintf("%s&after=%d", path, after.UnixNano()/1e6)
	}

	res, err := c.do(http.MethodPost, path)
	if err != nil {
		return false, err
	}
	_ = res.Body.Close()

	return true, nil
}

// PostReplayClear ...
func (c *Client) PostReplayClear(nodeID *int) (bool, error) {
	path := "/replay/clear"
	if nodeID != nil {
		path = fmt.Sprintf("%s?node_id=%d", path, *nodeID)
	}

	res, err := c.do(http.MethodPost, path)
	if err != nil {
		return false, err
	}
	_ = res.Body.Close()

	return true, nil
}

// PostReplayStop ...
func (c *Client) PostReplayStop(nodeID *int) (bool, error) {
	path := "/replay/stop"
	if nodeID != nil {
		path = fmt.Sprintf("%s?node_id=%d", path, *nodeID)
	}

	res, err := c.do(http.MethodPost, path)
	if err != nil {
		return false, err
	}
	_ = res.Body.Close()

	return true, nil
}

// FetchReplaySetContent ...
func (c *Client) FetchReplaySetContent(nodeID *int) ([]data.ReplayEvent, error) {
	path := "/replay"
	if nodeID != nil {
		path = fmt.Sprintf("%s?node_id=%d", path, *nodeID)
	}

	var res data.ReplayResponse
	err := c.fetchData(path, &res, nil)
	if err != nil {
		return nil, err
	}

	return res.SportEvents, nil
}

// PutReplayEvent ...
func (c *Client) PutReplayEvent(eventID protocols.URN, nodeID *int) (bool, error) {
	path := fmt.Sprintf("/replay/events/%s", eventID.ToString())
	if nodeID != nil {
		path = fmt.Sprintf("%s?node_id=%d", path, *nodeID)
	}

	res, err := c.do(http.MethodPut, path)
	if err != nil {
		return false, err
	}
	_ = res.Body.Close()

	return true, nil
}

// DeleteReplayEvent ...
func (c *Client) DeleteReplayEvent(eventID protocols.URN, nodeID *int) (bool, error) {
	path := fmt.Sprintf("/replay/events/%s", eventID.ToString())
	if nodeID != nil {
		path = fmt.Sprintf("%s?node_id=%d", path, *nodeID)
	}

	res, err := c.do(http.MethodDelete, path)
	if err != nil {
		return false, err
	}
	_ = res.Body.Close()

	return true, nil
}

// PostReplayStart ...
func (c *Client) PostReplayStart(nodeID *int, speed *int, maxDelay *int, useReplayTimestamp *bool, product *string, runParallel *bool) (bool, error) {
	queryParams := make(map[string]interface{})
	if nodeID != nil {
		queryParams["node_id"] = *nodeID
	}

	if speed != nil {
		queryParams["speed"] = *speed
	}

	if maxDelay != nil {
		queryParams["max_delay"] = *maxDelay
	}

	if useReplayTimestamp != nil {
		queryParams["use_replay_timestamp"] = *useReplayTimestamp
	}

	if product != nil {
		queryParams["product"] = *product
	}

	if runParallel != nil {
		queryParams["run_parallel"] = *runParallel
	}

	params := make([]string, len(queryParams))
	var count int
	for key, value := range queryParams {
		var arg string
		switch val := value.(type) {
		case string:
			arg = val
		case int:
			arg = strconv.Itoa(val)
		case bool:
			arg = strconv.FormatBool(val)
		}

		params[count] = fmt.Sprintf("%s=%s", key, arg)
	}

	query := strings.Join(params, "&")

	path := "/replay/play"
	if len(query) != 0 {
		path = fmt.Sprintf("%s?%s", path, query)
	}

	res, err := c.do(http.MethodPost, path)
	if err != nil {
		return false, err
	}
	_ = res.Body.Close()

	return true, nil
}

// Open for async processing
func (c *Client) Open() <-chan protocols.Response {
	c.msgCh = make(chan protocols.Response)

	return c.msgCh
}

// SubscribeWithAPIObserver for sync processing
func (c *Client) SubscribeWithAPIObserver(apiObserver Observer) {
	c.lock.Lock()
	defer c.lock.Unlock()

	c.observers = append(c.observers, apiObserver)
}

// Close ...
func (c *Client) Close() {
	c.lock.Lock()
	defer c.lock.Unlock()

	c.closed = true
	c.observers = nil

	if c.msgCh != nil {
		close(c.msgCh)
	}
}

func (c *Client) makeRequest(path, method string) (*http.Request, error) {
	basePath, err := c.cfg.APIURL()
	if err != nil {
		return nil, err
	}

	path = "https://" + basePath + "/" + apiVersion + path
	request, err := http.NewRequest(method, path, nil)
	if err != nil {
		return nil, err
	}
	request.Header["accept"] = []string{"application/xml"}
	request.Header["x-access-token"] = []string{*c.cfg.AccessToken()}

	return request, nil
}

func (c *Client) fetchData(path string, entity interface{}, locale *protocols.Locale) error {
	resp, err := c.do(http.MethodGet, path)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	err = xml.NewDecoder(resp.Body).Decode(entity)
	if err != nil {
		return err
	}

	respWithCode, ok := entity.(protocols.ResponseWithCode)
	if ok && respWithCode.Code() != protocols.OkResponseCode {
		return fmt.Errorf("not acceptable response code from API: %s", respWithCode.Code())
	}

	apiResponse := protocols.Response{
		Data:   entity,
		URL:    resp.Request.URL,
		Locale: locale,
	}

	c.lock.RLock()
	defer c.lock.RUnlock()

	if c.msgCh != nil && !c.closed {
		c.msgCh <- apiResponse
	}

	for _, observer := range c.observers {
		observer.OnAPIResponse(apiResponse)
	}

	return nil
}

func (c *Client) do(method, path string) (*http.Response, error) {
	callback := func() (*http.Response, error) {
		req, err := c.makeRequest(path, method)
		if err != nil {
			return nil, err
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, err
		}

		return resp, nil
	}

	var resp *http.Response
	var err error
	for i := 0; i < retryCount; i++ {
		resp, err = callback()
		if err != nil {
			return nil, err
		}

		var respOk bool
		switch {
		case resp.StatusCode == http.StatusOK,
			// Server side errors - don't retry
			resp.StatusCode >= 400 && resp.StatusCode < 500:
			respOk = true

			// Probably infrastructure error - retry
		case resp.StatusCode >= 500:
			time.Sleep(retryDelay)
		}

		if respOk {
			break
		}
	}

	if resp.StatusCode != http.StatusOK {
		apiErr, err := c.unmarshallPossibleError(resp.Body)
		// This means no parsable error in request
		if err != nil {
			return nil, fmt.Errorf("failed to %s data to server with status code %d", method, resp.StatusCode)
		}

		return nil, fmt.Errorf("api server returned err - %s", apiErr.Message)
	}

	return resp, nil
}

func (c *Client) unmarshallPossibleError(r io.Reader) (*data.Error, error) {
	var apiError data.Error
	err := xml.NewDecoder(r).Decode(&apiError)
	if err != nil {
		return nil, err
	}

	return &apiError, nil
}

// New ...
func New(cfg protocols.OddsFeedConfiguration) *Client {
	return &Client{
		cfg:       cfg,
		observers: make([]Observer, 0),
		httpClient: http.Client{
			Timeout: timeoutSeconds * time.Second,
		},
	}
}
