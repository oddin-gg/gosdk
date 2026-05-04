package api

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v5"
	data "github.com/oddin-gg/gosdk/internal/api/xml"
	"github.com/oddin-gg/gosdk/types"
)

const (
	apiVersion         = "v1"
	timeLayout         = "2006-01-02"
	defaultHTTPTimeout = 30 * time.Second
	defaultMaxRetries  = 3
	initialRetryDelay  = 500 * time.Millisecond
	maxRetryDelay      = 5 * time.Second
)

// Observer ...
type Observer interface {
	OnAPIResponse(apiResponse types.Response)
}

// APIEvent describes a single HTTP attempt the API client made: the
// successful response, a 4xx terminal error, or a transport failure.
// One event per attempt — retries produce multiple events with
// incrementing Attempt numbers. URL is redacted to scheme://host/path
// (no query string) so tokens in querystrings never reach observers.
type APIEvent struct {
	At        time.Time
	Method    string
	URL       string
	Status    int // 0 on transport-level errors (no HTTP response)
	Latency   time.Duration
	Attempt   int
	Locale    *types.Locale
	Request   []byte // empty unless EventCapture.RequestBody is set
	Response  []byte // empty unless EventCapture.ResponseBody is set
	Truncated bool
	Err       error
}

// APIEventEmitter is invoked synchronously from inside do() with one
// event per HTTP attempt. The emitter MUST NOT block — gosdk wraps the
// callback with a select+default lossy push.
type APIEventEmitter func(APIEvent)

// EventCapture tunes which payload bytes flow into APIEvents.
type EventCapture struct {
	Emit         APIEventEmitter
	RequestBody  bool
	ResponseBody bool
	BodyLimit    int // > 0 caps captured body size; <=0 disables capture
}

// Client ...
type Client struct {
	cfg         types.OddsFeedConfiguration
	httpClient  *http.Client
	logger      *slog.Logger
	maxRetries  uint
	mu          sync.RWMutex
	msgCh       chan types.Response
	observers   []Observer
	capture     EventCapture
	closed      bool
}

// New constructs an API client. Pass a nil logger to fall back to slog.Default().
func New(cfg types.OddsFeedConfiguration) *Client {
	return NewWithLogger(cfg, nil)
}

// NewWithLogger constructs an API client with a caller-provided slog.Logger.
func NewWithLogger(cfg types.OddsFeedConfiguration, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		cfg:        cfg,
		logger:     logger,
		httpClient: &http.Client{Timeout: defaultHTTPTimeout},
		maxRetries: defaultMaxRetries,
		observers:  make([]Observer, 0),
	}
}

// Open enables async API-response streaming via the returned channel.
// Used by the cache layer; will be retired in Phase 6.
func (c *Client) Open() <-chan types.Response {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgCh = make(chan types.Response)
	return c.msgCh
}

// SubscribeWithAPIObserver registers an observer that is called synchronously
// for every successful API response. Used by the cache layer.
func (c *Client) SubscribeWithAPIObserver(o Observer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.observers = append(c.observers, o)
}

// SetHTTPClient replaces the underlying http.Client. Used by gosdk's
// WithHTTPClient option (custom TLS, transport instrumentation) and by
// tests. Must be called before the first request.
func (c *Client) SetHTTPClient(h *http.Client) {
	if h == nil {
		return
	}
	c.mu.Lock()
	c.httpClient = h
	c.mu.Unlock()
}

// SetEventCapture installs the APIEvent emission callback. Pass a zero
// EventCapture to disable.
func (c *Client) SetEventCapture(ec EventCapture) {
	c.mu.Lock()
	c.capture = ec
	c.mu.Unlock()
}

// Close releases observer slots and closes the response channel if Open was called.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	c.observers = nil
	if c.msgCh != nil {
		close(c.msgCh)
	}
}

// FetchWhoAmI ...
func (c *Client) FetchWhoAmI(ctx context.Context) (*data.WhoAMI, error) {
	var resp data.WhoAMI
	err := c.fetchData(ctx, "/users/whoami", &resp, nil)
	return &resp, err
}

// FetchProducers ...
func (c *Client) FetchProducers(ctx context.Context) ([]data.Producer, error) {
	var resp data.ProducersResponse
	if err := c.fetchData(ctx, "/descriptions/producers", &resp, nil); err != nil {
		return nil, err
	}
	return resp.Producers, nil
}

// FetchSports ...
func (c *Client) FetchSports(ctx context.Context, locale types.Locale) ([]data.Sport, error) {
	var resp data.SportsResponse
	if err := c.fetchData(ctx, fmt.Sprintf("/sports/%s/sports", locale), &resp, &locale); err != nil {
		return nil, err
	}
	return resp.Sports, nil
}

// FetchMatchStatusDescriptions ...
func (c *Client) FetchMatchStatusDescriptions(ctx context.Context, locale types.Locale) ([]data.MatchStatus, error) {
	var resp data.MatchStatusDescriptionResponse
	if err := c.fetchData(ctx, fmt.Sprintf("/descriptions/%s/match_status", locale), &resp, &locale); err != nil {
		return nil, err
	}
	return resp.MatchStatus, nil
}

// FetchFixtureChanges ...
func (c *Client) FetchFixtureChanges(ctx context.Context, locale types.Locale, after time.Time) ([]data.FixtureChange, error) {
	path := fmt.Sprintf("/sports/%s/fixtures/changes", locale)
	if !after.IsZero() {
		path = fmt.Sprintf("%s?after=%d", path, after.UnixNano()/1e6)
	}
	var resp data.FixtureChangesResponse
	if err := c.fetchData(ctx, path, &resp, &locale); err != nil {
		return nil, err
	}
	return resp.Changes, nil
}

// FetchFixture ...
func (c *Client) FetchFixture(ctx context.Context, id types.URN, locale types.Locale) (*data.Fixture, error) {
	var resp data.FixtureResponse
	if err := c.fetchData(ctx, fmt.Sprintf("/sports/%s/sport_events/%s/fixture", locale, id.ToString()), &resp, &locale); err != nil {
		return nil, err
	}
	return &resp.Fixture, nil
}

// FetchSchedule ...
func (c *Client) FetchSchedule(ctx context.Context, startIndex, limit uint, locale types.Locale) ([]data.SportEvent, error) {
	var resp data.ScheduleResponse
	if err := c.fetchData(ctx, fmt.Sprintf("/sports/%s/schedules/pre/schedule?start=%d&limit=%d", locale, startIndex, limit), &resp, &locale); err != nil {
		return nil, err
	}
	return resp.SportEvents, nil
}

// FetchTournaments ...
func (c *Client) FetchTournaments(ctx context.Context, sportID types.URN, locale types.Locale) ([]data.Tournament, error) {
	var resp data.SportTournamentsResponse
	if err := c.fetchData(ctx, fmt.Sprintf("/sports/%s/sports/%s/tournaments", locale, sportID.ToString()), &resp, &locale); err != nil {
		return nil, err
	}
	if resp.Tournaments == nil {
		return nil, nil
	}
	return resp.Tournaments.Tournament, nil
}

// FetchTournament ...
func (c *Client) FetchTournament(ctx context.Context, id types.URN, locale types.Locale) (*data.TournamentExtended, error) {
	var resp data.SportTournamentInfoResponse
	if err := c.fetchData(ctx, fmt.Sprintf("/sports/%s/tournaments/%s/info", locale, id.ToString()), &resp, &locale); err != nil {
		return nil, err
	}
	return &resp.Tournament, nil
}

// FetchCompetitorProfile ...
func (c *Client) FetchCompetitorProfile(ctx context.Context, id types.URN, locale types.Locale) (*data.TeamExtended, error) {
	resp, err := c.FetchCompetitorProfileWithPlayers(ctx, id, locale)
	if err != nil {
		return nil, err
	}
	return &resp.Competitor, nil
}

// FetchCompetitorProfileWithPlayers ...
func (c *Client) FetchCompetitorProfileWithPlayers(ctx context.Context, id types.URN, locale types.Locale) (*data.CompetitorResponse, error) {
	var resp data.CompetitorResponse
	if err := c.fetchData(ctx, fmt.Sprintf("/sports/%s/competitors/%s/profile", locale, id.ToString()), &resp, &locale); err != nil {
		return nil, err
	}
	return &resp, nil
}

// FetchMatchSummary ...
func (c *Client) FetchMatchSummary(ctx context.Context, id types.URN, locale types.Locale) (*data.MatchSummaryResponse, error) {
	var resp data.MatchSummaryResponse
	if err := c.fetchData(ctx, fmt.Sprintf("/sports/%s/sport_events/%s/summary", locale, id.ToString()), &resp, &locale); err != nil {
		return nil, err
	}
	return &resp, nil
}

// FetchLiveMatches ...
func (c *Client) FetchLiveMatches(ctx context.Context, locale types.Locale) ([]data.SportEvent, error) {
	var resp data.ScheduleResponse
	if err := c.fetchData(ctx, fmt.Sprintf("/sports/%s/schedules/live/schedule", locale), &resp, &locale); err != nil {
		return nil, err
	}
	return resp.SportEvents, nil
}

// FetchMatches ...
func (c *Client) FetchMatches(ctx context.Context, t time.Time, locale types.Locale) ([]data.SportEvent, error) {
	var resp data.ScheduleResponse
	if err := c.fetchData(ctx, fmt.Sprintf("/sports/%s/schedules/%s/schedule", locale, t.Format(timeLayout)), &resp, &locale); err != nil {
		return nil, err
	}
	return resp.SportEvents, nil
}

// FetchMarketDescriptions ...
func (c *Client) FetchMarketDescriptions(ctx context.Context, locale types.Locale) ([]data.MarketDescription, error) {
	var resp data.MarketDescriptionResponse
	if err := c.fetchData(ctx, fmt.Sprintf("/descriptions/%s/markets", locale), &resp, &locale); err != nil {
		return nil, err
	}
	return resp.Markets, nil
}

// FetchMarketDescriptionsWithDynamicOutcomes ...
func (c *Client) FetchMarketDescriptionsWithDynamicOutcomes(
	ctx context.Context,
	marketTypeID uint,
	marketVariant string,
	locale types.Locale,
) ([]data.MarketDescription, error) {
	var resp data.MarketDescriptionResponse
	if err := c.fetchData(
		ctx,
		fmt.Sprintf("/descriptions/%s/markets/%d/variants/%s", locale, marketTypeID, marketVariant),
		&resp,
		&locale,
	); err != nil {
		return nil, err
	}
	return resp.Markets, nil
}

// FetchMarketVoidReasons ...
func (c *Client) FetchMarketVoidReasons(ctx context.Context) ([]data.MarketVoidReasons, error) {
	var resp data.MarketVoidReasonsResponse
	if err := c.fetchData(ctx, "/descriptions/void_reasons", &resp, nil); err != nil {
		return nil, err
	}
	return resp.VoidReasons, nil
}

// FetchPlayerProfile ...
func (c *Client) FetchPlayerProfile(ctx context.Context, playerID string, locale types.Locale) (*data.PlayerProfile, error) {
	var resp data.PlayerProfile
	if err := c.fetchData(ctx, fmt.Sprintf("/sports/%s/players/%s/profile", locale, playerID), &resp, &locale); err != nil {
		return nil, err
	}
	return &resp, nil
}

// PostEventStatefulRecovery ...
func (c *Client) PostEventStatefulRecovery(ctx context.Context, producerName string, eventID types.URN, requestID uint, nodeID *int) (bool, error) {
	path := fmt.Sprintf("/%s/stateful_messages/events/%s/initiate_request?request_id=%d", producerName, eventID.ToString(), requestID)
	if nodeID != nil {
		path = fmt.Sprintf("%s&node_id=%d", path, *nodeID)
	}
	return c.postEmpty(ctx, path)
}

// PostEventOddsRecovery ...
func (c *Client) PostEventOddsRecovery(ctx context.Context, producerName string, eventID types.URN, requestID uint, nodeID *int) (bool, error) {
	path := fmt.Sprintf("/%s/odds/events/%s/initiate_request?request_id=%d", producerName, eventID.ToString(), requestID)
	if nodeID != nil {
		path = fmt.Sprintf("%s&node_id=%d", path, *nodeID)
	}
	return c.postEmpty(ctx, path)
}

// PostRecovery ...
func (c *Client) PostRecovery(ctx context.Context, producerName string, requestID uint, nodeID *int, after time.Time) (bool, error) {
	path := fmt.Sprintf("/%s/recovery/initiate_request?request_id=%d", producerName, requestID)
	if nodeID != nil {
		path = fmt.Sprintf("%s&node_id=%d", path, *nodeID)
	}
	if !after.IsZero() {
		path = fmt.Sprintf("%s&after=%d", path, after.UnixNano()/1e6)
	}
	return c.postEmpty(ctx, path)
}

// PostReplayClear ...
func (c *Client) PostReplayClear(ctx context.Context, nodeID *int) (bool, error) {
	path := "/replay/clear"
	if nodeID != nil {
		path = fmt.Sprintf("%s?node_id=%d", path, *nodeID)
	}
	return c.postEmpty(ctx, path)
}

// PostReplayStop ...
func (c *Client) PostReplayStop(ctx context.Context, nodeID *int) (bool, error) {
	path := "/replay/stop"
	if nodeID != nil {
		path = fmt.Sprintf("%s?node_id=%d", path, *nodeID)
	}
	return c.postEmpty(ctx, path)
}

// FetchReplaySetContent ...
func (c *Client) FetchReplaySetContent(ctx context.Context, nodeID *int) ([]data.ReplayEvent, error) {
	path := "/replay"
	if nodeID != nil {
		path = fmt.Sprintf("%s?node_id=%d", path, *nodeID)
	}
	var resp data.ReplayResponse
	if err := c.fetchData(ctx, path, &resp, nil); err != nil {
		return nil, err
	}
	return resp.SportEvents, nil
}

// PutReplayEvent ...
func (c *Client) PutReplayEvent(ctx context.Context, eventID types.URN, nodeID *int) (bool, error) {
	path := fmt.Sprintf("/replay/events/%s", eventID.ToString())
	if nodeID != nil {
		path = fmt.Sprintf("%s?node_id=%d", path, *nodeID)
	}
	return c.doNoBody(ctx, http.MethodPut, path)
}

// DeleteReplayEvent ...
func (c *Client) DeleteReplayEvent(ctx context.Context, eventID types.URN, nodeID *int) (bool, error) {
	path := fmt.Sprintf("/replay/events/%s", eventID.ToString())
	if nodeID != nil {
		path = fmt.Sprintf("%s?node_id=%d", path, *nodeID)
	}
	return c.doNoBody(ctx, http.MethodDelete, path)
}

// PostReplayStart sends /replay/play with optional query parameters built
// from the supplied pointers. Each non-nil pointer becomes a single
// query-string entry.
func (c *Client) PostReplayStart(
	ctx context.Context,
	nodeID *int,
	speed *int,
	maxDelay *int,
	useReplayTimestamp *bool,
	product *string,
	runParallel *bool,
) (bool, error) {
	q := url.Values{}
	if nodeID != nil {
		q.Set("node_id", strconv.Itoa(*nodeID))
	}
	if speed != nil {
		q.Set("speed", strconv.Itoa(*speed))
	}
	if maxDelay != nil {
		q.Set("max_delay", strconv.Itoa(*maxDelay))
	}
	if useReplayTimestamp != nil {
		q.Set("use_replay_timestamp", strconv.FormatBool(*useReplayTimestamp))
	}
	if product != nil {
		q.Set("product", *product)
	}
	if runParallel != nil {
		q.Set("run_parallel", strconv.FormatBool(*runParallel))
	}

	path := "/replay/play"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	return c.postEmpty(ctx, path)
}

// fetchData performs a GET, decodes the XML body into entity, and broadcasts
// the response to observers and the optional Open() channel.
func (c *Client) fetchData(ctx context.Context, path string, entity interface{}, locale *types.Locale) error {
	resp, err := c.do(ctx, http.MethodGet, path)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if err := xml.NewDecoder(resp.Body).Decode(entity); err != nil {
		return fmt.Errorf("api: decode %s: %w", path, err)
	}

	if rwc, ok := entity.(types.ResponseWithCode); ok && rwc.Code() != types.OkResponseCode {
		return fmt.Errorf("api: not acceptable response code from %s: %s", path, rwc.Code())
	}

	apiResponse := types.Response{
		Data:   entity,
		URL:    resp.Request.URL,
		Locale: locale,
	}

	// Snapshot observers + msgCh under read-lock, then send outside the lock so
	// a slow consumer never blocks other API calls.
	c.mu.RLock()
	closed := c.closed
	msgCh := c.msgCh
	observers := c.observers
	c.mu.RUnlock()

	if !closed {
		if msgCh != nil {
			select {
			case msgCh <- apiResponse:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		for _, o := range observers {
			o.OnAPIResponse(apiResponse)
		}
	}
	return nil
}

// postEmpty sends a POST and discards the response body.
func (c *Client) postEmpty(ctx context.Context, path string) (bool, error) {
	return c.doNoBody(ctx, http.MethodPost, path)
}

// doNoBody runs a request that returns no useful body and just returns success.
func (c *Client) doNoBody(ctx context.Context, method, path string) (bool, error) {
	resp, err := c.do(ctx, method, path)
	if err != nil {
		return false, err
	}
	_ = resp.Body.Close()
	return true, nil
}

// do executes an HTTP request against the API host with retry on transient
// failures (network errors and 5xx). Successful responses are returned with
// an open Body that the caller MUST close. 4xx responses are decoded as
// API error payloads and returned as wrapped errors (no retry).
func (c *Client) do(ctx context.Context, method, path string) (*http.Response, error) {
	var (
		resp     *http.Response
		attempts uint
	)
	op := func() (*http.Response, error) {
		attempts++

		// Close any leftover body from a previous (transient-failure) attempt.
		if resp != nil {
			_ = resp.Body.Close()
			resp = nil
		}

		req, err := c.makeRequest(ctx, method, path)
		if err != nil {
			// Construction failures are not retryable.
			return nil, backoff.Permanent(err)
		}

		started := time.Now()
		r, err := c.httpClient.Do(req)
		if err != nil {
			// Network error: retry unless the context is canceled.
			if ctx.Err() != nil {
				c.emitEvent(req, 0, time.Since(started), int(attempts), nil, ctx.Err())
				return nil, backoff.Permanent(ctx.Err())
			}
			c.logger.Debug("api: request failed, will retry",
				slog.String("method", method),
				slog.String("path", path),
				slog.Uint64("attempt", uint64(attempts)),
				slog.String("error", err.Error()),
			)
			c.emitEvent(req, 0, time.Since(started), int(attempts), nil, err)
			return nil, err
		}

		latency := time.Since(started)
		c.logger.Debug("api: response",
			slog.String("method", method),
			slog.String("path", path),
			slog.Int("status", r.StatusCode),
			slog.Int64("latency_ms", latency.Milliseconds()),
			slog.Uint64("attempt", uint64(attempts)),
		)

		switch {
		case r.StatusCode == http.StatusOK:
			// Capture response body bytes when requested. We tee the body
			// into a buffer and replace r.Body so the downstream xml
			// decoder still works.
			respBytes, truncated, ok := c.captureBody(r)
			if ok {
				c.emitEventBytes(req, r.StatusCode, latency, int(attempts), respBytes, truncated, nil)
			} else {
				c.emitEvent(req, r.StatusCode, latency, int(attempts), nil, nil)
			}
			resp = r
			return r, nil
		case r.StatusCode >= 400 && r.StatusCode < 500:
			// Client error — read body, decode, and return permanent error.
			respBytes, truncated, _ := c.captureBody(r)
			err := c.toAPIError(method, path, r)
			_ = r.Body.Close()
			c.emitEventBytes(req, r.StatusCode, latency, int(attempts), respBytes, truncated, err)
			return nil, backoff.Permanent(err)
		default:
			// Server error or unexpected status — retry.
			respBytes, truncated, _ := c.captureBody(r)
			_ = r.Body.Close()
			retryErr := fmt.Errorf("api: %s %s: status %d", method, path, r.StatusCode)
			c.emitEventBytes(req, r.StatusCode, latency, int(attempts), respBytes, truncated, retryErr)
			return nil, retryErr
		}
	}

	exp := backoff.NewExponentialBackOff()
	exp.InitialInterval = initialRetryDelay
	exp.MaxInterval = maxRetryDelay
	exp.RandomizationFactor = 0.3

	r, err := backoff.Retry(ctx, op,
		backoff.WithBackOff(exp),
		backoff.WithMaxTries(c.maxRetries),
	)
	if err != nil {
		return nil, err
	}
	return r, nil
}

// captureBody slurps r.Body up to the configured limit when response
// capture is enabled, replacing r.Body with a re-readable buffer so the
// downstream decoder still works. Returns (bytes, truncated, captured).
// captured=false means the body was left untouched (no capture configured).
func (c *Client) captureBody(r *http.Response) ([]byte, bool, bool) {
	c.mu.RLock()
	cap := c.capture
	c.mu.RUnlock()
	if cap.Emit == nil || !cap.ResponseBody || cap.BodyLimit <= 0 || r.Body == nil {
		return nil, false, false
	}
	limit := cap.BodyLimit
	// Read up to limit+1 bytes so we can detect overflow without slurping
	// arbitrarily large payloads into memory.
	buf := make([]byte, limit+1)
	n, _ := io.ReadFull(r.Body, buf)
	rest, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()
	combined := append(buf[:n], rest...)
	truncated := len(combined) > limit
	captured := combined
	if truncated {
		captured = combined[:limit]
	}
	r.Body = io.NopCloser(bytes.NewReader(combined))
	return captured, truncated, true
}

// emitEvent fires a metadata-only APIEvent (no bytes).
func (c *Client) emitEvent(req *http.Request, status int, latency time.Duration, attempt int, locale *types.Locale, err error) {
	c.emitEventBytes(req, status, latency, attempt, nil, false, err)
}

// emitEventBytes fires an APIEvent including the supplied response body
// bytes. Snapshots the emitter under RLock so a concurrent SetEventCapture
// race doesn't deliver to a torn callback.
func (c *Client) emitEventBytes(req *http.Request, status int, latency time.Duration, attempt int, body []byte, truncated bool, err error) {
	c.mu.RLock()
	emit := c.capture.Emit
	c.mu.RUnlock()
	if emit == nil {
		return
	}
	emit(APIEvent{
		At:        time.Now(),
		Method:    req.Method,
		URL:       redactURL(req.URL),
		Status:    status,
		Latency:   latency,
		Attempt:   attempt,
		Response:  body,
		Truncated: truncated,
		Err:       err,
	})
}

// redactURL strips query strings before emitting events. The X-Access-Token
// is sent in a header (already not in the URL), but query strings can carry
// other sensitive identifiers (e.g., recovery `request_id`, replay node
// scoping) and we keep them out of observability streams by default.
func redactURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	if u.Host == "" && u.Scheme == "" {
		return u.Path
	}
	return u.Scheme + "://" + u.Host + u.Path
}

// toAPIError decodes the body of a non-2xx response into a structured error
// when possible, otherwise returns a generic wrapped error.
func (c *Client) toAPIError(method, path string, r *http.Response) error {
	apiErr, decodeErr := c.unmarshallPossibleError(r.Body)
	if decodeErr != nil {
		return fmt.Errorf("api: %s %s: status %d", method, path, r.StatusCode)
	}
	return fmt.Errorf("api: %s %s: status %d: %s", method, path, r.StatusCode, apiErr.Message)
}

// makeRequest builds an absolute request URL from the configured API host and
// attaches the access token using canonicalized headers.
func (c *Client) makeRequest(ctx context.Context, method, path string) (*http.Request, error) {
	basePath, err := c.cfg.APIURL()
	if err != nil {
		return nil, err
	}

	full := "https://" + basePath + "/" + apiVersion + path
	req, err := http.NewRequestWithContext(ctx, method, full, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/xml")
	if tok := c.cfg.AccessToken(); tok != nil {
		req.Header.Set("X-Access-Token", *tok)
	}
	return req, nil
}

func (c *Client) unmarshallPossibleError(r io.Reader) (*data.Error, error) {
	var apiError data.Error
	if err := xml.NewDecoder(r).Decode(&apiError); err != nil {
		return nil, err
	}
	return &apiError, nil
}

// errCanceled is exposed for tests that want to recognize the wrapped
// context.Canceled error returned from the retry path.
var errCanceled = errors.New("canceled")

func init() {
	// Silence the unused-error warning if the test helpers don't reference it.
	_ = errCanceled
}
