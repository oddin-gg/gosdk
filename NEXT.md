# gosdk rewrite — design plan (target: v1.0.0)

Design plan for the gosdk rewrite. Lives on the `next` branch. The current module is at `v0.0.x` pseudo-versions, so breaking changes are still in-charter under semver. Output of this rewrite is a real `v1.0.0` release on the existing import path (`github.com/oddin-gg/gosdk`) — no `/v2` directory, no `v2` branch. Pre-Phase-6 tags are internal alphas (`v1.0.0-alpha.N`); first consumer-facing beta (`v1.0.0-beta.1`) cuts at the end of Phase 6 once the public `Client` is wired up. See §13 for full tag cadence.

This document is the source of truth for the rewrite. Update it as decisions change.

## 0. Resolved key decisions

(Documented up front because the reviewer flagged ambiguity in earlier drafts.)

1. **Lazy AMQP open.** `gosdk.New(ctx, cfg)` does API + cache + producer setup only — no AMQP. AMQP opens on the first `Subscribe(...)` call, or eagerly via the explicit `client.Connect(ctx)`. API-only usage works with a misconfigured or down message broker.
2. **Entity/message accessors do NOT do I/O.** Methods like `match.LocalizedName(locale)` and `market.LocalizedName(locale)` (read off `oddsChange.Markets()`) return from in-memory data. Missing locale → `ErrLocaleNotAvailable`. Callers prefetch with `client.Match(ctx, urn, locales...)` or rely on `WithPreloadLocales(...)`. No hidden synchronous HTTP from accessor methods.
3. **Event channels are buffered + lossy with logged drop.** All three (`ConnectionEvents`, `RecoveryEvents`, `APIEvents`) drop oldest on overflow and emit a `slog.Warn`. Polling getters (`client.ConnectionState()`, `client.ProducerStatus(id)`) expose current state for consumers that miss events. RecoveryEvents has the largest buffer (1024) since recovery completion notifications matter more.
4. **Versioning is `v0.0.x → v1.0.0`, not "v2".** No `/v2` import path. No `v2` branch. Beta tags during the rewrite, then a clean `v1.0.0`.
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
6. **Modern Go used pragmatically.** Module directive `go 1.24` (matches current consumer floor: kollector / ots are on 1.24/1.25). Use `log/slog`, generic type aliases, `iter.Seq` where they actually simplify the design — not as goals in themselves. Avoid 1.26-only features (`testing/synctest` stabilization, etc.) that would force consumers to bump.
7. **Pragmatic migration.** Breaking changes are permitted, with a clear migration guide. Bootstrap code (~30 lines) is the easy diff; the message-consumption loop and producer/recovery semantics need real review per consumer (see §13).

## 2. Non-goals

- Removing functionality that .NET/Java expose (e.g., Replay, multi-session, all `MessageInterest` values). Keep all of it even though current Go consumers don't use it.
- Changing the wire protocol. AMQP routing keys, XML message envelopes, REST API endpoints stay identical to current — this is purely a Go SDK rewrite.
- Backward source compatibility with v0.0.x. We get one breaking change; we use it.
- A separate `/v2` module or `v2` branch. Same import path, beta tags during rewrite, `v1.0.0` at cutover.
- Forcing consumers off Go 1.24. Module directive sits at the consumer floor; we do not chase 1.26-only features unless they materially simplify a critical path.
- Sealing `protocols.Message` (or any other public interface) with unexported methods. Internal consumers already mock these interfaces in their tests; aggressive sealing would break that workflow without payoff.

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

The new top-level package is still `github.com/oddin-gg/gosdk`. The `protocols/` subpackage retains all current entity types (Match, Tournament, Competitor, Player, OddsChange, BetSettlement, BookmakerDetail, Producer, ProducerScope, MessageInterest, URN, Locale, Environment, Region, …). Field shapes and method signatures on these types stay source-compatible where possible — both consumers (kollector-esport, ots-odds-bridge) import them widely.

Top-level package replaces the manager-of-managers shape with a flat `Client`:

```go
package gosdk

// Construction — does API + cache + producer setup; does NOT open AMQP.
func NewConfig(token string, env protocols.Environment, opts ...Option) Config
func New(ctx context.Context, cfg Config) (*Client, error)

// Lifecycle
func (*Client) Connect(ctx context.Context) error  // explicit AMQP open; optional — Subscribe will lazy-connect
func (*Client) Close(ctx context.Context) error    // idempotent; safe to call repeatedly

// Subscribe replaces SessionBuilder + Build(). First call opens the AMQP connection if Connect wasn't called.
// Subscription supports graceful drain via Close(ctx) and abrupt termination via ctx cancellation — see §8 Subscriptions.
func (*Client) Subscribe(ctx context.Context, opts ...SubscribeOption) (*Subscription, error)

// Subscription — returned from Subscribe. All methods safe for concurrent use.
// (*Subscription).Messages() <-chan protocols.Message  // closed after graceful drain or abrupt termination
// (*Subscription).Close(ctx context.Context) error     // graceful drain; ctx is the drain deadline; safe to call repeatedly
// (*Subscription).Done() <-chan struct{}               // closed when subscription terminates (any reason)
// (*Subscription).Err() error                          // nil on graceful close; non-nil on ctx-cancel / terminal error

// Bookmaker / connection state
func (*Client) BookmakerDetails(ctx context.Context) (protocols.BookmakerDetail, error)
func (*Client) ConnectionState() ConnectionState         // current state, polling-friendly
func (*Client) ConnectionEvents() <-chan ConnectionEvent // state-change events; lossy on overflow
func (*Client) APIEvents() <-chan APIEvent               // raw HTTP request/response events (opt-in via WithAPICallLogging)

// Producers
func (*Client) Producers(ctx context.Context) ([]protocols.Producer, error)
func (*Client) ActiveProducers(ctx context.Context) ([]protocols.Producer, error)
func (*Client) ProducersInScope(ctx context.Context, scope protocols.ProducerScope) ([]protocols.Producer, error)
func (*Client) Producer(ctx context.Context, id uint) (protocols.Producer, error)
func (*Client) SetProducerEnabled(ctx context.Context, id uint, enabled bool) error
func (*Client) SetProducerRecoveryFromTimestamp(ctx context.Context, id uint, t time.Time) error  // NEW (parity)

// Recovery — every initiate call returns a handle for reliable per-request completion.
func (*Client) RecoverEventOdds(ctx context.Context, producerID uint, eventID protocols.URN) (*RecoveryHandle, error)
func (*Client) RecoverEventStateful(ctx context.Context, producerID uint, eventID protocols.URN) (*RecoveryHandle, error)
func (*Client) RecoveryEvents() <-chan RecoveryEvent           // ProducerStatus + EventRecoveryComplete stream; lossy on overflow
func (*Client) ProducerStatus(producerID uint) ProducerStatus  // current snapshot, polling-friendly
func (*Client) EventRecoveryStatus(requestID uint) (RecoveryStatus, bool) // by request ID; second result false when unknown / GC'd

// RecoveryHandle exposes per-request semantics. Tracked internally until completion + a grace period.
// (*RecoveryHandle).RequestID() uint
// (*RecoveryHandle).Done() <-chan struct{}        // closes on any terminal state
// (*RecoveryHandle).Result() (RecoveryResult, error)  // blocks until Done; returns terminal status
// (*RecoveryHandle).Status() RecoveryStatus       // non-blocking snapshot

// Sports info
func (*Client) Sports(ctx context.Context, locales ...protocols.Locale) ([]protocols.Sport, error)
func (*Client) Sport(ctx context.Context, id protocols.URN, locales ...protocols.Locale) (protocols.Sport, error)
func (*Client) ActiveTournaments(ctx context.Context, locales ...protocols.Locale) ([]protocols.Tournament, error)
func (*Client) AvailableTournaments(ctx context.Context, sportID protocols.URN, locales ...protocols.Locale) ([]protocols.Tournament, error)
func (*Client) Match(ctx context.Context, id protocols.URN, locales ...protocols.Locale) (protocols.Match, error)
func (*Client) MatchesFor(ctx context.Context, t time.Time, locales ...protocols.Locale) ([]protocols.Match, error)
func (*Client) LiveMatches(ctx context.Context, locales ...protocols.Locale) ([]protocols.Match, error)
func (*Client) ListMatches(ctx context.Context, start, limit int, locales ...protocols.Locale) ([]protocols.Match, error)
func (*Client) Competitor(ctx context.Context, id protocols.URN, locales ...protocols.Locale) (protocols.Competitor, error)
func (*Client) Player(ctx context.Context, id protocols.URN, locales ...protocols.Locale) (protocols.Player, error)
func (*Client) FixtureChanges(ctx context.Context, after time.Time, locales ...protocols.Locale) ([]protocols.FixtureChange, error)

// Cache invalidation (NEW — parity with .NET/Java).
// `variant *string`: nil means "the base description with no variant"; non-nil with empty value is rejected.
func (*Client) ClearMatch(id protocols.URN)
func (*Client) ClearTournament(id protocols.URN)
func (*Client) ClearCompetitor(id protocols.URN)
func (*Client) ClearPlayer(id protocols.URN)
func (*Client) ClearMarketDescription(marketID uint, variant *string)
func (*Client) ClearMarketVoidReasons()

// Market descriptions. `variant *string` preserves the nil-vs-empty distinction the wire protocol cares about.
func (*Client) MarketDescriptions(ctx context.Context, locales ...protocols.Locale) ([]protocols.MarketDescription, error)
func (*Client) MarketDescription(ctx context.Context, id uint, variant *string, locales ...protocols.Locale) (protocols.MarketDescription, error)
func (*Client) MarketVoidReasons(ctx context.Context) ([]protocols.MarketVoidReason, error)

// Replay (kept verbatim — surface drives the underlying API the same way as Java/.NET)
func (*Client) Replay() *Replay

type Replay struct{ /* methods below */ }
func (*Replay) List(ctx context.Context) ([]protocols.SportEvent, error)
func (*Replay) AddEvent(ctx context.Context, eventID protocols.URN) error
func (*Replay) RemoveEvent(ctx context.Context, eventID protocols.URN) error
func (*Replay) Start(ctx context.Context, opts ...ReplayOption) error
func (*Replay) Stop(ctx context.Context) error
func (*Replay) Clear(ctx context.Context) error
func (*Replay) StopAndClear(ctx context.Context) error  // NEW (parity with .NET)
func (*Replay) Status(ctx context.Context) (string, error)  // NEW (parity with .NET)
```

### Configuration via functional options

Replaces the broken value-receiver setter chain:

```go
cfg := gosdk.NewConfig(token, protocols.TestEnvironment,
    gosdk.WithNodeID(1),
    gosdk.WithDefaultLocale(protocols.EnLocale),
    gosdk.WithPreloadLocales(protocols.EnLocale, protocols.RuLocale),
    gosdk.WithRegion(protocols.RegionDefault),
    gosdk.WithAPIURL("..."),
    gosdk.WithMQURL("..."),
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
)
```

`Config` is an immutable value externally. Internally, `NewConfig` constructs a private draft, applies each `Option func(*Config)` to the draft, and returns the finalized value by copy. After return, the `Config` value cannot be mutated — `Option` closures don't escape `NewConfig`. No setter-chain pitfalls; no shared mutable state.

### Subscribe / Subscription

```go
sub, err := client.Subscribe(ctx,
    gosdk.WithMessageInterest(protocols.AllMessageInterest),
    gosdk.WithSpecificEvents(eventA, eventB), // optional
    gosdk.WithReplay(),                        // marks as replay session
)

for msg := range sub.Messages() {
    switch m := msg.(type) {
    case protocols.OddsChange:    ...
    case protocols.BetStop:       ...
    case protocols.BetSettlement: ...
    // ... all message types
    case protocols.Unparsable:    ...  // surfaced when SDK can't parse
    case protocols.RawFeed:       ...  // surfaced when WithExtendedDataReporting(true)
    }
}
// Subscription closes automatically when ctx is cancelled or client.Close is called.
err := sub.Err()  // sticky, set on terminal failure
```

`Subscription.Messages()` returns `<-chan protocols.Message`. `protocols.Message` is an **open interface** — the SDK ships concrete types (`OddsChange`, `BetStop`, `BetSettlement`, `BetCancel`, `RollbackBetCancel`, `RollbackBetSettlement`, `FixtureChange`, `Unparsable`, `RawFeed`) that implement it, but consumers can implement the interface in their own tests/mocks. No unexported sealing methods.

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

### What stays unchanged in `protocols/`

All entity interfaces (`Match`, `Tournament`, `Competitor`, `Player`, `Sport`, `Fixture`, `Scoreboard`, `MatchStatus`, `MarketDescription`, `MarketVoidReason`, `OutcomeDescription`, `Specifier`, etc.) keep their current method signatures. Both consumers depend on these widely (~40 files combined); this is the source-compatibility line.

Two extensions:
- Methods that take `locale` already take `locale`. We just plumb the value through to the cache properly so non-default locales actually return data.
- A handful of new methods may be added (e.g., `Specifiers()` if missing) but no existing method is removed or renamed.

## 5. Internal architecture

Layered, with no upward dependencies:

```
+------------------------------------------------+
|              gosdk (public Client)             |
+------------------------------------------------+
|                                                |
|  Subscribe / Recovery events / Manager methods |
|                                                |
+------------------------------------------------+
|     internal/feed         internal/recovery    |
|   (AMQP consumer)         (state machine)      |
+------------------------------------------------+
|     internal/cache         internal/api        |
|   (LRU + map caches)      (HTTP client)        |
+------------------------------------------------+
|              internal/xml (decoders)           |
+------------------------------------------------+
|             protocols (entity types)           |
+------------------------------------------------+
```

Every layer is testable in isolation:
- `internal/xml` — pure decode; golden-file tests.
- `internal/api` — `httptest.Server`-backed tests for every endpoint.
- `internal/cache` — concurrency tests, TTL tests, single-flight dedup tests.
- `internal/recovery` — deterministic state-machine tests with mock clock.
- `internal/feed` — fake AMQP broker tests for routing/delivery.
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

type Loader[K comparable, V any] func(ctx context.Context, key K, locales []protocols.Locale) (V, error)

type EventCache[K comparable, V any] struct {
    lru    *lru.LRU[K, V]
    sf     singleflight.Group
    loader Loader[K, V]
}

func (c *EventCache[K, V]) Get(ctx context.Context, key K, locales []protocols.Locale) (V, error)
func (c *EventCache[K, V]) Clear(key K)
func (c *EventCache[K, V]) Purge()
```

- `Get` returns the cached entry if all requested locales are present; otherwise calls `loader` (deduplicated via singleflight) and merges fetched locales into the entry.
- `Clear` is the public invalidation hook (`client.ClearMatch(urn)` etc.).
- `Purge` is invoked on shutdown.
- Every cache value is a struct with explicit per-locale fields (see §7). All field access is mutex-protected within the entry — no partial-locking like the current code.
- Default capacity per cache: configurable, sensible defaults (e.g., 5000 matches, 50000 competitors). Default TTL: 12h to match current behavior.

### Static-catalog caches (map + RWMutex)

For: base `MarketDescriptionCache` (non-variant), `MarketVoidReasonsCache`, `MatchStatusDescriptionCache`, `SportsCache`.

```go
type StaticCache[K comparable, V any] struct {
    mu     sync.RWMutex
    perLocale map[protocols.Locale]*staticEntry[K, V]
    loader func(ctx context.Context, locale protocols.Locale) (map[K]V, error)
}

type staticEntry[K comparable, V any] struct {
    mu      sync.Mutex     // serializes load attempts for this locale
    loaded  bool
    data    map[K]V
    lastErr error
}
```

- Loaded once per locale on first access; subsequent reads hit the map under RLock.
- **No `sync.Once`.** Failed loads (network error, 5xx) reset `loaded=false` so the next access retries. `sync.Once` is a footgun here — a transient failure would otherwise poison the cache forever.
- `Clear` resets the entry for that locale, forcing a refresh on next access.
- No size limit — these catalogs are small (hundreds of entries).

### Variant / dynamic market descriptions (LRU, not static)

Variant market descriptions (`/descriptions/{locale}/markets/{id}/variants/{variant}`) are NOT in the static catalog. They form an unbounded long tail (one entry per `(marketID, variant, locale)` tuple, where `variant` may include things like `mapnr=1`, `setnr=3`, etc.). They live in their own bounded LRU + singleflight, same shape as the per-event caches. Default capacity: 5000 entries.

### Cache invalidation triggers

- **Public methods** on `Client`: `ClearMatch`, `ClearTournament`, `ClearCompetitor`, `ClearPlayer`, `ClearMarketDescription`, `ClearMarketVoidReasons`. Parity with .NET/Java.
- **Auto-invalidation** on `FixtureChange` feed message: clears the affected match cache entry. Existing behavior, kept.
- **TTL eviction**: per-cache TTL handled by `expirable.LRU`.
- **LRU eviction**: per-cache size cap.

## 7. Localization

### Configuration

```go
gosdk.WithDefaultLocale(protocols.EnLocale)
gosdk.WithPreloadLocales(protocols.EnLocale, protocols.RuLocale, protocols.DeLocale)
```

`WithPreloadLocales` controls which locales the SDK fetches eagerly when warming static catalogs (sports, market descriptions). Per-event entities are still fetched lazily per locale on first request.

### Locale enum expansion

Current Go enum: `EnLocale`, `RuLocale`, `ZhLocale` (3 values).

New enum matches .NET/Java's 12: `en`, `br`, `de`, `es`, `fi`, `fr`, `pl`, `pt`, `ru`, `th`, `vi`, `zh`. The constant names follow current `XxLocale` convention (e.g., `BrLocale`, `DeLocale`, …). The `Locale` type itself stays a string alias so values can be added without recompiling consumers.

### Per-call locale plumbing

Every public query method takes `locales ...protocols.Locale` (variadic, defaults to configured default if empty):

```go
match, err := client.Match(ctx, urn)                              // default locale
match, err := client.Match(ctx, urn, protocols.RuLocale)          // explicit
match, err := client.Match(ctx, urn, protocols.EnLocale, protocols.RuLocale)  // multi
```

Inside the cache, `Get` is called with the requested locale slice. If any requested locale is missing from the entry, the loader fetches *only the missing locales* (one API call per missing locale, deduplicated across concurrent callers via singleflight).

### Entity / message accessors are pure data — no hidden I/O

This is the resolution to a fundamental tension: `match.LocalizedName(locale)` cannot accept a `ctx`, but it must not perform synchronous I/O without one. We resolve it by making accessors pure-data:

- `client.Match(ctx, urn, locales...)` — performs I/O (fetches missing locales), returns a `Match` whose internal cache is populated for the requested locales.
- `match.LocalizedName(locale)` — returns the cached value. Returns `(nil, ErrLocaleNotAvailable)` if the locale wasn't requested at fetch time.
- Same pattern for `Tournament`, `Competitor`, `Player`, `Sport`, etc.

For feed messages (`OddsChange`, `BetSettlement`, …) the same rule applies: messages contain market descriptions for every locale in `WithPreloadLocales(...)`. **At message-decode time, the SDK eagerly enriches each market on the message with the corresponding market description from the cache for every preloaded locale.** This makes `market.LocalizedName(locale)` an in-memory lookup with no possibility of blocking or I/O. Reading a market in an un-preloaded locale returns `ErrLocaleNotAvailable`; the consumer must call `client.MarketDescription(ctx, id, variant, locale)` first to prime the cache (and only future messages will pick up that locale — already-decoded messages don't retroactively enrich).

```go
// Sample usage with explicit prefetch:
match, err := client.Match(ctx, urn, protocols.EnLocale, protocols.RuLocale)
ru, _ := match.LocalizedName(protocols.RuLocale)  // cached → instant
de, err := match.LocalizedName(protocols.DeLocale) // err == ErrLocaleNotAvailable

// Sample feed-message usage:
cfg := gosdk.NewConfig(token, env, gosdk.WithPreloadLocales(protocols.EnLocale, protocols.RuLocale))
// ... in message loop ...
markets := oddsChange.Markets()                          // OddsChange.Markets() — no locale param (matches existing protocols)
name, err := markets[0].LocalizedName(protocols.RuLocale) // locale lives on the per-market accessor; cached at startup → instant; never blocks
```

This differs from .NET's "sync fetch on demand" but is the right Go idiom: synchronous I/O without `ctx` from a hot message-processing goroutine is a deadlock waiting to happen. Migration cost: callers that want non-default locales must enumerate them up front. Documented in `MIGRATION.md`.

## 8. Lifecycle & cancellation

### Construction

`gosdk.New(ctx, cfg)` — API/cache/producer setup only. **Does NOT touch AMQP.** API-only usage (e.g., a CLI that reads market descriptions) succeeds even when the message broker is unreachable.

1. Validate config.
2. Construct logger from config.
3. Construct HTTP API client.
4. Fetch BookmakerDetails (one call, blocking).
5. Construct producer manager, fetch producers from API.
6. Construct caches; warm static catalogs for every locale in `WithPreloadLocales(...)`.
7. Spawn recovery state-machine goroutine per active producer in **dormant mode**: no inactivity timer, no producer-down emission, no API recovery calls. Actors arm themselves on the first AMQP-related event (`Connect`, `Subscribe`, or first delivered message). API-only clients stay completely quiet — no spurious "producer down" events, no background HTTP from recovery actors.
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
2. Wait for all internal goroutines to exit via a `sync.WaitGroup`. No external deadline here — the shutdown sequence runs to completion regardless of any caller's `ctx`.
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

### Subscriptions

`client.Subscribe(ctx, opts...)`:
- Creates an AMQP queue + consumer, spawns a goroutine that fans out messages to the subscription's channel.
- Subscription has its own context derived from both the caller's `ctx` and the client's internal context.
- All channel sends use `select` with the derived ctx.
- Subscription exposes `Sub.Done() <-chan struct{}` and `Sub.Err() error` modeled on `context.Context`. `Done()` closes when the subscription terminates for any reason (graceful close, ctx-cancel, terminal error); `Err()` returns the cause (`nil` for graceful close, non-nil for error termination). Composes well with consumers' own select loops.

**Two distinct termination paths — different semantics, on purpose:**

| Path | API | Behavior |
|---|---|---|
| **Graceful** | `Subscription.Close(ctx)` | Stops accepting new deliveries; waits for the in-flight delivery to complete its decode + admit-to-buffer + ack cycle; drains the in-process buffered channel until consumers have read all admitted messages or the supplied `ctx` deadline expires; then closes `Messages()` channel and `Done()`. `Err()` returns nil. Use this when you want a clean shutdown with no in-flight loss. The provided `ctx` is the drain deadline — if it expires before drain completes, remaining buffered messages are discarded and `Err()` returns `ctx.Err()`. |
| **Abrupt** | Caller's `ctx` cancelled OR `client.Close(ctx)` called OR terminal error | Subscription terminates immediately. The currently-in-flight delivery (if any) is `Nack(requeue=false)` rather than acked — see §AMQP backpressure failure handling below. Buffered messages already admitted to `Messages()` channel remain readable until the consumer stops reading or the channel is closed. `Err()` returns the cancellation cause or terminal error. Use this for emergency shutdown or when the caller is unwinding via context. |

**Note:** abrupt cancellation may drop the single message currently being processed (between AMQP delivery and channel admission). Buffered messages already in the `Messages()` channel are still visible to readers until the channel closes. Consumers that need zero-loss shutdown should call `Subscription.Close(ctx)` with a generous deadline before cancelling their context.

**`MIGRATION.md` MUST call this out plainly:** `client.Close(ctx)` is **abrupt** for active subscriptions — it cancels the internal context, which terminates every subscription via the abrupt path (Nack on the in-flight delivery, no drain). Consumers that want graceful drain MUST call `sub.Close(ctx)` on each subscription **before** calling `client.Close(ctx)`. The recommended shutdown idiom:

```go
// Graceful shutdown: drain subscriptions first, then close the client.
drainCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()
_ = sub.Close(drainCtx)
_ = client.Close(drainCtx)
```

This is the single most common migration footgun for consumers used to the old `feed.Close()` shape — the migration guide must show this idiom prominently.

### AMQP backpressure & ack policy

**Manual ack (internal).** The SDK calls `channel.Consume(..., noAck=false, ...)` and acks each delivery itself, **after** decoding and after `select { case subscriptionBuffer <- msg: case <-ctx.Done(): return }` has admitted the message into the in-process subscription buffer. Users never see an `Ack`/`Nack` API — manual ack is implementation detail.

Why this matters: AMQP's `prefetch` setting bounds **unacked** deliveries. With the current SDK's auto-ack (`noAck=true`), the broker considers every delivery acked the instant it leaves the broker, so `prefetch` is meaningless and there is no broker-visible backpressure. The new SDK's internal manual-ack makes `prefetch` actually function — unacked-from-broker count is bounded by it, and a stalled consumer applies true broker-level backpressure.

**Message flow with the corrected ack model:**

1. RabbitMQ delivers up to `prefetch` (default 1000) **unacked** messages to the SDK.
2. SDK consumer goroutine reads from the AMQP delivery channel, decodes the message.
3. Consumer goroutine pushes the decoded message into the subscription's internal buffered channel via `select { case ch <- msg: case <-ctx.Done(): }`.
4. Once the channel send succeeds, the consumer calls `delivery.Ack(false)`. The slot becomes available for the next prefetch.
5. If the subscription buffer is full (consumer stalled), step 3 blocks. The delivery is not acked. After `prefetch` deliveries pile up unacked, the broker stops sending more to this consumer.

This means a slow or stalled consumer applies backpressure all the way to the broker, bounded by `prefetch`. No in-SDK dropping. If the consumer remains stalled long-term, the broker side may queue messages or close the connection per its own configuration — but the SDK has done its part.

**Failure handling:**

- **Decode failure** → ack the delivery (we'd never decode it correctly on retry) and emit an `Unparsable` message into the subscription if `ExceptionStrategy` is `Catch`, otherwise terminate the subscription.
- **Graceful subscription close** (`Subscription.Close(ctx)`) → finish the in-flight decode + admit + ack cycle for the current delivery; stop accepting new deliveries; drain the in-process buffer to consumers (up to the supplied `ctx` deadline). No `Nack`. See §Subscriptions for full graceful-close semantics.
- **Abrupt cancellation** (caller `ctx` cancelled, `client.Close(ctx)`, terminal error) → the in-flight delivery is `Nack(requeue=false)`. We don't requeue, because the SDK's own recovery mechanism (snapshot/event recovery) is the authoritative gap-filler; double-delivery from broker requeue would be noise. The single in-flight message may be lost — recovery covers it.
- **Connection drops** → all unacked deliveries are released by the broker on connection close. After reconnect + queue rebind, the broker may re-deliver. SDK relies on its recovery mechanism to reconcile; consumer-visible message-id dedup is not a goal.

**Configurable knobs:**

- `WithAMQPPrefetch(n int)` — broker-side prefetch. Default 1000.
- `WithSubscriptionBuffer(n int)` — internal channel buffer. Default 256.
- The "slow consumer" situation emits a `slog.Warn` once per detection window if the buffer stays full for >5s — observability, not action.

This makes the message-path contract explicit and contrasts cleanly with the event-channel policy (§0.3): events are metadata and may drop; messages are payload and must not.

A future `WithManualAck()` option exposing ack semantics to users is **not in scope for v1.0.0** — see open question §19.9.

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
| `ConnectionEvents` | 16 | Drop oldest on overflow + `slog.Warn` once per drop burst | `client.ConnectionState() ConnectionState` |
| `RecoveryEvents` | 1024 | Drop oldest on overflow + `slog.Warn` once per drop burst | `client.ProducerStatus(id) ProducerStatus` |
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

1. **Structured slog at debug level** — every API call emits a `slog.Debug` event with `method`, `url`, `status`, `latency_ms`, `request_id`, `attempt`, `bytes_in`, `bytes_out`. Free, automatic, hidden behind log level. Useful for ops/troubleshooting.

2. **`APIEvent` channel** — when `WithAPICallLogging(...)` is set, each API call also emits an event to `client.APIEvents()`:
   ```go
   type APIEvent struct {
       At        time.Time
       Method    string
       URL       string               // query-string tokens redacted before emission
       Status    int
       Latency   time.Duration
       Attempt   int                  // for retried calls
       Locale    *protocols.Locale    // when applicable
       Request   []byte               // populated only when level == APILogFull; redacted + capped
       Response  []byte               // populated when level >= APILogResponses; redacted + capped
       Truncated bool                 // true if Request or Response was truncated at WithAPICallBodyLimit
       Err       error                // network/decode errors; nil for HTTP errors (use Status)
   }
   ```

3. **Verbosity levels** — `WithAPICallLogging(level APILogLevel)`:
   - `APILogOff` (default) — slog-debug only, no events emitted.
   - `APILogMetadata` — events emitted with method/url/status/latency, no bodies.
   - `APILogResponses` — events include response body bytes (typical debug setting).
   - `APILogFull` — events include both request and response bytes (heavy; intended for short-lived diagnosis).

4. **Sensitive-data redaction and size cap.** Body capture is **always sanitized**, regardless of level. Order of operations matters:
   1. **Capture** up to `WithAPICallBodyLimit(bytes)` (default 64 KiB) into a separate event buffer. The cap applies to the **copied event buffer only** — the response body that the decoder consumes is untouched.
   2. **Redact** the captured prefix: scan it for the configured access token and any other registered redaction substring; replace matches with `[REDACTED]`.
   3. **Mark truncation**: set `Truncated bool` on `APIEvent` if the original body exceeded the cap.
   - **Important:** redaction is applied to whatever was captured, even when truncated. A token that appears within the captured prefix is still scrubbed; without this order, a token inside the cap could leak.
   - **Header redaction.** Headers `Authorization`, `X-Access-Token`, and any header matching `(?i)(token|secret|api[-_]?key|cookie|set-cookie)` are replaced with `[REDACTED]` in event metadata.
   - **Authorization-bearing URLs.** Any query-string token-like parameter (`token=`, `access_token=`, `key=`) is redacted in `APIEvent.URL` before emission.

Implementation: a small middleware around `http.RoundTripper`. For each call, a `LimitedTeeWriter` (a small custom `io.Writer` — *not* a `LimitReader` in the decode path) splits the response body so the decoder receives the full unmodified bytes while a separate buffer captures up to the cap. The capture buffer is then redacted before being attached to the `APIEvent`. This guarantees the decoder is never short-fed and large responses are not corrupted. The events channel is buffered (default 256); when full, oldest events are dropped and a single `slog.Warn` is emitted — never blocks the API call.

This closes the gap vs Java's `OddsFeedExtListener.onRawApiDataReceived` and the .NET request that motivated this section. It also subsumes the current `WithExtendedDataReporting` flag for the API path; that flag stays for raw *feed* messages (different concern, different channel).

Tests cover: events emitted on success, events emitted on retry, events dropped when channel full, body capture is faithful to wire bytes, no events emitted when disabled.

## 10. Error handling

### Sentinel errors

```go
var (
    ErrAlreadyClosed         = errors.New("client closed")
    ErrInvalidConfig         = errors.New("invalid configuration")
    ErrConnectionLost        = errors.New("connection lost")
    ErrLocaleNotAvailable    = errors.New("locale not available for entity")
    ErrProducerNotFound      = errors.New("producer not found")
    ErrEventNotFound         = errors.New("event not found")
    ErrRecoveryInProgress    = errors.New("recovery already in progress")
)
```

Callers can `errors.Is(err, gosdk.ErrAlreadyClosed)` etc.

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
type recoveryActor struct {
    producerID uint
    inbox      chan recoveryEvent  // alive, snapshotComplete, processingStarted, processingEnded, recoverEvent, tick, arm, shutdown
    out        chan<- protocols.RecoveryEvent
    handles    map[uint]*recoveryHandle  // requestID → per-request handle for RecoverEvent* callers
    state      recoveryState  // private, only accessed from the goroutine
    armed      bool           // false until first AMQP-related event; dormant actors don't tick or emit producer-down
    clock      clockwork.Clock // injected for tests
    api        apiCaller
    logger     *slog.Logger
}
```

### Dormant vs armed

Actors are spawned in `armed=false` mode by `gosdk.New`. While dormant:
- No inactivity timer runs.
- No producer-down events are emitted.
- No background HTTP calls are made.
- The actor only listens for `arm` and `shutdown` on its inbox.

On `arm` (sent by `client.Connect`, the first `client.Subscribe`, or arrival of a feed message for the producer), the actor transitions to `armed=true` and starts the inactivity ticker, recovery state machine, etc. This guarantees that API-only clients (`gosdk.New` + `client.Sports(ctx)` etc.) stay quiet — no log spam, no spurious producer-down events.

### Reliable per-request completion

When a caller invokes `client.RecoverEventOdds(ctx, p, e)`, the actor:
1. Issues the API recovery request.
2. Records a `*recoveryHandle` keyed by `requestID` with status `Pending`.
3. Returns the handle to the caller.

When the corresponding `SnapshotComplete` arrives (or recovery times out / fails), the actor updates the handle's status (`Completed` / `Failed` / `TimedOut`), closes its `Done` channel, and emits a `RecoveryEvent` on the (lossy) channel. **Even if the channel event is dropped, the handle is reliable** — the caller can `<-handle.Done()`, `handle.Result()`, or `handle.Status()` and get the correct outcome. Handles are GC'd from the actor's map a configurable grace period (default 1 minute) after completion, after which `client.EventRecoveryStatus(requestID)` returns `(_, false)`.

Tests use a fake clock and drive the actor through full recovery scenarios: dormant → arm → cold start → initial snapshot → first message → late alive → producer-down by inactivity → recovery → producer-up → event recovery → handle completes even with no channel reader → handle GC'd after grace period → out-of-order arrival, etc.

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

`gosdk.NewConfig` returns a `Config`; validation happens in `gosdk.New(ctx, cfg)`. Required fields (token, environment) missing → `ErrInvalidConfig` wrapped with detail.

### Environment helpers

```go
func SelectIntegration(region protocols.Region) protocols.Environment
func SelectProduction(region protocols.Region) protocols.Environment
func SelectTest(region protocols.Region) protocols.Environment
func SelectReplay() protocols.Environment        // NEW (parity)
func SelectCustom(host, apiHost string, port int) protocols.Environment  // NEW (parity)
```

Closes the `SelectReplay` and `SelectEnvironment(host, apiHost, port)` parity gaps. Region typo (`DefaulRegion` → `RegionDefault`) fixed; old name kept as deprecated alias for one release.

## 13. Migration path

Both internal consumers (`kollector-esport`, `ots-odds-bridge`) need ~30 lines of bootstrap-code changes. Migration guide structure:

### Bootstrap diff (kollector-esport, illustrative)

```diff
-cfg := gosdk.NewConfiguration(token.Token.String(), feedEnv, rand.IntN(1000), false)
-cfg = cfg.SetAPIURL(c.apiURL).SetMQURL(c.mqURL).SetMessagingPort(c.mqPort)
-c.feed = gosdk.NewOddsFeed(cfg)
-sb, err := c.feed.SessionBuilder()
-sCh, err := sb.SetMessageInterest(protocols.AllMessageInterest).Build()
-fCh, err := c.feed.Open()
+cfg := gosdk.NewConfig(token.Token.String(), feedEnv,
+    gosdk.WithNodeID(rand.IntN(1000)),
+    gosdk.WithAPIURL(c.apiURL),
+    gosdk.WithMQURL(c.mqURL),
+    gosdk.WithMessagingPort(c.mqPort),
+)
+c.client, err = gosdk.New(ctx, cfg)
+sub, err := c.client.Subscribe(ctx, gosdk.WithMessageInterest(protocols.AllMessageInterest))
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

`protocols/*` types stay source-compatible. Both consumers' protocol-level imports keep working without change.

### Migration is not just bootstrap — recovery loops change

The bootstrap diff above is the easy part. Both consumers' message-consumption loops require deeper review:

**kollector-esport** ([services/mq/feed/client.go](../kollector-esport/services/mq/feed/client.go)) currently:
- Multiplexes session messages, global recovery messages, and a close signal in one select.
- Per-producer `outOfOrderMessages` buffer ([producer.go:37-38](../kollector-esport/services/mq/feed/producer.go#L37)) holding messages received before recovery completes.
- Drives recovery via `RequestID()` arrival semantics that depend on the current (broken) recovery state machine timing.

The new event channels separate concerns differently — `RecoveryEvents` is independent of `Subscription.Messages()`. The out-of-order buffering pattern still works, but the producer-up/down notifications arrive on a separate channel with different timing. **Plan a real migration pass with the kollector team, not a mechanical rename.** Estimate: ~1–2 days of consumer-side work plus a 24h+ staging bake-in.

**ots-odds-bridge** ([connector/sdkclient/client.go](../ots-odds-bridge/connector/sdkclient/client.go)) is closer to the example shape and migrates more straightforwardly. It also currently doesn't call `feed.Close()` — the rewrite needs that path wired in (and the migration is a chance to add a real `defer client.Close(ctx)` plus `panic(err)` replacements at lines 511/517 with proper error handling).

### Beta tagging cadence

Pre-Phase-6 tags are **internal alphas** (`v1.0.0-alpha.N`), not consumer-facing — the public `Client` doesn't exist yet, so consumers can't actually integrate. They're useful internally for pinning a stable reference instead of branch pseudo-versions while Phases 1–5 land.

The first **consumer-facing beta** is `v1.0.0-beta.1`, cut at the end of **Phase 6** (when the public `Client` API is implemented and integration-tested). That's when `kollector-esport` and `ots-odds-bridge` can start their migration on a real tag. Subsequent `v1.0.0-beta.N` cut as bugs surface during Phase 7b. Stable `v1.0.0` after Phase 7b staging bake-in (24h minimum, 72h preferred).

## 14. Testing strategy

### Required coverage gates

- `protocols/`: 80%+ — pure data types, table-driven tests for URN parsing, locale handling, etc.
- `internal/xml/`: 90%+ — golden-file decode tests for every message type, captured from the test environment smoke run (`/tmp/gosdk_run.log` already provides real samples).
- `internal/api/`: 80%+ — every endpoint tested against `httptest.Server` with happy path, 4xx, 5xx, network error, and retry scenarios.
- `internal/cache/`: 90%+ — concurrency stress test (`go test -race`), TTL expiry, LRU eviction, single-flight dedup, cache invalidation, multi-locale fill-in.
- `internal/recovery/`: 90%+ — exhaustive state-machine tests with mock clock; covers all transitions documented in §11.
- `internal/feed/`: 80%+ — fake AMQP broker tests for routing, reconnect, backoff, message decode; goroutine-leak detection (`uber-go/goleak`).
- `gosdk` (top-level): 70%+ — end-to-end flows: open → subscribe → close, multiple subscriptions, ctx-cancellation, reconnect.

### Tools

- `github.com/stretchr/testify` for `require`/`assert`.
- `go.uber.org/goleak` for goroutine-leak detection in lifecycle tests.
- `github.com/jonboulle/clockwork` (or `testing/synctest` if stable in 1.26) for deterministic time.
- `httptest.Server` for API client tests.
- A small in-process AMQP fake under `internal/feed/testfake/`. Note: `amqp091-go` exposes concrete `*amqp.Connection` / `*amqp.Channel` types, not interfaces. The pattern: in `internal/feed/`, define local minimal interfaces (`amqpConn`, `amqpChan`) covering only the methods the SDK actually calls (`Channel()`, `NotifyClose()`, `Close()`, `QueueDeclare()`, `QueueBind()`, `Consume()`). Wrap real `*amqp.Connection` with a thin adapter, and have the fake satisfy the same interfaces. This keeps the dependency on `amqp091-go` real in production code while making consumer tests fully in-process.

### Smoke test (optional, gated)

`//go:build smoke` integration test that runs against the real test environment with a token from `TOKEN` env var. Runs the new client for 30s, verifies it connected, received messages, and shut down cleanly. Used pre-release to validate beta tags.

### CI

GitHub Actions on every PR to `next`:
- `go vet ./...`
- `go test -race ./...`
- `staticcheck ./...`
- `govulncheck ./...`
- Coverage gate: minimum thresholds per package above.

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

go 1.24

require (
    github.com/cenkalti/backoff/v5 v5.x
    github.com/google/uuid v1.6.0
    github.com/hashicorp/golang-lru/v2 v2.x
    github.com/rabbitmq/amqp091-go v1.11.x
    golang.org/x/sync v0.x   // singleflight
)

require (
    github.com/stretchr/testify v1.x  // tests only
    go.uber.org/goleak v1.x           // tests only
    github.com/jonboulle/clockwork v0.x // tests only — drop if testing/synctest is stable in 1.26
)
```

Tools (`staticcheck`, `govulncheck`) move to a sibling `tools/` Go module so they don't appear in consumers' indirect-dep lists. The current `tools.go` build-tag pattern is removed.

Removed: `sirupsen/logrus`, `patrickmn/go-cache`.

## 17. Phased rollout

Each phase is independently mergeable to `next` and CI-green. No phase merges without tests for the code it adds.

### Phase 0 — Foundation (2–3 days)

The branch must compile and pass CI at the end of every phase, including this one. We do **not** delete `logrus` and `go-cache` until their replacements are wired in (Phase 3 / 6).

- Move `staticcheck`, `govulncheck` into `tools/` submodule. Update Makefile.
- Set `go.mod` directive to `go 1.24` (matches consumer floor; we'll evaluate bumping after the rewrite ships).
- **Add** new dependencies (`golang-lru/v2`, `cenkalti/backoff/v5`, `golang.org/x/sync`, `stretchr/testify`, `uber-go/goleak`). Keep `logrus` and `go-cache` for now — they're removed in Phase 6 once nothing imports them.
- Set up CI pipeline with `-race`, coverage gate (initially soft).
- Add `goleak`, `testify` scaffolding.
- Bump `amqp091-go` to v1.11.x.

### Phase 1 — Pure types & decode (2–3 days)

- Stabilize `protocols/*` — extend `Locale` to 12 values, fix `RegionDefault` typo, add `MessageInterest` constants if any are missing.
- Rewrite `internal/xml/` with proper struct tags, no `<envelope>` synthesis.
- Golden-file tests for every message type using captures from the test-env smoke log.
- URN, routing-key parsing in `internal/feed/` — table-driven tests.

### Phase 2 — HTTP API client (2 days)

- New `internal/api/Client`: ctx-aware, slog-logged, exponential backoff via `backoff/v5`, body-close on every retry, network-error retry, header canonicalization (`Set` not direct map access), no `count++` bug in replay-start params.
- `httptest.Server`-backed test for every endpoint.

### Phase 3 — Cache layer (3–4 days)

- Generic `EventCache[K, V]` over `golang-lru/v2` + `singleflight`.
- `StaticCache[K, V]` for catalogs.
- Per-entity cache wrappers (Match, Competitor, Player, Tournament, Fixture, MarketDescription, MarketVoidReasons, Sport, MatchStatus).
- All field access in cached entries protected by per-entry mutex (no partial locking).
- Tests: concurrency stress (`-race`), TTL expiry, LRU eviction, single-flight dedup, locale fill-in, public Clear methods.

### Phase 4 — AMQP feed layer (3–4 days)

- New `internal/feed/Client`: single-goroutine reconnect loop, ctx-driven shutdown, backoff, atomic-pointer connection.
- `internal/feed/Consumer`: ctx-cancellable channel sends, no recursion, no synthesized `<envelope>`. Internal manual-ack: `noAck=false` on `Consume`, `delivery.Ack(false)` only after the decoded message is admitted to the subscription buffer.
- Fake AMQP broker (`internal/feed/testfake/`) — minimum needed to drive consumer tests, including ack/prefetch behavior.
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
| LRU eviction surprises a consumer that assumed unbounded cache | Low | Default capacities are generous (5k matches, 50k competitors); document the change in `MIGRATION.md`; expose `WithCacheSize(...)` options if needed. |
| Locale enum expansion breaks downstream pattern matches on a closed switch | Low | `Locale` stays a string alias; consumers' `switch` statements with a `default` keep compiling and treat new locales as default. |
| `testing/synctest` not stable in 1.26 at implementation time | Low | Fall back to `clockwork` — pure stdlib drop-in. |
| `go 1.26` features unstable when consumer floor is 1.24 | Eliminated by decision §0.6 — module directive is `go 1.24`; we don't depend on 1.26-only features. |
| Migration friction in consumer repos | Medium | Migration guide + a small codemod script (`gosdk-migrate`) that does mechanical renames (`feed.ProducerManager().AvailableProducers()` → `client.Producers(ctx)`). |
| Hidden behavior dependencies in `kollector-esport`'s out-of-order message buffer (it relies on current recovery-state semantics) | High | Run kollector against the new SDK on a staging environment for 24h+ before promoting beta to stable. Allow real migration time on the consumer side — see §13. |
| Performance regression vs current implementation | Low | The current implementation is not optimized; the new one isn't slower in any obvious way. Benchmark the per-message hot path (XML decode + market description lookup) and compare. |

## 19. Open questions

(The four reviewer-flagged decisions are now resolved in §0. The list below is what's still unresolved.)

1. **MaxInactivitySeconds default** — current is 20s. Java/.NET match. Keep, but verify with the recovery state-machine implementer that it integrates cleanly with the new tick rate.
2. ~~**InitialSnapshotTime semantics**~~ **RESOLVED:** `time.Duration` (matches Java's `Duration` shape, Go-idiomatic). Option signature: `gosdk.WithInitialSnapshotTime(d time.Duration)`. Current gosdk has no equivalent field — this is a pure parity addition for .NET/Java, no behavior to preserve.
3. ~~**Connection events on reconnect**~~ **RESOLVED:** first transition only. Sequence: `Disconnected{err}` once on drop → `Reconnecting` once when retry loop starts → per-attempt failures go to `slog.Warn(attempt=N, err)` only (no event) → `Connected` on success. A second drop *during* the reconnecting state must not re-emit `Reconnecting` — the loop just keeps going. Matches Java/.NET (which fire once, not per attempt). Polling `ConnectionState()` covers "still trying" for any consumer that needs it. A future `WithVerboseConnectionEvents()` opt-in can add per-attempt events if there's ever an ask.
4. **Replay session shape** — Java/.NET have a separate `IReplayOddsFeed` / `ReplayFeed` type. We're folding it into `Subscribe(ctx, gosdk.WithReplay())`. Confirm this is acceptable or split into a `ReplayClient` to mirror the references more closely.
5. ~~**`ExceptionHandlingStrategy` scope**~~ **RESOLVED:** message-processing only (AMQP consumer decode-and-route). Does NOT apply to `Client.X(ctx, ...)` API methods (Go's `(T, error)` is always the contract) or to background goroutines (recovery actors, reconnect — they always log-and-continue, otherwise a transient blip permanently downs a producer). See §10 Exception strategy for the full scope rule.
6. **Logger replacement at runtime** — should `client.SetLogger(*slog.Logger)` exist, or is logger immutable post-construction? Lean toward immutable.
7. **Codemod script** — worth writing or just a migration guide table?
8. **Recovery handle GC grace period** — default 1 minute is a guess. May need tuning based on consumer patterns: a consumer that polls `EventRecoveryStatus(requestID)` minutes after initiating recovery would miss a 1-minute window. Consider 5 or 10 minutes; cost is just a few entries in a map.
9. **User-facing manual-ack opt-in** — should we offer `WithManualAck()` exposing ack semantics to users (for at-least-once / exactly-once integrations on top), or leave that for v1.1+ if anyone asks? Lean toward "leave for later" — no current ask. Note this is separate from §0.6 (which is internal manual-ack, decided).
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

**Note:** Appendix A is illustrative, not exhaustive. Methods like `Player`, `MatchesFor`, `LiveMatches`, `ListMatches`, `AvailableTournaments`, `Sport(id)`, etc. follow the same mechanical mapping (manager → direct method on `Client`, with `ctx` first and `locales ...protocols.Locale` last). Likewise every config option moves from `cfg.SetX(...)` chained-setter form to `gosdk.WithX(...)` functional-option form. The full list is in §4.

## Appendix B — Verified parity gaps closed by this rewrite

(From the cross-SDK audit captured under `/Users/dsaiko/.claude/projects/.../memory/sdk_caching_localization.md` and the `next`-branch analysis.)

1. ✅ Per-call locale on every query method.
2. ✅ Per-message locale — `oddsChange.Markets()` is unchanged (no locale param, matching the existing `protocols.OddsChange` shape); locale lives on the per-market accessor `market.LocalizedName(locale)`, which reads from preloaded/prefetched cache. Returns `ErrLocaleNotAvailable` if absent (consumer must list locales in `WithPreloadLocales(...)` or prefetch via `client.MarketDescription(ctx, ...)`). No hidden synchronous I/O from message accessors.
3. ✅ Public cache invalidation on managers.
4. ✅ Wider locale enum (12 values vs current 3).
5. ✅ Maintained cache library (`golang-lru/v2` vs unmaintained `go-cache`).
6. ✅ Connection-state observability (`ConnectionEvents()`).
7. ✅ `ExceptionHandlingStrategy` config.
8. ✅ `InitialSnapshotTime` config.
9. ✅ `HTTPClientTimeout` config.
10. ✅ `SetProducerRecoveryFromTimestamp` on public surface.
11. ✅ `SelectReplay` / `SelectCustom` environment helpers.
12. ✅ Replay status query and `StopAndClear`.
13. ✅ Logger injection (`WithLogger(*slog.Logger)`).
14. ✅ `context.Context` propagation throughout.
15. ✅ API call logging — debug-level slog automatically; opt-in `APIEvent` channel with selectable verbosity (`WithAPICallLogging`). Closes Java's `onRawApiDataReceived` parity gap and the explicit .NET client ask.
