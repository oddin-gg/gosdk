package factory

import (
	"testing"
	"time"

	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	"github.com/oddin-gg/gosdk/types"
)

// The tests below state what a session feels when the REST API misbehaves,
// end to end: real API client, caches and message factory against an HTTP
// server. Timeouts are shortened through the factory's and the breaker's
// fields; request counts pin the behaviour wherever they can, and where time
// IS the behaviour (a session must not wait once per message) the bound is a
// multiple of the message timeout far below what the behaviour it rules out
// would cost, so a loaded runner does not fail a correct build.

const nameCatalog = `<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="1" name="Match winner" groups="all">
    <outcomes><outcome id="1" name="Home"/><outcome id="2" name="Away"/></outcomes>
  </market>
</market_descriptions>`

// oddsChangeWithCatalogMarket carries one market whose name is in the
// catalog (a static market, not a per-variant fetch).
func oddsChangeWithCatalogMarket(eventNum int) *types.FeedMessage {
	m := oddsChangeFor(eventNum)
	m.Message.(*feedXML.OddsChange).Odds.Markets = []*feedXML.MarketWithOutcome{
		feedMarket(1, "", "1", "2"),
	}
	return m
}

// marketName builds the message and returns the English name of its market
// and whether it has one.
func (r *messagePathRig) marketName(t *testing.T, eventNum int) (string, bool) {
	t.Helper()
	out, err := r.factory.BuildMessage(t.Context(), oddsChangeWithCatalogMarket(eventNum))
	if err != nil {
		t.Fatalf("BuildMessage: %v", err)
	}
	mk := out.(oddsChangeImpl).markets
	if len(mk) != 1 {
		t.Fatalf("markets = %d, want 1", len(mk))
	}
	return mk[0].Name(types.EnLocale).Get()
}

// A1. A hanging API must cost a session about one message timeout, not one
// per message: the first message waits its time and trips the breaker, the
// rest go to the caches at once.
func TestAcceptance_HangingAPI_OneTimeoutNotOnePerMessage(t *testing.T) {
	const timeout = 200 * time.Millisecond
	r := newTimedMessagePathRig(t, rigTiming{messageTimeout: timeout, breakerOpenFor: time.Second})
	r.ctl.hanging.Store(true)

	wave := func(firstID int) time.Duration {
		start := time.Now()
		for id := firstID; id < firstID+50; id++ {
			if ev := r.buildEvent(t, id); ev != nil {
				t.Fatalf("event %d = %v, want nil: the API hangs", id, ev)
			}
		}
		return time.Since(start)
	}
	// Here time IS the behaviour. 50 messages that each waited their timeout
	// would take 10 s; one wait is 0.2 s. The bound sits between with a wide
	// margin on both sides. The request count below pins the same thing
	// without a clock.
	const bound = 10 * timeout

	if elapsed := wave(1000); elapsed > bound {
		t.Fatalf("50 messages took %v, want about one timeout (%v), not one per message", elapsed, timeout)
	}
	if got := r.ctl.all.Load(); got > 2 {
		t.Fatalf("API requests = %d, want at most 2", got)
	}
	if !r.api.Breaker().Open() {
		t.Fatal("breaker closed after a message waited its whole timeout")
	}

	// Once the breaker's time has passed, the next message is the probe: it
	// pays at most one more timeout, and the others go on without waiting.
	waitFor(t, "the breaker to close", func() bool { return !r.api.Breaker().Open() })
	if elapsed := wave(2000); elapsed > bound {
		t.Fatalf("a second wave of 50 messages took %v, want at most one more timeout (%v)", elapsed, timeout)
	}
	if got := r.ctl.all.Load(); got > 4 {
		t.Fatalf("API requests = %d after two waves, want at most 4", got)
	}
}

// A1 for BuildUnparsableMessage, which builds the event the same way.
func TestAcceptance_HangingAPI_UnparsableMessagesAreBoundedToo(t *testing.T) {
	const timeout = 200 * time.Millisecond
	r := newTimedMessagePathRig(t, rigTiming{messageTimeout: timeout, breakerOpenFor: time.Minute})
	r.ctl.hanging.Store(true)

	start := time.Now()
	for id := 3000; id < 3030; id++ {
		if r.factory.BuildUnparsableMessage(t.Context(), oddsChangeFor(id)) == nil {
			t.Fatal("nil unparsable message")
		}
	}
	if elapsed := time.Since(start); elapsed > 10*timeout {
		t.Fatalf("30 unparsable messages took %v, want about one timeout (%v)", elapsed, timeout)
	}
	if got := r.ctl.all.Load(); got > 2 {
		t.Fatalf("API requests = %d, want at most 2", got)
	}
}

// A2. A catalog that downloads slower than the message timeout: the first
// message goes without names, the download carries on in the background, and
// later messages have the names. The catalog is requested once, not once per
// message and not again after the first message gave up.
func TestAcceptance_CatalogSlowerThanTheTimeout_NamesComeWithTheNextMessages(t *testing.T) {
	const timeout = 200 * time.Millisecond
	r := newTimedMessagePathRig(t, rigTiming{messageTimeout: timeout, breakerOpenFor: time.Minute})
	if ev := r.buildEvent(t, 99); ev == nil {
		t.Fatal("setup: event not built from a healthy API")
	}
	c := nameCatalog
	r.ctl.catalog.Store(&c)
	r.ctl.descriptionDelay.Store(int64(3 * timeout))
	r.settle(t)
	before := r.ctl.descriptions.Load() // the event's own description requests
	opened := r.api.Breaker().Opened()

	if name, ok := r.marketName(t, 99); ok {
		t.Fatalf("first message has the name %q, want none: the catalog is slower than its timeout", name)
	}
	// The trip is checked on the opening, not on the state: an answer that
	// lands after the trip closes the breaker again, as it should.
	if !tripped(opened) {
		t.Fatal("the message that waited its whole timeout did not trip the breaker")
	}

	// The download finishes in the background and its answer closes the
	// breaker; the messages after it carry the names. No message in between
	// may wait on, or restart, the download.
	waitFor(t, "the names to arrive", func() bool {
		name, ok := r.marketName(t, 99)
		return ok && name == "Match winner"
	})
	if got := r.ctl.descriptions.Load() - before; got != 1 {
		t.Fatalf("catalog requests = %d, want 1", got)
	}
}

// A3. A 5xx outage: the first message is released as soon as the breaker
// opens (it does not sit through the flight's retry pauses), the others skip
// the API, a getter still makes all its attempts, and the session recovers
// by itself once the API answers and the breaker's time has passed.
func TestAcceptance_ServerErrors_MessagesAreReleasedOnTheBreakerAndRecover(t *testing.T) {
	r := newTimedMessagePathRig(t, rigTiming{messageTimeout: 10 * time.Second, breakerOpenFor: 2 * time.Second})
	c := nameCatalog
	r.ctl.catalog.Store(&c)
	r.ctl.failing.Store(true)

	if ev := r.buildEvent(t, 1000); ev != nil {
		t.Fatalf("event = %v, want nil while the API fails", ev)
	}
	// The message left on the first failed attempt: the flight's next
	// attempt waits half a second, so a message that sat through the retries
	// would see all three requests here. The timeout of 10 s is not what
	// released it.
	if got := r.ctl.summaries.Load(); got != 1 {
		t.Fatalf("summary requests when the first message returned = %d, want 1 (released by the breaker, not by the end of the retries)", got)
	}
	for id := 1001; id < 1030; id++ {
		if ev := r.buildEvent(t, id); ev != nil {
			t.Fatalf("event %d = %v, want nil", id, ev)
		}
	}
	// One flight's attempts, nothing for the 29 messages behind it.
	if got := r.ctl.all.Load(); got > 3 {
		t.Fatalf("API requests = %d for 30 messages, want at most 3 (one flight's attempts)", got)
	}

	// A getter is not held back: it makes its three attempts. (The flight of
	// the message has made its own three by then.)
	waitFor(t, "the message's flight to end", func() bool { return r.ctl.summaries.Load() >= 3 })
	id := types.URN{Prefix: "od", Type: "match", ID: 500}
	if _, err := r.ef.BuildMatch(t.Context(), id, []types.Locale{types.EnLocale}, nil); err == nil {
		t.Fatal("getter succeeded against a failing API")
	}
	if got := r.ctl.summaries.Load(); got != 6 {
		t.Fatalf("summary requests = %d, want 6 (3 of the message's flight, 3 of the getter)", got)
	}

	// The API recovers; after the breaker's time the events and the names
	// load again, with no help from outside.
	r.ctl.failing.Store(false)
	waitFor(t, "the event to build again", func() bool { return r.buildEvent(t, 1000) != nil })
	waitFor(t, "the names to load again", func() bool {
		name, ok := r.marketName(t, 1000)
		return ok && name == "Match winner"
	})
}

// A4. A slow but healthy API: the first message of a new match may come
// without its event, and the build goes on in the background, so later
// messages of the match carry it with no extra wait.
func TestAcceptance_SlowHealthyAPI_LaterMessagesCarryTheEvent(t *testing.T) {
	const timeout = 500 * time.Millisecond
	r := newTimedMessagePathRig(t, rigTiming{messageTimeout: timeout, breakerOpenFor: time.Minute})
	// One request is 0.3 of the timeout; the event needs several of them.
	r.ctl.delay.Store(int64(timeout * 3 / 10))

	if ev := r.buildEvent(t, 99); ev != nil {
		t.Fatal("the event built within one timeout; the scenario needs a slower API")
	}
	waitFor(t, "the event to appear", func() bool { return r.buildEvent(t, 99) != nil })

	// Built: the next message needs no request and does not wait. A message
	// that waited would run to the timeout, so half of it is far from both.
	before := r.ctl.all.Load()
	start := time.Now()
	if ev := r.buildEvent(t, 99); ev == nil {
		t.Fatal("event missing from a message after the build finished")
	}
	if elapsed := time.Since(start); elapsed > timeout/2 {
		t.Fatalf("a message of a built match waited %v", elapsed)
	}
	if got := r.ctl.all.Load(); got != before {
		t.Fatalf("a message of a built match made %d request(s)", got-before)
	}
	// The match summary was asked for once: the messages in between joined
	// the build instead of each starting its own.
	if got := r.ctl.summaries.Load(); got != 1 {
		t.Fatalf("match summary requests = %d, want 1", got)
	}
}
