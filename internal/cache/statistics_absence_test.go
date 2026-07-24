package cache

import (
	"testing"

	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
)

// TestMakeFeedStatistics_OneSidedPreservesAbsence is the regression for
// present-zero statistics (Codex P2): a one-sided feed payload such as
// <statistics><yellow_cards home="2"/></statistics> must expose the
// absent AWAY side as None, not Some(0). Pre-fix StatisticsPair.Home/Away
// were plain ints, so the missing away attribute decoded as a present
// zero and surfaced as Some(0) — indistinguishable from a real 0.
func TestMakeFeedStatistics_OneSidedPreservesAbsence(t *testing.T) {
	home := 2
	stats := &feedXML.Statistics{
		YellowCards: &feedXML.StatisticsPair{Home: &home}, // away absent
	}

	got := makeFeedStatistics(stats)

	if v, ok := got.HomeYellowCards.Get(); !ok || v != 2 {
		t.Fatalf("HomeYellowCards = (%v, %v), want (2, true)", v, ok)
	}
	if _, ok := got.AwayYellowCards.Get(); ok {
		t.Fatalf("AwayYellowCards present; want None for an absent away attribute")
	}
	// Absent pair element → both sides None.
	if _, ok := got.HomeCorners.Get(); ok {
		t.Fatalf("HomeCorners present; want None for an absent <corners> element")
	}
	if _, ok := got.AwayCorners.Get(); ok {
		t.Fatalf("AwayCorners present; want None for an absent <corners> element")
	}

	// Fully-populated pair maps both sides through.
	h, a := 3, 4
	full := makeFeedStatistics(&feedXML.Statistics{RedCards: &feedXML.StatisticsPair{Home: &h, Away: &a}})
	if v, ok := full.HomeRedCards.Get(); !ok || v != 3 {
		t.Fatalf("HomeRedCards = (%v, %v), want (3, true)", v, ok)
	}
	if v, ok := full.AwayRedCards.Get(); !ok || v != 4 {
		t.Fatalf("AwayRedCards = (%v, %v), want (4, true)", v, ok)
	}
	// A genuine zero is still present (distinct from absent).
	zero := 0
	z := makeFeedStatistics(&feedXML.Statistics{RedCards: &feedXML.StatisticsPair{Home: &zero}})
	if v, ok := z.HomeRedCards.Get(); !ok || v != 0 {
		t.Fatalf("HomeRedCards = (%v, %v), want (0, true) — a real zero must stay present", v, ok)
	}
}
