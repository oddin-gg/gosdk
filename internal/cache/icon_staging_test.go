package cache

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

// TestCompetitorCache_FailedLoadDoesNotOrphanIcon pins the eighth-pass
// P2: the loader used to COMMIT the icon side-map entry per locale,
// before the remaining locale fetches, coverage validation, and the
// EventCache admission ran. A multi-locale load whose later step failed
// left the icon behind with NO parent entry — unreachable by eviction,
// served forever by CompetitorIcon, and unbounded across failing IDs.
// Icons are now staged on the entry and committed only on admission.
func TestCompetitorCache_FailedLoadDoesNotOrphanIcon(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/ru/") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<?xml version="1.0"?>
<competitor_profile generated_at="2026-01-01T00:00:00Z">
  <competitor id="od:competitor:9" name="Team" abbreviation="TM" icon_path="/icons/c9.png"/>
</competitor_profile>`)
	}))
	t.Cleanup(srv.Close)

	cc := newCompetitorCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))
	id := types.URN{Prefix: "od", Type: "competitor", ID: 9}

	// en succeeds (and stages the icon); ru 500s — the load fails, the
	// entry is never admitted, and the staged icon must die with it.
	if _, err := cc.Competitor(t.Context(), id, []types.Locale{types.EnLocale, types.RuLocale}); err == nil {
		t.Fatal("expected the multi-locale load to fail (ru 500s)")
	}
	if _, ok := cc.loadIcon(id); ok {
		t.Fatal("failed load left an orphaned competitor icon behind (no parent entry can ever evict it)")
	}
	if _, ok := cc.lru.Peek(id); ok {
		t.Fatal("failed load admitted a parent entry — test premise broken")
	}
}

// TestCompetitorCache_SuccessfulLoadCommitsIcon pins the other half:
// the opportunistic loader-populated icon must still land once the
// parent entry IS admitted — staging must not silently drop it.
func TestCompetitorCache_SuccessfulLoadCommitsIcon(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<?xml version="1.0"?>
<competitor_profile generated_at="2026-01-01T00:00:00Z">
  <competitor id="od:competitor:10" name="Team" abbreviation="TM" icon_path="/icons/c10.png"/>
</competitor_profile>`)
	}))
	t.Cleanup(srv.Close)

	cc := newCompetitorCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))
	id := types.URN{Prefix: "od", Type: "competitor", ID: 10}

	if _, err := cc.Competitor(t.Context(), id, []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("Competitor: %v", err)
	}
	icon, ok := cc.loadIcon(id)
	if !ok || icon == nil || *icon != "/icons/c10.png" {
		t.Fatalf("loader-populated icon not committed on admission: icon=%v ok=%v", icon, ok)
	}
}

// TestTournamentCache_FailedLoadDoesNotOrphanIcon is the tournament twin
// of the competitor orphan test.
func TestTournamentCache_FailedLoadDoesNotOrphanIcon(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/ru/") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<?xml version="1.0"?>
<tournament_info generated_at="2026-01-01T00:00:00Z">
  <tournament id="od:tournament:11" name="T" icon_path="/icons/t11.png">
    <sport id="od:sport:1" name="Football"/>
  </tournament>
</tournament_info>`)
	}))
	t.Cleanup(srv.Close)

	tc := newTournamentCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))
	id := types.URN{Prefix: "od", Type: "tournament", ID: 11}

	if _, err := tc.Tournament(t.Context(), id, []types.Locale{types.EnLocale, types.RuLocale}); err == nil {
		t.Fatal("expected the multi-locale load to fail (ru 500s)")
	}
	tc.iconMu.RLock()
	_, orphaned := tc.icons[id]
	tc.iconMu.RUnlock()
	if orphaned {
		t.Fatal("failed load left an orphaned tournament icon behind (no parent entry can ever evict it)")
	}
}

// TestLocalizedStaticDataCache_CloseCancelsInFlightFetch pins the
// eighth-pass P2: the static-data fetch used to run under
// WithoutCancel(callerCtx) with no lifetime root and no LoadTimeout —
// Close() stopped the refresh timer but an in-flight
// FetchMatchStatusDescriptions kept running (indefinitely, on a custom
// HTTP client without a timeout) and could write after Client.Close.
// The fetch is now rooted in the cache's lifetime ctx.
func TestLocalizedStaticDataCache_CloseCancelsInFlightFetch(t *testing.T) {
	fetchErr := make(chan error, 1)
	started := make(chan struct{})
	fetcher := func(ctx context.Context, _ types.Locale) ([]types.StaticData, error) {
		close(started)
		select {
		case <-ctx.Done():
			fetchErr <- ctx.Err()
		case <-time.After(5 * time.Second):
			fetchErr <- nil
		}
		return nil, context.Cause(ctx)
	}
	c := newLocalizedStaticDataCache(t.Context(), &fakeCacheCfg{}, log.New(nil), nil, fetcher)

	go func() {
		// Short-deadline caller: it stops WAITING quickly, but the shared
		// fetch keeps running detached — pre-fix, forever.
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, _ = c.Item(ctx, 1)
	}()

	<-started
	time.Sleep(80 * time.Millisecond) // caller has given up; fetch still in flight
	c.Close()

	select {
	case err := <-fetchErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("fetcher ctx err = %v, want Canceled via cache Close", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not cancel the in-flight static-data fetch")
	}
}

// TestLocalizedStaticDataCache_ConcurrentCloseIsSafe pins the ninth-pass
// P3: Close read-and-nilled closeFn without synchronization, so
// concurrent Close() calls (cache.Manager.Close has no guard of its own)
// raced on the field. Now sync.Once-guarded; the race detector is the
// assertion here.
func TestLocalizedStaticDataCache_ConcurrentCloseIsSafe(t *testing.T) {
	c := newLocalizedStaticDataCache(t.Context(), &fakeCacheCfg{}, log.New(nil), nil,
		func(context.Context, types.Locale) ([]types.StaticData, error) { return nil, nil })

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Go(c.Close)
	}
	wg.Wait()
}
