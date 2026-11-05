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
the session goroutine, so it never waits for a failing API the way a public
getter does: one attempt per request, no retries, and the whole build shares
one 2 s budget (each request gets what is left of it); market and outcome
name resolution during the build is covered by the same budget.

Only a failure **of the API** opens the event's failure window: a 5xx, a 429,
a transport error, a timeout after the request was sent, a
`SERVICE_UNAVAILABLE` envelope, or a response that cannot be read. A failed
event build or market/outcome name lookup of that kind delivers the message
at once with a nil event or nameless markets, and messages for the same
event then skip the API for up to 10 seconds; the first successful API
response of any call ends that at once. These do **not** open the window: a
404 for an event that is not in the API yet (the message is delivered with a
nil event, but names still resolve and the next message tries again), any
other 4xx, a request that was never sent because the 2 s budget was already
spent, and local name-resolution errors over a healthy catalog. Market name
catalogs have a window of their own: after a catalog download for a message
failed or ran out of its budget, messages for the next 30 seconds send no
catalog request and resolve no names for that locale (a stale catalog keeps
serving its names). The public entity reads (`Client.Match`, ...) keep their
retries and are not affected.

**A healthy but slow API counts too.** A cold event build is several
sequential requests (the summary, the tournament, each competitor, the
fixture, and the market catalog for every locale) that share the one 2 s
budget, and a timeout after a request was sent is a failure of the API. With
requests slower than roughly 250 ms the first messages of a new match
therefore arrive with a nil event and no market names, and open the window,
where a public getter would return the complete event, late. Add `WithPreloadLocales(...)` so the
market catalog is loaded up front and does not take part in that build.

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
the subscription has re-bound its queue, the next alive starts a snapshot
recovery reaching back at least to the loss (one alive interval further
back than strictly needed, on purpose). Watch
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

A producer's snapshot recovery has a **snapshot-complete deadline**
(`WithSnapshotCompleteDeadline`, default 5 minutes): if neither a message
carrying the recovery's request id nor its `snapshot_complete` arrives for
that long, the SDK logs once at WARN and requests the snapshot again from
the same point under a new request id; the producer stays down meanwhile.
`WithMaxRecoveryExecution` (default 6 h) still caps the whole attempt.

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
GitHub at test time (ref `main` by default, `ODDSFEEDSCHEMA_REF` to pin
a branch, tag or commit, `ODDSFEEDSCHEMA_DIR` to use a local checkout).
Every attribute and element the schema declares must have a Go field and
every Go tag must exist in the schema. Known deviations live in a ledger
in the test with the reason for each; a new deviation, or a ledger entry
the code has outgrown, fails `go test ./...`. Without network the test
skips locally and fails in CI.
