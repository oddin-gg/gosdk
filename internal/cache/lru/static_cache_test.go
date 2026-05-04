package lru

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

func TestStaticCache_All_LoadOnce(t *testing.T) {
	var calls atomic.Int32
	loader := func(ctx context.Context, locale string) (map[int]string, error) {
		calls.Add(1)
		return map[int]string{1: locale + "-one", 2: locale + "-two"}, nil
	}
	c := NewStaticCache[int, string, string](loader)

	for i := 0; i < 5; i++ {
		got, err := c.All(context.Background(), "en")
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		if got[1] != "en-one" {
			t.Fatalf("got[1] = %q", got[1])
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("loader calls = %d, want 1", calls.Load())
	}
}

func TestStaticCache_PerLocaleIsolation(t *testing.T) {
	var calls atomic.Int32
	loader := func(ctx context.Context, locale string) (map[int]string, error) {
		calls.Add(1)
		return map[int]string{1: locale}, nil
	}
	c := NewStaticCache[int, string, string](loader)

	if _, err := c.All(context.Background(), "en"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.All(context.Background(), "ru"); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("calls = %d, want 2 (one per locale)", got)
	}
}

func TestStaticCache_Get(t *testing.T) {
	loader := func(ctx context.Context, locale string) (map[int]string, error) {
		return map[int]string{42: "answer"}, nil
	}
	c := NewStaticCache[int, string, string](loader)

	v, ok, err := c.Get(context.Background(), "en", 42)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || v != "answer" {
		t.Fatalf("Get(42) = %q, ok=%v", v, ok)
	}
	_, ok, err = c.Get(context.Background(), "en", 99)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("Get(99) should report not-found")
	}
}

// Loader failures must NOT poison the cache; the next call retries.
// This is the sync.Once footgun the rewrite explicitly avoids.
func TestStaticCache_ErrorDoesNotPoison(t *testing.T) {
	var calls atomic.Int32
	failing := true
	loader := func(ctx context.Context, locale string) (map[int]string, error) {
		calls.Add(1)
		if failing {
			return nil, errors.New("boom")
		}
		return map[int]string{1: "ok"}, nil
	}
	c := NewStaticCache[int, string, string](loader)

	if _, err := c.All(context.Background(), "en"); err == nil {
		t.Fatal("expected error on first call")
	}
	failing = false
	got, err := c.All(context.Background(), "en")
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got[1] != "ok" {
		t.Fatalf("retry value = %q", got[1])
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2 (fail + succeed)", calls.Load())
	}
}

func TestStaticCache_Clear(t *testing.T) {
	var calls atomic.Int32
	loader := func(ctx context.Context, locale string) (map[int]string, error) {
		calls.Add(1)
		return map[int]string{1: "v"}, nil
	}
	c := NewStaticCache[int, string, string](loader)

	_, _ = c.All(context.Background(), "en")
	c.Clear("en")
	_, _ = c.All(context.Background(), "en")
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2 (load, clear, reload)", calls.Load())
	}
}

func TestStaticCache_Purge(t *testing.T) {
	var calls atomic.Int32
	loader := func(ctx context.Context, locale string) (map[int]string, error) {
		calls.Add(1)
		return map[int]string{1: "v"}, nil
	}
	c := NewStaticCache[int, string, string](loader)

	_, _ = c.All(context.Background(), "en")
	_, _ = c.All(context.Background(), "ru")
	c.Purge()
	_, _ = c.All(context.Background(), "en")
	_, _ = c.All(context.Background(), "ru")
	if calls.Load() != 4 {
		t.Fatalf("calls = %d, want 4 (en, ru, en-after-purge, ru-after-purge)", calls.Load())
	}
}

func TestStaticCache_ContextPropagation(t *testing.T) {
	loader := func(ctx context.Context, locale string) (map[int]string, error) {
		return nil, ctx.Err()
	}
	c := NewStaticCache[int, string, string](loader)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.All(ctx, "en")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
