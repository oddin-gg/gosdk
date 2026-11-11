package gosdk

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	"github.com/oddin-gg/gosdk/types"
)

// TestClient_Close_WithMessageFlightsHungOnTheServer closes a client while
// the loads that message builds started are all hung on the REST server:
// Close returns within its bound, the builds end, and no goroutine of the
// client or its flights is left (the count is polled back to where the test
// began; there is no goleak in the repo).
func TestClient_Close_WithMessageFlightsHungOnTheServer(t *testing.T) {
	var hung atomic.Int32
	release := make(chan struct{})
	var releaseOnce sync.Once
	unhold := func() { releaseOnce.Do(func() { close(release) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		switch {
		case strings.Contains(r.URL.Path, "/users/whoami"):
			_, _ = io.WriteString(w, whoAmIBody)
		case strings.Contains(r.URL.Path, "/descriptions/producers"):
			_, _ = io.WriteString(w, `<producers response_code="OK"><producer id="1" name="live" description="Live" active="true" api_url="https://x" scope="live" stateful_recovery_window_in_minutes="60"/></producers>`)
		default:
			hung.Add(1)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			http.Error(w, "late", http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	defer unhold()
	// Taken with the server running: its accept loop is not the client's.
	baseline := goruntime.NumGoroutine()

	cfg := NewConfig("t", types.IntegrationEnvironment,
		WithAPIHost("api.example.test"),
		WithHTTPClient(newTestHTTPClient(srv)),
		WithShutdownTimeout(2*time.Second),
	)
	bootCtx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	c, err := New(bootCtx, cfg)
	cancel()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.producerManager.Open(t.Context()); err != nil {
		t.Fatalf("producer manager open: %v", err)
	}

	// Messages for several matches: each needs its own load, and the market
	// names need the catalog.
	sportID, _ := types.ParseURN("od:sport:1")
	active := 1
	var builds sync.WaitGroup
	for _, id := range []string{"od:match:41", "od:match:42", "od:match:43"} {
		eventID, _ := types.ParseURN(id)
		market := &feedXML.MarketWithOutcome{}
		market.ID = 1
		market.Outcomes = []feedXML.Outcome{{ID: "1", Active: &active}}
		msg := &feedXML.OddsChange{EventID: id, ProductID: 1}
		msg.Odds.Markets = []*feedXML.MarketWithOutcome{market}
		builds.Add(1)
		go func() {
			defer builds.Done()
			_, _ = c.feedMessageFactory.BuildMessage(t.Context(), &types.FeedMessage{
				BasicFeedMessage: types.BasicFeedMessage{
					RawMessage: []byte(`<odds_change/>`),
					RoutingKey: &types.RoutingKeyInfo{
						FullRoutingKey: "hi.live.-.odds_change.1." + strings.Replace(id, ":", ".", 1) + ".-.-",
						SportID:        sportID,
						EventID:        eventID,
					},
				},
				Message: msg,
			})
		}()
	}
	waitForCount(t, "the flights to reach the server", &hung, 3)

	start := time.Now()
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	err = c.Close(closeCtx)
	closeCancel()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("Close took %v, want within the shutdown timeout", d)
	}
	builds.Wait()

	// The server is still holding: flights that were not stopped would keep
	// their requests, connections and goroutines until the HTTP timeout of
	// the test client (2 s), so the window here is shorter than that.
	deadline := time.Now().Add(1 * time.Second)
	for goruntime.NumGoroutine() > baseline {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			t.Fatalf("goroutines = %d, baseline %d\n%s", goruntime.NumGoroutine(), baseline, buf[:goruntime.Stack(buf, true)])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForCount(t *testing.T, what string, n *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for n.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s (%d of %d)", what, n.Load(), want)
		}
		time.Sleep(time.Millisecond)
	}
}
