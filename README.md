Oddin.gg Golang SDK
-------------------

Go SDK for Oddin.gg's REST API and streaming odds feed.

### Installing

```shell
go get github.com/oddin-gg/gosdk
```

Requires Go 1.26+.

### Quickstart

```go
package main

import (
    "context"
    "log"
    "log/slog"
    "os"
    "os/signal"
    "syscall"
    "time"

    "github.com/oddin-gg/gosdk"
    "github.com/oddin-gg/gosdk/types"
)

func main() {
    cfg := gosdk.NewConfig(os.Getenv("TOKEN"), types.IntegrationEnvironment,
        gosdk.WithLogger(slog.Default()),
        gosdk.WithDefaultLocale(types.EnLocale),
    )

    bootCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()

    client, err := gosdk.New(bootCtx, cfg)
    if err != nil {
        log.Fatalf("gosdk.New: %v", err)
    }
    defer func() {
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()
        _ = client.Close(ctx)
    }()

    // Subscribe's ctx bounds SETUP only (lazy-connect dial, queue
    // topology) — bound it; it does not govern the subscription's
    // lifetime.
    subCtx, cancelSub := context.WithTimeout(context.Background(), 30*time.Second)
    sub, err := client.Subscribe(subCtx,
        gosdk.WithMessageInterest(types.AllMessageInterest),
    )
    cancelSub()
    if err != nil {
        log.Fatalf("subscribe: %v", err)
    }

    go func() {
        // types.SessionMessage is a tagged union — exactly one variant
        // field is non-nil per parsed message.
        for msg := range sub.Messages() {
            switch {
            case msg.OddsChange != nil:
                log.Printf("odds change: %d markets", len(msg.OddsChange.Markets()))
            case msg.BetSettlement != nil:
                log.Printf("bet settlement: %d markets", len(msg.BetSettlement.Markets()))
            }
        }
    }()

    sigCh := make(chan os.Signal, 1)
    signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
    <-sigCh

    // Graceful drain BEFORE the deferred client.Close: client.Close is
    // abrupt for live subscriptions. sub.Close waits (bounded) until
    // the consumer goroutine above has read every admitted message —
    // its Messages() channel then closes, ending the goroutine.
    drainCtx, cancelDrain := context.WithTimeout(context.Background(), 30*time.Second)
    _ = sub.Close(drainCtx)
    cancelDrain()
}
```

### Configuration

`gosdk.NewConfig` takes the access token, an `Environment`, and any
number of functional options. Common options:

```go
gosdk.NewConfig(token, types.IntegrationEnvironment,
    gosdk.WithRegion(types.RegionDefault),
    gosdk.WithNodeID(1),
    gosdk.WithDefaultLocale(types.EnLocale),
    gosdk.WithPreloadLocales(types.EnLocale, types.RuLocale),
    gosdk.WithMaxInactivity(20*time.Second),
    gosdk.WithMaxRecoveryExecution(6*time.Hour),
    gosdk.WithStaleMessageLimit(2*time.Minute),
    gosdk.WithStaleMessageWindow(time.Minute),
    gosdk.WithShutdownTimeout(5*time.Second),
    gosdk.WithLogger(slog.Default()),
    gosdk.WithAPICallLogging(gosdk.APILogMetadata),
)
```

The full option list is in [config.go](config.go).

### Lifecycle

- `gosdk.New(ctx, cfg)` validates credentials with a bookmaker-details
  probe and sets up the API + cache + producer layer. **Does not** open
  AMQP.
- `client.Connect(ctx)` opens AMQP eagerly (optional).
- `client.Subscribe(ctx, opts...)` returns a `*Subscription`. First
  call lazy-connects if `Connect` wasn't called.
- `client.Close(ctx)` is idempotent. The supplied ctx caps how long the
  caller waits; the shutdown work itself is bounded by
  `WithShutdownTimeout` (default 5s). For graceful drain, call
  `sub.Close(drainCtx)` on each subscription **before**
  `client.Close(ctx)` — `client.Close` is abrupt for active
  subscriptions.

### Catalog API

Entity types are pure-data value structs — methods are pure field
reads, no errors. (Three exceptions remain interfaces:
`types.Producer`, whose accessors read the producer's LIVE state —
enabled/flagged-down/timestamps track the catalog as it changes;
`types.BookmakerDetail`; and `types.FixtureChange`, a legacy
immutable interface whose accessors return fixed values — despite the
"live manager-owned state" framing in older docs. Note `types.FixtureChange`
is a plain entity interface and is NOT the feed message payload; the
`fixture_change` feed message is `types.FixtureChangeMessage`.)

```go
match, err := client.Match(ctx, eventURN)
log.Println(match.Name(types.EnLocale))    // localized name
log.Println(match.Tournament.Name(types.EnLocale))
if match.HomeCompetitor != nil {
    log.Println(match.HomeCompetitor.Name(types.EnLocale))
}
log.Println(match.Status.Status)                // EventStatus
```

Catalog methods (singular and plural; locales are variadic — first
locale is primary, additional locales preload the cache):

```go
sport, _    := client.Sport(ctx, sportURN, types.EnLocale, types.RuLocale)
sports, _   := client.Sports(ctx, types.EnLocale)
match, _    := client.Match(ctx, eventURN)
player, _   := client.Player(ctx, playerURN)
desc, _     := client.MarketDescription(ctx, marketID, variant, types.EnLocale)
voids, _    := client.MarketVoidReasons(ctx)
status, _   := client.Replay().Status(ctx)  // replay-engine state

// Cache invalidation (no ctx — pure state):
client.ClearMatch(eventURN)
client.ClearPlayer(playerURN)
client.ClearSport(sportURN)
client.ClearMarketVoidReasons()

// Polling fallback for lossy event channels:
status, ok := client.ProducerStatus(producerID)  // current state, even if event missed
state := client.ConnectionState()                 // Connected / Closed / etc.
```

### Per-message locale lookups

Markets and outcomes carry per-locale name maps populated for every
locale in `WithPreloadLocales(...)`. Reading a name back off a decoded
message is an O(1) map lookup and does no I/O:

```go
cfg := gosdk.NewConfig(token, env,
    gosdk.WithPreloadLocales(types.EnLocale, types.RuLocale))

for msg := range sub.Messages() {
    if msg.OddsChange != nil {
        for _, m := range msg.OddsChange.Markets() {
            log.Println(m.Name(types.EnLocale).ValueOr(""))  // O(1) cached
            log.Println(m.Name(types.RuLocale).ValueOr(""))  // O(1) cached
            // m.Name(types.DeLocale) → None (not preloaded)
        }
    }
}
```

Filling those maps is the part that costs: at message-construction time
the SDK looks the market up in the description cache — once per market
for English, twice per market for every other configured locale, since
the outcome lookup also asks for the English catalog label that the
home/away substitution keys on — then reads a name off the cached entry
for the market and for each outcome, and does I/O when the cache is cold
(an unseen market, variant or player). A consumer that takes its names
from the catalog API (`Client.MarketDescription`) can skip the work:

```go
cfg := gosdk.NewConfig(token, env,
    gosdk.WithMessageNameResolution(false))
// m.Name(locale) / o.Name(locale) → None for every locale.
// Ids, specifiers, odds, status and settlement results are unaffected.
```

Only **market and outcome** names are affected. Event, tournament, sport
and competitor names travel on the same message, come from their own
caches, and resolve either way.

Note that the catalog API is **not a drop-in replacement** for the message
names. The message names are *composed*: `{specifier}` placeholders in the
catalog template are filled from the market's specifiers, a home/away
specifier value and the home/away placeholder outcomes are replaced with
the event's localized competitor names, and player-props entities resolve
to player names. `Client.MarketDescription` returns the raw template with
none of that applied. Opt out only when you
need no market/outcome display names at all, or are prepared to compose
them yourself. The SDK logs one Info line at `Client.New` when the option
is off, so an unexpected `None` later has a findable cause.

### When the REST API is down

Building the event of a feed message may need the REST API (a match, its
tournament, competitors and fixture that are not cached yet). That happens on
the session goroutine, so a message never waits for the API longer than **2 s
in total**: the whole build, including market and outcome name resolution,
waits under that one timeout. When it runs out the message is delivered with
what the caches have, a nil event or nameless markets. The public entity reads
(`Client.Match`, ...) are not affected; they wait for the API and retry.

**The requests keep going.** A load a message started is shared with everyone
who needs the same entity and runs with the getters' retries and timeouts,
detached from the message that started it. A message stops waiting for it
after its own 2 s, or sooner (see below), but the load carries on and fills
the cache, so later messages and getters find the entity there. A load that
has already run longer than the message timeout is not waited for at all:
when one endpoint hangs while others answer, each message that needs it waits
at most one timeout counted from the start of that load, not one timeout of
its own.

**One breaker per endpoint.** The client keeps a breaker for each kind of
resource the message path loads: match summary, fixture, competitor profile,
player profile, tournament info, market descriptions, variant descriptions and
the static descriptions (sports, match status, void reasons). A breaker opens
for 10 seconds when the API gives no answer: a transport error, a timeout after
the request was sent, a response body that the transport cut off, a 429 (a
rate limit refuses to answer for the whole client), or a message that waited
its whole 2 s on that endpoint (the endpoint the message was waiting on, not
the others). An answer is about one resource, so only a missing answer closes
the endpoint: a response that arrived complete never opens it, whatever it says
(a 500, 502, 503 or 504, a `SERVICE_UNAVAILABLE` envelope, a body that cannot be
decoded, a response about another resource, a 404 for an event that is not in
the API yet, any other 4xx). Nor does a cancelled context or a closed client. A
healthy answer closes the breaker of its own endpoint at once; healthy answers
of other endpoints do not. Otherwise it closes after the 10 seconds, and the
next message that needs the endpoint starts the load that tells whether it is
back. Only the message path looks at the breakers; getters never do (their
answers still feed them).

The price of this rule is an outage that answers fast. When the API, or a
gateway in front of it, returns a complete 502 or 503 to every request, no
breaker opens. Each key that a message needs and that has not failed in the
last 10 seconds then starts its own load, and the load makes up to 3 attempts:
N new keys give about 3N requests to an API that is already failing, and each
such message waits for one round trip before it goes on without the data. An
outage that gives no answer at all (timeouts, refused connections) still opens
the breaker.

**One failing key is remembered, not the whole client.** When the request of
one key (one match, one player, ...) is answered with an error (any of the
answers listed above except a 404), a message does not ask for that key again
for 10 seconds, and a message that waits on the
load stops waiting at the first failed attempt instead of sitting through its
retries. The load goes on retrying in the background and fills the cache if it
succeeds; getters are never held back by it, and a successful load forgets the
key. The other keys of the client, and the other messages of that match, are
not affected.

While the breaker of an endpoint is open the message path asks the caches
only for what that endpoint serves. A message that is waiting on a load from
it when the breaker opens stops waiting at once. An event that is fully cached
is delivered with its event; any other event, and any market or outcome name
that is not cached, comes without (a player-props market whose player lookup
was skipped or timed out has no name, not the raw player id). Cached market
catalog rows are served, stale ones included.

**A stale catalog is served while it refreshes.** When the market catalog of
a locale has passed its 12 hour age, a message reads the old rows at once and
the refresh runs in the background (without it, or after a clear, a message
waits at most its 2 s for the first download). Renamed markets appear one
download later. Public reads keep waiting for the refresh.

**A healthy but slow API.** A cold event build is several sequential requests
(the summary, the tournament, each competitor, the fixture, and the market
catalog for every locale). With requests slower than roughly 250 ms the first
messages of a new match may therefore come without the event; the loads go
on, and the later messages of that match carry it with no extra wait. Add
`WithPreloadLocales(...)` so the market catalog is loaded up front and does not
take part in that build.

### Delivery guarantees

The SDK consumes with manual acknowledgement and acks a delivery when its
decoded message has been placed into the subscription's `Messages()`
buffer (`WithSubscriptionBuffer`, default 256). Broker prefetch
(`WithAMQPPrefetch`, default 1000) therefore bounds what a slow consumer
can hold unacked in process — not queue depth: a consumer that stops
reading leaves its exclusive queue accumulating on the broker for as long
as the subscription stays open, so close a subscription you cannot drain
rather than stalling it. The boundary also means that if the process
dies, whatever sits **unread in that buffer is gone**: it was acked and
the queue is exclusive and auto-delete, so nothing is redelivered. Size
the buffer for how much you can afford to lose on a crash, and read
`Messages()` promptly.

Gaps are closed by recovery, not by the broker. When a consumer channel
is lost — with the whole AMQP connection or alone — its queue dies with
it and every message published until the SDK re-binds is lost from the
broker. At that moment the SDK flags every known producer that
subscription served down (`ConnectionDownProducerStatusReason`) and, once
the subscription has re-bound its queue, the next alive of the producer
starts a snapshot recovery that reaches back to what that subscription had
finished (its checkpoint: the newest live message or subscribed alive it
handed to the client and settled). Watch
`RecoveryEvents()` / `ProducerStatus()` for the down → up cycle.
A producer status is also published when only its `IsDelayed` flag flips (a
delay starting or ending), and a repeated down status with the same reason is
published too, so do not count statuses as up/down transitions: compare with
the previous status you saw.

### Recovery

```go
handle, err := client.RecoverEventOdds(ctx, producerID, eventURN)
if err != nil {
    return err // recovery was not accepted; handle is nil
}
<-handle.Done()
res := handle.Result()
if res.Status == types.RecoveryStatusCompleted { ... }
```

The handle is reliable — even if the lossy `RecoveryEvents()` channel
drops the event, `Done()` unblocks correctly.

One recovery actor runs every producer's recovery state. The rules, per producer:

- **What a recovery asks for.** A subscription tracks, per producer, a
  checkpoint (see above) and the gaps it may be missing: a lost queue opens one
  from its checkpoint, a producer that stops sending (an alive that says the
  feed is unsubscribed, or no alive for `WithMaxInactivity`) opens one from the
  producer's last subscribed alive. A recovery starts from the oldest open gap,
  clamped to the producer's stateful recovery window (the SDK logs a WARN with
  what it could not ask for), and a gap stays open until a recovery that covers
  it completes. `Producer.TimestampForRecovery()` is that oldest point (or the
  oldest checkpoint when nothing is open): the time a client that restarts
  resumes from. The SDK acks a message when it enters the subscription's buffer
  (`WithSubscriptionBuffer`, default 256), and the point counts it from then on,
  read or not. After a clean close in which the subscriptions were drained,
  resuming from the point misses nothing. When the process ends without draining,
  the messages still in a subscription's buffer are lost, up to the buffer's size
  per subscription, and the point does not cover them: they lie before it (see
  Delivery guarantees).
- **The start the client sets.** `SetProducerRecoveryFromTimestamp` before `Connect`
  is the seed of a producer's first recovery, and the zero time clears it (a full
  snapshot). After `Connect` the zero time does nothing: what was set before it stays
  the seed. Until the producer has completed a first recovery its start is the seed,
  and a value the client sets after `Connect` replaces it, whether it is earlier or
  later (a value the seed already has does nothing; a lane whose queue was lost starts
  from the lost queue's checkpoint if that is earlier; a recovery already in flight
  keeps what it asked for, and one more is asked for from the new value). Once the
  producer has a point of its own, an earlier value only lowers where the next
  recovery starts and opens no request, the point it is at does nothing, and a later
  one is ignored, with a WARN.
- **When it completes.** A recovery completes once every subscription that
  receives the producer has taken its `snapshot_complete`; a gap opened
  meanwhile asks for one more, and the producer comes up when none is open. A
  subscription of the `SystemAliveOnly` interest has no lane and is not waited
  for. A producer no subscription receives has nothing to recover: it comes up
  on a fresh alive.
- **When it fails.** An accepted recovery fails when no `snapshot_complete`
  arrives within `WithSnapshotCompleteDeadline` (default 5 minutes) of the last
  sign it is on its way (a snapshot message a subscription that awaits it
  finished, or a message queued ahead of it), or within `WithMaxRecoveryExecution`
  (default 6 h) of being asked for. A subscription whose client callback has
  been in one message for longer than `WithMaxInactivity` counts as having heard
  something, so the deadline does not fire on a stuck callback. A failed request
  (the API refuses it, or it times out) is asked for again after 5, 10 and 20
  seconds; after three re-issues the producer stays down with
  `OtherProducerStatusReason` and the request is repeated every 60 seconds until
  it succeeds or an alive arrives after the producer was silent, which starts the
  backoff again. A new request id is used for each, and a late
  `snapshot_complete` of an abandoned one is ignored.
- **Event recoveries** (`RecoverEventOdds`, `RecoverEventStateful`) run beside the
  producer's recovery. Each ends when every subscription that receives the
  producer has taken its `snapshot_complete`; it fails when the API refuses it,
  when the queue it waits for is lost (the connection, or a subscription's
  channel), and when nothing of it is heard for `WithSnapshotCompleteDeadline`
  after the API accepted it. A producer going down for any other reason (a
  processing delay, an alive interval, an unsubscribed alive) does not end it. One asked for while a subscription
  is waiting for its queue to be bound waits for it. At most 128 per producer are
  pending; an identical request while one is in flight is that one.
- **The safety net.** A subscription whose live messages stay older than
  `WithStaleMessageLimit` (default 2 minutes, by the producer's clock, measured
  against the offset taken from its alives) for `WithStaleMessageWindow` (default
  1 minute) is too far behind: the broker's queue for it holds a backlog that
  only grows. The SDK first asks for a recovery of every producer the
  subscription receives, from the subscription's checkpoint, and once the API has
  accepted them all it replaces the subscription's AMQP channel, which drops the
  queue and what it held. The recoveries are then asked for again, as after a lost
  queue, so nothing is missing afterwards. The producers the subscription receives
  are down with `ProcessingQueueDelayViolationProducerStatusReason` while this
  runs, and the subscription's `snapshot_complete`s that arrive meanwhile are
  ignored, since they come from the queue being replaced. A reset drops what the
  broker had delivered ahead of the client but not yet handed over, up to
  `WithAMQPPrefetch` messages (default 1000), and what is still in the
  subscription's buffer is delivered all the same: the recovery repeats those, so a
  client may see a message twice, never a hole. A subscription is reset at most
  three times in ten minutes, with 1, 2 and 4 minutes between the resets; after
  that it is only logged as lagging (a WARN) until it catches up or the first reset
  ages out, and the processing delay below keeps its producers down for as long as
  it is late. A request of the net's that the API refuses, a producer that is
  disabled in the meantime, and a `snapshot_complete` of the subscription's own for
  one of its recoveries (it has what the recovery sent) cancel the reset; the net
  then waits before it tries again, 1, 2 or 4 minutes by the number of resets it
  has made in the last ten minutes, as between resets. It does not reset a
  subscription whose backlog reaches past the producer's stateful recovery window
  (its checkpoint of any enabled producer it receives is older than the start a
  recovery could have, or lies within ten minutes of it, at most half the window,
  since the window moves on before the recovery asked into the new queue): the
  recovery could not repeat what the reset would drop, so the subscription is
  logged as lagging (one WARN) and nothing is asked, until it catches up. The
  margin is a margin, not a proof. The net is off while the offset of the
  producer's clock is stale, and paused for a producer while a recovery of it is in
  flight or a gap is open, since a snapshot ahead of a live message makes it old on
  every subscription. It acts on a backlog old enough to reach the limit: a broker
  queue length limit that drops the oldest messages first can keep the age under
  it, so size such a limit against `WithAMQPPrefetch` and the limit.
- **Processing delay.** Each subscription is judged on its own: a subscription
  that handles messages more than `WithMaxInactivity` after the producer
  generated them, or takes nothing for that long while alives flow, takes the
  producer down with `ProcessingQueueDelayViolationProducerStatusReason`, without
  a recovery; the producer comes back when every subscription is on time.
- **The clock.** The actor looks at the clock once a second, so deadlines and
  backoffs are exact to about a second; a producer that sends no alive within
  `WithMaxInactivity` of the feed opening is reported down with
  `AliveIntervalViolationProducerStatusReason`. Durations run on the monotonic
  clock, so a step of the system clock fires no deadline and holds none back.

The event-recovery handle is reliable — even if the lossy `RecoveryEvents()`
channel drops the event, `Done()` unblocks correctly — and a finished handle can
be read through `EventRecoveryStatus` for five minutes.

### Recovery behaviour changes

What a client that relied on the earlier behaviour will see differently:

- **Completion needs every subscription.** Was: the first `snapshot_complete` of the
  request (one per scope for a live-only or prematch-only subscription) completed
  it, and a client's `SystemAliveOnly` subscription could complete it alone. Is:
  every subscription that receives the producer must have taken its
  `snapshot_complete`; a `SystemAliveOnly` subscription takes none. A slow
  subscription next to a fast one keeps the producer down longer, and a producer
  of both scopes with only a live-only subscription completes.
- **A recovery starts when a subscription exists.** Was: the first alive after
  `Connect` asked for a recovery. Is: a producer no subscription receives comes up
  on a fresh alive with no recovery, and the first `Subscribe` that covers it takes
  it down (`OtherProducerStatusReason`) until its first recovery completes.
- **Lookback is the API window.** Was: `WithMaxRecoveryExecution` (6 h by default)
  bounded how far back a recovery reached. Is: a recovery starts from the oldest
  point a subscription may be missing, clamped to the producer's stateful recovery
  window, with a WARN when it must start later. After a long outage the replay can
  be as large as that window. `WithMaxRecoveryExecution` bounds the time of one
  request.
- **Failed recoveries back off.** Was: a failed request was asked for again at
  every alive, with no limit and no change of status. Is: 5, 10 and 20 seconds,
  three re-issues, then a retry every 60 seconds with the producer down
  (`OtherProducerStatusReason`).
- **An accepted event recovery can time out in silence.** Was: it waited for its
  `snapshot_complete` up to `WithMaxRecoveryExecution`. Is: it fails after
  `WithSnapshotCompleteDeadline` with nothing heard of it.
- **Event recoveries survive a down that loses no queue.** Was: any producer down
  failed every pending event recovery of the producer. Is: only the loss of a
  queue (the connection, a subscription's channel) does; a processing delay, an
  alive interval or an unsubscribed alive leaves them.
- **An event recovery asked for while a subscription awaits its channel waits for
  it.** Was: it was sent at once, and its replay could go to a queue that did not
  exist yet.
- **The resume point is published at the ack.** Was: `TimestampForRecovery()` was
  the last system alive's time, which skips what was queued but not yet handled.
  Is: the oldest point a subscription may still be missing, from the settled
  deliveries.
- **A subscription that stays far behind is reset.** Was: nothing bounded how far
  a subscription could fall behind; its queue grew on the broker until the
  broker dropped messages or the queue was lost. Is: see the safety net above
  (`WithStaleMessageLimit`, `WithStaleMessageWindow`). Up to `WithAMQPPrefetch`
  prefetched messages (1000 by default) are dropped with the queue at a reset, and
  the recovery repeats them.
- **Processing delay per subscription, and no recovery for it.** Was: one
  producer-wide maximum, so a slow subscription next to a fast one went unseen. Is:
  each subscription is judged on its own.
- **The loss of the SDK's own alive channel alone opens no gap.** Was: it counted
  as a loss for every producer, with a recovery. Is: a connection drop still loses
  every subscription's channel, and that is what recovers.
- **First status without a subscription.** Was: the first alive after `Connect`
  asked for a recovery and the producer came up when it completed. Is: a producer no
  subscription receives is reported up with `FirstRecoveryCompletedProducerStatusReason`
  on a fresh alive, with no recovery; the first `Subscribe` that covers it takes it
  down (`OtherProducerStatusReason`), and its real first recovery ends with
  `ReturnedFromInactivityProducerStatusReason`.
- **An event recovery with no subscription completes at the API's acceptance.** Was:
  it waited for a `snapshot_complete` no subscription could take. Is: with no
  subscription receiving the producer there is nothing to wait for; it completes when
  the API accepts it, and the stream tells of it. With subscriptions, every subscription that receives
  the producer must take its `snapshot_complete`.
- **`Producer.RecoveryInfo()` also tells a failure.** Was: set only when a request
  was accepted. Is: set when a request is accepted and when it fails (refused, or
  timed out), with `Successful() == false` for a failure. A request the close aborted
  tells nothing.
- **Statuses when a delay meets another state.** A delay that starts or ends on a
  producer that is already down changes only `IsDelayed` (a status with the same
  reason is published, as before). A recovery that completes while a subscription
  lags takes the producer to down for the delay at once, with no "up, delayed" status
  before it. A producer that sends only unsubscribed alives stays down as
  unsubscribed (`OtherProducerStatusReason`): it is alive, so it is not reported as
  silent (`AliveIntervalViolationProducerStatusReason`).
- **Admission of an event recovery.** A request is refused when the SDK's queue of
  event recovery requests (10 000) is full: the call fails at once, the request was not
  accepted, and it can be retried. A request the API answers without accepting and
  without an error fails the handle.
- **Log lines.** Was: a failing recovery API logged at ERROR with `producer_id` and
  `request_id` fields. Is: one WARN for the first failure of a streak and the next at
  DEBUG (the 60 s retry's first WARN too), and the producer and request ids are in
  the message text, not fields.
- **The tick is a second.** Was: ten seconds, with a 60 s warm-up before the
  alive-inactivity check. Is: a second, and the inactivity check counts from the
  opening of the feed.

### Memory bound

The per-entity caches are size-capped LRUs with a 12 h TTL; none of the
caps is configurable. At most this many entries are held:

| Cache | Entries (cap) |
|---|---|
| matches, fixtures, competitors, tournaments (one cache each) | 5 000 per cache |
| match status | 5 000 |
| players (one entry per player and locale) | 10 000 |
| dynamic market variants | 5 000 |

An entry is dropped when the cap pushes it out, when it is read after its
TTL, and — so that an idle but growing cache does not sit at its cap with
entries nobody reads any more — by a small sweep on every insert that
removes up to four expired entries from the least recently used end. The
catalogs (sports, static market descriptions, void reasons, match-status
descriptions) are small and kept whole. Memory therefore grows with the
number of distinct entities seen within the last 12 hours, never beyond the
caps above.

### Observability

Three lossy event channels plus polling counterparts:

```go
// One goroutine per channel: the channels stay open until client
// shutdown, so sequential `for range` loops would drain only the first
// one — the others overflow (the channels are lossy) and their events
// are dropped (drop-oldest, with a rate-limited slog warning).
go func() {
    for ev := range client.ConnectionEvents() { ... } // Connected/Disconnected/Reconnecting/Closed
}()
go func() {
    for ev := range client.RecoveryEvents() { ... }   // ProducerStatus + EventRecovery (switch on ev.Kind)
}()
go func() {
    for ev := range client.APIEvents() { ... }        // HTTP request/response (opt-in)
}()

state := client.ConnectionState()                     // polling getter
```

### Examples

See [examples/](examples/) for working programs:

- [examples/basic/](examples/basic/main.go) — minimal subscribe + consume
- [examples/api_only/](examples/api_only/main.go) — catalog reads without AMQP
- [examples/recovery/](examples/recovery/main.go) — RecoveryHandle usage
- [examples/multi_locale/](examples/multi_locale/main.go) — locale fill-in
- [examples/replay/](examples/replay/main.go) — replay API
- [examples/graceful/](examples/graceful/main.go) — clean shutdown

### Migration from pre-v1.0.0

[MIGRATION.md](MIGRATION.md) covers the breaking changes from the
legacy `v0.0.x` SDK: configuration via functional options, the flat
`*Client` shape (no manager-of-managers), the `Subscription` lifecycle,
and the entity reshape from interfaces to value structs.

### Design

[NEXT.md](NEXT.md) is the source-of-truth design document covering
the architecture, caching strategy, lifecycle, recovery state machine,
and observability shape. Where the shipped implementation deliberately
diverged from an original design decision, the superseded passage is
marked as historical in place — the supersession notes, MIGRATION.md,
and the package godocs describe the behaviour that actually ships.

### Schema conformance

The XML models in `internal/feed/xml` and `internal/api/xml` are written
by hand. `internal/schemacheck` keeps them honest against the
[oddsfeedschema](https://github.com/oddin-gg/oddsfeedschema) XSDs, which
stay the single source of truth: the test downloads the schema from
GitHub at test time. It is pinned to one immutable version
(`defaultSchemaRef` in `internal/schemacheck/source_test.go`, a release
tag or commit), so a schema change upstream cannot turn this build red
on its own; a newer schema is taken by a deliberate bump of that
constant in the change that updates the models. `ODDSFEEDSCHEMA_REF`
overrides the version for one run (for example `main`, to try upcoming
schema work) and `ODDSFEEDSCHEMA_DIR` uses a local checkout.
Every attribute and element the schema declares must have a Go field and
every Go tag must exist in the schema. Known deviations live in a ledger
in the test with the reason for each; a new deviation, or a ledger entry
the code has outgrown, fails `go test ./...`. Without network the test
skips locally and fails in CI.
