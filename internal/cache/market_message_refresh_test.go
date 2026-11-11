package cache

import (
	"strings"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/internal/apipolicy"
	"github.com/oddin-gg/gosdk/types"
)

// A message that finds the catalog expired reads the stale entry at once
// while the download goes on in the background.
func TestMarketDescriptionCache_MessageReadsAStaleCatalogAtOnce(t *testing.T) {
	srv := newMarketSrv(t, bulkCatalogWithStaticVariants)
	mc, ctx := newMarketCacheForTest(t, srv)
	mc.catalogTTL = 50 * time.Millisecond
	locales := []types.Locale{types.EnLocale}
	if _, err := mc.MarketDescriptionByID(ctx, 9, types.None[string](), locales); err != nil {
		t.Fatalf("seed lookup: %v", err)
	}
	time.Sleep(80 * time.Millisecond)

	hold := make(chan struct{})
	srv.hold.Store(&hold)
	defer close(hold)
	msgCtx := apipolicy.WithMessage(ctx, newGateStub())

	start := time.Now()
	entry, err := mc.MarketDescriptionByID(msgCtx, 9, types.None[string](), locales)
	if err != nil || entry == nil {
		t.Fatalf("message read = %v, %v; want the stale entry", entry, err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("the message read took %v, it must not wait for the refresh", d)
	}
	waitFor(t, "the background refresh to reach the server", func() bool { return srv.bulkHits.Load() == 2 })
}

// Concurrent stale reads of messages start one refresh between them.
func TestMarketDescriptionCache_ConcurrentStaleMessageReadsStartOneRefresh(t *testing.T) {
	srv := newMarketSrv(t, bulkCatalogWithStaticVariants)
	mc, ctx := newMarketCacheForTest(t, srv)
	mc.catalogTTL = 50 * time.Millisecond
	locales := []types.Locale{types.EnLocale}
	if _, err := mc.MarketDescriptionByID(ctx, 9, types.None[string](), locales); err != nil {
		t.Fatalf("seed lookup: %v", err)
	}
	time.Sleep(80 * time.Millisecond)

	hold := make(chan struct{})
	srv.hold.Store(&hold)
	msgCtx := apipolicy.WithMessage(ctx, newGateStub())
	for range 20 {
		if _, err := mc.MarketDescriptionByID(msgCtx, 9, types.None[string](), locales); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "the refresh to reach the server", func() bool { return srv.bulkHits.Load() >= 2 })
	close(hold)
	waitFor(t, "the refresh to finish", func() bool { return mc.localeLoaded(types.EnLocale) })
	if got := srv.bulkHits.Load(); got != 2 {
		t.Fatalf("bulk hits = %d, want 2 (the seed and one shared refresh)", got)
	}
}

// With the breaker open nothing is requested, and neither is anything while
// the locale is inside its refresh backoff.
func TestMarketDescriptionCache_NoBackgroundRefreshWhileOpenOrBackedOff(t *testing.T) {
	srv := newMarketSrv(t, bulkCatalogWithStaticVariants)
	mc, ctx := newMarketCacheForTest(t, srv)
	mc.catalogTTL = 50 * time.Millisecond
	mc.refreshBackoff = time.Hour
	locales := []types.Locale{types.EnLocale}
	if _, err := mc.MarketDescriptionByID(ctx, 9, types.None[string](), locales); err != nil {
		t.Fatalf("seed lookup: %v", err)
	}
	time.Sleep(80 * time.Millisecond)

	gate := newGateStub()
	gate.setOpen()
	if _, err := mc.MarketDescriptionByID(apipolicy.WithMessage(ctx, gate), 9, types.None[string](), locales); err != nil {
		t.Fatalf("read with the gate open: %v (the stale entry must be served)", err)
	}

	mc.mu.Lock()
	mc.refreshFailedAt[types.EnLocale] = time.Now()
	mc.mu.Unlock()
	if _, err := mc.MarketDescriptionByID(apipolicy.WithMessage(ctx, newGateStub()), 9, types.None[string](), locales); err != nil {
		t.Fatalf("read inside the backoff: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if got := srv.bulkHits.Load(); got != 1 {
		t.Fatalf("bulk hits = %d, want 1 (the seed only)", got)
	}
}

// A getter that asks while the background refresh runs joins it and reads
// the rows it downloaded.
func TestMarketDescriptionCache_GetterJoinsTheBackgroundRefresh(t *testing.T) {
	srv := newMarketSrv(t, bulkCatalogWithStaticVariants)
	mc, ctx := newMarketCacheForTest(t, srv)
	mc.catalogTTL = 50 * time.Millisecond
	locales := []types.Locale{types.EnLocale}
	if _, err := mc.MarketDescriptionByID(ctx, 9, types.None[string](), locales); err != nil {
		t.Fatalf("seed lookup: %v", err)
	}
	time.Sleep(80 * time.Millisecond)

	hold := make(chan struct{})
	srv.hold.Store(&hold)
	srv.serve(strings.ReplaceAll(bulkCatalogWithStaticVariants, `name="Plain"`, `name="Renamed"`))
	if _, err := mc.MarketDescriptionByID(apipolicy.WithMessage(ctx, newGateStub()), 9, types.None[string](), locales); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the background refresh to reach the server", func() bool { return srv.bulkHits.Load() == 2 })

	type result struct {
		name string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		e, err := mc.MarketDescriptionByID(ctx, 9, types.None[string](), locales)
		if err != nil {
			done <- result{err: err}
			return
		}
		done <- result{name: e.Snapshot().Names[types.EnLocale]}
	}()
	time.Sleep(50 * time.Millisecond)
	close(hold)
	r := <-done
	if r.err != nil || r.name != "Renamed" {
		t.Fatalf("getter = %q, %v; want the fresh rows", r.name, r.err)
	}
	if got := srv.bulkHits.Load(); got != 2 {
		t.Fatalf("bulk hits = %d, want 2 (the getter joined the refresh)", got)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
