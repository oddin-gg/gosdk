package factory

import (
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/internal/apipolicy"
	"github.com/oddin-gg/gosdk/internal/cache"
	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	"github.com/oddin-gg/gosdk/types"
)

// A player-props market whose player lookup gave no answer for the message
// (skipped while the endpoint's breaker is open, or a wait that ran out) comes
// without its name; it does not carry the raw player URN as if it were the
// name. An answer of the API (the player does not exist) keeps the raw value.
func TestMessagePath_PlayerPropsName_IsAbsentWhenTheLookupGaveNoAnswer(t *testing.T) {
	setup := func(t *testing.T, tm rigTiming) *messagePathRig {
		t.Helper()
		r := newTimedMessagePathRig(t, tm)
		c := playerPropsCatalog
		r.ctl.catalog.Store(&c)
		if ev := r.buildEvent(t, 99); ev == nil {
			t.Fatal("setup: event not built from a healthy API")
		}
		// Load the catalog with a props market whose specifier is no player.
		m := oddsChangeFor(99)
		m.Message.(*feedXML.OddsChange).Odds.Markets = []*feedXML.MarketWithOutcome{
			feedMarket(7, "player=od:competitor:1", "1"),
		}
		if _, err := r.factory.BuildMessage(t.Context(), m); err != nil {
			t.Fatal(err)
		}
		r.settle(t)
		return r
	}
	name := func(t *testing.T, r *messagePathRig) (string, bool) {
		t.Helper()
		out, err := r.factory.BuildMessage(t.Context(), oddsChangeWithPlayerProps(99))
		if err != nil {
			t.Fatal(err)
		}
		mk := out.(oddsChangeImpl).markets
		if len(mk) != 1 {
			t.Fatalf("markets = %d, want 1", len(mk))
		}
		return mk[0].Name(types.EnLocale).Get()
	}

	t.Run("skipped by the breaker", func(t *testing.T) {
		r := setup(t, rigTiming{messageTimeout: apipolicy.MessageTimeout, breakerOpenFor: time.Minute})
		r.api.Breaker().Trip(apipolicy.EndpointPlayer)
		if got, ok := name(t, r); ok {
			t.Fatalf("name = %q, want none: the player lookup was skipped", got)
		}
	})

	t.Run("wait ran out", func(t *testing.T) {
		r := setup(t, rigTiming{messageTimeout: 150 * time.Millisecond, breakerOpenFor: time.Minute})
		hang := "/players/"
		r.ctl.hangPath.Store(&hang)
		if got, ok := name(t, r); ok {
			t.Fatalf("name = %q, want none: the player lookup timed out", got)
		}
	})

	t.Run("not found keeps the raw value", func(t *testing.T) {
		r := setup(t, rigTiming{messageTimeout: apipolicy.MessageTimeout, breakerOpenFor: time.Minute})
		r.ctl.notFoundPlayers.Store(true)
		if got, ok := name(t, r); !ok || got != "od:player:301 kills" {
			t.Fatalf("name = %q (%v), want the raw value for a player the API does not know", got, ok)
		}
	})
}

// A player lookup that is already older than a message waits is not waited
// for: the message gets ErrFlightTooOld from it, and the market comes without
// its name, not with the raw player id.
func TestMessagePath_PlayerPropsName_IsAbsentWhenTheLookupIsOlderThanTheMessageTimeout(t *testing.T) {
	const timeout = 150 * time.Millisecond
	r := newTimedMessagePathRig(t, rigTiming{messageTimeout: timeout, breakerOpenFor: time.Minute})
	c := playerPropsCatalog
	r.ctl.catalog.Store(&c)
	if ev := r.buildEvent(t, 99); ev == nil {
		t.Fatal("setup: event not built from a healthy API")
	}
	// Load the catalog with a props market whose specifier is no player.
	m := oddsChangeFor(99)
	m.Message.(*feedXML.OddsChange).Odds.Markets = []*feedXML.MarketWithOutcome{
		feedMarket(7, "player=od:competitor:1", "1"),
	}
	if _, err := r.factory.BuildMessage(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	r.settle(t)

	hang := "/players/"
	r.ctl.hangPath.Store(&hang)
	// A getter holds the player's flight, which hangs.
	go func() {
		_, _ = r.cm.PlayersCache.GetPlayer(t.Context(), cache.PlayerCacheKey{PlayerID: "od:player:301", Locale: types.EnLocale})
	}()
	waitFor(t, "the player request to reach the server", func() bool { return r.ctl.players.Load() >= 1 })
	// The flight is now older than a message waits.
	time.Sleep(2 * timeout)

	start := time.Now()
	out, err := r.factory.BuildMessage(t.Context(), oddsChangeWithPlayerProps(99))
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > timeout {
		t.Fatalf("the message waited %v on a lookup that is older than its timeout %v", elapsed, timeout)
	}
	mk := out.(oddsChangeImpl).markets
	if len(mk) != 1 {
		t.Fatalf("markets = %d, want 1", len(mk))
	}
	if got, ok := mk[0].Name(types.EnLocale).Get(); ok {
		t.Fatalf("name = %q, want none: the lookup was older than the message timeout", got)
	}
}
