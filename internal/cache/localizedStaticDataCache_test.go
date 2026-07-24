package cache

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

// TestLocalizedStaticDataCache_AlreadyCancelledCtxNoFetch is the
// regression for the reviewer's low finding: an already-cancelled ctx
// must not start any detached fetcher invocation through WithoutCancel.
func TestLocalizedStaticDataCache_AlreadyCancelledCtxNoFetch(t *testing.T) {
	var calls atomic.Int32
	fetcher := func(ctx context.Context, locale types.Locale) ([]types.StaticData, error) {
		calls.Add(1)
		return []types.StaticData{{ID: 1, Description: types.Some("ok")}}, nil
	}
	cfg := &minimalCfg{}
	c := newLocalizedStaticDataCache(t.Context(), cfg, log.New(nil), nil, fetcher)
	defer c.Close()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := c.LocalizedItem(ctx, 1, []types.Locale{types.EnLocale})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := calls.Load(); got != 0 {
		t.Errorf("fetcher invocations = %d, want 0 (cancelled ctx must not start a detached fetch)", got)
	}
}

// TestLocalizedStaticDataCache_CtxCancelledMidIterationStopsFetches
// asserts the per-iteration ctx check in loadLocales: a multi-locale
// call cancelled while one fetcher is in flight must not start later
// detached fetches for the remaining locales.
func TestLocalizedStaticDataCache_CtxCancelledMidIterationStopsFetches(t *testing.T) {
	var calls atomic.Int32
	released := make(chan struct{})
	cancelOnFirst := make(chan struct{}, 1)
	fetcher := func(ctx context.Context, locale types.Locale) ([]types.StaticData, error) {
		calls.Add(1)
		// Signal the test that the first fetcher is in flight, then
		// block until released so the test can cancel mid-iteration.
		select {
		case cancelOnFirst <- struct{}{}:
		default:
		}
		<-released
		return nil, nil
	}
	cfg := &minimalCfg{}
	c := newLocalizedStaticDataCache(t.Context(), cfg, log.New(nil), nil, fetcher)
	defer c.Close()
	defer close(released)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := c.LocalizedItem(ctx, 1, []types.Locale{types.EnLocale, types.DeLocale, types.RuLocale})
		done <- err
	}()

	<-cancelOnFirst        // first fetcher in flight
	cancel()               // cancel ctx mid-iteration
	released <- struct{}{} // release the in-flight fetcher
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}

	// Give a beat to confirm the per-iteration guard prevented further
	// detached fetches.
	time.Sleep(50 * time.Millisecond)
	if got := calls.Load(); got > 1 {
		t.Errorf("fetcher invocations = %d, want 1 (subsequent locales must not fire after ctx cancel)", got)
	}
}

// TestLocalizedStaticDataCache_FirstRefreshAfterInitialDelay is the
// regression for the Codex P2 scheduling finding: startTimer waited
// initialDelay (24h) and then refreshed only on the FIRST TICKER FIRE
// another tickPeriod (24h) later — data loaded near startup stayed
// stale for ~48h instead of ~24h. The fix runs timerTick immediately
// once the initial delay elapses.
//
// The schedule is compressed via the injectable refreshInitialDelay /
// refreshTickPeriod fields: the tick period is set far beyond the test
// deadline, so an observed refresh can ONLY come from the immediate
// post-delay tick — under the old code this test times out.
func TestLocalizedStaticDataCache_FirstRefreshAfterInitialDelay(t *testing.T) {
	var calls atomic.Int32
	refreshed := make(chan struct{}, 8)
	fetcher := func(ctx context.Context, locale types.Locale) ([]types.StaticData, error) {
		if calls.Add(1) > 1 {
			refreshed <- struct{}{}
		}
		return []types.StaticData{{ID: 1, Description: types.Some("ok")}}, nil
	}

	lifeCtx, cancel := context.WithCancel(context.Background())
	c := &LocalizedStaticDataCache{
		oddsFeedConfiguration: &minimalCfg{},
		fetcher:               fetcher,
		locales:               []types.Locale{types.EnLocale},
		internalCache:         make(map[int]map[types.Locale]string),
		loadedLocales:         make(map[types.Locale]struct{}),
		lifeCtx:               lifeCtx,
		closeFn:               cancel,
		timerDone:             make(chan struct{}),
		refreshInitialDelay:   50 * time.Millisecond,
		refreshTickPeriod:     time.Hour,
		logger:                log.New(nil),
	}
	c.startTimer()
	defer c.Close()

	// Load a locale so the refresh has something to re-fetch (timerTick
	// only refreshes locales already marked loaded).
	if _, err := c.LocalizedItem(t.Context(), 1, []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("LocalizedItem: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("initial load fetches = %d, want 1", got)
	}

	select {
	case <-refreshed:
		// Refresh observed well before refreshTickPeriod (1h) could
		// possibly fire — it came from the immediate post-delay tick.
	case <-time.After(5 * time.Second):
		t.Fatal("no refresh within 5s of the initial delay — first refresh is being deferred a full tick period")
	}
}
