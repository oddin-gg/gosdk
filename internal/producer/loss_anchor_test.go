package producer

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	log "github.com/oddin-gg/gosdk/internal/log"
)

// TestManager_LossAnchor pins which cursor a recovery after a lost queue
// must reach back to: the alive BEFORE the most recent one (the most
// recent may have been generated after the queue died), an explicit
// rewind when one is in force, the only alive when there is no earlier
// one, and zero when the producer has no cursor at all.
func TestManager_LossAnchor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, producersBody)
	}))
	defer srv.Close()
	mgr := NewManager(&minimalCfg{}, newAPIClient(t, srv), log.New(nil))
	if err := mgr.Open(t.Context()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	const id = 1
	now := time.Now().Truncate(time.Millisecond)

	if got, err := mgr.LossAnchor(id); err != nil || !got.IsZero() {
		t.Fatalf("no cursor yet: anchor = %v, %v; want zero", got, err)
	}
	if _, err := mgr.LossAnchor(999); err == nil {
		t.Fatal("unknown producer must error, not return a zero anchor")
	}

	a1 := now.Add(-20 * time.Second)
	if err := mgr.SetLastAliveReceivedGenTimestamp(id, a1); err != nil {
		t.Fatal(err)
	}
	if got, _ := mgr.LossAnchor(id); !got.Equal(a1) {
		t.Fatalf("one alive: anchor = %v, want that alive %v", got, a1)
	}

	a2 := now.Add(-10 * time.Second)
	if err := mgr.SetLastAliveReceivedGenTimestamp(id, a2); err != nil {
		t.Fatal(err)
	}
	if got, _ := mgr.LossAnchor(id); !got.Equal(a1) {
		t.Fatalf("two alives: anchor = %v, want the earlier %v (the latest is not trusted)", got, a1)
	}
	// An out-of-order (older) alive does not disturb the pair.
	if err := mgr.SetLastAliveReceivedGenTimestamp(id, now.Add(-30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if got, _ := mgr.LossAnchor(id); !got.Equal(a1) {
		t.Fatalf("stale alive shifted the anchor to %v, want %v", got, a1)
	}

	rewind := now.Add(-5 * time.Second)
	if err := mgr.SetProducerRecoveryFromTimestamp(t.Context(), id, rewind); err != nil {
		t.Fatal(err)
	}
	if got, _ := mgr.LossAnchor(id); !got.Equal(rewind) {
		t.Fatalf("explicit rewind in force: anchor = %v, want %v", got, rewind)
	}
}
