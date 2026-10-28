package cache

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
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

// TestLocalizedStaticDataCache_PrimaryLocaleIsRequestedNotDefault pins
// Description to the CALL's primary locale (locales[0]). Pre-fix it was
// filled from the configured default locale, so LocalizedItem(ctx, id,
// [de]) with default en populated Descriptions[de] but returned
// Description == None — the documented primary-locale value silently
// missing for every non-default-locale consumer.
func TestLocalizedStaticDataCache_PrimaryLocaleIsRequestedNotDefault(t *testing.T) {
	fetcher := func(ctx context.Context, locale types.Locale) ([]types.StaticData, error) {
		return []types.StaticData{{ID: 1, Description: types.Some("desc-" + string(locale))}}, nil
	}
	c := newLocalizedStaticDataCache(t.Context(), &minimalCfg{}, log.New(nil), nil, fetcher)
	defer c.Close()

	// Non-default primary: Description must carry the de value.
	got, err := c.LocalizedItem(t.Context(), 1, []types.Locale{types.DeLocale})
	if err != nil {
		t.Fatalf("LocalizedItem(de): %v", err)
	}
	if v, ok := got.Description.Get(); !ok || v != "desc-de" {
		t.Fatalf("Description = %v, want Some(desc-de) (primary must be locales[0], not the configured default)", got.Description)
	}

	// Multi-locale call: still the FIRST locale, not the default.
	got, err = c.LocalizedItem(t.Context(), 1, []types.Locale{types.RuLocale, types.EnLocale})
	if err != nil {
		t.Fatalf("LocalizedItem(ru,en): %v", err)
	}
	if v, ok := got.Description.Get(); !ok || v != "desc-ru" {
		t.Fatalf("Description = %v, want Some(desc-ru)", got.Description)
	}

	// Default-locale calls keep their existing behavior.
	got, err = c.LocalizedItem(t.Context(), 1, []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("LocalizedItem(en): %v", err)
	}
	if v, ok := got.Description.Get(); !ok || v != "desc-en" {
		t.Fatalf("Description = %v, want Some(desc-en)", got.Description)
	}
}

// TestLocalizedStaticDataCache_LocaleCoverageValidated pins per-id
// requested-locale coverage. Pre-fix an id was "found" when it existed
// in ANY loaded locale: a request for [en, de] where the de catalog
// omits the id succeeded with only English populated — a silently
// partial localized value, inconsistent with the market/sport caches'
// typed Err*LocaleIncomplete contract. The partial value must still be
// returned WITH the error so tolerant callers can attach what exists.
func TestLocalizedStaticDataCache_LocaleCoverageValidated(t *testing.T) {
	fetcher := func(ctx context.Context, locale types.Locale) ([]types.StaticData, error) {
		if locale == types.EnLocale {
			return []types.StaticData{{ID: 1, Description: types.Some("en-desc")}}, nil
		}
		return nil, nil // the de catalog omits id 1 entirely
	}
	c := newLocalizedStaticDataCache(t.Context(), &minimalCfg{}, log.New(nil), nil, fetcher)
	defer c.Close()

	got, err := c.LocalizedItem(t.Context(), 1, []types.Locale{types.EnLocale, types.DeLocale})
	if !errors.Is(err, ErrStaticDataLocaleIncomplete) {
		t.Fatalf("err = %v, want ErrStaticDataLocaleIncomplete", err)
	}
	if errors.Is(err, ErrItemNotFoundInCache) {
		t.Fatalf("err = %v, must not classify as not-found (the id exists in en)", err)
	}
	// The partial value accompanies the error.
	if v, ok := got.Descriptions[types.EnLocale]; !ok || v != "en-desc" {
		t.Fatalf("Descriptions = %v, want en-desc present", got.Descriptions)
	}
	if _, ok := got.Descriptions[types.DeLocale]; ok {
		t.Fatalf("Descriptions = %v, de must be absent", got.Descriptions)
	}

	// Fully covered requests keep returning cleanly.
	if _, err := c.LocalizedItem(t.Context(), 1, []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("LocalizedItem(en): %v", err)
	}

	// An id absent from EVERY loaded locale stays a not-found, not an
	// incomplete.
	_, err = c.LocalizedItem(t.Context(), 99, []types.Locale{types.EnLocale, types.DeLocale})
	if !errors.Is(err, ErrItemNotFoundInCache) {
		t.Fatalf("err = %v, want ErrItemNotFoundInCache for unknown id", err)
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

// refreshTestCache builds a cache whose refresh timer is not started, so a
// test drives timerTick itself.
func refreshTestCache(fetcher func(ctx context.Context, locale types.Locale) ([]types.StaticData, error)) *LocalizedStaticDataCache {
	lifeCtx, cancel := context.WithCancel(context.Background())
	return &LocalizedStaticDataCache{
		oddsFeedConfiguration: &minimalCfg{},
		fetcher:               fetcher,
		locales:               []types.Locale{types.EnLocale},
		internalCache:         make(map[int]map[types.Locale]string),
		loadedLocales:         make(map[types.Locale]struct{}),
		lifeCtx:               lifeCtx,
		closeFn:               cancel,
		logger:                log.New(nil),
	}
}

// TestLocalizedStaticDataCache_EmptyRefreshKeepsCachedDescriptions: an OK
// answer with an empty list decodes without error, so the periodic refresh
// used to treat it as "every id was removed upstream" and delete every
// description of the locale; with the locale still marked loaded, every
// lookup then failed until the next tick. An empty answer must not prune.
// A non-empty one still does, so the removal of a single id keeps working.
func TestLocalizedStaticDataCache_EmptyRefreshKeepsCachedDescriptions(t *testing.T) {
	var answer atomic.Pointer[[]types.StaticData]
	set := func(rows ...types.StaticData) { answer.Store(&rows) }
	set(
		types.StaticData{ID: 1, Description: types.Some("one")},
		types.StaticData{ID: 2, Description: types.Some("two")},
	)
	c := refreshTestCache(func(context.Context, types.Locale) ([]types.StaticData, error) {
		return *answer.Load(), nil
	})
	defer c.Close()
	var logged bytes.Buffer
	c.logger = log.New(slog.New(slog.NewTextHandler(&logged, nil)))

	locales := []types.Locale{types.EnLocale}
	if _, err := c.LocalizedItem(t.Context(), 1, locales); err != nil {
		t.Fatalf("initial load: %v", err)
	}

	set() // the API answers OK with no rows
	c.timerTick(t.Context())
	for id, want := range map[int]string{1: "one", 2: "two"} {
		item, err := c.LocalizedItem(t.Context(), id, locales)
		if err != nil {
			t.Fatalf("id %d after an empty refresh: %v", id, err)
		}
		if got, _ := item.LocalizedDescription(types.EnLocale).Get(); got != want {
			t.Fatalf("id %d description = %q after an empty refresh, want %q", id, got, want)
		}
	}

	if !strings.Contains(logged.String(), "empty static data response") {
		t.Fatalf("the skipped refresh must be logged, got %q", logged.String())
	}

	set(types.StaticData{ID: 1, Description: types.Some("one v2")}) // id 2 removed upstream
	c.timerTick(t.Context())
	if item, err := c.LocalizedItem(t.Context(), 1, locales); err != nil {
		t.Fatalf("id 1 after a non-empty refresh: %v", err)
	} else if got, _ := item.LocalizedDescription(types.EnLocale).Get(); got != "one v2" {
		t.Fatalf("id 1 description = %q, want the refreshed %q", got, "one v2")
	}
	if _, err := c.LocalizedItem(t.Context(), 2, locales); err == nil {
		t.Fatal("id 2 was removed upstream by a non-empty refresh and must be pruned")
	}
}
