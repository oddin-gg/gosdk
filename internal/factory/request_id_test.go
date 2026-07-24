package factory

import (
	"testing"

	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	"github.com/oddin-gg/gosdk/types"
)

// TestImpls_RequestID_AcrossAllMessageTypes is the regression for the
// v2.30 Optional[T] migration of RequestMessage.RequestID(): every
// public message-type impl must surface the upstream feedXML
// *int as Optional[int] both when present and absent.
//
// The seven RequestMessage implementers are oddsChangeImpl,
// betStopImpl, betSettlementImpl, betCancelImpl, fixtureChangeImpl,
// rollbackBetSettlementImpl, and rollbackBetCancelImpl.
func TestImpls_RequestID_AcrossAllMessageTypes(t *testing.T) {
	id := int(42)

	cases := []struct {
		name string
		// withReq builds an impl with a present RequestID.
		withReq types.RequestMessage
		// withoutReq builds an impl with no RequestID.
		withoutReq types.RequestMessage
	}{
		{
			name: "OddsChange",
			withReq: oddsChangeImpl{
				message: &feedXML.OddsChange{RequestID: &id},
			},
			withoutReq: oddsChangeImpl{
				message: &feedXML.OddsChange{},
			},
		},
		{
			name: "BetStop",
			// betStopImpl stores the Optional directly (the only
			// impl that doesn't delegate to the embedded XML msg).
			withReq:    betStopImpl{requestID: types.Some(id)},
			withoutReq: betStopImpl{requestID: types.None[int]()},
		},
		{
			name: "BetSettlement",
			withReq: betSettlementImpl{
				message: &feedXML.BetSettlement{RequestID: &id},
			},
			withoutReq: betSettlementImpl{
				message: &feedXML.BetSettlement{},
			},
		},
		{
			name: "BetCancel",
			withReq: betCancelImpl{
				message: &feedXML.BetCancel{
					MessageAttributes: feedXML.MessageAttributes{RequestID: &id},
				},
			},
			withoutReq: betCancelImpl{
				message: &feedXML.BetCancel{},
			},
		},
		{
			name: "FixtureChange",
			withReq: fixtureChangeImpl{
				message: &feedXML.FixtureChange{RequestID: &id},
			},
			withoutReq: fixtureChangeImpl{
				message: &feedXML.FixtureChange{},
			},
		},
		{
			name: "RollbackBetSettlement",
			withReq: rollbackBetSettlementImpl{
				message: &feedXML.RollbackBetSettlement{
					MessageAttributes: feedXML.MessageAttributes{RequestID: &id},
				},
			},
			withoutReq: rollbackBetSettlementImpl{
				message: &feedXML.RollbackBetSettlement{},
			},
		},
		{
			name: "RollbackBetCancel",
			withReq: rollbackBetCancelImpl{
				message: &feedXML.RollbackBetCancel{
					MessageAttributes: feedXML.MessageAttributes{RequestID: &id},
				},
			},
			withoutReq: rollbackBetCancelImpl{
				message: &feedXML.RollbackBetCancel{},
			},
		},
	}

	if len(cases) != 7 {
		t.Fatalf("expected 7 RequestMessage implementers, got %d", len(cases))
	}

	for _, c := range cases {
		t.Run(c.name+"/present", func(t *testing.T) {
			got := c.withReq.RequestID()
			v, ok := got.Get()
			if !ok {
				t.Fatalf("%s.RequestID() = None, want Some(%d)", c.name, id)
			}
			if v != id {
				t.Errorf("%s.RequestID() = Some(%d), want Some(%d)", c.name, v, id)
			}
		})
		t.Run(c.name+"/absent", func(t *testing.T) {
			got := c.withoutReq.RequestID()
			if got.IsSet() {
				t.Errorf("%s.RequestID() = %v with no upstream RequestID, want None", c.name, got)
			}
		})
	}
}
