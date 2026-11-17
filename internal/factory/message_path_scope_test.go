package factory

import (
	"fmt"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/internal/apipolicy"
	"github.com/oddin-gg/gosdk/internal/cache/lru"
	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	"github.com/oddin-gg/gosdk/types"
)

// The breaker's scope follows how failures happen. One match whose summary
// keeps answering 500 is a failure of that key: the other matches of the
// client are not held back by it, and its own messages do not each wait for
// the retries of the flight that failed.
func TestMessagePath_OneKeyWithAServerError_DoesNotHoldBackOtherMatches(t *testing.T) {
	defer lru.SetFailureMemory(250 * time.Millisecond)()
	r := newMessagePathRig(t)
	const badID = 1000
	r.ctl.badSummary.Store(types.URN{Prefix: "od", Type: "match", ID: badID}.ToString(), true)

	// The messages of the bad match arrive all the time, and each time the
	// memory of its failure ends the next one fails again. New matches come
	// between them and must get their event every time.
	for round := range 6 {
		start := time.Now()
		if ev := r.buildEvent(t, badID); ev != nil {
			t.Fatalf("round %d: the bad match has an event", round)
		}
		// A message that sat through the flight's retries (0.5 s and more
		// of backoff) would take far longer than this.
		if elapsed := time.Since(start); elapsed > 400*time.Millisecond {
			t.Fatalf("round %d: a message of the bad match waited %v, it must not sit through the flight's retries", round, elapsed)
		}
		if anyOpen(r.api.Breaker()) {
			t.Fatalf("round %d: a server error of one match opened a breaker", round)
		}
		id := 2000 + round
		if ev := r.buildEvent(t, id); ev == nil {
			t.Fatalf("round %d: new match %d came without its event: one bad match must not hold back the others", round, id)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// The first message of the bad match leaves at the first failed attempt, and
// the messages after it make no request while its failure is remembered.
func TestMessagePath_OneKeyWithAServerError_IsAskedOncePerMemory(t *testing.T) {
	r := newMessagePathRig(t) // memory: the default 10 s, far longer than the test
	const badID = 1000
	r.ctl.badSummary.Store(types.URN{Prefix: "od", Type: "match", ID: badID}.ToString(), true)

	if ev := r.buildEvent(t, badID); ev != nil {
		t.Fatal("the bad match has an event")
	}
	if got := r.ctl.summaries.Load(); got != 1 {
		t.Fatalf("summary requests when the first message returned = %d, want 1 (released at the first failed attempt)", got)
	}
	// The flight retries on, up to three requests, and then the key is
	// remembered.
	waitFor(t, "the flight's attempts to end", func() bool { return r.ctl.summaries.Load() >= 3 })
	settled := r.ctl.summaries.Load()
	waitFor(t, "the failure to be remembered", func() bool {
		start := time.Now()
		r.buildEvent(t, badID)
		return time.Since(start) < 100*time.Millisecond && r.ctl.summaries.Load() == settled
	})
	for range 20 {
		r.buildEvent(t, badID)
	}
	if got := r.ctl.summaries.Load(); got != settled {
		t.Fatalf("summary requests grew from %d to %d while the key was remembered", settled, got)
	}
	// A getter is never skipped.
	if _, err := r.ef.BuildMatch(t.Context(), types.URN{Prefix: "od", Type: "match", ID: badID}, []types.Locale{types.EnLocale}, nil); err == nil {
		t.Fatal("the getter succeeded for the bad match")
	}
	if got := r.ctl.summaries.Load(); got <= settled {
		t.Fatalf("the getter made no request (%d, was %d)", got, settled)
	}
}

// One endpoint hangs while the others answer. The first message that waits
// its whole time on it trips the breaker of that endpoint; healthy answers
// of the other endpoints do not close it, so new keys on the hung endpoint
// are skipped at once for the breaker's time, and the other endpoints keep
// loading.
func TestMessagePath_OneEndpointHangs_NewKeysAreSkippedWhileOthersKeepLoading(t *testing.T) {
	const timeout = 300 * time.Millisecond
	r := newTimedMessagePathRig(t, rigTiming{messageTimeout: timeout, breakerOpenFor: time.Minute})
	c := playerPropsCatalog
	r.ctl.catalog.Store(&c)
	if ev := r.buildEvent(t, 99); ev == nil {
		t.Fatal("setup: event not built from a healthy API")
	}
	r.settle(t)
	hang := "/players/"
	r.ctl.hangPath.Store(&hang)

	playerMessage := func(player int) *types.FeedMessage {
		m := oddsChangeFor(99)
		m.Message.(*feedXML.OddsChange).Odds.Markets = []*feedXML.MarketWithOutcome{
			feedMarket(7, fmt.Sprintf("player=od:player:%d", player), "1"),
		}
		return m
	}
	build := func(player int) ([]types.MarketWithOdds, time.Duration) {
		start := time.Now()
		out, err := r.factory.BuildMessage(t.Context(), playerMessage(player))
		if err != nil {
			t.Fatalf("BuildMessage: %v", err)
		}
		return out.(oddsChangeImpl).markets, time.Since(start)
	}

	// The first message waits its whole time on the player profile.
	if _, elapsed := build(301); elapsed < timeout {
		t.Fatalf("the first message returned after %v, before its timeout %v: the player profile hangs", elapsed, timeout)
	}
	if !r.api.Breaker().Open(apipolicy.EndpointPlayer) {
		t.Fatal("the message that waited its whole time on the player profile did not trip its breaker")
	}
	for ep := apipolicy.Endpoint(0); ep < apipolicy.EndpointCount; ep++ {
		if ep != apipolicy.EndpointPlayer && r.api.Breaker().Open(ep) {
			t.Fatalf("the breaker of %v opened, the message waited on the player profile only", ep)
		}
	}
	requests := r.ctl.players.Load()

	// New players are skipped at once, however many healthy answers of the
	// other endpoints arrive between them, and new matches keep loading.
	for i := range 10 {
		if ev := r.buildEvent(t, 3000+i); ev == nil {
			t.Fatalf("new match %d came without its event while only the player profile hangs", 3000+i)
		}
		if _, elapsed := build(302 + i); elapsed > timeout/2 {
			t.Fatalf("a message with a new player waited %v (timeout %v): the player profile's breaker is open and must skip it", elapsed, timeout)
		}
		if !r.api.Breaker().Open(apipolicy.EndpointPlayer) {
			t.Fatalf("round %d: healthy answers of other endpoints closed the player profile's breaker", i)
		}
	}
	if got := r.ctl.players.Load(); got != requests {
		t.Fatalf("player requests grew from %d to %d while its breaker was open", requests, got)
	}
}
