package factory

import (
	"context"
	"crypto/tls"
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
	// descriptionDelay, in nanoseconds, holds only the market description
	// endpoints (a catalog that downloads slowly).
	descriptionDelay atomic.Int64
	// hanging holds every request (except the producer catalog) until the
	// test ends or the client gives the request up.
	hanging atomic.Bool
	// release is closed when the test ends, so no handler outlives it.
	release chan struct{}
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
	inFlight        atomic.Int32 // requests the server has not answered yet
	// catalog, when set, is served as the market description catalog.
	catalog atomic.Pointer[string]
	// failPlayers fails only the player profile endpoint with a server
	// error; notFoundPlayers answers it with 404 (a healthy API).
	failPlayers     atomic.Bool
	notFoundPlayers atomic.Bool
	players         atomic.Int32 // GET .../players/<id>/profile
}

type messagePathRig struct {
	ctl     *apiCtl
	api     *api.Client
	ef      *EntityFactory
	factory *FeedMessageFactory
}

// Timing of a rig. The message timeout and the breaker's time are short so
// the tests that need them to pass stay fast; the tests that only count
// requests use the defaults of the production code.
type rigTiming struct {
	messageTimeout time.Duration
	breakerOpenFor time.Duration
}

// newMessagePathRig wires a real api.Client, cache manager, entity factory
// and an opened producer manager against a fixture server that can be made
// to fail with HTTP 500 (a transient server error: the default policy
// retries it).
func newMessagePathRig(t *testing.T) *messagePathRig {
	t.Helper()
	return newTimedMessagePathRig(t, rigTiming{messageTimeout: apipolicy.MessageTimeout, breakerOpenFor: 10 * time.Second})
}

// newTimedMessagePathRig is newMessagePathRig with the message timeout and
// the breaker's open time of tm.
func newTimedMessagePathRig(t *testing.T, tm rigTiming) *messagePathRig {
	t.Helper()
	inner := entityFixtureServer(t)
	t.Cleanup(inner.Close)
	ctl := &apiCtl{release: make(chan struct{})}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/descriptions/producers") {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(w, producersXML)
			return
		}
		ctl.all.Add(1)
		ctl.inFlight.Add(1)
		defer ctl.inFlight.Add(-1)
		if ctl.hanging.Load() {
			if strings.Contains(r.URL.Path, "/summary") {
				ctl.summaries.Add(1)
			}
			select {
			case <-ctl.release:
			case <-r.Context().Done():
			}
			return
		}
		d := time.Duration(ctl.delay.Load())
		if strings.Contains(r.URL.Path, "/descriptions/") {
			d += time.Duration(ctl.descriptionDelay.Load())
		}
		if d > 0 {
			select {
			case <-time.After(d):
			case <-ctl.release:
				return
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
		if isDescription {
			if c := ctl.catalog.Load(); c != nil && strings.HasSuffix(r.URL.Path, "/markets") {
				w.Header().Set("Content-Type", "application/xml")
				_, _ = io.WriteString(w, *c)
				return
			}
		}
		if strings.Contains(r.URL.Path, "/players/") {
			ctl.players.Add(1)
			if ctl.failPlayers.Load() {
				http.Error(w, "<error/>", http.StatusInternalServerError)
				return
			}
			if ctl.notFoundPlayers.Load() {
				http.Error(w, "<error/>", http.StatusNotFound)
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
	// Registered after srv.Close, so it runs first: held requests end
	// before the server waits for its handlers.
	t.Cleanup(func() { close(ctl.release) })

	cfg := minimalCfg{}
	apiClient := api.New(cfg)
	apiClient.Breaker().SetOpenFor(tm.breakerOpenFor)
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
	// A real market factory: name resolution reaches the description caches
	// and, for an uncached market, the API.
	mdf := NewMarketDescriptionFactory(cm.MarketDescriptionCache, cm.MarketVoidReasonsCache, cm.PlayersCache, cm.CompetitorCache)
	mf := NewMarketFactory(NewMarketDataFactory(cfg, mdf), []types.Locale{types.EnLocale}, true, log.New(nil))
	f := NewFeedMessageFactory(ef, mf, pm, cfg, log.New(nil), apiClient.Breaker())
	f.messageTimeout = tm.messageTimeout
	return &messagePathRig{ctl: ctl, api: apiClient, ef: ef, factory: f}
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

// settle waits until the server holds no request of the setup and the
// breaker is closed. A flight the setup started outlives a setup message
// that ran out of time, and its healthy answer closes the breaker whenever
// it lands; a test that watches the breaker starts after it.
func (r *messagePathRig) settle(t *testing.T) {
	t.Helper()
	waitFor(t, "the setup's requests to end", func() bool {
		return r.ctl.inFlight.Load() == 0 && !r.api.Breaker().Open()
	})
}

// tripped reports whether the breaker has opened since opened was taken from
// Breaker().Opened(), even if a healthy answer has closed it again since.
func tripped(opened <-chan struct{}) bool {
	select {
	case <-opened:
		return true
	default:
		return false
	}
}

// waitFor polls cond until it holds. The bound is generous on purpose: it
// only ends a test that would never succeed, and a loaded runner must not
// fail a correct one.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A getter (the public entity reads) keeps its behaviour: retries, and it
// neither reads the breaker nor is held back by it.
func TestMessagePath_GettersStillRetryAndIgnoreTheBreaker(t *testing.T) {
	r := newMessagePathRig(t)
	r.ctl.failing.Store(true)
	r.api.Breaker().Trip()

	id := types.URN{Prefix: "od", Type: "match", ID: 99}
	if _, err := r.ef.BuildMatch(t.Context(), id, []types.Locale{types.EnLocale}, nil); err == nil {
		t.Fatal("getter succeeded against a failing API")
	}
	if got := r.ctl.summaries.Load(); got != 3 {
		t.Fatalf("summary requests = %d, want 3 (the getter's attempts, whatever the breaker says)", got)
	}
}

// A message whose event is fully cached is built from the cache even while
// the breaker is open: skipping the API must not throw away what is known.
func TestMessagePath_BreakerOpenStillServesCachedEvents(t *testing.T) {
	r := newMessagePathRig(t)
	if ev := r.buildEvent(t, 99); ev == nil {
		t.Fatal("setup: event not built from a healthy API")
	}
	r.ctl.failing.Store(true)
	r.api.Breaker().Trip()
	before := r.ctl.all.Load()
	if ev := r.buildEvent(t, 99); ev == nil {
		t.Fatal("cached event refused while the breaker is open")
	}
	if got := r.ctl.all.Load(); got != before {
		t.Fatalf("API requests = %d, want none for a cached event", got-before)
	}
}

// A caller's own cancellation or expired deadline says nothing about the
// API: it neither trips the breaker nor, through the message's own timeout,
// counts as the API making a message wait. The API itself is healthy here,
// so a message whose event stayed unbuilt proves the build was attempted and
// failed on the caller's context.
func TestMessagePath_CancelledContextDoesNotOpenTheBreaker(t *testing.T) {
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
			if r.api.Breaker().Open() {
				t.Fatal("breaker open after a build on a dead context")
			}
		})
	}
}

// The session's cancellation while a message waits is not the API making it
// wait: the message leaves, the breaker stays closed.
func TestMessagePath_SessionCancelWhileWaitingDoesNotOpenTheBreaker(t *testing.T) {
	r := newTimedMessagePathRig(t, rigTiming{messageTimeout: time.Minute, breakerOpenFor: time.Minute})
	r.ctl.hanging.Store(true)

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		waitFor(t, "the match summary request", func() bool { return r.ctl.summaries.Load() >= 1 })
		cancel()
	}()
	out, err := r.factory.BuildMessage(ctx, oddsChangeFor(99))
	if err != nil {
		t.Fatalf("BuildMessage: %v", err)
	}
	if out.(oddsChangeImpl).event != nil {
		t.Fatal("event built from a hanging API")
	}
	if r.api.Breaker().Open() {
		t.Fatal("breaker open after the session cancelled the wait")
	}
}

// A closed client is shutdown, not an API outage: it must not open the
// breaker either.
func TestMessagePath_ClosedClientDoesNotOpenTheBreaker(t *testing.T) {
	r := newMessagePathRig(t)
	r.api.Close()
	if ev := r.buildEvent(t, 99); ev != nil {
		t.Fatalf("event = %v, want nil from a closed client", ev)
	}
	if r.api.Breaker().Open() {
		t.Fatal("breaker open after a closed-client failure")
	}
}

// An API that degrades into truncated bodies still answers 2xx headers.
// Those are not healthy answers: they must not close the breaker, or every
// message would pay its wait again while the API stays degraded.
func TestMessagePath_AnswerWithATruncatedBodyDoesNotCloseTheBreaker(t *testing.T) {
	r := newMessagePathRig(t)
	r.ctl.failing.Store(true)
	r.buildEvent(t, 99)
	if !r.api.Breaker().Open() {
		t.Fatal("setup: breaker closed after a failing build")
	}

	r.ctl.failing.Store(false)
	r.ctl.truncating.Store(true)
	if _, err := r.api.FetchSports(t.Context(), types.EnLocale); err == nil {
		t.Fatal("FetchSports succeeded on a truncated body")
	}
	if !r.api.Breaker().Open() {
		t.Fatal("a truncated answer closed the breaker")
	}
	if ev := r.buildEvent(t, 100); ev != nil {
		t.Fatalf("event = %v, want nil while the breaker is open", ev)
	}

	// A complete answer does close it.
	r.ctl.truncating.Store(false)
	if _, err := r.api.FetchSports(t.Context(), types.EnLocale); err != nil {
		t.Fatalf("FetchSports: %v", err)
	}
	if r.api.Breaker().Open() {
		t.Fatal("breaker still open once the API answered completely")
	}
	waitFor(t, "the event to build once the breaker closed", func() bool { return r.buildEvent(t, 100) != nil })
}

// A 404 for an event that is not in REST yet is an answer of a healthy API:
// it does not open the breaker, so the next message of the event tries the
// API again.
func TestMessagePath_NotFoundEventDoesNotOpenTheBreaker(t *testing.T) {
	r := newMessagePathRig(t)
	r.ctl.notFoundSummaries.Store(true)
	for range 3 {
		if ev := r.buildEvent(t, 99); ev != nil {
			t.Fatalf("event = %v, want nil for an event the API does not know", ev)
		}
	}
	if r.api.Breaker().Open() {
		t.Fatal("breaker open after a 404")
	}
	if got := r.ctl.summaries.Load(); got != 3 {
		t.Fatalf("match summary requests = %d, want 3 (every message tries the API)", got)
	}
}

func tournamentMessage(id int) *types.FeedMessage {
	m := oddsChangeFor(1)
	tid := types.URN{Prefix: "od", Type: "tournament", ID: id}
	m.RoutingKey.EventID = &tid
	m.RoutingKey.FullRoutingKey = "hi.-.live.odds_change.1." + tid.ToString()
	return m
}

// A tournament-routed message goes through the same timeout and breaker.
func TestMessagePath_TournamentRoutedMessage_BreakerOpensAndSkips(t *testing.T) {
	r := newMessagePathRig(t)
	r.ctl.failing.Store(true)

	for i := range 20 {
		out, err := r.factory.BuildMessage(t.Context(), tournamentMessage(7+i))
		if err != nil {
			t.Fatalf("BuildMessage: %v", err)
		}
		if ev := out.(oddsChangeImpl).event; ev != nil {
			t.Fatalf("event = %v, want nil", ev)
		}
	}
	if !r.api.Breaker().Open() {
		t.Fatal("breaker closed after a failing tournament build")
	}
	// One flight's attempts at most: the 19 messages after the first were
	// skipped, none started a flight of its own.
	if got := r.ctl.tournaments.Load(); got > 3 {
		t.Fatalf("tournament requests = %d, want at most 3 (one flight)", got)
	}
}

// BuildUnparsableMessage builds the event on the same path: a failing match
// opens the breaker, and the tournament build after it is skipped without a
// request.
func TestMessagePath_UnparsableMessages_BreakerOpensAndSkips(t *testing.T) {
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
	if !r.api.Breaker().Open() {
		t.Fatal("breaker closed after a failing unparsable build")
	}
	if got := r.ctl.summaries.Load(); got > 3 {
		t.Fatalf("match summary requests = %d, want at most 3 (one flight)", got)
	}
	if got := r.ctl.tournaments.Load(); got != 0 {
		t.Fatalf("tournament requests = %d, want none: the breaker was open", got)
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

// Market and outcome name resolution waits under the SAME timeout and gate
// as the event build. It is bounded by the message timeout, and once the
// API has failed it makes no request at all.
func TestMessagePath_MarketNameResolution_WaitsUnderTheMessageTimeoutAndGate(t *testing.T) {
	// Event cached, description endpoints failing: the message is delivered
	// with its market and the breaker opens at the first failed attempt.
	t.Run("descriptions failing", func(t *testing.T) {
		r := newMessagePathRig(t)
		if ev := r.buildEvent(t, 99); ev == nil {
			t.Fatal("setup: event not built from a healthy API")
		}
		r.ctl.failDescriptions.Store(true)

		out, err := r.factory.BuildMessage(t.Context(), oddsChangeWithDynamicMarket(99))
		if err != nil {
			t.Fatal(err)
		}
		if len(out.(oddsChangeImpl).markets) != 1 {
			t.Fatalf("markets = %d, want the message delivered with its market", len(out.(oddsChangeImpl).markets))
		}
		if !r.api.Breaker().Open() {
			t.Fatal("breaker closed after a failed name lookup")
		}
	})

	// A lookup that hangs ends at the message timeout; the message is
	// delivered without the name and the timeout trips the breaker.
	t.Run("descriptions hanging", func(t *testing.T) {
		r := newTimedMessagePathRig(t, rigTiming{messageTimeout: 200 * time.Millisecond, breakerOpenFor: time.Minute})
		if ev := r.buildEvent(t, 99); ev == nil {
			t.Fatal("setup: event not built from a healthy API")
		}
		r.ctl.descriptionDelay.Store(int64(time.Hour))

		out, err := r.factory.BuildMessage(t.Context(), oddsChangeWithDynamicMarket(99))
		if err != nil {
			t.Fatal(err)
		}
		mk := out.(oddsChangeImpl).markets
		if len(mk) != 1 {
			t.Fatalf("markets = %d, want the message delivered with its market", len(mk))
		}
		if _, ok := mk[0].Name(types.EnLocale).Get(); ok {
			t.Fatal("market has a name although its lookup never answered")
		}
		if !r.api.Breaker().Open() {
			t.Fatal("a name lookup that waited the whole message timeout did not trip the breaker")
		}
	})

	// Everything failing, event uncached: the event build opens the breaker;
	// the market lookups after it are skipped without a request.
	t.Run("API down", func(t *testing.T) {
		r := newMessagePathRig(t)
		r.ctl.failing.Store(true)
		out, err := r.factory.BuildMessage(t.Context(), oddsChangeWithDynamicMarket(99))
		if err != nil {
			t.Fatal(err)
		}
		if out.(oddsChangeImpl).event != nil {
			t.Fatal("event built from a failing API")
		}
		if got := r.ctl.descriptions.Load(); got != 0 {
			t.Fatalf("description requests = %d, want none after the event build opened the breaker", got)
		}
	})

	// Breaker open: no API request at all, market included.
	t.Run("breaker open", func(t *testing.T) {
		r := newMessagePathRig(t)
		if ev := r.buildEvent(t, 99); ev == nil {
			t.Fatal("setup: event not built from a healthy API")
		}
		r.api.Breaker().Trip()
		before := r.ctl.all.Load()
		for range 5 {
			if _, err := r.factory.BuildMessage(t.Context(), oddsChangeWithDynamicMarket(99)); err != nil {
				t.Fatal(err)
			}
		}
		if got := r.ctl.all.Load() - before; got != 0 {
			t.Fatalf("API requests = %d while the breaker is open, want none", got)
		}
	})
}

// Market and outcome name lookups swallow API failures (a missing name must
// not fail the message). A failed lookup reaches the breaker where it
// happens, so the next messages skip the API, and the first healthy answer
// anywhere closes it.
func TestMessagePath_MarketLookupFailure_OpensTheBreaker(t *testing.T) {
	r := newMessagePathRig(t)
	if ev := r.buildEvent(t, 99); ev == nil {
		t.Fatal("setup: event not built from a healthy API")
	}
	r.ctl.failDescriptions.Store(true)

	if _, err := r.factory.BuildMessage(t.Context(), oddsChangeWithDynamicMarket(99)); err != nil {
		t.Fatal(err)
	}
	if r.ctl.descriptions.Load() < 1 {
		t.Fatal("the first message made no description request")
	}
	if !r.api.Breaker().Open() {
		t.Fatal("breaker closed after a failed market lookup")
	}

	// The flight of the failed lookup retries on its own (three attempts in
	// all); let it end, then count what the following messages add.
	waitFor(t, "the lookup's attempts to end", func() bool { return r.ctl.descriptions.Load() >= 3 })
	settled := r.ctl.descriptions.Load()
	for range 5 {
		if _, err := r.factory.BuildMessage(t.Context(), oddsChangeWithDynamicMarket(99)); err != nil {
			t.Fatal(err)
		}
	}
	if got := r.ctl.descriptions.Load(); got != settled {
		t.Fatalf("description requests grew from %d to %d while the breaker was open", settled, got)
	}

	// A healthy answer from any caller closes it: the next message tries again.
	// (A direct client call: a cached getter would make no request.)
	if _, err := r.api.FetchSports(t.Context(), types.EnLocale); err != nil {
		t.Fatal(err)
	}
	if r.api.Breaker().Open() {
		t.Fatal("breaker still open after a healthy answer")
	}
	if _, err := r.factory.BuildMessage(t.Context(), oddsChangeWithDynamicMarket(99)); err != nil {
		t.Fatal(err)
	}
	if got := r.ctl.descriptions.Load(); got <= settled {
		t.Fatalf("description requests = %d after a healthy answer closed the breaker, want a new attempt", got)
	}
}

const playerPropsCatalog = `<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="7" name="{player} kills" groups="all|player_props">
    <specifiers><specifier name="player" type="string"/></specifiers>
    <outcomes><outcome id="1" name="yes"/></outcomes>
  </market>
</market_descriptions>`

func oddsChangeWithPlayerProps(eventNum int) *types.FeedMessage {
	m := oddsChangeFor(eventNum)
	m.Message.(*feedXML.OddsChange).Odds.Markets = []*feedXML.MarketWithOutcome{
		feedMarket(7, "player=od:player:301", "1"),
	}
	return m
}

// The player lookup of a player-props market name is a name lookup like the
// others: when the player endpoint says the API is unavailable the first
// message opens the breaker and the following ones make no player request;
// a 404 answer of a healthy API does not open it.
func TestMessagePath_PlayerPropsNameLookup(t *testing.T) {
	setup := func(t *testing.T) *messagePathRig {
		t.Helper()
		r := newMessagePathRig(t)
		if ev := r.buildEvent(t, 99); ev == nil {
			t.Fatal("setup: event not built from a healthy API")
		}
		c := playerPropsCatalog
		r.ctl.catalog.Store(&c)
		return r
	}

	t.Run("server error opens the breaker", func(t *testing.T) {
		r := setup(t)
		r.ctl.failPlayers.Store(true)
		if _, err := r.factory.BuildMessage(t.Context(), oddsChangeWithPlayerProps(99)); err != nil {
			t.Fatal(err)
		}
		if r.ctl.players.Load() < 1 {
			t.Fatal("the first message made no player request")
		}
		if !r.api.Breaker().Open() {
			t.Fatal("breaker closed after a failed player lookup")
		}
		waitFor(t, "the lookup's attempts to end", func() bool { return r.ctl.players.Load() >= 3 })
		settled := r.ctl.players.Load()
		for range 5 {
			if _, err := r.factory.BuildMessage(t.Context(), oddsChangeWithPlayerProps(99)); err != nil {
				t.Fatal(err)
			}
		}
		if got := r.ctl.players.Load(); got != settled {
			t.Fatalf("player requests grew from %d to %d while the breaker was open", settled, got)
		}
	})

	t.Run("not found does not open the breaker", func(t *testing.T) {
		r := setup(t)
		r.ctl.notFoundPlayers.Store(true)
		before := r.ctl.players.Load()
		if _, err := r.factory.BuildMessage(t.Context(), oddsChangeWithPlayerProps(99)); err != nil {
			t.Fatal(err)
		}
		if r.ctl.players.Load() == before {
			t.Fatal("no player request was made")
		}
		if r.api.Breaker().Open() {
			t.Fatal("breaker open after a player not-found")
		}
	})
}

// A lookup the API answers with "not found" says nothing about its health and
// must not open the breaker.
func TestMessagePath_MarketLookupNotFound_DoesNotOpenTheBreaker(t *testing.T) {
	r := newMessagePathRig(t)
	if ev := r.buildEvent(t, 99); ev == nil {
		t.Fatal("setup: event not built from a healthy API")
	}
	// The fixture server answers description requests with 404 (healthy API).
	if _, err := r.factory.BuildMessage(t.Context(), oddsChangeWithDynamicMarket(99)); err != nil {
		t.Fatal(err)
	}
	if r.api.Breaker().Open() {
		t.Fatal("breaker open after a not-found lookup")
	}
}

func dynMarketSpec() *string { s := "variant=od:dynamic_outcomes:99"; return &s }

type marketMessageCase struct {
	name    string
	msg     func() types.BasicMessage
	markets func(out any) int
}

// marketMessageCases is one message of each market-bearing type, each with a
// dynamic market of event 99.
func marketMessageCases() []marketMessageCase {
	attrs := feedXML.MessageAttributes{Product: 1, EventID: "od:match:99"}
	mkAttrs := func() feedXML.MarketAttributes { return feedXML.MarketAttributes{ID: 3, Specifiers: dynMarketSpec()} }
	return []marketMessageCase{
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
}

// Every market-bearing message type resolves names under the message timeout
// and gate: with a cached event and a failing description endpoint the
// message is delivered, the failure opens the breaker, and the next message
// makes no request.
func TestMessagePath_AllMarketMessageTypes_WaitUnderTheMessageTimeoutAndGate(t *testing.T) {
	for _, tc := range marketMessageCases() {
		t.Run(tc.name, func(t *testing.T) {
			r := newMessagePathRig(t)
			if ev := r.buildEvent(t, 99); ev == nil {
				t.Fatal("setup: event not built from a healthy API")
			}
			r.ctl.failDescriptions.Store(true)

			build := func() any {
				m := oddsChangeFor(99)
				m.Message = tc.msg()
				out, err := r.factory.BuildMessage(t.Context(), m)
				if err != nil {
					t.Fatalf("BuildMessage: %v", err)
				}
				return out
			}
			out := build()
			if n := tc.markets(out); n != 1 {
				t.Fatalf("delivered markets = %d, want 1", n)
			}
			if !r.api.Breaker().Open() {
				t.Fatal("breaker closed after a failed market lookup")
			}
			waitFor(t, "the lookup's attempts to end", func() bool { return r.ctl.descriptions.Load() >= 3 })
			settled := r.ctl.descriptions.Load()
			build()
			if got := r.ctl.descriptions.Load(); got != settled {
				t.Fatalf("a second message made %d more description request(s) while the breaker was open", got-settled)
			}
		})
	}
}

// A name lookup that hangs is bounded by the message timeout for every
// market-bearing message type: the build returns shortly after the timeout,
// the market is delivered without a name, and the timeout trips the breaker.
func TestMessagePath_AllMarketMessageTypes_HangingNameLookupIsBoundedByTheMessageTimeout(t *testing.T) {
	const timeout = 200 * time.Millisecond
	// Without the timeout the lookup would wait an hour; the slack only
	// absorbs a loaded machine.
	const bound = timeout + 3*time.Second
	for _, tc := range marketMessageCases() {
		t.Run(tc.name, func(t *testing.T) {
			r := newTimedMessagePathRig(t, rigTiming{messageTimeout: timeout, breakerOpenFor: time.Minute})
			if ev := r.buildEvent(t, 99); ev == nil {
				t.Fatal("setup: event not built from a healthy API")
			}
			r.ctl.descriptionDelay.Store(int64(time.Hour))
			r.settle(t)
			opened := r.api.Breaker().Opened()

			m := oddsChangeFor(99)
			m.Message = tc.msg()
			start := time.Now()
			out, err := r.factory.BuildMessage(t.Context(), m)
			if err != nil {
				t.Fatalf("BuildMessage: %v", err)
			}
			if elapsed := time.Since(start); elapsed > bound {
				t.Fatalf("build took %v, want about the message timeout (%v)", elapsed, timeout)
			}
			if n := tc.markets(out); n != 1 {
				t.Fatalf("delivered markets = %d, want 1", n)
			}
			if !tripped(opened) {
				t.Fatal("a name lookup that waited the whole message timeout did not trip the breaker")
			}
		})
	}
}

// An event the API answers with 404 (not in REST yet) says nothing about the
// description API: the message's market names still resolve. A failure that
// says the API is unavailable makes no further request.
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
