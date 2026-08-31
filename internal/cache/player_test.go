package cache

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

// playerProfileBody renders a minimal /players/{locale}/players/{id}/profile
// XML response so the api.Client decodes a non-nil *PlayerProfile.
func playerProfileBody(id string) string {
	return fmt.Sprintf(`<?xml version="1.0"?>
<player_profile>
  <player id="%s" name="Player %s" full_name="Player Full %s"/>
</player_profile>`, id, id, id)
}

// TestPlayersCache_DistinctKeysLoadInParallel verifies the v2.24 F2
// fix: distinct (PlayerID, Locale) cache misses load concurrently
// via singleflight.Group rather than serialising on a global
// loadMu. The test stands up an HTTP fixture that holds each
// request for ~120 ms, then issues 4 parallel GetPlayer calls for
// 4 distinct keys; with the global-mutex serialisation, this would
// take ~480 ms, with singleflight it takes ~120 ms.
func TestPlayersCache_DistinctKeysLoadInParallel(t *testing.T) {
	const perRequestDelay = 120 * time.Millisecond

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Path looks like /sports/{locale}/players/{id}/profile (or
		// similar — we don't validate, we just delay + reply with the
		// id taken from the path).
		hits.Add(1)
		time.Sleep(perRequestDelay)
		// Extract id-ish token from path tail.
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		id := "unknown"
		for i, p := range parts {
			if p == "players" && i+1 < len(parts) {
				id = parts[i+1]
				break
			}
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, playerProfileBody(id))
	}))
	defer srv.Close()

	pc := newPlayersCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))

	keys := []PlayerCacheKey{
		{PlayerID: "od:player:1", Locale: types.EnLocale},
		{PlayerID: "od:player:2", Locale: types.EnLocale},
		{PlayerID: "od:player:3", Locale: types.EnLocale},
		{PlayerID: "od:player:4", Locale: types.EnLocale},
	}

	var wg sync.WaitGroup
	wg.Add(len(keys))
	errs := make([]error, len(keys))
	start := time.Now()
	for i, k := range keys {
		go func(i int, k PlayerCacheKey) {
			defer wg.Done()
			_, err := pc.GetPlayer(t.Context(), k)
			errs[i] = err
		}(i, k)
	}
	wg.Wait()
	elapsed := time.Since(start)

	for i, err := range errs {
		if err != nil {
			t.Errorf("key %d: %v", i, err)
		}
	}
	if h := hits.Load(); h != int32(len(keys)) {
		t.Errorf("HTTP hits = %d, want %d (one per distinct key)", h, len(keys))
	}
	// Generous bound: 4 parallel requests at ~120 ms each should
	// complete well under 300 ms. Pre-fix global-mutex serialisation
	// would take ~480 ms. The 300 ms ceiling tolerates scheduler
	// jitter without making the test flaky.
	if elapsed > 300*time.Millisecond {
		t.Errorf("4 parallel distinct-key loads took %v, want <300ms (singleflight should not serialise distinct keys)", elapsed)
	}
}

// TestPlayersCache_SingleCallBatchLoadsInParallel pins the WITHIN-call
// fan-out: one GetPlayers call with several missing keys must load
// them as a bounded parallel batch, not one at a time. Pre-fix only
// cross-CALLER parallelism existed (via singleflight); a single cold
// roster resolution still issued its round-trips sequentially, so
// BuildMatch's cold path scaled linearly with roster size.
func TestPlayersCache_SingleCallBatchLoadsInParallel(t *testing.T) {
	const perRequestDelay = 120 * time.Millisecond

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(perRequestDelay)
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		id := "unknown"
		for i, p := range parts {
			if p == "players" && i+1 < len(parts) {
				id = parts[i+1]
				break
			}
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, playerProfileBody(id))
	}))
	defer srv.Close()

	pc := newPlayersCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))

	keys := []PlayerCacheKey{
		{PlayerID: "od:player:1", Locale: types.EnLocale},
		{PlayerID: "od:player:2", Locale: types.EnLocale},
		{PlayerID: "od:player:3", Locale: types.EnLocale},
		{PlayerID: "od:player:4", Locale: types.EnLocale},
	}

	start := time.Now()
	got, err := pc.GetPlayers(t.Context(), keys)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("GetPlayers: %v", err)
	}
	if len(got) != len(keys) {
		t.Fatalf("players = %d, want %d", len(got), len(keys))
	}
	for _, k := range keys {
		if p, ok := got[k]; !ok || p.ID != k.PlayerID {
			t.Fatalf("key %s: got %+v", k, got[k])
		}
	}
	if h := hits.Load(); h != int32(len(keys)) {
		t.Errorf("HTTP hits = %d, want %d (one per distinct key)", h, len(keys))
	}
	// Generous bound: 4 batched requests at ~120 ms each should finish
	// well under 300 ms; the pre-fix sequential loop took ~480 ms.
	if elapsed > 300*time.Millisecond {
		t.Errorf("single-call batch of 4 took %v, want <300ms (misses must fan out)", elapsed)
	}
}

// TestPlayersCache_DuplicateKeyDeduplicated verifies the converse:
// concurrent calls for the SAME key share a single in-flight HTTP
// request. Pre-fix the global mutex did this implicitly; the
// singleflight rewrite must preserve the dedup.
func TestPlayersCache_DuplicateKeyDeduplicated(t *testing.T) {
	const perRequestDelay = 100 * time.Millisecond

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(perRequestDelay)
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, playerProfileBody("od:player:42"))
	}))
	defer srv.Close()

	pc := newPlayersCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))
	key := PlayerCacheKey{PlayerID: "od:player:42", Locale: types.EnLocale}

	const callers = 8
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			_, _ = pc.GetPlayer(t.Context(), key)
		}()
	}
	wg.Wait()

	if h := hits.Load(); h != 1 {
		t.Errorf("HTTP hits = %d, want 1 (singleflight should dedup duplicate-key calls)", h)
	}
}

// TestPlayersCache_ClearStorm_NeverLeaksStaleFlightOrDiverges is the
// regression for the register-vs-clear split-flight window (Codex P2):
// a getter reads flightGen, a Purge/Clear advances it and records its
// tombstone, and only THEN does the getter register its singleflight —
// pre-fix that old-generation flight ran to completion invisible to the
// time-based tombstones (its load STARTED after the clear) while
// new-generation callers registered a second, uncoalesced flight for
// the same key; the slower flight's store could roll back the newer
// one. Post-fix the loader closure re-checks the generation on entry
// and the caller retries under the fresh one, so (a) the internal
// errStaleFlight sentinel never escapes to callers and (b) every call
// converges to a real value even under a storm of invalidations.
func TestPlayersCache_ClearStorm_NeverLeaksStaleFlightOrDiverges(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		id := "unknown"
		for i, p := range parts {
			if p == "players" && i+1 < len(parts) {
				id = parts[i+1]
				break
			}
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, playerProfileBody(id))
	}))
	defer srv.Close()

	pc := newPlayersCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))
	key := PlayerCacheKey{PlayerID: "od:player:7", Locale: types.EnLocale}

	stop := make(chan struct{})
	var storm sync.WaitGroup
	storm.Add(1)
	go func() { // invalidation storm racing every load's registration
		defer storm.Done()
		for {
			select {
			case <-stop:
				return
			default:
				pc.Purge()
				time.Sleep(100 * time.Microsecond)
			}
		}
	}()

	const callers, rounds = 8, 25
	errCh := make(chan error, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < rounds; j++ {
				p, err := pc.GetPlayer(t.Context(), key)
				if err != nil {
					errCh <- fmt.Errorf("round %d: %w", j, err)
					return
				}
				if p == nil || p.ID != key.PlayerID {
					errCh <- fmt.Errorf("round %d: got %+v, want player %s", j, p, key.PlayerID)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	storm.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err) // any escape of errStaleFlight (or divergence) lands here
	}
}
