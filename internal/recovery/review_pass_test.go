package recovery

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/producer"
	"github.com/oddin-gg/gosdk/types"
)

// afterCapturingSrv serves the producers catalog and captures the
// `after` query parameter of each snapshot-recovery initiation.
func afterCapturingSrv(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var afters []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		switch {
		case strings.HasSuffix(r.URL.Path, "/descriptions/producers"):
			_, _ = io.WriteString(w, producersBody)
		case strings.Contains(r.URL.Path, "/recovery/initiate_request"):
			mu.Lock()
			afters = append(afters, r.URL.Query().Get("after"))
			mu.Unlock()
			_, _ = io.WriteString(w, `<?xml version="1.0"?><response response_code="OK"/>`)
		default:
			_, _ = io.WriteString(w, `<?xml version="1.0"?><response response_code="OK"/>`)
		}
	}))
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), afters...)
	}
}

func newActorWithSnapshotWindow(t *testing.T, srv *httptest.Server, window time.Duration) *recoveryActor {
	t.Helper()
	pm := newProducerManagerFor(t, srv)
	u, _ := url.Parse(srv.URL)
	cfg := &minimalCfg{apiURL: u.Host, token: "tok"}
	apiClient := api.New(cfg)
	apiClient.SetHTTPClient(&http.Client{
		Transport: &rewriteTransport{
			target: srv.URL,
			base:   &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		},
		Timeout: 2 * time.Second,
	})
	return newRecoveryActor(t.Context(), 1, cfg, apiClient, pm, newFakeManagerOps(), newDiscardLogger(), 32, window)
}

func waitForAfters(t *testing.T, get func() []string) []string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if a := get(); len(a) > 0 {
			return a
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("snapshot recovery request never reached the server")
	return nil
}

// TestMakeSnapshotRecovery_InitialSnapshotWindow pins the
// WithInitialSnapshotTime wiring: a producer with NO recovery cursor
// (zero timestamp) requests its first snapshot from now-window instead
// of full history. The option was previously stored on Config but never
// read anywhere.
func TestMakeSnapshotRecovery_InitialSnapshotWindow(t *testing.T) {
	srv, afters := afterCapturingSrv(t)
	defer srv.Close()

	const window = 30 * time.Minute
	a := newActorWithSnapshotWindow(t, srv, window)

	before := time.Now()
	if err := a.makeSnapshotRecovery(time.Time{}); err != nil {
		t.Fatalf("makeSnapshotRecovery: %v", err)
	}
	got := waitForAfters(t, afters)

	if got[0] == "" {
		t.Fatal("after param absent — initial snapshot window was not applied")
	}
	ms, err := strconv.ParseInt(got[0], 10, 64)
	if err != nil {
		t.Fatalf("after param %q not numeric: %v", got[0], err)
	}
	want := before.Add(-window)
	if diff := time.UnixMilli(ms).Sub(want); diff < -5*time.Second || diff > 5*time.Second {
		t.Fatalf("after = %s, want ≈ %s (now-30m)", time.UnixMilli(ms), want)
	}
}

// TestMakeSnapshotRecovery_NoWindowRequestsFullHistory pins the default:
// zero initialSnapshotTime keeps the pre-wiring behaviour — no `after`
// parameter, i.e. full-history snapshot.
func TestMakeSnapshotRecovery_NoWindowRequestsFullHistory(t *testing.T) {
	srv, afters := afterCapturingSrv(t)
	defer srv.Close()

	a := newActorWithSnapshotWindow(t, srv, 0)
	if err := a.makeSnapshotRecovery(time.Time{}); err != nil {
		t.Fatalf("makeSnapshotRecovery: %v", err)
	}
	if got := waitForAfters(t, afters); got[0] != "" {
		t.Fatalf("after param = %q, want absent for zero window", got[0])
	}
}

// TestDispatchRecoverEvent_StoppedActor_NoHang is the regression for
// the dispatch-vs-shutdown race: the reply select had no <-a.done case,
// so a RecoverEvent* call with a non-cancellable ctx whose event was
// queued but never dispatched (actor exited on shutdown) blocked
// FOREVER — failPendingHandles couldn't help because no handle had been
// registered yet.
func TestDispatchRecoverEvent_StoppedActor_NoHang(t *testing.T) {
	srv, _ := afterCapturingSrv(t)
	defer srv.Close()

	pm := newProducerManagerFor(t, srv)
	u, _ := url.Parse(srv.URL)
	cfg := &minimalCfg{apiURL: u.Host, token: "tok"}
	apiClient := api.New(cfg)
	apiClient.SetHTTPClient(&http.Client{
		Transport: &rewriteTransport{
			target: srv.URL,
			base:   &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		},
		Timeout: 2 * time.Second,
	})
	m := NewManager(cfg, pm, apiClient, newDiscardLogger(), 0)
	if _, err := m.Open(t.Context()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer m.Close()

	// Stop producer 1's actor so its inbox is never drained again —
	// the queued evRecoverEvent can never produce a reply.
	a := m.findOrSpawn(1)
	if a == nil {
		t.Fatal("no actor for producer 1")
	}
	a.stop()
	select {
	case <-a.done:
	case <-time.After(2 * time.Second):
		t.Fatal("actor did not stop")
	}

	type result struct {
		h   *Handle
		err error
	}
	resCh := make(chan result, 1)
	go func() {
		h, err := m.InitiateEventOddsRecoveryHandle(context.Background(), 1, types.URN{Prefix: "od", Type: "match", ID: 1})
		resCh <- result{h, err}
	}()

	select {
	case r := <-resCh:
		if r.err == nil {
			// The actor may have replied just before exiting — a handle
			// is an acceptable outcome; hanging is not.
			return
		}
		if !errors.Is(r.err, ErrManagerClosed) {
			t.Fatalf("err = %v, want ErrManagerClosed", r.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("InitiateEventOddsRecoveryHandle hung on a stopped actor (missing <-a.done case)")
	}
}

// TestDispatchRecoverEvent_InvalidProducer_NoActorLeak is the regression
// for the invalid-id actor leak (Codex P2): findOrSpawn allocated a
// 256-slot inbox, registered the actor, and started its goroutine BEFORE
// any producer validation — the recover command then failed with
// ErrProducerNotFound inside the actor, but the dead-weight actor lived
// until Client shutdown, so repeated RecoverEventOdds calls with
// distinct invalid ids grew the actor map, goroutines, and tick work
// without bound. Post-fix, dispatch validates catalog membership first:
// the caller still gets ErrProducerNotFound and NO actor is created.
func TestDispatchRecoverEvent_InvalidProducer_NoActorLeak(t *testing.T) {
	srv, _ := afterCapturingSrv(t)
	defer srv.Close()

	pm := newProducerManagerFor(t, srv)
	u, _ := url.Parse(srv.URL)
	cfg := &minimalCfg{apiURL: u.Host, token: "tok"}
	apiClient := api.New(cfg)
	apiClient.SetHTTPClient(&http.Client{
		Transport: &rewriteTransport{
			target: srv.URL,
			base:   &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		},
		Timeout: 2 * time.Second,
	})
	m := NewManager(cfg, pm, apiClient, newDiscardLogger(), 0)
	if _, err := m.Open(t.Context()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer m.Close()

	m.actorsMu.RLock()
	before := len(m.actors)
	m.actorsMu.RUnlock()

	for id := 9000; id < 9010; id++ {
		h, err := m.InitiateEventOddsRecoveryHandle(t.Context(), id, types.URN{Prefix: "od", Type: "match", ID: 1})
		if err == nil {
			t.Fatalf("id %d: expected error for invalid producer, got handle %+v", id, h)
		}
		if !errors.Is(err, producer.ErrProducerNotFound) {
			t.Fatalf("id %d: err = %v, want ErrProducerNotFound", id, err)
		}
	}

	m.actorsMu.RLock()
	after := len(m.actors)
	m.actorsMu.RUnlock()
	if after != before {
		t.Fatalf("actor map grew %d -> %d on invalid-id recoveries (leaked actors)", before, after)
	}
}
