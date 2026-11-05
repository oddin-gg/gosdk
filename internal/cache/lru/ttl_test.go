package lru

import (
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// TestNewTTL_NoBackgroundGoroutine is the regression for the expirable-
// LRU leak: hashicorp's expirable.LRU (v2.0.7) starts a cleanup
// goroutine per cache that can never exit (its done channel is never
// closed; Close ships commented out), so every constructed Client
// leaked seven goroutines and retained the cache memory forever —
// repeated New/Close grew both linearly and falsified Client.Close's
// full-join guarantee. The replacement must not create ANY goroutine.
func TestNewTTL_NoBackgroundGoroutine(t *testing.T) {
	before := runtime.NumGoroutine()
	caches := make([]*TTL[int, int], 0, 64)
	for i := 0; i < 64; i++ {
		c := NewTTL[int, int](8, nil, time.Minute)
		c.Add(i, i)
		caches = append(caches, c)
	}
	runtime.GC()
	after := runtime.NumGoroutine()
	if after > before {
		t.Fatalf("goroutines grew %d -> %d after creating 64 TTL caches; the cache must be janitor-free", before, after)
	}
	if v, ok := caches[7].Get(7); !ok || v != 7 {
		t.Fatalf("Get(7) = %v, %v", v, ok)
	}
}

// TestTTL_Semantics covers the surface the SDK uses: lazy expiry on
// Get/Peek/Keys, size-bound eviction with callback, Remove/Purge
// callbacks, and ttl<=0 disabling expiry.
func TestTTL_Semantics(t *testing.T) {
	evicted := map[int]string{}
	c := NewTTL[int, string](2, func(k int, v string) { evicted[k] = v }, 60*time.Millisecond)

	c.Add(1, "a")
	c.Add(2, "b")
	if v, ok := c.Get(1); !ok || v != "a" {
		t.Fatalf("Get(1) = %q, %v", v, ok)
	}
	// Size bound: adding a third evicts the LRU entry (2 — since 1 was
	// just touched) and fires the callback.
	c.Add(3, "c")
	if _, ok := c.Get(2); ok {
		t.Fatal("entry 2 survived past the size bound")
	}
	if evicted[2] != "b" {
		t.Fatalf("evict callback not fired for displaced entry: %v", evicted)
	}

	// Expiry: entries become misses after the TTL and fire the callback
	// on discovery. Sleeps carry wide margins (>= 20ms beyond the TTL
	// boundary in both directions) so loaded-CI scheduling jitter can't
	// flip the outcome.
	time.Sleep(90 * time.Millisecond)
	if _, ok := c.Get(1); ok {
		t.Fatal("expired entry 1 returned")
	}
	if _, ok := c.Peek(3); ok {
		t.Fatal("expired entry 3 returned from Peek")
	}
	if evicted[1] != "a" || evicted[3] != "c" {
		t.Fatalf("expiry evictions missing callbacks: %v", evicted)
	}
	if got := len(c.Keys()); got != 0 {
		t.Fatalf("Keys() = %d live entries, want 0", got)
	}

	// Refresh-on-Add: re-adding restarts the TTL.
	c.Add(4, "d")
	time.Sleep(40 * time.Millisecond)
	c.Add(4, "d2")
	time.Sleep(40 * time.Millisecond) // 80ms since first add, 40ms since refresh
	if v, ok := c.Get(4); !ok || v != "d2" {
		t.Fatalf("refreshed entry expired early: %q, %v", v, ok)
	}

	// Remove + Purge fire callbacks.
	c.Add(5, "e")
	if !c.Remove(5) {
		t.Fatal("Remove(5) = false")
	}
	if evicted[5] != "e" {
		t.Fatal("Remove did not fire evict callback")
	}
	c.Add(6, "f")
	c.Purge()
	if evicted[6] != "f" {
		t.Fatal("Purge did not fire evict callback")
	}
	if c.Len() != 0 {
		t.Fatalf("Len after Purge = %d", c.Len())
	}

	// ttl <= 0: no expiry.
	forever := NewTTL[int, string](2, nil, 0)
	forever.Add(1, "x")
	time.Sleep(5 * time.Millisecond)
	if v, ok := forever.Get(1); !ok || v != "x" {
		t.Fatalf("no-TTL cache expired an entry: %q, %v", v, ok)
	}
}

// fakeNow is a settable clock for the TTL tests.
type fakeNow struct{ t time.Time }

func (f *fakeNow) Now() time.Time          { return f.t }
func (f *fakeNow) Advance(d time.Duration) { f.t = f.t.Add(d) }

// TestTTL_AddShedsExpiredEntriesWithoutReads: expiry used to be purely
// lazy, so entries that were never read again stayed until the size cap
// pushed them out — an idle cache that kept seeing new keys only ever
// grew. Entries past their TTL must be gone after later Adds, with no
// read touching them, and the work per Add stays bounded.
func TestTTL_AddShedsExpiredEntriesWithoutReads(t *testing.T) {
	clk := &fakeNow{t: time.Unix(1_000_000, 0)}
	evicted := map[int]bool{}
	c := NewTTL[int, int](1000, func(k int, _ int) { evicted[k] = true }, time.Hour)
	c.now = clk.Now

	for i := range 20 {
		c.Add(i, i)
	}
	if c.Len() != 20 {
		t.Fatalf("Len = %d, want 20", c.Len())
	}

	clk.Advance(2 * time.Hour) // all 20 are past their TTL, none was read

	// One Add sheds at most expirySweepPerAdd expired entries (bounded
	// work) and inserts the new one.
	c.Add(100, 100)
	if got, want := c.Len(), 20-expirySweepPerAdd+1; got != want {
		t.Fatalf("Len after one Add = %d, want %d (bounded sweep)", got, want)
	}
	// The oldest go first, and the evict callback fires for each.
	for i := range expirySweepPerAdd {
		if !evicted[i] {
			t.Errorf("entry %d was not shed / evict callback not fired", i)
		}
	}
	if evicted[expirySweepPerAdd] {
		t.Errorf("entry %d shed on the first Add; the sweep must stay bounded", expirySweepPerAdd)
	}

	// Later Adds finish the job without any reads.
	for i := 101; i < 110; i++ {
		c.Add(i, i)
	}
	for i := range 20 {
		if !evicted[i] {
			t.Errorf("expired entry %d still held after later Adds", i)
		}
	}
	// 10 live entries (100..109) remain, all readable.
	if got := c.Len(); got != 10 {
		t.Fatalf("Len = %d, want 10 live entries", got)
	}
	if v, ok := c.Get(105); !ok || v != 105 {
		t.Fatalf("live entry lost: %v, %v", v, ok)
	}
}

// TestTTL_AddSweepStopsAtFirstLiveEntry: the sweep walks from the least
// recently used end and never drops a live entry.
func TestTTL_AddSweepStopsAtFirstLiveEntry(t *testing.T) {
	clk := &fakeNow{t: time.Unix(1_000_000, 0)}
	c := NewTTL[int, int](1000, nil, time.Hour)
	c.now = clk.Now

	c.Add(1, 1) // expires at +1h
	clk.Advance(50 * time.Minute)
	c.Add(2, 2)                   // expires at +1h50m
	clk.Advance(20 * time.Minute) // 1 expired, 2 live

	c.Add(3, 3)
	if _, ok := c.Peek(1); ok {
		t.Fatal("expired entry 1 survived the Add")
	}
	if v, ok := c.Peek(2); !ok || v != 2 {
		t.Fatalf("live entry 2 was dropped: %v, %v", v, ok)
	}
	if c.Len() != 2 {
		t.Fatalf("Len = %d, want 2", c.Len())
	}
}

// TestTTL_NoTTLNeverSweeps: ttl <= 0 disables expiry, sweep included.
func TestTTL_NoTTLNeverSweeps(t *testing.T) {
	clk := &fakeNow{t: time.Unix(1_000_000, 0)}
	c := NewTTL[int, int](1000, nil, 0)
	c.now = clk.Now
	for i := range 10 {
		c.Add(i, i)
	}
	clk.Advance(1000 * time.Hour)
	c.Add(99, 99)
	if c.Len() != 11 {
		t.Fatalf("Len = %d, want 11 (no expiry without a TTL)", c.Len())
	}
}

// TestEventCache_AddShedsExpiredEntriesWithoutReads: the per-event caches
// are built on TTL, so loads of new keys shed expired events that nobody
// reads any more.
func TestEventCache_AddShedsExpiredEntriesWithoutReads(t *testing.T) {
	clk := &fakeNow{t: time.Unix(1_000_000, 0)}
	var calls atomic.Int32
	c := NewEventCache[string, string, *localizedFoo](Config{Size: 1000, TTL: time.Hour}, recordingLoader(&calls, false))
	c.lru.now = clk.Now

	for i := range 12 {
		if _, _, err := c.Get(t.Context(), "old-"+string(rune('a'+i)), []string{"en"}); err != nil {
			t.Fatal(err)
		}
	}
	if c.Len() != 12 {
		t.Fatalf("Len = %d, want 12", c.Len())
	}
	clk.Advance(2 * time.Hour)

	// New keys only; the old ones are never read again.
	for i := range 4 {
		if _, _, err := c.Get(t.Context(), "new-"+string(rune('a'+i)), []string{"en"}); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 12 {
		if _, ok := c.lru.lru.Peek("old-" + string(rune('a'+i))); ok {
			// Peek on the inner simplelru does not apply expiry: any
			// entry still present was not shed.
			t.Fatalf("expired event old-%c still held after later loads", 'a'+i)
		}
	}
	if c.Len() != 4 {
		t.Fatalf("Len = %d, want 4 live entries", c.Len())
	}
}
