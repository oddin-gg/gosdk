package cache

import (
	"time"

	"github.com/oddin-gg/gosdk/internal/api"
	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	"github.com/oddin-gg/gosdk/protocols"
	"github.com/patrickmn/go-cache"
)

// FixtureCache ...
type FixtureCache struct {
	apiClient     *api.Client
	internalCache *cache.Cache
}

// OnFeedMessage ...
func (f *FixtureCache) OnFeedMessage(id protocols.URN, feedMessage *protocols.FeedMessage) {
	if feedMessage.Message == nil {
		return
	}

	_, ok := feedMessage.Message.(*feedXML.FixtureChange)
	if !ok || id.Type != "match" {
		return
	}

	f.ClearCacheItem(id)
}

// Fixture ...
func (f *FixtureCache) Fixture(id protocols.URN, locale protocols.Locale) (*LocalizedFixture, error) {
	item, _ := f.internalCache.Get(id.ToString())
	result, ok := item.(*LocalizedFixture)
	if ok {
		return result, nil
	}

	fixture, err := f.loadAndCacheItem(id, locale)
	if err != nil {
		return nil, err
	}

	return fixture, nil
}

// ClearCacheItem ...
func (f *FixtureCache) ClearCacheItem(id protocols.URN) {
	f.internalCache.Delete(id.ToString())
}

func (f *FixtureCache) loadAndCacheItem(id protocols.URN, locale protocols.Locale) (*LocalizedFixture, error) {
	data, err := f.apiClient.FetchFixture(id, locale)
	if err != nil {
		return nil, err
	}

	var fixture LocalizedFixture
	if data.StartTime != nil {
		fixture.startTime = (*time.Time)(data.StartTime)
	}

	if data.ExtraInfo != nil {
		fixture.extraInfo = make(map[string]string)
		for _, extraInfo := range data.ExtraInfo.List {
			fixture.extraInfo[extraInfo.Key] = extraInfo.Value
		}
	}

	if data.TVChannels != nil {
		fixture.tvChannels = make([]protocols.TvChannel, len(data.TVChannels.List))
		for i := range data.TVChannels.List {
			tvChannel := data.TVChannels.List[i]
			fixture.tvChannels[i] = tvChannelImpl{
				name:      tvChannel.Name,
				streamURL: tvChannel.StreamURL,
				language:  tvChannel.Language,
			}
		}
	}

	f.internalCache.Set(id.ToString(), &fixture, 0)
	return &fixture, nil
}

func newFixtureCache(client *api.Client) *FixtureCache {
	return &FixtureCache{
		apiClient:     client,
		internalCache: cache.New(12*time.Hour, 1*time.Hour),
	}
}

// LocalizedFixture ...
type LocalizedFixture struct {
	startTime  *time.Time
	extraInfo  map[string]string
	tvChannels []protocols.TvChannel
}

type fixtureImpl struct {
	id           protocols.URN
	fixtureCache *FixtureCache
	locales      []protocols.Locale
}

func (f fixtureImpl) StartTime() (*time.Time, error) {
	item, err := f.fixtureCache.Fixture(f.id, f.locales[0])
	if err != nil {
		return nil, err
	}

	return item.startTime, nil
}

func (f fixtureImpl) ExtraInfo() (map[string]string, error) {
	item, err := f.fixtureCache.Fixture(f.id, f.locales[0])
	if err != nil {
		return nil, err
	}

	return item.extraInfo, nil
}

func (f fixtureImpl) TvChannels() ([]protocols.TvChannel, error) {
	item, err := f.fixtureCache.Fixture(f.id, f.locales[0])
	if err != nil {
		return nil, err
	}

	return item.tvChannels, nil
}

type tvChannelImpl struct {
	name      string
	language  string
	streamURL string
}

func (t tvChannelImpl) Name() string {
	return t.name
}

func (t tvChannelImpl) StreamURL() string {
	return t.streamURL
}

func (t tvChannelImpl) Language() string {
	return t.language
}

// NewFixture ...
func NewFixture(id protocols.URN, fixtureCache *FixtureCache, locales []protocols.Locale) protocols.Fixture {
	return &fixtureImpl{
		id:           id,
		fixtureCache: fixtureCache,
		locales:      locales,
	}
}
