package cache

import (
	"encoding/xml"
	"testing"
	"time"

	apiXML "github.com/oddin-gg/gosdk/internal/api/xml"
	"github.com/oddin-gg/gosdk/internal/cache/lru"
	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

// TestScoreboardClockRunning checks the optional clock_running attribute
// from the wire to the public types.Scoreboard on both paths (feed
// odds_change and REST match summary): true, false, and absent. Absent
// must stay unset: unknown is not the same as not running.
func TestScoreboardClockRunning(t *testing.T) {
	cases := []struct {
		name    string
		attr    string
		wantVal bool
		wantSet bool
	}{
		{name: "running", attr: ` clock_running="true"`, wantVal: true, wantSet: true},
		{name: "stopped", attr: ` clock_running="false"`, wantVal: false, wantSet: true},
		{name: "absent", attr: ``},
	}
	for _, tc := range cases {
		doc := `<scoreboard home_goals="1"` + tc.attr + `/>`
		t.Run("feed/"+tc.name, func(t *testing.T) {
			var s feedXML.Scoreboard
			if err := xml.Unmarshal([]byte(doc), &s); err != nil {
				t.Fatal(err)
			}
			got := makeFeedScoreboard(&s).ClockRunning
			if v, ok := got.Get(); v != tc.wantVal || ok != tc.wantSet {
				t.Fatalf("ClockRunning = (%v, %v), want (%v, %v)", v, ok, tc.wantVal, tc.wantSet)
			}
		})
		t.Run("rest/"+tc.name, func(t *testing.T) {
			body := `<sport_event_status status="live" scoreboard_available="true">` + doc + `</sport_event_status>`
			var st apiXML.SportEventStatus
			if err := xml.Unmarshal([]byte(body), &st); err != nil {
				t.Fatal(err)
			}
			if st.Scoreboard == nil {
				t.Fatal("scoreboard not decoded")
			}
			got := makeAPIScoreboard(st.Scoreboard).ClockRunning
			if v, ok := got.Get(); v != tc.wantVal || ok != tc.wantSet {
				t.Fatalf("ClockRunning = (%v, %v), want (%v, %v)", v, ok, tc.wantVal, tc.wantSet)
			}
		})
	}
}

func newClockTestCache() *MatchStatusCache {
	return &MatchStatusCache{
		logger: log.New(nil),
		entries: lru.NewTTL[types.URN, *LocalizedMatchStatus](
			lru.DefaultEventCacheSize, nil, lru.DefaultEventCacheTTL),
	}
}

func clockOf(t *testing.T, c *MatchStatusCache, id types.URN) (types.Scoreboard, bool) {
	t.Helper()
	e, _ := c.entries.Get(id)
	if e == nil || e.scoreboard == nil {
		return types.Scoreboard{}, false
	}
	return *e.scoreboard, true
}

func wantClock(t *testing.T, sb types.Scoreboard, val, set bool) {
	t.Helper()
	if v, ok := sb.ClockRunning.Get(); v != val || ok != set {
		t.Fatalf("ClockRunning = (%v, %v), want (%v, %v)", v, ok, val, set)
	}
}

// TestClockRunningWithoutScoreboard: a newer status without a scoreboard
// keeps the other scoreboard fields but leaves the clock flag unknown;
// a status with a scoreboard replaces the flag as before. Feed and REST.
func TestClockRunningWithoutScoreboard(t *testing.T) {
	id := types.URN{Prefix: "od", Type: "match", ID: 7}
	live := func(attr string) feedXML.SportEventStatus {
		var st feedXML.SportEventStatus
		body := `<sport_event_status status="1" match_status="1"><scoreboard home_goals="3" time="45"` + attr + `/></sport_event_status>`
		if err := xml.Unmarshal([]byte(body), &st); err != nil {
			t.Fatal(err)
		}
		return st
	}
	liveAPI := func(attr string) apiXML.SportEventStatus {
		var st apiXML.SportEventStatus
		body := `<sport_event_status status="live" scoreboard_available="true"><scoreboard home_goals="3" time="45"` + attr + `/></sport_event_status>`
		if err := xml.Unmarshal([]byte(body), &st); err != nil {
			t.Fatal(err)
		}
		return st
	}

	t.Run("feed", func(t *testing.T) {
		c := newClockTestCache()
		first := live(` clock_running="true"`)
		c.refreshOrInsertFeedItem(id, &first, time.Time{})
		before, _ := clockOf(t, c, id)
		wantClock(t, before, true, true)

		c.refreshOrInsertFeedItem(id, &feedXML.SportEventStatus{Status: ptrOf(3), MatchStatus: ptrOf(2)}, time.Time{})
		sb, ok := clockOf(t, c, id)
		if !ok {
			t.Fatal("previous scoreboard dropped")
		}
		wantClock(t, sb, false, false)
		if v, _ := sb.HomeGoals.Get(); v != 3 {
			t.Fatalf("HomeGoals = %d, want 3", v)
		}
		if v, _ := sb.Time.Get(); v != 45 {
			t.Fatalf("Time = %d, want 45", v)
		}
		// the earlier value handed out is not mutated
		wantClock(t, before, true, true)

		stopped := live(` clock_running="false"`)
		c.refreshOrInsertFeedItem(id, &stopped, time.Time{})
		sb, _ = clockOf(t, c, id)
		wantClock(t, sb, false, true)
	})

	t.Run("rest", func(t *testing.T) {
		c := newClockTestCache()
		first := liveAPI(` clock_running="true"`)
		if err := c.refreshOrInsertAPIItem(id, first, time.Now()); err != nil {
			t.Fatal(err)
		}
		before, _ := clockOf(t, c, id)
		wantClock(t, before, true, true)

		time.Sleep(2 * time.Millisecond)
		if err := c.refreshOrInsertAPIItem(id, apiXML.SportEventStatus{Status: "closed"}, time.Now()); err != nil {
			t.Fatal(err)
		}
		sb, ok := clockOf(t, c, id)
		if !ok {
			t.Fatal("previous scoreboard dropped")
		}
		wantClock(t, sb, false, false)
		if v, _ := sb.HomeGoals.Get(); v != 3 {
			t.Fatalf("HomeGoals = %d, want 3", v)
		}
		if v, _ := sb.Time.Get(); v != 45 {
			t.Fatalf("Time = %d, want 45", v)
		}
		wantClock(t, before, true, true)

		time.Sleep(2 * time.Millisecond)
		stopped := liveAPI(` clock_running="false"`)
		if err := c.refreshOrInsertAPIItem(id, stopped, time.Now()); err != nil {
			t.Fatal(err)
		}
		sb, _ = clockOf(t, c, id)
		wantClock(t, sb, false, true)
	})
}
