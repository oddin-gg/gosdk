package cache

import (
	"encoding/xml"
	"testing"

	apiXML "github.com/oddin-gg/gosdk/internal/api/xml"
	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
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
