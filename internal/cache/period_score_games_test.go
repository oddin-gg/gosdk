package cache

import (
	"context"
	encodingxml "encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

// TestBuildMatchStatus_PeriodScoreGames covers period_score/@home_games, which
// set-based classic sports report per set. The feed publishes both numbers on
// the same row: home_score/away_score is the running sets-won tally, while
// home_games/away_games is the score of that set — a live tennis match with the
// first set won 6:4 arrives as
//
//	<period_score type="set" number="1" match_status_code="200"
//	              home_score="1" away_score="0" home_games="6" away_games="4"/>
//
// Before games were modelled, the attribute was dropped during XML decoding and
// consumers could only read the tally, so a per-set row rendered 1:0 instead of
// 6:4.
func TestBuildMatchStatus_PeriodScoreGames(t *testing.T) {
	body := `<?xml version="1.0"?>
<match_summary generated_at="2026-01-01T00:00:00Z">
  <sport_event id="od:match:9" name="X" scheduled="2026-01-01T12:00:00Z">
    <tournament id="od:tournament:1"><sport id="od:sport:1"/></tournament>
  </sport_event>
  <sport_event_status status="live" match_status_code="201" scoreboard_available="true" home_score="1.0" away_score="0.0">
    <period_scores>
      <period_score type="set" number="1" match_status_code="200" home_score="1.0" away_score="0.0" home_games="6" away_games="4"/>
      <period_score type="set" number="2" match_status_code="201" home_score="1.0" away_score="0.0" home_games="0" away_games="1"/>
      <period_score type="map" number="1" match_status_code="100" home_score="1.0" away_score="0.0" home_won_rounds="13"/>
    </period_scores>
  </sport_event_status>
</match_summary>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	apiC := newAPIClientForTest(t, srv)
	cfg := &minimalCfg{apiURL: srv.URL, token: "tok"}
	c := newMatchStatusCache(t.Context(), apiC, cfg, log.New(nil))

	urn, _ := types.ParseURN("od:match:9")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	status, err := BuildMatchStatus(ctx, c, nil, *urn, []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("BuildMatchStatus: %v", err)
	}
	if len(status.PeriodScores) != 3 {
		t.Fatalf("PeriodScores length = %d, want 3", len(status.PeriodScores))
	}

	tests := []struct {
		name      string
		index     int
		wantGames bool
		wantHome  int
		wantAway  int
	}{
		{name: "completed set keeps its games", index: 0, wantGames: true, wantHome: 6, wantAway: 4},
		{name: "set in progress carries current games", index: 1, wantGames: true, wantHome: 0, wantAway: 1},
		{name: "period without games leaves them unset", index: 2, wantGames: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ps := status.PeriodScores[tt.index]
			home, homeSet := ps.HomeGames.Get()
			away, awaySet := ps.AwayGames.Get()

			if homeSet != tt.wantGames || awaySet != tt.wantGames {
				t.Fatalf("games set = (%v, %v), want (%v, %v)", homeSet, awaySet, tt.wantGames, tt.wantGames)
			}
			if !tt.wantGames {
				return
			}
			if home != tt.wantHome || away != tt.wantAway {
				t.Errorf("games = %d:%d, want %d:%d", home, away, tt.wantHome, tt.wantAway)
			}
			// The tally on the same row must stay readable and independent.
			if ps.HomeScore != 1 || ps.AwayScore != 0 {
				t.Errorf("sets-won tally = %v:%v, want 1:0", ps.HomeScore, ps.AwayScore)
			}
		})
	}
}

// TestOnFeedMessage_PeriodScoreGames covers the same attribute on the feed path,
// which is where set scores actually move: odds_change arrives on every point,
// while the API summary is only refetched occasionally. The payload is a live
// tennis message as published on the live stream — note the feed spells the
// status attributes differently from the API (status="1", match_status="201").
func TestOnFeedMessage_PeriodScoreGames(t *testing.T) {
	raw := `<odds_change event_id="od:match:11" product="1" timestamp="1785831336922">
  <sport_event_status status="1" match_status="201" scoreboard_available="true" home_score="1" away_score="0">
    <period_scores>
      <period_score type="set" number="1" match_status_code="200" home_score="1" away_score="0" home_games="6" away_games="4"/>
      <period_score type="set" number="2" match_status_code="201" home_score="1" away_score="0" home_games="0" away_games="1"/>
      <period_score type="set" number="3" match_status_code="202" home_score="1" away_score="0" home_games="0" away_games="0"/>
    </period_scores>
    <scoreboard home_points="40" away_points="40" home_games="0" away_games="1"/>
  </sport_event_status>
  <odds/>
</odds_change>`

	var oddsChange feedXML.OddsChange
	if err := encodingxml.Unmarshal([]byte(raw), &oddsChange); err != nil {
		t.Fatalf("unmarshal odds_change: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	apiC := newAPIClientForTest(t, srv)
	cfg := &minimalCfg{apiURL: srv.URL, token: "tok"}
	c := newMatchStatusCache(t.Context(), apiC, cfg, log.New(nil))

	urn, _ := types.ParseURN("od:match:11")
	c.OnFeedMessage(*urn, &types.FeedMessage{
		BasicFeedMessage: types.BasicFeedMessage{
			Timestamp: types.MessageTimestamp{Created: time.Unix(1785831336, 0)},
		},
		Message: &oddsChange,
	})

	item, ok := c.lookup(*urn)
	if !ok {
		t.Fatal("match status not cached after OnFeedMessage")
	}
	if len(item.periodScores) != 3 {
		t.Fatalf("periodScores length = %d, want 3", len(item.periodScores))
	}

	wantGames := [][2]int{{6, 4}, {0, 1}, {0, 0}}
	for i, want := range wantGames {
		ps := item.periodScores[i]
		home, homeSet := ps.HomeGames.Get()
		away, awaySet := ps.AwayGames.Get()
		if !homeSet || !awaySet {
			t.Errorf("set %d: games unset, want %d:%d", i+1, want[0], want[1])
			continue
		}
		if home != want[0] || away != want[1] {
			t.Errorf("set %d: games = %d:%d, want %d:%d", i+1, home, away, want[0], want[1])
		}
		// Same row, different meaning: the tally must not be confused with games.
		if ps.HomeScore != 1 || ps.AwayScore != 0 {
			t.Errorf("set %d: sets-won tally = %v:%v, want 1:0", i+1, ps.HomeScore, ps.AwayScore)
		}
	}

	// The scoreboard keeps reporting the set in progress, unchanged by this.
	if item.scoreboard == nil {
		t.Fatal("scoreboard nil")
	}
	if games, set := item.scoreboard.AwayGames.Get(); !set || games != 1 {
		t.Errorf("scoreboard away games = %v (set=%v), want 1", games, set)
	}
}
