package cache

import (
	"github.com/oddin-gg/gosdk/protocols"
	log "github.com/sirupsen/logrus"
	"sync"
	"time"
)

type localizedStaticDataImpl struct {
	id                    uint
	data                  map[protocols.Locale]string
	oddsFeedConfiguration protocols.OddsFeedConfiguration
}

func (l localizedStaticDataImpl) GetID() uint {
	return l.id
}

func (l localizedStaticDataImpl) GetDescription() *string {
	return l.LocalizedDescription(l.oddsFeedConfiguration.DefaultLocale())
}

func (l localizedStaticDataImpl) LocalizedDescription(locale protocols.Locale) *string {
	description, ok := l.data[locale]
	if !ok {
		return nil
	}

	return &description
}

const (
	initialDelay = 24 * time.Hour
	tickPeriod   = 24 * time.Hour
)

// LocalizedStaticDataCache ...
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

// LocalizedItem ...
func (l *LocalizedStaticDataCache) LocalizedItem(id uint, locales []protocols.Locale) (protocols.LocalizedStaticData, error) {
	l.mux.Lock()
	defer l.mux.Unlock()

	fetchedLocales := l.fetchedLocales()

	missingLocales := make([]protocols.Locale, 0)
	for i := range locales {
		locale := locales[i]
		_, exists := fetchedLocales[locale]
		if !exists {
			missingLocales = append(missingLocales, locale)
		}
	}

	if len(missingLocales) != 0 {
		err := l.fetchData(missingLocales)
		if err != nil {
			return nil, err
		}
	}

	localeMap := l.internalCache[id]
	result := make(map[protocols.Locale]string, len(localeMap))
	for key, value := range localeMap {
		result[key] = value
	}

	return localizedStaticDataImpl{
		id:                    id,
		data:                  result,
		oddsFeedConfiguration: l.oddsFeedConfiguration,
	}, nil
}

// Item ...
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
	for i := range locales {
		locale := locales[i]

		data, err := l.fetcher(locale)
		if err != nil {
			return err
		}

		for _, staticData := range data {
			localCache, ok := l.internalCache[staticData.GetID()]
			if !ok {
				localCache = make(map[protocols.Locale]string)
				l.internalCache[staticData.GetID()] = localCache
			}

			localCache[locale] = *staticData.GetDescription()
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
	locales := make([]protocols.Locale, len(localeMap))

	index := 0
	for key := range localeMap {
		locales[index] = key
	}

	err := l.fetchData(locales)
	if err != nil {
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
