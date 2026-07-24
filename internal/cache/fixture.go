package cache

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/cache/lru"
	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	"github.com/oddin-gg/gosdk/types"
)

// FixtureCache stores fixture data per (URN, locale).
//
// Phase 3 rewrite: replaces the patrickmn/go-cache + partial-mutex design
// with lru.EventCache's multi-locale fill-in + singleflight semantics, and
// plumbs ctx through the loader. Per-entry mutex now guards every field
// (no more partial locking).
type FixtureCache struct {
	apiClient *api.Client
	lru       *lru.EventCache[types.URN, types.Locale, *LocalizedFixture]
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
	extraInfo  map[types.Locale]map[string]string
	tvChannels map[types.Locale][]types.TvChannel

	// loaded is the set of locales currently populated.
	loaded map[types.Locale]struct{}
}

// Locales implements lru.LocalizedEntry.
func (f *LocalizedFixture) Locales() []types.Locale {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]types.Locale, 0, len(f.loaded))
	for l := range f.loaded {
		out = append(out, l)
	}
	return out
}

// cloneForUpdate returns a copy the loader can merge into without
// mutating the live cached entry (copy-on-write): a failed later-locale
// fetch must not leave earlier locales of the SAME load visible to
// concurrent readers — the load/admit transaction admits the clone only
// after every locale and the coverage validation succeed. Top-level maps
// are copied one level deep; inner maps/slices are safe to alias because
// the loader only ever REPLACES per-locale values wholesale.
func (f *LocalizedFixture) cloneForUpdate() *LocalizedFixture {
	f.mu.RLock()
	defer f.mu.RUnlock()
	c := &LocalizedFixture{
		startTime:  f.startTime,
		extraInfo:  make(map[types.Locale]map[string]string, len(f.extraInfo)+1),
		tvChannels: make(map[types.Locale][]types.TvChannel, len(f.tvChannels)+1),
		loaded:     make(map[types.Locale]struct{}, len(f.loaded)+1),
	}
	for k, v := range f.extraInfo {
		c.extraInfo[k] = v
	}
	for k, v := range f.tvChannels {
		c.tvChannels[k] = v
	}
	for k := range f.loaded {
		c.loaded[k] = struct{}{}
	}
	return c
}

func (f *LocalizedFixture) StartTime() *time.Time {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.startTime
}

// ExtraInfo returns the extra-info map for the given locale, or nil if the
// locale wasn't loaded.
func (f *LocalizedFixture) ExtraInfo(locale types.Locale) map[string]string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.extraInfo[locale]
}

// TvChannels returns the channel list for the given locale, or nil if the
// locale wasn't loaded.
func (f *LocalizedFixture) TvChannels(locale types.Locale) []types.TvChannel {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.tvChannels[locale]
}

// Fixture returns a populated LocalizedFixture for the given key, fetching
// missing locales as needed. A definitive upstream 404 is classified via
// notFoundIfAbsent so errors.Is(err, ErrItemNotFound) holds — contract
// parity with the other by-id-fetch entity reads (match, tournament,
// competitor, player).
func (f *FixtureCache) Fixture(ctx context.Context, id types.URN, locales []types.Locale) (*LocalizedFixture, error) {
	v, _, err := f.lru.Get(ctx, id, locales)
	if err != nil {
		return nil, notFoundIfAbsent(err)
	}
	return v, nil
}

// OnFeedMessage clears the cached fixture for `id` when a FixtureChange
// arrives for a match. This is the auto-invalidation trigger documented in
// NEXT.md §6.
func (f *FixtureCache) OnFeedMessage(id types.URN, feedMessage *types.FeedMessage) {
	if feedMessage.Message == nil {
		return
	}
	if _, ok := feedMessage.Message.(*feedXML.FixtureChange); !ok || id.Type != "match" {
		return
	}
	f.ClearCacheItem(id)
}

// ClearCacheItem is the public invalidation hook (exposed via Client.ClearFixture).
func (f *FixtureCache) ClearCacheItem(id types.URN) {
	f.lru.Clear(id)
}

func newFixtureCache(lifeCtx context.Context, client *api.Client) *FixtureCache {
	fc := &FixtureCache{apiClient: client}
	loader := func(
		ctx context.Context,
		id types.URN,
		missing []types.Locale,
		existing *LocalizedFixture,
		hasExisting bool,
	) (*LocalizedFixture, error) {
		var entry *LocalizedFixture
		if hasExisting {
			// Copy-on-write: merge into a clone so a failed later-locale
			// fetch can't leave this load's earlier locales visible on
			// the live cached entry. The clone replaces the cached
			// pointer only at admission (lru.Add), after coverage
			// validation succeeds.
			entry = existing.cloneForUpdate()
		} else {
			entry = &LocalizedFixture{
				extraInfo:  make(map[types.Locale]map[string]string),
				tvChannels: make(map[types.Locale][]types.TvChannel),
				loaded:     make(map[types.Locale]struct{}),
			}
		}

		for _, locale := range missing {
			data, err := client.FetchFixture(ctx, id, locale)
			if err != nil {
				return nil, fmt.Errorf("fetch fixture %s/%s: %w", id.ToString(), locale, err)
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
				ch := make([]types.TvChannel, len(data.TVChannels.List))
				for i := range data.TVChannels.List {
					tv := data.TVChannels.List[i]
					ch[i] = types.TvChannel{
						Name:      tv.Name,
						Language:  tv.Language,
						StreamURL: tv.StreamURL,
					}
				}
				entry.tvChannels[locale] = ch
			}
			entry.loaded[locale] = struct{}{}
			entry.mu.Unlock()
		}
		return entry, nil
	}
	fc.lru = lru.NewEventCache[types.URN, types.Locale, *LocalizedFixture](
		lru.Config{Lifetime: lifeCtx}, loader,
	)
	return fc
}

// BuildFixture resolves a per-locale Fixture snapshot from the cache,
// fetching missing locales as needed, and returns the populated value.
// `locale` is the locale to project on the returned struct (extra info
// and tv channels are pulled from that locale; startTime is locale-
// independent).
//
// StartTime and ExtraInfo are deep-cloned so caller mutations on the
// returned snapshot don't corrupt the cache; a second read after a
// caller mutation returns the cache's value, not the mutated one.
// (TvChannels was already copied via append-clone.)
func BuildFixture(ctx context.Context, fc *FixtureCache, id types.URN, locale types.Locale) (*types.Fixture, error) {
	item, err := fc.Fixture(ctx, id, []types.Locale{locale})
	if err != nil {
		return nil, err
	}
	out := &types.Fixture{
		StartTime: clonePtr(item.StartTime()),
		ExtraInfo: cloneStringMap(item.ExtraInfo(locale)),
		Locale:    locale,
	}
	if ch := item.TvChannels(locale); len(ch) > 0 {
		out.TvChannels = append([]types.TvChannel(nil), ch...)
	}
	return out, nil
}
