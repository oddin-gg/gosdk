package lru

import (
	"runtime"
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
