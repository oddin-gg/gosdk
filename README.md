Oddin.gg Golang SDK
-------------------

Go SDK for Oddin.gg's REST API and streaming odds feed.

### Installing

```shell
go get github.com/oddin-gg/gosdk
```

Requires Go 1.24+.

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
    "github.com/oddin-gg/gosdk/protocols"
)

func main() {
    cfg := gosdk.NewConfig(os.Getenv("TOKEN"), protocols.IntegrationEnvironment,
        gosdk.WithLogger(slog.Default()),
        gosdk.WithDefaultLocale(protocols.EnLocale),
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

    sub, err := client.Subscribe(context.Background(),
        gosdk.WithMessageInterest(protocols.AllMessageInterest),
    )
    if err != nil {
        log.Fatalf("subscribe: %v", err)
    }

    go func() {
        for msg := range sub.Messages() {
            switch m := msg.Message.(type) {
            case protocols.OddsChange:
                log.Printf("odds change: %d markets", len(m.Markets()))
            case protocols.BetSettlement:
                log.Printf("bet settlement: %d markets", len(m.Markets()))
            }
        }
    }()

    sigCh := make(chan os.Signal, 1)
    signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
    <-sigCh
}
```

### Configuration

`gosdk.NewConfig` takes the access token, an `Environment`, and any
number of functional options. Common options:

```go
gosdk.NewConfig(token, protocols.IntegrationEnvironment,
    gosdk.WithRegion(protocols.RegionDefault),
    gosdk.WithNodeID(1),
    gosdk.WithDefaultLocale(protocols.EnLocale),
    gosdk.WithPreloadLocales(protocols.EnLocale, protocols.RuLocale),
    gosdk.WithMaxInactivity(20*time.Second),
    gosdk.WithMaxRecoveryExecution(6*time.Hour),
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
- `client.Close(ctx)` is idempotent. ctx caps the drain wait.

### Catalog API

All entity types are pure-data value structs — methods are pure
field reads, no errors:

```go
match, err := client.Match(ctx, eventURN)
log.Println(match.Name(protocols.EnLocale))    // localized name
log.Println(match.Tournament.Name(protocols.EnLocale))
if match.HomeCompetitor != nil {
    log.Println(match.HomeCompetitor.Name(protocols.EnLocale))
}
log.Println(match.Status.Status)                // EventStatus
```

### Recovery

```go
handle, err := client.RecoverEventOdds(ctx, producerID, eventURN)
<-handle.Done()
res := handle.Result()
if res.Status == protocols.RecoveryStatusCompleted { ... }
```

The handle is reliable — even if the lossy `RecoveryEvents()` channel
drops the event, `Done()` unblocks correctly.

### Observability

Three lossy event channels plus polling counterparts:

```go
for ev := range client.ConnectionEvents() { ... }   // Connected/Disconnected/Reconnecting/Closed
for ev := range client.RecoveryEvents()  { ... }    // ProducerStatus + EventRecovery
for ev := range client.APIEvents()       { ... }    // HTTP request/response (opt-in)

state := client.ConnectionState()                   // polling getter
```

### Examples

See [examples/](examples/) for working programs:

- [examples/basic/](examples/basic/main.go) — minimal subscribe + consume
- [examples/recovery/](examples/recovery/main.go) — RecoveryHandle usage
- [examples/multi_locale/](examples/multi_locale/main.go) — locale fill-in
- [examples/replay/](examples/replay/main.go) — replay API
- [examples/graceful/](examples/graceful/main.go) — clean shutdown

### Migration from pre-v1.0.0

[MIGRATION.md](MIGRATION.md) covers the breaking changes from the
pre-v1 SDK: configuration via functional options, the flat `*Client`
shape (no manager-of-managers), the `Subscription` lifecycle, and the
v1.0/v1.1 entity reshape from interfaces to value structs.

### Design

[NEXT.md](NEXT.md) is the source-of-truth design document covering
the architecture, caching strategy, lifecycle, recovery state machine,
and observability shape.
