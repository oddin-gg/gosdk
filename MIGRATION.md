# Migrating to gosdk v1.0.0

This guide is for the two existing internal consumers (`kollector-esport`,
`ots-odds-bridge`) and any future consumer porting from the pre-v1 SDK.

The v1.0.0 release is a **breaking** rewrite — there is no source-level
compatibility shim. Mechanical edits land most call sites; a few flows
(session builder, lifecycle, recovery) need targeted rework.

The reference for design rationale is [NEXT.md](NEXT.md). This document
shows how each pre-v1 idiom maps to the new surface.

---

## TL;DR — what changes

1. **Configuration** is now constructed via functional options
   (`gosdk.NewConfig` + `WithX(...)`) instead of the broken value-receiver
   setter chain on `protocols.OddsFeedConfiguration`.
2. **`gosdk.NewOddsFeed` is gone.** Replaced by `gosdk.New(ctx, cfg)`
   which returns `*gosdk.Client` — a flat type with direct methods, no
   manager-of-managers indirection.
3. **`SessionBuilder().Build()` is gone.** Replaced by
   `client.Subscribe(ctx, opts...)` returning a `*Subscription`.
4. **All I/O takes `context.Context`.** Manager methods that previously
   ignored ctx now propagate it through to the API and AMQP layers.
5. **Localization** finally works: methods take `locales ...protocols.Locale`
   variadic, and the cache holds per-locale fields rather than overwriting.
6. **Lifecycle** uses idempotent `Close(ctx)` with a fast-path on the
   already-closed channel and a deterministic drain wait. Subscriptions
   surface termination through `Done()` / `Err()`.
7. **Observability** has three lossy event channels:
   `ConnectionEvents()`, `RecoveryEvents()`, `APIEvents()` — plus polling
   counterparts (`ConnectionState()`, `ProducerStatus(id)`).
8. **Logging** is `*slog.Logger`. The `sirupsen/logrus` dependency is gone.
9. **Caches** are `hashicorp/golang-lru/v2` + `golang.org/x/sync/singleflight`
   for per-event entities, plain `map+RWMutex` for static catalogs. The
   `patrickmn/go-cache` dependency is gone.

No `// Deprecated` aliases or shims are kept — v1.0.0 is a clean cut.

---

## 1. Configuration

### Before

```go
cfg := gosdk.NewConfiguration(token, protocols.IntegrationEnvironment, /*nodeID*/ 1, /*reportExtended*/ false).
    SetRegion(protocols.RegionDefault).
    SetExchangeName("oddinfeed").
    SetMessagingPort(5672).
    SetAPIURL("api.example.com").
    SetMQURL("mq.example.com").
    SetSportIDPrefix("od:sport:")
```

`SetX` had two problems: (1) value receivers meant chained calls
silently dropped intermediate state in some compilers; (2) no way to set
locale, logger, recovery cap, or HTTP timeout.

### After

```go
cfg := gosdk.NewConfig(token, protocols.IntegrationEnvironment,
    gosdk.WithNodeID(1),
    gosdk.WithRegion(protocols.RegionDefault),
    gosdk.WithExchangeName("oddinfeed"),
    gosdk.WithMessagingPort(5672),
    gosdk.WithAPIURL("api.example.com"),
    gosdk.WithMQURL("mq.example.com"),
    gosdk.WithSportIDPrefix("od:sport:"),
    gosdk.WithDefaultLocale(protocols.EnLocale),
    gosdk.WithPreloadLocales(protocols.EnLocale, protocols.RuLocale),
    gosdk.WithMaxInactivity(20*time.Second),
    gosdk.WithMaxRecoveryExecution(6*time.Hour),
    gosdk.WithHTTPClientTimeout(30*time.Second),
    gosdk.WithLogger(slog.Default()),
    gosdk.WithExceptionStrategy(gosdk.StrategyCatch),
    gosdk.WithExtendedDataReporting(false),
    gosdk.WithAPICallLogging(gosdk.APILogMetadata),
)
```

`Config` is immutable after `NewConfig` returns. Each `WithX` is an
`Option func(*Config)` applied to a private draft inside `NewConfig`.

### Option mapping table

| Pre-v1 setter / parameter | v1.0.0 option |
|---|---|
| `NewConfiguration(_, _, nodeID, _)` | `WithNodeID(int)` |
| `NewConfiguration(_, _, _, reportExtended)` | `WithExtendedDataReporting(bool)` |
| `SetRegion(...)` | `WithRegion(protocols.Region)` |
| `SetExchangeName(...)` | `WithExchangeName(string)` + `WithReplayExchangeName(string)` |
| `SetAPIURL(...)` | `WithAPIURL(string)` |
| `SetMQURL(...)` | `WithMQURL(string)` |
| `SetMessagingPort(...)` | `WithMessagingPort(int)` |
| `SetSportIDPrefix(...)` | `WithSportIDPrefix(string)` |
| _none_ — locale was always `en` | `WithDefaultLocale`, `WithPreloadLocales` |
| _none_ | `WithMaxInactivity`, `WithMaxRecoveryExecution`, `WithInitialSnapshotTime`, `WithHTTPClientTimeout` |
| _none_ | `WithLogger`, `WithExceptionStrategy` |
| _none_ | `WithAPICallLogging`, `WithAPICallBodyLimit`, `WithAMQPPrefetch`, `WithSubscriptionBuffer` |
| _none_ | `WithHTTPClient(*http.Client)` (custom TLS / instrumentation / tests) |

---

## 2. Constructor + lifecycle

### Before

```go
feed := gosdk.NewOddsFeed(cfg)  // no ctx, no error
defer feed.Close()
```

`NewOddsFeed` returned `protocols.OddsFeed` synchronously and deferred
all work to the first manager call. There was no probe of credentials
up-front and no way to scope construction to a context.

### After

```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()

client, err := gosdk.New(ctx, cfg)
if err != nil { return err } // surfaces auth / DNS failures here

defer func() {
    closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()
    _ = client.Close(closeCtx)
}()
```

`New(ctx, cfg)`:
- Builds the API client + cache + producer manager
- Issues the bookmaker-details probe synchronously (returns wrapped error
  on failure)
- Does **NOT** open AMQP — that happens lazily on first `Subscribe`,
  or eagerly via `client.Connect(ctx)` when callers want to surface
  connection errors before adding subscriptions

`Close(ctx)`:
- Idempotent — repeated calls return immediately after the first
- Drains subscriptions and closes AMQP within the supplied ctx deadline
- Returns `ctx.Err()` if the deadline fires before drain completes;
  shutdown still finishes in the background

---

## 3. Sessions → Subscriptions

### Before

```go
ch, err := feed.SessionBuilder().
    SetMessageInterest(protocols.AllMessageInterest).
    SetSpecificEventOnly(eventURN).
    Build()
if err != nil { return err }

global, err := feed.Open()
if err != nil { return err }

for msg := range ch {
    switch m := msg.Message.(type) {
    case protocols.OddsChange:    ...
    case protocols.BetSettlement: ...
    }
}

for ev := range global {
    if ev.Recovery != nil { ... }
    if ev.APIMessage != nil { ... }
}
```

### After

```go
sub, err := client.Subscribe(ctx,
    gosdk.WithMessageInterest(protocols.AllMessageInterest),
    gosdk.WithSpecificEvents(eventURN),
)
if err != nil { return err }

go func() {
    for msg := range sub.Messages() {
        switch m := msg.Message.(type) {
        case protocols.OddsChange:    ...
        case protocols.BetSettlement: ...
        }
        if msg.UnparsableMessage != nil { ... }
        if msg.RawFeedMessage != nil    { ... } // when WithExtendedDataReporting(true)
    }
}()

go func() {
    for ev := range client.RecoveryEvents() { ... }
}()

go func() {
    for ev := range client.ConnectionEvents() { ... }
}()
```

Differences:
- `Subscribe` lazy-connects on first call. No separate `Open()` step.
- The session/global channel split is gone. Recovery and connection events
  flow on dedicated, lossy buffered channels.
- Subscriptions are independent — `validateInterestCombination` checks no
  longer apply across subscriptions; each gets its own AMQP queue.
- `Subscription.Done()` closes on any termination; `Subscription.Err()`
  is nil on graceful close, non-nil on abrupt termination.
- `BuildReplay()` becomes `WithReplay()` on `Subscribe`.

---

## 4. Manager flattening

The manager-of-managers shape is gone. Each `protocols.XxxManager`
interface still exists internally but is no longer reachable through the
public API — methods land directly on `*Client`.

| Before | After |
|---|---|
| `feed.BookmakerDetails()` | `client.BookmakerDetails(ctx)` |
| `feed.ProducerManager().AvailableProducers(ctx)` | `client.Producers(ctx)` |
| `feed.ProducerManager().ActiveProducers(ctx)` | `client.ActiveProducers(ctx)` |
| `feed.ProducerManager().ActiveProducersInScope(ctx, scope)` | `client.ProducersInScope(ctx, scope)` |
| `feed.ProducerManager().GetProducer(ctx, id)` | `client.Producer(ctx, id)` |
| `feed.ProducerManager().SetProducerState(ctx, id, on)` | `client.SetProducerEnabled(ctx, id, on)` |
| `feed.ProducerManager().SetProducerRecoveryFromTimestamp(ctx, id, t)` | `client.SetProducerRecoveryFromTimestamp(ctx, id, t)` |
| `feed.SportsInfoManager().Sports(ctx)` | `client.Sports(ctx, locales...)` |
| `feed.SportsInfoManager().LocalizedSports(ctx, l)` | `client.Sports(ctx, l)` |
| `feed.SportsInfoManager().Match(ctx, id)` | `client.Match(ctx, id, locales...)` |
| `feed.SportsInfoManager().LocalizedMatch(ctx, id, l)` | `client.Match(ctx, id, l)` |
| `feed.SportsInfoManager().MatchesFor(ctx, t)` | `client.MatchesFor(ctx, t, locales...)` |
| `feed.SportsInfoManager().LiveMatches(ctx)` | `client.LiveMatches(ctx, locales...)` |
| `feed.SportsInfoManager().ListOfMatches(ctx, s, l)` | `client.ListMatches(ctx, s, l, locales...)` |
| `feed.SportsInfoManager().Competitor(ctx, id)` | `client.Competitor(ctx, id, locales...)` |
| `feed.SportsInfoManager().FixtureChanges(ctx, t)` | `client.FixtureChanges(ctx, t, locales...)` |
| `feed.SportsInfoManager().AvailableTournaments(ctx, sportID)` | `client.AvailableTournaments(ctx, sportID, locales...)` |
| `feed.SportsInfoManager().ActiveTournaments(ctx)` | `client.ActiveTournaments(ctx, locales...)` |
| `feed.SportsInfoManager().ClearMatch(id)` | `client.ClearMatch(id)` |
| `feed.SportsInfoManager().ClearTournament(id)` | `client.ClearTournament(id)` |
| `feed.SportsInfoManager().ClearCompetitor(id)` | `client.ClearCompetitor(id)` |
| `feed.MarketDescriptionManager().MarketDescriptions(ctx)` | `client.MarketDescriptions(ctx, locales...)` |
| `feed.MarketDescriptionManager().MarketDescriptionByIDAndVariant(ctx, id, v)` | `client.MarketDescription(ctx, id, v)` |
| `feed.MarketDescriptionManager().MarketVoidReasons(ctx)` | `client.MarketVoidReasons(ctx)` |
| `feed.MarketDescriptionManager().ReloadMarketVoidReasons(ctx)` | `client.ReloadMarketVoidReasons(ctx)` |
| `feed.MarketDescriptionManager().ClearMarketDescription(id, v)` | `client.ClearMarketDescription(id, v)` |
| `feed.RecoveryManager().InitiateEventOddsMessagesRecovery(ctx, p, e)` | `client.RecoverEventOdds(ctx, p, e)` |
| `feed.RecoveryManager().InitiateEventStatefulMessagesRecovery(ctx, p, e)` | `client.RecoverEventStateful(ctx, p, e)` |
| `feed.ReplayManager().ReplayList(ctx)` | `client.Replay().List(ctx)` |
| `feed.ReplayManager().AddSportEventID(ctx, id)` | `client.Replay().AddEvent(ctx, id)` |
| `feed.ReplayManager().RemoveSportEventID(ctx, id)` | `client.Replay().RemoveEvent(ctx, id)` |
| `feed.ReplayManager().Play(ctx, params)` | `client.Replay().Start(ctx, opts...)` |
| `feed.ReplayManager().Stop(ctx)` | `client.Replay().Stop(ctx)` |
| `feed.ReplayManager().Clear(ctx)` | `client.Replay().Clear(ctx)` |
| _none_ | `client.Replay().StopAndClear(ctx)` (parity with .NET) |

### Locale handling on entity methods

Each Sports/Markets method takes `locales ...protocols.Locale` last:
- Pass nothing → uses `cfg.DefaultLocale()`
- Pass one locale → method behaves as if a `LocalizedX` had been called
- Pass several → each is preloaded into the cache (multi-locale fill-in
  via the `EventCache` primitive); the entity-method-level locale is the
  first one supplied

---

## 5. Replay options

### Before

```go
params := protocols.ReplayPlayParams{
    Speed:             ptr.Int(10),
    MaxDelayInMs:      ptr.Int(50),
    RewriteTimestamps: ptr.Bool(true),
}
_, err := feed.ReplayManager().Play(ctx, params)
```

### After

```go
err := client.Replay().Start(ctx,
    gosdk.WithReplaySpeed(10),
    gosdk.WithReplayMaxDelayMs(50),
    gosdk.WithReplayRewriteTimestamps(true),
)
```

Bool / int / string params become typed options. Each option is a
`ReplayOption func(*protocols.ReplayPlayParams)`.

---

## 6. Logging

### Before

```go
import "github.com/sirupsen/logrus"

l := logrus.New()
l.SetLevel(logrus.DebugLevel)
// SDK reads from package-level state — no way to inject.
```

### After

```go
import "log/slog"

logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
cfg := gosdk.NewConfig(token, env, gosdk.WithLogger(logger))
```

`logrus` is purged. The internal log wrapper preserves the
`WithField` / `WithError` / `Errorf` / `Warnf` call sites but emits
through `*slog.Logger`.

---

## 7. Observability — APIEvents, RecoveryEvents, ConnectionEvents

These channels are **lossy** — they drop on overflow rather than
back-pressuring the producing goroutine. Use the polling counterparts
when you need every transition.

```go
cfg := gosdk.NewConfig(token, env,
    gosdk.WithAPICallLogging(gosdk.APILogMetadata),  // url+status+latency, no bodies
    gosdk.WithAPICallBodyLimit(64*1024),             // cap when level=APILogResponses or APILogFull
)

go func() {
    for ev := range client.APIEvents() {
        // ev.Method, ev.URL (path-only, query redacted),
        // ev.Status, ev.Latency, ev.Attempt, ev.Err
    }
}()

go func() {
    for ev := range client.ConnectionEvents() {
        // ev.Kind: ConnectionConnected / Disconnected / Reconnecting / Closed
    }
}()

state := client.ConnectionState() // polling getter; never blocks
```

### APILogLevel

| Level | URL/method/status/latency | Response bytes | Request bytes |
|---|---|---|---|
| `APILogOff` (default) | — | — | — |
| `APILogMetadata` | ✓ | — | — |
| `APILogResponses` | ✓ | ✓ | — |
| `APILogFull` | ✓ | ✓ | ✓ |

---

## 8. Caching

The shape is internal but two consumer-visible behaviors changed:

1. **Per-locale fills.** Asking for the same `Match(id, l1)` then
   `Match(id, l2)` no longer overwrites — both locales coexist on the
   cached entry. `LocalizedName(l1)` and `LocalizedName(l2)` both work
   afterward.
2. **Failed catalog loads no longer poison.** The static caches
   (sports, market descriptions, void reasons, match-status descriptions)
   used to wrap loads in `sync.Once`, so a transient API error stuck for
   the rest of the process. v1 retries on the next access.

No code changes needed on consumers — both behaviors are upgrades.

---

## 9. Removed / renamed protocol fields

A handful of fields were removed because they were unused by either
internal consumer:

- `protocols.SportEvent.SportEventRefID()` — RefID was never populated
  by the API; removed.
- `protocols.Market.RefID()`, `protocols.Outcome.RefID()`,
  `protocols.Competitor.RefID()` — same.
- `protocols.DefaulRegion` (typo alias for `RegionDefault`) — removed.

If you discover a method call site that no longer compiles and isn't
listed above, file an issue — it likely got pruned in the same pass.

---

## 10. Mechanical migration script

For most call sites the transform is mechanical. A starting `sed` set:

```sh
# Constructor
gofmt -r 'gosdk.NewOddsFeed(cfg) -> gosdk.New(ctx, cfg)' -w .

# Manager flattening
gofmt -r 'a.ProducerManager() -> a' -w .
gofmt -r 'a.SportsInfoManager() -> a' -w .
gofmt -r 'a.MarketDescriptionManager() -> a' -w .
gofmt -r 'a.RecoveryManager() -> a' -w .
gofmt -r 'a.ReplayManager() -> a' -w .
```

(Note: `gofmt -r` only handles top-level expressions — chained calls
need a manual pass.)

Targets:
- `kollector-esport`: ~22 call sites (Match, OddsChange, Producer*).
- `ots-odds-bridge`: ~15 call sites (BetSettlement, Sport, Replay*).

Both consumers can land in a single PR; v1.0.0 is a coordinated bump
across the three repos.

---

## Questions

Open an issue or ping the SDK channel. The reference design lives in
[NEXT.md](NEXT.md) §0–§19 with the full rationale.
