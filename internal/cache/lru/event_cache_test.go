package lru

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// localizedFoo is a minimal LocalizedEntry implementation for testing.
type localizedFoo struct {
	id   string
	mu   sync.Mutex
	name map[string]string // locale → value
}

func (f *localizedFoo) Locales() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.name))
	for l := range f.name {
		out = append(out, l)
	}
	return out
}

// helper: build a loader that records each invocation and populates locales.
func recordingLoader(invocations *atomic.Int32, fail bool) Loader[string, string, *localizedFoo] {
	return func(ctx context.Context, key string, locales []string, existing *localizedFoo, has bool) (*localizedFoo, error) {
		invocations.Add(1)
		if fail {
			return nil, errors.New("boom")
		}
		var entry *localizedFoo
		if has {
			entry = existing
		} else {
			entry = &localizedFoo{id: key, name: make(map[string]string)}
		}
		entry.mu.Lock()
		for _, l := range locales {
			entry.name[l] = "name-" + l + "-" + key
		}
		entry.mu.Unlock()
		return entry, nil
	}
}

func TestEventCache_Get_FreshLoad(t *testing.T) {
	var calls atomic.Int32
	c := NewEventCache[string, string, *localizedFoo](Config{}, recordingLoader(&calls, false))

	v, ok, err := c.Get(t.Context(), "match-1", []string{"en"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true")
	}
	if v.name["en"] != "name-en-match-1" {
		t.Fatalf("name = %q", v.name["en"])
	}
	if calls.Load() != 1 {
		t.Fatalf("loader calls = %d, want 1", calls.Load())
	}
}

func TestEventCache_Get_HitsCache(t *testing.T) {
	var calls atomic.Int32
	c := NewEventCache[string, string, *localizedFoo](Config{}, recordingLoader(&calls, false))

	for i := 0; i < 5; i++ {
		_, _, err := c.Get(t.Context(), "match-1", []string{"en"})
		if err != nil {
			t.Fatalf("Get %d: %v", i, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("loader calls = %d, want 1 (subsequent reads should hit cache)", calls.Load())
	}
}

// Concurrent Get for the same key should call the loader exactly once
// (singleflight), with all callers seeing the populated entry.
func TestEventCache_SingleflightDedup(t *testing.T) {
	var calls atomic.Int32
	// Slow loader so multiple goroutines pile up.
	loader := func(ctx context.Context, key string, locales []string, existing *localizedFoo, has bool) (*localizedFoo, error) {
		calls.Add(1)
		time.Sleep(50 * time.Millisecond)
		entry := &localizedFoo{id: key, name: map[string]string{"en": "name"}}
		return entry, nil
	}
	c := NewEventCache[string, string, *localizedFoo](Config{}, loader)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := c.Get(t.Context(), "match-1", []string{"en"})
			if err != nil {
				t.Errorf("Get: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("loader calls = %d, want 1 under singleflight", got)
	}
}

// Asking for additional locales after the first call should fetch only the
// missing ones; existing locales are not re-fetched.
func TestEventCache_FetchOnlyMissingLocales(t *testing.T) {
	var requestedLocales [][]string
	loader := func(ctx context.Context, key string, locales []string, existing *localizedFoo, has bool) (*localizedFoo, error) {
		recv := make([]string, len(locales))
		copy(recv, locales)
		requestedLocales = append(requestedLocales, recv)
		var entry *localizedFoo
		if has {
			entry = existing
		} else {
			entry = &localizedFoo{id: key, name: map[string]string{}}
		}
		for _, l := range locales {
			entry.name[l] = "n-" + l
		}
		return entry, nil
	}
	c := NewEventCache[string, string, *localizedFoo](Config{}, loader)

	if _, _, err := c.Get(t.Context(), "k", []string{"en"}); err != nil {
		t.Fatalf("Get en: %v", err)
	}
	if _, _, err := c.Get(t.Context(), "k", []string{"en", "ru"}); err != nil {
		t.Fatalf("Get en+ru: %v", err)
	}
	if _, _, err := c.Get(t.Context(), "k", []string{"de"}); err != nil {
		t.Fatalf("Get de: %v", err)
	}

	if len(requestedLocales) != 3 {
		t.Fatalf("loader invocations = %d, want 3", len(requestedLocales))
	}
	if got, want := requestedLocales[0], []string{"en"}; !equalStrSlice(got, want) {
		t.Errorf("call 1 locales = %v, want %v", got, want)
	}
	if got, want := requestedLocales[1], []string{"ru"}; !equalStrSlice(got, want) {
		t.Errorf("call 2 locales = %v, want %v", got, want)
	}
	if got, want := requestedLocales[2], []string{"de"}; !equalStrSlice(got, want) {
		t.Errorf("call 3 locales = %v, want %v", got, want)
	}
}

func TestEventCache_LoaderError_NoPoison(t *testing.T) {
	var calls atomic.Int32
	failing := true
	loader := func(ctx context.Context, key string, locales []string, existing *localizedFoo, has bool) (*localizedFoo, error) {
		calls.Add(1)
		if failing {
			return nil, errors.New("boom")
		}
		entry := &localizedFoo{id: key, name: map[string]string{"en": "ok"}}
		return entry, nil
	}
	c := NewEventCache[string, string, *localizedFoo](Config{}, loader)

	if _, _, err := c.Get(t.Context(), "k", []string{"en"}); err == nil {
		t.Fatal("expected error on first call")
	}
	failing = false
	v, ok, err := c.Get(t.Context(), "k", []string{"en"})
	if err != nil {
		t.Fatalf("Get retry: %v", err)
	}
	if !ok || v.name["en"] != "ok" {
		t.Fatalf("retry should succeed; got ok=%v entry=%v", ok, v)
	}
	if calls.Load() != 2 {
		t.Fatalf("loader calls = %d, want 2 (failed + succeeded retry)", calls.Load())
	}
}

func TestEventCache_TTLExpiry(t *testing.T) {
	var calls atomic.Int32
	c := NewEventCache[string, string, *localizedFoo](
		Config{TTL: 50 * time.Millisecond},
		recordingLoader(&calls, false),
	)
	if _, _, err := c.Get(t.Context(), "k", []string{"en"}); err != nil {
		t.Fatalf("Get 1: %v", err)
	}
	time.Sleep(120 * time.Millisecond) // > TTL
	if _, _, err := c.Get(t.Context(), "k", []string{"en"}); err != nil {
		t.Fatalf("Get 2: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("loader calls = %d, want 2 (entry expired)", got)
	}
}

func TestEventCache_LRUEviction(t *testing.T) {
	var calls atomic.Int32
	c := NewEventCache[string, string, *localizedFoo](
		Config{Size: 2},
		recordingLoader(&calls, false),
	)
	for _, k := range []string{"a", "b", "c"} { // 3rd entry evicts oldest
		if _, _, err := c.Get(t.Context(), k, []string{"en"}); err != nil {
			t.Fatalf("Get %s: %v", k, err)
		}
	}
	// "a" should be evicted; reading it again triggers loader.
	if _, _, err := c.Get(t.Context(), "a", []string{"en"}); err != nil {
		t.Fatalf("Get a (re-load): %v", err)
	}
	if got := calls.Load(); got != 4 {
		t.Fatalf("loader calls = %d, want 4 (a, b, c, a-reload)", got)
	}
}

func TestEventCache_Clear(t *testing.T) {
	var calls atomic.Int32
	c := NewEventCache[string, string, *localizedFoo](Config{}, recordingLoader(&calls, false))
	_, _, _ = c.Get(t.Context(), "k", []string{"en"})
	c.Clear("k")
	_, _, _ = c.Get(t.Context(), "k", []string{"en"})
	if got := calls.Load(); got != 2 {
		t.Fatalf("loader calls = %d, want 2 after Clear+Get", got)
	}
}

func TestEventCache_Purge(t *testing.T) {
	c := NewEventCache[string, string, *localizedFoo](
		Config{},
		recordingLoader(new(atomic.Int32), false),
	)
	for _, k := range []string{"a", "b", "c"} {
		_, _, _ = c.Get(t.Context(), k, []string{"en"})
	}
	if c.Len() != 3 {
		t.Fatalf("Len = %d, want 3", c.Len())
	}
	c.Purge()
	if c.Len() != 0 {
		t.Fatalf("Len after Purge = %d, want 0", c.Len())
	}
}

func TestEventCache_ContextCancellation(t *testing.T) {
	loader := func(ctx context.Context, key string, locales []string, existing *localizedFoo, has bool) (*localizedFoo, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
			t.Error("loader did not observe ctx cancellation")
			return &localizedFoo{}, nil
		}
	}
	c := NewEventCache[string, string, *localizedFoo](Config{}, loader)

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, _, err := c.Get(ctx, "k", []string{"en"})
	if err == nil {
		t.Fatal("expected ctx cancellation error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func equalStrSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
