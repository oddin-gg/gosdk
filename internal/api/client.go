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
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v5"

	data "github.com/oddin-gg/gosdk/internal/api/xml"
	"github.com/oddin-gg/gosdk/internal/config"
	"github.com/oddin-gg/gosdk/internal/sdkerr"
	"github.com/oddin-gg/gosdk/internal/utils"
	"github.com/oddin-gg/gosdk/internal/version"
	"github.com/oddin-gg/gosdk/types"
)

const (
	apiVersion         = "v1"
	timeLayout         = "2006-01-02"
	defaultHTTPTimeout = 30 * time.Second
	// defaultMaxAttempts is the TOTAL attempt budget (1 initial + 2
	// retries) — backoff/v5's WithMaxTries counts attempts, not
	// retries. Named "attempts" so nobody re-reads it as retry count.
	defaultMaxAttempts = 3
	initialRetryDelay  = 500 * time.Millisecond
	maxRetryDelay      = 5 * time.Second
	// maxErrorBodyBytes caps how much of a non-2xx body is read for the
	// structured-error decode. Error envelopes are tiny; the cap defends
	// against a misbehaving server streaming an arbitrarily large body.
	maxErrorBodyBytes = 1 << 20
)

// maxSuccessBodyBytes caps how much of a SUCCESSFUL response body the
// XML decoder may consume (via byteLimitReader). Catalog responses are
// single-digit MBs at most; the cap defends memory against a
// compromised or misconfigured trusted endpoint streaming an unbounded
// 2xx body. Package variable so tests can shrink it.
var maxSuccessBodyBytes int64 = 32 << 20

// byteLimitReader passes reads through until more than `left` bytes
// have been consumed, then fails the stream with an explicit error
// (NOT io.EOF — a silent EOF would truncate-decode).
type byteLimitReader struct {
	r    io.Reader
	left int64
}

func (b *byteLimitReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.left -= int64(n)
	if b.left < 0 {
		return n, fmt.Errorf("api: response body exceeds %d-byte cap", maxSuccessBodyBytes)
	}
	return n, err
}

// Observer ...
type Observer interface {
	OnAPIResponse(apiResponse Response)
}

// APIEvent describes a single HTTP attempt the API client made: the
// successful response, a 4xx terminal error, or a transport failure.
// One event per attempt — retries produce multiple events with
// incrementing Attempt numbers. URL is redacted to scheme://host/path
// (no query string) so tokens in querystrings never reach observers.
type APIEvent struct {
	At      time.Time
	Method  string
	URL     string
	Status  int // 0 on transport-level errors (no HTTP response)
	Latency time.Duration
	Attempt int
	Locale  *types.Locale
	// Request is the (redacted, length-bounded) request body. Empty
	// unless EventCapture.RequestBody is set. Currently empty for every
	// SDK API call — all paths are bodyless GET / doNoBody POST/PUT —
	// but populated automatically for any future endpoint that sends a
	// request body.
	Request          []byte
	RequestTruncated bool
	// Response is the (redacted, length-bounded) response body. Empty
	// unless EventCapture.ResponseBody is set.
	Response  []byte
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
	cfg        config.Config
	httpClient *http.Client
	logger     *slog.Logger
	// maxAttempts is the total per-call attempt budget (initial try
	// included). Values <= 0 fall back to defaultMaxAttempts — a zero
	// must never reach backoff.WithMaxTries, where it means unlimited.
	maxAttempts int
	mu          sync.RWMutex
	observers   []Observer
	capture     EventCapture
	closed      bool

	// lifeCtx is the client's lifetime: every request derives its ctx
	// from it (via lifeCancel), so Close cancels in-flight HTTP requests
	// regardless of the caller's own ctx. inflight tracks those requests
	// so Close can join them before returning — an entry spans the FULL
	// public call including lazy response-body consumption (released via
	// do()'s cleanup: at do()-return on failure, at Body.Close on
	// success), so the join can't report completion while a decode is
	// still reading.
	lifeCtx    context.Context
	lifeCancel context.CancelFunc
	inflight   sync.WaitGroup
}

// ErrClosed is returned by request methods invoked after Close. It
// WRAPS the shared closed-client sentinel (sdkerr.ErrClosed ==
// gosdk.ErrAlreadyClosed), so consumers can classify post-Close
// catalog/entity failures with errors.Is against the public sentinel
// instead of string-matching an unexported internal error.
var ErrClosed = fmt.Errorf("api: client is closed: %w", sdkerr.ErrClosed)

// New constructs an API client with default per-request HTTP timeout.
// Use SetHTTPClient to override the http.Client (e.g. for tests).
func New(cfg config.Config) *Client {
	return NewWithLogger(cfg, nil, 0)
}

// NewWithLogger constructs an API client with a caller-provided slog.Logger
// and per-request HTTP timeout. timeout ≤ 0 falls back to defaultHTTPTimeout.
func NewWithLogger(cfg config.Config, logger *slog.Logger, timeout time.Duration) *Client {
	if logger == nil {
		logger = slog.Default()
	}
	if timeout <= 0 {
		timeout = defaultHTTPTimeout
	}
	lifeCtx, lifeCancel := context.WithCancel(context.Background())
	return &Client{
		cfg:         cfg,
		logger:      logger,
		httpClient:  guardRedirects(&http.Client{Timeout: timeout}),
		maxAttempts: defaultMaxAttempts,
		observers:   make([]Observer, 0),
		lifeCtx:     lifeCtx,
		lifeCancel:  lifeCancel,
	}
}

// guardRedirects returns a shallow COPY of h whose CheckRedirect refuses
// any cross-origin or scheme-downgrading redirect before delegating to
// the caller's original policy for permitted same-origin hops.
//
// Every request carries the access token in the custom X-Access-Token
// header. net/http strips only a fixed allowlist of sensitive headers
// (Authorization, WWW-Authenticate, Cookie, Cookie2) when a redirect
// changes origin — a CUSTOM header is copied across every hop, including
// to a different authority. With the default policy (CheckRedirect ==
// nil, follow up to 10) a 30x to another host would hand the credential
// to that host. makeRequest validates only the INITIAL host, so the
// redirect chain is the gap. Copying (not mutating) keeps a
// caller-supplied client's own CheckRedirect intact on their object.
func guardRedirects(h *http.Client) *http.Client {
	if h == nil {
		return nil
	}
	hc := *h
	userPolicy := h.CheckRedirect
	hc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > 0 {
			origin := via[0].URL
			if req.URL.Scheme != origin.Scheme || req.URL.Host != origin.Host {
				return fmt.Errorf("api: refusing redirect from %s://%s to %s://%s — a cross-origin or downgraded redirect would disclose the access token",
					origin.Scheme, origin.Host, req.URL.Scheme, req.URL.Host)
			}
		}
		// Same-origin: honor the caller's policy if any, else replicate
		// net/http's default 10-hop cap.
		if userPolicy != nil {
			return userPolicy(req, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return &hc
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
// tests. Safe for concurrent use — do() snapshots the client under the
// same lock per attempt — though in practice it is called once, before
// the first request.
func (c *Client) SetHTTPClient(h *http.Client) {
	if h == nil {
		return
	}
	c.mu.Lock()
	// Wrap with the cross-origin redirect guard (copy, never mutate the
	// caller's client — see guardRedirects).
	c.httpClient = guardRedirects(h)
	c.mu.Unlock()
}

// SetEventCapture installs the APIEvent emission callback. Pass a zero
// EventCapture to disable.
func (c *Client) SetEventCapture(ec EventCapture) {
	c.mu.Lock()
	c.capture = ec
	c.mu.Unlock()
}

// Close marks the client closed, cancels the client lifetime (aborting
// every in-flight request regardless of the caller's ctx), and JOINS
// those requests before returning. After Close, request methods return
// ErrClosed. Idempotent. Unbounded join — use CloseCtx from paths that
// carry a shutdown budget.
func (c *Client) Close() {
	c.CloseCtx(context.Background())
}

// CloseCtx is Close with the in-flight join BOUNDED by ctx. In-flight
// requests observe the cancelled lifeCtx and unwind promptly when the
// transport honours ctx cancellation — but a custom transport that
// ignores it could otherwise pin the join indefinitely, blowing the
// caller's shutdown budget. Reports whether the join actually completed;
// on false the joining goroutine leaks harmlessly until the stuck
// request finally unwinds (everything it waits on was cancelled).
func (c *Client) CloseCtx(ctx context.Context) bool {
	c.mu.Lock()
	c.closed = true
	c.observers = nil
	lifeCancel := c.lifeCancel
	c.mu.Unlock()

	if lifeCancel != nil {
		lifeCancel() // idempotent
	}
	// Bounded join — a repeat Close during a still-draining join waits
	// (bounded) again rather than falsely reporting completion.
	done := make(chan struct{})
	go func() {
		c.inflight.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		// Completion-first accounting (see gosdk.waitBounded): with both
		// arms ready — or a drained WaitGroup whose waiter goroutine
		// hasn't run — the ctx arm could falsely report an incomplete
		// join. Yield and re-check before reporting failure.
		runtime.Gosched()
		select {
		case <-done:
			return true
		default:
			return false
		}
	}
}

// pathSeg escapes a dynamic value for use as a single URL path segment,
// so reserved characters ('/', '?', '#', '%', spaces, …) can't alter the
// request path or query ("path traversal"/query-injection via a crafted
// URN, locale, or API-provided producer name). Values composed only of
// segment-legal characters — ordinary URNs like "od:match:123" (':' is
// legal in a path segment), locales, producer names — pass through
// byte-identical.
func pathSeg[T ~string](v T) string { return url.PathEscape(string(v)) }

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
	if err := c.fetchData(ctx, fmt.Sprintf("/sports/%s/sports", pathSeg(locale)), &resp, &locale); err != nil {
		return nil, err
	}
	return resp.Sports, nil
}

// FetchMatchStatusDescriptions ...
func (c *Client) FetchMatchStatusDescriptions(ctx context.Context, locale types.Locale) ([]data.MatchStatus, error) {
	var resp data.MatchStatusDescriptionResponse
	if err := c.fetchData(ctx, fmt.Sprintf("/descriptions/%s/match_status", pathSeg(locale)), &resp, &locale); err != nil {
		return nil, err
	}
	return resp.MatchStatus, nil
}

// FetchFixtureChanges ...
func (c *Client) FetchFixtureChanges(ctx context.Context, locale types.Locale, after time.Time) ([]data.FixtureChange, error) {
	path := fmt.Sprintf("/sports/%s/fixtures/changes", pathSeg(locale))
	if !after.IsZero() {
		path = fmt.Sprintf("%s?after=%d", path, after.UnixMilli())
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
	path := fmt.Sprintf("/sports/%s/sport_events/%s/fixture", pathSeg(locale), pathSeg(id.ToString()))
	if err := c.fetchDataValidated(ctx, path, &resp, &locale, func() error {
		return requireID("fixture", resp.Fixture.ID, id.ToString())
	}); err != nil {
		return nil, err
	}
	return &resp.Fixture, nil
}

// requireID enforces response identity: the decoded document must carry
// a non-empty id equal to the requested one (see fetchDataValidated).
func requireID(what, got, want string) error {
	switch {
	case got == "":
		return fmt.Errorf("%s response carries no id (want %s)", what, want)
	case got != want:
		return fmt.Errorf("%s response is for %q, requested %q", what, got, want)
	}
	return nil
}

// FetchSchedule ...
func (c *Client) FetchSchedule(ctx context.Context, startIndex, limit int, locale types.Locale) ([]data.SportEvent, error) {
	var resp data.ScheduleResponse
	if err := c.fetchData(ctx, fmt.Sprintf("/sports/%s/schedules/pre/schedule?start=%d&limit=%d", pathSeg(locale), startIndex, limit), &resp, &locale); err != nil {
		return nil, err
	}
	return resp.SportEvents, nil
}

// FetchTournaments ...
func (c *Client) FetchTournaments(ctx context.Context, sportID types.URN, locale types.Locale) ([]data.Tournament, error) {
	var resp data.SportTournamentsResponse
	path := fmt.Sprintf("/sports/%s/sports/%s/tournaments", pathSeg(locale), pathSeg(sportID.ToString()))
	if err := c.fetchDataValidated(ctx, path, &resp, &locale, func() error {
		// Validate the top-level <sport> identity FIRST. It is required by
		// the wire schema and is the ONLY identity an empty tournament
		// list carries: without it, a 2xx response for a different sport
		// (empty list → the nested loop runs zero times) silently
		// satisfied this request and was cached as this sport's
		// authoritative empty tournament list, hiding its real
		// tournaments until invalidation. The nested per-tournament check
		// stays as defense in depth.
		if err := requireID("sport-tournaments sport", resp.Sport.ID, sportID.ToString()); err != nil {
			return err
		}
		if resp.Tournaments == nil {
			return nil
		}
		for _, t := range resp.Tournaments.Tournament {
			if err := requireID("tournament-list sport", t.Sport.ID, sportID.ToString()); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
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
	path := fmt.Sprintf("/sports/%s/tournaments/%s/info", pathSeg(locale), pathSeg(id.ToString()))
	if err := c.fetchDataValidated(ctx, path, &resp, &locale, func() error {
		return requireID("tournament", resp.Tournament.ID, id.ToString())
	}); err != nil {
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
	path := fmt.Sprintf("/sports/%s/competitors/%s/profile", pathSeg(locale), pathSeg(id.ToString()))
	if err := c.fetchDataValidated(ctx, path, &resp, &locale, func() error {
		return requireID("competitor", resp.Competitor.ID, id.ToString())
	}); err != nil {
		return nil, err
	}
	return &resp, nil
}

// FetchMatchSummary ...
func (c *Client) FetchMatchSummary(ctx context.Context, id types.URN, locale types.Locale) (*data.MatchSummaryResponse, error) {
	var resp data.MatchSummaryResponse
	path := fmt.Sprintf("/sports/%s/sport_events/%s/summary", pathSeg(locale), pathSeg(id.ToString()))
	if err := c.fetchDataValidated(ctx, path, &resp, &locale, func() error {
		// Identity check BEFORE observer dispatch is load-bearing here:
		// the match-status observer stores under the RESPONSE's embedded
		// id, so a misrouted summary for B stored status under B while
		// the caller cached the match under A — internally inconsistent
		// snapshots from one response.
		if err := requireID("match summary", resp.SportEvent.ID, id.ToString()); err != nil {
			return err
		}
		// Validate the OPTIONAL winner_id at the response boundary, next to
		// the match identity. A malformed winner URN otherwise failed only
		// later inside the match-status observer, which drops the response
		// and leaves the MatchStatus loader returning ErrItemNotFound —
		// turning malformed upstream data into the (non-retryable)
		// definitive-absence classification. Surfacing it as a response
		// validation error keeps that distinction honest.
		if w := resp.SportEventStatus.WinnerID; w != nil {
			if _, err := types.ParseURN(*w); err != nil {
				return fmt.Errorf("match summary %s: malformed winner_id %q: %w", id.ToString(), *w, err)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return &resp, nil
}

// FetchLiveMatches ...
func (c *Client) FetchLiveMatches(ctx context.Context, locale types.Locale) ([]data.SportEvent, error) {
	var resp data.ScheduleResponse
	if err := c.fetchData(ctx, fmt.Sprintf("/sports/%s/schedules/live/schedule", pathSeg(locale)), &resp, &locale); err != nil {
		return nil, err
	}
	return resp.SportEvents, nil
}

// FetchMatches ...
func (c *Client) FetchMatches(ctx context.Context, t time.Time, locale types.Locale) ([]data.SportEvent, error) {
	var resp data.ScheduleResponse
	if err := c.fetchData(ctx, fmt.Sprintf("/sports/%s/schedules/%s/schedule", pathSeg(locale), t.Format(timeLayout)), &resp, &locale); err != nil {
		return nil, err
	}
	return resp.SportEvents, nil
}

// FetchMarketDescriptions ...
func (c *Client) FetchMarketDescriptions(ctx context.Context, locale types.Locale) ([]data.MarketDescription, error) {
	var resp data.MarketDescriptionResponse
	if err := c.fetchData(ctx, fmt.Sprintf("/descriptions/%s/markets", pathSeg(locale)), &resp, &locale); err != nil {
		return nil, err
	}
	return resp.Markets, nil
}

// FetchMarketDescriptionsWithDynamicOutcomes ...
func (c *Client) FetchMarketDescriptionsWithDynamicOutcomes(
	ctx context.Context,
	marketTypeID int,
	marketVariant string,
	locale types.Locale,
) ([]data.MarketDescription, error) {
	var resp data.MarketDescriptionResponse
	if err := c.fetchData(
		ctx,
		// Variant strings originate from feed messages — escape so a
		// value containing '?', '#', '/' or a space can't misroute the
		// request or fail URL parsing.
		fmt.Sprintf("/descriptions/%s/markets/%d/variants/%s", pathSeg(locale), marketTypeID, pathSeg(marketVariant)),
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

// FetchPlayerProfile fetches one player profile and validates the
// response IDENTITY before returning it: the decoded player must carry
// a non-empty id equal to the requested one. Pre-fix, a missing or
// mismatched player decoded into a (possibly zero-valued) profile that
// the cache stored as successful data under the requested key.
func (c *Client) FetchPlayerProfile(ctx context.Context, playerID string, locale types.Locale) (*data.PlayerProfile, error) {
	var resp data.PlayerProfile
	path := fmt.Sprintf("/sports/%s/players/%s/profile", pathSeg(locale), pathSeg(playerID))
	if err := c.fetchDataValidated(ctx, path, &resp, &locale, func() error {
		return requireID("player profile", resp.Player.ID, playerID)
	}); err != nil {
		return nil, err
	}
	return &resp, nil
}

// PostEventStatefulRecovery ...
//
// Idempotent: the request_id query parameter lets the server dedupe a
// retry against a previously-processed request.
func (c *Client) PostEventStatefulRecovery(ctx context.Context, producerName string, eventID types.URN, requestID int, nodeID *int) (bool, error) {
	path := fmt.Sprintf("/%s/stateful_messages/events/%s/initiate_request?request_id=%d", pathSeg(producerName), pathSeg(eventID.ToString()), requestID)
	if nodeID != nil {
		path = fmt.Sprintf("%s&node_id=%d", path, *nodeID)
	}
	return c.postEmpty(ctx, path, true)
}

// PostEventOddsRecovery ...
//
// Idempotent: request_id-keyed.
func (c *Client) PostEventOddsRecovery(ctx context.Context, producerName string, eventID types.URN, requestID int, nodeID *int) (bool, error) {
	path := fmt.Sprintf("/%s/odds/events/%s/initiate_request?request_id=%d", pathSeg(producerName), pathSeg(eventID.ToString()), requestID)
	if nodeID != nil {
		path = fmt.Sprintf("%s&node_id=%d", path, *nodeID)
	}
	return c.postEmpty(ctx, path, true)
}

// PostRecovery ...
//
// Idempotent: request_id-keyed.
func (c *Client) PostRecovery(ctx context.Context, producerName string, requestID int, nodeID *int, after time.Time) (bool, error) {
	path := fmt.Sprintf("/%s/recovery/initiate_request?request_id=%d", pathSeg(producerName), requestID)
	if nodeID != nil {
		path = fmt.Sprintf("%s&node_id=%d", path, *nodeID)
	}
	if !after.IsZero() {
		path = fmt.Sprintf("%s&after=%d", path, after.UnixMilli())
	}
	return c.postEmpty(ctx, path, true)
}

// PostReplayClear ...
//
// Non-idempotent: no dedupe key. A 5xx after server processed the
// request must NOT retry — that would re-clear the queue.
func (c *Client) PostReplayClear(ctx context.Context, nodeID *int) (bool, error) {
	path := "/replay/clear"
	if nodeID != nil {
		path = fmt.Sprintf("%s?node_id=%d", path, *nodeID)
	}
	return c.postEmpty(ctx, path, false)
}

// PostReplayStop ...
//
// Non-idempotent: no dedupe key.
func (c *Client) PostReplayStop(ctx context.Context, nodeID *int) (bool, error) {
	path := "/replay/stop"
	if nodeID != nil {
		path = fmt.Sprintf("%s?node_id=%d", path, *nodeID)
	}
	return c.postEmpty(ctx, path, false)
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

// FetchReplayStatus reports the current state of the replay engine for
// the configured (or supplied) node. Surfaces the GET /replay/status
// REST endpoint — the .NET SDK exposes this via IReplayManager;
// Java's SportsInfoManager doesn't currently expose replay status
// publicly, so this is API-endpoint parity rather than full
// cross-SDK parity.
func (c *Client) FetchReplayStatus(ctx context.Context, nodeID *int) (string, error) {
	path := "/replay/status"
	if nodeID != nil {
		path = fmt.Sprintf("%s?node_id=%d", path, *nodeID)
	}
	var resp data.ReplayStatusResponse
	if err := c.fetchData(ctx, path, &resp, nil); err != nil {
		return "", err
	}
	return resp.Status, nil
}

// PutReplayEvent ...
//
// Non-idempotent at the HTTP layer: even though "PUT this event into
// the replay queue" is conceptually idempotent, the server has no
// dedupe key and a 5xx returned after the server processed could
// otherwise be retried into a duplicate-queued event.
func (c *Client) PutReplayEvent(ctx context.Context, eventID types.URN, nodeID *int) (bool, error) {
	path := fmt.Sprintf("/replay/events/%s", pathSeg(eventID.ToString()))
	if nodeID != nil {
		path = fmt.Sprintf("%s?node_id=%d", path, *nodeID)
	}
	return c.doNoBody(ctx, http.MethodPut, path, false)
}

// DeleteReplayEvent ...
//
// Non-idempotent at the HTTP layer: a 5xx after server processed
// must not retry.
func (c *Client) DeleteReplayEvent(ctx context.Context, eventID types.URN, nodeID *int) (bool, error) {
	path := fmt.Sprintf("/replay/events/%s", pathSeg(eventID.ToString()))
	if nodeID != nil {
		path = fmt.Sprintf("%s?node_id=%d", path, *nodeID)
	}
	return c.doNoBody(ctx, http.MethodDelete, path, false)
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
	// Non-idempotent: replay-play has no dedupe key.
	return c.postEmpty(ctx, path, false)
}

// fetchData performs a GET, decodes the XML body into entity, and broadcasts
// the response to observers and the optional Open() channel.
func (c *Client) fetchData(ctx context.Context, path string, entity interface{}, locale *types.Locale) error {
	return c.fetchDataValidated(ctx, path, entity, locale, nil)
}

// fetchDataValidated is fetchData with a post-decode IDENTITY validator
// that runs BEFORE observer dispatch. Entity endpoints must verify that
// the decoded document is actually about the requested resource: a
// stale or misrouted 2xx response for B while requesting A previously
// flowed straight into the observers — contaminating A's cache with B's
// data, and (for match summaries) simultaneously storing status under
// B — before the caller could check anything. A validation failure is
// the call's error: no observer sees the response, the APIEvent carries
// the cause.
func (c *Client) fetchDataValidated(ctx context.Context, path string, entity interface{}, locale *types.Locale, validate func() error) error {
	// Captured BEFORE the request goes out: observers use it against
	// their clear tombstones (see Response.StartedAt).
	startedAt := time.Now()
	resp, pc, err := c.do(ctx, http.MethodGet, path, locale, true /* idempotent: GET */)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	// Resolve the call's final error BEFORE emitting so the APIEvent
	// carries it. Pre-fix, a decoded-but-not-OK response_code envelope
	// emitted Err=nil — indistinguishable from success on the
	// observability stream while the caller received an error.
	var callErr error
	dec := xml.NewDecoder(&byteLimitReader{r: resp.Body, left: maxSuccessBodyBytes})
	if decodeErr := dec.Decode(entity); decodeErr != nil {
		callErr = fmt.Errorf("api: decode %s: %w", path, decodeErr)
	} else if eofErr := utils.EnsureXMLEOF(dec); eofErr != nil {
		// A trailing second document previously decoded "successfully"
		// as the first root — reject it before envelope/identity checks
		// and observer dispatch.
		callErr = fmt.Errorf("api: decode %s: %w", path, eofErr)
	} else if rwc, ok := entity.(ResponseWithCode); ok && rwc.Code() != types.OkResponseCode {
		callErr = &Error{Method: http.MethodGet, Path: path, Status: resp.StatusCode, Code: rwc.Code()}
	}
	if callErr == nil && validate != nil {
		// Identity validation BEFORE observer dispatch — see
		// fetchDataValidated. A failure must stop the response from
		// reaching any cache-populating observer below.
		if verr := validate(); verr != nil {
			callErr = fmt.Errorf("api: %s: response identity: %w", path, verr)
		}
	}
	c.emit(pc, callErr) // streaming capture: emit AFTER body has flowed through tee
	if callErr != nil {
		return callErr
	}

	apiResponse := Response{
		Data:      entity,
		URL:       resp.Request.URL,
		Locale:    locale,
		StartedAt: startedAt,
	}

	// Snapshot observers under read-lock, then dispatch outside the
	// lock so a slow observer never blocks other API calls.
	c.mu.RLock()
	closed := c.closed
	observers := c.observers
	c.mu.RUnlock()

	if !closed {
		for _, o := range observers {
			o.OnAPIResponse(apiResponse)
		}
	}
	return nil
}

// postEmpty sends a POST whose 5xx semantics depend on idempotency.
// Recovery POSTs (request_id-keyed, server dedupes) pass true. Replay
// POSTs (no dedupe key, observable side-effects) pass false.
func (c *Client) postEmpty(ctx context.Context, path string, idempotent bool) (bool, error) {
	return c.doNoBody(ctx, http.MethodPost, path, idempotent)
}

// doNoBody runs a request that returns no useful body and just returns success.
// idempotent toggles 5xx-retry: see do() for full semantics.
func (c *Client) doNoBody(ctx context.Context, method, path string, idempotent bool) (bool, error) {
	resp, pc, err := c.do(ctx, method, path, nil, idempotent)
	if err != nil {
		return false, err
	}
	// Read the (small, bounded) body through the tee so the capture
	// buffer fills. A read failure does not fail the call — the 2xx
	// status line was already received, so the server processed the
	// request — but it is surfaced on the event stream and debug log
	// instead of being silently discarded.
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, int64(maxErrorBodyBytes)+1))
	_ = resp.Body.Close()
	if readErr != nil {
		// A mid-body read failure means acceptance is UNVERIFIED: the
		// body could have carried a rejection envelope we never saw.
		// Report it as an error (not success) so recovery does not hand
		// out a live handle and replay does not claim acceptance — pre-
		// fix this returned (true, nil) and a truncated 200 FORBIDDEN
		// read as success. Idempotent callers can retry; the event still
		// fires.
		c.logger.Debug("api: response body unreadable; acceptance unverified",
			slog.String("method", method),
			slog.String("path", path),
			slog.String("error", readErr.Error()),
		)
		err := fmt.Errorf("api: %s %s: 2xx response body unreadable, acceptance unverified: %w", method, path, readErr)
		c.emit(pc, err)
		return false, err
	}
	// A 2xx status does NOT by itself mean acceptance: recovery and
	// replay mutations can answer 200 with a REJECTION envelope such as
	// `<response response_code="FORBIDDEN">…</response>`. Pre-fix the
	// body was drained blind and the call reported success — recovery
	// returned a live handle for a rejected request, and replay
	// mutations falsely reported acceptance.
	//
	// An OVERSIZED body (over the cap, detected via the +1 read) cannot
	// be verified either — a padded or truncated rejection envelope
	// would slip through. Treat it the same as a read failure: unverified
	// acceptance, reported as an error.
	if len(body) > maxErrorBodyBytes {
		err := fmt.Errorf("api: %s %s: 2xx response body exceeds %d bytes, rejection envelope could not be verified", method, path, maxErrorBodyBytes)
		c.emit(pc, err)
		return false, err
	}
	// Classify the body. Empty (202/204 / empty OK) is accepted; a
	// well-formed <response> envelope must carry a RECOGNIZED SUCCESS
	// code (OK/CREATED/ACCEPTED) to count as accepted; anything else on a
	// <response> root is a rejection. A well-formed NON-<response> body
	// is some other payload and stays accepted; a body that does not
	// fully decode (truncated, malformed, or with trailing documents) is
	// UNVERIFIED and fails.
	if apiErr, ok := c.classifyMutationBody(method, path, resp.StatusCode, body); !ok {
		c.emit(pc, apiErr)
		return false, fmt.Errorf("api: %s %s: %w", method, path, apiErr)
	}
	c.emit(pc, nil)
	return true, nil
}

// isSuccessResponseCode reports whether a <response> envelope code means
// the mutation was accepted. Centralized so every 2xx classifier agrees.
func isSuccessResponseCode(c types.ResponseCode) bool {
	switch c {
	case types.OkResponseCode, types.CreatedResponseCode, types.AcceptedResponseCode:
		return true
	default:
		return false
	}
}

// classifyMutationBody decides whether a 2xx mutation body represents an
// accepted operation. Returns (nil, true) on acceptance; (*Error, false)
// on rejection or an unverifiable body. See the caller for the policy.
func (c *Client) classifyMutationBody(method, path string, status int, body []byte) (*Error, bool) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, true // empty 202/204 / empty OK
	}
	// Attempt to decode as the <response> envelope. data.Error pins
	// XMLName to "response", so Decode errors when the root is anything
	// else — that path is handled below.
	dec := xml.NewDecoder(bytes.NewReader(body))
	var env data.Error
	if decErr := dec.Decode(&env); decErr == nil {
		// It IS a <response> root. Require it to decode COMPLETELY with
		// nothing trailing — a second document or stray content means we
		// cannot trust the classification.
		if eofErr := utils.EnsureXMLEOF(dec); eofErr != nil {
			return &Error{Method: method, Path: path, Status: status,
				Message: "2xx <response> envelope with trailing content; acceptance unverified"}, false
		}
		code := types.ResponseCode(env.Code)
		if isSuccessResponseCode(code) {
			return nil, true
		}
		// Empty or non-success code on a <response> root → rejection.
		return &Error{
			Method:  method,
			Path:    path,
			Status:  status,
			Code:    code,
			Message: string(c.redactSensitive([]byte(env.Message))),
		}, false
	}
	// A bare <error>…</error> root is the API's OTHER defined error shape
	// (see xml.ErrorBody). A 2xx carrying it means the mutation was NOT
	// accepted — recognize it explicitly as a rejection. Pre-fix it fell
	// into the "well-formed → success" branch below, so a rejected replay/
	// recovery request reported success: snapshot recovery then waited
	// forever for a completion that never arrives, and event-recovery
	// handles stayed pending until their (up to 6h) timeout.
	if root, ok := xmlRootLocal(body); ok && root == "error" {
		var env data.ErrorBody
		_ = xml.Unmarshal(body, &env) // best-effort; message is diagnostic
		return &Error{
			Method:  method,
			Path:    path,
			Status:  status,
			Code:    types.ResponseCode(env.Code),
			Message: string(c.redactSensitive([]byte(env.Message))),
		}, false
	}
	// Not a <response>/<error> root. If the body is otherwise well-formed
	// XML it is some other success payload; if it does not parse at all it
	// is a truncated/malformed body whose acceptance cannot be verified.
	if wellFormedXML(body) {
		return nil, true
	}
	return &Error{Method: method, Path: path, Status: status,
		Message: "2xx body is malformed/truncated; acceptance unverified"}, false
}

// xmlRootLocal returns the local name of the first XML start element in
// body (the document root), or ("", false) if body is not well-formed XML
// or has no element. Used to recognize the API's <error> rejection shape
// without pinning a decode struct's XMLName.
func xmlRootLocal(body []byte) (string, bool) {
	dec := xml.NewDecoder(bytes.NewReader(body))
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", false
		}
		if start, ok := tok.(xml.StartElement); ok {
			return start.Name.Local, true
		}
	}
}

// wellFormedXML reports whether body parses as XML all the way to EOF.
func wellFormedXML(body []byte) bool {
	dec := xml.NewDecoder(bytes.NewReader(body))
	for {
		_, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return true
		}
		if err != nil {
			return false
		}
	}
}

// do executes an HTTP request against the API host with retry on transient
// failures. Successful responses are returned with an open Body that the
// caller MUST close. 4xx responses are decoded as API error payloads and
// returned as wrapped errors (no retry).
//
// idempotent governs retry semantics for 5xx AND transport failures:
//   - true: both are retried (GETs, recovery POSTs that carry request_id
//     so the server dedupes).
//   - false: both are terminal (no retry). Used for replay POST/PUT/DELETE
//     which carry no dedupe key — a failure surfaced after the server
//     processed the request would otherwise re-execute on retry. This
//     includes transport errors: http.Client.Timeout firing while
//     awaiting response headers, or a reset while reading the response,
//     both occur AFTER the request was fully sent.
//
// 429 is the exception on both policies: the server rate-limited the
// request without processing it, so it is always retried.
//
// On HTTP 200 the body is wrapped in an `io.TeeReader → capBuf` so the
// decoder fills the capture buffer as a side-effect of parsing. The
// returned *pendingCapture must be passed to c.emit(pc, decodeErr)
// after the caller has finished consuming r.Body — failure to call
// emit will silently drop the APIEvent.
//
// For non-OK statuses do() emits the event inline (those bodies are
// always small enough not to need streaming) and returns pc=nil.
func (c *Client) do(ctx context.Context, method, path string, locale *types.Locale, idempotent bool) (*http.Response, *pendingCapture, error) {
	// Reject new requests after Close, and register this one as in-flight
	// — both under mu so the check and the WaitGroup.Add can't straddle a
	// concurrent Close (which sets closed under mu BEFORE Wait).
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, nil, ErrClosed
	}
	c.inflight.Add(1)
	c.mu.Unlock()

	// Derive the request ctx from the caller's ctx AND the client
	// lifetime: Close cancels lifeCtx, aborting this request even if the
	// caller passed a ctx that never fires (e.g. context.Background()).
	reqCtx, cancelReq := context.WithCancel(ctx)
	//nolint:contextcheck // lifeCtx is the client-lifetime cancellation root; AfterFunc wires it to abort this request, it is not a request-scoped parent
	stopLife := context.AfterFunc(c.lifeCtx, cancelReq)
	ctx = reqCtx

	// Request cleanup (stop the lifeCtx relay, cancel reqCtx, release the
	// in-flight slot) must fire on every non-success return — but NOT when
	// do() returns an open 2xx body: the body is read lazily by the caller
	// (fetchData's XML decoder streams straight off the socket) and an
	// http response body is bound to its request ctx, so cancelling here
	// would abort any read that outruns the transport's receive buffer
	// with "context canceled" (real catalog-sized payloads, invisible to
	// small-body tests). The in-flight WaitGroup entry lives in the same
	// scope: Close's join must cover the public API call INCLUDING body
	// consumption — releasing it at do()-return would let CloseCtx report
	// "all requests joined" while a decode is still reading a body whose
	// transport ignores cancellation. On success, cleanup ownership
	// transfers to Body.Close via the cancelOnClose wrapper at the
	// bottom; sync.Once keeps the two paths from double-firing.
	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			stopLife()
			cancelReq()
			c.inflight.Done()
		})
	}
	bodyOwnsCleanup := false
	defer func() {
		if !bodyOwnsCleanup {
			cleanup()
		}
	}()

	var (
		pc       *pendingCapture
		attempts int
	)
	op := func() (*http.Response, error) {
		attempts++

		// Defensive reset: pc is only assigned on the terminal success
		// path today, but a future error-after-installCapture edit must
		// not leak a stale capture into the returned value.
		pc = nil

		req, err := c.makeRequest(ctx, method, path)
		if err != nil {
			// Construction failures are not retryable.
			return nil, backoff.Permanent(err)
		}

		// Per-attempt request body capture. No-op for every current SDK
		// API path (all bodyless), wired through for future endpoints.
		// Captured before httpClient.Do consumes req.Body; req.GetBody
		// is restored so retries can replay.
		reqBody, reqTruncated := c.captureRequestBytes(req)

		// Snapshot the http.Client under lock: SetHTTPClient writes it
		// under c.mu, and reading it bare here made that lock theater —
		// a swap after the first request was a data race.
		c.mu.RLock()
		httpClient := c.httpClient
		c.mu.RUnlock()

		started := time.Now()
		r, err := httpClient.Do(req)
		if err != nil {
			// Transport error: no usable HTTP response.
			if ctx.Err() != nil {
				c.emitEvent(req, 0, time.Since(started), attempts, locale, reqBody, reqTruncated, ctx.Err())
				return nil, backoff.Permanent(fmt.Errorf("api: %s %s attempt=%d: %w", method, path, attempts, ctx.Err()))
			}
			if !idempotent {
				// A transport error does NOT prove the server never
				// processed the request: http.Client.Timeout firing while
				// awaiting response headers, or a connection reset while
				// reading the response, both land here after the request
				// was fully sent — and neither cancels the request ctx,
				// so the guard above doesn't catch them. For requests
				// without a dedupe key, re-issuing risks a duplicate
				// side effect — same policy as the 5xx branch below.
				c.emitEvent(req, 0, time.Since(started), attempts, locale, reqBody, reqTruncated, err)
				return nil, backoff.Permanent(fmt.Errorf("api: %s %s attempt=%d: %w", method, path, attempts, err))
			}
			c.logger.Debug("api: request failed, will retry",
				slog.String("method", method),
				slog.String("path", path),
				slog.Uint64("attempt", uint64(attempts)),
				slog.String("error", err.Error()),
			)
			c.emitEvent(req, 0, time.Since(started), attempts, locale, reqBody, reqTruncated, err)
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
		case r.StatusCode >= 200 && r.StatusCode < 300:
			// Any 2xx is success — pinning this to exactly 200 would
			// route 201/202/204 into the server-error branch below and
			// retry them. (Betradar-style feeds answer some recovery
			// initiations with 202 Accepted.)
			//
			// Defer emission: install a TeeReader-backed capture, return the
			// pendingCapture, and have the caller (fetchData / doNoBody)
			// invoke emit() after consuming the body. Streaming = no full-
			// body materialization regardless of payload size.
			pc = c.installCapture(r, req, r.StatusCode, latency, attempts, locale, reqBody, reqTruncated)
			return r, nil
		case r.StatusCode == http.StatusTooManyRequests:
			// 429: the server rate-limited the request WITHOUT processing
			// it, so retrying is safe under both idempotency policies —
			// unlike other 4xx (terminal) and unlike 5xx for non-idempotent
			// requests. Pacing comes from the exponential schedule; the
			// Retry-After header is deliberately not mapped onto
			// backoff/v5's RetryAfterError, which carries no cause and
			// would degrade the terminal error on exhaustion.
			parserBytes, eventBytes, truncated := c.readErrorBody(r)
			_ = r.Body.Close()
			retryErr := c.toAPIError(method, path, r.StatusCode, parserBytes)
			c.emitEventReqResp(req, r.StatusCode, latency, attempts, reqBody, reqTruncated, eventBytes, truncated, locale, retryErr)
			return nil, retryErr
		case r.StatusCode >= 400 && r.StatusCode < 500:
			// Client error — read the body ONCE; reuse for both the
			// structured-error decode and the APIEvent capture.
			parserBytes, eventBytes, truncated := c.readErrorBody(r)
			err := c.toAPIError(method, path, r.StatusCode, parserBytes)
			_ = r.Body.Close()
			c.emitEventReqResp(req, r.StatusCode, latency, attempts, reqBody, reqTruncated, eventBytes, truncated, locale, err)
			return nil, backoff.Permanent(err)
		default:
			// Server error or unexpected status. Body read once and
			// decoded the same way 4xx is — pre-fix 5xx errors only
			// included the status code, dropping the server's error
			// envelope (`<error><message>...</message></error>`)
			// entirely while 4xx surfaced it.
			parserBytes, eventBytes, truncated := c.readErrorBody(r)
			_ = r.Body.Close()
			retryErr := c.toAPIError(method, path, r.StatusCode, parserBytes)
			c.emitEventReqResp(req, r.StatusCode, latency, attempts, reqBody, reqTruncated, eventBytes, truncated, locale, retryErr)
			if !idempotent {
				// Server may have processed the request — re-issuing
				// would re-execute. Treat 5xx as terminal.
				return nil, backoff.Permanent(retryErr)
			}
			return nil, retryErr
		}
	}

	exp := backoff.NewExponentialBackOff()
	exp.InitialInterval = initialRetryDelay
	exp.MaxInterval = maxRetryDelay
	exp.RandomizationFactor = 0.3

	tries := c.maxAttempts
	if tries <= 0 {
		// backoff/v5 treats WithMaxTries(0) as UNLIMITED attempts —
		// never let a zero value through the uint conversion.
		tries = defaultMaxAttempts
	}
	r, err := backoff.Retry(ctx, op,
		backoff.WithBackOff(exp),
		backoff.WithMaxTries(uint(tries)),
	)
	if err != nil {
		// Retry exhaustion / Permanent: include the actual attempt
		// count so the user can tell a 4-attempt cascade from a
		// single transient hiccup. The leaf-attempt error already
		// carries method+path+status (toAPIError) or method+path+
		// ctx context (transport-error path); double-prefix is
		// acceptable for the diagnostic value.
		return nil, nil, fmt.Errorf("api: %s %s: gave up after %d attempt(s): %w", method, path, attempts, err)
	}
	// Success: transfer ctx-cleanup ownership to the returned body (see
	// the comment above). The caller MUST close it — doing so releases
	// both the connection and the request-ctx resources.
	r.Body = &cancelOnClose{ReadCloser: r.Body, cleanup: cleanup}
	bodyOwnsCleanup = true
	return r, pc, nil
}

// cancelOnClose releases the request-scoped resources (lifeCtx
// AfterFunc relay, reqCtx cancel, in-flight WaitGroup slot) when the
// caller closes a successful response body. do() hands the open body to
// its caller for lazy decoding, so the request scope must outlive do()
// itself — cancelling the ctx at do() return aborted streamed reads
// mid-decode, and releasing the in-flight slot there let Close report
// a completed join while a decode was still consuming a body.
type cancelOnClose struct {
	io.ReadCloser
	cleanup func()
}

func (b *cancelOnClose) Close() error {
	err := b.ReadCloser.Close()
	b.cleanup()
	return err
}

// readErrorBody reads the full 4xx/5xx body once and returns both the
// parser-facing bytes (used by toAPIError to decode the structured
// error envelope) and the event-facing bytes (capped at BodyLimit,
// decoupled, redacted). Reading once avoids the double-consumption
// bug where readErrorBody consumed r.Body and toAPIError then saw an
// empty stream — losing the server's error message.
//
// Streaming isn't needed here — error bodies are always small.
func (c *Client) readErrorBody(r *http.Response) (parserBytes []byte, eventBytes []byte, truncated bool) {
	if r.Body == nil {
		return nil, nil, false
	}
	// Read ONE byte past the internal cap so an oversized body is
	// detectable: reading exactly maxErrorBodyBytes made a 3 MiB body
	// with a 2 MiB configured event limit report Truncated=false (only
	// 1 MiB was retained, and the configured-limit comparison alone
	// couldn't see the internal-cap overflow).
	body, readErr := io.ReadAll(io.LimitReader(r.Body, maxErrorBodyBytes+1))
	if readErr != nil {
		// Keep whatever arrived — a partial envelope may still decode,
		// and the capture retains diagnostic value either way.
		c.logger.Debug("api: error-body read failed",
			slog.Int("status", r.StatusCode),
			slog.String("error", readErr.Error()),
		)
	}
	internalOverflow := len(body) > maxErrorBodyBytes
	if internalOverflow {
		body = body[:maxErrorBodyBytes]
	}
	parserBytes = body

	c.mu.RLock()
	capture := c.capture
	c.mu.RUnlock()
	if capture.Emit == nil || !capture.ResponseBody || capture.BodyLimit <= 0 {
		return parserBytes, nil, false
	}
	limit := capture.BodyLimit
	truncated = internalOverflow || len(body) > limit
	capLen := len(body)
	if capLen > limit {
		capLen = limit
	}
	eventBytes = make([]byte, capLen)
	copy(eventBytes, body[:capLen])
	eventBytes = c.redactCapture(eventBytes, truncated)
	return parserBytes, eventBytes, truncated
}

// capBuf is the io.Writer used inside the response-capture TeeReader.
// It accumulates up to `limit` bytes in `buf`; once the cap is reached
// further bytes are silently discarded, and `truncated` becomes true.
//
// The decoder reads through the TeeReader, so the body is captured as
// a side-effect of the actual XML parse — no separate ReadAll, no
// peak-memory blast even on multi-MB responses.
type capBuf struct {
	limit     int
	buf       []byte
	truncated bool
}

func (b *capBuf) Write(p []byte) (int, error) {
	if b.truncated {
		return len(p), nil
	}
	room := b.limit - len(b.buf)
	if len(p) <= room {
		b.buf = append(b.buf, p...)
		return len(p), nil
	}
	b.buf = append(b.buf, p[:room]...)
	b.truncated = true
	return len(p), nil
}

// pendingCapture carries the in-flight event metadata across the
// install-on-OK / emit-after-decode boundary. Callers who receive a
// non-nil pendingCapture from do() MUST call emit(pc, err) after they
// finish consuming r.Body.
//
// reqBody / reqTruncated are populated by captureRequestBytes when
// EventCapture.RequestBody is enabled and the request had a body.
// These flow into APIEvent.Request via emit(). All current SDK call
// sites use bodyless requests (GET, doNoBody POST/PUT) so this stays
// empty in practice — wired through so future endpoints with bodies
// are captured automatically.
type pendingCapture struct {
	req          *http.Request
	status       int
	latency      time.Duration
	attempts     int
	locale       *types.Locale
	buf          *capBuf // nil when ResponseBody capture is disabled
	reqBody      []byte
	reqTruncated bool
}

// installCapture wraps r.Body with a TeeReader → capBuf so the
// downstream decoder fills the capture buffer as a side-effect of
// parsing. Returns a pendingCapture the caller must hand to emit()
// after the body is consumed. Returns (nil, false) when capture is
// disabled — caller still owns r.Body but no event will be emitted.
//
// When ResponseBody is disabled we still return a pendingCapture (with
// buf=nil) so the caller emits a metadata-only event.
//
// reqBody / reqTruncated are forwarded onto the pendingCapture from
// the per-attempt request capture (see captureRequestBytes). Empty
// for the bodyless requests every current SDK API path uses.
func (c *Client) installCapture(r *http.Response, req *http.Request, status int, latency time.Duration, attempts int, locale *types.Locale, reqBody []byte, reqTruncated bool) *pendingCapture {
	c.mu.RLock()
	capture := c.capture
	c.mu.RUnlock()
	if capture.Emit == nil {
		return nil
	}
	pc := &pendingCapture{
		req: req, status: status, latency: latency, attempts: attempts, locale: locale,
		reqBody: reqBody, reqTruncated: reqTruncated,
	}
	if capture.ResponseBody && capture.BodyLimit > 0 && r.Body != nil {
		pc.buf = &capBuf{limit: capture.BodyLimit}
		body := r.Body
		r.Body = struct {
			io.Reader
			io.Closer
		}{
			Reader: io.TeeReader(body, pc.buf),
			Closer: body,
		}
	}
	return pc
}

// captureRequestBytes drains req.Body into a redacted, length-bounded
// snapshot for APIEvent.Request, then restores req.Body so the http
// transport can still send it. Sets req.GetBody so retries can replay.
//
// Returns (nil, false) when capture is disabled or the request is
// bodyless (every current SDK API path is bodyless — this is wired
// through for future endpoints that send XML/JSON request bodies).
func (c *Client) captureRequestBytes(req *http.Request) ([]byte, bool) {
	c.mu.RLock()
	capture := c.capture
	c.mu.RUnlock()
	if !capture.RequestBody || capture.BodyLimit <= 0 || req.Body == nil {
		return nil, false
	}
	raw, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		return nil, false
	}
	// Restore req.Body + GetBody so the transport can send it (and
	// resend on retry). Keep an independent copy for the captured
	// snapshot so redaction doesn't mutate the bytes we hand to net/http.
	stored := append([]byte(nil), raw...)
	req.Body = io.NopCloser(bytes.NewReader(stored))
	req.ContentLength = int64(len(stored))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(stored)), nil
	}
	truncated := false
	captured := raw
	if len(captured) > capture.BodyLimit {
		captured = captured[:capture.BodyLimit]
		truncated = true
	}
	return c.redactCapture(captured, truncated), truncated
}

// emit dispatches the queued APIEvent after the response body has been
// consumed. err is the decode/post-decode error (if any).
func (c *Client) emit(pc *pendingCapture, err error) {
	if pc == nil {
		return
	}
	var body []byte
	var truncated bool
	if pc.buf != nil {
		// Decouple captured slice from the tee buffer so consumers
		// don't pin the underlying backing array. Cheap copy bounded
		// by BodyLimit.
		body = make([]byte, len(pc.buf.buf))
		copy(body, pc.buf.buf)
		truncated = pc.buf.truncated
		body = c.redactCapture(body, truncated)
	}
	c.emitEventReqResp(pc.req, pc.status, pc.latency, pc.attempts, pc.reqBody, pc.reqTruncated, body, truncated, pc.locale, err)
}

// redactSensitive scrubs the configured access token from the captured
// bytes — the raw form AND its common wire encodings: an XML response
// that echoes the token character-escapes &, <, >, ', " (so the raw
// bytes never appear and an exact raw-only match would miss it), and a
// URL rendered into an error or body carries the query-escaped form.
// Returns the (possibly modified) slice — buf is the caller's local
// copy, so in-place mutation is safe.
//
// Every non-empty token is redacted, whatever its length. The public
// config contract requires only a non-empty token, and the pre-fix
// 16-byte floor let a server response or error that echoes a shorter
// token reach opt-in APIEvent observers unsanitized — violating the
// "captured bodies are always sanitized" promise (NEXT.md §10). A very
// short token can over-scrub coincidental substrings in the capture;
// that costs diagnostics fidelity, never correctness or secrecy.
func (c *Client) redactSensitive(buf []byte) []byte {
	if len(buf) == 0 {
		return buf
	}
	tok := c.cfg.AccessToken()
	if tok == nil || *tok == "" {
		return buf
	}
	redacted := []byte(redactedToken)
	for _, form := range tokenWireForms(*tok) {
		buf = bytes.ReplaceAll(buf, form, redacted)
	}
	return buf
}

// redactedToken is the marker substituted for the access token (and its
// truncation-split fragments) in captured bodies and event errors.
const redactedToken = "[REDACTED]"

// tokenWireForms returns the byte patterns under which the access token
// can appear on the wire: raw, XML character-escaped, and URL
// query-escaped. Encodings identical to the raw form are dropped.
func tokenWireForms(tok string) [][]byte {
	raw := []byte(tok)
	forms := [][]byte{raw}
	var esc bytes.Buffer
	if err := xml.EscapeText(&esc, raw); err == nil {
		if e := esc.Bytes(); !bytes.Equal(e, raw) {
			forms = append(forms, append([]byte(nil), e...))
		}
	}
	if q := url.QueryEscape(tok); q != tok {
		forms = append(forms, []byte(q))
	}
	return forms
}

// redactCapture is redactSensitive for LENGTH-LIMITED captures: after
// exact-form replacement it also scrubs a token fragment sitting at the
// truncation boundary. Truncation happens BEFORE redaction (the capture
// buffers are capped at BodyLimit), so a token bisected by the limit
// leaves a prefix in the retained bytes that no exact match can find —
// with a boundary in the token's last byte, that fragment is nearly the
// whole credential. When the capture was truncated and its tail is a
// partial prefix of any token wire form, the tail is replaced with the
// redaction marker. Over-scrubbing a coincidental tail costs
// diagnostics fidelity on a truncated capture, never secrecy.
func (c *Client) redactCapture(buf []byte, truncated bool) []byte {
	buf = c.redactSensitive(buf)
	if !truncated || len(buf) == 0 {
		return buf
	}
	tok := c.cfg.AccessToken()
	if tok == nil || *tok == "" {
		return buf
	}
	longest := 0
	for _, form := range tokenWireForms(*tok) {
		if k := boundaryPrefixLen(buf, form); k > longest {
			longest = k
		}
	}
	if longest > 0 {
		buf = append(buf[:len(buf)-longest], redactedToken...)
	}
	return buf
}

// boundaryPrefixLen returns the length of the longest PROPER prefix of
// form that suffixes buf (0 if none) — the shape a truncation boundary
// leaves behind when it bisects an echoed token.
func boundaryPrefixLen(buf, form []byte) int {
	maxK := len(form) - 1
	if maxK > len(buf) {
		maxK = len(buf)
	}
	for k := maxK; k > 0; k-- {
		if bytes.HasSuffix(buf, form[:k]) {
			return k
		}
	}
	return 0
}

// redactEventErr rewrites err's message for the APIEvent stream:
// the configured access token is replaced with [REDACTED] (same policy
// as body capture) and the request's query string is stripped, matching
// what redactURL does for APIEvent.URL. Without this, transport errors
// (*url.Error renders the full URL) and API errors (Path embeds the
// query) leak exactly what the URL field deliberately omits — and a
// server error message echoing the token would hand it to observers.
//
// The attached Unwrap chain is SANITIZED, not the original — see
// sanitizedCause. errors.As to *Error and errors.Is to the context
// sentinels keep working; nothing secret-bearing is reachable.
func (c *Client) redactEventErr(req *http.Request, err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if req != nil && req.URL != nil && req.URL.RawQuery != "" {
		msg = strings.ReplaceAll(msg, "?"+req.URL.RawQuery, "")
	}
	msg = string(c.redactSensitive([]byte(msg)))
	return &redactedError{msg: msg, cause: c.sanitizedCause(err)}
}

// sanitizedCause returns a secret-free stand-in for err's chain that
// preserves the classifications event consumers legitimately branch on:
//
//   - *Error → a COPY with the query string stripped from Path and the
//     server Message run through token redaction (the original could
//     carry request_id/node_id parameters and token-echoing text);
//   - context.Canceled / context.DeadlineExceeded → the sentinel itself
//     (field-free, safe);
//   - anything else (notably *url.Error, whose Error() renders the full
//     URL) → nil: no classification is worth retaining secrets for.
func (c *Client) sanitizedCause(err error) error {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		cp := *apiErr
		if i := strings.IndexByte(cp.Path, '?'); i >= 0 {
			cp.Path = cp.Path[:i]
		}
		cp.Message = string(c.redactSensitive([]byte(cp.Message)))
		return &cp
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return nil
}

// emitEvent fires an APIEvent with optional captured request bytes
// (typically empty — bodyless requests) and no response bytes.
// Used on transport errors, before any response body exists.
func (c *Client) emitEvent(req *http.Request, status int, latency time.Duration, attempt int, locale *types.Locale, reqBody []byte, reqTruncated bool, err error) {
	c.emitEventReqResp(req, status, latency, attempt, reqBody, reqTruncated, nil, false, locale, err)
}

// emitEventReqResp fires an APIEvent including request + response
// payload bytes (either may be nil). Single emit point; older callers
// route through here. Snapshots the emitter under RLock so a
// concurrent SetEventCapture race doesn't deliver to a torn callback.
func (c *Client) emitEventReqResp(req *http.Request, status int, latency time.Duration, attempt int, reqBody []byte, reqTruncated bool, body []byte, truncated bool, locale *types.Locale, err error) {
	c.mu.RLock()
	emit := c.capture.Emit
	c.mu.RUnlock()
	if emit == nil {
		return
	}
	// Snapshot ownership: every event carries its OWN locale copy. All
	// retry attempts of one call (and the fetcher's own local) otherwise
	// share a single pointee — a consumer mutating attempt 1's
	// APIEvent.Locale while the call backs off would alias later
	// attempts' events (and historical ones): a consumer-side race on
	// observability metadata.
	if locale != nil {
		l := *locale
		locale = &l
	}
	emit(APIEvent{
		At:               time.Now(),
		Method:           req.Method,
		URL:              redactURL(req.URL),
		Status:           status,
		Latency:          latency,
		Attempt:          attempt,
		Locale:           locale,
		Request:          reqBody,
		RequestTruncated: reqTruncated,
		Response:         body,
		Truncated:        truncated,
		Err:              c.redactEventErr(req, err),
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

// toAPIError decodes the (already-read) body of a non-2xx response into
// a typed *Error, carrying the server's structured message when the
// error envelope decodes. Takes the body as bytes (rather than r.Body)
// because the body is also consumed for APIEvent capture — reading
// r.Body twice would silently lose the structured error message.
func (c *Client) toAPIError(method, path string, status int, body []byte) error {
	e := &Error{Method: method, Path: path, Status: status}
	if apiErr, decodeErr := c.unmarshallPossibleError(bytes.NewReader(body)); decodeErr == nil {
		// Copy the envelope response_code too — pre-fix only Message was
		// carried, so a <response response_code="NOT_FOUND"> non-2xx body
		// left APIError.Code empty and consumers could not classify the
		// documented code. Empty for the <error> shape (no attr).
		e.Code = types.ResponseCode(apiErr.Code)
		// Redact AT SOURCE: a server error message can echo the
		// X-Access-Token, and this Message travels far beyond the
		// sanitized APIEvent stream — Error() strings logged by
		// callers, recovery handle results, and recovery's own error
		// logs. Pre-fix only the event copy was scrubbed; the
		// caller-facing error carried the credential verbatim, so
		// routine error logging persisted it.
		e.Message = string(c.redactSensitive([]byte(apiErr.Message)))
	}
	return e
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
	// Self-identify on every request so the server can track which SDK
	// versions are in active use. User-Agent is the idiomatic carrier
	// (parseable from access logs with no server change);
	// X-Oddin-SDK-Version is the structured field for filtering without
	// parsing the UA string.
	req.Header.Set("User-Agent", version.UserAgent())
	req.Header.Set("X-Oddin-SDK-Version", version.Version())
	if tok := c.cfg.AccessToken(); tok != nil {
		req.Header.Set("X-Access-Token", *tok)
	}
	return req, nil
}

func (c *Client) unmarshallPossibleError(r io.Reader) (*data.ErrorBody, error) {
	// ErrorBody (not Error): lenient decode that accepts BOTH the
	// <response response_code=…> and bare <error> shapes. The pinned
	// data.Error only decoded <response>, silently dropping the message on
	// the <error> shape common to some 4xx/5xx responses.
	var apiError data.ErrorBody
	if err := xml.NewDecoder(r).Decode(&apiError); err != nil {
		return nil, err
	}
	return &apiError, nil
}
