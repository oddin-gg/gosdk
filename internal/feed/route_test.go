package feed

import (
	"strings"
	"testing"

	"github.com/oddin-gg/gosdk/types"
)

// Routing-key shape (8 dot-separated parts):
//   0: priority (hi/lo/-)
//   1: prematch indicator (pre/-)
//   2: live indicator (live/-)
//   3: message type (alive/odds_change/etc, or -)
//   4: sport id (numeric, or -)
//   5: event type — carries the URN prefix-and-type joined by ':' (e.g. "od:match"), or -
//   6: event id (numeric, or -)
//   7: node id (numeric or -)
//
// SportID URN is built as `sportIDPrefix + parts[4]` and parsed.
// EventID URN is built as `parts[5] + ":" + parts[6]` and parsed (so parts[5]
// must already be in `prefix:type` form — that's how the SDK publishes
// routing-key BINDINGS, see feed.go).

func TestParseRoute(t *testing.T) {
	c := &ChannelConsumer{sportIDPrefix: "od:sport:"}

	tests := []struct {
		name             string
		route            string
		wantSystem       bool
		wantSportPrefix  string
		wantSportID      uint
		wantEventType    string
		wantEventID      uint
		wantErr          bool
		wantErrSubstring string
	}{
		{
			name:            "match route with everything",
			route:           "hi.pre.-.odds_change.1.od:match.198314.1",
			wantSportPrefix: "od:sport:", wantSportID: 1, wantEventType: "match", wantEventID: 198314,
		},
		{
			name:            "live event",
			route:           "hi.-.live.odds_change.2.od:match.500.1",
			wantSportPrefix: "od:sport:", wantSportID: 2, wantEventType: "match", wantEventID: 500,
		},
		{
			name:            "tournament event type",
			route:           "lo.pre.-.fixture_change.1.od:tournament.42.1",
			wantSportPrefix: "od:sport:", wantSportID: 1, wantEventType: "tournament", wantEventID: 42,
		},
		{
			name:       "system alive routing key (all dashes for ids)",
			route:      "-.-.-.alive.-.-.-.-",
			wantSystem: true,
		},
		{
			name:       "system route partial dashes",
			route:      "-.-.-.snapshot_complete.-.-.-.1",
			wantSystem: true,
		},
		{
			name:             "wrong number of parts",
			route:            "hi.pre.-.odds_change.1.match.198314",
			wantErr:          true,
			wantErrSubstring: "incorrect route",
		},
		{
			name:             "too many parts",
			route:            "hi.pre.-.odds_change.1.match.198314.1.extra",
			wantErr:          true,
			wantErrSubstring: "incorrect route",
		},
		{
			name:    "empty",
			route:   "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, err := c.parseRoute(tt.route)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", info)
				}
				if tt.wantErrSubstring != "" && !strings.Contains(err.Error(), tt.wantErrSubstring) {
					t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErrSubstring)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if info == nil {
				t.Fatal("nil info")
			}
			if info.FullRoutingKey != tt.route {
				t.Fatalf("FullRoutingKey = %q, want %q", info.FullRoutingKey, tt.route)
			}
			if tt.wantSystem {
				if !info.IsSystemRoutingKey {
					t.Fatalf("IsSystemRoutingKey = false, want true")
				}
				if info.SportID != nil || info.EventID != nil {
					t.Fatalf("system route should have nil SportID/EventID, got sport=%v event=%v", info.SportID, info.EventID)
				}
				return
			}
			if info.IsSystemRoutingKey {
				t.Fatalf("IsSystemRoutingKey = true, want false")
			}
			if info.SportID == nil {
				t.Fatal("SportID is nil")
			}
			// SportID URN is built from `sportIDPrefix + parts[4]`, where
			// sportIDPrefix is "od:sport:" and parts[4] is the numeric id.
			// ParseURN splits on ":" giving Prefix=od, Type=sport, ID=<num>.
			wantSport := types.URN{Prefix: "od", Type: "sport", ID: tt.wantSportID}
			if *info.SportID != wantSport {
				t.Fatalf("SportID = %+v, want %+v", *info.SportID, wantSport)
			}
			_ = tt.wantSportPrefix // documented above; expectation is fixed
			if info.EventID == nil {
				t.Fatal("EventID is nil")
			}
			if info.EventID.Type != tt.wantEventType {
				t.Fatalf("EventID.Type = %q, want %q", info.EventID.Type, tt.wantEventType)
			}
			if info.EventID.ID != tt.wantEventID {
				t.Fatalf("EventID.ID = %d, want %d", info.EventID.ID, tt.wantEventID)
			}
		})
	}
}

// TestParseRoute_BadURNComponents verifies that malformed numeric IDs in the
// routing key surface as errors rather than silent zero values.
func TestParseRoute_BadURNComponents(t *testing.T) {
	c := &ChannelConsumer{sportIDPrefix: "od:sport:"}

	bad := []string{
		"hi.pre.-.odds_change.NOTNUM.od:match.1.1", // sport id non-numeric
		"hi.pre.-.odds_change.1.od:match.NOTNUM.1", // event id non-numeric
	}
	for _, route := range bad {
		t.Run(route, func(t *testing.T) {
			info, err := c.parseRoute(route)
			if err == nil {
				t.Fatalf("expected error for %q, got %+v", route, info)
			}
		})
	}
}
