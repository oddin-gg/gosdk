package xml

import "testing"

// TestDecode_OddsChangeScoreboardClockRunning pins the three states of the
// optional clock_running scoreboard attribute: true, false, and absent.
// Absent must stay nil: unknown is not the same as not running.
func TestDecode_OddsChangeScoreboardClockRunning(t *testing.T) {
	cases := []struct {
		name  string
		attr  string
		isNil bool
		want  bool
	}{
		{name: "running", attr: ` clock_running="true"`, want: true},
		{name: "stopped", attr: ` clock_running="false"`, want: false},
		{name: "absent", attr: ``, isNil: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := []byte(`<odds_change timestamp="1777832981632" product="2" event_id="od:match:1">` +
				`<sport_event_status status="1" scoreboard_available="true">` +
				`<scoreboard home_goals="1" away_goals="0"` + tc.attr + `/></sport_event_status>` +
				`<odds><market id="1" status="1"><outcome id="1" odds="1.5" active="1"/></market></odds></odds_change>`)
			msg, err := Decode(data)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			oc, ok := msg.(*OddsChange)
			if !ok {
				t.Fatalf("got %T, want *OddsChange", msg)
			}
			if oc.SportEventStatus == nil || oc.SportEventStatus.Scoreboard == nil {
				t.Fatal("scoreboard not decoded")
			}
			got := oc.SportEventStatus.Scoreboard.ClockRunning
			if tc.isNil {
				if got != nil {
					t.Fatalf("ClockRunning = %v, want nil", *got)
				}
				return
			}
			if got == nil || *got != tc.want {
				t.Fatalf("ClockRunning = %v, want %v", got, tc.want)
			}
		})
	}
}
