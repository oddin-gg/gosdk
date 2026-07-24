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
locale in `WithPreloadLocales(...)`. Lookups are O(1) — no I/O on the
message hot path:

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
