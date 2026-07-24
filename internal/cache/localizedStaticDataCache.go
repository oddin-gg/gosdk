package cache

import (
	"context"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/oddin-gg/gosdk/internal/cache/lru"
	"github.com/oddin-gg/gosdk/internal/config"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

const (
	initialDelay = 24 * time.Hour
	tickPeriod   = 24 * time.Hour
)

// LocalizedStaticDataCache caches static-catalog data per locale.
//
// Phase 6 reshape: returns types.LocalizedStaticData value structs
// directly (the previous wrapper impl is gone).
//
// Concurrency (v2.24): readers acquire mux only briefly (RLock for
// hit/miss check, Lock for cache writes). Loads run OUTSIDE the
// mutex, deduplicated by per-locale singleflight, so a slow upstream
// no longer serializes every reader of the cache. loadedLocales is
// tracked explicitly (rather than inferred from internalCache
// contents) so an empty-result locale isn't re-fetched on every call.
type LocalizedStaticDataCache struct {
	oddsFeedConfiguration config.Config
	fetcher               func(ctx context.Context, locale types.Locale) ([]types.StaticData, error)
	locales               []types.Locale
	internalCache         map[int]map[types.Locale]string
	loadedLocales         map[types.Locale]struct{}
	lifeCtx               context.Context
	closeFn               context.CancelFunc
	closeOnce             sync.Once
	// timerDone closes when the periodic-refresh goroutine exits;
	// Close joins on it so shutdown is deterministic.
	timerDone chan struct{}
	// refreshInitialDelay / refreshTickPeriod are initialDelay /
	// tickPeriod, held as fields so scheduling tests can compress the
	// clock instead of waiting real days.
	refreshInitialDelay time.Duration
	refreshTickPeriod   time.Duration
	logger              *log.Logger
	mux                 sync.RWMutex
	sf                  singleflight.Group
}

// LocalizedItem returns a populated LocalizedStaticData for the given
// id, fetching missing locales as needed. Loads run outside the mutex
// (deduplicated via per-locale singleflight) so a slow upstream
// doesn't block other readers.
func (l *LocalizedStaticDataCache) LocalizedItem(ctx context.Context, id int, locales []types.Locale) (types.LocalizedStaticData, error) {
	missing := l.unloadedLocales(locales)
	if len(missing) > 0 {
		if err := l.loadLocales(ctx, missing); err != nil {
			return types.LocalizedStaticData{}, err
		}
	}

	l.mux.RLock()
	localeMap, known := l.internalCache[id]
	out := types.LocalizedStaticData{
		ID:           id,
		Descriptions: make(map[types.Locale]string, len(localeMap)),
	}
	for k, v := range localeMap {
		out.Descriptions[k] = v
	}
	if def, ok := localeMap[l.oddsFeedConfiguration.DefaultLocale()]; ok {
		out.Description = types.Some(def)
	}
	l.mux.RUnlock()
	if !known {
		// The id isn't in the upstream catalog even after the locales
		// loaded. Returning a "successful" zero-description value made
		// callers attach a NON-NIL empty description (BuildMatchStatus's
		// documented nil-on-unknown semantics silently broken); a typed
		// not-found lets them distinguish absent from empty.
		return out, fmt.Errorf("static data id %d not in catalog: %w", id, ErrItemNotFoundInCache)
	}
	return out, nil
}

// unloadedLocales returns the subset of `locales` not yet loaded.
// Snapshot taken under RLock.
func (l *LocalizedStaticDataCache) unloadedLocales(locales []types.Locale) []types.Locale {
	l.mux.RLock()
	defer l.mux.RUnlock()
	var missing []types.Locale
	for _, locale := range locales {
		if _, ok := l.loadedLocales[locale]; !ok {
			missing = append(missing, locale)
		}
	}
	return missing
}

// loadLocales fetches each missing locale through singleflight so
// concurrent callers for the same locale share one round-trip.
//
// Concurrency: each fetch is dispatched via DoChan and the caller
// selects on its own ctx — a slow fetch can't block past the caller's
// deadline. The shared fetcher is detached from the caller's
// cancellation (a short-deadline first caller can't cancel the HTTP
// request for later waiters) but rooted in the cache's LIFETIME ctx
// and bounded by lru.LoadTimeout — Close() aborts an in-flight fetch
// instead of letting it finish (and write) after shutdown, and a
// custom HTTP client with no timeout can't hang the flight forever.
func (l *LocalizedStaticDataCache) loadLocales(ctx context.Context, locales []types.Locale) error {
	// If the caller's ctx is already done, fail fast without kicking off
	// detached fetches the caller will never wait for.
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, locale := range locales {
		// Re-check between locales so a multi-locale call cancelled
		// mid-iteration doesn't keep starting detached fetches.
		if err := ctx.Err(); err != nil {
			return err
		}
		loc := locale
		ch := l.sf.DoChan(string(loc), func() (interface{}, error) { //nolint:contextcheck // deliberate detach: the shared fetch roots in the cache lifetime, not the caller's ctx (see doc above)
			// Double-check inside the critical region.
			l.mux.RLock()
			_, alreadyLoaded := l.loadedLocales[loc]
			l.mux.RUnlock()
			if alreadyLoaded {
				return nil, nil
			}

			// Built INSIDE the flight body: only the winner runs this
			// closure, so coalesced waiters don't each retain a timer.
			fetchCtx, cancel := context.WithTimeout(l.lifeCtx, lru.LoadTimeout)
			defer cancel()
			data, err := l.fetcher(fetchCtx, loc)
			if err != nil {
				return nil, err
			}

			l.mux.Lock()
			// Close-gate: a flight whose fetch finished just before the
			// cache lifetime was cancelled could otherwise pause here,
			// survive a successful Client.Close, then resume and commit —
			// contradicting Close's "nil return ⇒ all internal work
			// finished" contract. Re-check the lifetime immediately before
			// the commit (mirrors the EventCache / whoami close-gates),
			// shrinking the window to the few instructions under the lock.
			if l.lifeCtx.Err() != nil {
				l.mux.Unlock()
				return nil, l.lifeCtx.Err()
			}
			for _, sd := range data {
				localCache, ok := l.internalCache[sd.GetID()]
				if !ok {
					localCache = make(map[types.Locale]string)
					l.internalCache[sd.GetID()] = localCache
				}
				if d, ok := sd.GetDescription().Get(); ok {
					localCache[loc] = d
				}
			}
			// Mark the locale loaded EVEN when data is empty so the
			// next call doesn't re-fetch unconditionally.
			l.loadedLocales[loc] = struct{}{}
			l.mux.Unlock()
			return nil, nil
		})
		select {
		case r := <-ch:
			if r.Err != nil {
				return r.Err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// Item returns the entry in the configured default locale.
func (l *LocalizedStaticDataCache) Item(ctx context.Context, id int) (types.LocalizedStaticData, error) {
	return l.LocalizedItem(ctx, id, l.locales)
}

// Close cancels the lifecycle ctx (stopping the periodic-refresh
// goroutine and any in-flight fetch) and JOINS the refresh goroutine —
// cancel-without-join let it briefly outlive Client.Done, breaking the
// "Close nil return means all internal work finished" determinism
// contract. The join cannot hang: every path in the refresh loop is
// bounded by lifeCtx (in-flight fetches derive from it with a load
// timeout), so cancellation unwinds it promptly. Idempotent AND safe
// for concurrent callers — the previous bare read-then-nil of closeFn
// raced when two goroutines closed at once (cache.Manager.Close has no
// serialization of its own); concurrent callers all block until the
// goroutine has exited.
func (l *LocalizedStaticDataCache) Close() {
	l.CloseCtx(context.Background())
}

// CloseCtx is Close with the refresh-goroutine join BOUNDED by ctx —
// the refresh performs synchronous fetch I/O, and a supported custom
// transport that ignores cancellation could pin an unbounded join
// forever. Reports whether the goroutine actually exited; on false it
// leaks harmlessly until its in-flight fetch unwinds (everything it
// waits on was cancelled).
func (l *LocalizedStaticDataCache) CloseCtx(ctx context.Context) bool {
	l.closeOnce.Do(func() {
		if l.closeFn != nil {
			l.closeFn()
		}
	})
	if l.timerDone == nil {
		return true
	}
	select {
	case <-l.timerDone:
		return true
	case <-ctx.Done():
		select {
		case <-l.timerDone:
			return true
		default:
			return false
		}
	}
}

// timerTick runs the periodic refresh: re-fetch every locale already
// loaded so cached descriptions stay fresh. Each locale is refreshed
// individually under singleflight; a single fetcher failure no longer
// poisons the cache (the locale stays marked loaded with its previous
// data). The mutex is held only for the snapshot of locale keys and
// for each per-locale write — never across the HTTP fetch.
func (l *LocalizedStaticDataCache) timerTick(ctx context.Context) {
	l.mux.RLock()
	locales := make([]types.Locale, 0, len(l.loadedLocales))
	for locale := range l.loadedLocales {
		locales = append(locales, locale)
	}
	l.mux.RUnlock()

	for _, locale := range locales {
		loc := locale
		_, _, _ = l.sf.Do(string(loc), func() (interface{}, error) {
			// Same bound as loadLocales: the refresh fetch dies with the
			// cache lifetime (ctx here IS lifeCtx) and can't run forever
			// on a timeout-less custom HTTP client.
			fetchCtx, cancel := context.WithTimeout(ctx, lru.LoadTimeout)
			defer cancel()
			data, err := l.fetcher(fetchCtx, loc)
			if err != nil {
				l.logger.WithError(err).Errorf("failed to periodically fetch static data for %s", loc)
				return nil, nil
			}
			// Replace this locale's snapshot ATOMICALLY: upsert what
			// the fresh response contains AND delete this locale's
			// value for every id absent from it — an id removed
			// upstream (or whose description disappeared) otherwise
			// stayed cached forever, refresh after refresh. Ids left
			// with no locale at all are dropped entirely.
			fresh := make(map[int]string, len(data))
			for _, sd := range data {
				if d, ok := sd.GetDescription().Get(); ok {
					fresh[sd.GetID()] = d
				}
			}
			l.mux.Lock()
			for id, d := range fresh {
				localCache, ok := l.internalCache[id]
				if !ok {
					localCache = make(map[types.Locale]string)
					l.internalCache[id] = localCache
				}
				localCache[loc] = d
			}
			for id, localCache := range l.internalCache {
				if _, ok := fresh[id]; ok {
					continue
				}
				delete(localCache, loc)
				if len(localCache) == 0 {
					delete(l.internalCache, id)
				}
			}
			l.mux.Unlock()
			return nil, nil
		})
	}
}

func (l *LocalizedStaticDataCache) startTimer() {
	go func() {
		defer close(l.timerDone)
		select {
		case <-time.After(l.refreshInitialDelay):
		case <-l.lifeCtx.Done():
			return
		}
		// Refresh NOW, not on the first ticker fire: the ticker's first
		// tick lands a full tickPeriod after the initial delay, so data
		// loaded near startup stayed stale for initialDelay+tickPeriod
		// (~48h) instead of the intended ~24h.
		l.timerTick(l.lifeCtx)
		ticker := time.NewTicker(l.refreshTickPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				l.timerTick(l.lifeCtx)
			case <-l.lifeCtx.Done():
				return
			}
		}
	}()
}

// newLocalizedStaticDataCache constructs the cache. ctx is used to
// derive a lifecycle context (via WithoutCancel + WithCancel): caller
// metadata propagates into the periodic refresh goroutine, but its
// cancellation is severed so the cache outlives the construction-time
// ctx. Close() cancels the lifecycle. preloadLocales (defaults to the
// configured DefaultLocale when empty) is the locale set the cache
// fetches on first Item() call and refreshes on each timer tick.
func newLocalizedStaticDataCache(
	ctx context.Context,
	oddsFeedConfiguration config.Config,
	logger *log.Logger,
	preloadLocales []types.Locale,
	fetcher func(ctx context.Context, locale types.Locale) ([]types.StaticData, error),
) *LocalizedStaticDataCache {
	lifeCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))

	// Default locale always first (primary). Append preloads, dedup.
	locales := []types.Locale{oddsFeedConfiguration.DefaultLocale()}
	seen := map[types.Locale]struct{}{oddsFeedConfiguration.DefaultLocale(): {}}
	for _, l := range preloadLocales {
		if _, ok := seen[l]; ok {
			continue
		}
		seen[l] = struct{}{}
		locales = append(locales, l)
	}

	ca := &LocalizedStaticDataCache{
		oddsFeedConfiguration: oddsFeedConfiguration,
		fetcher:               fetcher,
		locales:               locales,
		internalCache:         make(map[int]map[types.Locale]string),
		loadedLocales:         make(map[types.Locale]struct{}),
		lifeCtx:               lifeCtx,
		closeFn:               cancel,
		timerDone:             make(chan struct{}),
		refreshInitialDelay:   initialDelay,
		refreshTickPeriod:     tickPeriod,
		logger:                logger,
	}
	ca.startTimer()
	return ca
}
