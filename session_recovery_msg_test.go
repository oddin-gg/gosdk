package gosdk

import (
	"context"
	"testing"

	"github.com/google/uuid"

	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	"github.com/oddin-gg/gosdk/types"
)

// TestSession_ProcessFeedMessage_ReportsRecoveryRequestIDs: a message that
// carries a recovery request id is the replay of a recovery, so the session
// tells the recovery layer — that is what renews the recovery's snapshot-
// complete deadline. Live messages carry no id and report nothing.
func TestSession_ProcessFeedMessage_ReportsRecoveryRequestIDs(t *testing.T) {
	id := 4242
	attrs := func(requestID *int) feedXML.MessageAttributes {
		return feedXML.MessageAttributes{Product: 3, EventID: "od:match:1", RequestID: requestID}
	}
	cases := []struct {
		name string
		msg  func(requestID *int) types.BasicMessage
	}{
		{"OddsChange", func(r *int) types.BasicMessage {
			return &feedXML.OddsChange{ProductID: 3, EventID: "od:match:1", RequestID: r}
		}},
		{"BetSettlement", func(r *int) types.BasicMessage {
			return &feedXML.BetSettlement{ProductID: 3, EventID: "od:match:1", RequestID: r}
		}},
		{"FixtureChange", func(r *int) types.BasicMessage {
			return &feedXML.FixtureChange{ProductID: 3, EventID: "od:match:1", RequestID: r}
		}},
		{"BetStop", func(r *int) types.BasicMessage { return &feedXML.BetStop{MessageAttributes: attrs(r)} }},
		{"BetCancel", func(r *int) types.BasicMessage { return &feedXML.BetCancel{MessageAttributes: attrs(r)} }},
		{"RollbackBetSettlement", func(r *int) types.BasicMessage {
			return &feedXML.RollbackBetSettlement{MessageAttributes: attrs(r)}
		}},
		{"RollbackBetCancel", func(r *int) types.BasicMessage {
			return &feedXML.RollbackBetCancel{MessageAttributes: attrs(r)}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := func(requestID *int) []recoveryMsgSeen {
				rec := &recordingRecoveryProcessor{}
				o := &oddsFeedSessionImpl{
					cacheManager:             &spyCacheNotifier{},
					feedMessageFactory:       &fakeMessageBuilder{builtMessage: oddsChangeStub{}},
					recoveryMessageProcessor: rec,
					logger:                   discardLogger(),
					msgCh:                    make(chan sessionEnvelope, 1),
					sessionID:                uuid.New(),
				}
				fm := &types.FeedMessage{Message: tc.msg(requestID)}
				o.processFeedMessage(context.Background(), fm, types.AllMessageInterest, nil, false, nil)
				if p := rec.recoveryMsgs.Load(); p != nil {
					return *p
				}
				return nil
			}
			got := run(&id)
			if len(got) != 1 || got[0] != (recoveryMsgSeen{3, id}) {
				t.Fatalf("recovery message reports = %+v, want one {3 %d}", got, id)
			}
			if got := run(nil); len(got) != 0 {
				t.Fatalf("a live message (no request id) reported %+v", got)
			}
		})
	}
}
