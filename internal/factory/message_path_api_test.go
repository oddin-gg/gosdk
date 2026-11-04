package factory

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/apipolicy"
	"github.com/oddin-gg/gosdk/internal/cache"
	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/internal/producer"
	"github.com/oddin-gg/gosdk/internal/sdkerr"
	"github.com/oddin-gg/gosdk/types"
)

const producersXML = `<?xml version="1.0"?>
<producers response_code="OK">
  <producer id="1" name="live" description="Live" active="true" api_url="https://x" scope="live" stateful_recovery_window_in_minutes="60"/>
</producers>`

// apiCtl switches a fixture API between healthy and failing and counts the
// requests that reach it.
type apiCtl struct {
	failing atomic.Bool
	// notFoundSummaries answers the match summary with 404 (an event that is
	// not in REST yet) while the rest of the API is healthy.
	notFoundSummaries atomic.Bool
	// delay, in nanoseconds, is how long every request is held before it
	// is answered (a slow, not failing, API).
	delay atomic.Int64
	// truncating answers every request with 2xx headers and a body that
	// ends early (a degraded API).
	truncating atomic.Bool
	// failDescriptions fails only the market description endpoints.
	failDescriptions atomic.Bool
	descriptions     atomic.Int32 // GET .../descriptions/...
	// failTournaments fails only the tournament endpoint (a partial outage).
	failTournaments atomic.Bool
	tournaments     atomic.Int32 // GET .../tournaments/<id>/info
	summaries       atomic.Int32 // GET .../sport_events/<id>/summary
	all             atomic.Int32 // every request except the producer catalog
}

type messagePathRig struct {
	ctl     *apiCtl
	api     *api.Client
	ef      *EntityFactory
	factory *FeedMessageFactory
	clock   *stepClock
}

// newMessagePathRig wires a real api.Client, cache manager, entity factory
// and an opened producer manager against a fixture server that can be made
// to fail with HTTP 500 (a transient server error: the default policy
// retries it).
func newMessagePathRig(t *testing.T) *messagePathRig {
	t.Helper()
	inner := entityFixtureServer(t)
	t.Cleanup(inner.Close)
	ctl := &apiCtl{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/descriptions/producers") {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(w, producersXML)
			return
		}
		ctl.all.Add(1)
		if d := time.Duration(ctl.delay.Load()); d > 0 {
			select {
			case <-time.After(d):
			case <-r.Context().Done():
				return
			}
		}
		if strings.HasSuffix(r.URL.Path, "/summary") {
			ctl.summaries.Add(1)
			if ctl.notFoundSummaries.Load() {
				http.Error(w, "<error/>", http.StatusNotFound)
				return
			}
		}
		if ctl.truncating.Load() {
			w.Header().Set("Content-Type", "application/xml")
			w.Header().Set("Content-Length", "4096")
			_, _ = io.WriteString(w, `<?xml version="1.0"?><match_summary><sport_event id="`)
			return
		}
		isDescription := strings.Contains(r.URL.Path, "/descriptions/")
		if isDescription {
			ctl.descriptions.Add(1)
			if ctl.failDescriptions.Load() || ctl.failing.Load() {
				http.Error(w, "<error/>", http.StatusInternalServerError)
				return
			}
		}
		isTournament := strings.Contains(r.URL.Path, "/tournaments/")
		if isTournament {
			ctl.tournaments.Add(1)
		}
		if ctl.failing.Load() || (isTournament && ctl.failTournaments.Load()) {
			http.Error(w, "<error/>", http.StatusInternalServerError)
			return
		}
		// The fixtures describe match 99; the API client rejects a reply
		// for another id, so echo the requested one.
		if strings.Contains(r.URL.Path, "/sport_events/") && (strings.HasSuffix(r.URL.Path, "/summary") || strings.HasSuffix(r.URL.Path, "/fixture")) {
			parts := strings.Split(r.URL.Path, "/")
			id := parts[len(parts)-2]
			body := matchSummaryXML
			if strings.HasSuffix(r.URL.Path, "/fixture") {
				body = fixtureXML
			}
			w.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(w, strings.ReplaceAll(body, "od:match:99", id))
			return
		}
		inner.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	cfg := minimalCfg{}
	apiClient := api.New(cfg)
	apiClient.SetHTTPClient(&http.Client{
		Transport: &rewriteHTTPClient{
			target: srv.URL,
			base:   &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		},
		Timeout: 10 * time.Second,
	})
	cm := cache.NewManager(t.Context(), apiClient, cfg, log.New(nil), nil)
	ef := NewEntityFactory(cm)
	pm := producer.NewManager(cfg, apiClient, log.New(nil))
	if err := pm.Open(t.Context()); err != nil {
		t.Fatalf("producer manager Open: %v", err)
	}
	clk := &stepClock{t: time.Unix(1_000_000, 0)}
	// A real market factory: name resolution reaches the description caches
	// and, for an uncached market, the API.
	mdf := NewMarketDescriptionFactory(cm.MarketDescriptionCache, cm.MarketVoidReasonsCache, cm.PlayersCache, cm.CompetitorCache)
	mf := NewMarketFactory(NewMarketDataFactory(cfg, mdf), []types.Locale{types.EnLocale}, true, log.New(nil))
	f := NewFeedMessageFactory(ef, mf, pm, cfg, log.New(nil))
	f.apiFailures.now = clk.Now
	return &messagePathRig{ctl: ctl, api: apiClient, ef: ef, factory: f, clock: clk}
}

func oddsChangeFor(eventNum int) *types.FeedMessage {
	id := types.URN{Prefix: "od", Type: "match", ID: eventNum}
	sport := types.URN{Prefix: "od", Type: "sport", ID: 1}
	return &types.FeedMessage{
		BasicFeedMessage: types.BasicFeedMessage{
			RoutingKey: &types.RoutingKeyInfo{
				FullRoutingKey: "hi.-.live.odds_change.1." + id.ToString(),
				SportID:        &sport,
				EventID:        &id,
			},
			RawMessage: []byte("<odds_change/>"),
		},
		Message: &feedXML.OddsChange{ProductID: 1, EventID: id.ToString()},
	}
}

// buildEvent runs the message through BuildMessage and returns the event
// the built odds change carries (nil when the event could not be built).
func (r *messagePathRig) buildEvent(t *testing.T, eventNum int) interface{} {
	t.Helper()
	out, err := r.factory.BuildMessage(t.Context(), oddsChangeFor(eventNum))
	if err != nil {
		t.Fatalf("BuildMessage: %v (a failed event build must still dispatch the message)", err)
	}
	oc, ok := out.(oddsChangeImpl)
	if !ok {
		t.Fatalf("BuildMessage returned %T", out)
	}
	return oc.event
}

// TestMessagePath_FailingAPI_OneAttemptPerEventWithinTheWindow is the
// regression for the live session stalling behind a down API: every
// message of an uncached match built the match synchronously with the
// getters' retry policy (three attempts with backoff, ~1.2 s a message,
// no memory of the failure), so a long outage left the session thousands
// of messages behind. Now the first message pays one attempt without
// retries; the others go out at once with event == nil.
func TestMessagePath_FailingAPI_OneAttemptPerEventWithinTheWindow(t *testing.T) {
	r := newMessagePathRig(t)
	r.ctl.failing.Store(true)

	const messages = 40
	start := time.Now()
	for range messages {
		if ev := r.buildEvent(t, 99); ev != nil {
			t.Fatalf("event = %v, want nil while the API fails", ev)
		}
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("%d messages took %v; they must not wait on the failing API", messages, elapsed)
	}
	if got := r.ctl.summaries.Load(); got != 1 {
		t.Fatalf("match summary requests = %d, want 1 (one attempt, no retries, then the window)", got)
	}
	if r.factory.apiFailures.len() != 1 {
		t.Fatalf("window holds %d events, want 1", r.factory.apiFailures.len())
	}

	// Other events are not shielded by this one's failure: one attempt each.
	for _, id := range []int{100, 101, 102} {
		if ev := r.buildEvent(t, id); ev != nil {
			t.Fatalf("event %d = %v, want nil", id, ev)
		}
		r.buildEvent(t, id)
		r.buildEvent(t, id)
	}
	if got := r.ctl.summaries.Load(); got != 4 {
		t.Fatalf("match summary requests = %d, want 4 (one per distinct event)", got)
	}
}

// The window ends by time, too.
func TestMessagePath_WindowExpires(t *testing.T) {
	r := newMessagePathRig(t)
	r.ctl.failing.Store(true)

	r.buildEvent(t, 99)
	r.buildEvent(t, 99)
	if got := r.ctl.summaries.Load(); got != 1 {
		t.Fatalf("summary requests = %d, want 1 inside the window", got)
	}
	r.clock.Advance(apiFailureWindowTTL + time.Second)
	r.buildEvent(t, 99)
	if got := r.ctl.summaries.Load(); got != 2 {
		t.Fatalf("summary requests = %d, want 2 after the window expired", got)
	}
}

// An API that degrades into truncated bodies still answers 2xx headers. Those
// are not successes: they must not end the window, or every message would
// pay its attempt again while the API stays degraded.
func TestMessagePath_AnswerWithATruncatedBodyDoesNotClearTheWindow(t *testing.T) {
	r := newMessagePathRig(t)
	r.ctl.failing.Store(true)
	r.buildEvent(t, 99)
	if got := r.ctl.summaries.Load(); got != 1 {
		t.Fatalf("summary requests = %d, want 1", got)
	}

	r.ctl.failing.Store(false)
	r.ctl.truncating.Store(true)
	if ev := r.buildEvent(t, 100); ev != nil {
		t.Fatalf("event = %v, want nil: the body is truncated", ev)
	}
	if got := r.ctl.summaries.Load(); got != 2 {
		t.Fatalf("summary requests = %d, want 2 (event 100 made one attempt)", got)
	}
	// The truncated answer did not clear the window: both events stay
	// refused without a request.
	r.buildEvent(t, 99)
	r.buildEvent(t, 100)
	if got := r.ctl.summaries.Load(); got != 2 {
		t.Fatalf("summary requests = %d, want 2: the window must hold across an answer with a truncated body", got)
	}

	// A complete answer does clear it.
	r.ctl.truncating.Store(false)
	if _, err := r.api.FetchSports(t.Context(), types.EnLocale); err != nil {
		t.Fatalf("FetchSports: %v", err)
	}
	r.buildEvent(t, 99)
	if got := r.ctl.summaries.Load(); got != 3 {
		t.Fatalf("summary requests = %d, want 3 once the API has answered completely", got)
	}
}

// The first successful API response, of ANY endpoint, ends the whole
// window: nothing stays refused once the API is back.
func TestMessagePath_AnySuccessClearsTheWindow(t *testing.T) {
	r := newMessagePathRig(t)
	r.ctl.failing.Store(true)

	for _, id := range []int{99, 100, 101} {
		r.buildEvent(t, id)
	}
	base := r.ctl.summaries.Load()
	if base != 3 || r.factory.apiFailures.len() != 3 {
		t.Fatalf("setup: summaries=%d window=%d, want 3 and 3", base, r.factory.apiFailures.len())
	}

	// The API recovers, but nobody has seen it answer yet: still skipped.
	r.ctl.failing.Store(false)
	if ev := r.buildEvent(t, 99); ev != nil {
		t.Fatal("event built before any success was observed; the window should still hold")
	}
	if got := r.ctl.summaries.Load(); got != base {
		t.Fatalf("summary requests = %d, want still %d", got, base)
	}

	// An unrelated call succeeds (a getter for a sport, say).
	if _, err := r.ef.BuildSport(t.Context(), types.URN{Prefix: "od", Type: "sport", ID: 1}, []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("BuildSport: %v", err)
	}

	// Every event that was in the window builds now, on its next message.
	for _, id := range []int{99, 100, 101} {
		if ev := r.buildEvent(t, id); ev == nil {
			t.Fatalf("event %d still refused after the API answered", id)
		}
	}
}

// Getters (the public entity reads) keep their behaviour: retries, and no
// window.
func TestMessagePath_GettersStillRetryAndIgnoreTheWindow(t *testing.T) {
	r := newMessagePathRig(t)
	r.ctl.failing.Store(true)

	r.buildEvent(t, 99) // opens the window for the event
	if got := r.ctl.summaries.Load(); got != 1 {
		t.Fatalf("summary requests = %d, want 1", got)
	}

	id := types.URN{Prefix: "od", Type: "match", ID: 99}
	if _, err := r.ef.BuildMatch(t.Context(), id, []types.Locale{types.EnLocale}, nil); err == nil {
		t.Fatal("getter succeeded against a failing API")
	}
	if got := r.ctl.summaries.Load(); got != 1+3 {
		t.Fatalf("summary requests = %d, want 4 (1 message-path attempt + 3 getter attempts)", got)
	}
}

// A message whose event is fully cached is built from the cache even inside
// the window: skipping the API must not throw away what is already known.
func TestMessagePath_WindowStillServesCachedEvents(t *testing.T) {
	r := newMessagePathRig(t)
	if ev := r.buildEvent(t, 99); ev == nil {
		t.Fatal("setup: event not built from a healthy API")
	}
	id := types.URN{Prefix: "od", Type: "match", ID: 99}
	r.factory.apiFailures.record(id, r.factory.apiSuccessCount())
	r.ctl.failing.Store(true)
	before := r.ctl.all.Load()
	if ev := r.buildEvent(t, 99); ev == nil {
		t.Fatal("cached event refused inside the window")
	}
	if got := r.ctl.all.Load(); got != before {
		t.Fatalf("API requests = %d, want none for a cached event", got-before)
	}
}

// A caller's own cancellation or expired deadline says nothing about the
// API and must not open the window. The API itself is healthy here, so a
// message whose event stayed unbuilt proves the build was attempted and
// failed on the caller's context, not skipped by the window.
func TestMessagePath_CancelledContextDoesNotOpenTheWindow(t *testing.T) {
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	expired, cancelExpired := context.WithDeadline(t.Context(), time.Now().Add(-time.Minute))
	defer cancelExpired()

	for name, ctx := range map[string]context.Context{"cancelled": cancelled, "expired deadline": expired} {
		t.Run(name, func(t *testing.T) {
			r := newMessagePathRig(t)
			out, err := r.factory.BuildMessage(ctx, oddsChangeFor(99))
			if err != nil {
				t.Fatalf("BuildMessage: %v (a failed event build must still dispatch the message)", err)
			}
			oc, ok := out.(oddsChangeImpl)
			if !ok {
				t.Fatalf("BuildMessage returned %T", out)
			}
			if oc.event != nil {
				t.Fatal("event built on a dead context; the build was not attempted as expected")
			}
			if r.factory.apiFailures.len() != 0 {
				t.Fatalf("window holds %d events after a build on a dead context", r.factory.apiFailures.len())
			}
		})
	}
}

func TestAPIFailureWindow_Bounded(t *testing.T) {
	var w apiFailureWindow
	for i := range apiFailureWindowMax * 2 {
		w.record(types.URN{Prefix: "od", Type: "match", ID: i}, 7)
	}
	if got := w.len(); got > apiFailureWindowMax {
		t.Fatalf("window holds %d entries, cap is %d", got, apiFailureWindowMax)
	}
	// An entry of an older success count is not honoured and is dropped.
	id := types.URN{Prefix: "od", Type: "match", ID: 1 << 20}
	w.record(id, 7)
	if !w.blocked(id, 7) {
		t.Fatal("recorded entry not blocked")
	}
	if w.blocked(id, 8) {
		t.Fatal("entry blocked after the success counter moved")
	}
	if w.blocked(id, 7) {
		t.Fatal("entry survived being found stale")
	}
}

// A closed client is shutdown, not an API outage: it must not open the
// window either.
func TestMessagePath_ClosedClientDoesNotOpenTheWindow(t *testing.T) {
	r := newMessagePathRig(t)
	r.api.Close()
	if ev := r.buildEvent(t, 99); ev != nil {
		t.Fatalf("event = %v, want nil from a closed client", ev)
	}
	if r.factory.apiFailures.len() != 0 {
		t.Fatalf("window holds %d events after a closed-client failure", r.factory.apiFailures.len())
	}
}

// A partial outage: the match summary answers, the tournament endpoint does
// not. The build's own successful call must not void the window entry it
// records for the failure (the entry is bound to the success counter read
// AFTER the failed attempt; only a LATER success clears it).
func TestMessagePath_PartialOutage_WindowOpensAfterTheBuildsOwnSuccess(t *testing.T) {
	r := newMessagePathRig(t)
	r.ctl.failTournaments.Store(true)

	for range 10 {
		if ev := r.buildEvent(t, 99); ev != nil {
			t.Fatalf("event = %v, want nil while the tournament endpoint fails", ev)
		}
	}
	if got := r.ctl.tournaments.Load(); got != 1 {
		t.Fatalf("tournament requests = %d, want 1 (the window opened despite the summary's own success)", got)
	}
	if r.factory.apiFailures.len() != 1 {
		t.Fatalf("window holds %d events, want 1", r.factory.apiFailures.len())
	}

	// A LATER success of any call still clears it, as decided.
	r.ctl.failTournaments.Store(false)
	if _, err := r.ef.BuildSport(t.Context(), types.URN{Prefix: "od", Type: "sport", ID: 1}, []types.Locale{types.EnLocale}); err != nil {
		t.Fatal(err)
	}
	if ev := r.buildEvent(t, 99); ev == nil {
		t.Fatal("event still refused after a later success")
	}
}

// The budget is for the WHOLE build: BuildMatch makes several sequential
// requests (summary, tournament, competitors, fixture), and with a slow API
// each used to get its own full allowance, holding the session goroutine for
// many multiples of it. Every request now inherits what is left, so the
// build returns about one budget after it started, and the window opens.
func TestMessagePath_SlowAPI_TheWholeBuildIsBoundedByOneBudget(t *testing.T) {
	r := newMessagePathRig(t)
	r.ctl.delay.Store(int64(1500 * time.Millisecond)) // every request answers, slowly

	start := time.Now()
	ev := r.buildEvent(t, 99)
	elapsed := time.Since(start)
	if ev != nil {
		t.Fatal("event built although the API cannot answer the whole build within the budget")
	}
	if elapsed > apipolicy.MessageTimeout+700*time.Millisecond {
		t.Fatalf("one message build took %v, want about the %v budget (several requests of 1.5 s each)", elapsed, apipolicy.MessageTimeout)
	}
	if r.factory.apiFailures.len() != 1 {
		t.Fatalf("window holds %d events, want 1", r.factory.apiFailures.len())
	}

	// Later messages for the event are not held at all.
	before := r.ctl.all.Load()
	start = time.Now()
	for range 5 {
		r.buildEvent(t, 99)
	}
	if time.Since(start) > 300*time.Millisecond || r.ctl.all.Load() != before {
		t.Fatalf("messages inside the window took %v and made %d request(s)", time.Since(start), r.ctl.all.Load()-before)
	}
}

func tournamentMessage(id int) *types.FeedMessage {
	m := oddsChangeFor(1)
	tid := types.URN{Prefix: "od", Type: "tournament", ID: id}
	m.RoutingKey.EventID = &tid
	m.RoutingKey.FullRoutingKey = "hi.-.live.odds_change.1." + tid.ToString()
	return m
}

// A tournament-routed message goes through the same policy and window.
func TestMessagePath_TournamentRoutedMessage_OneAttemptThenTheWindow(t *testing.T) {
	r := newMessagePathRig(t)
	r.ctl.failing.Store(true)

	for range 20 {
		out, err := r.factory.BuildMessage(t.Context(), tournamentMessage(7))
		if err != nil {
			t.Fatalf("BuildMessage: %v", err)
		}
		if ev := out.(oddsChangeImpl).event; ev != nil {
			t.Fatalf("event = %v, want nil", ev)
		}
	}
	if got := r.ctl.tournaments.Load(); got != 1 {
		t.Fatalf("tournament requests = %d, want 1 (one attempt, then the window)", got)
	}
	if r.factory.apiFailures.len() != 1 {
		t.Fatalf("window holds %d events, want 1", r.factory.apiFailures.len())
	}
}

// BuildUnparsableMessage builds the event on the same path: a match and a
// tournament, one API attempt each and nothing more within the window.
func TestMessagePath_UnparsableMessages_OneAttemptThenTheWindow(t *testing.T) {
	r := newMessagePathRig(t)
	r.ctl.failing.Store(true)

	for range 20 {
		if r.factory.BuildUnparsableMessage(t.Context(), oddsChangeFor(99)) == nil {
			t.Fatal("nil unparsable message")
		}
		if r.factory.BuildUnparsableMessage(t.Context(), tournamentMessage(7)) == nil {
			t.Fatal("nil unparsable message")
		}
	}
	if got := r.ctl.summaries.Load(); got != 1 {
		t.Fatalf("match summary requests = %d, want 1", got)
	}
	if got := r.ctl.tournaments.Load(); got != 1 {
		t.Fatalf("tournament requests = %d, want 1", got)
	}
	if r.factory.apiFailures.len() != 2 {
		t.Fatalf("window holds %d events, want 2", r.factory.apiFailures.len())
	}
}

// oddsChangeWithDynamicMarket is an odds change carrying one market whose
// description is not cached (a dynamic variant is fetched per key).
func oddsChangeWithDynamicMarket(eventNum int) *types.FeedMessage {
	m := oddsChangeFor(eventNum)
	m.Message.(*feedXML.OddsChange).Odds.Markets = []*feedXML.MarketWithOutcome{
		feedMarket(3, "variant=od:dynamic_outcomes:99", "1", "2"),
	}
	return m
}

// Market and outcome name resolution runs under the SAME message policy as
// the event build: one budget, one attempt per request, no retries. With the
// original context it used the getters' policy and could hold the session
// through retries for every odds change during an outage.
func TestMessagePath_MarketNameResolution_UsesTheMessagePolicy(t *testing.T) {
	// Event cached, description endpoints failing: the market lookup is one
	// attempt per request inside the budget, not a retry cycle per message.
	t.Run("descriptions failing", func(t *testing.T) {
		r := newMessagePathRig(t)
		if ev := r.buildEvent(t, 99); ev == nil {
			t.Fatal("setup: event not built from a healthy API")
		}
		r.ctl.failDescriptions.Store(true)
		before := r.ctl.descriptions.Load()

		start := time.Now()
		out, err := r.factory.BuildMessage(t.Context(), oddsChangeWithDynamicMarket(99))
		elapsed := time.Since(start)
		if err != nil {
			t.Fatal(err)
		}
		if len(out.(oddsChangeImpl).markets) != 1 {
			t.Fatalf("markets = %d, want the message delivered with its market", len(out.(oddsChangeImpl).markets))
		}
		if elapsed > apipolicy.MessageTimeout+700*time.Millisecond {
			t.Fatalf("delivery took %v, want within the %v budget", elapsed, apipolicy.MessageTimeout)
		}
		// A retried request would show as three requests for one lookup.
		if got := r.ctl.descriptions.Load() - before; got < 1 || got > 2 {
			t.Fatalf("description requests = %d, want one attempt (no retries)", got)
		}
	})

	// Everything failing, event uncached: the event build is the one failed
	// attempt; markets make no request after it.
	t.Run("API down", func(t *testing.T) {
		r := newMessagePathRig(t)
		r.ctl.failing.Store(true)
		start := time.Now()
		out, err := r.factory.BuildMessage(t.Context(), oddsChangeWithDynamicMarket(99))
		if err != nil {
			t.Fatal(err)
		}
		if out.(oddsChangeImpl).event != nil {
			t.Fatal("event built from a failing API")
		}
		if elapsed := time.Since(start); elapsed > apipolicy.MessageTimeout+700*time.Millisecond {
			t.Fatalf("delivery took %v", elapsed)
		}
		if got := r.ctl.all.Load(); got != 1 {
			t.Fatalf("API requests = %d, want 1 (the event's single attempt; none for the market)", got)
		}
	})

	// Event inside the window: no API request at all, market included.
	t.Run("event in the window", func(t *testing.T) {
		r := newMessagePathRig(t)
		r.ctl.failing.Store(true)
		r.buildEvent(t, 99) // opens the window
		before := r.ctl.all.Load()
		start := time.Now()
		for range 5 {
			if _, err := r.factory.BuildMessage(t.Context(), oddsChangeWithDynamicMarket(99)); err != nil {
				t.Fatal(err)
			}
		}
		if got := r.ctl.all.Load() - before; got != 0 {
			t.Fatalf("API requests = %d inside the window, want none", got)
		}
		if time.Since(start) > 300*time.Millisecond {
			t.Fatalf("5 messages took %v inside the window", time.Since(start))
		}
	})
}

// Market and outcome name lookups swallow API failures (a missing name must
// not fail the message), so with a cached event nothing used to record them
// and every message of the event paid for the same failing lookup again. A
// failed lookup now opens the event's failure window: the next messages skip
// the API, and the first success anywhere clears it.
func TestMessagePath_MarketLookupFailure_OpensTheEventsWindow(t *testing.T) {
	r := newMessagePathRig(t)
	if ev := r.buildEvent(t, 99); ev == nil {
		t.Fatal("setup: event not built from a healthy API")
	}
	r.ctl.failDescriptions.Store(true)

	if _, err := r.factory.BuildMessage(t.Context(), oddsChangeWithDynamicMarket(99)); err != nil {
		t.Fatal(err)
	}
	first := r.ctl.descriptions.Load()
	if first < 1 {
		t.Fatalf("first message made %d description request(s), want its attempt", first)
	}
	if r.factory.apiFailures.len() != 1 {
		t.Fatalf("window holds %d events after a failed market lookup, want 1", r.factory.apiFailures.len())
	}

	start := time.Now()
	for range 5 {
		if _, err := r.factory.BuildMessage(t.Context(), oddsChangeWithDynamicMarket(99)); err != nil {
			t.Fatal(err)
		}
	}
	if got := r.ctl.descriptions.Load(); got != first {
		t.Fatalf("description requests grew from %d to %d inside the window", first, got)
	}
	if time.Since(start) > 300*time.Millisecond {
		t.Fatalf("5 messages inside the window took %v", time.Since(start))
	}

	// The first success anywhere clears it: the next message tries again.
	// (A direct client call: a cached getter would make no request.)
	if _, err := r.api.FetchSports(t.Context(), types.EnLocale); err != nil {
		t.Fatal(err)
	}
	if _, err := r.factory.BuildMessage(t.Context(), oddsChangeWithDynamicMarket(99)); err != nil {
		t.Fatal(err)
	}
	if got := r.ctl.descriptions.Load(); got <= first {
		t.Fatalf("description requests = %d after a success cleared the window, want a new attempt", got)
	}
}

// A lookup the API answers with "not found" says nothing about its health and
// must not open the window.
func TestMessagePath_MarketLookupNotFound_DoesNotOpenTheWindow(t *testing.T) {
	r := newMessagePathRig(t)
	if ev := r.buildEvent(t, 99); ev == nil {
		t.Fatal("setup: event not built from a healthy API")
	}
	// The fixture server answers description requests with 404 (healthy API).
	if _, err := r.factory.BuildMessage(t.Context(), oddsChangeWithDynamicMarket(99)); err != nil {
		t.Fatal(err)
	}
	if r.factory.apiFailures.len() != 0 {
		t.Fatalf("window holds %d events after a not-found lookup", r.factory.apiFailures.len())
	}
}

func dynMarketSpec() *string { s := "variant=od:dynamic_outcomes:99"; return &s }

// Every market-bearing message type resolves names under the message policy:
// with a cached event and a failing description endpoint the message is
// delivered within the shared budget, the first message makes a bounded
// number of requests (no retries) and the next one none.
func TestMessagePath_AllMarketMessageTypes_UseTheMessagePolicy(t *testing.T) {
	attrs := feedXML.MessageAttributes{Product: 1, EventID: "od:match:99"}
	mkAttrs := func() feedXML.MarketAttributes { return feedXML.MarketAttributes{ID: 3, Specifiers: dynMarketSpec()} }
	cases := []struct {
		name    string
		msg     func() types.BasicMessage
		markets func(out any) int
	}{
		{"BetSettlement", func() types.BasicMessage {
			return &feedXML.BetSettlement{ProductID: 1, EventID: "od:match:99", Markets: feedXML.MarketsWrapper{
				Markets: []*feedXML.MarketWithOutcome{feedMarket(3, *dynMarketSpec(), "1")},
			}}
		}, func(out any) int { return len(out.(betSettlementImpl).markets) }},
		{"BetCancel", func() types.BasicMessage {
			return &feedXML.BetCancel{MessageAttributes: attrs, Markets: []*feedXML.MarketWithoutOutcome{{MarketAttributes: mkAttrs()}}}
		}, func(out any) int { return len(out.(betCancelImpl).markets) }},
		{"RollbackBetSettlement", func() types.BasicMessage {
			return &feedXML.RollbackBetSettlement{MessageAttributes: attrs, Markets: []feedXML.RollbackBetSettlementMarket{{MarketAttributes: mkAttrs()}}}
		}, func(out any) int { return len(out.(rollbackBetSettlementImpl).markets) }},
		{"RollbackBetCancel", func() types.BasicMessage {
			return &feedXML.RollbackBetCancel{MessageAttributes: attrs, Markets: []feedXML.RollbackBetCancelMarket{{MarketAttributes: mkAttrs()}}}
		}, func(out any) int { return len(out.(rollbackBetCancelImpl).markets) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newMessagePathRig(t)
			if ev := r.buildEvent(t, 99); ev == nil {
				t.Fatal("setup: event not built from a healthy API")
			}
			r.ctl.failDescriptions.Store(true)
			before := r.ctl.descriptions.Load()

			build := func() any {
				m := oddsChangeFor(99)
				m.Message = tc.msg()
				out, err := r.factory.BuildMessage(t.Context(), m)
				if err != nil {
					t.Fatalf("BuildMessage: %v", err)
				}
				return out
			}
			start := time.Now()
			out := build()
			if n := tc.markets(out); n != 1 {
				t.Fatalf("delivered markets = %d, want 1", n)
			}
			if elapsed := time.Since(start); elapsed > apipolicy.MessageTimeout+700*time.Millisecond {
				t.Fatalf("delivery took %v, want within the %v budget", elapsed, apipolicy.MessageTimeout)
			}
			got := r.ctl.descriptions.Load() - before
			if got < 1 || got > 2 {
				t.Fatalf("description requests = %d, want one attempt (no retries)", got)
			}
			build()
			if again := r.ctl.descriptions.Load() - before; again != got {
				t.Fatalf("a second message made %d more description request(s) inside the window", again-got)
			}
		})
	}
}

// A 404 for an event that is not in REST yet is an answer of a healthy API:
// it must not open the window, so the next message of the event tries the API
// again.
func TestMessagePath_NotFoundEventDoesNotOpenTheWindow(t *testing.T) {
	r := newMessagePathRig(t)
	r.ctl.notFoundSummaries.Store(true)
	for range 3 {
		if ev := r.buildEvent(t, 99); ev != nil {
			t.Fatalf("event = %v, want nil for an event the API does not know", ev)
		}
	}
	if r.factory.apiFailures.len() != 0 {
		t.Fatalf("window holds %d events after a 404", r.factory.apiFailures.len())
	}
	if got := r.ctl.summaries.Load(); got != 3 {
		t.Fatalf("match summary requests = %d, want 3 (every message tries the API)", got)
	}
}

// Which build errors open the window: a refused request (budget spent,
// nothing was sent) and a 4xx other than 429 do not; a server failure, a
// rate limit and a transport failure do.
func TestMessagePath_NoteBuildFailureClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		open bool
	}{
		{"budget spent before sending", fmt.Errorf("api: GET /x: %w: %w", apipolicy.ErrBudgetSpent, context.DeadlineExceeded), false},
		{"404", &api.Error{Status: 404}, false},
		{"client closed", sdkerr.ErrClosed, false},
		{"500", &api.Error{Status: 500}, true},
		{"429", &api.Error{Status: 429}, true},
		{"transport failure", errors.New("connection refused"), true},
		{"timed out after sending", fmt.Errorf("fetch: %w", context.DeadlineExceeded), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newMessagePathRig(t)
			r.factory.noteBuildFailure(t.Context(), types.URN{Prefix: "od", Type: "match", ID: 99}, tc.err)
			if got := r.factory.apiFailures.len() == 1; got != tc.open {
				t.Fatalf("window opened = %v, want %v", got, tc.open)
			}
		})
	}
}

// A caller's snapshot of the success counter can be older than an entry: it
// read N, an unrelated success moved the counter to N+1, and another session
// recorded a failure at N+1. The older snapshot must not drop that newer
// entry; only a caller that has seen a success after it was recorded does.
func TestAPIFailureWindow_AStaleCountDoesNotDropANewerFailure(t *testing.T) {
	var w apiFailureWindow
	id := types.URN{Prefix: "od", Type: "match", ID: 1}
	w.record(id, 8)

	if !w.blocked(id, 7) {
		t.Fatal("a caller's stale count (7) dropped the entry recorded at 8")
	}
	if !w.blocked(id, 8) {
		t.Fatal("entry lost after the stale caller")
	}
	if w.blocked(id, 9) {
		t.Fatal("entry still blocked after a success past its count")
	}
	if w.blocked(id, 8) {
		t.Fatal("entry survived a caller that saw a later success")
	}
}

// A full window must not be emptied by a caller whose success-count snapshot
// is stale: the entries above its count are current. Making room costs at
// most the one arbitrary entry.
func TestAPIFailureWindow_FullWindowKeepsEntriesNewerThanTheCaller(t *testing.T) {
	var w apiFailureWindow
	const n = 7
	for i := range apiFailureWindowMax {
		w.record(types.URN{Prefix: "od", Type: "match", ID: i}, n+1)
	}
	fresh := types.URN{Prefix: "od", Type: "match", ID: 1 << 20}
	w.record(fresh, n) // the caller read the counter before the success at n+1

	if got := w.len(); got > apiFailureWindowMax {
		t.Fatalf("window holds %d entries, cap is %d", got, apiFailureWindowMax)
	}
	if !w.blocked(fresh, n) {
		t.Fatal("the entry just recorded is not blocked")
	}
	kept := 0
	for i := range apiFailureWindowMax {
		if w.blocked(types.URN{Prefix: "od", Type: "match", ID: i}, n) {
			kept++
		}
	}
	if kept < apiFailureWindowMax-1 {
		t.Fatalf("%d of %d current entries survived a stale caller, want all but the one evicted", kept, apiFailureWindowMax)
	}
}

// An event the API answers with 404 (not in REST yet) says nothing about the
// description API: the message's market names still resolve under its own
// budget. A failure that says the API is unavailable makes no further request.
func TestMessagePath_EventNotFound_NamesStillResolve(t *testing.T) {
	t.Run("event 404, descriptions healthy", func(t *testing.T) {
		r := newMessagePathRig(t)
		r.ctl.notFoundSummaries.Store(true)
		out, err := r.factory.BuildMessage(t.Context(), oddsChangeWithDynamicMarket(99))
		if err != nil {
			t.Fatal(err)
		}
		if out.(oddsChangeImpl).event != nil {
			t.Fatal("event built for a 404")
		}
		if got := r.ctl.descriptions.Load(); got < 1 {
			t.Fatalf("description requests = %d, want names resolved after a 404 event", got)
		}
	})
	t.Run("event 5xx", func(t *testing.T) {
		r := newMessagePathRig(t)
		r.ctl.failing.Store(true)
		if _, err := r.factory.BuildMessage(t.Context(), oddsChangeWithDynamicMarket(99)); err != nil {
			t.Fatal(err)
		}
		if got := r.ctl.descriptions.Load(); got != 0 {
			t.Fatalf("description requests = %d, want none after an unavailable API", got)
		}
	})
}
