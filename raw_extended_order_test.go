package gosdk

import (
	"testing"

	"github.com/google/uuid"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/feed"
	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/internal/producer"
	"github.com/oddin-gg/gosdk/types"
)

// TestSession_ExtendedData_RawPrecedesParsedAndCarriesNoAck pins the
// two-part contract for the extended-data raw side-channel:
//
//  1. Shared-state safety: the raw envelope is published only AFTER
//     BuildMessage — the SDK's last read of the shared decoded message /
//     byte slice / routing key — so a consumer holding the raw can't
//     mutate them mid-parse.
//  2. Ack ordering (P2-a): the raw envelope is emitted FIRST with a NIL
//     ack and the parsed envelope LAST carrying the ack. On the FIFO
//     msgCh the raw is therefore admitted to the public buffer before the
//     ack fires — so the delivery is never acked-then-lost (parsed
//     admitted, raw dropped on shutdown).
func TestSession_ExtendedData_RawPrecedesParsedAndCarriesNoAck(t *testing.T) {
	cfg := &fakeOFC{}
	srv := catalogFixtureServer(t)
	defer srv.Close()
	apiClient := api.New(cfg)
	apiClient.SetHTTPClient(newTestHTTPClient(srv))
	pm := producer.NewManager(cfg, apiClient, log.New(nil))
	if err := pm.Open(t.Context()); err != nil {
		t.Fatalf("producer manager open: %v", err)
	}

	o := &oddsFeedSessionImpl{
		producerManager:          pm,
		cacheManager:             &spyCacheNotifier{},
		feedMessageFactory:       &fakeMessageBuilder{builtMessage: stubMessage{}},
		recoveryMessageProcessor: &recordingRecoveryProcessor{},
		logger:                   discardLogger(),
		msgCh:                    make(chan sessionEnvelope, 4),
		sessionID:                uuid.New(),
	}

	basic := types.BasicFeedMessage{
		RawMessage: []byte("<odds_change/>"),
		RoutingKey: &types.RoutingKeyInfo{FullRoutingKey: "hi.pre.-.odds_change.1.123.-.-.-"},
	}
	// A non-Alive / non-SnapshotComplete wire message so processFeedMessage
	// takes the BuildMessage path (Product 1 = the fixture's producer).
	wire := &feedXML.BetStop{MessageAttributes: feedXML.MessageAttributes{Product: 1}}
	qm := &types.QueueMessage{
		FeedMessage:    &types.FeedMessage{BasicFeedMessage: basic, Message: wire},
		RawFeedMessage: &types.RawFeedMessage{BasicFeedMessage: basic, Message: wire},
	}

	mi := types.AllMessageInterest
	var acked int
	env := feed.QueueEnvelope{Msg: qm, Ack: func() { acked++ }}
	o.processMessage(t.Context(), env, &mi, true /*reportExtendedData*/)

	// Two envelopes: the raw one FIRST (no ack), the parsed one SECOND
	// (carrying the ack). BuildMessage has already completed by the time
	// either is published, so the shared-state window is closed.
	first := recvEnvelope(t, o.msgCh)
	second := recvEnvelope(t, o.msgCh)

	if first.msg.RawFeedMessage == nil {
		t.Fatalf("first envelope is not the raw side-channel: %+v", first.msg)
	}
	if first.ack != nil {
		t.Error("raw side-channel envelope must NOT carry the ack (parsed carries it)")
	}
	if second.msg.RawFeedMessage != nil {
		t.Fatalf("second envelope should be the parsed event, got raw again: %+v", second.msg)
	}
	if second.msg.OddsChange == nil {
		t.Fatalf("second envelope is not the parsed event: %+v", second.msg)
	}
	if second.ack == nil {
		t.Error("parsed envelope (published last) must carry the ack")
	}
	// The ack has not fired yet — it rides the parsed envelope and fires
	// only at public-buffer admission, which is downstream of msgCh.
	if acked != 0 {
		t.Errorf("ack fired during processing (%d); must fire only at admission", acked)
	}
}

// TestSession_ExtendedData_DropCarriesAckOnRaw pins the drop-path half of
// P2-a: when a delivery yields NO parsed envelope (here an Alive, consumed
// by the recovery machinery) but extended-data reporting is on, the raw
// envelope is the delivery's ONLY output and must carry the ack itself.
// Pre-fix the ack rode a bare runAck that fired before the deferred raw
// was admitted — acked-then-lost on shutdown.
func TestSession_ExtendedData_DropCarriesAckOnRaw(t *testing.T) {
	cfg := &fakeOFC{}
	srv := catalogFixtureServer(t)
	defer srv.Close()
	apiClient := api.New(cfg)
	apiClient.SetHTTPClient(newTestHTTPClient(srv))
	pm := producer.NewManager(cfg, apiClient, log.New(nil))
	if err := pm.Open(t.Context()); err != nil {
		t.Fatalf("producer manager open: %v", err)
	}

	o := &oddsFeedSessionImpl{
		producerManager:          pm,
		cacheManager:             &spyCacheNotifier{},
		feedMessageFactory:       &fakeMessageBuilder{builtMessage: stubMessage{}},
		recoveryMessageProcessor: &recordingRecoveryProcessor{},
		logger:                   discardLogger(),
		msgCh:                    make(chan sessionEnvelope, 4),
		sessionID:                uuid.New(),
	}

	basic := types.BasicFeedMessage{
		RawMessage: []byte("<alive/>"),
		RoutingKey: &types.RoutingKeyInfo{FullRoutingKey: "-.-.-.alive.1.-.-.-"},
	}
	subscribed := 1
	wire := &feedXML.Alive{ProductID: 1, Subscribed: &subscribed}
	qm := &types.QueueMessage{
		FeedMessage:    &types.FeedMessage{BasicFeedMessage: basic, Message: wire},
		RawFeedMessage: &types.RawFeedMessage{BasicFeedMessage: basic, Message: wire},
	}

	mi := types.AllMessageInterest
	var acked int
	env := feed.QueueEnvelope{Msg: qm, Ack: func() { acked++ }}
	o.processMessage(t.Context(), env, &mi, true /*reportExtendedData*/)

	// Exactly one envelope — the raw side-channel — and it carries the ack.
	only := recvEnvelope(t, o.msgCh)
	select {
	case extra := <-o.msgCh:
		t.Fatalf("expected only the raw envelope on a drop, got a second: %+v", extra.msg)
	default:
	}
	if only.msg.RawFeedMessage == nil {
		t.Fatalf("drop envelope is not the raw side-channel: %+v", only.msg)
	}
	if only.ack == nil {
		t.Fatal("raw envelope on a drop must carry the ack (else acked-then-lost)")
	}
	if acked != 0 {
		t.Errorf("ack fired during processing (%d); must fire only at admission", acked)
	}
}

func recvEnvelope(t *testing.T, ch <-chan sessionEnvelope) sessionEnvelope {
	t.Helper()
	select {
	case e := <-ch:
		return e
	default:
		t.Fatal("expected an envelope on msgCh, got none")
		return sessionEnvelope{}
	}
}
