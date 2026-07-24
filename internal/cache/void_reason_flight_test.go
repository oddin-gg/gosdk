package cache

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestVoidReasons_ClearDuringFlight_NextCallRefetches is the regression
// for the post-clear singleflight interleaving (Codex P2): the cache
// used a CONSTANT singleflight key, so a caller arriving strictly AFTER
// ClearMarketVoidReasons returned could JOIN a still-running pre-clear
// flight and be served its (stale) result directly — the tombstone only
// blocks the store, not the shared return value. The generation-prefixed
// key must make the post-clear caller start its OWN fetch.
func TestVoidReasons_ClearDuringFlight_NextCallRefetches(t *testing.T) {
	var serves atomic.Int32
	release := make(chan struct{})
	firstInFlight := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := serves.Add(1)
		w.Header().Set("Content-Type", "application/xml")
		if n == 1 {
			// First (pre-clear) fetch: signal, then block so a Clear can
			// land while it is still in flight.
			close(firstInFlight)
			<-release
			_, _ = w.Write([]byte(`<?xml version="1.0"?>
<void_reasons response_code="OK"><void_reason id="1" name="stale"/></void_reasons>`))
			return
		}
		// Second (post-clear) fetch: the authoritative fresh list.
		_, _ = w.Write([]byte(`<?xml version="1.0"?>
<void_reasons response_code="OK"><void_reason id="2" name="fresh"/></void_reasons>`))
	}))
	defer srv.Close()

	vc := newMarketVoidReasonsCache(t.Context(), newAPIClientForTest(t, srv))

	// Caller A starts the cold fetch and blocks inside it.
	aResult := make(chan []string, 1)
	go func() {
		v, err := vc.MarketVoidReasons(t.Context())
		if err != nil {
			aResult <- []string{"err:" + err.Error()}
			return
		}
		names := make([]string, 0, len(v))
		for _, r := range v {
			names = append(names, r.Name)
		}
		aResult <- names
	}()
	<-firstInFlight

	// Clear lands while A's flight is in flight.
	vc.Clear()

	// Caller B arrives strictly AFTER the clear. It must NOT join A's
	// pre-clear flight; it starts its own fetch of the fresh list.
	bDone := make(chan []string, 1)
	go func() {
		v, err := vc.MarketVoidReasons(t.Context())
		if err != nil {
			bDone <- []string{"err:" + err.Error()}
			return
		}
		names := make([]string, 0, len(v))
		for _, r := range v {
			names = append(names, r.Name)
		}
		bDone <- names
	}()

	// B must complete against the second server response WITHOUT waiting
	// on A's still-blocked flight — so releasing A is not required first.
	select {
	case names := <-bDone:
		if len(names) != 1 || names[0] != "fresh" {
			t.Fatalf("post-clear caller got %v, want [fresh] — it joined the stale pre-clear flight", names)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("post-clear caller blocked on the pre-clear flight (joined it) instead of refetching")
	}

	close(release) // let A finish (cleanup)
	<-aResult
}
