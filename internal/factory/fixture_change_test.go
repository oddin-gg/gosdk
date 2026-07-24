package factory

import (
	"testing"

	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	"github.com/oddin-gg/gosdk/types"
)

// TestFixtureChangeImpl_ChangeType exercises every wire-value mapping
// to the public FixtureChangeType. Wire 4 (FORMAT in javasdk /
// netcoresdk) was missing pre-fix and decoded as Unknown.
func TestFixtureChangeImpl_ChangeType(t *testing.T) {
	cases := []struct {
		name string
		wire feedXML.FixtureChangeType
		want types.FixtureChangeType
	}{
		{"new", feedXML.FixtureChangeTypeNew, types.NewFixtureChangeType},
		{"datetime", feedXML.FixtureChangeTypeDateTime, types.TimeUpdateChangeType},
		{"cancelled", feedXML.FixtureChangeTypeCancelled, types.CancelledFixtureChangeType},
		{"format", feedXML.FixtureChangeTypeFormat, types.OtherChangeFixtureChangeType},
		{"coverage", feedXML.FixtureChangeTypeCoverage, types.CoverageFixtureChangeType},
		{"streamurl", feedXML.FixtureChangeTypeStreamURL, types.StreamURLFixtureChangeType},
		{"unknown_wire", feedXML.FixtureChangeType(999), types.UnknownFixtureChangeType},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := fixtureChangeImpl{
				message: &feedXML.FixtureChange{ChangeType: tc.wire},
			}
			if got := f.ChangeType(); got != tc.want {
				t.Errorf("ChangeType wire=%d: got %d, want %d", tc.wire, got, tc.want)
			}
		})
	}
}
