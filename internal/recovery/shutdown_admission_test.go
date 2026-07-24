package recovery

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/producer"
	"github.com/oddin-gg/gosdk/types"
)

// newTestHTTPClient routes the api.Client at the fixture server.
func newTestHTTPClient(srv *httptest.Server) *http.Client {
	return &http.Client{
		Transport: &rewriteTransport{
			target: srv.URL,
			base:   &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		},
		Timeout: 2 * time.Second,
	}
}

// TestActor_Run_DrainsInboxOnShutdown guards the admitted-then-dropped
// race: sendCtx's admission and close(shutdown) can both be ready in
// run()'s select, so an event admitted (and its AMQP delivery acked on
// the strength of that admission) could sit in the buffered inbox while
// run() exits via the shutdown arm. run() must drain the inbox before
// exiting so every admitted event is dispatched. Pre-fix this test
// fails intermittently (the select picks the shutdown arm ~half the
// runs); post-fix it is deterministic.
func TestActor_Run_DrainsInboxOnShutdown(t *testing.T) {
	srv, _ := fixtureSrv(t)
	defer srv.Close()

	for i := 0; i < 10; i++ {
		fake := newFakeManagerOps()
		a := newWiredActor(t, srv, fake)

		now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
		// Admit BEFORE run() starts, then signal shutdown, so the loop's
		// first select sees both arms ready.
		if !a.send(evMsgProcessingStarted{timestamp: now}) {
			t.Fatal("inbox admission failed")
		}
		close(a.shutdown)
		go a.run()
		select {
		case <-a.done:
		case <-time.After(2 * time.Second):
			t.Fatal("actor did not exit after shutdown")
		}

		prod, err := a.pm.GetProducer(t.Context(), 1)
		if err != nil {
			t.Fatalf("GetProducer: %v", err)
		}
		if !prod.LastMessageTimestamp().Equal(now) {
			t.Fatalf("iteration %d: admitted event was dropped on shutdown exit (LastMessageTimestamp=%v)", i, prod.LastMessageTimestamp())
		}
	}
}

// TestActor_SendCtx_StoppingActorRejectsAdmission verifies sendCtx
// fast-fails with ErrManagerClosed once shutdown has begun, instead of
// racing an admission into an actor that may never dispatch it. Pre-fix
// the select picked uniformly between the free inbox slot and the
// closed shutdown channel, so this failed ~half the runs.
func TestActor_SendCtx_StoppingActorRejectsAdmission(t *testing.T) {
	srv, _ := fixtureSrv(t)
	defer srv.Close()

	for i := 0; i < 10; i++ {
		fake := newFakeManagerOps()
		a := newWiredActor(t, srv, fake)
		close(a.shutdown)

		err := a.sendCtx(t.Context(), evSnapshotComplete{requestID: 1, messageInterest: types.AllMessageInterest})
		if !errors.Is(err, ErrManagerClosed) {
			t.Fatalf("iteration %d: sendCtx after shutdown = %v, want ErrManagerClosed", i, err)
		}
	}
}

// TestManager_OnSnapshotCompleteReceived_ClosedManagerRejectsAck guards
// the shutdown half of snapshot-complete admission: cleanup resets the
// actor map while sessions are still consuming, so a racing completion
// used to miss the lookup and be reported as safely-admitted (nil) —
// the session then ACKED a delivery nobody would ever process. A missing
// actor on a non-open manager must surface ErrManagerClosed so the
// delivery stays unacked and the broker redelivers it.
func TestManager_OnSnapshotCompleteReceived_ClosedManagerRejectsAck(t *testing.T) {
	srv, _ := fixtureSrv(t)
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	cfg := &minimalCfg{apiURL: u.Host, token: "tok"}
	apiClient := api.New(cfg)
	apiClient.SetHTTPClient(newTestHTTPClient(srv))
	pm := producer.NewManager(cfg, apiClient, newDiscardLogger())
	mgr := NewManager(cfg, pm, apiClient, newDiscardLogger(), 0)

	if _, err := mgr.Open(t.Context()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Open manager, unknown producer: nothing to complete, safe to ack.
	if err := mgr.OnSnapshotCompleteReceived(t.Context(), 9999, 1, types.AllMessageInterest); err != nil {
		t.Fatalf("unknown producer on open manager = %v, want nil (safe ack)", err)
	}

	mgr.Close()

	// Closed manager: the actor map is reset — a completion for ANY
	// producer (known or not) must not be reported as admitted.
	if err := mgr.OnSnapshotCompleteReceived(t.Context(), 1, 2, types.AllMessageInterest); !errors.Is(err, ErrManagerClosed) {
		t.Fatalf("completion on closed manager = %v, want ErrManagerClosed", err)
	}
}

// TestManager_DispatchRecoverEvent_CancelledCtxNeverStartsRecovery is
// the regression for the nil-handle/live-recovery contradiction:
// sendCtx's admission select could pick the free inbox slot even with
// the caller's ctx already cancelled, and dispatchRecoverEvent's reply
// wait could pick ctx.Done() over a ready reply — either way the caller
// got (nil, ctx.Err()) while the actor registered the recovery and ran
// the detached POST anyway. A caller retrying on that error initiated a
// DUPLICATE recovery. Post-fix, a cancelled ctx fails admission
// deterministically (nil handle ⇒ no request issued), and once admitted
// the caller always receives the handle.
func TestManager_DispatchRecoverEvent_CancelledCtxNeverStartsRecovery(t *testing.T) {
	srv, hits := fixtureSrv(t)
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	cfg := &minimalCfg{apiURL: u.Host, token: "tok"}
	apiClient := api.New(cfg)
	apiClient.SetHTTPClient(newTestHTTPClient(srv))
	pm := producer.NewManager(cfg, apiClient, newDiscardLogger())
	mgr := NewManager(cfg, pm, apiClient, newDiscardLogger(), 0)
	if _, err := mgr.Open(t.Context()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer mgr.Close()

	eventID := types.URN{Prefix: "od", Type: "match", ID: 42}
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		h, err := mgr.InitiateEventOddsRecoveryHandle(ctx, 1, eventID)
		if err == nil {
			t.Fatalf("iteration %d: expected error on cancelled ctx, got handle=%v", i, h)
		}
		if h != nil {
			t.Fatalf("iteration %d: non-nil handle alongside error %v", i, err)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("iteration %d: err = %v, want context.Canceled", i, err)
		}
	}
	// The contract behind the errors above: NO recovery request may have
	// been issued. Give any stray detached POST a moment to land.
	time.Sleep(100 * time.Millisecond)
	if n := hits.eventRecover.Load(); n != 0 {
		t.Fatalf("%d event-recovery POST(s) executed despite every call returning nil handle + error", n)
	}
}
