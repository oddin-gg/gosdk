package cache

import (
	"context"
	"sync"
	"time"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/cache/lru"
	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	"github.com/oddin-gg/gosdk/protocols"
)

// FixtureCache stores fixture data per (URN, locale).
//
// Phase 3 rewrite: replaces the patrickmn/go-cache + partial-mutex design
// with lru.EventCache's multi-locale fill-in + singleflight semantics, and
// plumbs ctx through the loader. Per-entry mutex now guards every field
// (no more partial locking).
type FixtureCache struct {
	apiClient *api.Client
	lru       *lru.EventCache[protocols.URN, protocols.Locale, *LocalizedFixture]
}

// LocalizedFixture is the cached representation of a fixture, populated
// per-locale. Fields are read/written under mu.
//
// extraInfo varies by locale (the upstream API returns localized values for
// some keys). startTime and tvChannels are conceptually locale-independent
// but the API returns them per locale call; we keep the most recent set.
type LocalizedFixture struct {
	mu sync.RWMutex

	startTime  *time.Time
	extraInfo  map[protocols.Locale]map[string]string
	tvChannels map[protocols.Locale][]protocols.TvChannel

	// loaded is the set of locales currently populated.
	loaded map[protocols.Locale]struct{}
}

// Locales implements lru.LocalizedEntry.
func (f *LocalizedFixture) Locales() []protocols.Locale {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]protocols.Locale, 0, len(f.loaded))
	for l := range f.loaded {
		out = append(out, l)
	}
	return out
}

func (f *LocalizedFixture) StartTime() *time.Time {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.startTime
}

// ExtraInfo returns the extra-info map for the given locale, or nil if the
// locale wasn't loaded.
func (f *LocalizedFixture) ExtraInfo(locale protocols.Locale) map[string]string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.extraInfo[locale]
}

// TvChannels returns the channel list for the given locale, or nil if the
// locale wasn't loaded.
func (f *LocalizedFixture) TvChannels(locale protocols.Locale) []protocols.TvChannel {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.tvChannels[locale]
}

// Fixture returns a populated LocalizedFixture for the given key, fetching
// missing locales as needed.
func (f *FixtureCache) Fixture(ctx context.Context, id protocols.URN, locales []protocols.Locale) (*LocalizedFixture, error) {
	v, _, err := f.lru.Get(ctx, id, locales)
	if err != nil {
		return nil, err
	}
	return v, nil
}

// OnFeedMessage clears the cached fixture for `id` when a FixtureChange
// arrives for a match. This is the auto-invalidation trigger documented in
// NEXT.md §6.
func (f *FixtureCache) OnFeedMessage(id protocols.URN, feedMessage *protocols.FeedMessage) {
	if feedMessage.Message == nil {
		return
	}
	if _, ok := feedMessage.Message.(*feedXML.FixtureChange); !ok || id.Type != "match" {
		return
	}
	f.ClearCacheItem(id)
}

// ClearCacheItem is the public invalidation hook (exposed via SportsInfoManager).
func (f *FixtureCache) ClearCacheItem(id protocols.URN) {
	f.lru.Clear(id)
}

func newFixtureCache(client *api.Client) *FixtureCache {
	fc := &FixtureCache{apiClient: client}
	loader := func(
		ctx context.Context,
		id protocols.URN,
		missing []protocols.Locale,
		existing *LocalizedFixture,
		hasExisting bool,
	) (*LocalizedFixture, error) {
		var entry *LocalizedFixture
		if hasExisting {
			entry = existing
		} else {
			entry = &LocalizedFixture{
				extraInfo:  make(map[protocols.Locale]map[string]string),
				tvChannels: make(map[protocols.Locale][]protocols.TvChannel),
				loaded:     make(map[protocols.Locale]struct{}),
			}
		}

		for _, locale := range missing {
			data, err := client.FetchFixture(ctx, id, locale)
			if err != nil {
				return nil, err
			}

			entry.mu.Lock()
			if data.StartTime != nil {
				entry.startTime = (*time.Time)(data.StartTime)
			}
			if data.ExtraInfo != nil {
				m := make(map[string]string, len(data.ExtraInfo.List))
				for _, info := range data.ExtraInfo.List {
					m[info.Key] = info.Value
				}
				entry.extraInfo[locale] = m
			}
			if data.TVChannels != nil {
				ch := make([]protocols.TvChannel, len(data.TVChannels.List))
				for i := range data.TVChannels.List {
					tv := data.TVChannels.List[i]
					ch[i] = tvChannelImpl{
						name:      tv.Name,
						streamURL: tv.StreamURL,
						language:  tv.Language,
					}
				}
				entry.tvChannels[locale] = ch
			}
			entry.loaded[locale] = struct{}{}
			entry.mu.Unlock()
		}
		return entry, nil
	}
	fc.lru = lru.NewEventCache[protocols.URN, protocols.Locale, *LocalizedFixture](
		lru.Config{}, loader,
	)
	return fc
}

// fixtureImpl satisfies protocols.Fixture. Its accessors are pure data —
// they read from the cached *LocalizedFixture but do not perform I/O. The
// public API queries this with the SDK's default locale (locales[0]).
type fixtureImpl struct {
	id           protocols.URN
	fixtureCache *FixtureCache
	locales      []protocols.Locale
}

func (f fixtureImpl) StartTime() (*time.Time, error) {
	item, err := f.fixtureCache.Fixture(context.Background(), f.id, f.locales)
	if err != nil {
		return nil, err
	}
	return item.StartTime(), nil
}

func (f fixtureImpl) ExtraInfo() (map[string]string, error) {
	item, err := f.fixtureCache.Fixture(context.Background(), f.id, f.locales)
	if err != nil {
		return nil, err
	}
	return item.ExtraInfo(f.locales[0]), nil
}

func (f fixtureImpl) TvChannels() ([]protocols.TvChannel, error) {
	item, err := f.fixtureCache.Fixture(context.Background(), f.id, f.locales)
	if err != nil {
		return nil, err
	}
	return item.TvChannels(f.locales[0]), nil
}

type tvChannelImpl struct {
	name      string
	language  string
	streamURL string
}

func (t tvChannelImpl) Name() string      { return t.name }
func (t tvChannelImpl) StreamURL() string { return t.streamURL }
func (t tvChannelImpl) Language() string  { return t.language }

// NewFixture ...
func NewFixture(id protocols.URN, fixtureCache *FixtureCache, locales []protocols.Locale) protocols.Fixture {
	return &fixtureImpl{
		id:           id,
		fixtureCache: fixtureCache,
		locales:      locales,
	}
}
