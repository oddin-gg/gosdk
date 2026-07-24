package cache

import (
	"testing"
	"time"

	apiXML "github.com/oddin-gg/gosdk/internal/api/xml"
	"github.com/oddin-gg/gosdk/internal/cache/lru"
	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

// TestMatchStatusCache_FeedMergeWinnerID is the regression for the
// dropped feed-side winner_id: the feed decoder exposes it
// (feedXML.SportEventStatus.WinnerID) but refreshOrInsertFeedItem never
// assigned it, so a live final-status update left MatchStatus.WinnerID
// nil (or stale from an earlier API summary) until an API refetch
// happened to overwrite the entry. The merge must assign a present,
// parseable winner_id, and preserve the previous value when the
// attribute is absent or malformed.
func TestMatchStatusCache_FeedMergeWinnerID(t *testing.T) {
	c := &MatchStatusCache{
		logger: log.New(nil),
		entries: lru.NewTTL[types.URN, *LocalizedMatchStatus](
			lru.DefaultEventCacheSize, nil, lru.DefaultEventCacheTTL),
	}
	id := types.URN{Prefix: "od", Type: "match", ID: 1}
	winner := "od:competitor:77"

	// In-play update without winner: WinnerID stays nil.
	c.refreshOrInsertFeedItem(id, &feedXML.SportEventStatus{Status: ptrOf(1), MatchStatus: ptrOf(1)}, time.Time{})
	entry, _ := c.entries.Get(id)
	if entry.winnerID != nil {
		t.Fatalf("winnerID = %v before any winner update, want nil", entry.winnerID)
	}

	// Final update carrying the winner: assigned.
	c.refreshOrInsertFeedItem(id, &feedXML.SportEventStatus{Status: ptrOf(3), MatchStatus: ptrOf(2), WinnerID: &winner}, time.Time{})
	entry, _ = c.entries.Get(id)
	if entry.winnerID == nil || entry.winnerID.ToString() != winner {
		t.Fatalf("winnerID = %v after winner update, want %s", entry.winnerID, winner)
	}

	// Later update WITHOUT the attribute: previous winner preserved.
	c.refreshOrInsertFeedItem(id, &feedXML.SportEventStatus{Status: ptrOf(3), MatchStatus: ptrOf(2)}, time.Time{})
	entry, _ = c.entries.Get(id)
	if entry.winnerID == nil || entry.winnerID.ToString() != winner {
		t.Fatalf("winnerID = %v after winner-less update, want preserved %s", entry.winnerID, winner)
	}

	// Malformed winner_id: previous value preserved, no panic.
	bad := "not a urn"
	c.refreshOrInsertFeedItem(id, &feedXML.SportEventStatus{Status: ptrOf(3), MatchStatus: ptrOf(2), WinnerID: &bad}, time.Time{})
	entry, _ = c.entries.Get(id)
	if entry.winnerID == nil || entry.winnerID.ToString() != winner {
		t.Fatalf("winnerID = %v after malformed update, want preserved %s", entry.winnerID, winner)
	}
}

// TestMarketDescription_OutcomeLevelLocaleCoverage is the regression for
// the partial-outcome-localization gap: coverage validation checked only
// the market NAME, so a market present in both locales' catalogs — with
// one catalog omitting an outcome — passed validation while that outcome
// silently lacked a requested locale. missingLocales must treat a locale
// as missing unless the market name AND every known outcome name cover
// it; outcome descriptions stay optional and never count.
func TestMarketDescription_OutcomeLevelLocaleCoverage(t *testing.T) {
	d := &LocalizedMarketDescription{
		id:   1,
		name: map[types.Locale]string{types.EnLocale: "Winner", types.RuLocale: "Победитель"},
		outcomes: map[string]*LocalizedOutcomeDescription{
			"1": {
				name:        map[types.Locale]string{types.EnLocale: "home", types.RuLocale: "Хозяева"},
				description: map[types.Locale]string{},
			},
			"2": {
				// ru payload omitted this outcome.
				name:        map[types.Locale]string{types.EnLocale: "away"},
				description: map[types.Locale]string{},
			},
		},
	}

	missing := d.missingLocales([]types.Locale{types.EnLocale, types.RuLocale})
	if len(missing) != 1 || missing[0] != types.RuLocale {
		t.Fatalf("missingLocales = %v, want [ru] (outcome 2 lacks ru)", missing)
	}
	if d.hasLocale(types.RuLocale) {
		t.Fatal("hasLocale(ru) = true with an outcome-level ru gap")
	}
	if !d.hasLocale(types.EnLocale) {
		t.Fatal("hasLocale(en) = false despite full en coverage")
	}

	// Close the gap: full coverage again. Descriptions stay absent —
	// they are optional and must not count against coverage.
	d.outcomes["2"].name[types.RuLocale] = "Гости"
	if missing := d.missingLocales([]types.Locale{types.EnLocale, types.RuLocale}); len(missing) != 0 {
		t.Fatalf("missingLocales after fill = %v, want none (descriptions are optional)", missing)
	}
}

// TestLocalizedMatch_UnknownSportFormat is the forward-compat
// regression: an unrecognised upstream sport_format used to abort merge
// with an error, breaking Client.Match / match listings / feed
// enrichment for every match carrying a newly introduced format. It
// must map to types.SportFormatUnknown, keep the raw value in
// ExtraInfo, and merge successfully; absent values keep the classic
// legacy default.
func TestLocalizedMatch_UnknownSportFormat(t *testing.T) {
	mk := func(format string) apiXML.SportEvent {
		ev := apiXML.SportEvent{
			ID: "od:match:5",
			Tournament: apiXML.Tournament{
				ID:    "od:tournament:1",
				Sport: apiXML.Sport{ID: "od:sport:1", Name: "Soccer"},
			},
		}
		if format != "" {
			ev.ExtraInfo = &apiXML.ExtraInfoWrapper{List: []apiXML.ExtraInfo{
				{Key: apiXML.ExtraInfoSportFormatKey, Value: format},
			}}
		}
		return ev
	}
	newLM := func() *LocalizedMatch {
		return &LocalizedMatch{
			name:      make(map[types.Locale]string),
			extraInfo: make(map[types.Locale]map[string]string),
		}
	}

	lm := newLM()
	if err := lm.merge(types.EnLocale, mk("futuristic")); err != nil {
		t.Fatalf("merge with unknown sport_format errored: %v", err)
	}
	if lm.sportFormat != types.SportFormatUnknown {
		t.Fatalf("sportFormat = %q, want %q", lm.sportFormat, types.SportFormatUnknown)
	}
	if got := lm.extraInfo[types.EnLocale][apiXML.ExtraInfoSportFormatKey]; got != "futuristic" {
		t.Fatalf("raw ExtraInfo sport_format = %q, want preserved", got)
	}

	lm2 := newLM()
	if err := lm2.merge(types.EnLocale, mk("")); err != nil {
		t.Fatalf("merge without sport_format errored: %v", err)
	}
	if lm2.sportFormat != types.SportFormatClassic {
		t.Fatalf("absent sport_format = %q, want classic legacy default", lm2.sportFormat)
	}
}

// TestMatchStatusCache_FreshnessOrdering pins the anti-rollback guards:
// (a) an API summary whose fetch STARTED before a feed update was
// applied must not overwrite it; (b) a feed message older (by upstream
// timestamp) than the newest applied feed update must not overwrite it.
func TestMatchStatusCache_FreshnessOrdering(t *testing.T) {
	c := &MatchStatusCache{
		logger:    log.New(nil),
		clearedAt: map[types.URN]time.Time{},
		entries: lru.NewTTL[types.URN, *LocalizedMatchStatus](
			lru.DefaultEventCacheSize, nil, lru.DefaultEventCacheTTL),
	}
	id := types.URN{Prefix: "od", Type: "match", ID: 9}

	// Fetch "starts", then a newer live update lands.
	fetchStarted := time.Now()
	time.Sleep(2 * time.Millisecond)
	c.refreshOrInsertFeedItem(id, &feedXML.SportEventStatus{Status: ptrOf(1), MatchStatus: ptrOf(5), HomeScore: ptrOf(2.0)}, time.Now())
	entry, _ := c.entries.Get(id)
	if entry.homeScore == nil || *entry.homeScore != 2 {
		t.Fatalf("feed update not applied: %+v", entry)
	}

	// The OLDER API response completes afterwards: must be skipped.
	if err := c.refreshOrInsertAPIItem(id, apiXML.SportEventStatus{CommonSportEventStatus: apiXML.CommonSportEventStatus{HomeScore: ptrOf(0.0)}, Status: "live", MatchStatusCode: ptrOf(1)}, fetchStarted); err != nil {
		t.Fatalf("refreshOrInsertAPIItem: %v", err)
	}
	entry, _ = c.entries.Get(id)
	if entry.homeScore == nil || *entry.homeScore != 2 {
		t.Fatalf("stale API response rolled back live score: %+v", entry)
	}

	// A NEWER fetch (started after the feed apply) may overwrite.
	if err := c.refreshOrInsertAPIItem(id, apiXML.SportEventStatus{CommonSportEventStatus: apiXML.CommonSportEventStatus{HomeScore: ptrOf(3.0)}, Status: "live", MatchStatusCode: ptrOf(1)}, time.Now()); err != nil {
		t.Fatalf("refreshOrInsertAPIItem(fresh): %v", err)
	}
	entry, _ = c.entries.Get(id)
	if entry.homeScore == nil || *entry.homeScore != 3 {
		t.Fatalf("fresh API response was not applied: %+v", entry)
	}

	// Feed-vs-feed: an out-of-order older message must not overwrite.
	newer := time.Now()
	older := newer.Add(-time.Minute)
	c.refreshOrInsertFeedItem(id, &feedXML.SportEventStatus{Status: ptrOf(1), MatchStatus: ptrOf(5), HomeScore: ptrOf(7.0)}, newer)
	c.refreshOrInsertFeedItem(id, &feedXML.SportEventStatus{Status: ptrOf(1), MatchStatus: ptrOf(5), HomeScore: ptrOf(1.0)}, older)
	entry, _ = c.entries.Get(id)
	if entry.homeScore == nil || *entry.homeScore != 7 {
		t.Fatalf("older feed message overwrote newer: %+v", entry)
	}
}
