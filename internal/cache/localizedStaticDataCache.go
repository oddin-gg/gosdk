package cache

import (
	"sync"
	"time"

	"github.com/oddin-gg/gosdk/protocols"
	log "github.com/oddin-gg/gosdk/internal/log"
)

const (
	initialDelay = 24 * time.Hour
	tickPeriod   = 24 * time.Hour
)

// LocalizedStaticDataCache caches static-catalog data per locale.
//
// Phase 6 reshape: returns protocols.LocalizedStaticData value structs
// directly (the previous wrapper impl is gone).
type LocalizedStaticDataCache struct {
	oddsFeedConfiguration protocols.OddsFeedConfiguration
	fetcher               func(locale protocols.Locale) ([]protocols.StaticData, error)
	locales               []protocols.Locale
	internalCache         map[uint]map[protocols.Locale]string
	ticker                *time.Ticker
	closeCh               chan bool
	logger                *log.Logger
	mux                   sync.Mutex
}

// LocalizedItem returns a populated LocalizedStaticData for the given
// id, fetching missing locales as needed.
func (l *LocalizedStaticDataCache) LocalizedItem(id uint, locales []protocols.Locale) (protocols.LocalizedStaticData, error) {
	l.mux.Lock()
	defer l.mux.Unlock()

	fetched := l.fetchedLocales()
	missing := make([]protocols.Locale, 0)
	for _, locale := range locales {
		if _, ok := fetched[locale]; !ok {
			missing = append(missing, locale)
		}
	}
	if len(missing) > 0 {
		if err := l.fetchData(missing); err != nil {
			return protocols.LocalizedStaticData{}, err
		}
	}

	localeMap := l.internalCache[id]
	out := protocols.LocalizedStaticData{
		ID:           id,
		Descriptions: make(map[protocols.Locale]string, len(localeMap)),
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
func (l *LocalizedStaticDataCache) Item(id uint) (protocols.LocalizedStaticData, error) {
	return l.LocalizedItem(id, l.locales)
}

// Close ...
func (l *LocalizedStaticDataCache) Close() {
	if l.closeCh != nil {
		l.closeCh <- true
	}
	l.closeCh = nil
}

func (l *LocalizedStaticDataCache) fetchData(locales []protocols.Locale) error {
	for _, locale := range locales {
		data, err := l.fetcher(locale)
		if err != nil {
			return err
		}
		for _, sd := range data {
			localCache, ok := l.internalCache[sd.GetID()]
			if !ok {
				localCache = make(map[protocols.Locale]string)
				l.internalCache[sd.GetID()] = localCache
			}
			if d := sd.GetDescription(); d != nil {
				localCache[locale] = *d
			}
		}
	}
	return nil
}

func (l *LocalizedStaticDataCache) fetchedLocales() map[protocols.Locale]struct{} {
	result := make(map[protocols.Locale]struct{})
	for _, value := range l.internalCache {
		for locale := range value {
			result[locale] = struct{}{}
		}
	}
	return result
}

func (l *LocalizedStaticDataCache) timerTick() {
	l.mux.Lock()
	defer l.mux.Unlock()

	localeMap := l.fetchedLocales()
	locales := make([]protocols.Locale, 0, len(localeMap))
	for k := range localeMap {
		locales = append(locales, k)
	}

	if err := l.fetchData(locales); err != nil {
		l.logger.WithError(err).Errorf("failed to periodically fetch static data")
	}
}

func (l *LocalizedStaticDataCache) startTimer() {
	l.closeCh = make(chan bool, 1)
	go func() {
		time.Sleep(initialDelay)
		l.ticker = time.NewTicker(tickPeriod)
		for {
			select {
			case <-l.ticker.C:
				l.timerTick()
			case <-l.closeCh:
				return
			}
		}
	}()
}

func newLocalizedStaticDataCache(oddsFeedConfiguration protocols.OddsFeedConfiguration, fetcher func(locale protocols.Locale) ([]protocols.StaticData, error)) *LocalizedStaticDataCache {
	ca := &LocalizedStaticDataCache{
		oddsFeedConfiguration: oddsFeedConfiguration,
		fetcher:               fetcher,
		locales:               []protocols.Locale{oddsFeedConfiguration.DefaultLocale()},
		internalCache:         make(map[uint]map[protocols.Locale]string),
	}
	ca.startTimer()
	return ca
}
