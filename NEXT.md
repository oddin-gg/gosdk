# gosdk rewrite — design plan (target: v1.0.0)

Design plan for the gosdk rewrite. Lives on the `next` branch. The current module is at `v0.0.x` pseudo-versions, so breaking changes are still in-charter under semver. Output of this rewrite is a real `v1.0.0` release on the existing import path (`github.com/oddin-gg/gosdk`) — no `/v2` directory, no `v2` branch. Pre-Phase-6 tags are internal alphas (`v1.0.0-alpha.N`); first consumer-facing beta (`v1.0.0-beta.1`) cuts at the end of Phase 6 once the public `Client` is wired up. See §13 for full tag cadence.

This document is the source of truth for the rewrite. Update it as decisions change.

## 0.0 Why this rewrite — the original SDK was structurally unsound

The trigger for the rewrite is not "we want a nicer API". It's that the
legacy `v0.0.x` SDK accumulated enough load-bearing defects, with **zero
test coverage to catch any of them**, that landing fixes in place became
slower and riskier than starting over. The defect catalogue, captured
across consumer incidents and the v2.x review pass:

- **Configuration is broken.** `OddsFeedConfiguration.SetX()` is a
  value-receiver setter chain; some compilers silently drop intermediate
  state across chained calls. There is no way to set locale, `slog`
  logger, recovery cap, or HTTP timeout — the surface simply doesn't
  expose them.
- **Lifecycle deadlocks.** `Close()` races in-flight `Subscribe()`;
  `wg.Add(1)` after `wg.Wait()` panics with `WaitGroup misuse`. Hit in
  production. The legacy code carries no guard against this — admission
  and shutdown share no critical section.
- **Recovery races.** The recovery state machine runs on whichever
  goroutine called `OnRecoveryComplete`. Producer-down + concurrent
  `RecoverEvent*` calls hit data races on shared maps. `go test -race`
  has never been run against this code path.
- **Auto-ack AMQP, no backpressure.** The legacy consumer uses
  `noAck=true`. The broker considers every delivery acked instantly,
  so `prefetch` is meaningless — a stalled subscriber buffers unbounded
  in process memory until OOM. We have observed this in slow consumers.
- **Localization silently broken.** `Match.Name(locale)` returns
  whichever locale was cached *last*. There is no per-locale cache, no
  fill-in, no concurrency-safe per-locale fields. Consumers asking for
  `RuLocale` get `EnLocale` data after a different goroutine warmed the
  same key — and never know.
- **Hidden synchronous HTTP on accessor methods.** `match.Tournament()`
  silently performs an HTTP round-trip via `context.Background()` if the
  field is not loaded. Callers cannot impose timeouts; cancellation
  cannot reach the call. This has been a recurring source of latency
  surprises on the message hot path.
- **Retry loop leaks HTTP bodies.** The legacy API client retries
  without `resp.Body.Close()` on the failed attempt. Long-running
  consumers leak file descriptors until the OS limit fires.
- **Manager-of-managers indirection blocks mocking.** Every test of a
  consumer flow has to fake 2–3 layers of interface to reach the one
  HTTP call. Consumers don't write SDK-level tests for this reason —
  they paper over with end-to-end integration tests against staging.
- **`DefaulRegion` typo'd.** Yes, really. Exported, used by both
  consumers, can't be renamed without a breaking change.

The combined effect: **most of these bugs would be caught by a single
unit test or a `go test -race` run**. None exist. The legacy SDK has:

- `0` test files
- `0` lines of test code
- `0` CI gates (no race detector, no vet, no staticcheck)
- `0` coverage measurement

This is not a rewrite to *modernize*. It's a rewrite because the legacy
SDK is undefended against an entire class of bugs that the test pyramid
exists to catch, and we have evidence that those bugs reach production.
The rewrite ships with the test infrastructure as a first-class
deliverable — see §14 for coverage gates per package, §17 for the
phased rollout that requires CI-green at the end of every phase.

## 0. Resolved key decisions

(Documented up front because the reviewer flagged ambiguity in earlier drafts.)

1. **Lazy AMQP open.** `gosdk.New(ctx, cfg)` does API + cache + producer setup only — no AMQP. AMQP opens on the first `Subscribe(...)` call, or eagerly via the explicit `client.Connect(ctx)`. API-only usage works with a misconfigured or down message broker.
2. **Entity/message accessors do NOT do I/O.** Methods like `match.Name(locale)` and `market.Name(locale)` (read off `oddsChange.Markets()`) return `Optional[string]` from in-memory data. Missing locale → `None`. Callers prefetch with `client.Match(ctx, urn, locales...)` or rely on `WithPreloadLocales(...)`. No hidden synchronous HTTP from accessor methods.
3. **Event channels are buffered + lossy with logged drop.** All three (`ConnectionEvents`, `RecoveryEvents`, `APIEvents`) drop oldest on overflow and emit a `slog.Warn`. Polling getters (`client.ConnectionState()`, `client.ProducerStatus(id)`) expose current state for consumers that miss events. RecoveryEvents has the largest buffer (1024) since recovery completion notifications matter more.
4. **Versioning is `v0.0.x → v1.0.0` on the existing import path.** No `/v2` import path, no `v2` branch — Go requires a `/v2+` path suffix for `v2+` tags once a `go.mod` is present, so the first stable release is `v1.0.0` (not `v2.0.0`). "v2" is only the internal name for the rewrite; released tags are `v1.x`. Beta tags during the rewrite, then a clean `v1.0.0`.
5. **Recovery completions are not lossy.** `RecoveryEvents` (the channel) is lossy for liveness — but per-request completion is reliable. Each `RecoverEvent*` call returns a `*RecoveryHandle` with `Done()`/`Result()`/`Status()`. The SDK also exposes `client.EventRecoveryStatus(requestID)` for callers that only kept the request ID. A consumer that misses the channel event can still query the outcome.
6. **No deprecated surface at v1.0.0.** Every `// Deprecated:` field, method, type alias, and the `DefaulRegion` typo-alias must be removed before the stable `v1.0.0` tag. The Phase 6 cleanup pass enumerates and purges them; consumers receive the breaking changes in `MIGRATION.md` (e.g., `RefID()` removals on every entity, `SportEventRefID` dropped from `FixtureChange`, `EventRefID` dropped from feed messages, `DefaulRegion` removed). v1.0.0 ships with zero deprecated symbols — the rewrite is the breaking-change window.

7. **AMQP backpressure: manual-ack internally, block not drop.** The SDK consumes with `noAck=false` (manual ack) and acks each delivery only **after** its decoded message has been admitted to the subscription buffer. This makes broker-side `prefetch` actually mean something — unacked deliveries cap at the prefetch window, and a stalled consumer applies real backpressure to RabbitMQ. Auto-ack (the current SDK's behavior) is the wrong choice for this rewrite: with auto-ack the broker sees no unacked messages and `prefetch` is meaningless, leading to silent unbounded memory growth on slow consumers. Manual ack is purely internal; users never see an `Ack`/`Nack` API. Configurable prefetch via `WithAMQPPrefetch(n)`. Documented contract: subscribers MUST drain `Subscription.Messages()`; if they stall, RabbitMQ stops delivering after the prefetch window fills. (Event channels in §0.3 are still lossy — different concern, different policy.)

---

## 1. Goals

1. **Feature parity with Java SDK and .NET SDK.** Any capability either reference SDK exposes must exist in the Go rewrite. External clients should be able to choose any of the three and get the same product.
2. **Fix the bugs.** Lifecycle deadlocks, recovery races, configuration setters that silently lose mutations, retry loops that leak HTTP bodies — all gone.
3. **Reduce surface complexity.** Consumers complain about the manager-of-managers shape. Flatten to a single `Client` with direct methods. Eliminate interface pollution.
4. **Strong caching.** Public cache invalidation, proper concurrency, multi-locale support, per-call locale override.
5. **Test coverage.** Currently zero tests. Target: meaningful coverage on every package, deterministic tests on the recovery state machine and lifecycle.
6. **Modern Go used pragmatically.** Module directive `go 1.26.0` (consumer repos kollector / ots have been authorised to move to 1.26 alongside the v2.x SDK cutover). Use `log/slog`, generic type aliases, `iter.Seq`, `sync.WaitGroup.Go`, and the now-stable `testing/synctest` where they actually simplify the design — not as goals in themselves.
7. **Pragmatic migration.** Breaking changes are permitted, with a clear migration guide. Bootstrap code (~30 lines) is the easy diff; the message-consumption loop and producer/recovery semantics need real review per consumer (see §13).

## 2. Non-goals

- Removing functionality that .NET/Java expose (e.g., Replay, multi-session, all `MessageInterest` values). Keep all of it even though current Go consumers don't use it.
- Changing the wire protocol. AMQP routing keys, XML message envelopes, REST API endpoints stay identical to current — this is purely a Go SDK rewrite.
- Backward source compatibility with v0.0.x. We get one breaking change; we use it.
- A separate `/v2` module or `v2` branch. Same import path, beta tags during rewrite, `v1.0.0` at cutover.
- Holding the module directive at Go 1.24 indefinitely. The consumer floor moved to 1.26 once kollector / ots were cleared to bump; the directive now sits at `go 1.26.0` and is allowed to use 1.25 / 1.26 stdlib idioms (`sync.WaitGroup.Go`, stable `testing/synctest`, etc.).
- Sealing `types.Message` (or any other public interface) with unexported methods. Internal consumers already mock these interfaces in their tests; aggressive sealing would break that workflow without payoff.
  - **Documented exception: `types.BetStop`.** `BetStop` carries no payload distinct from `RequestMessage + EventMessage` — every other `Request*Message` interface (`OddsChange.Markets()`, `BetCancel.StartTime()`, `FixtureChangeMessage.ChangeType()`, …) has a distinguishing exported method, but BetStop genuinely has nothing extra. Without a marker, `case types.BetStop:` in the session's type-switch would also match every other `RequestMessage+EventMessage` shape and silently steal their dispatch. The chosen pattern: `types.BetStopMarker` is a public empty struct whose unexported `isBetStop()` method satisfies the `BetStop` interface seal. Concrete impls — including consumer mocks — embed the marker via composition (`type myMock struct { types.BetStopMarker; ... }`); a one-line add. This is the minimum intrusion that preserves both type-switch dispatch correctness and consumer-mockability. Do NOT extend this pattern to other interfaces; they already have natural exported discriminators.

## 3. Architectural principles

1. **Context everywhere.** Every method that does I/O or could block takes `ctx context.Context`. Cancellation is the only shutdown mechanism.
2. **One goroutine per concern.** Each long-lived task (AMQP consumer, recovery state machine per producer, reconnect loop, message router) owns its state and runs on a single goroutine. Communication is via channels, not shared memory.
3. **Idempotent lifecycle, faithful waiting.** Every `Close` method is safe to call repeatedly. The pattern: shutdown side effects run exactly once (`sync.Once` guards the trigger), but every caller of `Close(ctx)` waits on its own deadline for completion and gets the real terminal result — not a stale `nil`. See §8 Close.
4. **No bare channel sends.** Every `ch <- v` is wrapped in a `select`. Two patterns are allowed: blocking-with-cancellation (`select { case ch <- v: case <-ctx.Done(): return }` — used on the message path) and try-or-drop (`select { case ch <- v: default: /* log + drop */ }` — used on the lossy event channels in §0.3). A bare `ch <- v` is never acceptable.
5. **Errors wrap.** Every `fmt.Errorf` uses `%w`. Sentinel errors are exported. Callers get `errors.Is`/`errors.As`.
6. **Slim public surface.** A package-public function or type must justify its existence. Default to internal.
7. **No silent locale loss.** Locale is a first-class parameter on every query method, with the configured default as fallback.
8. **Test-first on critical paths.** Lifecycle, recovery state machine, cache, and HTTP retry are not merged without tests.

## 4. Public API surface

The new top-level package is still `github.com/oddin-gg/gosdk`. The `types/` subpackage retains all current entity types (Match, Tournament, Competitor, Player, OddsChange, BetSettlement, BookmakerDetail, Producer, ProducerScope, MessageInterest, URN, Locale, Environment, Region, …). Field shapes and method signatures on these types stay source-compatible where possible — both consumers (kollector-esport, ots-odds-bridge) import them widely.

Top-level package replaces the manager-of-managers shape with a flat `Client`:

```go
package gosdk

// Construction — does API + cache + producer setup; does NOT open AMQP.
func NewConfig(token string, env types.Environment, opts ...Option) Config
func New(ctx context.Context, cfg Config) (*Client, error)

// Lifecycle
func (*Client) Connect(ctx context.Context) error  // explicit AMQP open; optional — Subscribe will lazy-connect
func (*Client) Close(ctx context.Context) error    // idempotent; safe to call repeatedly

// Subscribe replaces SessionBuilder + Build(). First call opens the AMQP connection if Connect wasn't called.
// Subscription supports graceful drain via Close(ctx) and abrupt termination via client.Close / terminal error — see §8 Subscriptions. The Subscribe ctx bounds setup only.
func (*Client) Subscribe(ctx context.Context, opts ...SubscribeOption) (*Subscription, error)

// Subscription — returned from Subscribe. All methods safe for concurrent use.
// (*Subscription).Messages() <-chan types.SessionMessage // envelope: tagged-union {OddsChange|BetStop|BetSettlement|BetCancel|FixtureChange|Rollback*} + UnparsableMessage + RawFeedMessage; closed after drain
// (*Subscription).Close(ctx context.Context) error       // graceful drain; ctx is the drain deadline; safe to call repeatedly
// (*Subscription).Done() <-chan struct{}                 // closed when subscription terminates (any reason)
// (*Subscription).Err() error                            // nil on graceful close; non-nil on client.Close-abort / terminal error
// See §3 "Subscribe / Subscription" below for the consuming example.

// Bookmaker / connection state
func (*Client) BookmakerDetails(ctx context.Context) (types.BookmakerDetail, error)
func (*Client) ConnectionState() ConnectionState         // current state, polling-friendly
func (*Client) ConnectionEvents() <-chan ConnectionEvent // state-change events; lossy on overflow
func (*Client) APIEvents() <-chan APIEvent               // raw HTTP request/response events (opt-in via WithAPICallLogging)

// Producers
func (*Client) Producers(ctx context.Context) ([]types.Producer, error)
func (*Client) ActiveProducers(ctx context.Context) ([]types.Producer, error)
func (*Client) ProducersInScope(ctx context.Context, scope types.ProducerScope) ([]types.Producer, error)
func (*Client) Producer(ctx context.Context, id int) (types.Producer, error)
func (*Client) SetProducerEnabled(ctx context.Context, id int, enabled bool) error
func (*Client) SetProducerRecoveryFromTimestamp(ctx context.Context, id int, t time.Time) error  // NEW (parity)

// Recovery — every initiate call returns a handle for reliable per-request completion.
func (*Client) RecoverEventOdds(ctx context.Context, producerID int, eventID types.URN) (*RecoveryHandle, error)
func (*Client) RecoverEventStateful(ctx context.Context, producerID int, eventID types.URN) (*RecoveryHandle, error)
func (*Client) RecoveryEvents() <-chan RecoveryEvent           // ProducerStatus + EventRecoveryComplete stream; lossy on overflow
func (*Client) ProducerStatus(producerID int) (types.ProducerStatus, bool)  // current snapshot, polling-friendly; false = no status yet
func (*Client) EventRecoveryStatus(requestID int) (types.RecoveryResult, bool) // by request ID; second result false when unknown / GC'd

// RecoveryHandle exposes per-request semantics. Tracked internally until completion + a grace period.
// (*RecoveryHandle).RequestID() int
// (*RecoveryHandle).Done() <-chan struct{}        // closes on any terminal state
// (*RecoveryHandle).Result() RecoveryResult           // blocks until Done; returns terminal status
// (*RecoveryHandle).Status() RecoveryRequestStatus     // non-blocking snapshot

// Sports info
func (*Client) Sports(ctx context.Context, locales ...types.Locale) ([]types.Sport, error)
func (*Client) Sport(ctx context.Context, id types.URN, locales ...types.Locale) (types.Sport, error)
func (*Client) ActiveTournaments(ctx context.Context, locales ...types.Locale) ([]types.Tournament, error)
// ActiveTournamentsForSport mirrors Java's getActiveTournaments(sportName, locale)
// and .NET's GetActiveTournaments(name) — name lookup runs against every supplied
// locale's catalog (not just default). See MIGRATION §29.5.
func (*Client) ActiveTournamentsForSport(ctx context.Context, sportName string, locales ...types.Locale) ([]types.Tournament, error)
func (*Client) AvailableTournaments(ctx context.Context, sportID types.URN, locales ...types.Locale) ([]types.Tournament, error)
// Tournament resolves a single tournament by URN; sportID inferred from the
// tournament-info cache (matches the documented "client.Tournament(ctx, urn)
// for each urn in sport.TournamentIDs" migration shape). See MIGRATION §29.6.
func (*Client) Tournament(ctx context.Context, id types.URN, locales ...types.Locale) (types.Tournament, error)
func (*Client) Match(ctx context.Context, id types.URN, locales ...types.Locale) (types.Match, error)
func (*Client) MatchesFor(ctx context.Context, t time.Time, locales ...types.Locale) ([]types.Match, error)
func (*Client) LiveMatches(ctx context.Context, locales ...types.Locale) ([]types.Match, error)
func (*Client) ListMatches(ctx context.Context, start, limit int, locales ...types.Locale) ([]types.Match, error)
func (*Client) Competitor(ctx context.Context, id types.URN, locales ...types.Locale) (types.Competitor, error)
func (*Client) Player(ctx context.Context, id types.URN, locales ...types.Locale) (types.Player, error)
func (*Client) FixtureChanges(ctx context.Context, after time.Time, locales ...types.Locale) ([]types.FixtureChange, error)

// Cache invalidation (NEW — parity with .NET/Java).
// `variant types.Optional[string]` (v2.29 reshape from the original
// *string plan): None selects the base description with no variant;
// Some("") is normalised to None rather than rejected.
func (*Client) ClearMatch(id types.URN)
func (*Client) ClearTournament(id types.URN)
func (*Client) ClearCompetitor(id types.URN)
func (*Client) ClearPlayer(id types.URN)
func (*Client) ClearFixture(id types.URN)
func (*Client) ClearMatchStatus(id types.URN)
func (*Client) ClearSport(id types.URN)
func (*Client) ClearMarketDescription(marketID int, variant types.Optional[string])
func (*Client) ClearMarketVoidReasons()
func (*Client) ReloadMarketVoidReasons(ctx context.Context) ([]types.MarketVoidReason, error)

// Market descriptions. `variant types.Optional[string]` preserves the
// none-vs-set distinction the wire protocol cares about (v2.29 reshape).
func (*Client) MarketDescriptions(ctx context.Context, locales ...types.Locale) ([]types.MarketDescription, error)
func (*Client) MarketDescription(ctx context.Context, id int, variant types.Optional[string], locales ...types.Locale) (*types.MarketDescription, error)
func (*Client) MarketVoidReasons(ctx context.Context) ([]types.MarketVoidReason, error)

// Replay (kept verbatim — surface drives the underlying API the same way as Java/.NET)
func (*Client) Replay() *Replay

type Replay struct{ /* methods below */ }
func (*Replay) List(ctx context.Context) ([]types.Match, error)   // Phase 6 reshape: Match values (SportEvent interface retired)
func (*Replay) EventIDs(ctx context.Context) ([]types.URN, error) // queued URNs without per-entry Match resolution
func (*Replay) AddEvent(ctx context.Context, eventID types.URN) error
func (*Replay) RemoveEvent(ctx context.Context, eventID types.URN) error
func (*Replay) Start(ctx context.Context, opts ...ReplayOption) error
func (*Replay) Stop(ctx context.Context) error
func (*Replay) Clear(ctx context.Context) error
func (*Replay) StopAndClear(ctx context.Context) error  // NEW (parity with .NET)
func (*Replay) Status(ctx context.Context) (string, error)  // NEW (parity with .NET)
```

### Configuration via functional options

Replaces the broken value-receiver setter chain:

```go
cfg := gosdk.NewConfig(token, types.TestEnvironment,
    gosdk.WithNodeID(1),
    gosdk.WithDefaultLocale(types.EnLocale),
    gosdk.WithPreloadLocales(types.EnLocale, types.RuLocale),
    gosdk.WithRegion(types.RegionDefault),
    gosdk.WithAPIHost("..."),
    gosdk.WithMQHost("..."),
    gosdk.WithMessagingPort(5672),
    gosdk.WithExchangeName("oddinfeed"),
    gosdk.WithSportIDPrefix("od:sport:"),
    gosdk.WithMaxInactivity(20*time.Second),
    gosdk.WithMaxRecoveryExecution(6*time.Hour),
    gosdk.WithInitialSnapshotTime(30*time.Minute),  // NEW (parity)
    gosdk.WithHTTPClientTimeout(30*time.Second),    // NEW (parity)
    gosdk.WithExceptionStrategy(gosdk.StrategyCatch), // NEW (parity)
    gosdk.WithLogger(slog.Default()),               // NEW (parity)
    gosdk.WithExtendedDataReporting(true),
    gosdk.WithAPICallLogging(gosdk.APILogResponses), // NEW: opt-in API call observability
    gosdk.WithShutdownTimeout(5*time.Second),       // NEW: graceful-shutdown cap
)
```

`Config` is an immutable value externally. Internally, `NewConfig` constructs a private draft, applies each `Option func(*Config)` to the draft, and returns the finalized value by copy. After return, the `Config` value cannot be mutated — `Option` closures don't escape `NewConfig`. No setter-chain pitfalls; no shared mutable state.

### Subscribe / Subscription

#### Normal subscription — option resolution

```go
// Option resolution (see MIGRATION §28.1):
//   - No options                  → AllMessageInterest (every message).
//   - WithSpecificEvents alone    → SpecifiedMatchesOnly (per-event
//     AMQP routing keys).
//   - WithMessageInterest(All)    → "subscribe to all, filter
//     manually" — passing WithSpecificEvents alongside it does NOT
//     narrow routing (caller filters by event ID in their consumer).
sub, err := client.Subscribe(ctx,
    gosdk.WithSpecificEvents(eventA, eventB), // event-specific routing
)
```

#### Replay subscription — separate flow

Replay subscriptions consume the entire replay exchange (all routing
keys); event selection is done via the replay queue API, NOT via
`WithSpecificEvents`. Combining the two has no narrowing effect — the
session ignores specific-event routing on the replay path
([client.go's `routingKeys`](client.go) maps replay to
`AllMessageInterest`).

```go
// 1) Queue the events to replay via the replay-queue API.
if err := client.Replay().AddEvent(ctx, eventA); err != nil { return err }
if err := client.Replay().AddEvent(ctx, eventB); err != nil { return err }

// 2) Open a replay subscription. WithReplay marks the session as
//    replay-mode (uses the replay exchange + the dummy recovery
//    manager). No WithSpecificEvents / WithMessageInterest needed.
sub, err := client.Subscribe(ctx, gosdk.WithReplay())
if err != nil { return err }

// 3) Start playback when ready.
if err := client.Replay().Start(ctx); err != nil { return err }
```

#### Consuming messages

```go
for env := range sub.Messages() {
    switch {
    case env.OddsChange != nil:            // ...
    case env.BetStop != nil:               // ...
    case env.BetSettlement != nil:         // ...
    case env.BetCancel != nil:             // ...
    case env.FixtureChange != nil:         // ...
    case env.RollbackBetSettlement != nil: // ...
    case env.RollbackBetCancel != nil:     // ...
    case env.UnparsableMessage != nil:     // SDK couldn't decode the body
    }
    if env.RawFeedMessage != nil { ... } // when WithExtendedDataReporting(true)
}
// Extended-data expansion — with WithExtendedDataReporting(true), ONE
// decodable AMQP delivery becomes TWO envelopes on Messages(), in order:
//   1. an envelope with ONLY RawFeedMessage set (every variant field nil),
//   2. the envelope carrying the parsed variant or UnparsableMessage
//      (RawFeedMessage nil).
// The fields are mutually exclusive across the pair — no envelope carries
// both. Deliveries the session drops (alive traffic, disabled/out-of-scope
// producer, admitted snapshot_complete) emit only envelope 1; deliveries
// whose body fails XML decode emit only an UnparsableMessage envelope (raw
// is built for decodable bodies only). Consumers counting or correlating
// deliveries must not count RawFeedMessage envelopes as separate
// deliveries. With reporting off: AT MOST one envelope per delivery —
// intentionally dropped deliveries (message-interest filtering, replay
// isolation) emit zero (see types.SessionMessage).
// The ctx passed to Subscribe bounds SETUP only (lazy-connect dial, queue
// declaration) — cancelling it later does NOT close a live subscription.
// The subscription lives until sub.Close(ctx), client.Close(ctx), or a
// terminal error.
err = sub.Err()  // sticky, set on terminal failure
```

`Subscription.Messages()` returns `<-chan types.SessionMessage`. The
envelope is a tagged union (v2.25): exactly one of the embedded
`EventMessage` variants OR `UnparsableMessage` is non-nil per
delivery. The variants are:

- `OddsChange`, `BetStop`, `BetSettlement`, `BetCancel`,
  `FixtureChange`, `RollbackBetSettlement`, `RollbackBetCancel` —
  the parsed feed-message types.
- `UnparsableMessage` — populated when the SDK couldn't decode the
  body; consumers can still observe a malformed delivery.
- `RawFeedMessage` — populated only when
  `WithExtendedDataReporting(true)` is set; carries the raw XML and
  routing-key metadata for trace/diagnostics.

The session-vs-global split disappears — recovery and connection events surface on `client.RecoveryEvents()` and `client.ConnectionEvents()`, message data on the subscription.

### Connection events (NEW — parity gap)

```go
type ConnectionEvent struct {
    Kind     ConnectionEventKind  // Connected, Disconnected, Reconnecting, Closed
    Err      error                // populated on Disconnected
    At       time.Time
}

for ev := range client.ConnectionEvents() { ... }
```

Closes the gap vs Java's `onConnectionDown` and .NET's `ConnectionException` / `Disconnected` / `Closed`.

### `types/` — entities are value structs (plan superseded)

> **Superseded during implementation.** The original plan here kept the
> entity interfaces (`Match`, `Tournament`, `Competitor`, `Player`,
> `Sport`, `Fixture`, `Scoreboard`, `MatchStatus`, `MarketDescription`,
> `MarketVoidReason`, `OutcomeDescription`, `Specifier`, …) with their
> v0.x method signatures as the source-compatibility line. The shipped
> v1.0.0 instead reshapes ALL of these into eagerly-populated **value
> structs** (fields, not accessor methods; optionals as
> `types.Optional[T]`) — see MIGRATION §10 for the rationale
> (hidden-I/O elimination, immutability, hot-path cost) and the full
> accessor→field mapping. Message types (`OddsChange`, `BetStop`,
> `BetSettlement`, `BetCancel`, the rollbacks, `FixtureChange`,
> `UnparsableMessage`, `ProducerStatus`, `EventRecoveryMessage`)
> remain interfaces. Locale-parameterised reads survive as map/`Name(locale)`
> lookups populated at construction for the preloaded locales.

## 5. Internal architecture

Layered, with no upward dependencies:

```
+----------------------------------------------------------------+
|                    gosdk (public Client)                       |
|       Subscribe / Recovery events / catalog methods            |
+----------------------------------------------------------------+
|       internal/feed                internal/recovery           |
|     (AMQP consumer +              (per-producer actor          |
|      ChannelConsumer)               state machine)             |
+----------------------------------------------------------------+
|  internal/cache       internal/api         internal/factory    |
|  (LRU + map caches    (HTTP client +       (XML→types builders |
|   + cache/lru shim)    api/xml decoders)    for messages /     |
|                                             markets / replay)  |
+----------------------------------------------------------------+
|  internal/producer  internal/sport  internal/market            |
|  internal/replay    internal/whoami internal/utils             |
|  internal/log       internal/feed/xml (feed-side decoders)     |
+----------------------------------------------------------------+
|                  types (public entity types)                   |
+----------------------------------------------------------------+
```

The XML decoders are split per layer (HTTP responses live under
`internal/api/xml`; AMQP feed envelopes under `internal/feed/xml`).
The original NEXT plan had a single `internal/xml` — splitting kept
the API and feed wire formats from leaking into each other and let
each side own its golden-file fixtures (`internal/feed/xml/testdata`).

Every layer is testable in isolation:
- `internal/api/xml`, `internal/feed/xml` — pure decode; `feed/xml` has direct decode tests, `api/xml` is exercised through the api/cache/factory suites (direct golden-file tests remain open work).
- `internal/api` — `httptest.Server`-backed tests for every endpoint.
- `internal/cache` (+ `internal/cache/lru`) — concurrency tests, TTL tests, single-flight dedup tests.
- `internal/recovery` — deterministic state-machine tests driven by fake tickers/handshakes (no mock-clock framework; see §14 Tools).
- `internal/feed` — in-process tests for the dial loop, channel consumer, and process-delivery path; goroutine-leak guards via structural done-channel joins (`goleak` was never added — see §14 Tools).
- `internal/factory` — table-driven tests against captured XML samples for message + market builders.
- `internal/producer`, `internal/replay`, `internal/sport`, `internal/whoami` — per-manager unit tests with mocked api/cache deps.
- `gosdk` — integration tests stitching it all together.

## 6. Caching

Two cache flavors as decided:

### Per-event LRU caches (with TTL and singleflight)

For: `MatchCache`, `CompetitorCache`, `FixtureCache`, `TournamentCache`, `PlayersCache`.

```go
package cache

import (
    lru "github.com/hashicorp/golang-lru/v2/expirable"
    "golang.org/x/sync/singleflight"
)

type Loader[K comparable, V any] func(ctx context.Context, key K, locales []types.Locale) (V, error)

type EventCache[K comparable, V any] struct {
    lru    *lru.LRU[K, V]
    sf     singleflight.Group
    loader Loader[K, V]
}

func (c *EventCache[K, V]) Get(ctx context.Context, key K, locales []types.Locale) (V, error)
func (c *EventCache[K, V]) Clear(key K)
func (c *EventCache[K, V]) Purge()
```

- `Get` returns the cached entry if all requested locales are present; otherwise calls `loader` (deduplicated via singleflight) and merges fetched locales into the entry.
- `Clear` is the public invalidation hook (`client.ClearMatch(urn)` etc.).
- `Purge` is invoked on shutdown.
- Every cache value is a struct with explicit per-locale fields (see §7). All field access is mutex-protected within the entry — no partial-locking like the current code.
- Default capacity per cache: configurable, sensible defaults (e.g., 5000 matches, 50000 competitors). Default TTL: 12h to match current behavior.

### Static-catalog caches (map + RWMutex)

For: base `MarketDescriptionCache` (everything the bulk catalog returns — see the correction under "Variant / dynamic market descriptions" below), `MarketVoidReasonsCache`, `MatchStatusDescriptionCache`, `SportsCache`.

Each catalog cache implements this pattern directly (per-locale maps under an
RWMutex, loaded lazily per locale). An earlier shared `StaticCache[K, V]`
generic sketched here was built but never wired in, and was removed as dead
code — its loader ran under the entry mutex (uncancellable waiters), it
returned internal maps by reference, and its loaded flag was permanent, the
exact staleness class the shipped caches fixed with the expiring catalog
marks below.

- Loaded per locale on first access; subsequent reads hit the map under RLock.
- **The loaded-per-locale mark expires** (`defaultCatalogTTL`, 12h), so the catalog is re-fetched at most once per window. It was originally permanent for the process lifetime, which meant each catalog was downloaded exactly once and anything **added or renamed upstream** stayed invisible to a long-running consumer until it restarted — a new market, a new sport, or a new tournament for an existing sport. `SportCache` applies the same window to its per-sport tournament lists (`LocalizedSport.tournamentsLoadedAt`), and refreshing those **replaces** the tournament set rather than merging it, so a tournament removed upstream also disappears.
- Making a once-per-process load **recurring** brings two concurrency obligations that a load-once flag hides, and both apply to any future cache converted this way:
  - **Coalesce the refresh.** Every caller that finds the data stale at the same instant would otherwise issue its own fetch — a thundering herd on each expiry, rather than the single first-load race a permanent flag allowed. All refresh paths go through `lru.LoadCoalesced` with a generation-prefixed key.
  - **Make the commit monotonic.** Replacement plus expiry opens a lost-update window that neither opens alone: concurrent post-expiry refreshes can complete out of order (different locales are different flight keys, so coalescing alone does not serialize them), and an earlier-started fetch finishing last would reinstall its older snapshot and stamp it fresh — serving data already known to be superseded for a full window. A snapshot no newer than the committed one is rejected, and the freshness stamp only ever advances. Same discipline as the producer cursors.
- `LocalizedStaticDataCache` is deliberately outside this scheme: it already refreshes every loaded locale from a 24h background ticker (`timerTick`), which solves the same staleness problem by a different mechanism, including an atomic per-locale replace that drops ids absent from the fresh response.
- The catalog refresh is a **replace, not an accumulate**: each bulk load reconciles per locale — entities the complete response no longer carries lose that locale, and entries with no usable data left are dropped (`reconcileBulk` / `reconcileCatalog`). An empty-but-successful response carries no removal authority (indistinguishable from a broken one), and stores are additionally ordered by a per-locale monotonic fetch cursor so a stale pre-clear flight finishing after a newer one cannot overwrite fresh rows or resurrect reconciled-away entries.
- **No `sync.Once`.** Failed loads (network error, 5xx) reset `loaded=false` so the next access retries. `sync.Once` is a footgun here — a transient failure would otherwise poison the cache forever.
- `Clear` resets the entry for that locale, forcing a refresh on next access.
- No size limit — these catalogs are small (hundreds of entries).

### Variant / dynamic market descriptions (LRU, not static)

**CORRECTED — this section as originally written was the source of a production defect.** It claimed that *any* description carrying a variant is outside the static catalog, and the implementation followed by routing on "does the key have a variant string". That is false: a large minority of the bulk catalog carries a **static** variant. The live test-env catalog is 229 rows, of which **47 carry a variant and all 47 are static** (`way:*`, `best_of:*`, `best_of_games:*`, `best_of_rounds:*`, `gnr:*`, `mr:*`, `st:*`); it contains **zero** `od:dynamic_outcomes:` rows. Putting those 47 in a 12h-TTL LRU made them unrecoverable once they expired, because the by-id refill path short-circuits on the already-loaded locale flag — consumers dropped every odds change carrying such a market ~12h after each restart.

The split is by **provenance**, not by key shape:

- **Bulk catalog** (`/descriptions/{locale}/markets`) — plain rows *and* static variants. Permanent map, no eviction, restorable only by another bulk load.
- **Per-variant endpoint** (`/descriptions/{locale}/markets/{id}/variants/{variant}`) — the `od:dynamic_outcomes:` family only. This is the genuine unbounded long tail (one entry per `(marketID, variant, locale)` tuple, `variant` encoding things like `mapnr=1`, `setnr=3`), and it is the only family safe in a bounded LRU: its by-id miss re-fetches that single key and is not gated on the loaded-locale flag. Bounded LRU + singleflight, same shape as the per-event caches. Default capacity: 5000 entries.

The loaded-locale mark itself also expires (`marketCatalogTTL`, 12h). It was permanent for the process lifetime, so the bulk catalog was downloaded exactly once and markets added or renamed upstream never appeared until restart.

### Cache invalidation triggers

- **Public methods** on `Client`: `ClearMatch`, `ClearTournament`, `ClearCompetitor`, `ClearPlayer`, `ClearMarketDescription`, `ClearMarketVoidReasons`. Parity with .NET/Java.
- **Auto-invalidation** on `FixtureChange` feed message: clears the affected match cache entry. Existing behavior, kept.
- **TTL eviction**: per-cache TTL handled by `expirable.LRU`.
- **LRU eviction**: per-cache size cap.

## 7. Localization

### Configuration

```go
gosdk.WithDefaultLocale(types.EnLocale)
gosdk.WithPreloadLocales(types.EnLocale, types.RuLocale, types.DeLocale)
```

`WithPreloadLocales` controls which locales the SDK fetches eagerly when warming static catalogs (sports, market descriptions). Per-event entities are still fetched lazily per locale on first request.

### Locale enum expansion

Current Go enum: `EnLocale`, `RuLocale`, `ZhLocale` (3 values).

New enum matches .NET/Java's 12: `en`, `br`, `de`, `es`, `fi`, `fr`, `pl`, `pt`, `ru`, `th`, `vi`, `zh`. The constant names follow current `XxLocale` convention (e.g., `BrLocale`, `DeLocale`, …). The `Locale` type itself stays a string alias so values can be added without recompiling consumers.

### Per-call locale plumbing

Every public query method takes `locales ...types.Locale` (variadic, defaults to configured default if empty):

```go
match, err := client.Match(ctx, urn)                              // default locale
match, err := client.Match(ctx, urn, types.RuLocale)          // explicit
match, err := client.Match(ctx, urn, types.EnLocale, types.RuLocale)  // multi
```

Inside the cache, `Get` is called with the requested locale slice. If any requested locale is missing from the entry, the loader fetches *only the missing locales* (one API call per missing locale, deduplicated across concurrent callers via singleflight).

### Entity / message accessors are pure data — no hidden I/O

This is the resolution to a fundamental tension: `match.Name(locale)` cannot accept a `ctx`, but it must not perform synchronous I/O without one. We resolve it by making accessors pure-data:

- `client.Match(ctx, urn, locales...)` — performs I/O (fetches missing locales), returns a `Match` whose internal cache is populated for the requested locales.
- `match.Name(locale)` — returns the cached value as `Optional[string]`. Returns `None` if the locale wasn't requested at fetch time.
- Same pattern for `Tournament`, `Competitor`, `Player`, `Sport`, etc.

**v2.x reshape** — every locale-keyed string accessor returns `Optional[string]`. This unifies the previous `string` (silent "" on miss) and `*string` (nil on miss) idioms into one. Use `.ValueOr("")` for the always-string ergonomics, `.Get()` to detect "not loaded" explicitly. Migration code that needs strict-error semantics:

```go
name, ok := match.Name(types.RuLocale).Get()
if !ok { return fmt.Errorf("missing locale ru for match %s", match.ID) }
```

For feed messages (`OddsChange`, `BetSettlement`, …) the same rule applies: messages contain market descriptions for every locale in `WithPreloadLocales(...)`. **At message-decode time, the SDK eagerly enriches each market on the message with the corresponding market description from the cache for every preloaded locale.** This makes `market.Name(locale)` an in-memory lookup with no possibility of blocking or I/O. Reading a market in an un-preloaded locale returns `None`; the consumer must call `client.MarketDescription(ctx, id, variant, locale)` first to prime the cache (and only future messages will pick up that locale — already-decoded messages don't retroactively enrich).

```go
// Sample usage with explicit prefetch:
match, err := client.Match(ctx, urn, types.EnLocale, types.RuLocale)
ru := match.Name(types.RuLocale).ValueOr("")  // cached → instant
de, ok := match.Name(types.DeLocale).Get()    // ok == false (not preloaded)

// Sample feed-message usage:
cfg := gosdk.NewConfig(token, env, gosdk.WithPreloadLocales(types.EnLocale, types.RuLocale))
// ... in message loop ...
markets := oddsChange.Markets()              // OddsChange.Markets() — no locale param (matches existing protocols)
name := markets[0].Name(types.RuLocale).ValueOr("") // locale lives on the per-market accessor; cached at startup → instant; never blocks
```

This differs from .NET's "sync fetch on demand" but is the right Go idiom: synchronous I/O without `ctx` from a hot message-processing goroutine is a deadlock waiting to happen. Migration cost: callers that want non-default locales must enumerate them up front. Documented in `MIGRATION.md`.

## 8. Lifecycle & cancellation

### Construction

`gosdk.New(ctx, cfg)` — API/cache/producer setup only. **Does NOT touch AMQP.** API-only usage (e.g., a CLI that reads market descriptions) succeeds even when the message broker is unreachable.

1. Validate config.
2. Construct logger from config.
3. Construct HTTP API client.
4. Fetch BookmakerDetails (one call, blocking).
5. Construct producer manager. *(Superseded during implementation:
   the producer CATALOG fetch happens on the connect path —
   `producerManager.Open` inside Connect/first Subscribe — not at
   construction; an API-only client that never connects performs no
   producers fetch.)*
6. Construct caches; warm static catalogs for every locale in `WithPreloadLocales(...)`.
7. ~~Spawn recovery state-machine goroutine per active producer in
   **dormant mode** … Actors arm themselves on the first AMQP-related
   event.~~ **Superseded:** no dormant-actor lifecycle exists.
   Construction stores an UNOPENED `recovery.Manager` placeholder (so
   the atomic handle is never nil) but spawns NO goroutines and NO
   actors; the manager is OPENED on the connect path only (see §11), which
   pre-spawns actors for the active-producer catalog and lazily spawns
   later ones. The design GOAL this step encoded still holds: API-only
   clients stay completely quiet — achieved by not creating recovery
   machinery at all until Connect. Producer-down suppression right
   after connect is handled by the tick loop's warm-up gate
   (`inactivityArmed`), not a dormant mode.
8. Return `*Client` ready for API/cache calls.

If any step fails, partial state is torn down and an error is returned.

### Connecting to the message broker

Two paths:

- **Explicit:** `client.Connect(ctx)` opens AMQP, registers `NotifyClose`, spawns the reconnect goroutine. Useful when you want fail-fast at boot.
- **Lazy:** the first `client.Subscribe(...)` call opens AMQP if not already connected. The most common path for a typical feed consumer.

Connect is **NOT** `sync.Once`. A failed first `Connect(ctx)` (e.g., broker unreachable, transient DNS error) must not poison subsequent attempts. Implementation: a small state machine guarded by a mutex, with three states (`notConnected`, `connecting`, `connected`) and `singleflight` deduplication for concurrent callers:

- Concurrent `Connect`/`Subscribe` calls during `connecting` all wait on the same in-flight attempt and observe the same outcome.
- On success, state moves to `connected`; further calls are no-ops returning `nil`.
- On failure, state returns to `notConnected`; the next call retries from scratch.
- Once `connected`, the reconnect goroutine handles transient drops; `Connect`/`Subscribe` never re-enter the connect state machine.

### Close

**Shutdown starts once; waiting is per-call.** This separates two concerns the older "wrapped in `sync.Once`" design conflated: who triggers cleanup vs. who waits for it. The trap with `sync.Once` is that a first caller with a tight deadline returns `ctx.Err()` while a second caller returns `nil` immediately — making it look like cleanup completed when it didn't.

Implementation:

```
type Client struct {
    closeOnce sync.Once
    closed    chan struct{}   // closed by the shutdown goroutine when cleanup is done
    closeErr  error           // written by runShutdown BEFORE close(closed); read by callers AFTER <-closed
    // ...
}

func (c *Client) Close(ctx context.Context) error {
    c.closeOnce.Do(func() {
        go c.runShutdown()  // exactly one shutdown sequence ever runs
    })

    // Fast path: if shutdown already completed, return its result immediately,
    // even if ctx is already cancelled. Completed shutdown always wins over ctx.Err().
    select {
    case <-c.closed:
        return c.closeErr  // safe: close(closed) provides the happens-before edge
    default:
    }

    // Otherwise wait for whichever happens first. If the runtime picks the
    // ctx.Done() branch in a race where both are ready, re-check c.closed
    // before returning ctx.Err() — completed shutdown still wins.
    select {
    case <-c.closed:
        return c.closeErr
    case <-ctx.Done():
        select {
        case <-c.closed:
            return c.closeErr
        default:
            return ctx.Err()
        }
    }
}
```

What `runShutdown()` does:
1. Cancel internal context (cancels all subscriptions, recovery actors, reconnect goroutine).
2. Wait for all internal goroutines to exit via a `sync.WaitGroup` —
   independent of any CALLER's ctx, but bounded by the shutdown WORK
   budget (`WithShutdownTimeout`, see "Shutdown work budget" below;
   this sentence originally said "no external deadline", which
   described caller-independence but contradicted the budget section —
   the budget is the authority).
3. Close AMQP channels and connection.
4. Purge caches.
5. Write any cleanup error into `closeErr` (plain field write).
6. `close(c.closed)` — this acts as the synchronization barrier: every `Close(ctx)` caller that observes `<-c.closed` is guaranteed to see the final value of `closeErr` per Go's memory model. No atomic or mutex needed because `closeErr` is written exactly once before the channel close, and read only after.

Properties:
- Exactly one shutdown sequence ever runs (idempotent in the strong sense).
- A `Close(ctx)` caller with a tight deadline returns `ctx.Err()` — but the shutdown is still progressing in the background.
- A subsequent `Close(ctx)` with a live or longer-deadline context waits on the same `closed` channel and returns the real terminal result, not a fake `nil`. If shutdown has already completed by the time `Close(ctx)` is invoked, even an already-cancelled context returns the recorded terminal result — the fast-path check at the top of `Close(ctx)` makes "completed shutdown always wins" the rule. If shutdown is still in progress and `ctx` is already cancelled, the call returns `ctx.Err()` immediately (no point waiting on a deadline that's already past).
- Once `closed` is closed, further `Close(ctx)` calls return immediately with the recorded `closeErr` (commonly `nil`).
- `Subscription.Close(ctx)` follows the same pattern: shutdown starts on first call, every call waits on its own ctx for completion.

#### Shutdown work budget

`runShutdown` runs in its own goroutine spawned by `Close`; the caller's `ctx` (passed to `Close`) bounds the caller's *wait*, not the shutdown work itself. The work is rooted on `context.Background()` because the goroutine outlives any caller — but the shutdown still needs an upper bound so a stuck broker can't keep the Client alive forever. That bound is **shared across session-close and broker-close** so the cap applies to total work (not per-step), and it is configurable via `WithShutdownTimeout(d)` (default 5s). The same value also applies to:

- The two partial-init rollback paths inside `Connect` (recovery-open or alive-session-open failure rolls back the AMQP connection, capped at the same budget).
- `Subscription.runShutdown` — session-close + pump-drain wait share the same ceiling.

This consolidates what was previously three independent 5-second `time.Second` literals into one shared, user-overridable knob.

### Subscriptions

`client.Subscribe(ctx, opts...)`:
- Creates an AMQP queue + consumer, spawns a goroutine that fans out messages to the subscription's channel.
- The caller's `ctx` bounds SETUP only (lazy-connect dial, channel/queue topology). The subscription's own lifetime context derives from the client's internal context with the caller's cancellation severed (`context.WithoutCancel`) — cancelling the Subscribe ctx after Subscribe returns does NOT terminate the subscription. Deliberate: a caller wrapping Subscribe in `context.WithTimeout` for setup fail-fast must not have that timeout silently kill the live subscription moments later. Termination is explicit: `sub.Close(ctx)`, `client.Close(ctx)`, or a terminal error.
- All channel sends use `select` with the derived lifetime ctx.
- Subscription exposes `Sub.Done() <-chan struct{}` and `Sub.Err() error` modeled on `context.Context`. `Done()` closes when the subscription terminates for any reason (graceful close, client.Close-abort, terminal error); `Err()` returns the cause (`nil` for graceful close, non-nil for error termination). Composes well with consumers' own select loops.

**Two distinct termination paths — different semantics, on purpose:**

| Path | API | Behavior |
|---|---|---|
| **Graceful** | `Subscription.Close(ctx)` | Stops accepting new deliveries; waits for the in-flight delivery to complete its decode + admit-to-buffer + ack cycle; drains the in-process buffered channel until consumers have read all admitted messages or the supplied `ctx` deadline expires; then closes `Messages()` channel and `Done()`. `Err()` returns nil. Use this when you want a clean shutdown with no in-flight loss. The provided `ctx` is the drain deadline — if it expires before drain completes, remaining buffered messages are discarded and `Err()` returns `ctx.Err()`. |
| **Abrupt** | `client.Close(ctx)` called OR terminal error (the Subscribe `ctx` does NOT terminate a live subscription — it bounds setup only) | Subscription terminates immediately. The currently-in-flight delivery (if any) is `Nack(requeue=false)` rather than acked — see §AMQP backpressure failure handling below. Buffered messages already admitted to `Messages()` channel remain readable until the consumer stops reading or the channel is closed. `Err()` returns the abort cause or terminal error. Use this for emergency shutdown. |

**Note:** abrupt termination may drop the single message currently being processed (between AMQP delivery and channel admission). Buffered messages already in the `Messages()` channel are still visible to readers until the channel closes. Consumers that need zero-loss shutdown should call `Subscription.Close(ctx)` with a generous deadline before calling `client.Close(ctx)`.

**`MIGRATION.md` MUST call this out plainly:** `client.Close(ctx)` is **abrupt** for active subscriptions — it cancels the internal context, which terminates every subscription via the abrupt path (Nack on the in-flight delivery, no drain). Consumers that want graceful drain MUST call `sub.Close(ctx)` on each subscription **before** calling `client.Close(ctx)`. The recommended shutdown idiom:

```go
// Graceful shutdown: drain subscriptions first, then close the client.
// Fresh bounded ctx PER lifecycle call (doc.go guidance) — a drain that
// consumes its budget must not start client shutdown with an expired
// deadline.
drainCtx, cancelDrain := context.WithTimeout(context.Background(), 30*time.Second)
_ = sub.Close(drainCtx)
cancelDrain()
closeCtx, cancelClose := context.WithTimeout(context.Background(), 10*time.Second)
_ = client.Close(closeCtx)
cancelClose()
```

This is the single most common migration footgun for consumers used to the old `feed.Close()` shape — the migration guide must show this idiom prominently.

### AMQP backpressure & ack policy

**Manual ack (internal).** The SDK calls `channel.Consume(..., noAck=false, ...)` and acks each delivery itself, **after** its decoded message has been admitted to the subscription's configured public buffer (`WithSubscriptionBuffer`) — the ack closure travels with the message through the pipeline (consumer → session → subscription pump) and fires at the pump's successful `Messages()`-buffer send, or at the session when it terminally consumes/drops the delivery itself (alive handling, out-of-scope producer filtering). Nothing between broker and public buffer is ever acked early: the intermediate hops are UNBUFFERED, and a delivery abandoned mid-pipeline by an abrupt shutdown simply stays unacked (the broker releases it on channel close). Users never see an `Ack`/`Nack` API — manual ack is implementation detail.

Why this matters: AMQP's `prefetch` setting bounds **unacked** deliveries. With the current SDK's auto-ack (`noAck=true`), the broker considers every delivery acked the instant it leaves the broker, so `prefetch` is meaningless and there is no broker-visible backpressure. The new SDK's internal manual-ack makes `prefetch` actually function — unacked-from-broker count is bounded by it, and a stalled consumer applies true broker-level backpressure.

**Message flow with the corrected ack model:**

1. RabbitMQ delivers up to `prefetch` (default 1000) **unacked** messages to the SDK.
2. SDK consumer goroutine reads from the AMQP delivery channel, decodes the message, and pairs it with an ack closure for its delivery.
3. The envelope flows through the unbuffered consumer→session and session→pump hops; the session builds the public message.
4. The subscription pump sends the built message into the subscription's configured public buffer (`Messages()`); once that send succeeds, the ack closure fires `delivery.Ack(false)`. The slot becomes available for the next prefetch. (Messages the SDK terminally consumes itself — alive/snapshot bookkeeping, disabled/out-of-scope producers — are acked at that drop point instead.)
5. If the subscription buffer is full (consumer stalled), step 4 blocks and the pump emits the slow-consumer warning once per detection window. The delivery — and everything queued behind it — is not acked. After `prefetch` deliveries pile up unacked, the broker stops sending more to this consumer.

This means a slow or stalled consumer applies backpressure all the way to the broker, bounded by `prefetch`. No in-SDK dropping. If the consumer remains stalled long-term, the broker side may queue messages or close the connection per its own configuration — but the SDK has done its part.

**Failure handling:**

- **Decode failure** → the delivery is admitted as an `Unparsable` message. Under `ExceptionStrategy` `Catch` it reaches the consumer like any message and is acked on public-buffer admission (we'd never decode it correctly on retry). Under `Throw` the subscription terminates and the delivery stays unacked — the session never handled it.
- **Graceful subscription close** (`Subscription.Close(ctx)`) → finish the in-flight decode + admit cycle for the current delivery; stop accepting new deliveries; drain the pipeline and the in-process buffer to consumers (up to the supplied `ctx` deadline) — acks fire as each drained message reaches the public buffer. No `Nack`. See §Subscriptions for full graceful-close semantics.
- **Abrupt cancellation** (caller `ctx` cancelled, `client.Close(ctx)`, terminal error) → the delivery in the consumer's hand is `Nack(requeue=false)`; deliveries parked deeper in the pipeline are simply never acked, and the broker releases them on channel close. We don't requeue, because the SDK's own recovery mechanism (snapshot/event recovery) is the authoritative gap-filler; double-delivery from broker requeue would be noise. No message is ever acked-then-lost — anything undelivered was unacked; already-buffered messages on `Messages()` remain readable (they were acked on admission), and recovery covers any gap.
- **Connection drops** → all unacked deliveries are released by the broker on connection close. After reconnect + queue rebind, the broker may re-deliver. SDK relies on its recovery mechanism to reconcile; consumer-visible message-id dedup is not a goal.

**Configurable knobs:**

- `WithAMQPPrefetch(n int)` — broker-side prefetch. Default 1000, accepted range 0..65535 (AMQP carries the count as uint16).
- `WithSubscriptionBuffer(n int)` — internal channel buffer. Default 256, max 2^20.
- The "slow consumer" situation emits a `slog.Warn` once per detection window if the buffer stays full for >5s — observability, not action.

**Known bound gap (open team decision, pre-beta):** backpressure is
COUNT-based, not byte-based, and the 8 MiB decode cap does not bound
PEAK allocation. Two facets:

  1. *Retention.* Feed payloads are individually capped at 8 MiB
     (oversized deliveries are rejected and retain only a 64 KiB
     diagnostic prefix), but a broker sending valid messages just UNDER
     the cap into a stalled consumer can pin `buffer × payload` bytes —
     ~2 GiB of raw payload at the defaults (halved again by the
     extended-data raw copy when `WithExtendedDataReporting` is on)
     before count-based backpressure engages. Parsed messages retain
     their full `RawMessage`, so truncating it unconditionally would
     break extended-data consumers — the mitigation is an aggregate
     queued-byte budget in admission, or a smaller default buffer for
     raw-retaining interests.
  2. *Peak allocation.* `Decode` checks `len(d.Body)` only AFTER
     amqp091-go has already reassembled the logical body, which the
     dependency preallocates from the untrusted AMQP content-header
     size. A hostile/misconfigured broker declaring a huge content
     length forces that allocation BEFORE the SDK's 8 MiB rejection
     runs. Bounding this requires a broker-side logical-message limit
     and/or an AMQP client that rejects oversized headers pre-allocation
     (patch or replace amqp091-go) — not fixable in the SDK's decode
     path alone.

Both require the authenticated Oddin broker (or a compromise of it) to
emit pathological traffic. Consumers can lower `WithSubscriptionBuffer`
today to cap facet 1. Decide the byte-budget / dependency-patch work
alongside the prefetch bake (§19).

This makes the message-path contract explicit and contrasts cleanly with the event-channel policy (§0.3): events are metadata and may drop; messages are payload and must not.

A future `WithManualAck()` option exposing ack semantics to users is **not in scope for v1.0.0** — see open question §19.9.

### SDK self-identification (version reporting)

So the server side can track which SDK versions are in active use — and proactively support or retire them — the SDK identifies itself on every server contact.

**Version resolution is runtime-first.** `internal/version.Version()` (re-exported as the public `gosdk.Version()`) reads the actual module version the consumer resolved via `go get` from the binary's build info (`runtime/debug.ReadBuildInfo`) — so a consumer on `v1.0.3` reports `1.0.3` with no constant to bump, and the "forgot to bump → stale telemetry" failure mode is designed out. The compiled-in `const version.SDK` is only the **fallback** for unstamped builds (local `go run`, a `replace` directive, `go test`); when it is used the reported value is suffixed `-dev` (e.g. `1.0.0-dev`) so an unstamped build is unmistakable in telemetry. A CI guard on `v*` tags fails the release unless the tag equals `v`+`SDK` and `SDK` is valid semver, keeping the fallback baseline in lockstep with the release line.

- **HTTP (every API request).** Set at the one request choke point (`makeRequest`), so all endpoints carry it — no per-endpoint work:
  - `User-Agent: oddin-gosdk/<version> (go<toolchain>)` — the idiomatic carrier, parseable straight from access / reverse-proxy logs with no server change. The Go toolchain suffix (`runtime.Version()`) tells support what built a given agent.
  - `X-Oddin-SDK-Version: <version>` — the structured field, for server-side filtering/aggregation without parsing the UA string.
- **AMQP (per connection).** The broker `Properties` table sends `SDK` (language, unchanged for backward compatibility) plus `SDK_version`, so the version is visible per live connection in the RabbitMQ management UI and broker logs — the cleanest "who is connected right now, on what version" view, complementing the HTTP-log history.

The HTTP headers apply to all endpoints on purpose: active consumers hit the API continuously, so version telemetry is a genuine liveness signal rather than a once-at-startup beacon. No config knob — self-identification is always on and carries no user data (just the SDK version and Go toolchain).

> **Release identity.** The module path stays `github.com/oddin-gg/gosdk` (no `/v2`), so the first stable release is tagged **`v1.0.0`** — Go requires a `/v2` path suffix for `v2+` tags when a `go.mod` is present, which the "no `/v2`" decision below rules out. "v2" remains the internal name for the rewrite; the released semver line is `v1.x`.

### Reconnect

Single goroutine per client. Receives `*amqp.Error` from `NotifyClose`. On non-nil error:
1. Marks all subscriptions as `Reconnecting` (sends `ConnectionEvent` to `client.ConnectionEvents()`).
2. Backoff via `cenkalti/backoff/v5` with exponential delay, capped (e.g., 30s).
3. Reopens connection, re-declares queues, re-binds, resumes delivery to existing subscription channels.
4. Sends `ConnectionEvent{Kind: Connected}`.

No recursion. No goroutine pyramid. Backs off forever (or until `client.Close`).

### Event channel backpressure policy

All three event channels (`ConnectionEvents`, `RecoveryEvents`, `APIEvents`) are **buffered + lossy** with a documented drop policy:

| Channel | Default buffer | Drop policy | Polling alternative |
|---|---|---|---|
| `ConnectionEvents` | 32 | Drop oldest on overflow + `slog.Warn` once per drop burst | `client.ConnectionState() ConnectionState` |
| `RecoveryEvents` | 1024 | Drop oldest on overflow + `slog.Warn` once per drop burst | `client.ProducerStatus(id) (ProducerStatus, bool)` |
| `APIEvents` | 256 | Drop oldest on overflow + `slog.Warn` once per drop burst | (none — by design, debug-only) |

Rationale: a slow or dead consumer must NEVER block the SDK's internal goroutines. Recovery actors send to `RecoveryEvents` from inside their state-machine loop; blocking them would deadlock the entire recovery pipeline. The buffer is sized so a momentary consumer stall doesn't drop important events; the polling getters give a safety net for consumers that miss events. Document expectation: subscribe-and-drain promptly.

Tests verify: events fire on state changes, drop policy logs once per burst (not per event), polling getter reflects state even when channel was full.

## 9. Logging

### General logging

`log/slog` everywhere internally. Public option to inject:

```go
cfg := gosdk.NewConfig(token, env, gosdk.WithLogger(myLogger))
```

Default (when `WithLogger` not used): `slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))`.

All log call sites use structured attributes — `slog.String("producer", name)`, `slog.Int("request_id", id)`, etc. Drop free-form `Errorf`-style messages.

### API call logging (parity with .NET — explicit client request)

The .NET SDK team received explicit asks for first-class API-call observability — clients want to inspect every REST call the SDK makes, for debugging, audit, latency tracing, and reproducing issues. The Go rewrite ships this as a first-class feature, opt-in.

Three independent layers:

1. **Structured slog at debug level** — a response that returns from the HTTP layer emits a `slog.Debug("api: response", …)` with `method`, `path`, `status`, `latency_ms`, `attempt` (retries and unreadable/failed bodies emit their own Debug lines). Free, automatic, hidden behind log level. Useful for ops/troubleshooting. *(The originally-planned `url`/`request_id`/`bytes_in`/`bytes_out` fields were not added; the `APIEvent` channel below carries URL, bytes, and error detail.)*

2. **`APIEvent` channel** — when `WithAPICallLogging(...)` is set, each API call also emits an event to `client.APIEvents()`:
   ```go
   type APIEvent struct {
       At               time.Time
       Method           string
       URL              string            // scheme://host/path — the ENTIRE query string is stripped before emission
       Status           int               // 0 on transport-level failures
       Latency          time.Duration
       Attempt          int               // for retried calls
       Locale           *types.Locale     // when applicable
       Request          []byte            // populated only when level == APILogFull; redacted + capped
       RequestTruncated bool              // request-side counterpart of Truncated
       Response         []byte            // populated when level >= APILogResponses; redacted + capped
       Truncated        bool              // true if the RESPONSE was truncated at WithAPICallBodyLimit
       Err              error             // set for transport, decode, envelope AND HTTP errors (typed gosdk.APIError — errors.As for Status/Code)
   }
   ```

3. **Verbosity levels** — `WithAPICallLogging(level APILogLevel)`:
   - `APILogOff` (default) — slog-debug only, no events emitted.
   - `APILogMetadata` — events emitted with method/url/status/latency, no bodies.
   - `APILogResponses` — events include response body bytes (typical debug setting).
   - `APILogFull` — events include both request and response bytes (heavy; intended for short-lived diagnosis).

4. **Sensitive-data redaction and size cap.** Body capture is **always sanitized**, regardless of level. Order of operations:
   1. **Capture** up to `WithAPICallBodyLimit(bytes)` (default 64 KiB) into a separate event buffer. The cap applies to the **copied event buffer only** — the response body that the decoder consumes is untouched.
   2. **Redact** the captured prefix: every occurrence of the configured access token (whatever its length) is replaced with `[REDACTED]`. The shipped implementation redacts the ACCESS TOKEN ONLY — there is no registry of additional redaction substrings (the originally-planned "registered substrings" feature was not built; add your own scrubbing downstream if your deployment embeds other secrets in bodies).
   3. **Mark truncation**: `Truncated` (response) / `RequestTruncated` (request) report cap overflow — including the internal 1 MiB error-body cap.
   - **Important:** redaction is applied to whatever was captured, even when truncated — a token inside the captured prefix is always scrubbed.
   - **Headers are never captured.** APIEvent carries no header metadata at all, so there is nothing to redact there (the originally-planned header-redaction list is moot). The access-token header never leaves the transport layer.
   - **URLs.** `APIEvent.URL` is reduced to `scheme://host/path` — the WHOLE query string is stripped before emission (strictly stronger than the originally-planned selective `token=`/`access_token=`/`key=` parameter redaction). `APIEvent.Err` messages get the same treatment (query stripped, token scrubbed, sanitized cause chain).

Implementation: capture lives inside the API client's `do()` path, not a `http.RoundTripper` middleware. On a 2xx the response body is wrapped in an `io.TeeReader` feeding a bounded `capBuf` (a small custom `io.Writer` that accumulates up to `WithAPICallBodyLimit` and then discards, flagging truncation) — the decoder reads THROUGH the tee, so it receives the full unmodified bytes while the buffer fills as a side-effect of the parse. The buffer is redacted before being attached to the `APIEvent`. This guarantees the decoder is never short-fed and large responses are not corrupted. The events channel is buffered (default 256); when full, oldest events are dropped and a single `slog.Warn` is emitted — never blocks the API call.

This closes the gap vs Java's `OddsFeedExtListener.onRawApiDataReceived` and the .NET request that motivated this section. It also subsumes the current `WithExtendedDataReporting` flag for the API path; that flag stays for raw *feed* messages (different concern, different channel).

Tests cover: events emitted on success, events emitted on retry, events dropped when channel full, body capture is faithful to wire bytes, no events emitted when disabled.

## 10. Error handling

### Sentinel errors

```go
// Shipped (errors.go):
var (
    ErrAlreadyClosed  = errors.New("gosdk: client closed")
    ErrInvalidConfig  = errors.New("gosdk: invalid configuration")
    ErrManagerNotOpen = errors.New("gosdk: recovery manager not open (call Connect first)")
)
```

`ErrManagerNotOpen` is surfaced by the Recover* methods invoked before
the feed pipeline is up (internal lifecycle errors are translated to the
public sentinel at the Client boundary; a manager racing into Closed
surfaces `ErrAlreadyClosed` the same way).

Callers can `errors.Is(err, gosdk.ErrAlreadyClosed)` etc. The original
plan also listed `ErrConnectionLost`, `ErrLocaleNotAvailable`,
`ErrProducerNotFound`, `ErrEventNotFound`, and `ErrRecoveryInProgress`;
those conditions are currently reported as wrapped internal errors (or,
for locale misses, as `Optional.None` — the v2.x reshape made "locale
not loaded" a value, not an error). Export them only when a consumer
demonstrates a need to branch on one programmatically.

### Wrapping

Every internal `fmt.Errorf` uses `%w`. No `%v` for errors, ever.

### Exception strategy (parity with .NET/Java)

```go
gosdk.WithExceptionStrategy(gosdk.StrategyCatch)  // logs and emits Unparsable (default)
gosdk.WithExceptionStrategy(gosdk.StrategyThrow)  // terminates the subscription via Sub.Err()
```

**Scope (explicit):** `ExceptionHandlingStrategy` affects exactly one pipeline — the AMQP message consumer's decode-and-route step.

| Where strategy applies | Behavior under `Catch` (default) | Behavior under `Throw` |
|---|---|---|
| AMQP message decode/route failure | Log + emit `Unparsable` into the subscription; subscription stays alive | Terminate the subscription; `Sub.Err()` returns the underlying error |

**Where strategy explicitly does NOT apply:**

- **`Client.X(ctx, ...)` methods.** All public API methods always return `(T, error)`. Strategy does not change return semantics — that's how Go signals errors. There is no "swallow the error" mode in Go without lying to the caller, so a `Catch` setting cannot meaningfully toggle API-call behavior.
- **Background goroutines (recovery actors, reconnect loop).** These always log-and-continue regardless of strategy. A transient API failure in the recovery state machine must not permanently down a producer; a network blip during reconnect must not abort the SDK. These goroutines have no caller to propagate to.

This scope is narrower than .NET/Java's, but it's the only coherent interpretation in Go. `MIGRATION.md` should call this out for users coming from those SDKs.

## 11. Recovery state machine

Single goroutine per producer. Owns all state for that producer. Communicates via channels.

```go
// As built (internal/recovery). Handle REGISTRY lives on the Manager,
// not the actor; the actor tracks only its own in-flight event recoveries.
type recoveryActor struct {
    producerID      int
    inbox           chan actorEvent          // alive, snapshotComplete, processingStarted/Ended, recoverEvent(+Completed), tick, ...
    eventRecoveries map[int]*eventRecovery   // requestID → in-flight per-event recovery
    lastSystemAlive *time.Time
    downReason      types.ProducerDownReason
    ctx             context.Context          // actor lifetime; cancelled on manager cleanup (aborts detached API calls)
    api             *api.Client
    pm              *producer.Manager
    mgr             actorManagerOps          // registerHandle / completeHandle, etc.
    logger          *log.Logger
}
```

### Staying quiet for API-only clients

There is no per-actor "dormant/armed" lifecycle and no `arm` event. Quietness
is achieved structurally instead:

- **The recovery Manager is OPENED only on the connect path**
  (`client.Connect` / the first live `client.Subscribe`), never by
  `gosdk.New`. `New` constructs an UNOPENED placeholder Manager so the
  atomic handle is never nil, but it owns no goroutines and runs no work
  until opened. An API-only client (`gosdk.New` + `client.Sports(ctx)`
  etc.) that never connects leaves that Manager unopened, so no tick loop runs and no
  producer-down events are emitted.
- **Actors exist only while the Manager is open.** `Open` pre-spawns one
  actor per producer in the active-producer catalog (the bootstrap fetch it
  already performs); producers that appear later are spawned lazily by
  `findOrSpawn` on their first recovery request or feed event. Nothing is
  spawned before `Open`, and `gosdk.New` never creates the Manager at all.
- **Producer-down is warm-up-gated after connect.** The tick loop starts
  immediately (so `MaxRecoveryExecution` expiry is enforced promptly) but
  carries an `inactivityArmed` flag that stays false until `initialDelay`
  elapses; the per-tick inactivity/producer-down check is suppressed until
  then, so a freshly-connected producer isn't flagged down before its first
  alive can arrive.

### Reliable per-request completion

When a caller invokes `client.RecoverEventOdds(ctx, p, e)`, the actor
(early-reply restructure — the API call must not block the actor goroutine):
1. Registers the handle (creates it keyed by `requestID`, status `Pending`)
   and records the in-flight recovery.
2. Returns the handle to the caller **synchronously**, before the API call
   runs — so the caller always gets a live handle even if it cancels its
   request `ctx` immediately after (the common `defer cancel()` pattern).
3. Issues the API recovery request from a **detached** goroutine whose ctx
   is rooted at the actor lifetime, NOT the caller's ctx. The caller's ctx
   bounds only admission (the send into the actor inbox and the wait for
   this reply); once the handle is accepted, a caller-side cancel can no
   longer abort the recovery. The API outcome feeds back to the actor as a
   follow-up event so state mutation stays single-threaded.

The `SnapshotComplete` message is a **correctness event, not lossy observability**: the session admits it to the producer's recovery actor with ctx-bounded backpressure (a full actor inbox blocks rather than dropping) and acks the underlying AMQP delivery ONLY after admission succeeds. If admission can't complete (the recovery manager is shutting down), the delivery is left unacked so the broker redelivers — a dropped-then-acked completion would strand recovery until `MaxRecoveryExecution`.

When the corresponding `SnapshotComplete` arrives (or recovery times out / fails), the actor updates the handle's status (`Completed` / `Failed` / `TimedOut`), closes its `Done` channel, and emits a `RecoveryEvent` on the (lossy) channel. **Even if the channel event is dropped, the handle is reliable** — the caller can `<-handle.Done()`, `handle.Result()`, or `handle.Status()` and get the correct outcome. Handles are GC'd from the manager's map a grace period after completion (constant `recovery.HandleGCGracePeriod`, currently 5 minutes), after which `client.EventRecoveryStatus(requestID)` returns `(_, false)`. Five minutes is generous enough for late pollers without growing the map indefinitely; a consumer that polls only at very long intervals should cache the request ID + outcome itself rather than relying on the SDK map.

Tests drive the actor through full recovery scenarios against an httptest-backed API (real timers with shortened windows — no fake clock): warm-up gating (`inactivityArmed` suppresses producer-down until `initialDelay` elapses), cold start → initial snapshot, producer-down by inactivity → recovery → producer-up, interrupted re-issue, event recovery, handle completion with no channel reader, late/stale API completions, and shutdown-vs-admission races.

## 12. Configuration

### Type

```go
type Config struct {
    // unexported; modified only via Option functions
}

type Option func(*Config)
```

Options listed in §4.

### Validation

`gosdk.NewConfig` returns a `Config`; validation happens in `gosdk.New(ctx, cfg)`. Missing/invalid config surfaces as `ErrInvalidConfig` (wrapped with detail) UP FRONT, not as a runtime surprise later. New validates:

- **Required:** access token present; API and MQ endpoints resolvable from the environment/region (or the forced hosts).
- **Bounded options:** durations (`WithMaxInactivity`, `WithMaxRecoveryExecution`, `WithHTTPClientTimeout`, `WithShutdownTimeout`) must be > 0; `WithInitialSnapshotTime` ≥ 0; `WithMessagingPort` in 1..65535; `WithAMQPPrefetch` / `WithSubscriptionBuffer` / `WithAPICallBodyLimit` ≥ 0 (0 = default/none); `WithExceptionStrategy` / `WithAPICallLogging` a known enum; default locale and exchange/prefix names non-empty.
- **Forced hosts:** `WithAPIHost` / `WithMQHost` must be a bare host (no scheme — already rejected at option time — no userinfo/path/query/fragment/whitespace, well-formed IPv6 brackets). The API host may carry an explicit numeric `:port`; the **MQ host must NOT**, because the dialer appends `WithMessagingPort` (a `host:port` MQ value would build `host:port:port`).

### Environment helpers

**NOT SHIPPED — plan superseded.** The `Select*` helper functions were
designed when `Environment` was expected to bundle host+region. The
shipped model keeps them orthogonal: pick the environment constant
(`types.IntegrationEnvironment`, `types.ProductionEnvironment`, …) as
the `NewConfig` argument, the region via `WithRegion(...)`, and custom
hosts via `WithAPIHost`/`WithMQHost`/`WithMessagingPort`. That covers
every `Select*` use case (including replay and fully-custom endpoints)
without a second construction vocabulary. Revisit only if a consumer
asks for one-call environment selection. Region typo (`DefaulRegion` →
`RegionDefault`) fixed; no deprecated alias survives at v1.0.0 (§0.6).

## 13. Migration path

Both internal consumers (`kollector-esport`, `ots-odds-bridge`) need ~30 lines of bootstrap-code changes. Migration guide structure:

### Bootstrap diff (kollector-esport, illustrative)

```diff
-cfg := gosdk.NewConfiguration(token.Token.String(), feedEnv, rand.IntN(1000), false)
-cfg = cfg.SetAPIURL(c.apiURL).SetMQURL(c.mqURL).SetMessagingPort(c.mqPort)
-c.feed = gosdk.NewOddsFeed(cfg)
-sb, err := c.feed.SessionBuilder()
-sCh, err := sb.SetMessageInterest(types.AllMessageInterest).Build()
-fCh, err := c.feed.Open()
+cfg := gosdk.NewConfig(token.Token.String(), feedEnv,
+    gosdk.WithNodeID(rand.IntN(1000)),
+    gosdk.WithAPIHost(c.apiURL),
+    gosdk.WithMQHost(c.mqURL),
+    gosdk.WithMessagingPort(c.mqPort),
+)
+c.client, err = gosdk.New(ctx, cfg)
+sub, err := c.client.Subscribe(ctx, gosdk.WithMessageInterest(types.AllMessageInterest))
```

Then the consumption loop changes from a 3-channel select (session/feed/close) to:

```go
for {
    select {
    case msg, ok := <-sub.Messages():
        if !ok { return }
        // handle message
    case ev := <-c.client.RecoveryEvents():
        // handle recovery event
    case ev := <-c.client.ConnectionEvents():  // NEW
        // handle connection state change
    case <-ctx.Done():
        return
    }
}
```

Manager calls update mechanically:
```diff
-prods, _ := c.feed.ProducerManager()
-avail, _ := prods.AvailableProducers()
+avail, _ := c.client.Producers(ctx)
```

```diff
-rm, _ := c.feed.RecoveryManager()
-id, _ := rm.InitiateEventOddsMessagesRecovery(producerID, urn)
+handle, _ := c.client.RecoverEventOdds(ctx, producerID, urn)
+id := handle.RequestID()  // if you only kept the request ID before; otherwise use handle.Done()/Result()/Status()
```

### Migration guide deliverable

A `MIGRATION.md` in the repo root with a side-by-side table of every old API → new API. Auto-checkable: a one-page script that greps the consumer repo for old method names and reports.

### Source compatibility

*(Original goal; superseded by the shipped v1.0.0 reshape — see §0 and
MIGRATION.md.)* `types/*` identifiers largely survive, but the value
structs, `Optional[T]` accessors, and removed manager interfaces are a
deliberate breaking change; consumers migrate via MIGRATION.md rather
than recompiling unchanged.

### Migration is not just bootstrap — recovery loops change

The bootstrap diff above is the easy part. Both consumers' message-consumption loops require deeper review:

**kollector-esport** (`services/mq/feed/client.go` in the kollector-esport repository — sibling repo, not linkable from here) currently:
- Multiplexes session messages, global recovery messages, and a close signal in one select.
- Per-producer `outOfOrderMessages` buffer (`services/mq/feed/producer.go:37-38` in that repo) holding messages received before recovery completes.
- Drives recovery via `RequestID()` arrival semantics that depend on the current (broken) recovery state machine timing.

The new event channels separate concerns differently — `RecoveryEvents` is independent of `Subscription.Messages()`. The out-of-order buffering pattern still works, but the producer-up/down notifications arrive on a separate channel with different timing. **Plan a real migration pass with the kollector team, not a mechanical rename.** Estimate: ~1–2 days of consumer-side work plus a 24h+ staging bake-in.

**ots-odds-bridge** (`connector/sdkclient/client.go` in the ots-odds-bridge repository — sibling repo, not linkable from here) is closer to the example shape and migrates more straightforwardly. It also currently doesn't call `feed.Close()` — the rewrite needs that path wired in (and the migration is a chance to add a real `defer client.Close(ctx)` plus `panic(err)` replacements at lines 511/517 with proper error handling).

### Beta tagging cadence

Pre-Phase-6 tags are **internal alphas** (`v1.0.0-alpha.N`), not consumer-facing — the public `Client` doesn't exist yet, so consumers can't actually integrate. They're useful internally for pinning a stable reference instead of branch pseudo-versions while Phases 1–5 land.

The first **consumer-facing beta** is `v1.0.0-beta.1`, cut at the end of **Phase 6** (when the public `Client` API is implemented and integration-tested). That's when `kollector-esport` and `ots-odds-bridge` can start their migration on a real tag. Subsequent `v1.0.0-beta.N` cut as bugs surface during Phase 7b. Stable `v1.0.0` after Phase 7b staging bake-in (24h minimum, 72h preferred).

## 14. Testing strategy

### Required coverage gates

- `types/`: 80%+ — pure data types, table-driven tests for URN parsing, locale handling, Optional[T] semantics.
- `internal/api/xml/`, `internal/feed/xml/`: 90%+ — golden-file decode tests for every HTTP response and AMQP envelope (the original plan was a single `internal/xml/` — split per-layer during Phase 1 once it became clear the two wire formats had nothing in common).
- `internal/api/`: 80%+ — every endpoint tested against `httptest.Server` with happy path, 4xx, 5xx, network error, and retry scenarios.
- `internal/cache/` (+ `internal/cache/lru/`): 90%+ — concurrency stress test (`go test -race`), TTL expiry, LRU eviction, single-flight dedup, cache invalidation, multi-locale fill-in, scoreboard-aliasing regression.
- `internal/recovery/`: 90%+ — exhaustive state-machine tests driven by fake tickers/handshakes; covers all transitions documented in §11.
- `internal/feed/`: 80%+ — dial-loop, channel-consumer, and process-delivery tests with an in-process AMQP fake; goroutine-leak detection via structural done-channel joins.
- `internal/factory/`: 80%+ — message and market builders against captured XML samples (covers `feedMessage`, `markets`, `unparsable`, `request_id`).
- `internal/producer/`, `internal/replay/`, `internal/sport/`, `internal/whoami/`: per-manager unit tests with mocked api/cache deps.
- `gosdk` (top-level): 70%+ — end-to-end flows: open → subscribe → close, multiple subscriptions, ctx-cancellation, reconnect.

### Coverage status (2026-07)

The gates above are the TARGET; current actuals are below them in most
packages (cache/feed/factory/recovery furthest). CI publishes a coverage
report on PRs but does not yet FAIL on the thresholds — enforcing the
gate is open work tracked alongside the remaining test build-out.

### Tools

What shipped diverges from the original plan — the test suite settled on
the standard library only:

- Plain `testing` with hand-rolled asserts (no `testify` — the
  dependency was never added; keep it that way for consistency).
- Goroutine-leak checks are done structurally per-test (waiting on
  done-channels) rather than via `go.uber.org/goleak`.
- Deterministic-time tests use fake tickers/gates instead of
  `testing/synctest` so far; `synctest` remains available (Go 1.26)
  where a future test genuinely needs virtual time.
- `httptest.Server` for API client tests.
- An in-process AMQP fake. The original plan placed this under `internal/feed/testfake/`; in practice the test-only stubs live alongside their consumers (`internal/feed/dial_ctx_test.go`, `process_delivery_test.go`, `open_concurrent_test.go`) since the surface needed per-test was small enough that a separate package added more import friction than it removed. Mechanics unchanged: `amqp091-go` exposes concrete `*amqp.Connection` / `*amqp.Channel` types, so `internal/feed/` defines local minimal interfaces covering only the methods the SDK calls (`Channel()`, `NotifyClose()`, `Close()`, `QueueDeclare()`, `QueueBind()`, `Consume()`); production wraps the real types via a thin adapter, tests provide their own implementations.

### Smoke test (planned — NOT yet implemented)

The plan: a `//go:build smoke` integration test running against the real
test environment with a token from the `TOKEN` env var — 30s of the new
client, verifying it connected, received messages, and shut down
cleanly; used pre-release to validate tags. Status: no tracked file
carries the `smoke` (or `integration`) build tag yet, so the Makefile's
`test-smoke` / `test-integration` targets currently just rerun the
ordinary suite under an unused tag. Writing the real smoke test remains
open pre-release work (the manual example runs against the test env
cover it operationally for now).

### CI

GitHub Actions on every PR to `next`:
- `go vet ./...`
- `go test -race ./...`
- `staticcheck ./...`
- `govulncheck ./...`
- Coverage REPORT published on PRs (fgrosse/go-coverage-report). The
  per-package minimums above are TARGETS — the gate does not fail the
  build yet; see "Coverage status" above.

## 15. Demo / examples

Replace the monolithic `example/main.go` with focused, runnable examples under `examples/`:

```
examples/
  basic/         # minimal: connect, subscribe, print odds changes, shutdown on signal
  recovery/     # explicit event recovery, connection-event handling
  multi_locale/ # fetch the same match in 3 locales
  replay/       # use the Replay API
  graceful/     # context-driven shutdown with deadline
  README.md     # index pointing at each
```

Each example is a self-contained `main.go` ≤ 200 lines. The `README.md` describes each. The current `example/main.go` is removed (the new `basic` is its straight-line replacement).

## 16. Dependencies (target go.mod)

```
module github.com/oddin-gg/gosdk

go 1.26.0

require (
    github.com/cenkalti/backoff/v5 v5.x
    github.com/google/uuid v1.6.0
    github.com/hashicorp/golang-lru/v2 v2.x
    github.com/rabbitmq/amqp091-go v1.11.x
    golang.org/x/sync v0.x   // singleflight
)

// Test-only dependencies: NONE — the suite is stdlib-only (see §14
// Tools). testify/goleak from the original plan were never added.
// testing/synctest is stable as of 1.26 — no clockwork shim needed.
```

Tools (`staticcheck`, `govulncheck`) — **superseded:** instead of the
originally-planned sibling `tools/` module, they are managed with the
go.mod `tool` directive (Go 1.24+) in the MAIN module (see the
`tool (…)` block in go.mod). Tool dependencies are excluded from
compilation of the SDK's packages — consumers never BUILD them — but
note the isolation is weaker than the sibling-module plan promised:
they still participate in `go list -m all`, MVS version resolution,
SBOM generation, and vulnerability-graph output for consumers.
Accepted trade-off for the simpler single-module layout. The old
`tools.go` build-tag pattern is gone.

Removed: `sirupsen/logrus`, `patrickmn/go-cache`.

## 17. Phased rollout

Each phase is independently mergeable to `next` and CI-green. No phase merges without tests for the code it adds.

### Phase 0 — Foundation (2–3 days)

The branch must compile and pass CI at the end of every phase, including this one. We do **not** delete `logrus` and `go-cache` until their replacements are wired in (Phase 3 / 6).

- Move `staticcheck`, `govulncheck` into `tools/` submodule. Update Makefile.
- Set `go.mod` directive to `go 1.26.0` (consumers cleared to bump alongside the v2.x cutover).
- **Add** new dependencies (`golang-lru/v2`, `cenkalti/backoff/v5`, `golang.org/x/sync`, `stretchr/testify`, `uber-go/goleak`). Keep `logrus` and `go-cache` for now — they're removed in Phase 6 once nothing imports them.
- Set up CI pipeline with `-race`, coverage gate (initially soft).
- Add `goleak`, `testify` scaffolding.
- Bump `amqp091-go` to v1.11.x.

### Phase 1 — Pure types & decode (2–3 days)

- Stabilize `types/*` — extend `Locale` to 12 values, fix `RegionDefault` typo, add `MessageInterest` constants if any are missing.
- Rewrite XML decoders with proper struct tags, no `<envelope>` synthesis. Split per-layer into `internal/api/xml/` (HTTP responses) and `internal/feed/xml/` (AMQP envelopes) — see §5.
- Golden-file tests for every message type using captures from the test-env smoke log.
- URN, routing-key parsing in `internal/feed/` — table-driven tests.

### Phase 2 — HTTP API client (2 days)

- New `internal/api/Client`: ctx-aware, slog-logged, exponential backoff via `backoff/v5`, body-close on every retry, network-error retry, header canonicalization (`Set` not direct map access), no `count++` bug in replay-start params.
- `httptest.Server`-backed test for every endpoint.

### Phase 3 — Cache layer (3–4 days)

- Generic `EventCache[K, V]` over `golang-lru/v2` + `singleflight`.
- Per-catalog map+RWMutex caches (a shared `StaticCache[K, V]` was prototyped, never wired in, and later removed as dead code).
- Per-entity cache wrappers (Match, Competitor, Player, Tournament, Fixture, MarketDescription, MarketVoidReasons, Sport, MatchStatus).
- All field access in cached entries protected by per-entry mutex (no partial locking).
- Tests: concurrency stress (`-race`), TTL expiry, LRU eviction, single-flight dedup, locale fill-in, public Clear methods.

### Phase 4 — AMQP feed layer (3–4 days)

- New `internal/feed/Client`: single-goroutine reconnect loop, ctx-driven shutdown, backoff, atomic-pointer connection.
- `internal/feed/ChannelConsumer` (renamed from the original plan's `Consumer`): ctx-cancellable channel sends, no recursion, no synthesized `<envelope>`. Internal manual-ack: `noAck=false` on `Consume`, `delivery.Ack(false)` only after the decoded message is admitted to the subscription buffer.
- In-process AMQP fakes alongside their tests (`dial_ctx_test.go`, `process_delivery_test.go`, `open_concurrent_test.go`) — the planned `internal/feed/testfake/` package was inlined per §14 once the surface stayed small.
- Tests: routing-key parsing, reconnect backoff, ctx-cancellation, goroutine leak (`goleak`), and explicit backpressure tests that prove a stalled subscription stops the broker after `prefetch` unacked deliveries.
- End of phase: cut `v1.0.0-alpha.N` (internal-only — public `Client` not yet wired).

### Phase 5 — Recovery state machine (3 days)

- Per-producer actor goroutine pattern as described in §11.
- Mock clock; full state-transition coverage.
- Replaces all of `internal/recovery/` from scratch.

### Phase 6 — Public Client & cutover (4–5 days)

- New `gosdk.Client` wiring it all together.
- Lazy AMQP open via `Subscribe` and explicit `Connect` (mutex + state machine, NOT `sync.Once` — failed Connect must be retryable).
- `Subscribe`, `RecoveryEvents`, `ConnectionEvents`, `APIEvents`, polling getters (`ConnectionState`, `ProducerStatus`, `EventRecoveryStatus`), all manager-equivalent methods.
- New `Replay` subtype.
- Functional-options config.
- **Drop** `logrus` and `go-cache` from `go.mod` — at this point nothing imports them.
- Integration tests (open → subscribe → drain → close, multi-subscription, reconnect under load).
- `MIGRATION.md`.
- **End of phase: cut `v1.0.0-beta.1`** — first consumer-facing tag. `kollector-esport` and `ots-odds-bridge` migration starts here.

### Phase 7a — Examples & migration prep (1–2 days)

- New `examples/` directory replacing `example/main.go`.
- `README.md` updates pointing at examples and migration guide.
- Final touch-ups to `MIGRATION.md` based on Phase-6 integration findings.
- Optional: codemod script for mechanical renames in consumer repos.

### Phase 7b — Consumer migration & staging bake-in (1–2 weeks calendar)

This phase is **calendar-bound, not engineer-bound** — most of the elapsed time is staging soak, not coding.

- Update `kollector-esport` on a feature branch; run its integration tests; deploy to staging.
- Update `ots-odds-bridge` on a feature branch; run its integration tests; deploy to staging.
- Soak in staging for **at least 24 hours of representative traffic per consumer**, ideally 72h covering at least one weekend live-event window.
- Cut `v1.0.0-beta.N` tags as bugs surface during bake-in and get fixed (the first beta was cut at end of Phase 6 — see §13).
- After bake-in completes cleanly: cut stable `v1.0.0` tag.

### Total time

| Track | Effort | Calendar |
|---|---|---|
| **Phases 0–6 (engineering)** | 4–5 engineer-weeks of focused work | ~3 weeks calendar with two engineers in parallel on phases 1–4; ~4–5 weeks solo |
| **Phase 7a (examples)** | 1–2 days | Same |
| **Phase 7b (bake-in)** | <1 day of engineer time per consumer + monitoring | 1–2 weeks calendar — staging soak gates the cutover |
| **Total to consumer-facing beta** | ~4–5 weeks | End of Phase 6 |
| **Total to stable v1.0.0** | ~5–7 weeks | After Phase 7b bake-in |

Phases 1–4 are mostly independent and can run in parallel across two engineers. Phase 5 (recovery state machine) is on the critical path — it depends on Phase 4 (feed) and Phase 2 (api) and gates Phase 6.

## 18. Risks

| Risk | Impact | Mitigation |
|---|---|---|
| Recovery state-machine rewrite changes observable behavior under edge cases (e.g., when producers flap) | Medium | Drive tests from real-world traces captured in the smoke run; cover specific scenarios reported as bugs by current consumers. |
| LRU eviction surprises a consumer that assumed unbounded cache | Low | Default capacities are generous (5k entries for event/variant caches, 10k players); no public cache-size option shipped — expose `WithCacheSize(...)` later if a consumer needs it. |
| Locale enum expansion breaks downstream pattern matches on a closed switch | Low | `Locale` stays a string alias; consumers' `switch` statements with a `default` keep compiling and treat new locales as default. |
| `testing/synctest` stabilization (Go 1.26) lands after consumer cutover plan is locked | Resolved | Consumers cleared to bump to 1.26 alongside the v2.x SDK; `testing/synctest` is stable and AVAILABLE — not yet used (the suite settled on fake tickers/handshakes, see §14 Tools); `clockwork` shim never landed. |
| `go 1.26` features unstable when consumer floor is 1.24 | Resolved | Consumer floor raised to 1.26; module directive is `go 1.26.0`. The SDK is free to use 1.25 / 1.26 stdlib idioms (`sync.WaitGroup.Go`, stable `testing/synctest`, etc.). |
| Migration friction in consumer repos | Medium | Migration guide + a small codemod script (`gosdk-migrate`) that does mechanical renames (`feed.ProducerManager().AvailableProducers()` → `client.Producers(ctx)`). |
| Hidden behavior dependencies in `kollector-esport`'s out-of-order message buffer (it relies on current recovery-state semantics) | High | Run kollector against the new SDK on a staging environment for 24h+ before promoting beta to stable. Allow real migration time on the consumer side — see §13. |
| Performance regression vs current implementation | Low | The current implementation is not optimized; the new one isn't slower in any obvious way. Benchmark the per-message hot path (XML decode + market description lookup) and compare. |

## 19. Open questions

(The four reviewer-flagged decisions are now resolved in §0. The list below is what's still unresolved.)

1. ~~**MaxInactivitySeconds default**~~ **RESOLVED:** default kept at 20s (matches Java/.NET). Renamed surface as part of v2's `time.Duration`-everywhere clean-up — `MaxInactivitySeconds() int` and `MaxRecoveryExecutionMinutes() int` on the internal config interface became `MaxInactivity() time.Duration` and `MaxRecoveryExecution() time.Duration`. Drops the `int(d.Seconds())` round-trip in `configAdapter` (which truncated sub-second values to 0) and the `float64(...)` wrappers at the recovery actor's call sites. The interface itself was relocated from `types.OddsFeedConfiguration` to `internal/config.Config` (it's purely internal-facing — consumers construct via `gosdk.NewConfig(...) + WithX(...)` and never implement it). The legacy no-op `Set*` methods on the interface were dropped; nothing called them.
2. ~~**InitialSnapshotTime semantics**~~ **RESOLVED:** `time.Duration` (matches Java's `Duration` shape, Go-idiomatic). Option signature: `gosdk.WithInitialSnapshotTime(d time.Duration)`. Current gosdk has no equivalent field — this is a pure parity addition for .NET/Java, no behavior to preserve.
3. ~~**Connection events on reconnect**~~ **RESOLVED:** first transition only. Sequence: `Disconnected{err}` once on drop → `Reconnecting` once when retry loop starts → per-attempt failures go to `slog.Warn(attempt=N, err)` only (no event) → `Connected` on success. A second drop *during* the reconnecting state must not re-emit `Reconnecting` — the loop just keeps going. Matches Java/.NET (which fire once, not per attempt). Polling `ConnectionState()` covers "still trying" for any consumer that needs it. A future `WithVerboseConnectionEvents()` opt-in can add per-attempt events if there's ever an ask.
4. **Replay session shape** — Java/.NET have a separate `IReplayOddsFeed` / `ReplayFeed` type. We're folding it into `Subscribe(ctx, gosdk.WithReplay())`. Confirm this is acceptable or split into a `ReplayClient` to mirror the references more closely.
5. ~~**`ExceptionHandlingStrategy` scope**~~ **RESOLVED:** message-processing only (AMQP consumer decode-and-route). Does NOT apply to `Client.X(ctx, ...)` API methods (Go's `(T, error)` is always the contract) or to background goroutines (recovery actors, reconnect — they always log-and-continue, otherwise a transient blip permanently downs a producer). See §10 Exception strategy for the full scope rule.
6. **Logger replacement at runtime** — should `client.SetLogger(*slog.Logger)` exist, or is logger immutable post-construction? Lean toward immutable.
7. **Codemod script** — worth writing or just a migration guide table?
8. ~~**Recovery handle GC grace period**~~ **RESOLVED:** 5 minutes (constant `recovery.HandleGCGracePeriod`). Generous enough for a consumer that polls `EventRecoveryStatus(requestID)` minutes after initiating recovery; cheap because completed handles are tiny and GC happens on the actor tick. A consumer that needs longer retention should cache `(requestID, outcome)` itself rather than rely on SDK retention.
9. **User-facing manual-ack opt-in** — should we offer `WithManualAck()` exposing ack semantics to users (for at-least-once / exactly-once integrations on top), or leave that for v2.1+ if anyone asks? Lean toward "leave for later" — no current ask. Note this is separate from §0.6 (which is internal manual-ack, decided).
10. **Body redaction completeness** — the redaction substring set covers the access token by default. Should we expose `WithAPICallRedaction(substrings ...string)` for callers who route additional secrets through the SDK (e.g., custom headers in a future API)? Lean toward yes, deferred to first concrete need.

---

## Appendix A — Mapping current → new at a glance

| Old | New |
|---|---|
| `gosdk.NewConfiguration(...).SetX(...)` | `gosdk.NewConfig(..., gosdk.WithX(...))` |
| `gosdk.NewOddsFeed(cfg)` | `gosdk.New(ctx, cfg)` (no separate Open) |
| `feed.SessionBuilder().SetMessageInterest(x).Build()` | `client.Subscribe(ctx, gosdk.WithMessageInterest(x))` |
| `feed.Open()` | (split: API/cache setup → `New`; AMQP connect → first `Subscribe` or explicit `client.Connect(ctx)`) |
| `feed.Close()` | `client.Close(ctx)` (idempotent, ctx-aware) |
| `feed.BookmakerDetails()` | `client.BookmakerDetails(ctx)` |
| `feed.ProducerManager().AvailableProducers()` | `client.Producers(ctx)` |
| `feed.ProducerManager().ActiveProducers()` | `client.ActiveProducers(ctx)` |
| `feed.ProducerManager().GetProducer(id)` | `client.Producer(ctx, id)` |
| `feed.ProducerManager().SetProducerState(id, b)` | `client.SetProducerEnabled(ctx, id, b)` |
| `feed.RecoveryManager().InitiateEventOddsMessagesRecovery(p, e)` | `client.RecoverEventOdds(ctx, p, e)` — returns `*RecoveryHandle` instead of bare `requestID` |
| `feed.RecoveryManager().InitiateEventStatefulMessagesRecovery(p, e)` | `client.RecoverEventStateful(ctx, p, e)` — returns `*RecoveryHandle` instead of bare `requestID` |
| `feed.MarketDescriptionManager().MarketDescriptions()` | `client.MarketDescriptions(ctx, locales...)` |
| `feed.MarketDescriptionManager().MarketDescriptionByIDAndVariant(id, v)` | `client.MarketDescription(ctx, id, v, locales...)` |
| `feed.MarketDescriptionManager().MarketVoidReasons()` | `client.MarketVoidReasons(ctx)` |
| `feed.SportsInfoManager().Sports()` | `client.Sports(ctx, locales...)` |
| `feed.SportsInfoManager().Match(urn)` | `client.Match(ctx, urn, locales...)` |
| `feed.SportsInfoManager().ActiveTournaments()` | `client.ActiveTournaments(ctx, locales...)` |
| `feed.SportsInfoManager().Competitor(urn)` | `client.Competitor(ctx, urn, locales...)` |
| `feed.SportsInfoManager().FixtureChanges(after)` | `client.FixtureChanges(ctx, after, locales...)` |
| `feed.ReplayManager().StartReplay(...)` | `client.Replay().Start(ctx, opts...)` |
| `feed.ReplayManager().StopReplay()` | `client.Replay().Stop(ctx)` |
| `feed.ReplayManager().ClearReplay()` | `client.Replay().Clear(ctx)` |
| `feed.ReplayManager().Add/RemoveEvent(urn)` | `client.Replay().AddEvent/RemoveEvent(ctx, urn)` |
| `feed.ReplayManager().GetReplayList()` | `client.Replay().List(ctx)` |
| (no equivalent) | `client.Replay().Status(ctx)` (NEW) |
| (no equivalent) | `client.Replay().StopAndClear(ctx)` (NEW) |
| (no equivalent) | `client.Connect(ctx)` (NEW; explicit AMQP connect — optional) |
| (no equivalent) | `client.ConnectionEvents()` + `client.ConnectionState()` (NEW) |
| (no equivalent) | `client.RecoveryEvents()` + `client.ProducerStatus(id)` + `client.EventRecoveryStatus(reqID)` (NEW polling shape) |
| (no equivalent) | `*RecoveryHandle` (`Done()` / `Result()` / `Status()`) — reliable per-request completion (NEW) |
| (no equivalent) | `Subscription.Done()` + `Subscription.Err()` modeled on `context.Context` (NEW) |
| (no equivalent) | `WithAMQPPrefetch(n)` / `WithSubscriptionBuffer(n)` — explicit backpressure knobs (NEW) |
| (no equivalent) | `client.APIEvents()` + `WithAPICallLogging(level)` (NEW; explicit .NET client ask) |
| (no equivalent) | `client.ClearMatch/Tournament/Competitor/Player(...)` (NEW) |
| (no equivalent) | `client.SetProducerRecoveryFromTimestamp(...)` (NEW; was internal-only) |

**Note:** Appendix A is illustrative, not exhaustive. Methods like `Player`, `MatchesFor`, `LiveMatches`, `ListMatches`, `AvailableTournaments`, `Sport(id)`, etc. follow the same mechanical mapping (manager → direct method on `Client`, with `ctx` first and `locales ...types.Locale` last). Likewise every config option moves from `cfg.SetX(...)` chained-setter form to `gosdk.WithX(...)` functional-option form. The full list is in §4.

## Appendix B — Verified parity gaps closed by this rewrite

(From the cross-SDK audit captured under `/Users/dsaiko/.claude/projects/.../memory/sdk_caching_localization.md` and the `next`-branch analysis.)

1. ✅ Per-call locale on every query method.
2. ✅ Per-message locale — `oddsChange.Markets()` is unchanged (no locale param, matching the existing `types.OddsChange` shape); locale lives on the per-market accessor `market.Name(locale)` returning `Optional[string]`, which reads from preloaded/prefetched cache. Returns `None` if absent (consumer must list locales in `WithPreloadLocales(...)` or prefetch via `client.MarketDescription(ctx, ...)`). No hidden synchronous I/O from message accessors.
3. ✅ Public cache invalidation on managers.
4. ✅ Wider locale enum (12 values vs current 3).
5. ✅ Maintained cache library (`golang-lru/v2` vs unmaintained `go-cache`).
6. ✅ Connection-state observability (`ConnectionEvents()`).
7. ✅ `ExceptionHandlingStrategy` config.
8. ✅ `InitialSnapshotTime` config.
9. ✅ `HTTPClientTimeout` config.
10. ✅ `SetProducerRecoveryFromTimestamp` on public surface.
11. ➖ `SelectReplay` / `SelectCustom` environment helpers — superseded, not shipped: environment constants + `WithRegion`/`WithAPIHost`/`WithMQHost` cover the use cases (see §12 Environment helpers).
12. ✅ Replay status query and `StopAndClear`.
13. ✅ Logger injection (`WithLogger(*slog.Logger)`).
14. ✅ `context.Context` propagation throughout.
15. ✅ API call logging — debug-level slog automatically; opt-in `APIEvent` channel with selectable verbosity (`WithAPICallLogging`). Closes Java's `onRawApiDataReceived` parity gap and the explicit .NET client ask.
