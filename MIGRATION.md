# Migrating to gosdk v1.0.0

This guide is for the two existing internal consumers (`kollector-esport`,
`ots-odds-bridge`) and any future consumer porting from the pre-v2 SDK.

The v1.0.0 release is a **breaking** rewrite — there is no source-level
compatibility shim. Mechanical edits land most call sites; a few flows
(session builder, lifecycle, recovery) need targeted rework.

The reference for design rationale is [NEXT.md](NEXT.md). This document
shows how each pre-v2 idiom maps to the new surface.

> **A note on version labels.** The first stable release is **`v1.0.0`**
> on the existing module path. "v2" and the `v2.N` section labels below
> (e.g. §11 "v2.1", §12 "v2.3") are **internal rewrite-iteration
> checkpoints** used while building this SDK — they are not release tags
> and do not imply a v2 release line. Treat them as a changelog of the
> rewrite's phases, not as versions you can `go get`.

---

## TL;DR — what changes

1. **Configuration** is now constructed via functional options
   (`gosdk.NewConfig` + `WithX(...)`) instead of the broken value-receiver
   setter chain on `types.OddsFeedConfiguration`.
2. **`gosdk.NewOddsFeed` is gone.** Replaced by `gosdk.New(ctx, cfg)`
   which returns `*gosdk.Client` — a flat type with direct methods, no
   manager-of-managers indirection.
3. **`SessionBuilder().Build()` is gone.** Replaced by
   `client.Subscribe(ctx, opts...)` returning a `*Subscription`.
4. **All I/O takes `context.Context`.** Manager methods that previously
   ignored ctx now propagate it through to the API and AMQP layers.
5. **Localization** finally works: methods take `locales ...types.Locale`
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

> **Package rename**: in v2, the `protocols` package was renamed to
> `types`. Throughout this document — including the "Before" code
> samples — references read `types.IntegrationEnvironment`, `types.Match`,
> etc. **Your pre-v2 code uses the same identifiers under `protocols`**
> (e.g., `protocols.IntegrationEnvironment`); the rename is mechanical:
> ```sh
> # In your project:
> find . -name '*.go' -print0 | xargs -0 sed -i '' \
>   -e 's|github.com/oddin-gg/gosdk/protocols|github.com/oddin-gg/gosdk/types|g' \
>   -e 's|protocols\.|types.|g'
> ```

---

## 1. Configuration

### Before

```go
cfg := gosdk.NewConfiguration(token, types.IntegrationEnvironment, /*nodeID*/ 1, /*reportExtended*/ false).
    SetRegion(types.RegionDefault).
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
cfg := gosdk.NewConfig(token, types.IntegrationEnvironment,
    gosdk.WithNodeID(1),
    gosdk.WithRegion(types.RegionDefault),
    gosdk.WithExchangeName("oddinfeed"),
    gosdk.WithMessagingPort(5672),
    gosdk.WithAPIHost("api.example.com"),
    gosdk.WithMQHost("mq.example.com"),
    gosdk.WithSportIDPrefix("od:sport:"),
    gosdk.WithDefaultLocale(types.EnLocale),
    gosdk.WithPreloadLocales(types.EnLocale, types.RuLocale),
    gosdk.WithMaxInactivity(20*time.Second),
    gosdk.WithMaxRecoveryExecution(6*time.Hour),
    gosdk.WithHTTPClientTimeout(30*time.Second),
    gosdk.WithLogger(slog.Default()),
    gosdk.WithExceptionStrategy(gosdk.StrategyCatch),
    gosdk.WithExtendedDataReporting(false),
    gosdk.WithAPICallLogging(gosdk.APILogMetadata),
)
```

`Config` is immutable after `NewConfig` returns (the custom
`WithHTTPClient` value is defensively shallow-copied on store AND on
read, so post-construction mutation of the caller's client cannot
reach the SDK — the shared `Transport` remains the one caller-owned
object, needed for connection pooling; a transport that ignores
cancellation also weakens the shutdown-bound guarantees, see doc.go).
Each `WithX` is an
`Option func(*Config)` applied to a private draft inside `NewConfig`.

### Option mapping table

| Pre-v2 setter / parameter | v1.0.0 option |
|---|---|
| `NewConfiguration(_, _, nodeID, _)` | `WithNodeID(int)` |
| `NewConfiguration(_, _, _, reportExtended)` | `WithExtendedDataReporting(bool)` |
| `SetRegion(...)` | `WithRegion(types.Region)` |
| `SetExchangeName(...)` | `WithExchangeName(string)` + `WithReplayExchangeName(string)` |
| `SetAPIURL(...)` | `WithAPIHost(string)` (renamed v2.30; pre-v2.30 callers used `WithAPIURL`) |
| `SetMQURL(...)` | `WithMQHost(string)` (renamed v2.30; pre-v2.30 callers used `WithMQURL`) |
| `SetMessagingPort(...)` | `WithMessagingPort(int)` |
| `SetSportIDPrefix(...)` | `WithSportIDPrefix(string)` |
| _none_ — locale was always `en` | `WithDefaultLocale`, `WithPreloadLocales` |
| _none_ | `WithMaxInactivity`, `WithMaxRecoveryExecution`, `WithInitialSnapshotTime`, `WithHTTPClientTimeout` |
| _none_ | `WithLogger`, `WithExceptionStrategy` |
| _none_ | `WithAPICallLogging`, `WithAPICallBodyLimit`, `WithAMQPPrefetch`, `WithSubscriptionBuffer` |
| _none_ | `WithHTTPClient(*http.Client)` (custom TLS / instrumentation / tests) |
| _none_ | `WithShutdownTimeout(time.Duration)` (caps graceful shutdown work; default 5s) |

---

## 2. Constructor + lifecycle

### Before

```go
feed := gosdk.NewOddsFeed(cfg)  // no ctx, no error
defer feed.Close()
```

`NewOddsFeed` returned `types.OddsFeed` synchronously and deferred
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
- **Validates the config up front** — missing token, unresolvable
  endpoints, out-of-range options (negative durations, a `WithMessagingPort`
  outside 1..65535, negative sizes, unknown enums), or a malformed
  `WithAPIHost`/`WithMQHost` (a scheme, a port on the MQ host, userinfo/
  path/query/whitespace, bad IPv6 brackets) all return
  `errors.Is(err, gosdk.ErrInvalidConfig)` here rather than failing at a
  later API/AMQP call. (`WithAPIHost`/`WithMQHost` with a scheme still
  panics at `NewConfig` time, as before.)
- Does **NOT** open AMQP — that happens lazily on first `Subscribe`,
  or eagerly via `client.Connect(ctx)` when callers want to surface
  connection errors before adding subscriptions

`Close(ctx)`:
- Idempotent side effects — the shutdown sequence runs exactly once.
  Repeated calls do NOT necessarily return immediately: a call made
  while shutdown is still in flight WAITS (bounded by its own ctx) for
  that same shutdown and returns the real terminal result; only after
  shutdown has completed do further calls return immediately.
- **Abrupt for active subscriptions — it does NOT wait for consumers to
  drain.** Every still-open subscription is terminated via the abrupt
  path: the in-flight AMQP delivery is Nacked (not requeued; snapshot
  recovery is the authoritative gap-filler) and no new deliveries are
  pulled. Messages ALREADY admitted to a subscription's `Messages()`
  buffer are NOT discarded — they were acked on admission and remain
  readable, so a consumer whose `range sub.Messages()` is still running
  drains them before the channel closes. What's lost is only the
  in-flight delivery and anything parked deeper in the pipeline (never
  acked; recovery fills the gap). Closes AMQP afterwards.
- Returns `ctx.Err()` if the deadline fires before shutdown completes;
  shutdown still finishes in the background

> ⚠️ **The single most common migration footgun** (NEXT.md §8):
> consumers that want a graceful drain MUST call `sub.Close(ctx)` on
> each subscription **before** `client.Close(ctx)`. `sub.Close(ctx)`
> stops intake, lets the in-flight delivery finish its
> decode→admit→ack cycle, and waits (up to ctx) until you have read
> every admitted message; only then close the client:
>
> ```go
> // Graceful shutdown: drain subscriptions first, then close the client.
> // Fresh bounded ctx PER lifecycle call (see doc.go): if the drain
> // consumes its budget, client shutdown must not start with an
> // already-expired deadline.
> drainCtx, cancelDrain := context.WithTimeout(context.Background(), 30*time.Second)
> _ = sub.Close(drainCtx)    // one per open subscription
> cancelDrain()
> closeCtx, cancelClose := context.WithTimeout(context.Background(), 10*time.Second)
> _ = client.Close(closeCtx)
> cancelClose()
> ```
>
> Calling only `client.Close(ctx)` (fine for the no-subscriptions
> example above) silently loses the in-flight and undelivered
> messages a v0.x `feed.Close()` consumer might assume were drained.

---

## 3. Sessions → Subscriptions

### Before

```go
ch, err := feed.SessionBuilder().
    SetMessageInterest(types.AllMessageInterest).
    SetSpecificEventOnly(eventURN).
    Build()
if err != nil { return err }

global, err := feed.Open()
if err != nil { return err }

for msg := range ch {
    switch m := msg.Message.(type) {
    case types.OddsChange:    ...
    case types.BetSettlement: ...
    }
}

for ev := range global {
    if ev.Recovery != nil { ... }
    if ev.APIMessage != nil { ... }
}
```

### After

```go
// Event-specific subscription: WithSpecificEvents alone implies
// SpecifiedMatchesOnlyMessageInterest, narrowing the AMQP routing to
// per-event keys. To receive ALL messages and filter client-side
// instead, pass gosdk.WithMessageInterest(types.AllMessageInterest)
// (with or without WithSpecificEvents) — explicit All wins and means
// "subscribe to all, filter manually". See §28.1 for the option
// resolution rules.
sub, err := client.Subscribe(ctx,
    gosdk.WithSpecificEvents(eventURN),
)
if err != nil { return err }

go func() {
    // types.SessionMessage is a tagged union (v2.25 reshape) — the
    // pre-v2 `msg.Message.(type)` switch is gone. Exactly one of the
    // embedded variant fields is non-nil per parsed message.
    for msg := range sub.Messages() {
        switch {
        case msg.OddsChange != nil:    // msg.OddsChange.Markets() ...
        case msg.BetSettlement != nil: // msg.BetSettlement.Markets() ...
        }
        if msg.UnparsableMessage != nil { ... }
        if msg.RawFeedMessage != nil    { ... } // when WithExtendedDataReporting(true)
    }
}()
```

> **Extended-data expansion.** With `WithExtendedDataReporting(true)`,
> one decodable AMQP delivery becomes **two** `SessionMessage` values,
> in order: first an envelope with only `RawFeedMessage` set (all
> variant fields nil), then the parsed/unparsable envelope
> (`RawFeedMessage` nil). Never both in one envelope. Dropped
> deliveries (alive traffic, disabled/out-of-scope producer) emit only
> the raw envelope; bodies that fail XML decode emit only an
> `UnparsableMessage` envelope (no raw). Consumers counting or
> correlating deliveries must not count raw envelopes as separate
> deliveries. Also note the Subscribe `ctx` bounds **setup only** —
> cancelling it later does not close the subscription; use
> `sub.Close(ctx)`.

```go

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

The manager-of-managers shape is gone, and the `types.XxxManager`
interfaces are REMOVED in v1.0.0 (their behavior lives on unexported
internal managers) — methods land directly on `*Client`.

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

Each Sports/Markets method takes `locales ...types.Locale` last:
- Pass nothing → uses `cfg.DefaultLocale()`
- Pass one locale → method behaves as if a `LocalizedX` had been called
- Pass several → each is preloaded into the cache (multi-locale fill-in
  via the `EventCache` primitive); the entity-method-level locale is the
  first one supplied

---

## 5. Replay options

### Before

```go
params := types.ReplayPlayParams{
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
`ReplayOption func(*types.ReplayPlayParams)`.

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
back-pressuring the producing goroutine. The polling counterparts
(`ConnectionState()`, `ProducerStatus(id)`, `EventRecoveryStatus(id)`)
return the CURRENT/LATEST state only — they converge but do not replay
missed transitions, and the SDK exposes no lossless transition-history
API. A consumer that needs an audit trail of every intermediate state
must build it externally; API events have no polling counterpart at
all (debug-only by design).

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

### SDK version reporting (nothing to do)

The SDK identifies itself to Oddin on every server contact so version
usage can be tracked centrally — no configuration, no consumer action, no
user data. Every API request carries `User-Agent: oddin-gosdk/<version>
(go<toolchain>)` and `X-Oddin-SDK-Version: <version>`, and each AMQP
connection reports `SDK` + `SDK_version` in its broker properties.

The reported version is resolved from build info — it is the module
version you resolved via `go get` (e.g. `1.0.0`), so it tracks your
dependency pin automatically. Local/unstamped builds (a `replace`
directive, `go run` from a checkout) report a `-dev`-suffixed fallback
(e.g. `1.0.0-dev`). Call `gosdk.Version()` if you want to log or assert
the running version yourself.

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
   the rest of the process. v2 retries on the next access.

No code changes needed on consumers — both behaviors are upgrades.

---

## 9. Removed / renamed protocol fields

A handful of fields were removed because they were unused by either
internal consumer:

- `types.SportEvent.SportEventRefID()` — RefID was never populated
  by the API; removed.
- `types.Market.RefID()`, `types.Outcome.RefID()`,
  `types.Competitor.RefID()` — same.
- `types.DefaulRegion` (typo alias for `RegionDefault`) — removed.

If you discover a method call site that no longer compiles and isn't
listed above, file an issue — it likely got pruned in the same pass.

## 10. Entity reshape — interfaces → value structs

The largest behavior shift in v1.0.0. Every entity returned by SDK
catalog methods (Match / Tournament / Competitor / Sport / Fixture /
Player / MatchStatus and their helpers PeriodScore / Scoreboard /
Statistics / Category / TvChannel / LocalizedStaticData / StaticData)
went from **interface-with-lazy-loads** to **plain value struct**.

### Before — lazy-loading interfaces

```go
match, err := client.Match(ctx, urn)
// match is a types.Match interface — every accessor returns
// (value, error) and re-enters the cache to fetch on demand.
name, err := match.LocalizedName(types.EnLocale) // *string, error
ts,   err := match.ScheduledTime()                   // *time.Time, error
status      := match.Status()                        // types.MatchStatus interface
home,  err := match.HomeCompetitor()                 // types.TeamCompetitor, error
hname, err := home.LocalizedName(types.EnLocale) // *string, error
// (pre-v2 — every accessor wrapped (value, error) + lazy I/O)
```

Every accessor took a cache lock + map walk + nil/locale check. Every
accessor could return an error. Hidden lazy fetches via
`context.Background()` because interface methods didn't take ctx.

### After — value structs

```go
match, err := client.Match(ctx, urn)
// match is a types.Match value — fully populated at construction.
name := match.Name(types.EnLocale)                   // Optional[string]
nameStr := match.Name(types.EnLocale).ValueOr("")    // always-string convenience
ts := match.ScheduledTime                            // *time.Time field
status := match.Status                               // value struct
home := match.HomeCompetitor                         // *TeamCompetitor field (nil for non-classic)
if home != nil {                                     // nil-guard BEFORE dereferencing
    hname := home.Name(types.EnLocale).ValueOr("")   // Optional[string], coerced
    _ = hname
}
```

Field access is allocation-free, never errors, never locks. Eager
loading at construction means **one** cache lookup per entity — not
one per accessor.

**Locale-keyed accessors return `Optional[string]`.** Every locale-
keyed name/abbreviation accessor on `Match`, `Tournament`, `Sport`,
`Competitor`, `Market`, `Outcome`, `MarketDescription`,
`OutcomeDescription` returns `Optional[string]`. `.ValueOr("")` keeps
the always-string ergonomics; `.Get()` distinguishes "loaded but
empty" from "not loaded". This replaces the pre-v2.x mix of
`(value, error)` lazy accessors *and* the early-rewrite `string`
(silent "" on miss) / `*string` (nil on miss) parallel pair —
collapsed into one explicit idiom.

### Per-entity migration

| Pre-v2 idiom | v1.0.0 equivalent |
|---|---|
| `match.ID()` | `match.ID` |
| `match.LocalizedName(loc)` | `match.Name(loc)` (returns `Optional[string]`; use `.ValueOr("")` or `.Get()`) |
| `match.SportID()` (returns `(*URN, error)`) | `match.SportID` |
| `match.ScheduledTime()` | `match.ScheduledTime` |
| `match.LiveOddsAvailability()` | `match.LiveOddsAvailability` (an event with no `liveodds` attribute now reports `UnknownLiveOddsAvailability`, the zero value — branch on `.IsAvailable()`) |
| `match.SportFormat()` | `match.SportFormat` |
| `match.ExtraInfo()` | `match.ExtraInfoFor(loc)` (per-locale) |
| `match.Status()` | `match.Status` (value, not interface) |
| `match.Tournament()` | `match.Tournament` |
| `match.HomeCompetitor()` / `AwayCompetitor()` | `match.HomeCompetitor` / `match.AwayCompetitor` (`*TeamCompetitor`, nil-check) |
| `match.Competitors()` | `match.Competitors` |
| `match.Fixture()` | `match.Fixture` |
| `tournament.Sport()` | `tournament.Sport` |
| `tournament.Competitors()` | `client.Competitor(ctx, urn)` for each `urn` in `tournament.CompetitorIDs` (kept lazy — multiple tournaments share competitors) |
| `tournament.RiskTier()` | `tournament.RiskTier` |
| `tournament.Category()` | `tournament.Category` (`*Category`, nil-check) |
| `tournament.StartDate()` | `tournament.StartDate` |
| `competitor.Names()` | `competitor.Names` |
| `competitor.LocalizedName(loc)` | `competitor.Name(loc)` (`Optional[string]`) |
| `competitor.Players()` | `competitor.Players` (a `map[Locale][]Player`) |
| `competitor.LocalizedPlayers(loc)` | `competitor.PlayersFor(loc)` |
| `competitor.Underage()` | `competitor.Underage` |
| `competitor.IconPath()` | `competitor.IconPath` |
| `sport.Names()` | `sport.Names` |
| `sport.LocalizedName(loc)` | `sport.Name(loc)` (`Optional[string]`) |
| `sport.Tournaments()` | `client.Tournament(ctx, urn)` for each `urn` in `sport.TournamentIDs` |
| `player.LocalizedName()` | `player.Name` |
| `player.FullName()` | `player.FullName` |
| `fixture.StartTime()` | `fixture.StartTime` |
| `fixture.TvChannels()` | `fixture.TvChannels` |
| `tvChannel.Name()` | `tvChannel.Name` |
| `status.WinnerID()` | `status.WinnerID` |
| `status.Status()` | `status.Status` |
| `status.MatchStatus()` (returns `LocalizedStaticData`) | `status.StatusDescription` (`*LocalizedStaticData`, nil-check) |
| `status.HomeScore()` / `AwayScore()` | `status.HomeScore` / `AwayScore` (`Optional[float64]`) |
| `status.Scoreboard()` | `status.Scoreboard` (`*Scoreboard`, nil-check) |
| `status.Statistics()` | `status.Statistics` (`*Statistics`, nil-check) |
| `status.PeriodScores()` | `status.PeriodScores` |
| `periodScore.HomeWonRounds()` | `periodScore.HomeWonRounds` |
| `scoreboard.HomeGoals()` | `scoreboard.HomeGoals` |
| `statistics.HomeYellowCards()` | `statistics.HomeYellowCards` |
| `localizedStaticData.LocalizedDescription(loc)` | kept as a helper method, now returning `Optional[string]` |

### Performance

- **Per-accessor**: a plain field read vs a lazy cache lookup + locks
  — order-of-magnitude estimates, not measurements (the repository
  tracks no benchmarks); the direction, not the exact nanoseconds, is
  the point.
- **Per-entity construction**: one eager fetch chain (match → tournament
  → competitors → fixture → status). For typical access patterns
  (consumers read most fields), this is the same total work — just
  shifted from "first accessor" to "construction." For listings
  (`MatchesFor`, `LiveMatches`, `ListMatches`) the API endpoints
  already return enough to populate without per-entry fan-out.
- **Concurrency**: value structs are immutable post-construction. No
  locks per accessor.

### What did NOT change

- **Message types** (`OddsChange`, `BetStop`, `BetSettlement`,
  `BetCancel`, `RollbackBetSettlement`, `RollbackBetCancel`,
  `FixtureChangeMessage`, `Unparsable`) — still interfaces. They're
  decoded fully at construction and don't carry lazy loads, so the
  reshape isn't needed.
- ~~**Market** / `MarketDescription` / `OutcomeDescription` — still
  interfaces~~ **Superseded by the Phase 6.1 market reshape:** these
  are value structs in v1.0.0 too (`types.Market`,
  `types.MarketDescription`, `types.OutcomeDescription`,
  `types.MarketVoidReason`), with locale-keyed `Names` maps populated
  at construction — see §38 for the `Optional[string]` variant
  signatures and §14.2 for preload semantics.
- **`ProducerStatus`**, `EventRecoveryMessage` — still interfaces.
  They're message-shaped, not entity-shaped, and are decoded fully at
  construction.

---

## 10.1. Mechanical migration script

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

## 11. v2.1 — Market reshape + RecoveryHandle

v2.1 extends the v2.0 entity reshape to the market/outcome tree and
delivers reliable per-request recovery completion.

### 11.1 Market types — interfaces → value structs

| Pre-v2.1 idiom | v2.1 equivalent |
|---|---|
| `market.ID()` | `market.ID` |
| `market.Specifiers()` | `market.Specifiers` |
| `market.Name()` (returns `(*string, error)`) | `market.Name(loc)` (returns `Optional[string]`; `.ValueOr("")` for the always-string ergonomics) |
| `market.LocalizedName(loc)` | folded into `market.Name(loc)` (`Optional[string]`); preload required locales via `WithPreloadLocales(...)` so the message-decode path enriches them |
| `marketWithOdds.Status()` | `marketWithOdds.Status` |
| `marketWithOdds.OutcomeOdds()` | `marketWithOdds.OutcomeOdds` |
| `marketWithOdds.IsFavourite()` | `marketWithOdds.IsFavourite` |
| `marketCancel.VoidReasonID()` | `marketCancel.VoidReasonID` |
| `marketCancel.VoidReasonParams()` | `marketCancel.VoidReasonParams` |
| `marketWithSettlement.OutcomeSettlements()` | `marketWithSettlement.OutcomeSettlements` |
| `outcome.ID()` | `outcome.ID` |
| `outcome.Name()` | `outcome.Name(loc)` (returns `Optional[string]`) |
| `outcome.LocalizedName(loc)` | folded into `outcome.Name(loc)` (`Optional[string]`) |
| `outcomeOdds.IsActive()` | `outcomeOdds.IsActive` |
| `outcomeOdds.Probability()` | `outcomeOdds.Probability` |
| `outcomeOdds.Odds(displayType)` | `outcomeOdds.Odds(displayType)` (helper kept) |
| `outcomeSettlement.OutcomeResult()` | `outcomeSettlement.OutcomeResult` |
| `outcomeSettlement.VoidFactor()` | `outcomeSettlement.VoidFactor` |

### 11.2 MarketDescription / OutcomeDescription / Specifier / VoidReason

| Pre-v2.1 idiom | v2.1 equivalent |
|---|---|
| `desc.ID()` (returns `(uint, error)`) | `desc.ID` |
| `desc.LocalizedName(loc)` | `desc.LocalizedName(loc)` (helper kept, now returns `Optional[string]`) |
| `desc.Variant()` | `desc.Variant` |
| `desc.Outcomes()` | `desc.Outcomes` |
| `desc.Specifiers()` | `desc.Specifiers` |
| `desc.Groups()` | `desc.Groups` |
| `desc.OutcomeType()` | `desc.OutcomeType` |
| `desc.IncludesOutcomesOfType()` | `desc.IncludesOutcomesOfType` |
| `outcomeDesc.ID()` | `outcomeDesc.ID` |
| `outcomeDesc.LocalizedName(loc)` | `outcomeDesc.LocalizedName(loc)` (helper kept, now returns `Optional[string]`) |
| `outcomeDesc.Description(loc)` | `outcomeDesc.Description(loc)` (helper kept, now returns `Optional[string]`) |
| `specifier.Name()` / `Type()` | `specifier.Name` / `specifier.Type` |
| `voidReason.ID()` / `Name()` / `Description()` / `Template()` / `Params()` | `voidReason.ID` / `Name` / `Description` / `Template` / `Params` |

`Client.MarketDescription` now returns `*types.MarketDescription`
(was: `types.MarketDescription` interface).

### 11.3 RecoveryHandle — reliable per-request completion

`Client.RecoverEventOdds` and `Client.RecoverEventStateful` previously
returned just a `(uint, error)` request id. Consumers had to scan the
lossy `RecoveryEvents()` channel to learn when a specific recovery
completed — and dropped events were silent.

v2.1 returns `*RecoveryHandle`. The handle is reliable: even if the
channel event is dropped, `<-handle.Done()` unblocks and
`handle.Result()` reflects the correct terminal state.

```go
handle, err := client.RecoverEventOdds(ctx, producerID, eventURN)
if err != nil { ... }

// Block until terminal:
<-handle.Done()
res := handle.Result()
switch res.Status {
case types.RecoveryStatusCompleted:
    log.Printf("recovery %d completed in %v", res.RequestID, res.EndedAt.Sub(res.StartedAt))
case types.RecoveryStatusFailed:
    log.Printf("recovery %d failed: %v", res.RequestID, res.Err)
}

// Or non-blocking:
status := handle.Status()
```

For consumers that only kept the request id:

```go
result, ok := client.EventRecoveryStatus(requestID)
if !ok { /* unknown / GC'd */ }
```

Handles stay queryable for `recovery.HandleGCGracePeriod` (default 5
minutes) after they reach a terminal state.

### 11.4 Phase 5 v2 — per-producer actor model

The recovery state machine moved to one goroutine per producer
(NEXT.md §11). State-machine semantics are preserved exactly —
verified against the Java/Kotlin and .NET reference SDKs — but the
implementation no longer needs locks on per-producer state.

This is invisible to consumers: `Client.RecoverEventOdds` still
returns a `*RecoveryHandle`, `RecoveryEvents()` still emits
`ProducerStatus` and event-recovery completions, the lossy semantics
on the channel are unchanged. The only observable effect is that
deadlocks-in-principle are now deadlocks-impossible-by-construction:
no two locks to acquire in the wrong order because there are no
locks on per-producer state.

For maintainers, the layout changed:
- `internal/recovery/actor.go` — per-producer `recoveryActor` with
  inbox + run loop + handler methods.
- `internal/recovery/actor_events.go` — typed event messages on the
  inbox.
- `internal/recovery/manager.go` — thin dispatcher: receives feed
  events, looks up the actor, pushes to its inbox; owns the handle
  registry + output channel + request-id generator.
- `internal/recovery/recovery_data.go` — gone. Its state lives in
  the actor as plain fields.

## 12. v2.3 — Context plumbing + eager market build + shutdown knob

The v2.3 set of changes is internal hygiene plus one new option. No
public consumer-facing types or methods were removed, renamed, or had
their signatures changed.

### 12.1 `WithShutdownTimeout(d)` — new option

Caps the total time the SDK spends on graceful shutdown work
(`Client.Close`, `Subscription.Close`, partial-init rollback inside
`Connect`). The same budget is shared across session-close and
broker-close so a stuck broker can't compound across sub-shutdowns.

Default is **5 seconds**, matching prior hard-coded behaviour — no
migration required for the default. Lower it in tests for faster
failure; raise it for slow brokers:

```go
cfg := gosdk.NewConfig(token, env,
    gosdk.WithShutdownTimeout(2*time.Second),  // tests
    // gosdk.WithShutdownTimeout(15*time.Second), // slow broker
)
```

### 12.2 Context plumbing through cache + factory layers (internal)

`context.Background()` was previously rooted in 7+ places inside
internal layers (cache fetchers, factory message-build paths,
`MarketData` lookups). v2.3 plumbs caller-rooted ctx through:

- `internal/factory/MarketDescriptionFactory.*` — all methods now take
  `ctx context.Context`.
- `internal/factory/MarketFactory.BuildMarket*` and `buildOutcome*`
  — ctx threaded.
- `internal/factory/FeedMessageFactory.BuildMessage` /
  `BuildUnparsableMessage` — take `ctx`.
- `internal/cache/LocalizedStaticDataCache` — `LocalizedItem(ctx, …)`,
  `Item(ctx, …)`; periodic refresh goroutine bound to a lifecycle ctx
  derived from the `cache.NewManager(ctx, …)` ctx (`WithoutCancel +
  WithCancel`).
- `internal/cache/NewManager(ctx, client, cfg, logger)` — new ctx
  parameter (was missing).
- Internal `sdkOddsFeedSession.Close()` → `Close(ctx)`.

**Consumers don't see any of this** — the public `gosdk.Client` /
`gosdk.Subscription` / `types.OddsChange` / `types.MarketDescription`
APIs are unchanged. Message-build paths now correctly carry caller
metadata (logger fields, OTel trace ids) into cache fetches when those
fall through to the network.

After this work, only **two** `context.Background()` sites remain in
production code: `Client.runShutdown` and `Subscription.runShutdown`.
Both are at the top of independent shutdown goroutines that outlive
their callers — Background is the only correct primitive there. Both
are documented inline.

### 12.3 Eager market build — fixes a latent `Markets()` bug

Pre-v2.3 message impls (`oddsChangeImpl`, `betSettlementImpl`,
`betCancelImpl`, `rollbackBet*`) lazy-built the market slice on the
first `Markets()` / `RolledBackSettledMarkets()` / etc. call:

```go
func (m oddsChangeImpl) Markets() []types.MarketWithOdds {
    if m.markets == nil {
        m.markets = make(...)  // BUG: m is a value receiver
        ...
    }
    return m.markets
}
```

Because the message impls are returned by value from `BuildMessage`
and the receiver is `m` (not `*m`), the cache assignment modified a
local copy. Every call to `Markets()` rebuilt from scratch.

v2.3 builds markets eagerly inside `BuildMessage(ctx, …)` and stores
the slice on the impl struct. `Markets()` is now a constant-time
field read. **Behavior change for consumers:**

- ✅ `Markets()` is now O(1) instead of O(markets × outcomes) per call.
- ✅ Names resolved with the build-path ctx (proper logger fields).
- ⚠️ Resolution errors are no longer deferred to first `Markets()`
  call — they're already absorbed by `resolveMarketName` /
  `resolveOutcomeName` which return `""` on lookup failure (same as
  before). No new error path.

If your code calls `msg.Markets()` repeatedly in a hot loop, you'll
see less GC pressure and lower CPU after upgrading.

### 12.4 Internal-only API changes

These are out of `internal/` so no consumer can break, but listed for
SDK contributors:

- `types.MarketData` interface (internal lookup shim — comment in
  `types/market.go` says "Not consumer-facing"):
  ```diff
  -MarketName(locale Locale) (*string, error)
  -OutcomeName(id string, locale Locale) (*string, error)
  +MarketName(ctx context.Context, locale Locale) (*string, error)
  +OutcomeName(ctx context.Context, id string, locale Locale) (*string, error)
  ```
- `cache.NewManager` — new leading `ctx` parameter (used to derive the
  lifecycle ctx for periodic-refresh goroutines).

## 13. v2.4 — Review-driven hardening

A pre-beta review surfaced eight items spanning correctness, spec drift,
and missing surface. v2.4 closes all of them. No silent behaviour
changes for code paths that were already correct; everything else moves
toward the documented design.

### 13.1 Session shutdown panic — fixed (correctness)

`session.go` previously read `case msg := <-ch:` without an ok-check; on
consumer-channel close it received a `nil` pointer and panicked in the
next `msg.UnparsableMessage` deref. Compounding that, `Close` closed
`msgCh` *before* the goroutine finished, while bare `o.msgCh <- ...`
sends were still in flight — risking send-on-closed-channel panics.

Fixed:

- `case msg, ok := <-ch:` checks the second return value.
- `Close` cancels loop ctx, waits (bounded by ctx) for the goroutine to
  exit, then closes the consumer.
- The goroutine now owns `msgCh` and is the sole closer (via deferred
  `close(msgCh)`) — no race with Close.
- All previously-bare sends (`processMessage` / `processFeedMessage`)
  are now `select { case msgCh <- m: case <-ctx.Done(): return }`.

Behaviour change: impossible in normal operation. The path that used to
panic now exits cleanly. No consumer changes required.

### 13.2 `WithExceptionStrategy(StrategyThrow)` — wired (was a no-op)

`StrategyThrow` was stored on `Config` but never read; the message
pipeline always emitted `UnparsableMessage` and continued. v2.4 implements
the documented contract:

- The session captures the build/decode error in `session.Err()` and
  cancels the loop ctx (graceful goroutine exit).
- `pumpSubscription` notices the underlying session terminated with a
  non-nil `Err()` and calls `sub.abortWithErr(err)`.
- The consumer's `sub.Err()` returns the original cause; `Done()`
  closes; the subscription terminates.

`StrategyCatch` (the default) is unchanged. Behaviour change applies
only to consumers that explicitly opted into Throw.

### 13.3 `WithHTTPClientTimeout` + `WithAMQPPrefetch` — wired (were no-ops)

Both options were stored on `Config` but never read by their consumers
(api.Client used a hard-coded 30s; ChannelConsumer hard-coded prefetch
to 1000). Now:

- `api.NewWithLogger(cfg, logger, timeout)` takes the timeout (≤ 0
  falls back to default).
- `feed.NewChannelConsumer(client, factory, logger, exchange, prefix, prefetch)`
  takes prefetch (≤ 0 falls back to default).
- `client.go` plumbs `cfg.httpClientTimeout` and `cfg.amqpPrefetch`
  through.

Behaviour change: consumers that set these options now actually get
them. Defaults unchanged.

### 13.4 Concurrent `Connect` waits on in-flight (was an error)

Previously `Connect` returned `gosdk: connect already in progress` when
called concurrently. NEXT.md §8 specifies "concurrent callers should
wait on the same in-flight attempt." Fixed via a `connectDone chan`
plus `connectErr` field; second callers `<-connectDone` and read the
result. Each caller's ctx still bounds *their own wait* — a tight ctx
returns `ctx.Err()` while the in-flight attempt continues.

`Subscribe` is unaffected — it already calls `Connect` lazily; only
the synchronisation contract for explicit `Connect` callers improves.

### 13.5 Event channels: per-channel buffer + drop-oldest + slog.Warn

NEXT.md §19.3 specified per-channel buffer sizes, drop-oldest semantics
(not drop-newest), and slog warnings on overflow. Implementation drift
had collapsed all three into a single 64-slot buffer with silent
drop-newest. Fixed:

| Channel | Buffer | Why |
|---|---|---|
| `connEvents` | 32 | connection transitions are rare |
| `recvEvents` | 1024 | largest — recovery completions matter most (NEXT.md §0.3) |
| `apiEvents` | 256 | opt-in debug stream; drops are acceptable |

A new `pushDropOldest[T]` helper drains one slot then enqueues the new
event when the buffer is full. A rate-limited (one per channel per 5s)
slog.Warn fires when overflow occurs. **Behaviour change**: consumers
that lag will now see *fresher* events (last-N) and a log warning
instead of stale events (first-N) and silent loss.

### 13.6 API body capture — capped event slice (was full body)

`captureBody` previously did `io.ReadAll(rest)` and used the full body
both for the parser and as the captured slice (capped via
`combined[:limit]`). The slice header was capped, but its backing array
was still the full body — pinning all those bytes for as long as the
event was held by a consumer.

Fixed: the captured event is a fresh `make([]byte, captureLen)` + copy.
The parser sees the full body via a separate `io.NopCloser(bytes.NewReader(full))`.
Once the parser is done, the full-body buffer is GC-eligible; the
event holds only its (≤ BodyLimit) bytes.

Memory impact: for a 5 MB API response with a 16 KiB BodyLimit, the
event used to pin 5 MB; now it pins 16 KiB.

### 13.7 Public API surface — closes parity gaps with .NET / Java

New methods on `*Client`:

| Method | Purpose |
|---|---|
| `Sport(ctx, id, locales...)` | singular sport by URN (was Sports() only) |
| `Player(ctx, id, locales...)` | player profile by URN |
| `ClearPlayer(id)` | evict cached player across all locales |
| `ClearSport(id)` | evict cached sport |
| `ClearMarketVoidReasons()` | evict the void-reasons catalog |
| `Replay.Status(ctx)` | hits GET /replay/status; returns engine state string |

Signature change:

| Before | After |
|---|---|
| `MarketDescription(ctx, id, variant)` | `MarketDescription(ctx, id, variant, locales...)` |

The `locales` parameter is variadic — existing callers compile unchanged.
Pass one or more locales to fan out the cache fill; omit for the default
locale (prior behaviour).

The internal `types.SportsInfoManager` and `types.MarketDescriptionManager`
interfaces gained matching methods (`Sport`, `LocalizedSport`, `Player`,
`LocalizedPlayer`, `ClearPlayer`, `ClearSport`,
`LocalizedMarketDescriptionByIDAndVariant`, `ClearMarketVoidReasons`).
Out-of-tree implementations of these interfaces will need to add the
new methods.

`types.ReplayManager` gained `Status(ctx)`.

### 13.8 `WithPreloadLocales` — wired (was a no-op)

Previously stored on `Config` but never read. Now:

- `MarketFactory` is constructed with `[default, preload...]` (deduped).
  AMQP message-build name resolution fills *all* configured locales in
  the cache on first build.
- `LocalizedStaticDataCache` (used for match-status descriptions) takes
  the same locale list. The first `Item()` call eagerly fetches every
  locale; the periodic refresh keeps every locale fresh.

Behaviour change: consumers that set `WithPreloadLocales(en, ru, de)`
now actually get all three locales preloaded. The default locale is
always first regardless of preload list ordering.

### 13.9 Internal API changes (out of `internal/`, listed for contributors)

```diff
- types.SportsInfoManager: + Sport, LocalizedSport, Player, LocalizedPlayer
                            ClearPlayer, ClearSport
- types.MarketDescriptionManager: + LocalizedMarketDescriptionByIDAndVariant
                                    ClearMarketVoidReasons
- types.ReplayManager:     + Status(ctx)
- cache.NewManager(ctx, client, cfg, logger, preloadLocales)  // new arg
- api.NewWithLogger(cfg, logger, timeout)                     // new arg
- feed.NewChannelConsumer(.., prefetch)                       // new arg
- newSession(.., exceptionStrategy, amqpPrefetch)              // new args
- sdkOddsFeedSession.Err() error                              // new method
```

## 14. v2.5 — Second-pass review hardening

A second review pass surfaced five more findings spanning concurrency
correctness, missing public surface, and partial-implementation gaps
in the API observability and per-message locale stories. v2.5 closes
all of them. The big shape change is item 14.2 — Market.Name/Outcome.Name
move from a single string to a per-locale map.

### 14.1 Close races Connect — fixed (correctness)

Connect's success path used to do an unconditional
`connectState.Store(Connected)`. A concurrent Close that had already
transitioned to Closed would be silently overwritten, leaving the
client reporting "connected" with goroutines/sessions that runShutdown
had only partially torn down.

Fixed:

- Connect's success path is now a `CompareAndSwap(Connecting, Connected)`.
  If the CAS fails (state is already Closed), Connect returns
  `errSubscriptionClosed`; runShutdown still tears down the goroutines
  Connect spawned.
- Close waits (bounded by its ctx) for an in-flight Connect's
  `connectDone` to settle before transitioning state and starting
  runShutdown. This guarantees the goroutines/sessions Connect would
  spawn are observable to runShutdown.

Behaviour change: Close called during a slow Connect now blocks on
that ctx instead of returning immediately. Concurrent users of these
two methods (e.g., signal handlers) get correct ordering for free.

### 14.2 Per-message locale enrichment — fixed (breaking shape change)

NEXT.md §7 says markets in feed messages should be enriched with names
for every locale in `WithPreloadLocales(...)` so consumers can call
`market.LocalizedName(locale)` as a pure in-memory lookup. v2.4 wired
preload locales into the cache but the factory still resolved only
`m.locales[0]`, leaving `Market.Name` a single string.

**Public type change** — `types.Market` and `types.Outcome`:

```diff
 type Market struct {
     ID         uint
     Specifiers map[string]string
-    Name       string
+    Names      map[Locale]string
 }
+
+func (m Market) Name(locale Locale) Optional[string]  // None if not preloaded

 type Outcome struct {
     ID    string
-    Name  string
+    Names map[Locale]string
 }
+
+func (o Outcome) Name(locale Locale) Optional[string]
```

The original v2.4 reshape introduced *two* parallel accessors —
`LocalizedName(loc) *string` (nil on miss) and `Name(loc) string`
(empty on miss). The v2.x reshape collapsed them into a single
`Name(loc) Optional[string]` for consistency with the rest of the
SDK's "maybe-loaded" idiom and to remove the silent-empty-string
footgun that bit migration code (a caller passing an unloaded locale
silently got `""` instead of an error). `LocalizedName` is gone from
`Market` and `Outcome`; `MarketDescription.LocalizedName` and
`OutcomeDescription.LocalizedName` / `.Description` are kept (the
catalog types) but now return `Optional[string]`.

Migration for consumers:

```diff
- log.Println(market.Name)
- log.Println(outcome.Name)
+ log.Println(market.Name(types.EnLocale).ValueOr(""))   // always-string convenience
+ log.Println(outcome.Name(types.EnLocale).ValueOr(""))
+
+ // Or strict:
+ name, ok := market.Name(types.RuLocale).Get()
+ if !ok { return fmt.Errorf("missing locale ru for market %d", market.ID) }
```

If you want a non-default locale, configure it via
`WithPreloadLocales(types.EnLocale, types.RuLocale, …)` — the factory
fills `Names` for every preloaded locale at message-decode time, so
`market.Name(types.RuLocale)` is a constant-time map lookup that
returns `Some(...)`.

If a locale wasn't preloaded, `Name(loc)` returns `None` — there is
no synchronous fetch from the message hot path (per NEXT.md §7's
"no hidden I/O" rule). To get a non-preloaded locale, prime the
cache via `Client.MarketDescription(ctx, id, variant, locale)`
first; future messages will pick it up.

### 14.3 Client.ProducerStatus(producerID) — added

NEXT.md §0.3 documents `client.ProducerStatus(id)` as the polling
fallback for the lossy `RecoveryEvents` channel. v2.4 had no such
method. v2.5 adds it:

```go
func (c *Client) ProducerStatus(producerID int) (types.ProducerStatus, bool)
```

Implementation: the per-producer recovery actor stores the most recent
`ProducerStatus` in an `atomic.Pointer` snapshot every time it emits
on the channel; the Client method reads the snapshot. So even if a
consumer missed the live event, the polling getter reflects the
latest state.

### 14.4 API event observability — locale + streaming + redaction

Three related fixes in `internal/api`:

- **Locale propagation.** `do()` and `fetchData()` now thread the
  `*types.Locale` through to the emitted `APIEvent`. Previously
  `APIEvent.Locale` was always nil despite being part of the type.
- **Streaming capture.** `captureBody` is replaced by
  `installCapture` + `emit`: the response body is wrapped in an
  `io.TeeReader` → bounded `capBuf`. The decoder reads through the
  tee; the buffer fills *as a side-effect of parsing* and is
  hard-capped at `BodyLimit`. No `io.ReadAll` of the rest, no
  full-body materialization for large payloads. Memory peak is now
  decoder working set + BodyLimit, not full body.
- **Body redaction.** Captured bytes are scanned for the configured
  access token and replaced with `[REDACTED]` before the
  `APIEvent` is emitted. *(Superseded: the original 16-character
  minimum gate was removed — EVERY non-empty token is redacted,
  whatever its length, and redaction now also covers the token's
  XML-escaped and URL-escaped wire forms plus fragments split by the
  capture's truncation boundary. Over-scrubbing short coincidental
  substrings is an accepted fidelity cost; secrecy wins.)*

### 14.5 feed.Client.Open concurrent callers wait on in-flight

Mirror of v2.4's gosdk.Client.Connect fix at the layer below. Replay
subscriptions bypass `Client.Connect` and call `feed.Client.Open`
directly, so the same coalescing contract is now enforced there:

- Two concurrent `Open(ctx)` calls share the same in-flight attempt
  via `openDone` + `openErr`.
- Late callers wait (bounded by their own ctx) and observe the
  in-flight outcome (nil on success, the original error on failure).
- The `"feed: open already in progress"` error is gone.

This closes the last remaining "in progress" race surface — concurrent
replay subscribes, replay during a normal connect, etc.

### 14.6 Internal-API delta (out of `internal/`, listed for contributors)

```diff
- types.Market: + LocalizedName(locale) *string, Name(locale) string
                  Names map[Locale]string  (was: Name string)
- types.Outcome: + LocalizedName(locale) *string, Name(locale) string
                   Names map[Locale]string  (was: Name string)
- types.SportsInfoManager: unchanged
- recovery.Manager: + ProducerStatus(producerID) (types.ProducerStatus, bool)
- api.Client: do() now returns *pendingCapture, takes locale
              installCapture/emit replace captureBody
              redactSensitive scrubs the access token
- feed.Client: Open() coalesces concurrent callers via openDone/openErr
```

## 15. v2.6 — Two more review-driven fixes

A third review pass surfaced two residual issues. v2.6 closes both.

### 15.1 Close-runShutdown vs Connect race on the ctx-timeout path — fixed

v2.5 made `Close(ctx)` wait for an in-flight Connect via `connectDone`,
but the wait was bounded by the caller's ctx. If that ctx fired first,
`runShutdown` was spawned **before Connect finished writing
`c.aliveSession` / `c.internalCancel` / recovery actor refs**, leaking
those resources.

Fix: `runShutdown` itself now waits on `connectDone` before reading any
shared field. This is *internal* synchronization, not bounded by
Close's caller ctx — Connect always terminates (its caller ctx bounds
the dial; the success-path CAS sees state=Closed and returns
`errSubscriptionClosed`; rollback paths return inline). After Connect
settles, runShutdown proceeds with full visibility of every resource
Connect may have spawned.

Behaviour change: Close's caller still gets `ctx.Err()` if their ctx
expires; the *cleanup* completes correctly in the background.

### 15.2 4xx body double-consumption — fixed (correctness)

With `WithAPICallLogging(APILogResponses)` enabled, a 4xx response
went through `readErrorBody` first (which consumed `r.Body`), then
`toAPIError(method, path, r)` tried to decode the now-empty stream —
silently losing the server's structured error message and falling
back to a generic `status: 4xx` wrap.

Fix: `readErrorBody` now reads the body **once** into bytes and
returns both the parser-facing slice (full body) and the event-facing
slice (capped + redacted). `toAPIError(method, path, status, body)`
takes the pre-read bytes via `bytes.NewReader` for `xml.Decoder`. The
structured error message is preserved end-to-end whether or not body
capture is enabled.

Behaviour change: error wraps now include server messages even when
body capture is on. Previously this was an information-loss bug
specific to the API-events path.

## 16. v2.7 — Connect failure-path CAS (close out the state-stomp race)

The third dimension of the Close/Connect race surfaced after v2.6.

v2.5 fixed the **success path** (`Store(Connected)` → CAS).
v2.6 fixed the **runShutdown ordering** (waits on `connectDone`).
v2.7 fixes the **failure path**.

Connect's defer used to do an unconditional `Store(NotConnected)` when
`settled == false`. If a concurrent `Close` had already transitioned
state Connecting → Closed *during* the in-flight attempt and the
attempt then errored out, the defer would overwrite Closed back to
NotConnected — letting a subsequent `Connect` run on a client whose
`runShutdown` had already started.

Fix: the failure-path now uses `CompareAndSwap(Connecting,
NotConnected)`. If state is Closed, the CAS no-ops; the Closed state
is preserved.

**Invariant audit.** Every `connectState` mutation is now safe under
concurrent Close:

| Site | Transition | Type | Safety |
|---|---|---|---|
| `New` | → NotConnected | Store | initial state |
| Connect prelude | NotConnected → Connecting | Store-under-mu | switch first reads state |
| Connect success | Connecting → Connected | **CAS** | won't stomp Closed |
| Connect failure-defer | Connecting → NotConnected | **CAS** *(new)* | won't stomp Closed |
| Close | * → Closed | Store-under-mu | authoritative; intentional |
| runShutdown | * → Closed | Store-under-mu | idempotent re-store |
| Replay subscribe | NotConnected → Connected | CAS | won't fire if Closed |

Once state reaches Closed it cannot be transitioned away. No new
`Connect` can start after Close: the prelude's switch returns
`errSubscriptionClosed` before any state mutation.

## 17. v2.8 — Edge-case hardening (rollback, lifecycle, timeouts, panics)

A fourth review pass found six real edge-case bugs. v2.8 closes them.

### 17.1 Partial Connect rollback no longer poisons retries

`feed.Client` and `recovery.Manager` are one-shot in their internals
(once Closed they reject reopen). Connect's rollback paths called
`Close()` on each, leaving the top-level `Client` retryable but with
internals in a terminal state — the next `Connect()` would either be a
no-op-on-stale-state or fail with "already opened".

Fix: `Client.resetConnectionLayer()` re-creates fresh `rabbitMQClient`
+ `recoveryManager` instances. Called from each Connect rollback site
(amqp-open failure, recovery-open failure, alive-session-open
failure). A retry now sees a clean state.

### 17.2 Recovery manager lifecycle gate

`Manager.findOrSpawn` could spawn actors before Open or after Close.
`dispatchRecoverEvent` used `sendBlocking` which ignored ctx — a Close
racing with a `RecoverEventOdds` call could hang the caller forever.
Session callbacks (`OnMessageProcessingStarted`, `OnAliveReceived`, …)
could spawn actors during shutdown.

Fix:

- Explicit `state atomic.Int32` with NotOpened / Open / Closed.
- `Open()` is one-shot via CAS NotOpened → Open; Closed is
  authoritative (transitions any → Closed unconditionally).
- `findOrSpawn` returns nil unless `state == Open`. Session callbacks
  null-check before sending.
- `dispatchRecoverEvent` returns `ErrManagerNotOpen` /
  `ErrManagerClosed` immediately and uses ctx-bounded `sendCtx` instead
  of unbounded `sendBlocking`.

### 17.3 RecoveryHandle now times out

Event recoveries that never received a `SnapshotComplete` would hang
their Handle forever. `RecoveryStatusTimedOut` was defined but
unreachable.

Fix: per-tick scan in `recoveryActor.expireStuckEventRecoveries`
transitions any in-flight recovery older than `MaxRecoveryExecutionMinutes`
(later renamed to `MaxRecoveryExecution time.Duration` in v2.28 — see §47)
to `RecoveryStatusTimedOut`. Handle's
`Done()` channel closes; consumers blocked on it observe the timeout
status and the wrapped error.

### 17.4 Subscription pump owns `close(s.messages)`

`Subscription.runShutdown` closed `s.messages` while
`pumpSubscription` could still be mid-`case sub.messages <- msg:` on
the timeout path — send-on-closed-channel panic.

Fix: pump owns the close, deferred on its exit. `runShutdown` no
longer closes `s.messages` — it waits for pumpDone (which guarantees
messages is already closed) or, on timeout, leaves the goroutine
running with the channel still open. A leaked goroutine is preferable
to a panic; the pump exits naturally when its `respCh` upstream
closes.

### 17.5 Producer state is now race-safe

`producer.data` carried mutable fields (`enabled`, `flaggedDown`,
`lastMessageTimestamp`, …) accessed across goroutines without
synchronization — explicitly documented as a known issue.

Fix: `data.mu sync.RWMutex` guards every mutable field. The manager's
setters take `mu.Lock`; `producerImpl` accessors take `mu.RLock`. The
"known concurrency issues" comment is gone.

Also fixed: `producerImpl.RecoveryInfo()` no longer panics — it
returns a snapshot of the most recent recovery summary (or the zero
value when none has been recorded).

### 17.6 Internal recovery events are actually lossy

`emitRecoveryMessage` blocked on a full `m.out`. Under sustained
load, an actor stuck on the send could stall its goroutine, which in
turn could stall shutdown (Close waits for actors to exit). The same
drop-oldest contract that the public event channels follow now
applies to the internal recovery stream — no actor blocks indefinitely
on a slow consumer.

### 17.7 home/away outcome localized comparison

*(Superseded — the English-only limitation this section described has
been fixed.)* The home/away placeholder is now recognised by the
outcome's CANONICAL (English catalog) label — its only
locale-independent identity — so the substitution keys off English but
returns the competitor's name in the REQUESTED locale for every locale,
not just English (see `marketDataImpl.makeOutcomeName`). The only
remaining fall-through is a locale in which the competitor name itself
was never loaded: there `Name(locale)` honestly reports `None` (never a
bogus `Some("")`). What was "kept per the project lead" is the MATCHING
key (English), not an English-only output.

## 18. v2.9 — Race-safety follow-ups

### 18.1 atomic.Pointer for swappable connection-layer fields

`Client.recoveryManager` and `Client.rabbitMQClient` are
*replaced* on Connect's rollback paths (v2.8's
`resetConnectionLayer`), but public methods read those fields without
synchronization — a real Go data race when Recover-event polling /
replay-Subscribe ran concurrently with a failed Connect retry.

Fix: both fields are now `atomic.Pointer[recovery.Manager]` /
`atomic.Pointer[feed.Client]`. Every public read uses `.Load()`;
`resetConnectionLayer` and other writers use `.Store()`. Subscribe
takes a single Load each so the session is built against a coherent
view (resetConnectionLayer can't race a swap mid-construction).

### 18.2 recovery.Manager.Open: Opening state separates start from publish

`Open` previously CAS'd NotOpened → Open *before* initializing
`m.ctx`, `m.out`, `m.actors`, etc. — a racing `RecoverEvent*` call
could pass the `state == Open` gate and reach into half-built fields.

Fix: new `mgrStateOpening` state. CAS NotOpened → Opening, run init,
CAS Opening → Open via `Store(mgrStateOpen)` only after every field
is published. `dispatchRecoverEvent` and `findOrSpawn` accept work
exclusively in state `Open`; calls during `Opening` return
`ErrManagerNotOpen` (callers can retry rather than terminal-fail).

### 18.3 RecoveryInfo() now correctly returns nil

`producerImpl.RecoveryInfo()` returned `&out` where `out` was the
nil interface — a non-nil pointer to a nil interface, making
`if p.RecoveryInfo() != nil` lie.

Fix: explicit `if p.producerData.lastRecoveryInfo == nil { return nil }`
gate. The nil check now means what it says.

## 19. v2.10 — Subscribe generation mix + manager open/close race

### 19.1 Subscribe takes a coherent connection-layer snapshot

In the replay path, `c.rabbitMQClient.Load()` was called once for
`Open(ctx)` and again later for `newSession`. If a concurrent failed
Connect ran `resetConnectionLayer()` between the two Loads, the
session was built against the *new* (unopened) `rabbitMQClient` while
the *old* one was the one that got Open'd.

Fix: snapshot `rmq` and `rmgr` once at the top of `Subscribe` and use
the same generation for the rest of the call. Non-replay path
re-loads after `Connect` (which itself may have rolled back and
swapped via `resetConnectionLayer`).

### 19.2 recovery.Manager.Open uses CAS Opening → Open

`Open`'s final state publish was an unconditional `Store(Open)`. A
`Close` that landed during init had already transitioned to `Closed`,
but the late `Store(Open)` resurrected the manager — same anti-pattern
as the v2.5 Connect race.

Fix: `CompareAndSwap(Opening, Open)`. If the CAS fails the state must
be `Closed`, the manager's cancel ctx is already cancelled, and the
goroutines just spawned will observe their ctx done and exit. `Open`
returns `ErrManagerClosed` on this race so the caller sees a coherent
"manager was closed during open" outcome.

## 20. v2.11 — Lifecycle mutex + inspect-and-nil cleanup

### 20.1 recovery.Manager Open/Close direct race

If `Close` ran before `Open` had assigned `cancelCtx`, `closeCh`,
`out`, or `actors`, Close's teardown saw an empty manager and did
nothing. `Open` then continued to allocate, failed the final CAS,
and returned without cleanup — leaking the just-allocated resources.

Fix: lifecycle mutex + inspect-and-nil cleanup helper.

- `lifecycleMu sync.Mutex` serialises Close's teardown with Open's
  CAS-fail cleanup.
- `cleanup()` *inspects* each field, tears down the non-nil ones,
  and *nils* them. A second call sees nil and no-ops naturally.
- The previous `closeOnce` + `cleaned bool` patterns are gone — both
  would have skipped the second call's work even when there was more
  to clean up (the exact race the reviewer flagged).

`Close` and `Open`-on-CAS-fail both call `cleanup()` under
`lifecycleMu`. Whichever runs first tears down what's there;
whichever runs second tears down anything allocated in between
(typically nothing, but covers the race precisely).

## 21. v2.12 — recovery.Manager full lifecycle race-safety

The previous `lifecycleMu`-guarded `cleanup()` was directionally
right but left the actual shared fields (`m.cancelCtx`, `m.out`,
`m.closeCh`, `m.ticker`, `m.ctx`) writable WITHOUT the mutex from
`Open` and `runTickLoop`. The mutex didn't establish any happens-before
edge for those reads/writes. Reviewer flagged this as a real Go data
race under `-race`.

### 21.1 lifecycleSession atomic.Pointer

All per-Open allocations (`ctx`, `cancelCtx`, `out`, `closeCh`) move
into a `lifecycleSession` struct stored behind `atomic.Pointer`.
Open builds the struct *locally* first; the publish step is a single
atomic `Store` under `lifecycleMu`. Close runs `Swap(nil)` under the
same mutex to atomically take ownership for teardown. Concurrent
readers (`findOrSpawn`, `emitRecoveryMessage`) `Load()` the pointer
and observe a coherent snapshot or nil — never a half-built struct.

### 21.2 Ticker + closeCh local to runTickLoop

The ticker is no longer a manager field — it's a local in
`runTickLoop` (with `defer ticker.Stop()`). `closeCh` is captured at
goroutine spawn time as a parameter. Neither is touched by `cleanup`,
so the tick loop is fully race-isolated from teardown.

### 21.3 Open's publish step under lifecycleMu

Open now holds `lifecycleMu` from before the session-publish through
the final CAS Opening → Open. This means:

- A concurrent Close that observes `state == Opening` waits for mu,
  then sees the published session and tears it down via cleanup's
  `Swap(nil)`.
- A concurrent Close that beats Open to `state.Store(Closed)` is then
  blocked at `mu.Lock()` until Open releases. Open re-checks state
  under mu; if it's Closed, Open releases mu, tears down its locals
  inline, and returns `ErrManagerClosed`. The session pointer was
  never published.

There is no observable half-published window.

### 21.4 Findings audited

Every `m.session` read uses `atomic.Pointer.Load`. Every write
(`Store`/`Swap`) happens under `lifecycleMu`. Every direct field
access on the manager that previously lived directly on the struct
is now routed through the session pointer. `actorsMu` continues to
guard `m.actors` (its scope is unchanged). `m.state` is atomic.

**Stress test:** `TestManager_OpenCloseStress` runs 200 iterations of
concurrent publish + Close + reader Loads under `-race` and passes
clean. The full suite passes `-race` clean too.

## 22. v2.13 — Cross-flow lifecycle correctness

Five cross-flow lifecycle bugs surfaced in the v2.13 review pass — all
real, all bounded fixes (deferring the broader admission-layer
refactor the reviewer suggested as a follow-up).

### 22.1 Subscribe-vs-Close admission race (HIGH)

`Subscribe` checked `Closed` once at the top, then did substantial
work (Connect, session.Open) before inserting into `c.subs` and
calling `c.wg.Add(1)`. Meanwhile `Close` set `Closed`, snapshotted
`c.subs`, and called `c.wg.Wait()`. A subscription inserted *after*
the snapshot was missed; worse, `c.wg.Add(1)` after `Wait` returned
panics with `"sync: WaitGroup misuse"`.

Fix: re-check `Closed` under `subsMu` *after* `session.Open` succeeds,
and either insert + `wg.Add(1)` atomically OR roll back the session
and return `errSubscriptionClosed`. The rollback uses
`WithoutCancel(ctx)` so a tight Subscribe ctx doesn't abort cleanup.

### 22.2 Replay subscribe poisoning the connect lifecycle (HIGH)

The replay path opened AMQP directly and CAS'd `connectState` to
`Connected` — but skipped producers, recovery manager, alive session,
and the recovery pump. A later normal `Connect()` saw `Connected` and
fast-returned, leaving normal subscribers without recovery
infrastructure.

Fix: replay no longer touches `connectState`. Replay subscriptions
are independent of the global feed-SDK lifecycle — they live as long
as their `*Subscription` is open. The global state stays
`NotConnected` until a normal `Subscribe` / explicit `Connect` runs
the full setup. `feed.Client.Open` is idempotent, so a normal
`Connect` after replay reuses the already-open AMQP connection.

### 22.3 Subscription.Close was tearing down the shared cache manager (MED)

`session.Close(ctx)` called `o.cacheManager.Close()`. But cacheManager
is owned by *Client* and shared across every session — closing one
subscription killed the cache for the whole client (and every other
in-flight subscription).

Fix: drop the call from `session.go`. Cache lifecycle is solely
`Client.Close`'s responsibility.

### 22.4 API event capture panic post-Close (MED)

`runShutdown` closed `apiEvents` but didn't disable the api.Client's
`EventCapture`. A late public API call (post-Close) still went
through `pushAPIEvent` → `pushDropOldest` → send-on-closed-channel =
panic.

Fix: in `runShutdown`, call `c.apiClient.SetEventCapture(EventCapture{})`
*before* `close(c.apiEvents)`. The emitter is cleared, captureBody
short-circuits, and `pushDropOldest` is never reached.

### 22.5 c.subs entries leaked across short-lived subscriptions (architectural)

Inserts at `c.subs[sub.id] = sub` had no matching delete — long-lived
clients with many short-lived subscriptions accumulated dead
*Subscription pointers until `Client.Close`.

Fix: `Subscription.runShutdown` removes itself from `c.subs` (via the
new `client *Client` back-reference). Safe regardless of whether
`Client.runShutdown` already snapshotted it — `abortWithErr` is
idempotent and the snapshot loop drives the same `runShutdown`.

### 22.6 Deferred: admission-layer refactor

The reviewer's larger suggestion — "a small lifecycle/admission layer
that owns Connect, Subscribe, replay, Close together" — is sound and
left as future work. The cross-flow bugs are all patched bounded; the
broader restructure is a substantive design change and the bugs
themselves don't require it.

## 23. v2.14 — Event-channel race + replay observability

### 23.1 Event-channel race fully closed (MED)

v2.13 cleared the API EventCapture before closing `apiEvents`, but
that only protected calls that started *after* the clear. An
in-flight emitter that already snapshotted `emit := c.capture.Emit`
under api.Client's RLock could call the (stale) function pointer
AFTER `close(c.apiEvents)` — send-on-closed-channel panic.

Fix: an `eventsMu sync.RWMutex` + `eventsClosed bool` gate now wraps
*every* event-channel push (`pushConn`, `pushRecovery`, `pushAPIEvent`).
Emitters take `RLock` + check the flag; runShutdown takes `Lock` +
sets the flag + closes the three channels as a single critical
section. Any in-flight emitter either completes before the Lock OR
runs after the Lock release, observes `eventsClosed == true`, and
returns without touching the channel.

Applied uniformly to all three channels (not just `apiEvents`)
because the gate is cheap and protects against any future bug shape
where a goroutine emitter outlives `c.wg.Wait()`.

### 23.2 Replay observability consistency (LOW/MED)

Replay-only AMQP opens called `feed.Client.Open` which fires an
`EventConnected` to the wired `onFeedEvent`. That translated to a
public `ConnectionConnected` event on `ConnectionEvents()`. Meanwhile,
`Client.ConnectionState()` was deliberately left at `NotConnected`
(per the v2.13 fix that prevents replay from poisoning recovery
init). Consumers subscribed to events would see "connected!" while
the polling getter said "not connected". Internally inconsistent.

Fix: `onFeedEvent` is now gated on `connectState != NotConnected`.
Replay-only opens (which keep state at NotConnected) suppress the
public event. Normal `Connect` runs flip state to `Connecting`
*before* the broker dial, so the gate lets through every legitimate
feed-layer transition during normal operation.

The semantic contract for `ConnectionEvents()` is now: events fire
*if and only if* the global `ConnectionState()` is past `NotConnected`.
No more split between event-layer "connected" and getter-layer "not
connected".

## 24. v2.15 — Connect emits ConnectionConnected on AMQP-reuse path

### 24.1 The gap (LOW/MED)

v2.14 closed the replay-vs-normal observability split by gating
`onFeedEvent` on `connectState != NotConnected`. That fixed the
direction "replay must not announce connected", but left the
opposite direction open: a normal `Connect()` that runs *after* a
prior replay subscription already dialed AMQP would silently skip
its `ConnectionConnected` event.

Why: `feed.Client.Open` is idempotent — the second caller observes
`c.opened == true` and returns nil immediately, never publishing an
`EventConnected` to the wired `onFeedEvent`. The gosdk-level CAS
`Connecting → Connected` still succeeded, but consumers subscribed to
`ConnectionEvents()` saw nothing for that edge. Event-layer and
state-layer drifted apart again, just on the other side.

### 24.2 Why a simple "was-already-open" predicate is wrong

The first cut at the fix captured `wasAlreadyOpen := rmq.IsOpen()`
before `rmq.Open(ctx)` and emitted `ConnectionConnected` from
`Connect`'s success path when that bool was true. That predicate is
too coarse — `IsOpen()` only reports the feed client's lifecycle
flag, not whether a feed-layer `EventConnected` will *also* arrive
during this `Connect`. Concretely:

> Replay opens AMQP, then the broker connection drops and
> `feed.Client`'s autoreconnect goroutine is dialing back. Normal
> `Connect` runs, captures `wasAlreadyOpen=true`, transitions
> `connectState → Connecting`. Mid-Connect, autoreconnect succeeds,
> fires `EventConnected` to `onFeedEvent`. The gate (`state !=
> NotConnected`) passes — `onFeedEvent` emits `ConnectionConnected`.
> `Connect` finishes, CAS `Connecting → Connected` succeeds, sees
> `wasAlreadyOpen=true`, emits `ConnectionConnected` again. Duplicate.

`IsOpen()` cannot distinguish "no event will fire" from "an event is
about to fire from autoreconnect" because that distinction lives in
the AMQP runtime, not the lifecycle flag. The fix needs a gate at
the *publication* point, not a predicate at the entry point.

### 24.3 The fix — at-most-once-per-up-edge gate

A new atomic flag `connectedEmitted` on `Client` plus a single helper
`emitConnConnectedOnce` that both publication paths route through:

```go
func (c *Client) emitConnConnectedOnce(err error) bool {
    if !c.connectedEmitted.CompareAndSwap(false, true) {
        return false
    }
    c.emitConn(ConnectionConnected, err)
    return true
}
```

The flag is the gate for the "up edge" — the transition into
`Connected` from any non-connected state. It is:

- **Cleared at the top of each `Connect` attempt** (under
  `connectMu`, while state is still `NotConnected` so concurrent
  feed events are gated out by `onFeedEvent`'s `NotConnected`
  check). Ensures a rolled-back prior attempt's stale `true` does
  not mute a fresh attempt.
- **Cleared in `onFeedEvent` on `EventDisconnected` /
  `EventReconnecting`** so a post-Connect natural reconnect's
  `Connected` emits as a fresh up edge.
- **Claimed by either `onFeedEvent`'s `EventConnected` branch OR
  `Connect`'s success path**, whichever runs first. The other call
  is a CAS no-op.

`Connect`'s success path now calls `emitConnConnectedOnce(nil)`
unconditionally after the `Connecting → Connected` CAS. The previous
`feed.Client.IsOpen()` method is removed — no caller needs it.

### 24.4 Invariant

`ConnectionEvents()` observes exactly one `ConnectionConnected` per
up edge:

- Plain normal `Connect`: `feed.Client.Open` dials and fires
  `EventConnected` → `onFeedEvent` claims the gate. `Connect`'s
  explicit emit is a no-op. ✓
- Replay-then-Connect, broker stable: `rmq.Open` is a no-op, no
  feed event fires. `Connect`'s explicit emit claims the gate. ✓
- Replay-then-Connect, broker reconnects mid-`Connect`:
  autoreconnect's `EventConnected` claims the gate first. `Connect`'s
  explicit emit is a no-op. ✓
- Post-Connect natural reconnect: `Disconnected`/`Reconnecting`
  clear the gate. The next `Connected` event re-claims and emits. ✓

### 24.5 Tests

- `TestClient_emitConnConnectedOnce_AtMostOncePerUpEdge` —
  helper-level: first call publishes, second is a no-op, post-clear
  re-emits.
- `TestClient_emitConnConnectedOnce_RaceFreeBetweenFeedAndConnect` —
  200 trials of `onFeedEvent(EventConnected)` racing the explicit
  `Connect` emit; asserts exactly one event observed under `-race`.
- `TestClient_onFeedEvent_DisconnectClearsUpEdgeGate` — verifies the
  cycle Connected → Disconnected → Reconnecting → Connected emits
  the full sequence to consumers.
- `TestClient_onFeedEvent_NotConnectedSuppressesEverything` —
  verifies replay-only events neither emit nor pre-claim the gate
  (so the first real `Connect`'s emit is never muted).

## 25. v2.16 — Lifecycle/admission layer

### 25.1 Why now

v2.0–v2.15 closed every concrete race the reviewer found, but each
fix lived in a different gate: `connectMu` for in-flight serialisation,
`subsMu` for admission, `eventsMu` for channel-send safety,
`connectState` for public state, an implicit `state==NotConnected ∧
rmq.opened==true` for "broker-only / replay open", and atomic-pointer
double-Loads for connection-layer generation pairing. Correctness
depended on six small gates agreeing across four flows (Connect,
Subscribe, replay open, Close).

The release was correct, but the surface was a future-regression
hazard: future work that touches one flow could miss the cross-flow
invariant another flow depends on. v2.16 collapses these into one
state machine.

### 25.2 What changed

**Internal `clientMode` enum** with explicit
`modeBrokerOnly` — captures replay-only state that v2.14 had to
infer from `state==NotConnected ∧ rmq.opened`. Public
`ConnectionState` is now *derived* from mode (`mode.publicState()`),
keeping the lock-free atomic mirror as a fast read for
`ConnectionState()`.

```go
type clientMode uint8
const (
    modeNew              // initial; nothing open
    modeBrokerOnly       // replay opened AMQP only; full pipeline not built
    modeNormalConnecting // ensureNormal in flight
    modeNormalReady      // full pipeline ready
    modeClosing          // Close called; runShutdown started
    modeClosed           // runShutdown finished
)
```

**`runtime` snapshot.** Generation-paired `(rmq, rmgr)` pair captured
under `lifecycleMu` and returned by `ensureBroker`/`ensureNormal`.
Removes the double-Load gymnastics in Subscribe — both the AMQP open
and the session-construction reads now operate on the same
generation, even if a concurrent rollback swaps the atomic pointers
between phases.

**Five lifecycle methods** behind one mutex (`lifecycleMu` =
renamed `connectMu`):

| method                                | role                                                                |
|---------------------------------------|---------------------------------------------------------------------|
| `ensureBroker(ctx) (runtime, error)`  | replay AMQP open; transitions to `modeBrokerOnly` from `modeNew`    |
| `ensureNormal(ctx) (runtime, error)`  | full Connect pipeline; transitions to `modeNormalReady`             |
| `admitSubscription(sub) error`        | atomic admission: re-check Closed, insert sub + `wg.Add` under mu   |
| `beginClose() chan struct{}`          | atomic Close transition: set `modeClosing`, return in-flight chan   |
| `setMode`/`snapshotRuntime`           | helpers — single writer of mode, coherent runtime snapshot          |

**Connect collapses to four lines** — public method delegates to
`ensureNormal`. The 180-line body that mixed serialisation,
mode transitions, AMQP open, recovery setup, alive session, and
emit-once now lives in one named function with a clear flow.

**Subscribe collapses too** — replay vs. normal is now a single
ternary `c.ensureBroker(ctx)` / `c.ensureNormal(ctx)` call, returning
the same `runtime` shape. Admission is a one-line
`c.admitSubscription(sub)`.

### 25.3 Invariants this preserves

The full v2.13–v2.15 set of cross-flow correctness invariants is
unchanged — every existing regression test (≈800 lines in
`review_fixes_test.go` covering closed-during-Connect,
Subscribe-after-Close, replay-not-poisoning-state, race-free
ConnectionConnected emit, event-channel race, subscribe rollback)
passes under `-race` with no modification. The tests are now
*guardrails* on the lifecycle layer: future changes that re-introduce
a cross-flow race fail concrete asserts, not vague "correctness drift".

Notably preserved:

- `ensureNormal`'s defer reverts to `priorMode` on failure (was
  `Connecting → NotConnected`); this means a rolled-back attempt that
  started from `modeBrokerOnly` correctly preserves the broker-open
  state instead of stomping it back to `modeNew` and breaking
  in-flight replay subscriptions.
- `admitSubscription` re-checks mode under `lifecycleMu` after `subs`
  insert and `wg.Add` — the same atomic admission contract that v2.13
  introduced via `subsMu`.
- `beginClose` sets `modeClosing` *before* `runShutdown` spawns, so
  ensureNormal's success-path mode-CAS still refuses to transition past
  `modeNormalConnecting` (returns `errSubscriptionClosed`).
- Up-edge gate (`connectedEmitted`) is intentionally orthogonal — it
  governs event correctness across reconnects, not lifecycle phase.

### 25.4 Public API

Unchanged. `Client.Connect`, `Client.Subscribe`, `Client.Close`,
`Client.ConnectionState`, `Client.ConnectionEvents` all keep their
signatures and observable behavior. No caller code requires changes.

## 26. v2.17 — Lifecycle layer completeness

v2.16 introduced the lifecycle/admission layer (`clientMode` +
`runtime` + `ensureBroker`/`ensureNormal`/`admitSubscription`/
`beginClose`). v2.17 closes three remaining gaps the layer didn't
fully cover.

### 26.1 F1 (HIGH) — Broker-preservation was logical, not physical

`ensureNormal` recorded `priorMode == modeBrokerOnly` and reverted the
mode on failure, but the recovery-Open and alive-Open rollback paths
unconditionally called `rmq.Close` + `resetConnectionLayer`. That
physically tore down the rmq that existing replay subscriptions held
references to via their session's `channelConsumer`. Mode said
"broker still open"; reality said "broker is gone".

Fix: new helper `rollbackPartialNormal(ctx, alive, rmgr, rmq, priorMode)`
routes all three failure paths (rmq.Open, recovery.Open, alive.Open)
through one priorMode-aware cleanup:

| priorMode       | rmq behavior                                 | rmgr behavior          |
|-----------------|----------------------------------------------|------------------------|
| `modeBrokerOnly`| **Preserved** — replay subs stay functional  | Closed and replaced    |
| `modeNew`       | Closed (bounded by `cfg.shutdownTimeout`) and replaced via `resetConnectionLayer` | Closed and replaced |

Always: `internalCancel` is invoked + nilled; `alive` is `Close`'d if
non-nil (`session.Close` is safe after a failed Open —
`channelConsumer.Close` is closeOnce-gated and nil-checks its fields).

Test: `TestClient_rollbackPartialNormal_PreservesRmqWhenBrokerOnly`
asserts `rabbitMQClient.Load()` is unchanged after a `modeBrokerOnly`
rollback; `…ResetsAllWhenModeNew` asserts both pointers swap from
`modeNew`.

### 26.2 F2 (HIGH) — `ensureBroker` was outside Close admission

v2.16's `beginClose` only fenced `ensureNormal` (via `connectDone`).
A replay-only `rt.rmq.Open(ctx)` from `ensureBroker` could be in
flight while `runShutdown` proceeded to call `rmq.Close` on the same
client. `feed.Client` mutex-serialises Open/Close internally, but the
post-Close-Open semantics were undefined and Close-during-Open could
hang or leak partial state.

Fix: new `brokerOpenWG sync.WaitGroup`. `ensureBroker` does
`Add(1)` under `lifecycleMu` after the mode-admission check (which
rejects `modeClosing`/`modeClosed`), then `defer Done()` around the
`rt.rmq.Open` call. `runShutdown` calls `brokerOpenWG.Wait()`
**before** `rmq.Close`. The two halves combined guarantee:

- `beginClose`'s mode transition under `lifecycleMu` blocks any new
  `Add` (ensureBroker returns `errSubscriptionClosed`).
- `runShutdown.Wait()` drains existing in-flight opens before
  shutting down rmq.

Tests:
- `TestClient_ensureBroker_RejectsAfterBeginClose` — admission half.
- `TestClient_runShutdown_WaitsForInFlightBrokerOpen` — fence half:
  Add 1, spawn `Close`, assert it blocks ≥75 ms, `Done`, assert it
  completes.

### 26.3 F3 (MED) — `ConnectionConnected` could fire before pipeline ready

`onFeedEvent` propagated `EventConnected` whenever
`state != NotConnected`. During `ensureNormal`'s
`modeNormalConnecting` window (publicState=Connecting), feed-layer's
`rmq.Open` dial — or an autoreconnect during recovery/alive setup —
fired `EventConnected`. `onFeedEvent` published
`ConnectionConnected` immediately, before recovery/alive could still
fail. A subsequent rollback would leave consumers having seen
`ConnectionConnected` with `ConnectionState()` back at
`NotConnected`. Observability divergence.

Fix: tighten the gate. `onFeedEvent`'s `EventConnected` branch now
publishes only when `state == Connected` (i.e., after the
`modeNormalConnecting → modeNormalReady` CAS). `ensureNormal`'s
success-path `emitConnConnectedOnce(nil)` owns the initial transition
edge. Post-Connect natural reconnects (state stays `Connected` across
feed-layer Disconnected → Reconnecting → Connected) flow through
normally.

`EventDisconnected` and `EventReconnecting` keep the older
`!= NotConnected` gate so mid-Connect broker drops remain observable.

Test: `TestClient_OnFeedEvent_SuppressedWhileConnecting` —
EventConnected during Connecting yields no event AND no
`connectedEmitted` claim; subsequent EventConnected during Connected
publishes normally.

### 26.4 Net effect

The lifecycle layer is now complete in the senses the reviewer
called out:

- **Replay broker is admitted under the same mu/state machine** as
  ensureNormal (mode + brokerOpenWG fence).
- **Broker preservation is physical**, not just logical (rmq stays
  alive across a failed `ensureNormal` from `modeBrokerOnly`).
- **Public ConnectionConnected fires only when the SDK can actually
  serve traffic** (post-modeNormalReady), regardless of whether the
  feed-layer dialed during Connecting.

Public API still unchanged. Existing v2.13–v2.16 regression tests pass
under `-race` alongside the four new F1/F2/F3 tests.

## 27. v2.18 — Replay sequenced after in-flight normal Connect

### 27.1 The remaining cross-flow case

v2.17 closed F1's "preserve rmq when priorMode == modeBrokerOnly"
correctly, but only for the case where replay had **already**
reached modeBrokerOnly before normal Connect started. The remaining
gap: replay arriving **during** modeNormalConnecting that started
from modeNew.

Sequence:

1. modeNew. ensureNormal starts: priorMode = modeNew, mode →
   modeNormalConnecting.
2. ensureBroker arrives, took the v2.17 reuse path
   (modeNormalConnecting was in the reuse case-list), snapshotted rt,
   called `rt.rmq.Open(ctx)` — succeeded by piggy-backing on the
   in-flight ensureNormal's open.
3. Replay session built and admitted — now uses rt.rmq.
4. ensureNormal recovery/alive setup fails. rollbackPartialNormal runs
   with priorMode = modeNew → closes rmq + resetConnectionLayer.
5. Replay subscription is silently attached to a closed broker.

The mode rollback was correct (Connecting → New); rmq's physical
state was correct for modeNew (replaced); but the replay sub admitted
in step 3 dangled.

### 27.2 The fix

`ensureBroker` now **waits** for `connectDone` while
`mode == modeNormalConnecting`, looping if a fresh ensureNormal
starts before re-acquire. After the wait, mode is one of:

- `modeNew` (rollback from priorMode=modeNew → fresh rmq via
  resetConnectionLayer): replay transitions modeNew → modeBrokerOnly,
  dials the fresh rmq.
- `modeBrokerOnly` (rollback from priorMode=modeBrokerOnly → rmq
  preserved by v2.17's F1): replay reuse-path, rmq.Open is no-op.
- `modeNormalReady` (success): replay reuse-path, rmq.Open is no-op.
- `modeClosing`/`modeClosed` (Close raced in): return
  errSubscriptionClosed.

Replay is sequenced after the in-flight ensureNormal in every case.
It never adopts an rmq that's about to be torn down.

```go
for c.mode == modeNormalConnecting {
    done := c.connectDone
    c.lifecycleMu.Unlock()
    select {
    case <-done:
    case <-ctx.Done():
        return runtime{}, ctx.Err()
    }
    c.lifecycleMu.Lock()
}
```

The wait is bounded by the caller's ctx (replay Subscribe is
already ctx-scoped). The loop drains consecutive ensureNormal
attempts that might overlap.

### 27.3 Why "wait" rather than "track replay use during Connect"

The reviewer offered two fix paths:

- **A.** ensureBroker waits for connectDone, then re-enters.
- **B.** Track "replay retained during normal connect" under
  lifecycleMu so normal rollback preserves rmq.

We picked A. Rationale:

- A makes the cross-flow invariant **structural** (replay never
  proceeds during Connecting) instead of recoverable (rollback
  detects replay use and adapts). Structural invariants are easier
  to preserve under future change.
- B requires tracking a counter or flag that follows replay subs
  from ensureBroker through session.Open, admission, and beyond.
  Each transition is a place to introduce a new bug.
- A's only cost is a small replay-Subscribe latency hit when a
  normal Connect is in flight (typically seconds). In practice
  replay is rarely co-issued with Connect, and Subscribe ctx
  bounds the wait.

### 27.4 Stale race test refreshed

`TestClient_emitConnConnectedOnce_RaceFreeBetweenFeedAndConnect`
staged its 200-trial race at `state == Connecting`, but v2.17's F3
suppresses feed-layer Connected during Connecting — so the
feed-layer side became a no-op and the CAS never raced.

Renamed to `TestClient_emitConnConnectedOnce_RaceFreeOnReconnect`
and re-staged at `state == Connected` with the gate cleared
(simulating the post-Connect natural reconnect cycle). Now both
sides actually try to publish, so the CAS gate is exercised.

### 27.5 Cross-flow regression test

`TestClient_ensureBroker_WaitsForInFlightEnsureNormal`:

1. Stage: mode = modeNormalConnecting, connectDone open.
2. Spawn ensureBroker; verify it blocks ≥75 ms (proves the wait).
3. Simulate Close: setMode(modeClosing) + close(connectDone).
4. Verify ensureBroker returns errSubscriptionClosed (proves it
   observed the post-wakeup mode, not the modeNormalConnecting it
   entered with).

Together with v2.17's F1 preservation tests (rmq pointer unchanged
after modeBrokerOnly rollback) and F2 admission/fence tests, this
proves the cross-flow invariant: replay either adopts a stable
post-Connect rmq, or is rejected if Close races in — never adopts
an about-to-be-replaced one.

## 28. v2.19 — API/feed correctness pass (5 review findings)

### 28.1 F1 (HIGH) — Specific-event subscriptions were silently AllMessages

`Subscribe` pre-defaulted `messageInterest = AllMessageInterest`,
then ran options, then ran a "if interest == empty, default to All"
fallback. `WithSpecificEvents` only switched to
`SpecifiedMatchesOnlyMessageInterest` if the current interest was
"" — but `SpecifiedMatchesOnlyMessageInterest` IS "". So the
post-options fallback stomped the explicit specified-events choice
back to All, and the documented migration sample
`Subscribe(WithSpecificEvents(eventID))` silently subscribed to
*every* message rather than the requested events.

Fix: explicit `messageInterestSet bool` on `subscribeConfig` —
distinguishes "unset (zero value)" from "specified-events (the empty
string is the routing-key sentinel)". Both `WithMessageInterest` and
`WithSpecificEvents` set the flag; the post-options fallback now
checks the flag, not the string.

Tests:
- `TestSubscribeOptions_NoOptions_DefaultsToAll` — baseline.
- `TestSubscribeOptions_WithSpecificEvents_ImpliesSpecifiedMatchesOnly` —
  explicit specified-events survive.
- `TestSubscribeOptions_WithMessageInterestAll_PlusWithSpecificEvents` —
  the migration shape; option order doesn't matter.
- `TestRoutingKeys_SpecifiedMatchesOnly_BuildsPerEventKeys` —
  downstream routing-key derivation produces per-event keys.

### 28.2 F2 (HIGH) — Feed-message timestamps weren't Java/.NET compatible

Two distinct defects:

1. `MessageTimestamp.Created` was set from the AMQP delivery timestamp
   (when the broker dispatched). Java sets it from the XML message's
   own `Timestamp()` (when the feed *generated* the event); .NET's
   `GeneratedAt` is the same. Cross-SDK consumers that dedup by
   Created saw inconsistent values.
2. `_unmarshalUnixTime` parsed XML-encoded UNIX-millisecond
   timestamps via `time.Unix(sec/1000, 0)` — integer-divided away
   millisecond precision. Java/.NET preserve full ms.

Fix:
- `processDelivery` initialises `Sent` from the AMQP delivery
  timestamp and `Received` from `time.Now()`. After successful XML
  decode it sets `Created = message.Timestamp()`. If the AMQP delivery
  timestamp is zero, `Sent` falls back to `Created` so it's never an
  unintentional sentinel.
- `_unmarshalUnixTime` switches to `time.UnixMilli(ms)` —
  millisecond precision preserved.

Tests:
- `TestTimestamp_UnmarshalPreservesMilliseconds` — verifies
  `1736899200123` parses with `Nanosecond() == 123_000_000`.

### 28.3 F3 (MED) — Malformed system messages could panic the consumer

`BuildUnparsableMessage` unconditionally read
`feedMessage.RoutingKey.EventID.Type` and dereferenced `EventID`/
`SportID`. System routing keys (alive, snapshot_complete, etc.)
leave both nil — a malformed system message hitting the unparsable
path crashed the consumer goroutine. The function also returned
`timestamp: types.MessageTimestamp{}` despite computing
`timestamp.Published`, discarding both Published and the upstream
Created/Sent/Received.

Fix:
- Nil-check `RoutingKey` and `EventID` before dereferencing; leave
  `event` nil for system routing keys (and unknown types).
- Return the computed timestamp instead of the zero value.

Tests:
- `TestBuildUnparsableMessage_SystemRoutingKey_NilSafe` — system
  routing key with nil EventID/SportID doesn't panic.
- `TestBuildUnparsableMessage_NilRoutingKey_NilSafe` — defensive
  shape for nil RoutingKey at all.
- `TestBuildUnparsableMessage_PreservesUpstreamTimestamp` — the
  upstream Created/Sent/Received survive; Published is non-zero.

### 28.4 F4 (LOW) — APILogFull didn't actually capture request bytes

`APILogFull` was wired through `EventCapture.RequestBody = true` and
documented as "request and response bytes", but the emit path only
filled `APIEvent.Response`. `APIEvent.Request` was permanently empty.

All current SDK API paths are bodyless (`GET`, `doNoBody POST/PUT`),
so the contract was a code smell rather than a production bug — but
future endpoints with bodies would silently fail the contract.

Fix:
- New `captureRequestBytes(req)` drains `req.Body` into a redacted,
  length-bounded snapshot, then restores `req.Body` (via
  `io.NopCloser` + `bytes.NewReader`) and `req.GetBody` so the http
  transport (and retries) still send the body normally.
- Threaded through `do()`'s emit chain (new `emitEventReqResp`
  consolidating request + response bytes; old `emitEvent`/
  `emitEventBytesLocale` now route through it).
- `APIEvent.Request` and a new `APIEvent.RequestTruncated` propagate
  to `gosdk.APIEvent`.

Tests:
- `TestCaptureRequestBytes_FillsAPIEventRequest` — drains, restores,
  GetBody replays.
- `TestCaptureRequestBytes_TruncatesAtBodyLimit` — captured snapshot
  truncates; transport still sees full body.
- `TestCaptureRequestBytes_NoOpWhenDisabled` — `RequestBody=false`
  remains zero-cost (preserves APILogResponses behaviour).
- `TestCaptureRequestBytes_NilBodyIsSafe` — every current SDK call
  stays a no-op.

### 28.5 F5 (LOW) — Replay event IDs need a no-resolution alternative

.NET's `IReplayManager.GetEventsInQueue` returns the queued event
URNs without resolving each to a Match. Go's
`Replay.List → ReplayList` only exposed the resolved-Match form,
forcing callers who only need "is X queued?" to pay one sports-info
round-trip per queued event.

Fix:
- New `Replay.EventIDs(ctx) ([]types.URN, error)` and
  `ReplayManager.ReplayEventIDs(ctx)` that fetch the same XML payload
  but skip the Match build.

Test: `TestReplay_ReplayEventIDs_NoSportsInfoCalls` — verifies the
returned URNs match the XML and that `sportsInfoManager.Match` is
called *zero* times.

## 29. v2.20 — AMQP/recovery/feed correctness pass (6 review findings)

### 29.1 F1 (HIGH) — AMQP dial wasn't actually bounded by caller ctx

`internal/feed/client.go` threaded `ctx` into `dial()`, but
`amqp.DialConfig` was called WITHOUT a custom `Dial` callback, so the
amqp091-go library used its own 30 s deadline and ignored the ctx. A
short `Connect(ctx)` / lazy `Subscribe(ctx)` could block much longer
than the caller asked for, and `Client.Close` waited for in-flight
connect work before its shutdown budget could apply.

Fix: new `newCtxBoundDialer(ctx)` returns an `amqp.Config.Dial`
closure that:
- Uses `net.Dialer.DialContext(ctx, ...)` for the TCP connect.
- Sets `conn.SetDeadline(ctx.Deadline())` (or a 30 s fallback when
  ctx has no deadline) so the post-TCP AMQP protocol handshake is
  bounded too. The library clears this deadline once it installs
  heartbeat-based I/O timeouts, so steady-state reads are
  unaffected.

Test: `TestNewCtxBoundDialer_RespectsCtxDeadline` dials TEST-NET-1
(RFC 5737, unroutable) with a 100 ms ctx and asserts the dial
returns within <1 s — far under the 30 s default that would have
applied without the fix. `TestNewCtxBoundDialer_FallbackDeadlineWhenNoCtxDeadline`
covers the no-deadline branch.

### 29.2 F2 (HIGH) — Recovery event API calls outlived shutdown budget

`runShutdown` called `rmgr.Close()` before creating the shutdown
timeout context. `Manager.cleanup` synchronously stopped actors;
`actor.stop()` waited unbounded on `<-a.done`. If a recovery-event
HTTP call (`PostEventStatefulRecovery` / `PostEventOddsRecovery`)
was in-flight using the *caller's* ctx (e.g. a long-lived
`ctx.Background()`), `dispatch` was blocked waiting for the HTTP
call, the actor's lifetime ctx cancellation had no effect on it,
and `Close` waited past `WithShutdownTimeout`.

Fix: `onRecoverEvent` now derives the API request ctx from the
caller's ctx but registers `context.AfterFunc(a.ctx, cancel)` so
cancellation of the actor lifetime propagates into the in-flight
request. When `Manager.cleanup` cancels the session ctx (which IS
`a.ctx`), the API call returns `context.Canceled` and the actor
drains promptly. `PostRecovery` (snapshot recovery) already used
`a.ctx` directly and was unaffected.

Test: `TestActor_OnRecoverEvent_LifecycleCtxCancelsBlockedAPICall`
stages a hanging HTTP fixture, spawns `onRecoverEvent` with a
long-lived caller ctx, asserts it blocks, cancels the actor
lifetime ctx, and asserts the goroutine returns within <1 s with
a non-nil err.

### 29.3 F3 (MED) — Public variadic-locale methods discarded extras

The migration docs promised multi-locale preloading, but `client.go`
wrappers like `ActiveTournaments(ctx, locales...)`, `Match`,
`Competitor`, `MatchesFor`, etc. all called
`localeOrDefault(locales)` (singular) which dropped everything past
`locales[0]`. The cache/factory layer already supported
multi-locale; this was a public-wrapper plumbing gap.

Fix: new `localesOrDefault(locales []Locale) []Locale` helper on
Client. New `MultiLocalized*` parallel methods on
`types.SportsInfoManager` and `internal/sport.Manager` that thread
the full slice through `entityFactory.BuildMatch / BuildTournament /
BuildCompetitor` (which already accepted `[]Locale`). Existing
single-locale `LocalizedX(ctx, locale)` methods now thin-wrap the
multi variants for backward compatibility.

Public Client methods updated:
`ActiveTournaments`, `AvailableTournaments`, `Match`, `MatchesFor`,
`LiveMatches`, `ListMatches`, `Competitor`, `FixtureChanges`. Each
preloads every supplied locale into the returned entity's `Names`
map, matching Java/.NET preload semantics.

Tests: `TestLocalesOrDefault_EmptyReturnsDefaultSingleton`,
`TestLocalesOrDefault_PreservesAllSupplied`.

### 29.4 F4 (HIGH) — BuildMessage panicked on malformed routing keys

`BuildMessage` dereferenced `feedMessage.RoutingKey.EventID.Type`
and the inner `*EventID` / `*SportID` fields without nil-checks.
`parseRoute` can return non-system `RoutingKeyInfo` with `EventID
== nil` for malformed 8-part routes (sportID present, event
type/id empty), so a malformed-but-parseable route panicked the
consumer goroutine.

Fix: `BuildMessage` now nil-checks `RoutingKey`, `EventID`, and
`SportID` (where required for the tournament branch) and returns
an error instead of dereferencing. Sessions handle returned errors
by emitting `UnparsableMessage` to consumers.

Tests: `TestBuildMessage_NilRoutingKeyReturnsError`,
`TestBuildMessage_NilEventIDDoesNotPanic`. (v2.19 added similar
guards on `BuildUnparsableMessage` for the system-routing-key
shape; v2.20 covers the parseable-but-malformed shape on
`BuildMessage`.)

### 29.5 F5 (MED) — Sport-name active tournaments missing + locale bug

.NET exposes `ISportDataProvider.GetActiveTournaments(name, ...)`
and Java exposes `SportsInfoManager.getActiveTournaments(sportName,
locale)`. Go had the internal manager interface but no public
`Client` method. Worse, the existing internal implementation
called `m.Sports(ctx)` (default locale) before checking
`sport.Name(locale)` — non-default-locale lookups failed unless
the requested locale's catalog was already cached.

Fix:
- New public `Client.ActiveTournamentsForSport(ctx, sportName,
  locales...)`.
- New `MultiLocalizedSportActiveTournaments` on the manager that
  iterates the supplied locales, fetching each catalog if needed
  and matching the sport name against that locale's `Names`.
- The first match wins; tournaments are returned with all locales
  preloaded.

Test: `TestClient_ActiveTournamentsForSport_PublicMethodExists`
(compile-time existence). Behavioural coverage of the locale
search lives in `internal/sport` against the HTTP fixture stack.

### 29.6 F6 (LOW) — Docs referenced Client.Tournament but no method existed

`MIGRATION.md` §3.5 and `types/sport_event.go` directed users to
`client.Tournament(ctx, urn)` for resolving individual tournaments
from `sport.TournamentIDs`, but no such public method existed.
`BuildTournament` on the entity factory required a `sportID` the
caller didn't have on hand.

Fix: new `cache.TournamentCache.SportIDFor(ctx, id, locales)` that
resolves the cached sportID after a fetch (the `/tournaments/{id}/info`
endpoint includes the sport URN). New manager method
`MultiLocalizedTournament(ctx, id, locales)` uses it to infer
sportID then call `BuildTournament`. New public
`Client.Tournament(ctx, id, locales...)` — matches the documented
migration path.

Test: `TestClient_Tournament_PublicMethodExists` (compile-time
existence).

### 29.7 Public API additions (v2.20)

- `Client.ActiveTournamentsForSport(ctx, sportName, locales...)`
- `Client.Tournament(ctx, id, locales...)`
- `APIEvent` field changes: none new in v2.20 (v2.19 added
  `RequestTruncated`).
- `types.SportsInfoManager`: 9 new `MultiLocalized*` methods
  (additive — single-locale methods retained as backward-compat
  wrappers).

No public surface was removed or had its signature changed.

## 30. v2.21 — v2.20 follow-ups (4 review findings)

### 30.1 F1 — BuildMessage silently dispatched malformed routes

v2.20's F4 nil-checked `RoutingKey.EventID` to stop the panic but
left a `// rk.EventID == nil: leave event nil` branch that
silently dispatched the message with `event == nil`. The
regression test had a *secondary* `defer func() { _ = recover() }()`
that swallowed any further panic, masking the behavioural
divergence: the test was a panic-only guard, not a proper
correctness check.

The reviewer's analysis is correct: legitimate nil-EventID feed
messages (Alive, SnapshotComplete) are routed elsewhere and never
reach `BuildMessage`. A non-system route arriving at `BuildMessage`
with `EventID == nil` is corruption.

Fix: `BuildMessage` now returns a descriptive error when
`RoutingKey == nil` or `RoutingKey.EventID == nil`. The session
converts the returned error into `UnparsableMessage` so consumers
observe the malformed delivery instead of seeing a phantom
event-less message.

The regression test is rewritten without the masking second-recover:
`TestBuildMessage_NilEventIDReturnsError` asserts the error path
without needing a producer-manager fixture (the early-error return
short-circuits before reaching `producerManager.GetProducerCached`).

### 30.2 F2 — TLS/AMQP handshake cancellation on no-deadline ctx

v2.20's F1 set `conn.SetDeadline(ctx.Deadline())` to bound the
post-TCP AMQP handshake, but `SetDeadline` is wall-clock; a
cancelable-without-deadline ctx (e.g., one driven by Close)
doesn't propagate. The handshake ran for up to the 30 s fallback
even after cancellation.

Fix: `dial()` now spawns a watcher goroutine that observes
`ctx.Done()` and forcibly `Close()`s the dialed conn while the
handshake is in flight. The watcher is paired with a
`stopWatcher` chan that signals once `amqp.DialConfig` returns
(handshake done — the conn is now owned by the library and its
heartbeat-based timeouts take over). A `dialedConn` capture on
the Dial closure plus a small mutex+`handshakeDone` flag keep the
state coherent.

`newCtxBoundDialer` is now a thin wrapper around
`newCtxBoundDialerWithCapture(ctx, nil)`; the captured-conn variant
is used by `dial()` so the watcher can find the conn.

Test: `TestDialWatcher_AbortsHandshakeOnCtxCancel` — silent TCP
listener (broker stuck mid-handshake), no-deadline cancelable ctx,
cancel mid-handshake; assertion that the watcher closes the conn
within <250 ms.

### 30.3 F3 — MarketDescriptions multi-locale

v2.20's F3 added `MultiLocalized*` variants for sport / match /
competitor / fixture-change / live-match / list / available-
tournament / active-tournament methods, but
`MarketDescriptions(ctx, locales...)` still discarded extras via
`localeOrDefault`. .NET / Java expose this as single-locale, but
Go's variadic-locale promise made the existing signature suggest
multi-locale support that wasn't there.

Fix:
- `cache.MarketDescriptionCache.MultiLocalizedMarketDescriptions(ctx, locales)` —
  loads every supplied locale (skipping ones already loaded), returns
  the catalog gated by the primary locale.
- `factory.MarketDescriptionFactory.MultiMarketDescriptions(ctx, locales)` —
  thin wrapper that snapshots the cache map.
- `market.Manager.MultiLocalizedMarketDescriptions(ctx, locales)` —
  surfaced on `types.MarketDescriptionManager`.
- `Client.MarketDescriptions(ctx, locales...)` calls the multi
  variant so every supplied locale preloads. The returned slice is
  snapshotted against the primary locale; each entry's
  `Names` + outcome `Names` / `Descriptions` maps include all
  supplied locales.

### 30.4 F4 — NEXT.md API sketch missing v2.20 additions

`MIGRATION.md` documented `Client.ActiveTournamentsForSport` and
`Client.Tournament` (added in v2.20 F5/F6) but `NEXT.md` §1's
public-API sketch was never updated. Added both signatures to the
sketch with a one-line cross-reference to MIGRATION §29.5/§29.6.

## 31. v2.22 — Dial cancellation: result-channel pattern

### 31.1 Two race windows in the v2.21 watcher pattern

v2.21 added a watcher goroutine that closed the dialed conn on
`ctx.Done()` to abort an in-flight TLS / AMQP handshake. The
reviewer flagged two narrow but real races at the success/cancel
boundary:

**Window 1.** ctx fires after `d.DialContext` returns the TCP conn
but BEFORE the Dial closure's `capture(conn)` runs. The watcher
wakes, takes `dialMu`, sees `dialedConn == nil`, and exits. Capture
then fires — but no one is left to close on cancel. The handshake
runs unbounded until the SetDeadline fallback (~30 s for
no-deadline ctxs).

**Window 2.** `amqp.DialConfig` returns success, then ctx fires
before the main goroutine sets `handshakeDone = true`. The watcher
wakes, sees `handshakeDone == false` and a non-nil `dialedConn`,
and closes the just-handed-off conn. `dial()` then returns the
(now-closed) conn with `err == nil`. Caller has a closed
connection and no error.

### 31.2 Result-channel pattern

The watcher goroutine + shared-state handoff is replaced with a
result-channel + cancellation-aware capture. The orchestration is
extracted into a testable helper:

```go
func orchestrateCtxBoundedDial(
    ctx context.Context,
    host string,
    do func(capture func(net.Conn)) (*amqp.Connection, error),
) (*amqp.Connection, error)
```

How it eliminates both races:

- **Cancellation-aware capture.** The capture closure stores the
  conn under `dialMu`, then re-checks the `cancelled` flag in the
  same critical section. If cancellation arrived before capture,
  the closure closes the conn itself before returning — so the
  handshake aborts even if the main goroutine hasn't observed
  ctx.Done yet. Window 1 closed.

- **Atomic select.** The main goroutine selects on `resultCh` vs
  `ctx.Done`. After a branch commits, the other side is
  well-defined: if `resultCh` wins, we return its outcome verbatim
  — no late cancel can touch the handed-off conn. If `ctx.Done`
  wins, we close any captured raw conn AND drain `resultCh`; if
  the library returned a fully-built `*amqp.Connection` in the
  tiny window before our select committed, we close that too so
  its heartbeat goroutine doesn't leak. Window 2 closed.

No detached watcher goroutine. The cancellation path and the
capture path serialise on `dialMu`; the success path doesn't touch
`dialMu` after the select commitment. Result: every dial returns
either `(conn, nil)` with a fully-owned working conn, or
`(nil, ctx-wrapped-error)` with no leaked resources.

### 31.3 Tests

The stale `TestDialWatcher_AbortsHandshakeOnCtxCancel` (which
replicated the v2.21 watcher pattern in the test itself, not in
production code) is replaced by direct tests of the orchestration
helper:

- `TestOrchestrateCtxBoundedDial_SuccessNoCancel` — happy path.
- `TestOrchestrateCtxBoundedDial_CancelBeforeCapture` — Window 1:
  cancel arrives before capture; the cancellation-aware capture
  closes the conn itself.
- `TestOrchestrateCtxBoundedDial_CancelAfterCapture` — capture
  has stored the conn; cancel arrives mid-handshake; main
  goroutine closes the captured conn.
- `TestOrchestrateCtxBoundedDial_WindowTwoStress` — 500-trial
  race-stress around the success/cancel boundary. Asserts every
  iteration either returns `(nil-conn-from-do, nil-err)` cleanly
  OR `(nil, ctx.Canceled-wrapped)` with no late close interference.
  Combined with `-race`, surfaces double-close / leaked-goroutine
  regressions.

A lightweight `fakeNetConn` (records whether `Close` was called)
stands in for a real net.Conn so we can drive timing precisely
without standing up a TCP listener and reasoning about scheduler-
level behaviour.

## 32. v2.23 — Producer/feed/cache/api correctness pass (4 review findings)

### 32.1 F1 (MED) — Producer overrides lost on Connect

`SetProducerEnabled` and `SetProducerRecoveryFromTimestamp` mutate
in-memory producer state. But `Connect` calls
`producerManager.Open` as part of `ensureNormal`, and Open
unconditionally rebuilt the entire `producerMap` with fresh
`newData` entries — silently stomping any caller-owned overrides
set BEFORE the first Connect (a documented usage pattern).

Fix: `Open` now snapshots the existing map under RLock and copies
caller-owned mutable state (enabled, flaggedDown,
lastMessageTimestamp, lastProcessedMessageGenTimestamp,
lastAliveReceivedGenTimestamp, recoveryFromTimestamp,
lastRecoveryInfo) onto the fresh entries before installing the new
map. Catalog fields (name/description/scope/active) are refreshed
from the API. Producers absent from the API on a re-Open are
dropped (the catalog is authoritative for "exists").

We don't mutate existing entries in place — the data struct
documents its catalog fields as "immutable after newData", and
`buildProducerImpl` reads them without holding `data.mu`. Building
a fresh `*data` and copying preserves that contract.

Tests:
- `TestManager_Open_PreservesCallerOwnedState` — disable producer 1
  + set recovery-from on producer 2; re-Open; assert both
  overrides survive.
- `TestManager_Open_DropsProducersAbsentFromAPI` — first Open
  returns 4 producers, second returns 2; assert the disappeared
  producers are dropped.

### 32.2 F2 (MED) — Malformed routing keys ack-and-dropped

`processDelivery`'s function comment says "routing-key parse
failures are admitted to the buffer (consumer wants to know)",
but the implementation acked-and-dropped — consumers never saw
the malformed delivery.

Fix: malformed routes now build an `UnparsableMessage` with a
minimal `RoutingKeyInfo{FullRoutingKey: d.RoutingKey}` (EventID
and SportID nil — `BuildUnparsableMessage` nil-checks both since
v2.19). `processDelivery` does NOT ack on this path; the caller
in `consume` acks after admission to the outgoing buffer, matching
the function contract.

Test: `TestProcessDelivery_MalformedRouteAdmitsAsUnparsable` —
malformed `garbage.route` delivery; assert non-nil QueueMessage
with UnparsableMessage populated, FeedMessage/RawFeedMessage nil,
and the recording Acknowledger NOT called from inside
processDelivery.

### 32.3 F3 (LOW/MED) — Tournament empty-competitor false negative

`BuildTournament` used `len(item.competitorIDList()) == 0` as a
"not yet loaded" signal, triggering `tc.lru.Clear(id)` +
re-fetch on every build for tournaments that legitimately have
zero competitors.

Fix: explicit `competitorsLoaded bool` on `LocalizedTournament`,
set by `merge()` only when the wrapper is a
`TournamentExtendedWrapper` (the only payload shape that carries
the competitor list). `BuildTournament` now keys on
`item.competitorsAreLoaded()` instead of slice length, so empty-
competitor tournaments don't trigger an infinite refetch loop.

Tests:
- `TestMerge_ExtendedPayloadFlagsCompetitorsLoaded` — extended
  payload with empty competitor list still flags loaded.
- `TestMerge_ExtendedPayloadWithCompetitorsLoadsList` — happy path.
- `TestMerge_NonExtendedPayloadDoesNotFlagLoaded` — bare
  `/tournaments/{id}/info` response leaves the flag false so the
  /competitors fetch still happens on first build.

### 32.4 F4 (LOW) — Stale `api.Client.Open` / `msgCh` send-vs-close

`Open() <-chan types.Response` and the paired `msgCh` field were
a Phase-6 retired path with no production callers. The
`fetchData` send path snapshotted `msgCh` under RLock then sent
outside the lock — racing against `Close()` which closed `msgCh`
under Lock. A future caller reusing the path could panic on
send-on-closed-channel.

Fix: deleted. `Open() <-chan types.Response` removed; `msgCh`
field removed; `Close()` no longer closes a channel; `fetchData`
no longer dispatches to one. Observer-callback path remains for
the cache layer (`SubscribeWithAPIObserver`) — that path takes
RLock, snapshots `observers + closed`, releases, calls observers
synchronously. No goroutine outlives the snapshot, no channel to
race.

`types.Response` is still referenced by `Observer.OnAPIResponse`
and the response struct used internally — the deletion is scoped
to the unused channel-streaming path.

## 33. v2.24 — Critical/high review-driven correctness pass

(See commit `645c398`. 22 fixes — five Critical, seventeen High —
covering feed/recovery races, cache stampedes, cache build correctness,
public-API parity gaps, HTTP-retry idempotency, and the
`RecoveryEvent` discriminator. Mostly internal; the `RecoveryEvent`
struct grew a `Kind` field, `ClearMatch` semantics changed to
invalidate fixture+status alongside the match summary, and `Sport`/
`Player`/`Sports` now plumb multi-locale through to the cache.)

## 34. v2.25 — Review follow-ups (correctness + lint)

(See commit `695e12e`. Six fixes: mixed-scope producer recovery
validation, `ClearSport` and `ClearMarketDescription` now invalidate
loaded-locale markers, producer Open vs setter race, snapshot
pointer/map aliasing on Fixture/MatchStatus/MarketDescription via
shallow `clonePtr` helpers, contextcheck lint cleanup. No public-API
changes.)

## 35. v2.26 — `types.Optional[T]` (closes inner-pointer aliasing)

### What changed (and why)

The v2.25 shallow-clone of `MatchStatus.Scoreboard` / `Statistics`
decoupled the OUTER pointer from the cache, but the inner
`*uint32` / `*int32` / `*bool` fields still aliased the cache's
pointees. A consumer doing `*status.Scoreboard.HomeKills = 999` would
mutate the cache for every other reader of that Scoreboard.

v2.26 introduces `types.Optional[T]` — a value-type optional wrapper
— and migrates the inner pointer fields on `Scoreboard`, `Statistics`,
`PeriodScore`, plus the numeric optionals on `MatchStatus`
(`MatchStatusID`, `HomeScore`, `AwayScore`). With value semantics, a
shallow copy of `Scoreboard` is now fully decoupled from the cache —
the bug class disappears.

### `Optional[T]` API

```go
package types

type Optional[T any] struct { /* unexported */ }

func Some[T any](v T) Optional[T]          // construct a set value
func None[T any]() Optional[T]             // construct an unset value
func FromPtr[T any](p *T) Optional[T]      // *T → Optional, copies pointee

func (o Optional[T]) Get() (T, bool)       // (value, set) — check set
func (o Optional[T]) IsSet() bool          // is a value present?
func (o Optional[T]) Value() T             // value (zero when unset)
func (o Optional[T]) ValueOr(def T) T      // value or default
func (o Optional[T]) Ptr() *T              // back to *T (allocates on Some)
func (o Optional[T]) String() string       // for fmt — "<unset>" or %v
```

JSON: `MarshalJSON` emits the held value or `null`; `UnmarshalJSON`
reads `null` as None. Wire shape for present fields is unchanged.
**Caveat:** Go's `encoding/json` doesn't recognize a zero-value
`Optional[T]` as "empty" for `omitempty` purposes — unset fields are
emitted as `null` instead of being omitted. If your downstream
consumer requires field absence (not null), filter at the emit layer.

XML: not migrated. The `internal/api/xml` and `internal/feed/xml`
packages keep `*T` for upstream-feed decoding; conversion happens at
the cache→types boundary via `FromPtr`.

### Fields migrated

| Type | Field | Before | After |
| --- | --- | --- | --- |
| `Scoreboard` | every inner numeric/bool (35 fields) | `*uint32`/`*int32`/`*bool` | `Optional[uint32]`/`Optional[int32]`/`Optional[bool]` |
| `Statistics` | every inner numeric (8 fields) | `*uint32` | `Optional[uint32]` |
| `PeriodScore` | every inner numeric/bool (15 fields) | `*uint32`/`*int32`/`*bool` | `Optional[uint32]`/`Optional[int32]`/`Optional[bool]` |
| `MatchStatus` | `MatchStatusID` | `*uint` | `Optional[uint]` |
| `MatchStatus` | `HomeScore` | `*float64` | `Optional[float64]` |
| `MatchStatus` | `AwayScore` | `*float64` | `Optional[float64]` |

`MatchStatus.WinnerID` (`*URN`), `MatchStatus.Scoreboard`/`Statistics`/
`StatusDescription` (structural pointers to wholesale-optional structs)
stay as `*T`. Their inner fields are now value-style, so
`clonePtr(entry.scoreboard)` (added in v2.25) yields a fully
decoupled snapshot.

### Migration guide for consumers

Read sites — pre/post:

```go
// Before (v2.25):
if s.Scoreboard != nil && s.Scoreboard.HomeKills != nil {
    fmt.Println(*s.Scoreboard.HomeKills)
}

// After (v2.26):
if s.Scoreboard != nil {
    if v, ok := s.Scoreboard.HomeKills.Get(); ok {
        fmt.Println(v)
    }
}
// Or default-zero:
fmt.Println(s.Scoreboard.HomeKills.ValueOr(0))
```

If you absolutely need `*T` (e.g., to pass to a function that takes
`*uint32`), call `.Ptr()` — but prefer `Get()` / `Value()` /
`ValueOr()` to avoid the allocation:

```go
fn(s.Scoreboard.HomeKills.Ptr()) // legal; allocates a fresh *uint32
```

Write sites: `Some(v)`, `None[T]()`, or `FromPtr(p)`:

```go
// Construction:
sb := types.Scoreboard{HomeKills: types.Some[int32](10)}

// From a *T (e.g. XML decode):
sb := types.Scoreboard{HomeKills: types.FromPtr(decoded.HomeKills)}
```

### Future migrations (separate commits)

Several other `*T` optionality fields remain across the SDK and are
candidates for follow-up `Optional[T]` migration:

- `OutcomeOdds.Probability` (`*float32`), `OutcomeOdds.DecimalOdds`
  (`*float32`), `OutcomeOdds.IsFavourite` (`*bool`).
- `MarketCancel.VoidReasonID` (`*uint`), `VoidReasonParams`
  (`*string`).
- `MarketDescription.Variant` / `IncludesOutcomesOfType` /
  `OutcomeType` (`*string`).
- `ReplayPlayParams.Speed` / `MaxDelayInMs` / `RunParallel` /
  `RewriteTimestamps` / `Producer`.
- `Competitor.IconPath` (`*string`).
- `RecoveryInfo.NodeID` (`*int`).
- `RequestMessage.RequestID()` (`*uint`) — interface method on every
  message type. Biggest-blast-radius migration; deserves its own
  release.

These were left as-is in v2.26 to keep the scope of this commit
focused on the inner-pointer aliasing bug.

## 36. v2.27 — `Optional[T]` migration: markets/odds cluster

Continues the v2.26 `Optional[T]` migration into the markets/odds
struct fields. Same value-semantics rationale as v2.26 (no aliasing,
no per-field heap allocation, self-documenting at the call site).

### Fields migrated

| Type | Field | Before | After |
| --- | --- | --- | --- |
| `OutcomeOdds` | `Probability` | `*float32` | `Optional[float32]` |
| `OutcomeOdds` | `DecimalOdds` | `*float32` | `Optional[float32]` |
| `OutcomeSettlement` | `VoidFactor` | `*VoidFactor` | `Optional[VoidFactor]` |
| `MarketWithOdds` | `IsFavourite` | `*bool` | `Optional[bool]` |
| `MarketCancel` | `VoidReasonID` | `*uint` | `Optional[uint]` |
| `MarketCancel` | `VoidReasonParams` | `*string` | `Optional[string]` |

### API change: `OutcomeOdds.Odds()` return type

`OutcomeOdds.Odds(displayType OddsDisplayType)` now returns
`Optional[float32]` instead of `*float32`. The internal helper
`convertToAmericanOdds` accepts and returns `Optional[float32]`.

```go
// Before (v2.26):
if odds := outcome.Odds(types.AmericanOddsDisplayType); odds != nil {
    fmt.Println(*odds)
}

// After (v2.27):
if v, ok := outcome.Odds(types.AmericanOddsDisplayType).Get(); ok {
    fmt.Println(v)
}
// Or default-zero:
fmt.Println(outcome.Odds(types.AmericanOddsDisplayType).ValueOr(0))
```

### Migration guide

Read sites — pre/post:

```go
// Before (v2.26):
if outcome.DecimalOdds != nil {
    fmt.Println(*outcome.DecimalOdds)
}
if mwo.IsFavourite != nil && *mwo.IsFavourite {
    fmt.Println("favourite")
}
if cancel.VoidReasonID != nil {
    fmt.Println(*cancel.VoidReasonID)
}

// After (v2.27):
if v, ok := outcome.DecimalOdds.Get(); ok {
    fmt.Println(v)
}
if mwo.IsFavourite.ValueOr(false) {
    fmt.Println("favourite")
}
if v, ok := cancel.VoidReasonID.Get(); ok {
    fmt.Println(v)
}
```

Write sites — `Some(v)`, `None[T]()`, or `FromPtr(p)`:

```go
o := types.OutcomeOdds{
    DecimalOdds: types.Some[float32](2.5),
    Probability: types.None[float32](),
}
// Or from XML decode:
o := types.OutcomeOdds{
    DecimalOdds: types.FromPtr(decoded.Odds),
    Probability: types.FromPtr(decoded.Probabilities),
}
```

### Remaining future migrations after v2.27

(See v2.28 §37 below for the next batch.)

## 37. v2.28 — `Optional[T]` migration: string-pointer cluster

Continues v2.26/v2.27 by migrating the remaining `*string` /
identifier-handle optionality fields on the public type surface.

### Fields migrated

| Type | Field | Before | After |
| --- | --- | --- | --- |
| `MarketDescription` | `Variant` | `*string` | `Optional[string]` |
| `MarketDescription` | `IncludesOutcomesOfType` | `*string` | `Optional[string]` |
| `MarketDescription` | `OutcomeType` | `*string` | `Optional[string]` |
| `MarketVoidReason` | `Description` | `*string` | `Optional[string]` |
| `MarketVoidReason` | `Template` | `*string` | `Optional[string]` |
| `Competitor` | `IconPath` | `*string` | `Optional[string]` |
| `TeamCompetitor` | `Qualifier` | `*string` | `Optional[string]` |
| `Tournament` | `IconPath` | `*string` | `Optional[string]` |
| `SportSummary` | `IconPath` | `*string` | `Optional[string]` |

The cache's `Snapshot` builders that previously wrapped `clonePtr(...)`
on these fields (added in v2.25 to defend against caller-mutation-
into-cache aliasing) now use `types.FromPtr(...)`. Value semantics
make the v2.25 clonePtr workaround obsolete for these fields.

### Migration guide

Read sites:

```go
// Before (v2.27):
if md.OutcomeType != nil {
    fmt.Println(*md.OutcomeType)
}

// After (v2.28):
if v, ok := md.OutcomeType.Get(); ok {
    fmt.Println(v)
}
// Or default-empty:
fmt.Println(md.OutcomeType.ValueOr(""))
```

Write sites:

```go
md := types.MarketDescription{
    Variant: types.Some("default"),
}
// Or from XML decode:
md := types.MarketDescription{
    OutcomeType: types.FromPtr(decoded.OutcomeType),
}
```

### What's NOT migrated (parameter-typed `*string`)

The `MarketDescriptionManager` interface methods that take a `variant
*string` *parameter* are unchanged. Function-argument `*string` is
conventional Go for "may be nil" arguments and the migration would
add no aliasing safety:

- `MarketDescriptionByIDAndVariant(ctx, marketID uint, variant *string)`
- `LocalizedMarketDescriptionByIDAndVariant(ctx, marketID uint, variant *string, locales ...Locale)`
- `ClearMarketDescription(marketID uint, variant *string)`

Same for the internal `cache.NewMarketVoidReason(description *string,
template *string, ...)` constructor — its callers already pass `*string`
from XML decode; conversion to `Optional[string]` happens inside.

## 38. v2.29 — `Optional[T]` migration: replay + static-data + recovery

Continues the `Optional[T]` migration into the remaining catalog and
config types. Same value-semantics rationale as v2.26-v2.28.

### Fields migrated

| Type | Field | Before | After |
| --- | --- | --- | --- |
| `ReplayPlayParams` | `Speed` | `*int` | `Optional[int]` |
| `ReplayPlayParams` | `MaxDelayInMs` | `*int` | `Optional[int]` |
| `ReplayPlayParams` | `RunParallel` | `*bool` | `Optional[bool]` |
| `ReplayPlayParams` | `RewriteTimestamps` | `*bool` | `Optional[bool]` |
| `ReplayPlayParams` | `Producer` | `*string` | `Optional[string]` |
| `StaticData` | `Description` | `*string` | `Optional[string]` |
| `LocalizedStaticData` | `Description` | `*string` | `Optional[string]` |
| `RecoveryInfo` | `NodeID()` | `*int` | `Optional[int]` |

### API changes

`StaticData.GetDescription()` and `LocalizedStaticData.GetDescription()`
now return `Optional[string]` instead of `*string`. The same change
applies to `LocalizedStaticData.LocalizedDescription(locale)`.

`RecoveryInfo` is an interface — its `NodeID()` signature changed
from `*int` to `Optional[int]`. The single internal implementer
(`recoveryInfoImpl`) was updated; the constructor still accepts a
`*int` argument (its callers pass `cfg.SdkNodeID()` which returns
`*int`) and converts via `FromPtr` at the boundary.

### Migration guide

ReplayPlayParams — direct construction:

```go
// Before (v2.28):
speed := 10
params := types.ReplayPlayParams{Speed: &speed}

// After (v2.29):
params := types.ReplayPlayParams{Speed: types.Some(10)}
```

The `gosdk.WithReplay*` option builders are unchanged at the call
site — only their internals migrated:

```go
// Unchanged option builders; the public entry point is Replay().Start:
client.Replay().Start(ctx, gosdk.WithReplaySpeed(10), gosdk.WithReplayRunParallel(true))
```

`internal/replay/manager.go.Play` bridges from `Optional[T]` back to
the `*T` arguments that `apiClient.PostReplayStart` takes (HTTP
query-string assembly) via `Optional.Ptr()` — `None` → nil (omits
the query-string entry), `Some(v)` → `&v`.

StaticData read sites:

```go
// Before:
if d := sd.GetDescription(); d != nil {
    fmt.Println(*d)
}

// After:
if v, ok := sd.GetDescription().Get(); ok {
    fmt.Println(v)
}
```

RecoveryInfo.NodeID:

```go
// Before:
if id := info.NodeID(); id != nil {
    fmt.Println(*id)
}

// After:
if v, ok := info.NodeID().Get(); ok {
    fmt.Println(v)
}
```

## 39. v2.30 — `Optional[T]` migration: `RequestMessage.RequestID()`

The largest-blast-radius migration in the `Optional[T]` series:
the `RequestMessage` interface's `RequestID()` method, embedded into
**every public message type** (`OddsChange`, `BetStop`,
`BetSettlement`, `BetCancel`, `FixtureChangeMessage`,
`RollbackBetSettlement`, `RollbackBetCancel`).

### Interface change

```go
// Before (v2.29):
type RequestMessage interface {
    Message
    RequestID() *uint
    RawMessage() []byte
}

// After (v2.30):
type RequestMessage interface {
    Message
    RequestID() Optional[uint]
    RawMessage() []byte
}
```

`None` indicates the upstream feed did not include a request id —
typical for non-recovery-correlated messages. `Some(id)` indicates
this message corresponds to a specific recovery request.

### Migration guide

Read sites — pre/post:

```go
// Before (v2.29):
if reqID := msg.RequestID(); reqID != nil {
    log.Printf("recovery-correlated message, request_id=%d", *reqID)
}

// After (v2.30):
if v, ok := msg.RequestID().Get(); ok {
    log.Printf("recovery-correlated message, request_id=%d", v)
}
// Or default-zero / log-only:
log.Printf("request_id=%d", msg.RequestID().ValueOr(0))
```

If you previously type-switched on the message and accessed
`m.RequestID()`, the migration is purely the call-site read pattern.
The dispatch is unchanged.

### Internal impact

Seven factory implementations updated (one per message type) — each
returns `types.FromPtr(m.message.RequestID)` at the XML→types
boundary. `betStopImpl`'s stored `requestID *uint` field changed
to `types.Optional[uint]` (the only impl that stored the request id
separately rather than delegating to the embedded XML message).

### Remaining `*T` optionality on the public type surface

After v2.30, the migration is complete for "pure optionality"
fields on public types. The following `*T` fields **stay as-is by
design**:

- **`*time.Time`** — `Match.ScheduledTime`, `Match.ScheduledEndTime`,
  `Fixture.StartTime`, `Tournament.StartDate`/`EndDate`/
  `ScheduledTime`/`ScheduledEndTime`. Conventional Go optionality
  for time values; `time.Time` already has value semantics, so the
  aliasing concern doesn't apply.
- **`*URN`** — `MatchStatus.WinnerID`, `RoutingKeyInfo.SportID`,
  `RoutingKeyInfo.EventID`. Structural identifier handle; URN itself
  is a small value type and the pointer is the natural shape.
- **Wholesale-optional struct pointers** — `MatchStatus.Scoreboard`,
  `MatchStatus.Statistics`, `MatchStatus.StatusDescription`,
  `Tournament.Category`. Outer pointer is the conventional Go shape
  for "may be nil"; interior fields are already value-style
  (`Optional[T]`) after v2.26-v2.28.
- **Function-argument `*string`** —
  `MarketDescriptionByIDAndVariant(variant *string)`,
  `ClearMarketDescription(variant *string)`. Conventional Go for
  optional arguments; aliasing safety doesn't apply.

The `Optional[T]` series ends with v2.30. The bug class flagged in
the v2.25 review (cache-aliased inner pointer fields on Scoreboard /
Statistics / PeriodScore) is fully closed; the broader migration
brought every "pure optionality" struct field on the public type
surface into value semantics.

## 40. v2.x — Critical correctness fixes

Production-correctness pass before merge. No public-API surface
shape changes for consumer code beyond what's noted below.

### 40.1 `types.BetStop` dispatch — fixed (composition-via-marker)

**Pre-fix bug**: `types.BetStop` was sealed via an unexported
`isBetStop()` method, but the concrete `betStopImpl` in
`internal/factory` did not (and structurally could not) implement
it. As a result, every real `bet_stop` feed message failed the
`case types.BetStop:` arm in the session loop's type-switch and
fell through to the default → unparsable path. Consumers saw
`UnparsableMessage` instead of `BetStop`, with no diagnostic.

**Fix**: introduced `types.BetStopMarker` — a public empty struct
in the `types` package whose unexported `isBetStop()` method
implements the seal. `betStopImpl` embeds the marker via composition,
which propagates the method. The `BetStop` interface stays sealed
(only `BetStopMarker` can satisfy `isBetStop()`), so unrelated
`RequestMessage+EventMessage` shapes (BetCancel, FixtureChange,
Rollback*) still don't accidentally match the type-switch arm.

Compile-time guards added in `internal/factory`
(`var _ types.BetStop = betStopImpl{}` and similar for every
concrete impl) so any future drift fails the build, not the
runtime.

**Impact for consumers**: none for production code — `BetStop` looks
identical from the outside. **For consumer test mocks** that satisfy
`types.BetStop`, embed `types.BetStopMarker` (a public empty struct):

```go
// Pre-fix consumer mock:
type myBetStopMock struct{ /* methods */ }

// Post-fix:
type myBetStopMock struct {
    types.BetStopMarker  // one-line add; satisfies the sealed isBetStop()
    /* methods */
}
```

`BetStopMarker` is the only documented exception to NEXT.md §2's
"do not seal public interfaces" rule. Rationale: `BetStop` is the
only `Request*Message` interface that has no naturally-distinguishing
exported method (no `Markets()`, no `StartTime()`, no `ChangeType()`),
so a structural marker is the minimum mechanism to keep type-switch
dispatch correct. See NEXT.md §2 for the full rationale; do NOT
extend the pattern to other interfaces.

The bug was 100% production-side; existing consumer code
written against `types.BetStop` Just Works after upgrade.

### 40.2 EventCache mixed-locale singleflight — fixed (recurse on uncovered)

**Pre-fix bug**: `EventCache.Get` keyed its singleflight on the
entity URN alone. Two callers asking for the same entity in
different locales (caller A: `[en]`, caller B: `[ru]`) shared one
in-flight load; the loader used A's locales (closure capture). B
received A's result — a snapshot covering only `[en]` — and
`Get` returned `(entry, true, nil)` without re-checking that B's
requested locales were actually populated.

**Fix**: after receiving the singleflight result, `Get` re-checks
`coversAll(entry.Locales(), locales)` for the *current caller's*
locales. If incomplete, it recurses — the next call sees the
partially-populated cache entry and singleflights only the gap.
Bounded to one extra round-trip per caller in the worst case.

**Impact for consumers**: improved correctness only. No API change.

### 40.3 `FixtureChangeType` value 4 (FORMAT) — added

**Pre-fix gap**: javasdk and netcoresdk both decode wire
`change_type=4` as FORMAT (mapped to `OTHER_CHANGE` in Java's
public enum). The Go xml package skipped 4 entirely, so a real
FORMAT change arrived as `UnknownFixtureChangeType` — silent
feature gap vs the reference SDKs.

**Fix**: added `feedXML.FixtureChangeTypeFormat = 4` and the
`fixtureChangeImpl.ChangeType()` mapper case
`FixtureChangeTypeFormat → types.OtherChangeFixtureChangeType`.

### 40.4 Loaded-but-empty market/outcome name preserved

**Pre-fix subtlety**: with the new `Optional[string]` accessors on
`Market.Name(loc)` / `Outcome.Name(loc)`, "loaded but empty" should
be `Some("")` and "not loaded" should be `None`. The factory's
internal name resolver was dropping empty names from the per-locale
map, collapsing both cases to `None`.

**Fix**: `resolveMarketName` / `resolveOutcomeName` now return
`(string, bool)` — `(zero, false)` only on lookup failure;
loaded-but-empty returns `("", true)` and the per-locale map gets
the empty entry. Preserves the Optional contract.

### 40.5 `go` directive reverted to 1.24.0

`go.mod` had drifted to `go 1.25.9` (CVE-driven bump). NEXT.md §1's
consumer-floor target is `go 1.24` — kollector-esport is still
`go 1.24.2`. Reverted the language minimum to `go 1.24.0`; added
`toolchain go1.25.9` for stdlib CVE patches at build/test time
without forcing consumer Go bumps. `golang.org/x/sync` downgraded
v0.20.0 → v0.18.0 (the latter still requires only Go 1.24);
`golang.org/x/sys` downgraded v0.43.0 → v0.38.0 for the same
reason. Functional behavior unchanged.

## 41. v2.x — Parity + architecture pass

### 41.1 `MarketWithSettlement.VoidReasonID` / `VoidReasonParams` — added

**Pre-fix gap**: bet_settlement markets carry `void_reason_id` and
`void_reason_params` on the wire (per netcoresdk's
`betSettlementMarket` schema), but Go's `MarketWithOutcome` XML
struct didn't decode them. `types.MarketWithSettlement` therefore
had no void-metadata surface — a parity gap vs .NET's
`IMarketWithSettlement` (which inherits both fields from
`IMarketCancel`).

**Fix**: XML struct now decodes the modern fields; public type
exposes them as `Optional[uint]` / `Optional[string]`. The deprecated
single-int `void_reason` attribute is intentionally NOT decoded —
javasdk marks it `@Deprecated` and `MarketWithSettlementImpl` returns
null for the corresponding accessor; origin/main Go never exposed it
either. Only the modern ID + Params fields are surfaced.

```go
// New surface on types.MarketWithSettlement:
VoidReasonID     Optional[uint]
VoidReasonParams Optional[string]
```

Consumer migration: read with `.Get()` / `.ValueOr(...)` like every
other `Optional[T]` field on the public types.

### 41.2 `SportCache` empty-tournaments cached after first fetch

**Pre-fix bug**: `SportCache` had `tournamentIDs map[URN]struct{}` but
no "loaded" flag. `SportTournaments` and `BuildSport` keyed off
`len(tournamentIDs) > 0`, so a sport with a genuinely empty
tournament list re-fetched on every call.

**Fix**: added `tournamentsLoaded bool` to `LocalizedSport`. Set
after a successful `FetchTournaments`, even when the result was
empty. Mirrors the existing `LocalizedTournament.competitorsLoaded`
pattern. New `ensureSportEntry` helper guarantees the marker
attaches even on first access for a sport never previously loaded
via `Sports()`.

### 41.3 `TournamentCompetitors` empty-list early return

**Pre-fix smell**: `TournamentCompetitors` returned cached competitor
URNs only when `len(urns) > 0`; an empty list forced re-fetch on
every direct internal call. `competitorsLoaded` already existed on
`LocalizedTournament` but wasn't checked here.

**Fix**: gate the early return on `competitorsAreLoaded()` instead
of `len(urns) > 0`. `BuildTournament` was already correct (it forces
a reload only when competitors-not-loaded); this fix aligns the
direct internal API with the same semantics.

### 41.4 Snapshot recovery — PostRecovery API detached from actor goroutine

**Pre-fix architectural smell**: event recovery was correctly
restructured in v2.24 (HTTP off the actor goroutine, result fed back
as `evRecoverEventCompleted`). Snapshot recovery — `makeSnapshotRecovery`
— still ran `PostRecovery` inline on the actor goroutine. A slow
recovery initiate (~30s with retries) blocked every alive / tick /
snapshot-complete / recover-event for that producer; the 256-slot
inbox dropped alives under load and triggered false producer-down.

**Fix**: same detach pattern as event recovery:
- State mutation (`currentRecovery`, `recoveryState=Started`) and
  `requestID` allocation stay inline on the actor goroutine — fast,
  single-threaded.
- The `PostRecovery` HTTP call runs in a detached goroutine bounded
  by `a.ctx` (actor lifetime ctx), so a manager Close still cancels
  in-flight recoveries.
- The result feeds back as `evSnapshotRecoveryAPICompleted`;
  `SetProducerRecoveryInfo` runs single-threaded on the actor.

**Test**: `TestActor_MakeSnapshotRecovery_DetachedAPI_DoesNotBlockActor`
verifies that with a hung recovery handler, `makeSnapshotRecovery`
returns near-instantly, state transitions to Started before the API
is even attempted, and a follow-up handler (`onMessageProcessingStarted`)
runs synchronously without waiting.

**Impact for consumers**: improved liveness only. No API change.

## 42. v2.x — Recovery state-machine correctness pass

Two bugs in the recovery actor's snapshot-recovery state machine
discovered in the final review pass. Both pre-existed the v2.x rewrite
and would have caused recovery hangs in production under specific
race conditions.

### 42.1 Interrupted re-issue overwrote re-issued recovery's state

**Pre-fix bug**: `snapshotRecoveryFinished` had this shape:

```go
if a.recoveryState == InterruptedRecoveryState {
    a.makeSnapshotRecovery(...)  // sets currentRecovery=NEW, state=Started
}
// ↓ unconditional fall-through:
a.currentRecovery = newRecoveryData(requestID, started)  // overwrites NEW with OLD
a.recoveryState = CompletedRecoveryState                 // overwrites Started with Completed
return a.producerUp(reason)                              // emits "producer up" while still recovering
```

When an alive with `subscribed=false` arrived during a snapshot
recovery (the H1 fix path), the actor transitioned to `Interrupted`.
When the original snapshot then completed, `snapshotRecoveryFinished`
was supposed to re-issue a fresh snapshot recovery. It did call
`makeSnapshotRecovery`, but then **continued past the if-block and
clobbered the re-issued recovery's state**. Consequence: the new
recovery's eventual `snapshot_complete` arrived with a `requestID`
that no longer matched `currentRecovery.recoveryID`, was treated as
"stale/unknown", and silently ignored. The new recovery hung
forever; the producer was incorrectly emitted as "up" before
recovery actually finished.

**Fix**: return early after the re-issue so the completion path
doesn't run. Producer stays "down" until the new snapshot's
`snapshot_complete` arrives and drives the next transition.

**Test**: `TestActor_SnapshotRecoveryFinished_InterruptedReissuesAndPreservesNewState`.

### 42.2 PostRecovery error left actor stuck in Started forever

**Pre-fix bug**: after the F4 detach (snapshot recovery API moved off
the actor goroutine), `onSnapshotRecoveryAPICompleted` on API error
just logged and returned — without rolling back state:

```go
if e.err != nil {
    a.logger.WithError(e.err)...Error("PostRecovery API failed")
    return  // currentRecovery + recoveryState=Started UNCHANGED
}
```

The recovery never started on the upstream side (the API call
failed), so no `snapshot_complete` would ever arrive. But the actor
remained in `StartedRecoveryState` indefinitely; `isPerformingRecovery()`
returned true; subsequent alives took the "in recovery" branch and
couldn't re-issue. Every producer with a transient PostRecovery
failure stayed silently broken.

**Fix**: on API error, transition `recoveryState` to
`ErrorRecoveryState` and clear `currentRecovery`. The next tick or
alive then re-evaluates and can re-issue. Plus: stale-event guard —
if `currentRecovery` rotated between the send and arrival of the
completion event (e.g., the Interrupted re-issue fix above triggers
exactly this), the stale completion is dropped without clobbering
the newer recovery.

**Test**: `TestActor_OnSnapshotRecoveryAPICompleted_ErrorTransitionsToErrorState`
+ `TestActor_OnSnapshotRecoveryAPICompleted_StaleEventIgnored`.

### 42.3 Shutdown-race log on dropped snapshot completion (Low)

**Pre-fix observability gap**: when `manager.Close` cancelled `a.ctx`
while a detached `PostRecovery` goroutine was in flight, the
goroutine's `sendCtx` returned `ErrManagerClosed` and the result
was silently discarded. An operator correlating a hung handle to a
shutdown race had no breadcrumb.

**Fix**: log the drop at WARN level with `producer_id` + `request_id`
context. Behavior unchanged (the actor is gone — there's nothing
useful to do with the result), but the diagnostic trail exists.

## 43. v2.24 — Recovery / cache / parser correctness pass (4 review findings)

### 43.1 F1 (LOW) — Unpaired Started / Ended on failed message paths

`session.processFeedMessage` called
`recoveryMessageProcessor.OnMessageProcessingStarted` immediately,
but the BuildMessage-failure path and the unrecognized-built-type
default branch returned WITHOUT calling `OnMessageProcessingEnded`.
The recovery manager tracks active processing in a per-session map
that's cleared by `OnMessageProcessingEnded` (see
`recovery.Manager.OnMessageProcessingEnded`). Each failed message
leaked one stale "in-flight" entry per session; over time this
accumulated into bogus processing-queue-delay metrics and could
keep producers flagged as "still processing".

Fix: every Started call is now paired with exactly one Ended call.
A `processingEnded` bool + idempotent `endProcessing(ts)` closure
gates the call. Every success branch invokes `endProcessing` with
the real message-gen timestamp; a deferred fallback fires only
when none of the success branches did, using `time.Time{}` (no
cursor advance — same convention the SnapshotComplete branch uses).

Tests:
- `TestSession_ProcessFeedMessage_BuildErrorEndsProcessing` — drives
  the BuildMessage-failure path with a counting recovery processor;
  asserts Started == 1, Ended == 1.
- `TestSession_ProcessFeedMessage_UnrecognizedTypeEndsProcessing` —
  drives the default branch with a builder that returns a type the
  switch doesn't recognise.

### 43.2 F2 (Code Smell) — Player cache global lock serialised unrelated misses

`PlayersCache.GetPlayers` took a global `loadMu` for every cache
miss. Concurrent calls for distinct (PlayerID, Locale) keys
serialised behind the same mutex, so a 4-locale preload of N
players ran 4N sequential HTTP calls instead of N parallel
batches.

Fix: replaced `loadMu` with `golang.org/x/sync/singleflight.Group`
keyed by `PlayerCacheKey.String()`. Distinct keys load in parallel;
duplicate-key concurrent calls share a single in-flight HTTP
request. Each singleflight closure re-checks the cache under
`mu.RLock` before issuing the API call (the leader of a duplicate
group may have stored before the follower entered the closure).

Tests:
- `TestPlayersCache_DistinctKeysLoadInParallel` — 4 distinct keys
  with a 120 ms server delay; asserts total time <300 ms (pre-fix
  ~480 ms with global-mutex serialisation).
- `TestPlayersCache_DuplicateKeyDeduplicated` — 8 concurrent
  callers for the SAME key; asserts the server saw exactly 1 hit.

### 43.3 F3 (Code Smell) — Specifier parser dropped values containing `=`

`MarketFactory.extractSpecifiers` used `strings.Split(part, "=")`
and rejected any specifier whose split produced != 2 parts.
Specifier values that legitimately contain `=` (opaque
base64-ish payloads from a future protocol revision) were dropped
entirely — including the key.

Fix: `strings.Cut(part, "=")` splits on the FIRST `=` only, so
values with embedded `=` are preserved verbatim. Key with empty
string is still rejected (key-without-value is meaningless); a
part with no `=` at all is still rejected.

Test cases added to the existing `TestMarketFactory_ExtractSpecifiers`:
- `value contains equals`: `opaque=a=b=c` → `{opaque: a=b=c}`.
- `leading equals rejected`: `=lonelyvalue` → `{}`.
- `no equals rejected`: `total` → `{}`.

### 43.4 F4 (Doc Nit) — Replay status doc overstated cross-SDK parity

`api.Client.FetchReplayStatus`'s comment said "Mirrors GET
/replay/status in the .NET / Java SDKs". Java's
SportsInfoManager doesn't currently expose replay status
publicly; only .NET does (via `IReplayManager`). Updated the
comment to call this out as ".NET SDK / API-endpoint parity"
rather than full cross-SDK parity.

## 44. v2.25 — API ergonomics: SessionMessage union, manager relocation, alias cleanup

Three breaking-but-mechanical reshapes from the v2.24 review. None
fix bugs; all improve consumer ergonomics and package layering.

### 44.1 #1 — `SessionMessageDelivery` type alias deleted

`types.SessionMessageDelivery` was a type alias for `<-chan
SessionMessage`. The alias added a name without adding meaning;
consumers had to look up what it expanded to. Deleted; the channel
type appears verbatim where it's used (one method on
`oddsFeedSessionImpl` and the comment in NEXT.md §1's API sketch).

### 44.2 #2 — Manager interfaces relocated from `types/` to `gosdk/`

Manager interfaces describe what the SDK *does*; types/ should be
data shapes the SDK *returns*. The four interfaces below moved out
of `types/` into a new `gosdk/managers.go`:

- `gosdk.WhoAmIManager` (was `types.WhoAmIManager`)
- `gosdk.ReplayManager` (was `types.ReplayManager`)
- `gosdk.SportsInfoManager` (was `types.SportsInfoManager`)
- `gosdk.MarketDescriptionManager` (was `types.MarketDescriptionManager`)

> **v1.0.0 update:** all four were subsequently **unexported**
> (`whoAmIManager`, `replayManager`, `sportsInfoManager`,
> `marketDescriptionManager`). They are wiring seams between `Client`
> and the internal manager implementations — no public method takes
> or returns them — so exporting them only leaked names into the
> consumer namespace against the "flatten to a single Client" goal.
> The public surface is `Client` alone.

Internal implementations (`internal/whoami.Manager`,
`internal/replay.Manager`, `internal/sport.Manager`,
`internal/market.Manager`) now return their concrete `*Manager`
type. They satisfy the gosdk-side interfaces structurally — Go's
"accept interfaces, return concrete types" idiom — and don't import
the gosdk root package (which would be a cycle).

`internal/feed` and `internal/replay` consume narrow local
interfaces (`feed.WhoAmIProbe`, `replay.SportsInfoLookup`) defined
where they're used. Same structural-typing rationale.

Data shapes the manager interfaces return — `BookmakerDetail`,
`ReplayPlayParams`, `Match`, `Tournament`, etc. — stay in `types/`
where they belong.

### 44.3 #3 — `SessionMessage` is now a tagged-union envelope

The pre-v2.25 envelope:

```go
type SessionMessage struct {
    RawFeedMessage    *RawFeedMessage
    Message           interface{}      // type-switch every consumer
    UnparsableMessage UnparsableMessage
}
```

Required every consumer to write
`switch m := env.Message.(type) { case types.OddsChange: … }`. The
`interface{}` form was IDE-opaque (no autocomplete on what
variants exist), risked silent fall-through to the default branch
on unexpected concrete types, and forced two-level retyping for
the per-message `Event() interface{}` accessor.

The post-v2.25 shape replaces `Message interface{}` with an
embedded `EventMessage` struct that exposes one nilable field per
message variant:

```go
type EventMessage struct {
    OddsChange            OddsChange
    BetStop               BetStop
    BetSettlement         BetSettlement
    BetCancel             BetCancel
    FixtureChange         FixtureChangeMessage
    RollbackBetSettlement RollbackBetSettlement
    RollbackBetCancel     RollbackBetCancel
}

type SessionMessage struct {
    RawFeedMessage *RawFeedMessage
    EventMessage    // embedded; field promotion gives env.OddsChange etc.
    UnparsableMessage UnparsableMessage
}
```

Consumer surface becomes:

```go
for env := range sub.Messages() {
    switch {
    case env.OddsChange != nil:    /* … */
    case env.BetSettlement != nil: /* … */
    case env.UnparsableMessage != nil: /* … */
    }
}
```

IDE-discoverable, panic-free, no type-switch-on-`interface{}`.

The pre-v2.25 `EventMessage interface { Event() interface{} }`
(per-message accessor for the associated event entity) was renamed
to `WithEvent` — same shape, same return type, just freeing the
`EventMessage` name for the new union. Every concrete message
interface (OddsChange, BetStop, BetSettlement, BetCancel,
FixtureChangeMessage, RollbackBetSettlement, RollbackBetCancel,
UnparsableMessage) now embeds `WithEvent` instead of
`EventMessage`.

Producer side (session.go's type-switch on `BuildMessage`'s output)
populates the matching field per built message type:

```go
case types.OddsChange:
    o.send(ctx, types.SessionMessage{
        EventMessage: types.EventMessage{OddsChange: msg},
    })
```

Examples in `examples/basic`, `examples/replay`, `examples/graceful`
updated to the new consumer shape; NEXT.md §3 sample updated.
`Event() interface{}` on the per-message `WithEvent` interface is
unchanged — tightening it to a typed union is a separate reshape
deferred past v2.25.

### 44.4 Migration steps for existing consumers

1. `s/types.SessionMessageDelivery/<-chan types.SessionMessage/`
   if you embedded that alias anywhere.
2. Delete any direct reference to `types.WhoAmIManager` (and the
   three sibling manager interfaces) — as of v1.0.0 the interfaces
   are unexported in the gosdk root package and cannot be named by
   consumers. Most consumers never did — they use
   `client.Match(ctx, …)` etc. and never name the manager type. If
   you need a seam for mocking, define your own narrow local
   interface over the `Client` methods you call (Go's structural
   typing makes `*gosdk.Client` satisfy it automatically).
3. Replace
   ```go
   switch m := env.Message.(type) { case types.OddsChange: … }
   ```
   with
   ```go
   switch {
   case env.OddsChange != nil: …
   case env.UnparsableMessage != nil: …
   }
   ```
   The factory side (consumer never touches it) was already
   updated; this is purely a consumer-side rewrite.

## 45. v2.26 — Idiomatic int for IDs and counts (uint/uint32 → int)

### 45.1 What changed

Every public-API ID, request ID, producer ID, void-reason ID, market
ID, bookmaker ID, and Scoreboard / PeriodScore / Statistics counter
field switched from `uint` (or `uint32`) to `int`. Matches the
Go-community convention: `int` for ordinary numbers; `uint` reserved
for bitmasks and sizes.

Concrete affected surfaces:

- `types.URN.ID`: `uint → int`. `ParseURN` now uses `strconv.Atoi`
  and explicitly rejects negative IDs (the protocol doesn't use
  them; `strconv.Atoi("-1")` would otherwise parse silently).
- `types.Producer.ID()`, `BookmakerID()`, `RequestID()`,
  `Product()` → `int`.
- `types.ProducerManager.GetProducer(ctx, id int)` and siblings;
  `map[uint]Producer` → `map[int]Producer`.
- `types.Market.ID`, `MarketDescription.ID`, `MarketVoidReason.ID`,
  `Optional[uint]` (VoidReasonID) → `int` / `Optional[int]`.
- `types.PeriodScore.PeriodNumber`, `MatchStatusCode`,
  `MatchStatus.MatchStatusID`: `uint → int` /
  `Optional[uint] → Optional[int]`.
- `types.Scoreboard.*`, `PeriodScore.*`, `Statistics.*` counter
  fields: `Optional[uint32] → Optional[int]`.
- `types.RecoveryResult.RequestID`, `ProducerID`, and the
  session→recovery processing hooks (now unexported in the gosdk
  root package): `uint → int`.
- `gosdk.Client.RecoverEventOdds`, `RecoverEventStateful`,
  `SetProducerEnabled`, `Producer`,
  `SetProducerRecoveryFromTimestamp`, `EventRecoveryStatus`: all
  `uint → int`.

The wire-format XML structs in `internal/feed/xml` and
`internal/api/xml` were swept too — `Product() uint` accessors and
`ProductID uint` attribute fields now use `int`. Go's XML decoder
handles `int` and `uint` identically for non-negative decimal
numbers, so the wire format isn't affected.

### 45.2 Rationale

The reviewer's concrete critique: `int` is the idiomatic Go choice
for ordinary numbers. `uint` enforces "non-negative" at the type
level, but ID overflow at 2^31 (or 2^63) doesn't happen in practice;
the actual benefit is the contract clarity Go community style guides
spell out — and SDK consumers writing `int(x)` casts at every API
boundary is friction with no upside.

`Optional[uint32]` on Scoreboard/PeriodScore stat fields was the
wire-faithful narrow type, but consumers don't care about the
30-bit-vs-63-bit distinction for "goals scored"; `Optional[int]`
wins the readability + uniformity tradeoff.

### 45.3 Migration

Mostly mechanical for consumer code:

- Drop `uint(...)` / `int(...)` casts at SDK call sites — every
  `id`-shaped public method now takes / returns `int`.
- `Optional[uint]` literals in tests / wiring → `Optional[int]`.
- `map[uint]Producer` declarations →`map[int]Producer`.
- `Producer().ID()` returns `int` directly; remove any internal
  `int(p.ID())` conversions.

`URN.ID` going from `uint` to `int` is the only edge case worth
flagging: code that does arithmetic on `URN.ID` won't compile until
the `uint(...)` casts are removed, and code that constructed a URN
with a literal like `URN{ID: ^uint(0)}` (max-uint64 sentinel — rare)
now overflows. The `ParseURN("od:match:18446744073709551615")` test
case was replaced with the next-largest meaningful value
(`MaxInt64`).

Negative URN IDs are explicitly rejected by `ParseURN` (matches the
pre-v2.26 `ParseUint` ErrRange behaviour for `-1`).

## 46. v2.27 — Go 1.26 bump (toolchain + idioms)

Consumer repos (kollector-esport, ots-odds-bridge) have been
authorised to move to Go 1.26, so the SDK's language minimum bumps
in lockstep. NEXT.md §0.6 / §1 / §2 / §16 / §17 / §18 are updated
to match; the previous "stay on 1.24, treat 1.26 as risky" framing
is gone.

### 46.1 `go.mod`

```
go 1.26.0
toolchain go1.26.2
```

(Three-part form `1.26.0` is the canonical directive value `go mod
tidy` writes — disambiguates "compiler 1.26.x" from "language
version 1.26 with no patch". Functionally equivalent to `go 1.26`.)

Also bumped: `golang.org/x/sync` v0.18.0 → v0.20.0; tool deps
(`staticcheck`, `govulncheck`, `golang.org/x/tools` family)
refreshed to current. The two `-deprecated` `golang.org/x/tools`
sub-packages dropped out of the indirect set on `go mod tidy` —
they were only pulled in transitively by the older tool versions.

### 46.2 `sync.WaitGroup.Go` (Go 1.25)

Two production goroutine spawns migrated from the
`Add(1) + go func() { defer Done(); … }()` pattern to
`wg.Go(func() { … })`:

- `client.go`: alive-session drain pump and `pumpRecovery`
  (called from `ensureNormal` after the alive session opens).
- `internal/feed/channelConsumer.go`: the per-consumer
  `run` goroutine.

Spots that **still** use the explicit `wg.Add(1)` form are
*deliberate* — `client.admitSubscription` and
`internal/feed/Client.Open` both need `Add` to land inside a
critical section so it pairs atomically with a "Closed?"
re-check; `wg.Go` would force the goroutine spawn into the
locked region. The doc comments at those sites explain the
constraint.

### 46.3 `testing/synctest`

Stable as of Go 1.26. NEXT.md §17 / §18 risk table updated to
reflect that the previously-hedged "fall back to clockwork"
plan is moot — `testing/synctest` is the deterministic-time
substrate going forward. No `clockwork` dependency was ever
introduced.

### 46.4 README

`Requires Go 1.24+` → `Requires Go 1.26+`.

## 47. v2.28 — Internal config interface: `time.Duration` everywhere; relocated to `internal/config`

The internal config-shape contract was the last place where v2's
`time.Duration`-everywhere convention had not landed. The legacy
`types.OddsFeedConfiguration` interface still carried Java-flavoured
`MaxInactivitySeconds() int` and `MaxRecoveryExecutionMinutes() int`
methods, with `configAdapter` round-tripping through `int(d.Seconds())`
and `int(d.Minutes())`. Recovery actor call sites then re-converted
with `float64(...)` to compare against `time.Duration`. v2.28 removes
the round-trip and relocates the interface.

### 47.1 Interface relocation: `types.OddsFeedConfiguration` → `internal/config.Config`

The interface is purely internal-facing — only `internal/*` packages
consume it; consumers construct config via `gosdk.NewConfig(...)` +
functional options and never implement the interface. Per NEXT.md
§3.6 ("default to internal"), it now lives in a new neutral
`internal/config` package that both `gosdk` (for the adapter) and
`internal/*` (for the managers) can import without a cycle.

```diff
-import "github.com/oddin-gg/gosdk/types"
+import "github.com/oddin-gg/gosdk/internal/config"
 ...
-func NewManager(cfg types.OddsFeedConfiguration, ...) *Manager
+func NewManager(cfg config.Config, ...) *Manager
```

`types/odds_feed_config.go` was renamed to `types/environment.go`
(the file's remaining contents — `Environment`, `Region`, `Locale`,
`AllLocales` — are the public enums it always carried alongside the
relocated interface).

### 47.2 Method renames: `Seconds`/`Minutes` → `time.Duration`

```diff
 type Config interface {
-    MaxInactivitySeconds() int
-    MaxRecoveryExecutionMinutes() int
+    MaxInactivity() time.Duration
+    MaxRecoveryExecution() time.Duration
     ...
 }
```

`configAdapter` drops both conversion lines:

```diff
-func (a *configAdapter) MaxInactivitySeconds() int { return int(a.cfg.maxInactivity.Seconds()) }
-func (a *configAdapter) MaxRecoveryExecutionMinutes() int { return int(a.cfg.maxRecoveryExecution.Minutes()) }
+func (a *configAdapter) MaxInactivity() time.Duration        { return a.cfg.maxInactivity }
+func (a *configAdapter) MaxRecoveryExecution() time.Duration { return a.cfg.maxRecoveryExecution }
```

This eliminates a real bug class: `int(d.Seconds())` truncated
sub-second `Duration` values (e.g., `WithMaxInactivity(500*time.Millisecond)`
in tests) to `0`, silently disabling the inactivity check. End-to-end
`time.Duration` cannot lose precision.

### 47.3 Recovery-actor call sites: drop the `float64(...)` round-trip

```diff
 // internal/recovery/actor.go
-case aliveInterval.Seconds() > float64(a.cfg.MaxInactivitySeconds()):
+case aliveInterval > a.cfg.MaxInactivity():
 ...
-maxInactivity := float64(a.cfg.MaxInactivity())
-... math.Abs(messageProcessingDelay.Seconds()) < maxInactivity ...
+maxInactivity := a.cfg.MaxInactivity()
+... messageProcessingDelay.Abs() < maxInactivity ...
 ...
-maxAge := time.Duration(a.cfg.MaxRecoveryExecution()) * time.Minute
+maxAge := a.cfg.MaxRecoveryExecution()
 ...
-if recoveryTime.Minutes() > float64(a.cfg.MaxRecoveryExecution()) {
-    recoverFrom = now.Add(-time.Duration(a.cfg.MaxRecoveryExecution()) * time.Minute)
+maxRecovery := a.cfg.MaxRecoveryExecution()
+if now.Sub(recoverFrom) > maxRecovery {
+    recoverFrom = now.Add(-maxRecovery)
 }
```

Drops the `math` import from `actor.go`. Comparisons happen between
like-typed `Duration` values throughout — the kind of expression that
previously hid off-by-one unit bugs is gone.

### 47.4 No-op `Set*` methods removed from the interface

The legacy `OddsFeedConfiguration` interface required `SetRegion(...)`,
`SetExchangeName(...)`, `SetAPIURL(...)`, `SetMQURL(...)`,
`SetMessagingPort(...)`, `SetSportIDPrefix(...)` — all of which the v2
adapter implemented as no-ops returning the same adapter. **No internal
caller invoked any of them.** They're gone from `internal/config.Config`
and from the adapter; ~60 lines of dead code removed across the test
fakes that previously had to stub them.

### 47.5 Surface affected (rough numbers)

- `1` new file: `internal/config/config.go`
- `1` deleted file: `types/odds_feed_config.go` (renamed to `types/environment.go`, interface removed from contents)
- `28` files updated for type name + import + method-name renames
- `10` test-fake files updated for return-type changes (`int` → `time.Duration`) and `Set*` no-op cleanup
- `~30` fewer lines of code overall (no-op cleanup outweighs the new `internal/config/config.go`)

### 47.6 Consumer impact (`kollector-esport`, `ots-odds-bridge`)

Zero. Both consumers construct config via `gosdk.NewConfig(...)` +
functional options. Neither imports `types.OddsFeedConfiguration` nor
implements it. The rename is invisible to them.

### 47.7 NEXT.md §19 Q1 closed

The "verify with the recovery state-machine implementer" hedge resolves
with this change. Default `MaxInactivity = 20s` is unchanged (matches
Java/.NET); the rename + relocation is the actual closure work.

## 48. v2.29 — Public API holdout: `variant *string` → `types.Optional[string]`

The `Optional[T]` migration that swept the entity types in v2.26–v2.30
left one straggler on the public `Client` surface: the
`MarketDescription` / `ClearMarketDescription` pair still took
`variant *string`. Closes the consistency gap.

### 48.1 Surface change

```diff
-func (c *Client) MarketDescription(ctx context.Context, id int, variant *string, locales ...types.Locale) (*types.MarketDescription, error)
-func (c *Client) ClearMarketDescription(marketID int, variant *string)
+func (c *Client) MarketDescription(ctx context.Context, id int, variant types.Optional[string], locales ...types.Locale) (*types.MarketDescription, error)
+func (c *Client) ClearMarketDescription(marketID int, variant types.Optional[string])
```

Same change on the market-description manager interface in
[managers.go](managers.go) (unexported as of v1.0.0) and on the
internal cache / factory / manager methods that funnel through.

### 48.2 Caller migration

```diff
-c.MarketDescription(ctx, 1, nil)        // base description
+c.MarketDescription(ctx, 1, types.None[string]())

-c.MarketDescription(ctx, 1, &someStr)   // variant
+c.MarketDescription(ctx, 1, types.Some(someStr))

-c.ClearMarketDescription(42, nil)
+c.ClearMarketDescription(42, types.None[string]())
```

`types.None[string]()` is the explicit "no variant — base description"
selector; `types.Some(v)` carries a variant. Same semantics as the
prior `*string`; cleaner type with no aliasing concerns and consistent
with every other Optional[T]-shaped field on the public surface.

### 48.3 Internal stack updated end-to-end

- [internal/cache/market.go](internal/cache/market.go):
  `variantKey`, `MarketDescriptionByID`, `ClearCacheItem`, `loadOne`
  all take `types.Optional[string]`. The internal data-layer
  `description.Variant` (still `*string` from XML decode) is wrapped
  via `types.FromPtr(...)` at the boundary in `upsert`.
- [internal/factory/marketDescription.go](internal/factory/marketDescription.go):
  `MarketDescriptionByIDAndVariant` and the
  `MarketDescriptionByIDAndSpecifiers` helper that derives `variant`
  from the `specifiers["variant"]` map.
- [internal/market/manager.go](internal/market/manager.go):
  the public-mirror methods.

The single `variantKey()` function is the seam — every callsite that
needs a `CompositeKey` from a variant goes through it, so the
pointer-to-Optional swap was localized to that one helper plus the
public/manager/cache method signatures.

### 48.4 No behaviour change

All three test cases that previously passed `nil` (in `client_test.go`,
`internal/market/manager_test.go`, `internal/cache/clear_invalidation_test.go`)
mechanically rewritten to `types.None[string]()` and remain passing.
The `variantKey` map keying is bit-for-bit identical: `nil` was
`CompositeKey{MarketID: id}` (Variant nil), `types.None` produces the
same. `&"foo"` was `CompositeKey{MarketID: id, Variant: &"foo"}`,
`types.Some("foo")` produces the same.

### 48.5 NEXT.md §19 Q? closed (review-pass follow-up)

The v2-pass review flagged `variant *string` as the last public-API
holdout. This commit is the closure.

## 49. v2.30 — Review-driven correctness pass (4 findings)

Closes four review findings on the v2 PR. Each is small and
self-contained; bundled into one commit because they share the same
"defensive correctness on the public surface" theme.

### 49.1 Empty variant `Some("")` normalised to "no variant"

NEXT.md §0 has always said `Some("")` is invalid, but
`internal/cache/market.go` was treating it as a *distinct* cache
key from `None`. Concrete failure modes pre-fix:

- `client.MarketDescription(ctx, id, types.Some(""))` → URL becomes
  `/variants/` → API 404 → `ErrItemNotFoundInCache`. The error is
  indistinguishable from "this id genuinely doesn't exist".
- `client.ClearMarketDescription(id, types.Some(""))` evicts a
  never-populated entry *and* invalidates `loadedLocales`, forcing a
  full catalog refetch. Cache-busting via what should be a no-op.

Fix: `variantKey()` and `loadOne()`'s singleflight-key construction
both treat `Some("") == None`. Five lines of normalisation; the public
contract (NEXT.md §0) now matches the implementation.

### 49.2 `RawMessage()` defensively copies bytes

`oddsChangeImpl.RawMessage()`, `betStopImpl.RawMessage()`,
`betSettlementImpl.RawMessage()`, `betCancelImpl.RawMessage()`,
`fixtureChangeImpl.RawMessage()`, the rollback variants, and
`unparsableMessageImpl.RawMessage()` all returned the SDK's backing
buffer directly. A consumer doing `bytes.Replace(rawMsg, ...)` would
silently mutate the buffer for every other consumer that received the
same message envelope.

Fix: a small `cloneBytes()` helper at the top of `feedMessage.go`;
each `RawMessage()` body wraps its return through it. RawMessage is a
debug API (replay capture, log diagnostics) — the per-call allocation
is a correct trade-off for tamper-proof contract.

The exported maps on `types.Market`, `types.MarketDescription`, etc.
were *not* changed. Snapshot construction copies the inner data once,
so a downstream `m.Names["en"] = "evil"` mutates only that caller's own
snapshot copy — it does not corrupt the cache and does not race the
builder. The contract is "treat as read-only": mutation affects only
your copy and is a programmer error. Kept as documentation rather than
enforced by per-access defensive copies (the message hot path can't
afford them).

### 49.3 `WithAPIURL` / `WithMQURL` → `WithAPIHost` / `WithMQHost`

Pre-fix, the option name said "URL" but the docstring and accepted
input were a bare host. Passing `https://api.example.com` produced
URLs like `https://https://api.example.com/v1/...`. Footgun.

```diff
-gosdk.WithAPIURL("api.example.com")
+gosdk.WithAPIHost("api.example.com")

-gosdk.WithMQURL("mq.example.com")
+gosdk.WithMQHost("mq.example.com")
```

Both new options reject scheme-prefixed input — passing
`http://` / `https://` / `amqp://` / `amqps://` panics at config
construction with a clear message:

```
WithAPIHost: expected bare host, got URL with scheme https:// — pass
`host:port` only, the SDK builds the URL
```

Panic at construction is the right severity for what is unambiguously
a programmer error: the alternative is silently producing malformed
URLs at every API call later, which is strictly harder to diagnose.
Returning errors from Options would force every caller to check
Option errors — far more disruptive.

The internal field names `forcedAPIURL` / `forcedMQURL` renamed to
`forcedAPIHost` / `forcedMQHost` to match.

**Caller migration** — mechanical rename:

```sh
# In each consumer repo:
find . -name '*.go' -print0 | xargs -0 sed -i '' \
  -e 's/gosdk\.WithAPIURL/gosdk.WithAPIHost/g' \
  -e 's/gosdk\.WithMQURL/gosdk.WithMQHost/g'
```

If any callsite passes a scheme-prefixed value, the panic message
will guide the fix.

### 49.4 Detached cache loads now bounded

The singleflight loaders in `internal/cache/lru/{singleflight,event_cache}.go`
detach from the caller's context (`context.WithoutCancel`) so a
short-deadline first caller can't kill a load that later waiters
share. Pre-fix: the detached load had no timeout of its own, relying
solely on the underlying `*http.Client.Timeout` to bound work. With
`WithHTTPClient(custom)` + a custom client whose `Timeout == 0`, a
stuck TCP connect could pin the singleflight slot indefinitely,
jamming every other caller of the same key.

Fix — defense in depth, two layers:

1. **`WithHTTPClient` panics** when the supplied client has
   `Timeout <= 0`. Same severity as `WithAPIHost` scheme rejection —
   surface the misconfiguration at config-construction time.
2. **`LoadCoalesced` and `EventCache.Get` wrap their detached
   `loadCtx` in `context.WithTimeout(60*time.Second)`** by default.
   Configurable via the package-level `lru.LoadTimeout` for tests
   that need a different bound. 60s is generous for typical SDK
   loads (catalog / per-event fetches finish in milliseconds on the
   happy path) and only fires on genuine network hangs.

Together, these guarantee no single goroutine pins a cache slot for
more than 60s even if every other defense fails.

### 49.5 Toolchain bump: go1.26.2 → go1.26.3 (CVE-driven)

`go tool govulncheck ./...` on `go1.26.2` reports two stdlib CVEs
that **are reachable** from SDK code paths (CI's vulndb has the
call-graph traces — local scans on stale databases miss them):

- **`GO-2026-4971`** — `net.Dial` panic on Windows NUL byte. Reachable
  via `internal/feed/client.go:dial` → `amqp091.DialConfig` →
  `net.DialTimeout`, and `internal/feed/client.go:newCtxBoundDialerWithCapture`
  → `net.Dialer.DialContext`.
- **`GO-2026-4918`** — Infinite loop in `net/http` HTTP/2 transport
  on bad `SETTINGS_MAX_FRAME_SIZE`. Reachable via
  `internal/api/client.go:do` → `http.Client.Do`.

Plus a handful of additional non-reachable stdlib CVEs (Escaper
bypass in `html/template`, quadratic string concat in `net/mail`,
`net/http/httputil` ReverseProxy query forwarding) — moot for the
SDK but cleared by the same bump.

Fix: `toolchain go1.26.2` → `toolchain go1.26.3` in [go.mod](go.mod).
Consumer impact: zero mechanically — a dependency's `toolchain`
directive governs the build of *this* module only and does NOT select
the consumer's toolchain. **Consumers are NOT protected by this bump:**
applications compile the SDK with their OWN Go toolchain (their
go.mod / CI), so to get these stdlib fixes a consumer must move its
own module/CI to a patched Go release (≥ go1.26.3, or the latest
patch of its chosen minor).

## 50. v2.31 — Drop dead interfaces; relocate `EventRecoveryMessage`; delete `types/subscribe.go`

A `types/` package audit (full plan in the v2 PR review thread)
flagged several types that were declared but had zero callsites in
production or tests — leftovers from the manager-of-managers era
that the v2 rewrite should have purged but didn't.

### 50.1 Dropped types

| Type | Was at |
|---|---|
| `types.RawAPIData` interface | `types/subscribe.go` |
| `types.ConnectionDownMessage` interface | `types/subscribe.go` |
| `types.ProducerStatusChangeMessage` interface | `types/subscribe.go` |
| `types.ProducerManager` interface | `types/producer.go` |
| `types.RecoveryManager` interface | `types/recovery.go` |

`grep -r` confirmed zero references in production code, tests, or
examples for any of these. The `internal/recovery.Manager` methods
`InitiateEventOddsMessagesRecovery` / `InitiateEventStatefulMessagesRecovery`
that were "kept for `types.RecoveryManager` interface compatibility"
were also unreferenced — dropped.

### 50.2 `EventRecoveryMessage` relocated

`types/subscribe.go` was the wrong home for `EventRecoveryMessage` —
the file name suggested Subscribe-flow concepts, but the only
remaining live interface in it was a *recovery* payload. Moved to
`types/recovery.go` where it sits next to `RecoveryMessage` (which
contains it as a field).

### 50.3 `types/subscribe.go` deleted

After 40.1 and 40.2 the file was empty. Deleted.

### 50.4 Consumer impact

Zero. None of the dropped interfaces were ever satisfied or consumed
by any production callsite — they only existed as type declarations
that no code pointed at.

## 51. v2.32 — Move internal-only contracts out of `types/`

Four exports in `types/` that the audit identified as internal-only
contracts (consumed by `internal/*` packages, never crossing the
public surface) were relocated.

### 51.1 `types.EntityFactory` interface → consumer-side declaration in `internal/cache`

`types.EntityFactory` was a type-erasure boundary that let
`internal/cache` accept the factory without importing
`internal/factory` (which imports `internal/cache` — would be a
cycle). With the public-package home gone, the interface moves to a
new `internal/cache/factory.go` — a Go-idiomatic consumer-side
declaration. `*internal/factory.EntityFactory` satisfies the
contract via structural typing; no caller-side change needed.

The cache-side interface is also pruned to only the methods cache
actually calls (7 of the original 12 — `BuildSports`, `BuildMatch`,
`BuildMatches`, `BuildCompetitors`, `BuildTournaments` were never
invoked from cache).

### 51.2 `types.IDMessage` interface → unexported `idMessage` in `internal/cache`

Single-method (`GetEventID() string`), used at one site
(`internal/cache/manager.go:OnFeedMessageReceived`). Inlined as an
unexported `idMessage` interface in the new
`internal/cache/factory.go`.

### 51.3 `types.Response` struct + `types.ResponseWithCode` interface → `internal/api`

Both shape the API client's `OnAPIResponse` callback contract.
Pre-fix: a downstream consumer that imported `types.Response` for
testing the API client surface would compile; post-fix that
disappears. Verified: zero such callsites in the repo.

The consumer-visible `types.ResponseCode` enum (with
`OkResponseCode`, `ForbiddenResponseCode`, etc.) **stays** in
`types/` — those constants are useful when matching against wrapped
errors in consumer code.

### 51.4 `types/factory.go` deleted

After 41.1 the file was empty. Deleted.

### 51.5 Consumer impact

Zero. None of the relocated contracts were referenced by consumer
code (the public `*Client` surface uses concrete types or higher-
level interfaces).

## 52. v2.33 — Cluster by concept

Three groups of producer-state symbols had drifted into
conceptually-wrong files. Pure file-level moves; no signature
changes.

| Symbol | Was at | Now at |
|---|---|---|
| `ProducerStatus` interface | `types/message.go` | `types/producer.go` |
| `RecoveryState` enum + states | `types/recovery.go` | `types/producer.go` |
| `ProducerStatusReason` enum + reasons | `types/recovery.go` | `types/producer.go` |
| `ProducerDownReason` enum + reasons | `types/recovery.go` | `types/producer.go` |
| `ProducerUpReason` enum + reasons | `types/recovery.go` | `types/producer.go` |

Rationale: every relocated symbol describes producer-side state.
`message.go` was hosting `ProducerStatus` only because the interface
embeds the `Message` interface — a declaration site coincidence,
not a conceptual home. `recovery.go` was hosting the producer-state
enums because they appear in `RecoveryMessage.ProducerStatus`
payloads — but they describe the *producer*, not the recovery
process. After the move, [types/recovery.go](types/recovery.go) is
limited to per-request recovery types (`RecoveryMessage`,
`EventRecoveryMessage`, `RecoveryRequestStatus`, `RecoveryResult`;
the session→recovery processing hooks formerly published as
`types.RecoveryMessageProcessor` moved unexported into the gosdk
root package in the v1.0.0 surface pass — internal wiring, not
consumer surface); [types/producer.go](types/producer.go)
gathers everything producer-state-related.

### 52.1 Consumer impact

Zero. The exported names did not change — only the file each lives
in. Consumer imports remain `github.com/oddin-gg/gosdk/types`; the
`types.ProducerStatus` / `types.RecoveryState` / etc. references
resolve identically.

## 53. v2.34 — Full-review correctness pass (api / feed / cache / recovery / factory / lifecycle)

Closes every confirmed finding from the pre-beta full-codebase review
(two independent reviewers). Consumer-visible changes:

### 53.1 Public surface

- **`gosdk.ErrAlreadyClosed` / `gosdk.ErrInvalidConfig` exported**
  (NEXT.md §10). Connect/Subscribe on a closed client, and
  `Subscription.Err()` after the parent client closed, now match
  `errors.Is(err, gosdk.ErrAlreadyClosed)`. `gosdk.New` validates the
  config up-front (empty token, unresolvable environment/region) and
  returns wrapped `ErrInvalidConfig` before any HTTP.
- **`Subscription.Close(ctx)` implements the documented graceful
  drain** (NEXT.md §8): the first caller's ctx is the drain deadline —
  the in-flight AMQP delivery finishes its decode+admit+ack cycle (no
  Nack), intake stops, and admitted messages drain to the consumer.
  If the deadline expires first, the remaining buffered messages are
  discarded and `Err()` returns the ctx error. Session close + drain
  share ONE budget (min(ctx, WithShutdownTimeout)) — previously each
  stage got its own full `shutdownTimeout`, and ctx was only a wait
  bound with `Err()` always nil.
- **`WithInitialSnapshotTime` is now functional** (was stored but never
  read): a producer with no recovery cursor requests its first snapshot
  from `now - initialSnapshotTime` instead of full history. Zero (the
  default) keeps the previous full-history behaviour.
- **`BetCancel` / `RollbackBetCancel` `StartTime()`/`EndTime()` fixed**:
  the wire carries epoch milliseconds; they were decoded as seconds,
  yielding year-58000 timestamps. Fields now `*int64` end-to-end.
- **No more `Some("")` market/outcome names**: a home/away substitution
  whose competitor name is unavailable in the requested locale now
  yields `None` instead of a bogus `Some("")`, and message events are
  built with the full preload set so the competitor names are populated.
  Note the home/away **outcome** substitution keys off the English wire
  template (`"home"`/`"away"`) as its locale-independent MATCH key, then
  substitutes the competitor's name in the REQUESTED locale — so a
  ru/de consumer gets the localized team name, not the English template
  (see §17.7). A locale in which the competitor name was never loaded
  falls through to `None` (never `Some("")`).
- **`types.Category.IconPath Optional[string]`** — main-branch parity
  (feat(category), PR #43): decoded from the `icon_path` attribute and
  populated on the tournament snapshot's `Category`.

### 53.2 Reliability fixes (no API change)

- Feed reconnect loop no longer dies after 15 minutes of broker outage
  (backoff/v5's default `MaxElapsedTime` is now disabled) and no longer
  leaks a live connection when `Close` races a successful re-dial.
- Cache invalidation is no longer silently undone by an in-flight load
  (tombstone + singleflight-Forget in the event cache), and
  `RecoverEvent*` with a non-cancellable ctx cannot block forever when
  it races `Close`.
- Market-description caching actually caches variants now: the cache
  key compared its `*string` variant field by POINTER, so every variant
  lookup missed, refetched, and inserted a duplicate entry. By-id
  misses also share the bulk singleflight and mark the locale loaded;
  one malformed catalog row no longer fails the whole locale.
- Bounded caches (match statuses, players, variant descriptions) and
  icon side-maps cleaned on LRU eviction — together with the above,
  this addresses the "SDK cache taking too much space" consumer report.
- Transport errors on dedupe-key-less replay POSTs are terminal (no
  duplicate side effects); 429 is retryable; all 2xx are success; API
  errors are typed (`errors.As` → `*api.Error`); `APIEvent.Err` is
  redacted like the URL field.

### 53.3 Internal-API delta (contributors)

```diff
- recovery.NewManager(cfg, pm, apiClient, logger, initialSnapshotTime)  // new arg
- newRecoveryActor(..., inboxSize, initialSnapshotTime)                 // new arg
- recovery.Manager: + InboxDropCount() (non-tick feed-event drops now logged/counted)
- producer.Manager: + ErrNotOpened sentinel; GetProducerCached propagates it
                    + UnknownProducerPlaceholder(id) (diagnostics-only fallback)
- feed.ChannelConsumer: + CloseGraceful(ctx)
- sdkOddsFeedSession:   + CloseGraceful(ctx)
- cache.newMarketDescriptionCache(client, logger)                       // new arg
- cache.CompositeKey.Variant: *string → string ("" = base)
- api: + Error type; maxRetries → maxAttempts
```

## Questions

Open an issue or ping the SDK channel. The reference design lives in
[NEXT.md](NEXT.md) §0–§19 with the full rationale.
