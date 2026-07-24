package gosdk

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/types"
)

// catalogFixtureServer extends the fixture-server pattern from
// client_test.go's fullFixtureServer with the per-event endpoints
// needed to exercise Match / Competitor / Player / Tournament / Schedule
// methods on *Client. Self-contained so each test can opt in / out
// without affecting tests that use fullFixtureServer.
//
// Endpoints handled (returning the canned bodies below):
//
//	GET /users/whoami                            (boot probe)
//	GET /descriptions/producers                  (Producers boot)
//	GET /sports/{l}/sports                       (Sports / Sport)
//	GET /sports/{l}/sports/{sportID}/tournaments
//	GET /sports/{l}/tournaments/{id}/info
//	GET /sports/{l}/sport_events/{id}/summary    (Match)
//	GET /sports/{l}/sport_events/{id}/fixture    (eager-load on Match)
//	GET /sports/{l}/competitors/{id}/profile     (Competitor)
//	GET /sports/{l}/players/{id}/profile         (Player)
//	GET /sports/{l}/schedules/live/schedule      (LiveMatches)
//	GET /sports/{l}/schedules/{date}/schedule    (MatchesFor)
//	GET /sports/{l}/schedules/pre/schedule?...   (ListMatches)
//	GET /sports/{l}/fixtures/changes             (FixtureChanges)
//	GET /descriptions/{l}/markets                (MarketDescriptions)
//	GET /descriptions/{l}/match_status           (touched by status decode)
//	GET /void_reasons                            (void reasons)
//	GET /replay                                  (Replay.List / EventIDs)
//	GET /replay/status                           (Replay.Status)
//	POST /replay/play                            (Replay.Start)
//	POST /replay/stop                            (Replay.Stop)
//	POST /replay/clear                           (Replay.Clear)
//	PUT /replay/events/{id}                      (Replay.AddEvent)
//	DELETE /replay/events/{id}                   (Replay.RemoveEvent)
func catalogFixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	const producersBody = `<?xml version="1.0"?>
<producers response_code="OK">
  <producer id="1" name="live" description="Live" active="true" api_url="https://x" scope="live" stateful_recovery_window_in_minutes="60"/>
</producers>`

	const sportsBody = `<?xml version="1.0"?>
<sports generated_at="2026-01-01T00:00:00">
  <sport id="od:sport:1" name="Soccer" abbreviation="SOC"/>
  <sport id="od:sport:5" name="Tennis" abbreviation="TEN"/>
</sports>`

	// Template — echoes the REQUESTED sport id (identity-validated by
	// the API client, same as the competitor profile above).
	const sportTournamentsBodyTmpl = `<?xml version="1.0"?>
<sport_tournaments generated_at="2026-01-01T00:00:00">
  <sport id="%[1]s" name="Soccer" abbreviation="SOC"/>
  <tournaments>
    <tournament id="od:tournament:1" name="Premier League" abbreviation="PL" risk_tier="1">
      <sport id="%[1]s" name="Soccer" abbreviation="SOC"/>
    </tournament>
  </tournaments>
</sport_tournaments>`

	const tournamentInfoBody = `<?xml version="1.0"?>
<tournament_info generated_at="2026-01-01T00:00:00">
  <tournament id="od:tournament:1" name="Premier League" abbreviation="PL" risk_tier="1">
    <sport id="od:sport:1" name="Soccer" abbreviation="SOC"/>
  </tournament>
</tournament_info>`

	const matchSummaryBody = `<?xml version="1.0"?>
<match_summary generated_at="2026-01-01T00:00:00">
  <sport_event id="od:match:42" name="Home vs Away" scheduled="2026-01-01T20:00:00" liveodds="booked">
    <tournament id="od:tournament:1" name="Premier League" abbreviation="PL" risk_tier="1">
      <sport id="od:sport:1" name="Soccer" abbreviation="SOC"/>
    </tournament>
    <competitors>
      <competitor id="od:competitor:10" name="Home" abbreviation="HOM" qualifier="home"/>
      <competitor id="od:competitor:11" name="Away" abbreviation="AWY" qualifier="away"/>
    </competitors>
  </sport_event>
  <sport_event_status status="live" match_status_code="6" home_score="1" away_score="0" scoreboard_available="false"/>
</match_summary>`

	const fixtureBody = `<?xml version="1.0"?>
<fixtures_fixture generated_at="2026-01-01T00:00:00">
  <fixture id="od:match:42" name="Home vs Away" scheduled="2026-01-01T20:00:00" start_time="2026-01-01T20:00:00">
    <tournament id="od:tournament:1" name="Premier League" abbreviation="PL" risk_tier="1">
      <sport id="od:sport:1" name="Soccer" abbreviation="SOC"/>
    </tournament>
    <competitors>
      <competitor id="od:competitor:10" name="Home" abbreviation="HOM" qualifier="home"/>
      <competitor id="od:competitor:11" name="Away" abbreviation="AWY" qualifier="away"/>
    </competitors>
  </fixture>
</fixtures_fixture>`

	// Template — the fixture echoes the REQUESTED competitor id/name:
	// the API client validates response identity, and a static
	// one-body-for-all-ids reply is exactly the misrouted-response
	// class it now rejects.
	const competitorProfileBodyTmpl = `<?xml version="1.0"?>
<competitor_profile>
  <competitor id="%s" name="%s" abbreviation="%s" country="GB" country_code="GB"/>
  <players>
    <player id="od:player:100" name="Striker One" full_name="Striker One" sport="od:sport:1"/>
  </players>
</competitor_profile>`

	const playerProfileBody = `<?xml version="1.0"?>
<player_profile>
  <player id="od:player:100" name="Striker One" full_name="Striker One" sport="od:sport:1"/>
</player_profile>`

	const liveScheduleBody = `<?xml version="1.0"?>
<schedule generated_at="2026-01-01T00:00:00">
  <sport_event id="od:match:42" name="Home vs Away" scheduled="2026-01-01T20:00:00" liveodds="booked">
    <tournament id="od:tournament:1" name="Premier League" abbreviation="PL" risk_tier="1">
      <sport id="od:sport:1" name="Soccer" abbreviation="SOC"/>
    </tournament>
    <competitors>
      <competitor id="od:competitor:10" name="Home" abbreviation="HOM" qualifier="home"/>
      <competitor id="od:competitor:11" name="Away" abbreviation="AWY" qualifier="away"/>
    </competitors>
  </sport_event>
</schedule>`

	const fixtureChangesBody = `<?xml version="1.0"?>
<fixture_changes generated_at="2026-01-01T00:00:00">
  <fixture_change sport_event_id="od:match:42" update_time="2026-01-01T10:00:00"/>
</fixture_changes>`

	const marketsBody = `<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="1" name="1x2" groups="all">
    <outcomes>
      <outcome id="1" name="home"/>
      <outcome id="2" name="away"/>
    </outcomes>
  </market>
</market_descriptions>`

	const matchStatusDescBody = `<?xml version="1.0"?>
<match_status_descriptions response_code="OK">
  <match_status id="6" description="1st period"/>
</match_status_descriptions>`

	const voidReasonsBody = `<?xml version="1.0"?>
<void_reasons response_code="OK">
  <void_reason id="1" name="canceled" description="Canceled" template="Canceled"/>
</void_reasons>`

	const replayListBody = `<?xml version="1.0"?>
<replay_set_content>
  <replay_event id="od:match:42" position="0"/>
</replay_set_content>`

	const replayStatusBody = `<?xml version="1.0"?>
<player_status status="stopped"/>`

	const okEnvelopeBody = `<?xml version="1.0"?>
<response response_code="OK"><action>ok</action></response>`

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		path := r.URL.Path

		// Replay endpoints (paths don't contain /sports/{l}/... prefix).
		switch {
		case path == "/v1/replay" && r.Method == http.MethodGet:
			_, _ = io.WriteString(w, replayListBody)
			return
		case path == "/v1/replay/status" && r.Method == http.MethodGet:
			_, _ = io.WriteString(w, replayStatusBody)
			return
		case path == "/v1/replay/play" && r.Method == http.MethodPost:
			_, _ = io.WriteString(w, okEnvelopeBody)
			return
		case path == "/v1/replay/stop" && r.Method == http.MethodPost:
			_, _ = io.WriteString(w, okEnvelopeBody)
			return
		case path == "/v1/replay/clear" && r.Method == http.MethodPost:
			_, _ = io.WriteString(w, okEnvelopeBody)
			return
		case strings.HasPrefix(path, "/v1/replay/events/"):
			// PUT (Add) or DELETE (Remove) — both return OK.
			_, _ = io.WriteString(w, okEnvelopeBody)
			return
		}

		switch {
		case strings.HasSuffix(path, "/users/whoami"):
			_, _ = io.WriteString(w, whoAmIBody)
		case strings.HasSuffix(path, "/descriptions/producers"):
			_, _ = io.WriteString(w, producersBody)
		case strings.HasSuffix(path, "/sports") && !strings.Contains(path, "/sport_events/"):
			// Matches /sports/{l}/sports (sports list) AND
			// /sports/{l}/sports/{sportID}/tournaments collisions are
			// handled by the more specific /tournaments suffix below.
			if strings.HasSuffix(path, "/tournaments") {
				_, _ = fmt.Fprintf(w, sportTournamentsBodyTmpl, sportIDFromTournamentsPath(path))
				return
			}
			_, _ = io.WriteString(w, sportsBody)
		case strings.HasSuffix(path, "/tournaments") && strings.Contains(path, "/sports/od:sport:"):
			_, _ = fmt.Fprintf(w, sportTournamentsBodyTmpl, sportIDFromTournamentsPath(path))
		case strings.HasSuffix(path, "/info") && strings.Contains(path, "/tournaments/"):
			_, _ = io.WriteString(w, tournamentInfoBody)
		case strings.HasSuffix(path, "/summary") && strings.Contains(path, "/sport_events/"):
			_, _ = io.WriteString(w, matchSummaryBody)
		case strings.HasSuffix(path, "/fixture") && strings.Contains(path, "/sport_events/"):
			_, _ = io.WriteString(w, fixtureBody)
		case strings.HasSuffix(path, "/profile") && strings.Contains(path, "/competitors/"):
			parts := strings.Split(strings.TrimSuffix(path, "/profile"), "/")
			id := parts[len(parts)-1]
			name, abbr := "Home", "HOM"
			if strings.HasSuffix(id, ":11") {
				name, abbr = "Away", "AWY"
			}
			_, _ = fmt.Fprintf(w, competitorProfileBodyTmpl, id, name, abbr)
		case strings.HasSuffix(path, "/profile") && strings.Contains(path, "/players/"):
			_, _ = io.WriteString(w, playerProfileBody)
		case strings.HasSuffix(path, "/schedules/live/schedule"):
			_, _ = io.WriteString(w, liveScheduleBody)
		case strings.HasSuffix(path, "/schedule") && strings.Contains(path, "/schedules/"):
			// Both /schedules/{date}/schedule (MatchesFor) and
			// /schedules/pre/schedule (ListMatches) hit this branch.
			_, _ = io.WriteString(w, liveScheduleBody)
		case strings.HasSuffix(path, "/fixtures/changes"):
			_, _ = io.WriteString(w, fixtureChangesBody)
		case strings.HasSuffix(path, "/markets"):
			_, _ = io.WriteString(w, marketsBody)
		case strings.Contains(path, "/match_status"):
			_, _ = io.WriteString(w, matchStatusDescBody)
		case strings.HasSuffix(path, "/void_reasons"):
			_, _ = io.WriteString(w, voidReasonsBody)
		default:
			t.Logf("catalogFixtureServer: unhandled %s %s", r.Method, path)
			http.NotFound(w, r)
		}
	}))
}

// newCatalogClient constructs a *Client wired to catalogFixtureServer.
func newCatalogClient(t *testing.T) *Client {
	t.Helper()
	srv := catalogFixtureServer(t)
	t.Cleanup(srv.Close)

	cfg := NewConfig("t", types.IntegrationEnvironment,
		WithAPIHost("api.example.test"),
		WithHTTPClient(newTestHTTPClient(srv)),
	)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	c, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	closeClientOnCleanup(t, c)
	return c
}

// --- single-entity catalog methods ---

func TestClient_Sport_Single(t *testing.T) {
	c := newCatalogClient(t)
	urn, _ := types.ParseURN("od:sport:1")
	s, err := c.Sport(t.Context(), *urn)
	if err != nil {
		t.Fatalf("Sport: %v", err)
	}
	if s.ID != *urn {
		t.Errorf("Sport.ID = %v, want %v", s.ID, *urn)
	}
}

func TestClient_Player_Single(t *testing.T) {
	c := newCatalogClient(t)
	urn, _ := types.ParseURN("od:player:100")
	p, err := c.Player(t.Context(), *urn)
	if err != nil {
		t.Fatalf("Player: %v", err)
	}
	if p.ID != urn.ToString() {
		t.Errorf("Player.ID = %q, want %q", p.ID, urn.ToString())
	}
}

func TestClient_Tournament_Single(t *testing.T) {
	c := newCatalogClient(t)
	urn, _ := types.ParseURN("od:tournament:1")
	tour, err := c.Tournament(t.Context(), *urn)
	if err != nil {
		t.Fatalf("Tournament: %v", err)
	}
	if tour.ID != *urn {
		t.Errorf("Tournament.ID = %v, want %v", tour.ID, *urn)
	}
}

func TestClient_Match_Single(t *testing.T) {
	c := newCatalogClient(t)
	urn, _ := types.ParseURN("od:match:42")
	m, err := c.Match(t.Context(), *urn)
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if m.ID != *urn {
		t.Errorf("Match.ID = %v, want %v", m.ID, *urn)
	}
	// Match build does eager fan-out — verify tournament + competitors
	// resolved so we know the full pipeline ran.
	if m.Tournament.ID.ToString() != "od:tournament:1" {
		t.Errorf("Match.Tournament.ID = %v", m.Tournament.ID)
	}
	if len(m.Competitors) != 2 {
		t.Errorf("Match competitors = %d, want 2", len(m.Competitors))
	}
}

func TestClient_Competitor_Single(t *testing.T) {
	c := newCatalogClient(t)
	urn, _ := types.ParseURN("od:competitor:10")
	comp, err := c.Competitor(t.Context(), *urn)
	if err != nil {
		t.Fatalf("Competitor: %v", err)
	}
	if comp.ID != *urn {
		t.Errorf("Competitor.ID = %v, want %v", comp.ID, *urn)
	}
}

// --- schedule-style methods ---

func TestClient_LiveMatches(t *testing.T) {
	c := newCatalogClient(t)
	matches, err := c.LiveMatches(t.Context())
	if err != nil {
		t.Fatalf("LiveMatches: %v", err)
	}
	if len(matches) == 0 {
		t.Errorf("LiveMatches returned no matches; want at least 1")
	}
}

func TestClient_MatchesFor(t *testing.T) {
	c := newCatalogClient(t)
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	matches, err := c.MatchesFor(t.Context(), when)
	if err != nil {
		t.Fatalf("MatchesFor: %v", err)
	}
	if len(matches) == 0 {
		t.Errorf("MatchesFor returned no matches; want at least 1")
	}
}

func TestClient_ListMatches(t *testing.T) {
	c := newCatalogClient(t)
	matches, err := c.ListMatches(t.Context(), 0, 50)
	if err != nil {
		t.Fatalf("ListMatches: %v", err)
	}
	if len(matches) == 0 {
		t.Errorf("ListMatches returned no matches; want at least 1")
	}
}

// TestClient_ActiveTournamentsForSport hits the sport-name lookup path.
func TestClient_ActiveTournamentsForSport(t *testing.T) {
	c := newCatalogClient(t)
	tours, err := c.ActiveTournamentsForSport(t.Context(), "Soccer")
	if err != nil {
		t.Fatalf("ActiveTournamentsForSport: %v", err)
	}
	if len(tours) == 0 {
		t.Errorf("ActiveTournamentsForSport returned no tournaments; want at least 1")
	}
}

// --- Replay subtype ---

func TestClient_Replay_List(t *testing.T) {
	c := newCatalogClient(t)
	r := c.Replay()
	// The fixture serves od:match:42 on /replay AND a resolvable match
	// summary for it, so List must succeed deterministically — the
	// previous log-and-tolerate shape could stay green with List broken.
	events, err := r.List(t.Context())
	if err != nil {
		t.Fatalf("Replay.List: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("Replay.List returned %d events, want 1", len(events))
	}
	if got := events[0].ID.ToString(); got != "od:match:42" {
		t.Fatalf("Replay.List[0].ID = %s, want od:match:42", got)
	}
}

func TestClient_Replay_EventIDs(t *testing.T) {
	c := newCatalogClient(t)
	r := c.Replay()
	ids, err := r.EventIDs(t.Context())
	if err != nil {
		t.Fatalf("Replay.EventIDs: %v", err)
	}
	if len(ids) == 0 {
		t.Errorf("Replay.EventIDs returned 0; want at least 1")
	}
}

func TestClient_Replay_Status(t *testing.T) {
	c := newCatalogClient(t)
	r := c.Replay()
	if _, err := r.Status(t.Context()); err != nil {
		t.Fatalf("Replay.Status: %v", err)
	}
}

func TestClient_Replay_AddEvent_RemoveEvent(t *testing.T) {
	c := newCatalogClient(t)
	r := c.Replay()
	urn, _ := types.ParseURN("od:match:42")
	if err := r.AddEvent(t.Context(), *urn); err != nil {
		t.Fatalf("AddEvent: %v", err)
	}
	if err := r.RemoveEvent(t.Context(), *urn); err != nil {
		t.Fatalf("RemoveEvent: %v", err)
	}
}

func TestClient_Replay_StartStop(t *testing.T) {
	c := newCatalogClient(t)
	r := c.Replay()
	if err := r.Start(t.Context(),
		WithReplaySpeed(10),
		WithReplayMaxDelayMs(50),
		WithReplayRunParallel(true),
		WithReplayRewriteTimestamps(false),
		WithReplayProducer("live"),
	); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := r.Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestClient_Replay_StopAndClear(t *testing.T) {
	c := newCatalogClient(t)
	r := c.Replay()
	if err := r.StopAndClear(t.Context()); err != nil {
		t.Fatalf("StopAndClear: %v", err)
	}
}

func TestClient_Replay_Clear(t *testing.T) {
	c := newCatalogClient(t)
	r := c.Replay()
	if err := r.Clear(t.Context()); err != nil {
		t.Fatalf("Clear: %v", err)
	}
}

// --- ProducerStatus polling getter ---

func TestClient_ProducerStatus_PollingGetter(t *testing.T) {
	c := newCatalogClient(t)
	// Without Connect, the recovery actor never spawned for any
	// producer, so ProducerStatus returns false for everything —
	// including ids that DO exist in the producer manager. That's
	// the documented "no status until subscribed" contract.
	if _, ok := c.ProducerStatus(99999); ok {
		t.Errorf("ProducerStatus(99999) = true, want false for unknown id")
	}
	if _, ok := c.ProducerStatus(1); ok {
		t.Errorf("ProducerStatus(1) = true pre-Connect, want false")
	}
}

// --- additional Clear method coverage (already-tested ones share a body) ---

func TestClient_AdditionalClearMethods(t *testing.T) {
	c := newCatalogClient(t)
	urn, _ := types.ParseURN("od:match:1")
	c.ClearFixture(*urn)
	c.ClearMatchStatus(*urn)
	c.ClearPlayer(*urn)
	c.ClearSport(*urn)
	c.ClearMarketVoidReasons()
}

// sportIDFromTournamentsPath extracts {sportID} from
// /sports/{l}/sports/{sportID}/tournaments so the fixture can echo the
// requested identity.
func sportIDFromTournamentsPath(path string) string {
	parts := strings.Split(strings.TrimSuffix(path, "/tournaments"), "/")
	return parts[len(parts)-1]
}
