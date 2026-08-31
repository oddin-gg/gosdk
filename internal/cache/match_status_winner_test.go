package cache

import (
	"context"
	"errors"
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

// TestMatchStatusCache_StatuslessFirstPayloadIsUnknown: a match whose
// FIRST observed payload carries no status attribute must surface the
// documented types.UnknownEventStatus — not the out-of-enum zero
// EventStatus(""). Both merge paths are presence-preserving, so only
// entry creation could produce the empty string; the wire-value
// mappers already normalise unrecognised values to unknown.
func TestMatchStatusCache_StatuslessFirstPayloadIsUnknown(t *testing.T) {
	c := &MatchStatusCache{
		logger:    log.New(nil),
		clearedAt: map[types.URN]time.Time{},
		entries: lru.NewTTL[types.URN, *LocalizedMatchStatus](
			lru.DefaultEventCacheSize, nil, lru.DefaultEventCacheTTL),
	}
	id := types.URN{Prefix: "od", Type: "match", ID: 31}

	// Feed-first: an odds change with scores but no status attribute.
	c.refreshOrInsertFeedItem(id, &feedXML.SportEventStatus{HomeScore: ptrOf(1.0)}, time.Now())
	entry, _ := c.entries.Get(id)
	if entry.status != types.UnknownEventStatus {
		t.Fatalf("feed-first status = %q, want %q", entry.status, types.UnknownEventStatus)
	}

	// API-first: a summary whose status attribute is empty.
	id2 := types.URN{Prefix: "od", Type: "match", ID: 32}
	if err := c.refreshOrInsertAPIItem(id2, apiXML.SportEventStatus{
		CommonSportEventStatus: apiXML.CommonSportEventStatus{HomeScore: ptrOf(1.0)},
	}, time.Now()); err != nil {
		t.Fatalf("refreshOrInsertAPIItem: %v", err)
	}
	entry, _ = c.entries.Get(id2)
	if entry.status != types.UnknownEventStatus {
		t.Fatalf("API-first status = %q, want %q", entry.status, types.UnknownEventStatus)
	}

	// A later real status still lands.
	c.refreshOrInsertFeedItem(id, &feedXML.SportEventStatus{Status: ptrOf(1)}, time.Now())
	entry, _ = c.entries.Get(id)
	if entry.status != types.LiveEventStatus {
		t.Fatalf("status after real update = %q, want %q", entry.status, types.LiveEventStatus)
	}
}

// TestBuildMatchStatus_StatusDescriptionOutcomes pins the three
// contractual outcomes of the status-description resolution — the
// ErrStaticDataLocaleIncomplete tolerance branch in particular, whose
// ONLY production consumer is BuildMatchStatus: were the branch
// removed or reordered, the default case would turn a routine upstream
// catalog gap into a hard failure for every multi-locale match-status
// read, and (pre-this-test) nothing would have caught it.
//
//   - catalog gap in SOME requested locale: tolerated, the PARTIAL
//     description is attached (the locales that exist beat dropping
//     the description entirely);
//   - id absent from EVERY loaded locale: tolerated, StatusDescription
//     stays nil (documented optional);
//   - transport/fetch failure: propagates as an error — never silently
//     converted to "no description".
func TestBuildMatchStatus_StatusDescriptionOutcomes(t *testing.T) {
	newStatusCache := func(id types.URN, statusID int) *MatchStatusCache {
		c := &MatchStatusCache{
			logger:    log.New(nil),
			clearedAt: map[types.URN]time.Time{},
			entries: lru.NewTTL[types.URN, *LocalizedMatchStatus](
				lru.DefaultEventCacheSize, nil, lru.DefaultEventCacheTTL),
		}
		c.refreshOrInsertFeedItem(id, &feedXML.SportEventStatus{Status: ptrOf(1), MatchStatus: ptrOf(statusID)}, time.Now())
		return c
	}
	id := types.URN{Prefix: "od", Type: "match", ID: 21}

	t.Run("locale gap attaches the partial description", func(t *testing.T) {
		fetcher := func(ctx context.Context, locale types.Locale) ([]types.StaticData, error) {
			if locale == types.EnLocale {
				return []types.StaticData{{ID: 42, Description: types.Some("live")}}, nil
			}
			return nil, nil // the de catalog omits status 42
		}
		static := newLocalizedStaticDataCache(t.Context(), &minimalCfg{}, log.New(nil), nil, fetcher)
		defer static.Close()

		out, err := BuildMatchStatus(t.Context(), newStatusCache(id, 42), static, id, []types.Locale{types.EnLocale, types.DeLocale})
		if err != nil {
			t.Fatalf("BuildMatchStatus: %v (a catalog locale gap must be tolerated)", err)
		}
		if out.StatusDescription == nil {
			t.Fatal("StatusDescription = nil, want the partial description attached")
		}
		if got := out.StatusDescription.Descriptions[types.EnLocale]; got != "live" {
			t.Fatalf("Descriptions[en] = %q, want %q", got, "live")
		}
		if _, ok := out.StatusDescription.Descriptions[types.DeLocale]; ok {
			t.Fatalf("Descriptions[de] = %v, want absent", out.StatusDescription.Descriptions)
		}
		if v, ok := out.StatusDescription.Description.Get(); !ok || v != "live" {
			t.Fatalf("Description = %v, want Some(live) (primary = locales[0])", out.StatusDescription.Description)
		}
	})

	t.Run("unknown status id leaves StatusDescription nil", func(t *testing.T) {
		fetcher := func(ctx context.Context, locale types.Locale) ([]types.StaticData, error) {
			return []types.StaticData{{ID: 1, Description: types.Some("other")}}, nil
		}
		static := newLocalizedStaticDataCache(t.Context(), &minimalCfg{}, log.New(nil), nil, fetcher)
		defer static.Close()

		out, err := BuildMatchStatus(t.Context(), newStatusCache(id, 42), static, id, []types.Locale{types.EnLocale, types.DeLocale})
		if err != nil {
			t.Fatalf("BuildMatchStatus: %v (genuine absence must be tolerated)", err)
		}
		if out.StatusDescription != nil {
			t.Fatalf("StatusDescription = %+v, want nil for an id absent from every locale", out.StatusDescription)
		}
	})

	t.Run("fetch failure propagates", func(t *testing.T) {
		fetcher := func(ctx context.Context, locale types.Locale) ([]types.StaticData, error) {
			return nil, errors.New("upstream exploded")
		}
		static := newLocalizedStaticDataCache(t.Context(), &minimalCfg{}, log.New(nil), nil, fetcher)
		defer static.Close()

		if _, err := BuildMatchStatus(t.Context(), newStatusCache(id, 42), static, id, []types.Locale{types.EnLocale}); err == nil {
			t.Fatal("BuildMatchStatus = nil error, want the fetch failure propagated (only the two catalog-gap outcomes are tolerated)")
		}
	})
}

// TestMatchStatusCache_APIvsAPIOrdering is the regression for the
// missing API-vs-API monotonic guard. Concurrent summary fetches for
// one id are real (the match cache's per-locale loads and the status
// cache's own loader run under separate singleflight groups; every
// response lands in refreshOrInsertAPIItem via the observer). An
// earlier-STARTED fetch finishing LAST used to reinstall its older
// status/scores/winner over the newer one — for a just-finished match
// with no further feed traffic the rollback stood until entry TTL.
func TestMatchStatusCache_APIvsAPIOrdering(t *testing.T) {
	c := &MatchStatusCache{
		logger:    log.New(nil),
		clearedAt: map[types.URN]time.Time{},
		entries: lru.NewTTL[types.URN, *LocalizedMatchStatus](
			lru.DefaultEventCacheSize, nil, lru.DefaultEventCacheTTL),
	}
	id := types.URN{Prefix: "od", Type: "match", ID: 11}
	winner := "od:competitor:42"

	olderStart := time.Now()
	newerStart := olderStart.Add(50 * time.Millisecond)

	// The NEWER fetch (match already ended, winner known) finishes first.
	if err := c.refreshOrInsertAPIItem(id, apiXML.SportEventStatus{
		CommonSportEventStatus: apiXML.CommonSportEventStatus{HomeScore: ptrOf(3.0), WinnerID: &winner},
		Status:                 "ended", MatchStatusCode: ptrOf(9),
	}, newerStart); err != nil {
		t.Fatalf("refreshOrInsertAPIItem(newer): %v", err)
	}

	// The OLDER fetch (still live, no winner yet) finishes last: rejected.
	if err := c.refreshOrInsertAPIItem(id, apiXML.SportEventStatus{
		CommonSportEventStatus: apiXML.CommonSportEventStatus{HomeScore: ptrOf(2.0)},
		Status:                 "live", MatchStatusCode: ptrOf(5),
	}, olderStart); err != nil {
		t.Fatalf("refreshOrInsertAPIItem(older): %v", err)
	}

	entry, _ := c.entries.Get(id)
	if entry.status != types.EndedEventStatus {
		t.Fatalf("status = %v, want ended (older API response must not roll back)", entry.status)
	}
	if entry.homeScore == nil || *entry.homeScore != 3 {
		t.Fatalf("homeScore = %v, want 3", entry.homeScore)
	}
	if entry.winnerID == nil || entry.winnerID.ToString() != winner {
		t.Fatalf("winnerID = %v, want %s (rollback erased the winner)", entry.winnerID, winner)
	}

	// A strictly newer fetch still lands (the cursor only advances).
	if err := c.refreshOrInsertAPIItem(id, apiXML.SportEventStatus{
		CommonSportEventStatus: apiXML.CommonSportEventStatus{HomeScore: ptrOf(4.0), WinnerID: &winner},
		Status:                 "ended", MatchStatusCode: ptrOf(9),
	}, newerStart.Add(time.Millisecond)); err != nil {
		t.Fatalf("refreshOrInsertAPIItem(newest): %v", err)
	}
	entry, _ = c.entries.Get(id)
	if entry.homeScore == nil || *entry.homeScore != 4 {
		t.Fatalf("homeScore = %v, want 4 (a newer fetch must still apply)", entry.homeScore)
	}
}
