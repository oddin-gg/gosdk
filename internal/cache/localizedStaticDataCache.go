package cache

import (
	"context"
	"sync"
	"time"

	"github.com/oddin-gg/gosdk/types"
	log "github.com/oddin-gg/gosdk/internal/log"
)

const (
	initialDelay = 24 * time.Hour
	tickPeriod   = 24 * time.Hour
)

// LocalizedStaticDataCache caches static-catalog data per locale.
//
// Phase 6 reshape: returns types.LocalizedStaticData value structs
// directly (the previous wrapper impl is gone).
type LocalizedStaticDataCache struct {
	oddsFeedConfiguration types.OddsFeedConfiguration
	fetcher               func(ctx context.Context, locale types.Locale) ([]types.StaticData, error)
	locales               []types.Locale
	internalCache         map[uint]map[types.Locale]string
	lifeCtx               context.Context
	closeFn               context.CancelFunc
	logger                *log.Logger
	mux                   sync.Mutex
}

// LocalizedItem returns a populated LocalizedStaticData for the given
// id, fetching missing locales as needed.
func (l *LocalizedStaticDataCache) LocalizedItem(ctx context.Context, id uint, locales []types.Locale) (types.LocalizedStaticData, error) {
	l.mux.Lock()
	defer l.mux.Unlock()

	fetched := l.fetchedLocales()
	missing := make([]types.Locale, 0)
	for _, locale := range locales {
		if _, ok := fetched[locale]; !ok {
			missing = append(missing, locale)
		}
	}
	if len(missing) > 0 {
		if err := l.fetchData(ctx, missing); err != nil {
			return types.LocalizedStaticData{}, err
		}
	}

	localeMap := l.internalCache[id]
	out := types.LocalizedStaticData{
		ID:           id,
		Descriptions: make(map[types.Locale]string, len(localeMap)),
	}
	for k, v := range localeMap {
		out.Descriptions[k] = v
	}
	if def, ok := localeMap[l.oddsFeedConfiguration.DefaultLocale()]; ok {
		out.Description = &def
	}
	return out, nil
}

// Item returns the entry in the configured default locale.
func (l *LocalizedStaticDataCache) Item(ctx context.Context, id uint) (types.LocalizedStaticData, error) {
	return l.LocalizedItem(ctx, id, l.locales)
}

// Close cancels the periodic-refresh goroutine. Idempotent.
func (l *LocalizedStaticDataCache) Close() {
	if l.closeFn != nil {
		l.closeFn()
		l.closeFn = nil
	}
}

func (l *LocalizedStaticDataCache) fetchData(ctx context.Context, locales []types.Locale) error {
	for _, locale := range locales {
		data, err := l.fetcher(ctx, locale)
		if err != nil {
			return err
		}
		for _, sd := range data {
			localCache, ok := l.internalCache[sd.GetID()]
			if !ok {
				localCache = make(map[types.Locale]string)
				l.internalCache[sd.GetID()] = localCache
			}
			if d := sd.GetDescription(); d != nil {
				localCache[locale] = *d
			}
		}
	}
	return nil
}

func (l *LocalizedStaticDataCache) fetchedLocales() map[types.Locale]struct{} {
	result := make(map[types.Locale]struct{})
	for _, value := range l.internalCache {
		for locale := range value {
			result[locale] = struct{}{}
		}
	}
	return result
}

func (l *LocalizedStaticDataCache) timerTick(ctx context.Context) {
	l.mux.Lock()
	defer l.mux.Unlock()

	localeMap := l.fetchedLocales()
	locales := make([]types.Locale, 0, len(localeMap))
	for k := range localeMap {
		locales = append(locales, k)
	}

	if err := l.fetchData(ctx, locales); err != nil {
		l.logger.WithError(err).Errorf("failed to periodically fetch static data")
	}
}

func (l *LocalizedStaticDataCache) startTimer() {
	go func() {
		select {
		case <-time.After(initialDelay):
		case <-l.lifeCtx.Done():
			return
		}
		ticker := time.NewTicker(tickPeriod)
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
// ctx. Close() cancels the lifecycle.
func newLocalizedStaticDataCache(
	ctx context.Context,
	oddsFeedConfiguration types.OddsFeedConfiguration,
	logger *log.Logger,
	fetcher func(ctx context.Context, locale types.Locale) ([]types.StaticData, error),
) *LocalizedStaticDataCache {
	lifeCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	ca := &LocalizedStaticDataCache{
		oddsFeedConfiguration: oddsFeedConfiguration,
		fetcher:               fetcher,
		locales:               []types.Locale{oddsFeedConfiguration.DefaultLocale()},
		internalCache:         make(map[uint]map[types.Locale]string),
		lifeCtx:               lifeCtx,
		closeFn:               cancel,
		logger:                logger,
	}
	ca.startTimer()
	return ca
}
