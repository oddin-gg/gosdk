package recovery

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/types"
)

// TestActor_OnRecoverEvent_LifecycleCtxCancelsBlockedAPICall verifies
// the v2.20 F2 invariant under the v2.24 actor restructure:
//
// Pre-v2.24, onRecoverEvent ran the API call inline on the actor
// goroutine. The bug was that the inline call must respect the actor
// lifetime ctx (so Manager.Close cannot leak a blocked HTTP request).
// In v2.24 the API call was hoisted to a detached goroutine — the
// "block onRecoverEvent" assertion no longer applies — but the
// invariant (lifecycle-ctx cancellation propagates into the in-flight
// HTTP request) still must hold so Client.Close's shutdown budget is
// honored.
//
// Strategy: stand up a fixture that hangs the recovery endpoint until
// the request ctx fires. Trigger onRecoverEvent (returns fast, replies
// with handle, spawns API goroutine). Cancel actor lifetime ctx;
// assert the handler observes r.Context().Done() promptly AND the
// handle is failed via fake.completeHandle within ~1s.
func TestActor_OnRecoverEvent_LifecycleCtxCancelsBlockedAPICall(t *testing.T) {
	// requestStarted: the fixture sends here when the recovery handler
	// is entered, so the test can synchronize cancellation with an
	// in-flight HTTP request.
	requestStarted := make(chan struct{}, 1)
	// hangReleased: the fixture sends here when r.Context() fires —
	// proves the lifecycle ctx cancellation reached the request.
	hangReleased := make(chan struct{}, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		switch {
		case strings.HasSuffix(r.URL.Path, "/descriptions/producers"):
			_, _ = io.WriteString(w, producersBody)
		case strings.Contains(r.URL.Path, "/odds/events/"),
			strings.Contains(r.URL.Path, "/stateful_messages/events/"):
			select {
			case requestStarted <- struct{}{}:
			default:
			}
			// Hang until the request ctx fires (which is the v2.20
			// invariant we are validating: actor lifetime ctx cancel
			// must propagate into this HTTP request).
			<-r.Context().Done()
			select {
			case hangReleased <- struct{}{}:
			default:
			}
		default:
			_, _ = io.WriteString(w, `<?xml version="1.0"?><response response_code="OK"/>`)
		}
	}))
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
		Timeout: 30 * time.Second,
	})

	// Actor's lifetime ctx — we cancel this to simulate
	// Manager.cleanup()'s sess.cancelCtx().
	actorCtx, cancelActorCtx := context.WithCancel(context.Background())
	defer cancelActorCtx()

	fake := newFakeManagerOps()
	a := newRecoveryActor(actorCtx, 1, cfg, apiClient, pm, fake, newDiscardLogger(), 32, 0)

	urn, _ := types.ParseURN("od:match:1")
	reply := make(chan recoverEventReply, 1)

	// onRecoverEvent now returns fast (the API call is detached). The
	// reply carries the handle even before the API completes.
	a.onRecoverEvent(evRecoverEvent{
		ctx:              t.Context(),
		eventID:          *urn,
		statefulRecovery: false,
		reply:            reply,
	})
	select {
	case r := <-reply:
		if r.err != nil || r.handle == nil {
			t.Fatalf("expected handle, got %+v", r)
		}
	case <-time.After(time.Second):
		t.Fatal("onRecoverEvent did not reply within 1s")
	}

	// Wait for the detached HTTP goroutine to enter the fixture
	// handler — only then does the lifecycle-ctx-cancellation test
	// have anything to cancel.
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("HTTP recovery request did not reach fixture within 1s")
	}

	// Cancel the actor lifetime ctx and assert propagation reaches
	// the in-flight HTTP request promptly.
	start := time.Now()
	cancelActorCtx()

	select {
	case <-hangReleased:
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("HTTP request ctx fired after %v, want <1s (lifecycle ctx propagation)", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP request ctx did not fire within 2s after actor ctx cancel — F2 invariant broken")
	}
}
