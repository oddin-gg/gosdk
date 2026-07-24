package cache

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

// TestBuildMatchStatus_ScoreboardInnerFieldsNotAliased is the
// regression for the v2.25 follow-up finding: pre-v2.26, BuildMatchStatus
// shallow-cloned the *Scoreboard outer pointer (via clonePtr) but the
// returned Scoreboard's inner *int/*int32/*bool fields still aliased
// the cache's pointees. A caller doing
// `*status.Scoreboard.HomeKills = 999` mutated the cache for every other
// reader; a second read by the SAME caller would observe the mutation.
//
// v2.26 fix: Scoreboard / Statistics / PeriodScore inner fields migrated
// to types.Optional[T] (a value-type wrapper). A snapshot copy of the
// Scoreboard pointee is now fully decoupled from the cache.
//
// Strategy: stand up an API fixture that returns a populated Scoreboard,
// fetch the MatchStatus, mutate the snapshot's Optional fields, then
// fetch again and verify the cache returned the original values.
func TestBuildMatchStatus_ScoreboardInnerFieldsNotAliased(t *testing.T) {
	body := `<?xml version="1.0"?>
<match_summary generated_at="2026-01-01T00:00:00Z">
  <sport_event id="od:match:7" name="X" scheduled="2026-01-01T12:00:00Z">
    <tournament id="od:tournament:1"><sport id="od:sport:1"/></tournament>
  </sport_event>
  <sport_event_status status="live" match_status_code="6" scoreboard_available="true" home_score="3.0" away_score="2.0">
    <scoreboard home_kills="10" away_kills="7" home_won_rounds="3" away_won_rounds="2" home_goals="4" away_goals="2" current_round="5"/>
  </sport_event_status>
</match_summary>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	apiC := newAPIClientForTest(t, srv)
	cfg := &minimalCfg{apiURL: srv.URL, token: "tok"}
	c := newMatchStatusCache(t.Context(), apiC, cfg, log.New(nil))

	urn, _ := types.ParseURN("od:match:7")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	// Populate the cache via the FetchMatchSummary observer path.
	first, err := BuildMatchStatus(ctx, c, nil, *urn, []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("first BuildMatchStatus: %v", err)
	}
	if first.Scoreboard == nil {
		t.Fatal("Scoreboard nil on first build")
	}
	v, ok := first.Scoreboard.HomeKills.Get()
	if !ok || v != 10 {
		t.Fatalf("first.Scoreboard.HomeKills = (%d, %v), want (10, true)", v, ok)
	}

	// Tamper with the snapshot — simulating a buggy consumer.
	first.Scoreboard.HomeKills = types.Some[int32](999)
	first.Scoreboard.AwayKills = types.None[int32]()
	first.Scoreboard.HomeWonRounds = types.Some[int](42)
	*first.Scoreboard = types.Scoreboard{} // even wholesale reset

	// Read again. Must reflect the cache's untouched values.
	second, err := BuildMatchStatus(ctx, c, nil, *urn, []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("second BuildMatchStatus: %v", err)
	}
	if second.Scoreboard == nil {
		t.Fatal("Scoreboard nil on second build (cache poisoned)")
	}
	if v, ok := second.Scoreboard.HomeKills.Get(); !ok || v != 10 {
		t.Errorf("second.Scoreboard.HomeKills = (%d, %v), want (10, true) — cache aliased", v, ok)
	}
	if v, ok := second.Scoreboard.AwayKills.Get(); !ok || v != 7 {
		t.Errorf("second.Scoreboard.AwayKills = (%d, %v), want (7, true) — cache aliased", v, ok)
	}
	if v, ok := second.Scoreboard.HomeWonRounds.Get(); !ok || v != 3 {
		t.Errorf("second.Scoreboard.HomeWonRounds = (%d, %v), want (3, true) — cache aliased", v, ok)
	}
}
