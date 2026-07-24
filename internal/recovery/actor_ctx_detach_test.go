package recovery

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/types"
)

// TestActor_StopBounded pins the bounded actor join used by
// Manager.CloseCtx: a wedged actor (its run loop never exiting) must not
// pin the caller's shutdown budget — stopBounded returns false on the
// deadline — while a live actor joins promptly with true.
func TestActor_StopBounded(t *testing.T) {
	// Wedged: run() never started, so done never closes.
	wedged := newTestActor(t, newFakeManagerOps())
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if wedged.stopBounded(ctx) {
		t.Fatal("stopBounded reported completion on a never-running actor")
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("stopBounded blocked %v despite the ctx bound", el)
	}

	// Live: run() drains shutdown and closes done → prompt true join.
	live := newTestActor(t, newFakeManagerOps())
	go live.run()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if !live.stopBounded(ctx2) {
		t.Fatal("stopBounded reported timeout on a live actor")
	}
}

// TestActor_OnRecoverEvent_DetachesFromCallerCtx pins P1-b: once the
// handle is returned, the detached recovery API call must be bounded by
// the ACTOR lifetime, never by the caller's request ctx. Callers
// routinely `defer cancel()` the moment they hold the handle; if the API
// call still rode that ctx, the deferred cancel would abort a recovery
// the caller believed was running.
//
// The server blocks the recovery POST until the test releases it. We
// cancel the caller ctx while the POST is in-flight and assert the call
// does NOT return early — it stays blocked (bounded by the actor
// lifetime, not the caller) and completes successfully once released.
func TestActor_OnRecoverEvent_DetachesFromCallerCtx(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseIt := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseIt() // never leave the handler goroutine wedged (srv.Close joins it)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		switch {
		case strings.HasSuffix(r.URL.Path, "/descriptions/producers"):
			_, _ = io.WriteString(w, producersBody)
		case strings.Contains(r.URL.Path, "/odds/events/"):
			close(entered)
			<-release
			_, _ = io.WriteString(w, `<?xml version="1.0"?><response response_code="OK"/>`)
		default:
			_, _ = io.WriteString(w, `<?xml version="1.0"?><response response_code="OK"/>`)
		}
	}))
	defer srv.Close()

	fake := newFakeManagerOps()
	a := newWiredActorForProducer(t, srv, fake, 1)

	urn, _ := types.ParseURN("od:match:1")
	reply := make(chan recoverEventReply, 1)
	callerCtx, cancelCaller := context.WithCancel(context.Background())

	a.onRecoverEvent(evRecoverEvent{
		ctx:              callerCtx,
		eventID:          *urn,
		statefulRecovery: false,
		reply:            reply,
	})

	// Handle comes back synchronously, before the API call runs.
	select {
	case r := <-reply:
		if r.err != nil || r.handle == nil {
			t.Fatalf("reply err=%v handle=%v", r.err, r.handle)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("onRecoverEvent did not reply with a handle")
	}

	// Wait until the detached POST is in-flight (blocked in the handler).
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("recovery API never reached the server")
	}

	// The caller does what callers do once they hold the handle: cancels
	// its request ctx. Pre-fix this cancelled apiCtx and aborted the POST.
	cancelCaller()

	// Grace window: if the POST were still tied to the caller ctx, the
	// cancel would make it return NOW — the completion would land on the
	// inbox before we release the server. It must NOT.
	select {
	case ev := <-a.inbox:
		releaseIt()
		ce, _ := ev.(evRecoverEventCompleted)
		t.Fatalf("recovery completed on caller-ctx cancel before the server released it "+
			"(success=%v err=%v): apiCtx still derives from the caller ctx", ce.success, ce.err)
	case <-time.After(300 * time.Millisecond):
		// Good: the POST is unaffected by the caller cancel.
	}

	// Release the server; the POST must now complete successfully.
	releaseIt()
	select {
	case ev := <-a.inbox:
		ce, ok := ev.(evRecoverEventCompleted)
		if !ok {
			t.Fatalf("unexpected inbox event %T", ev)
		}
		if !ce.success || ce.err != nil {
			t.Fatalf("recovery did not complete cleanly: success=%v err=%v", ce.success, ce.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no completion event after releasing the server")
	}
}
