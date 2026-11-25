package gosdk

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	"github.com/oddin-gg/gosdk/internal/recovery"
	"github.com/oddin-gg/gosdk/types"
)

// TestSession_ProcessFeedMessage_ProcessedFactCarriesTheRecoveryRequestID: a
// message that carries a recovery request id is the replay of a recovery, so the
// processed fact it leaves says so: that is what puts off the recovery's
// snapshot-complete deadline. Live messages carry no id.
func TestSession_ProcessFeedMessage_ProcessedFactCarriesTheRecoveryRequestID(t *testing.T) {
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
			run := func(requestID *int) []factEvent {
				l := &factLog{}
				o := factSession(l, &stepClock{})
				fm := &types.FeedMessage{Message: tc.msg(requestID)}
				o.processFeedMessage(context.Background(), fm, types.AllMessageInterest, nil, false, nil)
				env := <-o.msgCh
				if env.settled == nil {
					t.Fatal("no processed fact rides the envelope")
				}
				env.settled()
				return l.of("processed")
			}
			got := run(&id)
			if len(got) != 1 || got[0].producer != 3 || got[0].requestID != id {
				t.Fatalf("processed facts = %+v, want one of producer 3 and request %d", got, id)
			}
			if got := run(nil); len(got) != 1 || got[0].requestID != 0 {
				t.Fatalf("a live message (no request id) left %+v, want one with request id 0", got)
			}
		})
	}
}

// processingRecorder counts the processing hooks the session drives and
// remembers the timestamp each end carried.
type processingRecorder struct {
	noopRecoveryProcessor
	started, ended atomic.Int32
	endedTS        atomic.Pointer[time.Time]
}

func (p *processingRecorder) OnMessageProcessingStarted(uuid.UUID, int, recovery.Stamp) {
	p.started.Add(1)
}

func (p *processingRecorder) OnMessageProcessingEnded(_ uuid.UUID, _ int, ts time.Time) {
	p.ended.Add(1)
	p.endedTS.Store(&ts)
}

// TestSession_ProcessFeedMessage_SystemAliveSessionIsNotClientProcessing: the
// alive-only session is the SDK's own. Its alives must not count as the
// client's processing (no processed-cursor advance, no in-processing count,
// no processing-start time), while the alive of a session of the client does
// and is paired start/end with the alive's creation time.
func TestSession_ProcessFeedMessage_SystemAliveSessionIsNotClientProcessing(t *testing.T) {
	created := time.Unix(1_700_000_000, 0)
	run := func(interest types.MessageInterest) *processingRecorder {
		rec := &processingRecorder{}
		o := &oddsFeedSessionImpl{
			cacheManager:             &spyCacheNotifier{},
			feedMessageFactory:       &fakeMessageBuilder{builtMessage: oddsChangeStub{}},
			recoveryMessageProcessor: rec,
			logger:                   discardLogger(),
			msgCh:                    make(chan sessionEnvelope, 1),
			sessionID:                uuid.New(),
		}
		subscribed := 1
		fm := &types.FeedMessage{
			BasicFeedMessage: types.BasicFeedMessage{Timestamp: types.MessageTimestamp{Created: created}},
			Message:          &feedXML.Alive{ProductID: 1, Subscribed: &subscribed},
		}
		o.processFeedMessage(context.Background(), fm, interest, nil, false, nil)
		return rec
	}

	sys := run(types.SystemAliveOnly)
	if sys.started.Load() != 0 || sys.ended.Load() != 0 {
		t.Fatalf("system session alive reported processing: started=%d ended=%d", sys.started.Load(), sys.ended.Load())
	}

	user := run(types.AllMessageInterest)
	if user.started.Load() != 1 || user.ended.Load() != 1 {
		t.Fatalf("user session alive: started=%d ended=%d, want 1/1", user.started.Load(), user.ended.Load())
	}
	if ts := user.endedTS.Load(); ts == nil || !ts.Equal(created) {
		t.Fatalf("user session alive ended with %v, want %v", ts, created)
	}
}
