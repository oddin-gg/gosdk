package recovery

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/producer"
	"github.com/oddin-gg/gosdk/types"
)

// TestManager_OnSnapshotCompleteReceived_ClosedManagerRejectsAck guards
// the shutdown half of snapshot-complete admission: a completion that races
// the close must not be reported as safely admitted (nil), or the session would
// ACK a delivery nobody will ever process. Once the manager is not open it
// surfaces ErrManagerClosed, so the delivery stays unacked and the broker
// redelivers it.
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
	// Open manager, a request this feed never asked for: nothing to complete,
	// safe to ack.
	if err := mgr.OnSnapshotCompleteReceived(t.Context(), uuid.New(), 9999, 1); err != nil {
		t.Fatalf("stray completion on open manager = %v, want nil (safe ack)", err)
	}

	mgr.Close()

	// Closed manager: a completion for ANY producer (known or not) must not
	// be reported as admitted.
	if err := mgr.OnSnapshotCompleteReceived(t.Context(), uuid.New(), 1, 2); !errors.Is(err, ErrManagerClosed) {
		t.Fatalf("completion on closed manager = %v, want ErrManagerClosed", err)
	}
}

// TestManager_DispatchRecoverEvent_CancelledCtxNeverStartsRecovery is
// the regression for the nil-handle/live-recovery contradiction:
// admission could pick a free queue slot even with the caller's ctx already
// cancelled, and the reply wait could pick ctx.Done() over a ready reply: either
// way the caller got (nil, ctx.Err()) while the actor registered the recovery
// and ran the detached POST anyway. A caller retrying on that error initiated a
// DUPLICATE recovery. A cancelled ctx fails admission deterministically (nil
// handle means no request issued), and once admitted the caller always receives
// the handle.
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
