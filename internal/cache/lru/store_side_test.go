package lru

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sync/singleflight"
)

type sideEntry struct{ locales []string }

func (e sideEntry) Locales() []string { return e.locales }

// TestStoreSide_SuppressedByLaterClear pins the seventh-pass P2 fence:
// a side-map store whose snapshot predates a Clear must be suppressed —
// and because the store runs UNDER the clear lock, there is no window
// between the tombstone check and the write for a Clear to slip through.
func TestStoreSide_SuppressedByLaterClear(t *testing.T) {
	side := map[string]string{}
	var sideMu sync.Mutex
	c := NewEventCache[string, string, sideEntry](Config{
		OnEvict: func(key any, _ any) {
			sideMu.Lock()
			delete(side, key.(string))
			sideMu.Unlock()
		},
	}, func(_ context.Context, _ string, locales []string, _ sideEntry, _ bool) (sideEntry, error) {
		return sideEntry{locales: locales}, nil
	})

	// Snapshot taken BEFORE the clear: the store must be suppressed.
	before := time.Now()
	time.Sleep(time.Millisecond)
	c.Clear("k")
	if ok := c.StoreSide("k", before, func() {
		sideMu.Lock()
		side["k"] = "stale-icon"
		sideMu.Unlock()
	}); ok {
		t.Fatal("StoreSide ran a store whose snapshot predates the Clear")
	}
	sideMu.Lock()
	_, present := side["k"]
	sideMu.Unlock()
	if present {
		t.Fatal("suppressed store still wrote the side value")
	}

	// Snapshot taken AFTER the clear: fresh data, store allowed.
	if ok := c.StoreSide("k", time.Now(), func() {
		sideMu.Lock()
		side["k"] = "fresh-icon"
		sideMu.Unlock()
	}); !ok {
		t.Fatal("StoreSide suppressed a store whose snapshot postdates the Clear")
	}
}

// TestStoreSide_AtomicWithClear hammers Clear against StoreSide with
// pre-clear snapshots under -race: the invariant is that once ALL clears
// have finished, no store whose snapshot predates the FIRST clear can
// have survived — either it ran first (and a clear's eviction wiped the
// parent + the racing clear tombstone suppressed re-stores) or the
// tombstone suppressed it. With the old check-then-store shape a store
// could pass the check, lose the CPU, and write AFTER the clear removed
// everything.
func TestStoreSide_AtomicWithClear(t *testing.T) {
	for round := 0; round < 200; round++ {
		side := map[string]string{}
		var sideMu sync.Mutex
		c := NewEventCache[string, string, sideEntry](Config{},
			func(_ context.Context, _ string, locales []string, _ sideEntry, _ bool) (sideEntry, error) {
				return sideEntry{locales: locales}, nil
			})

		snapshot := time.Now() // both racers use a pre-clear snapshot
		time.Sleep(time.Microsecond)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			c.StoreSide("k", snapshot, func() {
				sideMu.Lock()
				side["k"] = "icon"
				sideMu.Unlock()
			})
		}()
		go func() {
			defer wg.Done()
			c.Clear("k")
		}()
		wg.Wait()

		// Whatever the interleaving, a FOLLOW-UP store with the stale
		// snapshot must now be suppressed (the tombstone is set), and the
		// cache entry is gone. If the racing store won, its value may
		// remain — that ordering is "store happened-before clear", which
		// production pairs with the parent-entry eviction wiping side
		// state via OnEvict (exercised in the deterministic test above).
		if ok := c.StoreSide("k", snapshot, func() {
			sideMu.Lock()
			side["k"] = "stale"
			sideMu.Unlock()
		}); ok {
			t.Fatalf("round %d: post-clear StoreSide with pre-clear snapshot was not suppressed", round)
		}
	}
}

// TestLoadCoalesced_LifetimeCancelAbortsDetachedLoad pins the seventh-
// pass P2 lifetime contract: the shared load detaches from the CALLER's
// ctx but not from the owner's lifetime — cancelling the lifetime ctx
// (Manager.Close / client teardown) must cancel the in-flight load
// instead of letting it run to LoadTimeout.
func TestLoadCoalesced_LifetimeCancelAbortsDetachedLoad(t *testing.T) {
	var sf singleflight.Group
	lifetime, lifetimeCancel := context.WithCancel(context.Background())

	loadCtxErr := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		// Caller with a short deadline: it stops WAITING, but pre-fix the
		// load itself kept running detached for up to LoadTimeout.
		callerCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, _ = LoadCoalesced(callerCtx, lifetime, &sf, "k", func(loadCtx context.Context) (int, error) {
			close(started)
			select {
			case <-loadCtx.Done():
				loadCtxErr <- loadCtx.Err()
			case <-time.After(5 * time.Second):
				loadCtxErr <- nil
			}
			return 0, nil
		})
	}()

	<-started
	// The caller has (or will) come back with DeadlineExceeded; the load
	// must still be running — now cancel the OWNER's lifetime.
	time.Sleep(80 * time.Millisecond)
	lifetimeCancel()

	select {
	case err := <-loadCtxErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("load ctx err = %v, want Canceled via lifetime cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("lifetime cancellation did not reach the detached load")
	}
}

// TestEventCacheGet_LifetimeCancelAbortsDetachedLoad is the EventCache
// twin: Config.Lifetime is the detach root for Get's singleflight loads.
func TestEventCacheGet_LifetimeCancelAbortsDetachedLoad(t *testing.T) {
	lifetime, lifetimeCancel := context.WithCancel(context.Background())
	loadCtxErr := make(chan error, 1)
	started := make(chan struct{})

	c := NewEventCache[string, string, sideEntry](Config{Lifetime: lifetime},
		func(ctx context.Context, _ string, locales []string, _ sideEntry, _ bool) (sideEntry, error) {
			close(started)
			select {
			case <-ctx.Done():
				loadCtxErr <- ctx.Err()
			case <-time.After(5 * time.Second):
				loadCtxErr <- nil
			}
			return sideEntry{locales: locales}, nil
		})

	go func() {
		callerCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, _, _ = c.Get(callerCtx, "k", []string{"en"})
	}()

	<-started
	time.Sleep(80 * time.Millisecond)
	lifetimeCancel()

	select {
	case err := <-loadCtxErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("loader ctx err = %v, want Canceled via lifetime cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("lifetime cancellation did not reach the detached loader")
	}
}

// TestPurge_NotUndoneByInFlightLoad pins the ninth-pass P3: Purge must
// behave like a global Clear under concurrency — a loader that started
// BEFORE the purge cannot finish afterward and repopulate the cache
// (nor, via OnAdmit, commit side state for an entry the purge already
// covered). The retry that follows the discarded flight runs post-purge
// and here fails, so anything left in the cache or side map can only
// have come from the pre-purge flight.
func TestPurge_NotUndoneByInFlightLoad(t *testing.T) {
	side := map[string]string{}
	var sideMu sync.Mutex
	gate := make(chan struct{})
	started := make(chan struct{})
	var calls atomic.Int32

	c := NewEventCache[string, string, sideEntry](Config{
		OnEvict: func(key any, _ any) {
			sideMu.Lock()
			delete(side, key.(string))
			sideMu.Unlock()
		},
		OnAdmit: func(key any, _ any) {
			sideMu.Lock()
			side[key.(string)] = "icon"
			sideMu.Unlock()
		},
	}, func(_ context.Context, _ string, locales []string, _ sideEntry, _ bool) (sideEntry, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-gate
			return sideEntry{locales: locales}, nil
		}
		return sideEntry{}, errors.New("post-purge fetch fails")
	})

	done := make(chan error, 1)
	go func() {
		_, _, err := c.Get(context.Background(), "k", []string{"en"})
		done <- err
	}()
	<-started
	c.Purge() // lands while the first load is in flight
	close(gate)

	if err := <-done; err == nil {
		t.Fatal("expected the post-purge retry to fail (loader errors after first call)")
	}
	if got := c.Len(); got != 0 {
		t.Fatalf("cache len = %d after Purge, want 0 — pre-purge flight repopulated it", got)
	}
	sideMu.Lock()
	_, orphaned := side["k"]
	sideMu.Unlock()
	if orphaned {
		t.Fatal("pre-purge flight committed side state via OnAdmit after Purge")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("loader calls = %d, want 2 (pre-purge flight + post-purge retry)", got)
	}
}

// TestClear_DoesNotForgetNewerPostPurgeFlight pins the tenth-pass P2:
// Clear must Forget the flight key for the generation current at its
// tombstone, NOT whatever generation exists by the time it calls
// Forget. The clearForgetHook fires in exactly the reported window —
// after Clear captures its flight key and releases clearMu, before the
// Forget — and simulates the delayed-Clear race: it Purges (advancing
// the generation) and starts a legitimate post-Purge flight B for the
// same key. If Clear (buggy) recomputed the key after this, it would
// Forget B; a second post-Purge caller D would then start a SECOND load
// instead of coalescing. The fix captures the key under the lock, so
// Clear forgets only the (stale, harmless) pre-Purge key and B survives
// — D coalesces onto it and the loader runs exactly once post-Purge.
func TestClear_DoesNotForgetNewerPostPurgeFlight(t *testing.T) {
	var loaderCalls atomic.Int32
	entered := make(chan struct{}, 8)
	gate := make(chan struct{})

	c := NewEventCache[string, string, sideEntry](Config{},
		func(_ context.Context, _ string, locales []string, _ sideEntry, _ bool) (sideEntry, error) {
			loaderCalls.Add(1)
			entered <- struct{}{}
			<-gate
			return sideEntry{locales: locales}, nil
		})

	var bDone sync.WaitGroup
	clearForgetHook = func() {
		clearForgetHook = nil // one-shot: fire only for this Clear
		c.Purge()             // advance the generation past Clear's captured key
		bDone.Add(1)
		go func() {
			defer bDone.Done()
			_, _, _ = c.Get(context.Background(), "k", []string{"ru"})
		}()
		<-entered // B's loader is running → its post-Purge flight is registered
	}
	defer func() { clearForgetHook = nil }()

	// Clear captures the pre-Purge flight key under clearMu; the hook then
	// runs the Purge + starts B; Clear forgets its captured (stale) key.
	c.Clear("k")

	// D: a second post-Purge caller for the same key. It must COALESCE
	// onto B's still-in-flight load, not start a fresh one.
	var dDone sync.WaitGroup
	dDone.Add(1)
	go func() {
		defer dDone.Done()
		_, _, _ = c.Get(context.Background(), "k", []string{"ru"})
	}()

	// If Clear forgot B's flight, D starts a second loader invocation and
	// signals `entered` again within this window.
	secondLoad := false
	select {
	case <-entered:
		secondLoad = true
	case <-time.After(300 * time.Millisecond):
	}

	close(gate)
	bDone.Wait()
	dDone.Wait()

	if secondLoad {
		t.Fatal("Clear forgot the newer post-Purge flight — D started a second load instead of coalescing")
	}
	if got := loaderCalls.Load(); got != 1 {
		t.Fatalf("post-Purge loader calls = %d, want 1 (B + D coalesced into one load)", got)
	}
}
