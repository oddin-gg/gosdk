package lru

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// TestEventCache_ClearDuringLoad_NotResurrected is the regression for
// the clear-vs-in-flight-load race: a Clear landing while a load is in
// flight used to be silently undone — the loader re-Added its
// (pre-clear) merge with a fresh TTL, defeating FixtureChange
// auto-invalidation and the public Clear* methods.
func TestEventCache_ClearDuringLoad_NotResurrected(t *testing.T) {
	var calls atomic.Int32
	gate := make(chan struct{})
	loading := make(chan struct{})

	loader := func(ctx context.Context, key string, locales []string, existing *localizedFoo, has bool) (*localizedFoo, error) {
		n := calls.Add(1)
		if n == 1 {
			close(loading) // signal: first load entered
			<-gate         // hold the load open until the Clear landed
		}
		entry := existing
		if !has {
			entry = &localizedFoo{id: key, name: make(map[string]string)}
		}
		entry.mu.Lock()
		for _, l := range locales {
			entry.name[l] = "gen" + string('0'+n) + "-" + l
		}
		entry.mu.Unlock()
		return entry, nil
	}
	c := NewEventCache[string, string, *localizedFoo](Config{}, loader)

	type result struct {
		v   *localizedFoo
		err error
	}
	resCh := make(chan result, 1)
	go func() {
		v, _, err := c.Get(context.Background(), "match-1", []string{"en"})
		resCh <- result{v, err}
	}()

	<-loading
	c.Clear("match-1") // invalidate while the load is in flight
	close(gate)        // let the load finish

	r := <-resCh
	if r.err != nil {
		t.Fatalf("Get: %v", r.err)
	}
	// The caller must have received data from a FRESH (post-clear) load,
	// not the invalidated first-generation merge.
	if got := r.v.name["en"]; got != "gen2-en" {
		t.Fatalf("caller got %q, want post-clear gen2-en", got)
	}
	if calls.Load() != 2 {
		t.Fatalf("loader calls = %d, want 2 (retry after discarded merge)", calls.Load())
	}
	// And whatever is cached now must be the post-clear generation.
	if v, ok := c.Peek("match-1"); ok && v.name["en"] != "gen2-en" {
		t.Fatalf("cache holds %q — pre-clear entry was resurrected", v.name["en"])
	}
}

// TestEventCache_GetAfterClear_DoesNotJoinStaleFlight pins the
// sf.Forget half of the fix: a Get arriving AFTER a Clear must not
// coalesce onto the pre-clear in-flight load and receive the entry the
// Clear just discarded.
func TestEventCache_GetAfterClear_DoesNotJoinStaleFlight(t *testing.T) {
	var calls atomic.Int32
	gate := make(chan struct{})
	loading := make(chan struct{})

	loader := func(ctx context.Context, key string, locales []string, existing *localizedFoo, has bool) (*localizedFoo, error) {
		n := calls.Add(1)
		if n == 1 {
			close(loading)
			<-gate
		}
		entry := existing
		if !has {
			entry = &localizedFoo{id: key, name: make(map[string]string)}
		}
		entry.mu.Lock()
		for _, l := range locales {
			entry.name[l] = "gen" + string('0'+n)
		}
		entry.mu.Unlock()
		return entry, nil
	}
	c := NewEventCache[string, string, *localizedFoo](Config{}, loader)

	go func() {
		_, _, _ = c.Get(context.Background(), "k", []string{"en"})
	}()
	<-loading
	c.Clear("k")

	// This Get starts after the Clear; it must run its own load rather
	// than joining the (Forgotten) pre-clear flight. Release the gate
	// from a timer so both flights can complete regardless of ordering.
	time.AfterFunc(50*time.Millisecond, func() { close(gate) })
	v, _, err := c.Get(context.Background(), "k", []string{"en"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if v.name["en"] == "gen1" {
		t.Fatal("post-clear Get received the pre-clear flight's result")
	}
}

// TestEventCache_ColdKeyEmptyLocales_NoPanic is the regression for the
// typed-nil escape: Get on an uncached key with an empty locale slice
// used to box a nil *T into the flight result — `r.Val == nil` is false
// for a typed nil — and panic on the coverage re-check's Locales()
// call. It must surface ErrEntryNotPopulated instead.
func TestEventCache_ColdKeyEmptyLocales_NoPanic(t *testing.T) {
	var calls atomic.Int32
	c := NewEventCache[string, string, *localizedFoo](Config{}, recordingLoader(&calls, false))

	_, ok, err := c.Get(t.Context(), "cold", nil)
	if ok {
		t.Fatal("expected ok=false for cold key with no locales")
	}
	if !errors.Is(err, ErrEntryNotPopulated) {
		t.Fatalf("err = %v, want ErrEntryNotPopulated", err)
	}

	// Warm key + empty locales keeps returning the cached entry.
	if _, _, err := c.Get(t.Context(), "warm", []string{"en"}); err != nil {
		t.Fatalf("warm load: %v", err)
	}
	v, ok, err := c.Get(t.Context(), "warm", nil)
	if err != nil || !ok {
		t.Fatalf("warm empty-locales Get: ok=%v err=%v", ok, err)
	}
	if v == nil {
		t.Fatal("warm empty-locales Get returned nil entry")
	}
}

// A load merges its locales onto the entry that was cached when it started.
// If that entry expires or is evicted while the fetch runs, the merge would
// give its stale locales a fresh lifetime, so the load is discarded and
// retried from the cache as it is now.
func TestEventCache_MergeBaseGoneDuringLoad_NotResurrected(t *testing.T) {
	for name, drop := range map[string]func(c *EventCache[string, string, *localizedFoo], now *atomic.Int64){
		"evicted": func(c *EventCache[string, string, *localizedFoo], _ *atomic.Int64) {
			c.lru.Remove("match-1")
		},
		"expired": func(_ *EventCache[string, string, *localizedFoo], now *atomic.Int64) {
			now.Add(int64(2 * time.Hour))
		},
	} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			gate := make(chan struct{})
			loading := make(chan struct{})
			// The loader merges onto a clone of the cached entry, as the
			// production loaders do, so the clone carries the old locales.
			loader := func(ctx context.Context, key string, locales []string, existing *localizedFoo, has bool) (*localizedFoo, error) {
				n := calls.Add(1)
				entry := &localizedFoo{id: key, name: make(map[string]string)}
				if has {
					existing.mu.Lock()
					for l, v := range existing.name {
						entry.name[l] = v
					}
					existing.mu.Unlock()
				}
				if n == 2 {
					close(loading)
					<-gate
				}
				for _, l := range locales {
					entry.name[l] = "gen" + string('0'+n) + "-" + l
				}
				return entry, nil
			}
			c := NewEventCache[string, string, *localizedFoo](Config{TTL: time.Hour}, loader)
			start := time.Now()
			var now atomic.Int64
			c.lru.now = func() time.Time { return start.Add(time.Duration(now.Load())) }

			if _, _, err := c.Get(context.Background(), "match-1", []string{"en"}); err != nil {
				t.Fatalf("seed Get: %v", err)
			}
			type result struct {
				v   *localizedFoo
				err error
			}
			resCh := make(chan result, 1)
			go func() {
				v, _, err := c.Get(context.Background(), "match-1", []string{"cs"})
				resCh <- result{v, err}
			}()
			<-loading
			drop(c, &now)
			close(gate)

			r := <-resCh
			if r.err != nil {
				t.Fatalf("Get: %v", r.err)
			}
			if got := r.v.name["cs"]; got != "gen3-cs" {
				t.Fatalf("caller got %q, want the fresh gen3-cs", got)
			}
			if calls.Load() != 3 {
				t.Fatalf("loader calls = %d, want 3 (the discarded merge is retried)", calls.Load())
			}
			cur, ok := c.Peek("match-1")
			if !ok {
				t.Fatal("the retried load was not admitted")
			}
			if _, stale := cur.name["en"]; stale {
				t.Fatalf("the cache holds %v: the stale en locale was re-admitted", cur.name)
			}
		})
	}
}
