package cache

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/internal/apipolicy"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

// When the owner's lifetime ends, the flights it detached stop with it: a
// match load that a message started and the background refresh of a stale
// catalog are both hung on the server, the lifetime is cancelled, and the
// server then answers. Neither flight may write what it received into its
// cache, and no goroutine of theirs may outlive the shutdown.
func TestFlights_DetachedFlightsStopAtClose(t *testing.T) {
	baseline := runtime.NumGoroutine()

	var hung atomic.Int32
	release := make(chan struct{})
	var releaseOnce atomic.Bool
	unhold := func() {
		if releaseOnce.CompareAndSwap(false, true) {
			close(release)
		}
	}
	// The catalog answers the first request at once (the seed) and holds the
	// later ones. Every match request is held. After release both answer with
	// valid data, which is what a flight must not store once it was stopped.
	var catalogHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/markets"):
			if catalogHits.Add(1) > 1 {
				hung.Add(1)
				<-release
			}
			w.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(w, bulkCatalogWithStaticVariants)
		case strings.Contains(r.URL.Path, "/summary"):
			hung.Add(1)
			<-release
			w.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(w, matchSummaryBody("od:match:42", "en"))
		default:
			http.NotFound(w, r)
		}
	}))
	// Handlers must return before the server can close, and the test must
	// not wait on them if it fails early.
	defer srv.Close()
	defer unhold()

	life, closeCaches := context.WithCancel(t.Context())
	defer closeCaches()
	client := newAPIClientForTest(t, srv)
	markets := newMarketDescriptionCache(life, client, log.New(nil))
	matches := newMatchCache(life, client, log.New(nil))
	locales := []types.Locale{types.EnLocale}

	// A catalog the message path finds expired: it reads the stale entry and
	// starts the refresh in the background.
	markets.catalogTTL = 50 * time.Millisecond
	if _, err := markets.MarketDescriptionByID(t.Context(), 9, types.None[string](), locales); err != nil {
		t.Fatalf("seed lookup: %v", err)
	}
	time.Sleep(80 * time.Millisecond)
	markets.mu.RLock()
	loadedAt := markets.loadedLocales[types.EnLocale]
	markets.mu.RUnlock()

	msg := apipolicy.WithMessage(t.Context(), newGateStub(), time.Minute)
	if _, err := markets.MarketDescriptionByID(msg, 9, types.None[string](), locales); err != nil {
		t.Fatalf("stale read: %v", err)
	}
	urn, _ := types.ParseURN("od:match:42")
	matchDone := make(chan error, 1)
	go func() {
		_, err := matches.Match(msg, *urn, locales)
		matchDone <- err
	}()
	waitFor(t, "both flights to reach the server", func() bool { return hung.Load() == 2 })

	closeCaches()
	select {
	case err := <-matchDone:
		if err == nil {
			t.Fatal("the message's wait ended with a match although the lifetime ended")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a message kept waiting on a flight after the lifetime ended")
	}

	unhold()
	// Give a flight that ignored the lifetime the time to commit.
	time.Sleep(200 * time.Millisecond)
	if _, ok := matches.lru.Peek(*urn); ok {
		t.Fatal("a stopped match flight wrote its entry to the cache")
	}
	markets.mu.RLock()
	after := markets.loadedLocales[types.EnLocale]
	markets.mu.RUnlock()
	if !after.Equal(loadedAt) {
		t.Fatal("a stopped catalog refresh committed its download")
	}

	srv.Close()
	// No goleak here: poll the count back to where the test started.
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > baseline {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			t.Fatalf("goroutines = %d, baseline %d\n%s", runtime.NumGoroutine(), baseline, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A background catalog refresh whose download completed just before the
// lifetime ended must not commit it afterwards: the owner declared the cache
// quiescent. The lifetime is cancelled at the point between the download and
// the stores, where the request itself can no longer be cut short.
func TestFlights_ARefreshThatDownloadedBeforeCloseCommitsNothing(t *testing.T) {
	srv := newMarketSrv(t, bulkCatalogWithStaticVariants)
	life, closeCaches := context.WithCancel(t.Context())
	defer closeCaches()
	mc := newMarketDescriptionCache(life, newAPIClientForTest(t, srv.Server), log.New(nil))
	mc.catalogTTL = 50 * time.Millisecond
	locales := []types.Locale{types.EnLocale}
	if _, err := mc.MarketDescriptionByID(t.Context(), 9, types.None[string](), locales); err != nil {
		t.Fatalf("seed lookup: %v", err)
	}
	time.Sleep(80 * time.Millisecond)
	mc.mu.RLock()
	loadedAt := mc.loadedLocales[types.EnLocale]
	mc.mu.RUnlock()

	// The refreshed catalog differs: one market left, renamed. Committed, it
	// would also prune the other rows.
	srv.serve(`<?xml version="1.0"?><market_descriptions response_code="OK">` +
		`<market id="9" name="Renamed"><outcomes><outcome id="1" name="renamed-o1"/><outcome id="2" name="o2"/></outcomes></market>` +
		`</market_descriptions>`)
	hookRan := make(chan struct{})
	marketCommitGateHook = func(types.Locale) {
		closeCaches()
		close(hookRan)
	}
	t.Cleanup(func() { marketCommitGateHook = nil })

	msg := apipolicy.WithMessage(t.Context(), newGateStub(), time.Minute)
	if _, err := mc.MarketDescriptionByID(msg, 9, types.None[string](), locales); err != nil {
		t.Fatalf("stale read: %v", err)
	}
	select {
	case <-hookRan:
	case <-time.After(5 * time.Second):
		t.Fatal("the background refresh never reached its stores")
	}
	// The flight is over once it no longer counts as loading the locale.
	waitFor(t, "the refresh to end", func() bool {
		mc.mu.RLock()
		defer mc.mu.RUnlock()
		return len(mc.loadingLocales) == 0
	})

	mc.mu.RLock()
	after := mc.loadedLocales[types.EnLocale]
	mc.mu.RUnlock()
	if !after.Equal(loadedAt) {
		t.Fatal("a refresh that finished after the lifetime ended marked the locale loaded")
	}
	if got := mc.baseLen(); got != 3 {
		t.Fatalf("base holds %d entries, want the 3 of the seed: the refresh committed after the lifetime ended", got)
	}
	// The market the refresh also carried keeps what the seed stored.
	existing, ok := mc.lookup(variantKey(9, types.None[string]()))
	if !ok {
		t.Fatal("market 9 vanished")
	}
	if name, _ := existing.Name(types.EnLocale); name != "Plain" {
		t.Fatalf("market 9 name = %q, want %q: the refresh committed after the lifetime ended", name, "Plain")
	}
	if name, found, _ := existing.OutcomeName("1", types.EnLocale); !found || name != "o1" {
		t.Fatalf("market 9 outcome 1 = %q (found=%v), want o1", name, found)
	}
	if _, found, _ := existing.OutcomeName("2", types.EnLocale); found {
		t.Fatal("market 9 gained an outcome from a refresh that finished after the lifetime ended")
	}
}
