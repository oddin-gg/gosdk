package gosdk

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/cache"
	"github.com/oddin-gg/gosdk/internal/config"
	"github.com/oddin-gg/gosdk/internal/factory"
	"github.com/oddin-gg/gosdk/internal/feed"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/internal/market"
	"github.com/oddin-gg/gosdk/internal/producer"
	"github.com/oddin-gg/gosdk/internal/recovery"
	"github.com/oddin-gg/gosdk/internal/replay"
	"github.com/oddin-gg/gosdk/internal/sport"
	"github.com/oddin-gg/gosdk/internal/whoami"
	"github.com/oddin-gg/gosdk/types"
)

// clientMode is the internal lifecycle phase. The public ConnectionState
// returned from ConnectionState() is *derived* from this — kept in
// connectState (atomic.Int32) for lock-free reads, but mode is the
// authoritative value, written only under lifecycleMu.
//
// The mode answers cross-flow questions in one place that used to be
// spread across connectState + connecting + rmq.opened + subsClosed:
//   - Is the client closed?                 mode in {modeClosing, modeClosed}
//   - Is AMQP open only for replay?         mode == modeBrokerOnly
//   - Is the full feed pipeline ready?      mode == modeNormalReady
//   - Can a subscription be admitted?       mode not in {modeClosing, modeClosed}
//
// Transition graph (each edge taken under lifecycleMu):
//
//	modeNew ──ensureBroker──▶ modeBrokerOnly
//	modeNew ──ensureNormal──▶ modeNormalConnecting ──success──▶ modeNormalReady
//	modeBrokerOnly ──ensureNormal──▶ modeNormalConnecting ──success──▶ modeNormalReady
//	modeNormalConnecting ──failure──▶ modeNew or modeBrokerOnly (whichever it came from)
//	{modeNew, modeBrokerOnly, modeNormalConnecting, modeNormalReady} ──beginClose──▶ modeClosing ──runShutdown done──▶ modeClosed
type clientMode uint8

const (
	modeNew              clientMode = iota // initial; nothing open
	modeBrokerOnly                         // replay opened AMQP only; full pipeline not built
	modeNormalConnecting                   // ensureNormal in flight
	modeNormalReady                        // full pipeline ready (producers + recovery + alive session)
	modeClosing                            // Close called; runShutdown started
	modeClosed                             // runShutdown finished
)

// publicState maps a clientMode to its public ConnectionState observable.
// modeBrokerOnly maps to NotConnected because replay subscriptions
// deliberately do NOT advertise a Connected client (the full feed-SDK
// pipeline isn't initialised — only AMQP is open).
func (m clientMode) publicState() ConnectionState {
	switch m {
	case modeNew, modeBrokerOnly:
		return ConnectionStateNotConnected
	case modeNormalConnecting:
		return ConnectionStateConnecting
	case modeNormalReady:
		return ConnectionStateConnected
	case modeClosing:
		// Distinct from Closed: events.go documents Closed as "Close has
		// COMPLETED" (channels closed, subscriptions terminated). During
		// teardown those are still live, so publishing Closed here let a
		// poller observe "closed" while Done()s hadn't fired yet.
		return ConnectionStateClosing
	case modeClosed:
		return ConnectionStateClosed
	}
	return ConnectionStateNotConnected
}

// runtime is a generation-paired snapshot of the connection-layer
// pointers. ensureBroker / ensureNormal capture this under lifecycleMu
// and return it so callers (Subscribe, Connect's success path) operate
// on a coherent (rmq, rmgr) pair across the lifetime of the call. Without
// this, a concurrent Connect rollback that calls resetConnectionLayer
// between two atomic.Pointer.Load()s could otherwise leave Subscribe
// opening the old rmq while building a session against the new rmgr.
//
// The atomic.Pointer fields on Client remain — public Recover* and
// ProducerStatus methods load them directly without lifecycleMu, since
// they only need the latest, not a generation-paired pair.
type runtime struct {
	rmq  *feed.Client
	rmgr *recovery.Manager
}

// connectResult is one ensureNormal attempt's outcome record — see the
// connectAttempt field. Fields are written exactly once, before
// close(done), and are immutable afterwards.
type connectResult struct {
	done chan struct{}
	err  error
	rt   runtime
	ok   bool // attempt reached modeNormalReady
}

// Per-channel buffer sizes for the lossy event streams. Sized per
// NEXT.md §0.3 / §9.3 — RecoveryEvents largest because completion
// notifications matter most; APIEvents at the documented 256;
// ConnectionEvents tiny because transitions are rare.
// Each channel uses drop-oldest semantics on overflow.
const (
	connEventBuffer     = 32   // NEXT.md §0.3 — transitions are rare
	recoveryEventBuffer = 1024 // NEXT.md §0.3 — largest; recovery completes matter most
	apiEventBuffer      = 256  // NEXT.md §9.3 — default 256

	// maxSubscriptionBuffer caps WithSubscriptionBuffer — see
	// validateConfigBounds for the rationale (post-broker-setup
	// allocation must never panic/OOM on an accepted config).
	maxSubscriptionBuffer = 1 << 20
)

// dropWarnInterval rate-limits the slog.Warn issued when an event channel
// drops. One warning per channel per interval avoids log spam under
// sustained overflow.
const dropWarnInterval = 5 * time.Second

// snapshotKeyTemplate is the routing-key prefix for snapshot-complete
// messages. The trailing field is the SDK node id (or "-" when unset).
const snapshotKeyTemplate = "-.-.-.snapshot_complete.-.-.-."

// Client is the flat v1.0.0 SDK entry-point. It replaces the legacy
// OddsFeed + manager-of-managers shape with direct methods.
//
// Lifecycle (NEXT.md §0.1, §8):
//   - New(ctx, cfg) does API + cache + producer setup. It does NOT open AMQP.
//   - Connect(ctx) opens AMQP and starts the recovery loop. Optional —
//     Subscribe lazy-connects on first call.
//   - Subscribe(ctx, opts...) returns a *Subscription pumping messages.
//   - Close(ctx) terminates everything; idempotent. It is ABRUPT for
//     still-active subscriptions (they are aborted, not drained) — call
//     Subscription.Close(ctx) on each FIRST for a graceful drain. ctx
//     caps the wait, not the shutdown work.
//
// Concurrency: all methods are safe for concurrent use after New returns.
type Client struct {
	cfg     Config
	cfgAdpt config.Config
	logger  *log.Logger

	apiClient          *api.Client
	whoAmIManager      whoAmIManager
	producerManager    *producer.Manager
	cacheManager       *cache.Manager
	feedMessageFactory *factory.FeedMessageFactory

	// recoveryManager and rabbitMQClient are *replaced* on Connect's
	// rollback paths (see resetConnectionLayer). Reads happen on every
	// public Recover* / ProducerStatus / Subscribe call and must
	// observe a consistent pointer concurrent with the writes — so
	// they're stored as atomic.Pointer rather than plain fields.
	recoveryManager atomic.Pointer[recovery.Manager]
	rabbitMQClient  atomic.Pointer[feed.Client]

	marketDescriptionManager marketDescriptionManager
	sportsInfoManager        sportsInfoManager
	replayManager            replayManager
	replay                   *Replay

	// lifecycleMu serialises every state-machine transition: mode
	// changes, connect-in-flight bookkeeping, and subs admission.
	// connectState is its public-observable mirror — read lock-free
	// from ConnectionState() but written only under lifecycleMu so
	// readers see a consistent edge with mode.
	//
	// Per NEXT.md §0: "retryable Connect, not sync.Once" — a failed
	// first attempt returns mode to its prior value (modeNew or
	// modeBrokerOnly) so the next call retries from scratch.
	lifecycleMu  sync.Mutex
	mode         clientMode    // source of truth (under lifecycleMu)
	connectState atomic.Int32  // ConnectionState; lock-free mirror of mode for public reads
	connecting   bool          // a normal-pipeline attempt is in flight
	connectDone  chan struct{} // closed when the in-flight attempt finishes
	// connectAttempt is the IMMUTABLE result record of the attempt
	// connectDone belongs to: the owner writes err/rt/ok exactly once
	// before closing done; waiters read the record they captured under
	// lifecycleMu with no re-lock after waking. Pre-fix waiters re-read
	// the Client's mutable connectErr/mode/runtime after <-done — a
	// fresh attempt starting in that window (clearing connectErr,
	// replacing connectDone) made them observe the LATER attempt's
	// state: a generic error, or even the new attempt's success,
	// instead of their own attempt's outcome (losing errors.Is
	// classification of the real failure).
	connectAttempt *connectResult
	aliveSession   sdkOddsFeedSession
	internalCancel context.CancelFunc

	// brokerOpenWG tracks in-flight ensureBroker calls that have
	// passed mode-admission (i.e., are about to call rt.rmq.Open).
	// Add happens under lifecycleMu while mode is not modeClosing /
	// modeClosed, so beginClose's mode transition serialises against
	// it: after beginClose, no new Add can occur (ensureBroker
	// returns ErrAlreadyClosed). runShutdown waits on this WG
	// before calling rmq.Close, so an in-flight replay broker open
	// never races with shutdown's broker teardown.
	brokerOpenWG sync.WaitGroup

	// sessionSetupWG tracks in-flight Subscribe session setups (the
	// window from session.Open to admitSubscription/rollback). Add
	// happens under lifecycleMu with mode-admission — after beginClose
	// no new setup can start — and runShutdown waits on it before
	// snapshotting c.subs, so a pre-admission session can never escape
	// shutdown's join.
	sessionSetupWG sync.WaitGroup

	// connectedEmitted gates ConnectionConnected to at-most-once per
	// "up edge" — the window from any non-connected state into Connected,
	// until the next Disconnected/Reconnecting event clears it. Both
	// onFeedEvent and Connect's success path route their Connected emits
	// through emitConnConnectedOnce so consumers observe exactly one
	// ConnectionConnected per transition, regardless of whether the
	// broker dial happened during this Connect (feed-layer fires the
	// event) or during a prior replay subscription (rmq.Open is a no-op
	// during this Connect). Cleared on Disconnect/Reconnecting so the
	// next reconnect's Connected emits naturally; cleared at the top of
	// each Connect attempt so a rolled-back attempt's stale "true" can't
	// mute a fresh attempt's emit.
	connectedEmitted atomic.Bool

	// feedDown tracks feed-layer broker liveness: set on
	// EventDisconnected/EventReconnecting, cleared on EventConnected.
	// A broker drop while the pipeline is up does NOT change mode (the
	// client is still lifecycle-Connected; the feed layer reconnects
	// forever underneath) — but the ConnectionState() polling contract
	// promises reconnect visibility ("Connecting = a dial or reconnect
	// attempt is in flight"). ConnectionState() overlays this flag onto
	// the mode-derived state so a consumer that missed the lossy
	// Reconnecting event still observes the reconnect window by polling.
	// Maintained unconditionally in onFeedEvent (even in replay-only
	// mode, where it stays invisible) so a later ensureNormal that
	// reuses the already-open rmq inherits an accurate value.
	feedDown atomic.Bool

	// subscriptions tracked for shutdown propagation.
	subsMu sync.Mutex
	subs   map[uuid.UUID]*Subscription

	// Lossy event channels (NEXT.md §19.3).
	connEvents chan ConnectionEvent
	recvEvents chan RecoveryEvent
	apiEvents  chan APIEvent

	// eventsMu + eventsClosed gate ALL sends on the three event
	// channels. Senders take RLock + check the flag; runShutdown
	// takes Lock + sets the flag + closes the channels. Without
	// this, an in-flight emitter that already snapshotted the
	// `Emit` callback (under the api.Client's RLock) could send on
	// a channel runShutdown just closed — send-on-closed-channel panic.
	eventsMu     sync.RWMutex
	eventsClosed bool

	// Shutdown state.
	closeOnce sync.Once
	closed    chan struct{}
	closeErr  error
	wg        sync.WaitGroup

	// lifetimeCtx is the construction-to-Close lifetime of this client:
	// the detach root handed to the who-am-i manager (and, via
	// cache.NewManager, the root the cache layer derives its own
	// lifetime from). Cancelled by teardownPartialInit when New fails
	// and by runShutdown on Close, so detached loads started during a
	// failed construction (preload catalog fetches, the who-am-i probe)
	// are cancelled instead of running on for up to LoadTimeout against
	// a client that will never exist.
	lifetimeCtx    context.Context
	lifetimeCancel context.CancelFunc

	// Per-channel last-warning timestamp for rate-limiting drop slog.Warn.
	// Stored as UnixNano to allow lock-free CompareAndSwap-based gating.
	lastDropWarnConn atomic.Int64
	lastDropWarnRecv atomic.Int64
	lastDropWarnAPI  atomic.Int64
}

// RecoveryEventKind discriminates the populated payload on a
// RecoveryEvent. types.ProducerStatus and the event-recovery payload
// are both INTERFACES (nilable) — Kind is the documented
// discriminator; switch on it rather than nil-probing the fields.
type RecoveryEventKind int

const (
	// RecoveryEventKindUnknown is the zero value — only used as a
	// guard when reading an uninitialized RecoveryEvent. Never
	// emitted by the SDK.
	RecoveryEventKindUnknown RecoveryEventKind = iota
	// RecoveryEventKindProducerStatus marks events carrying a
	// ProducerStatus payload.
	RecoveryEventKindProducerStatus
	// RecoveryEventKindEventRecovery marks events carrying an
	// EventRecoveryMessage payload (per-event recovery completion).
	RecoveryEventKindEventRecovery
)

// String renders the kind for logs/debugging.
func (k RecoveryEventKind) String() string {
	switch k {
	case RecoveryEventKindProducerStatus:
		return "ProducerStatus"
	case RecoveryEventKindEventRecovery:
		return "EventRecovery"
	default:
		return "Unknown"
	}
}

// RecoveryEvent is the typed event delivered on RecoveryEvents().
//
// Exactly one of ProducerStatus or EventRecovery is populated; Kind
// names which. Consumers should switch on Kind rather than nil-probe
// the fields — BOTH payloads are interfaces and therefore nilable.
type RecoveryEvent struct {
	Kind           RecoveryEventKind
	ProducerStatus types.ProducerStatus
	EventRecovery  types.EventRecoveryMessage
	At             time.Time
}

// New constructs a Client. It does NOT open AMQP — call Connect or
// Subscribe to do that. The bookmaker-details API call is made eagerly
// so configuration errors surface up-front; pass a ctx with a timeout if
// you want to bound that probe.
func New(ctx context.Context, cfg Config) (*Client, error) {
	if err := validateConfig(&cfg); err != nil {
		return nil, err
	}
	c := &Client{
		cfg:        cfg,
		cfgAdpt:    newConfigAdapter(&cfg),
		subs:       make(map[uuid.UUID]*Subscription),
		connEvents: make(chan ConnectionEvent, connEventBuffer),
		recvEvents: make(chan RecoveryEvent, recoveryEventBuffer),
		apiEvents:  make(chan APIEvent, apiEventBuffer),
		closed:     make(chan struct{}),
	}
	c.mode = modeNew
	c.connectState.Store(int32(ConnectionStateNotConnected))

	// Construction-lifetime ctx (see field doc): survives the caller's
	// New deadline but dies with teardownPartialInit / Close.
	c.lifetimeCtx, c.lifetimeCancel = context.WithCancel(context.WithoutCancel(ctx))

	c.apiClient = api.NewWithLogger(c.cfgAdpt, cfg.Logger(), c.cfg.httpClientTimeout)
	if h := cfg.HTTPClient(); h != nil {
		c.apiClient.SetHTTPClient(h)
	}
	c.installAPICapture()

	// The configured logger must reach the probe path too — the
	// token-expiry warning fires from here, and the nil-logger
	// constructor would route it to slog.Default() instead of the
	// WithLogger-supplied sink.
	c.whoAmIManager = whoami.NewManagerWithLogger(c.lifetimeCtx, c.cfgAdpt, c.apiClient, cfg.Logger()) //nolint:contextcheck // lifetimeCtx is derived from ctx above; it is the construction-to-Close cancellation root
	details, err := c.whoAmIManager.BookmakerDetails(ctx)
	if err != nil {
		c.teardownPartialInit(ctx)
		return nil, fmt.Errorf("gosdk: who-am-i probe: %w", err)
	}

	logger := cfg.Logger()
	c.logger = log.New(logger).WithField("client_id", details.BookmakerID())

	c.producerManager = producer.NewManager(c.cfgAdpt, c.apiClient, c.logger)
	// The cache manager derives its own lifetime from the client's, so
	// both teardownPartialInit (via lifetimeCancel) and Manager.Close
	// cancel in-flight detached loads.
	c.cacheManager = cache.NewManager(c.lifetimeCtx, c.apiClient, c.cfgAdpt, c.logger, c.cfg.preloadLocales) //nolint:contextcheck // lifetimeCtx is derived from ctx above; it is the construction-to-Close cancellation root

	entityFactory := factory.NewEntityFactory(c.cacheManager)
	marketDescriptionFactory := factory.NewMarketDescriptionFactory(
		c.cacheManager.MarketDescriptionCache,
		c.cacheManager.MarketVoidReasonsCache,
		c.cacheManager.PlayersCache,
		c.cacheManager.CompetitorCache,
	)
	marketDataFactory := factory.NewMarketDataFactory(c.cfgAdpt, marketDescriptionFactory)
	// MarketFactory's locales drive AMQP message-build name resolution.
	// Default locale is always first (the primary), then any preload
	// locales follow — both are filled in the cache on first build.
	marketFactoryLocales := []types.Locale{c.cfg.DefaultLocale()}
	for _, l := range c.cfg.preloadLocales {
		if l == c.cfg.DefaultLocale() {
			continue
		}
		marketFactoryLocales = append(marketFactoryLocales, l)
	}
	marketFactory := factory.NewMarketFactory(
		marketDataFactory,
		marketFactoryLocales,
		c.logger,
	)
	c.feedMessageFactory = factory.NewFeedMessageFactory(
		entityFactory,
		marketFactory,
		c.producerManager,
		c.cfgAdpt,
		c.logger,
	)

	c.marketDescriptionManager = market.NewManager(c.cacheManager, marketDescriptionFactory, c.cfgAdpt)
	c.sportsInfoManager = sport.NewManager(entityFactory, c.apiClient, c.cacheManager, c.cfgAdpt)
	c.replayManager = replay.NewManager(c.apiClient, c.cfgAdpt, c.sportsInfoManager)
	c.replay = &Replay{client: c}

	// Eager static-catalog warm (NEXT.md §8 step 6): WithPreloadLocales
	// promises the sports + market-description catalogs are fetched
	// up-front for every listed locale, moving that HTTP latency out of
	// first-message processing. Warmed at the CACHE layer on purpose —
	// the manager-level Sports() would fan out one tournament fetch per
	// sport via BuildSport, turning a two-request warm into ~50 per
	// locale. A warm failure FAILS New (§8: "If any step fails, partial
	// state is torn down and an error is returned") — the same
	// fail-fast contract as the who-am-i probe, and the only behaviour
	// under which the preload promise is actually kept: warning and
	// continuing would silently move the fetch (and its failure mode)
	// back onto first-message processing. Bounded by the caller's ctx.
	if len(c.cfg.preloadLocales) > 0 {
		if _, err := c.cacheManager.SportDataCache.Sports(ctx, marketFactoryLocales); err != nil {
			c.teardownPartialInit(ctx)
			return nil, fmt.Errorf("gosdk: preload sports catalog (locales=%v): %w", marketFactoryLocales, err)
		}
		if _, err := c.cacheManager.MarketDescriptionCache.MultiLocalizedMarketDescriptions(ctx, marketFactoryLocales); err != nil {
			c.teardownPartialInit(ctx)
			return nil, fmt.Errorf("gosdk: preload market descriptions (locales=%v): %w", marketFactoryLocales, err)
		}
	}

	// Connection-layer components (rabbitMQClient, recoveryManager) are
	// one-shot in their internals — Open/Close transitions can't be
	// undone. We construct fresh instances here for the first attempt;
	// any failed Connect rollback path calls resetConnectionLayer to
	// re-construct them so a retry sees a clean state.
	c.resetConnectionLayer()

	return c, nil
}

// teardownPartialInit releases whatever New allocated before a failed
// construction step (NEXT.md §8: "If any step fails, partial state is
// torn down and an error is returned"). Nil-checks every component so
// it is safe from any point in the construction sequence. ctx is New's
// caller ctx — its VALUES flow through, but its cancellation is severed
// (WithoutCancel): the teardown must run even when that ctx expiring is
// exactly why we're here.
func (c *Client) teardownPartialInit(ctx context.Context) {
	// Cancel the construction lifetime FIRST: any detached load still
	// in flight (who-am-i probe, preload catalog fetch) aborts its
	// HTTP request now instead of running up to LoadTimeout against a
	// client New will never return.
	if c.lifetimeCancel != nil {
		c.lifetimeCancel()
	}
	if c.cacheManager != nil {
		// Bounded, Background-rooted: construction failed, the caller's
		// ctx may already be dead, and teardown must still run.
		tearCtx, tearCancel := context.WithTimeout(context.Background(), c.cfg.shutdownTimeout)
		c.cacheManager.CloseCtx(tearCtx) //nolint:contextcheck // intentional Background root (see above)
		tearCancel()
	}
	if c.apiClient != nil {
		// Bounded join: even during construction teardown, a custom
		// HTTP transport that ignores ctx cancellation must not hang
		// New's error return past the configured shutdown budget.
		closeCtx, closeCancel := context.WithTimeout(context.WithoutCancel(ctx), c.cfg.shutdownTimeout)
		defer closeCancel()
		c.apiClient.CloseCtx(closeCtx)
	}
}

// validateConfig rejects configurations that cannot possibly work,
// wrapping ErrInvalidConfig so callers can errors.Is (NEXT.md §12:
// validation happens in New, not NewConfig). Endpoint resolution is
// checked up-front so a bad environment/region combination surfaces as
// a config error instead of a confusing who-am-i probe failure.
func validateConfig(cfg *Config) error {
	if cfg.accessToken == "" {
		return fmt.Errorf("%w: access token is required", ErrInvalidConfig)
	}
	if err := validateRegion(cfg.selectedRegion); err != nil {
		return err
	}
	adpt := newConfigAdapter(cfg)
	if _, err := adpt.APIURL(); err != nil {
		return fmt.Errorf("%w: resolve api endpoint: %w", ErrInvalidConfig, err)
	}
	if _, err := adpt.MQURL(); err != nil {
		return fmt.Errorf("%w: resolve mq endpoint: %w", ErrInvalidConfig, err)
	}
	// A forced API host may carry an explicit :port (it goes straight
	// into the API URL); the MQ host must NOT, because the dialer appends
	// WithMessagingPort — a "host:port" MQ value would build
	// "host:port:port" and fail every connection.
	if err := validateForcedHost("WithAPIHost", cfg.forcedAPIHost, true); err != nil {
		return err
	}
	if err := validateForcedHost("WithMQHost", cfg.forcedMQHost, false); err != nil {
		return err
	}
	return validateConfigBounds(cfg)
}

// validateRegion structurally validates the WithRegion value BEFORE it is
// interpolated into the derived endpoint authorities
// (types.Environment.APIEndpoint builds e.g.
// "api-mq.integration.<region>oddin.gg"). Region is an open string —
// custom/new regions are allowed — but it MUST be either empty (default
// region) or a dot-TERMINATED sequence of valid DNS labels
// ("ap-southeast-1."). Anything else is an authority-injection vector:
// a crafted region like "x@evil.example/" turns the derived URL into
// https://api-mq.integration.x@evil.example/oddin.gg... — net/url reads
// the prefix as USERINFO and evil.example as the HOST, and New's
// synchronous who-am-i probe would send X-Access-Token to the
// attacker's TLS host. The trailing-dot requirement is itself
// security-relevant: "eu" (no dot) would silently retarget the base
// domain to "euoddin.gg".
func validateRegion(region types.Region) error {
	r := string(region)
	if r == "" {
		return nil
	}
	if !strings.HasSuffix(r, ".") {
		return fmt.Errorf("%w: WithRegion %q must end with '.' (e.g. \"ap-southeast-1.\") — it is a DNS-label prefix of the endpoint host", ErrInvalidConfig, r)
	}
	// isValidHostname permits exactly one trailing root dot, so the
	// dot-terminated region validates directly: every label must be
	// 1..63 chars of [A-Za-z0-9-] with no leading/trailing hyphen —
	// which structurally excludes every authority delimiter
	// ('@', '/', '?', '#', ':', whitespace, …).
	if !isValidHostname(r) {
		return fmt.Errorf("%w: WithRegion %q is not a valid DNS-label prefix", ErrInvalidConfig, r)
	}
	return nil
}

// validateForcedHost structurally validates a forced host override. Empty
// means "derive from environment/region" and is left alone. A scheme is
// already rejected at option-application time (stripOrRejectScheme); this
// catches the remaining ways a value can be malformed. When allowPort is
// false (MQ), an embedded port is rejected; when true (API), a port is
// allowed but must be numeric and in range. The host/IP literal itself
// is validated, so colon-bearing garbage (repeated ports, unbracketed
// IPv6) and malformed IPv6 brackets are rejected rather than passing
// through to fail later at URL parse or dial time.
func validateForcedHost(opt, host string, allowPort bool) error {
	if host == "" {
		return nil
	}
	if strings.ContainsAny(host, " \t\r\n\v\f") {
		return fmt.Errorf("%w: %s %q must not contain whitespace", ErrInvalidConfig, opt, host)
	}
	// '/', '?', '#' are URL path/query/fragment; '@' is userinfo. None
	// belong in a bare host.
	if i := strings.IndexAny(host, "/?#@"); i >= 0 {
		return fmt.Errorf("%w: %s %q must be a bare host, not a URL (found %q)", ErrInvalidConfig, opt, host, host[i:i+1])
	}

	var hostLiteral string
	h, port, err := net.SplitHostPort(host)
	switch {
	case err == nil:
		// An explicit :port is present (SplitHostPort strips the brackets
		// around an IPv6, so h is the bare literal).
		if !allowPort {
			return fmt.Errorf("%w: %s %q must not include a port; set the port via WithMessagingPort", ErrInvalidConfig, opt, host)
		}
		// net/url permits ONLY ASCII digits in a port, so reject a sign
		// or other stray character up front (strconv.Atoi would accept
		// "+443"/"-443" and let it fail later at URL construction).
		if !isAllDigits(port) {
			return fmt.Errorf("%w: %s %q has a non-numeric port %q", ErrInvalidConfig, opt, host, port)
		}
		p, perr := strconv.Atoi(port)
		if perr != nil || p < 1 || p > 65535 {
			return fmt.Errorf("%w: %s %q has an invalid port %q (want 1..65535)", ErrInvalidConfig, opt, host, port)
		}
		hostLiteral = h
	default:
		// SplitHostPort fails for MORE than just "no port": "too many
		// colons" (unbracketed IPv6 / repeated ports) and "missing ']'"
		// (malformed brackets) are malformed inputs, not bare hosts. Only
		// the "missing port in address" error means a genuine no-port host.
		var addrErr *net.AddrError
		if !errors.As(err, &addrErr) || addrErr.Err != "missing port in address" {
			return fmt.Errorf("%w: %s %q is not a valid host: %w", ErrInvalidConfig, opt, host, err)
		}
		// Bare host, no port. A bracketed value must be a valid IPv6.
		if strings.HasPrefix(host, "[") {
			if !strings.HasSuffix(host, "]") {
				return fmt.Errorf("%w: %s %q has malformed IPv6 brackets", ErrInvalidConfig, opt, host)
			}
			inner := host[1 : len(host)-1]
			if ip := net.ParseIP(inner); ip == nil || ip.To4() != nil {
				return fmt.Errorf("%w: %s %q is not a valid bracketed IPv6 address", ErrInvalidConfig, opt, host)
			}
			return nil
		}
		hostLiteral = host
	}

	// hostLiteral is a hostname or an unbracketed IP literal.
	if net.ParseIP(hostLiteral) != nil {
		return nil
	}
	if !isValidHostname(hostLiteral) {
		return fmt.Errorf("%w: %s %q is not a valid hostname or IP address", ErrInvalidConfig, opt, host)
	}
	return nil
}

// isAllDigits reports whether s is non-empty and every byte is an ASCII
// digit — matching net/url's port rule (0-9 only, no sign).
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// isValidHostname reports whether h is a syntactically valid DNS
// hostname: 1..253 chars total, dot-separated labels of 1..63 chars,
// each label alphanumeric or hyphen with no leading/trailing hyphen. A
// single trailing dot (absolute/FQDN form, e.g. "api.example.com.") is
// permitted — net/url and DNS both accept it.
func isValidHostname(h string) bool {
	if h == "" || len(h) > 254 {
		return false
	}
	// Permit exactly one trailing root dot; more than one leaves an
	// empty final label and is rejected below.
	h = strings.TrimSuffix(h, ".")
	if h == "" {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			case c == '-':
				if i == 0 || i == len(label)-1 {
					return false
				}
			default:
				return false
			}
		}
	}
	return true
}

// validateConfigBounds rejects out-of-range numeric / enum / required-
// string options up front so they surface as ErrInvalidConfig from New
// rather than as confusing runtime failures later (a negative inactivity
// marks producers down immediately; a non-positive shutdown timeout
// makes graceful Close deadline-fail at once; an out-of-range port fails
// only when AMQP dials). Zero-semantics per option:
//   - durations with a NewConfig default MUST be > 0 (the default is
//     already applied; an explicit 0/negative is a mistake);
//   - initialSnapshotTime is "0 = use default", so only < 0 is rejected;
//   - sizes (prefetch, buffer, body limit) are "0 = default/none", so
//     only < 0 is rejected;
//   - port must be in 1..65535; enums must be a defined value; the
//     default locale and exchange/prefix names must be non-empty.
func validateConfigBounds(cfg *Config) error {
	type durCheck struct {
		name string
		d    time.Duration
	}
	for _, c := range []durCheck{
		{"WithMaxInactivity", cfg.maxInactivity},
		{"WithMaxRecoveryExecution", cfg.maxRecoveryExecution},
		{"WithHTTPClientTimeout", cfg.httpClientTimeout},
		{"WithShutdownTimeout", cfg.shutdownTimeout},
	} {
		if c.d <= 0 {
			return fmt.Errorf("%w: %s must be > 0, got %v", ErrInvalidConfig, c.name, c.d)
		}
	}
	if cfg.initialSnapshotTime < 0 {
		return fmt.Errorf("%w: WithInitialSnapshotTime must be >= 0, got %v", ErrInvalidConfig, cfg.initialSnapshotTime)
	}
	if cfg.messagingPort < 1 || cfg.messagingPort > 65535 {
		return fmt.Errorf("%w: WithMessagingPort must be in 1..65535, got %d", ErrInvalidConfig, cfg.messagingPort)
	}
	type sizeCheck struct {
		name string
		n    int
	}
	for _, c := range []sizeCheck{
		{"WithAMQPPrefetch", cfg.amqpPrefetch},
		{"WithSubscriptionBuffer", cfg.subscriptionBuffer},
		{"WithAPICallBodyLimit", cfg.apiCallBodyLimit},
	} {
		if c.n < 0 {
			return fmt.Errorf("%w: %s must be >= 0, got %d", ErrInvalidConfig, c.name, c.n)
		}
	}
	// Upper bound is protocol-level: AMQP basic.qos carries the count as
	// uint16, and amqp091-go truncates the int — 65536 wraps to protocol
	// value 0, which means UNLIMITED prefetch (the exact opposite of the
	// documented backpressure bound: a stalled consumer would let the
	// dependency buffer deliveries in an unbounded slice), and 65537
	// wraps to 1. Reject anything a uint16 can't carry. (0 stays valid:
	// the SDK normalizes it to the default before it reaches Qos.)
	if cfg.amqpPrefetch > 65535 {
		return fmt.Errorf("%w: WithAMQPPrefetch must be in 0..65535 (AMQP basic.qos carries prefetch as uint16), got %d", ErrInvalidConfig, cfg.amqpPrefetch)
	}
	// Practical ceiling: the buffer is allocated per subscription AFTER
	// broker setup (make(chan types.SessionMessage, n)) — an extreme
	// accepted value panicked or OOMed only once external side effects
	// (queue, bindings) already existed. 2^20 messages is far beyond any
	// sane consumer lag budget.
	if cfg.subscriptionBuffer > maxSubscriptionBuffer {
		return fmt.Errorf("%w: WithSubscriptionBuffer must be <= %d, got %d", ErrInvalidConfig, maxSubscriptionBuffer, cfg.subscriptionBuffer)
	}
	if cfg.exceptionStrategy != StrategyCatch && cfg.exceptionStrategy != StrategyThrow {
		return fmt.Errorf("%w: WithExceptionStrategy has unknown value %d", ErrInvalidConfig, cfg.exceptionStrategy)
	}
	if cfg.apiCallLogging < APILogOff || cfg.apiCallLogging > APILogFull {
		return fmt.Errorf("%w: WithAPICallLogging has unknown value %d", ErrInvalidConfig, cfg.apiCallLogging)
	}
	if cfg.defaultLocale == "" {
		return fmt.Errorf("%w: WithDefaultLocale must not be empty", ErrInvalidConfig)
	}
	if cfg.httpClient != nil && cfg.httpClient.Timeout <= 0 {
		// Belt-and-braces: the option panics on a zero Timeout and the
		// stored client is a private copy, but revalidate the ACTUAL
		// stored value so no construction path can smuggle in an
		// unbounded client.
		return fmt.Errorf("%w: WithHTTPClient client must have Timeout > 0", ErrInvalidConfig)
	}
	for _, l := range cfg.preloadLocales {
		if l == "" {
			// An empty preload locale slipped past validation and made
			// New issue malformed catalog requests (/sports//...).
			return fmt.Errorf("%w: WithPreloadLocales contains an empty locale", ErrInvalidConfig)
		}
	}
	type strCheck struct {
		name, v string
	}
	for _, c := range []strCheck{
		{"WithExchangeName", cfg.exchangeName},
		{"WithReplayExchangeName", cfg.replayExchangeName},
		{"WithSportIDPrefix", cfg.sportIDPrefix},
	} {
		if c.v == "" {
			return fmt.Errorf("%w: %s must not be empty", ErrInvalidConfig, c.name)
		}
	}
	// The sport-id prefix is concatenated with a numeric routing-key
	// segment and parsed as a URN on EVERY feed delivery (parseRoute).
	// A malformed prefix (e.g. "sport:") passes the non-empty check but
	// makes every ordinary delivery unparsable — and under StrategyCatch
	// those surface as UnparsableMessage and are ACKed, so the breakage
	// is silent and unrecoverable. Require the exact two-component
	// "prefix:type:" shape: appending an id must parse, and the id must
	// round-trip untouched (u.ID==0 plus the trailing-colon check reject
	// prefixes that end in digits, e.g. "od:sport:0", which would
	// silently rewrite every sport id by concatenation).
	if u, err := types.ParseURN(cfg.sportIDPrefix + "0"); err != nil || u.ID != 0 || !strings.HasSuffix(cfg.sportIDPrefix, ":") {
		return fmt.Errorf("%w: WithSportIDPrefix %q must have the form \"prefix:type:\" (e.g. \"od:sport:\") so that appending a numeric id yields a valid URN", ErrInvalidConfig, cfg.sportIDPrefix)
	}
	// Node ids become routing-key segments (snapshot_complete.<id>,
	// base.<id>.#) and node_id query parameters for recovery/replay —
	// the server protocol has no notion of a negative node.
	if cfg.sdkNodeID != nil && *cfg.sdkNodeID < 0 {
		return fmt.Errorf("%w: WithNodeID must be >= 0, got %d", ErrInvalidConfig, *cfg.sdkNodeID)
	}
	return nil
}

// resetConnectionLayer (re)creates the one-shot rabbitMQClient +
// recoveryManager. Called once from New for the first attempt and
// again from ensure* rollback paths after a transient failure left
// either component in its terminal Closed state.
//
// Safe to call under lifecycleMu (the typical rollback case) or before
// any goroutines exist (New). Concurrent reads from public Recover* /
// ProducerStatus see either generation atomically — that's tolerable
// because those calls only need the latest, not a generation-paired
// pair (Subscribe and Connect get coherent pairs via runtime snapshots).
func (c *Client) resetConnectionLayer() {
	c.recoveryManager.Store(recovery.NewManager(c.cfgAdpt, c.producerManager, c.apiClient, c.logger, c.cfg.initialSnapshotTime))
	rmq := feed.NewClient(c.cfgAdpt, c.whoAmIManager, c.logger)
	rmq.SetEventEmitter(c.onFeedEvent)
	c.rabbitMQClient.Store(rmq)
}

// setMode is the only writer of mode + connectState. Caller MUST hold
// lifecycleMu. The atomic.Int32 mirror is updated alongside so
// ConnectionState() readers see a coherent edge with the mode change.
func (c *Client) setMode(m clientMode) {
	c.mode = m
	c.connectState.Store(int32(m.publicState()))
}

// rollbackPartialNormal undoes a partially-built ensureNormal pipeline
// without disturbing replay subscriptions that share the broker.
//
// Honors priorMode:
//   - priorMode == modeBrokerOnly: rmq stays open and is NOT replaced.
//     Existing replay subscriptions remain functional. Only the
//     normal-only pieces (alive session, recovery manager, internal
//     ctx) are torn down; recoveryManager is replaced with a fresh
//     instance so a retry ensureNormal sees clean state.
//   - priorMode == modeNew: full reset. rmq is closed and the
//     connection layer is recreated for a clean retry.
//
// alive is closed only if non-nil — pass the local newSession() result
// from the alive-Open failure path; pass nil from the recovery-Open
// path. session.Close is safe even if Open failed (closeOnce + nil
// checks on closeFn/done; ChannelConsumer.Close is idempotent).
//
// rmgr is always closed when non-nil. Pass the local
// recoveryManager.Load() from the attempt; pass nil from the rmq.Open
// failure path (rmgr was never opened).
func (c *Client) rollbackPartialNormal(
	ctx context.Context,
	alive sdkOddsFeedSession,
	rmgr *recovery.Manager,
	rmq *feed.Client,
	priorMode clientMode,
) {
	// Read-and-clear under lifecycleMu like every other internalCancel
	// access: an attempt in rollback never owns a PUBLISHED
	// internalCancel (publication happens only on the modeNormalReady
	// path, and the attempt's own cancel is still held locally by the
	// cancelPublished defer), so both sides only ever see nil today —
	// but without the lock there is no happens-before edge against
	// runShutdown's read after a shutdown-timeout race, and the
	// invariant keeping the bare access benign is one refactor away
	// from breaking.
	c.lifecycleMu.Lock()
	if c.internalCancel != nil {
		c.internalCancel()
		c.internalCancel = nil
	}
	c.lifecycleMu.Unlock()

	shutdownCtx, sCancel := context.WithTimeout(context.WithoutCancel(ctx), c.cfg.shutdownTimeout)
	defer sCancel()

	if alive != nil {
		alive.Close(shutdownCtx)
	}
	if rmgr != nil {
		// Bounded by the rollback's shutdown budget — same rationale as
		// runShutdown: a wedged actor must not pin the rollback.
		rmgr.CloseCtx(shutdownCtx)
	}

	if priorMode == modeBrokerOnly {
		// rmq stays — replay subscriptions are still attached to it via
		// their session's channelConsumer. Replace ONLY the
		// recovery.Manager (it's normal-only; replay never sees it).
		c.lifecycleMu.Lock()
		if c.mode != modeClosing && c.mode != modeClosed {
			c.recoveryManager.Store(recovery.NewManager(c.cfgAdpt, c.producerManager, c.apiClient, c.logger, c.cfg.initialSnapshotTime))
		}
		c.lifecycleMu.Unlock()
		return
	}

	// priorMode == modeNew: full reset. Close rmq bounded by
	// cfg.shutdownTimeout so a stuck broker doesn't hang the rollback.
	_ = rmq.Close(shutdownCtx)
	c.lifecycleMu.Lock()
	// NEVER install a fresh connection generation once Closing/Closed:
	// runShutdown's one-shot teardown has already run (or is running) —
	// nothing would ever close the replacements, and a rollback resumed
	// AFTER a timed-out shutdown would mutate a client that has already
	// published Closed. The terminal generation stays in place; retry
	// paths are gone anyway (Connect after Close is rejected at
	// admission).
	if c.mode != modeClosing && c.mode != modeClosed {
		c.resetConnectionLayer()
	}
	c.lifecycleMu.Unlock()
}

// snapshotRuntime captures (rmq, rmgr) under the caller's lifecycleMu.
// Returned to ensureBroker / ensureNormal callers so all subsequent
// reads in the flow operate on the same generation, even if a
// concurrent rollback swaps the atomic pointers.
func (c *Client) snapshotRuntime() runtime {
	return runtime{
		rmq:  c.rabbitMQClient.Load(),
		rmgr: c.recoveryManager.Load(),
	}
}

// ensureBroker is the replay path's lifecycle entry. It guarantees
// AMQP is open without standing up the producer/recovery/alive-session
// pipeline. Idempotent: multiple replay subscriptions share one
// underlying feed.Client. Compatible with ensureNormal: if a normal
// Connect is in flight or done, the existing rmq is reused.
func (c *Client) ensureBroker(ctx context.Context) (runtime, error) {
	c.lifecycleMu.Lock()
	// Wait for any in-flight ensureNormal to settle before adopting rmq.
	//
	// Why: a normal Connect that fails at recovery/alive setup runs
	// rollbackPartialNormal, which (when priorMode == modeNew) closes
	// rmq and replaces it via resetConnectionLayer. If we proceeded
	// during modeNormalConnecting, we would adopt that rmq, build a
	// session against it, possibly admit a subscription — and then the
	// rollback would tear it out from under us. Sequencing ensureBroker
	// after the in-flight attempt settles guarantees we see the
	// post-rollback (or post-success) mode + rmq, never an
	// about-to-be-replaced one.
	//
	// Loop, not single-wait: a fresh ensureNormal could start between
	// the wakeup and the re-acquire, putting us back into
	// modeNormalConnecting. The loop drains all consecutive attempts.
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
	var transitioned bool
	switch c.mode {
	case modeClosing, modeClosed:
		c.lifecycleMu.Unlock()
		return runtime{}, ErrAlreadyClosed
	case modeBrokerOnly, modeNormalReady:
		// AMQP already up. rmq.Open below is idempotent.
	case modeNew:
		c.setMode(modeBrokerOnly)
		transitioned = true
	case modeNormalConnecting:
		// Unreachable: the wait loop above drained this state.
	}
	rt := c.snapshotRuntime()
	// Add under lifecycleMu while mode is not Closing/Closed — pairs
	// with beginClose's mode transition + runShutdown's brokerOpenWG.Wait
	// to fence broker open against shutdown's broker teardown.
	c.brokerOpenWG.Add(1)
	c.lifecycleMu.Unlock()
	defer c.brokerOpenWG.Done()

	// Make the broker dial cancellable by the client lifetime: shutdown
	// cancels lifetimeCtx (runShutdown, top), aborting an in-flight dial
	// instead of letting it run for feed.Client's 30s fallback and blow
	// past WithShutdownTimeout. The caller's ctx still bounds it too.
	openCtx, cancelOpen := context.WithCancel(ctx)
	defer cancelOpen()
	//nolint:contextcheck // lifetimeCtx is the client-lifetime cancellation root wired to abort this dial on shutdown, not a request parent
	stopLife := context.AfterFunc(c.lifetimeCtx, cancelOpen)
	defer stopLife()

	openErr := rt.rmq.Open(openCtx)

	// Reconcile the root mode against the ACTUAL broker state, resolving
	// concurrent ensureBroker generations regardless of the order their
	// lifecycleMu sections interleave. RETAIN the connection layer on
	// failure — do NOT resetConnectionLayer: a failed feed Open leaves the
	// client retryable-unopened (round-8 fix), and replacing it would
	// orphan a concurrent caller's still-in-flight open of the same
	// generation.
	c.lifecycleMu.Lock()
	c.reconcileBrokerModeLocked(transitioned, rt.rmq.IsOpen())
	c.lifecycleMu.Unlock()

	if openErr != nil {
		return runtime{}, fmt.Errorf("gosdk: amqp open (replay): %w", openErr)
	}
	return rt, nil
}

// reconcileBrokerModeLocked repairs the root mode after a broker-open
// attempt settles. Caller holds lifecycleMu. It fixes the two-generation
// race where an older FAILED ensureBroker's rollback and a newer
// SUCCESSFUL one interleave:
//
//   - `transitioned` = this call performed the modeNew→modeBrokerOnly
//     transition (only one concurrent call can, it's one-way).
//   - `opened` = the shared feed client is now open (rt.rmq.IsOpen()).
//
// The decision keys on the ACTUAL broker state, not on which call
// happens to hold the lock first:
//
//   - broker open + mode drifted to modeNew (a peer's failed rollback
//     landed): restore modeBrokerOnly so the root reflects the live
//     broker — otherwise a later normal Connect captures priorMode=New
//     and its rollback would close the broker out from under live replay
//     subscriptions.
//   - broker NOT open + we made the transition + still modeBrokerOnly:
//     our own attempt failed and no peer holds it open — undo the
//     transition.
//   - Closing/Closed: shutdown owns the mode; never touch it. Ready:
//     a normal Connect owns it; never stomp to brokerOnly.
func (c *Client) reconcileBrokerModeLocked(transitioned, opened bool) {
	switch {
	case c.mode == modeClosing || c.mode == modeClosed:
		return
	case opened && c.mode == modeNew:
		c.setMode(modeBrokerOnly)
	case !opened && transitioned && c.mode == modeBrokerOnly:
		c.setMode(modeNew)
	}
}

// ensureNormal is the full-pipeline lifecycle entry — implements
// Connect's body and the lazy-connect branch of normal Subscribe.
// Returns a generation-paired runtime snapshot on success.
//
// Serialisation: at most one in-flight attempt; concurrent callers
// wait on connectDone and observe its outcome. Idempotent on
// modeNormalReady: returns the current runtime, no work.
func (c *Client) ensureNormal(ctx context.Context) (rt runtime, err error) {
	c.lifecycleMu.Lock()
	switch c.mode {
	case modeNormalReady:
		rt = c.snapshotRuntime()
		c.lifecycleMu.Unlock()
		return rt, nil
	case modeClosing, modeClosed:
		c.lifecycleMu.Unlock()
		return runtime{}, ErrAlreadyClosed
	case modeNew, modeBrokerOnly, modeNormalConnecting:
		// Fall through: either start a fresh attempt or join the
		// in-flight one (gated on c.connecting below).
	}
	if c.connecting {
		// Concurrent caller — capture THIS attempt's immutable result
		// record, wait for it, and read the record; never the Client's
		// mutable state, which a fresh attempt may already have
		// replaced by the time we wake (see connectAttempt).
		res := c.connectAttempt
		c.lifecycleMu.Unlock()
		select {
		case <-res.done:
		case <-ctx.Done():
			return runtime{}, ctx.Err()
		}
		if res.err != nil {
			return runtime{}, res.err
		}
		if res.ok {
			return res.rt, nil
		}
		return runtime{}, errors.New("gosdk: connect attempt did not yield a connection")
	}
	// Fresh attempt: take the Connecting transition. Remember which
	// state we came from so the rollback defer can restore it (a
	// rolled-back attempt that started from modeBrokerOnly must not
	// stomp the broker-only state back to modeNew, since rmq is still
	// open and replay subscriptions still depend on it).
	priorMode := c.mode // modeNew or modeBrokerOnly
	c.connecting = true
	res := &connectResult{done: make(chan struct{})}
	c.connectAttempt = res
	c.connectDone = res.done
	// Arm the up-edge gate while public state is still NotConnected
	// (so a pending feed-layer event is gated out by onFeedEvent's
	// NotConnected check) and BEFORE the Connecting transition opens
	// the gate. A rolled-back prior attempt may have left the flag at
	// true; clearing here ensures the fresh attempt's Connected emit
	// (whether feed-layer or this function's explicit) lands.
	c.connectedEmitted.Store(false)
	c.setMode(modeNormalConnecting)
	rmq := c.rabbitMQClient.Load()
	c.lifecycleMu.Unlock()

	settled := false
	defer func() {
		c.lifecycleMu.Lock()
		c.connecting = false
		if !settled {
			// Failure-path revert: only reverse our own transition.
			// If a concurrent Close stomped to modeClosing/Closed, do
			// NOT undo it.
			if c.mode == modeNormalConnecting {
				c.setMode(priorMode)
			}
		}
		// Publish this attempt's outcome on ITS record — written once,
		// before close(done), immutable afterwards (see connectAttempt).
		res.err = err
		res.rt = rt
		res.ok = settled && err == nil
		close(res.done)
		c.lifecycleMu.Unlock()
	}()

	if err = c.producerManager.Open(ctx); err != nil {
		err = fmt.Errorf("gosdk: producer init: %w", err)
		return runtime{}, err
	}

	// Make the broker dial cancellable by the client lifetime so shutdown
	// aborts it (beginClose cancels lifetimeCtx) instead of letting it run
	// for feed.Client's 30s dial fallback past WithShutdownTimeout. The
	// caller's ctx still bounds it too.
	openCtx, cancelOpen := context.WithCancel(ctx)
	defer cancelOpen()
	//nolint:contextcheck // lifetimeCtx is the client-lifetime cancellation root wired to abort this dial on shutdown, not a request parent
	stopLife := context.AfterFunc(c.lifetimeCtx, cancelOpen)
	defer stopLife()
	if err = rmq.Open(openCtx); err != nil {
		// In practice unreachable from priorMode == modeBrokerOnly
		// (rmq.Open is a no-op once rmq.opened is true), but we still
		// route through rollbackPartialNormal which honors priorMode:
		// modeBrokerOnly leaves rmq alone, modeNew tears it down +
		// resets the connection layer for a clean retry.
		c.rollbackPartialNormal(ctx, nil, nil, rmq, priorMode)
		err = fmt.Errorf("gosdk: amqp open: %w", err)
		return runtime{}, err
	}

	// internalCtx is the lifetime ctx for the pumpRecovery goroutine
	// and is cancelled on Close (via internalCancel). It's NOT given
	// to recoveryManager.Open — Open derives its own actor lifecycle
	// ctx internally and uses the user-bounded ctx for its bootstrap
	// HTTP fetch (active-producer list). Pre-fix this same internal
	// ctx was passed to Open, severing the user's Subscribe timeout
	// from the bootstrap fetch.
	//
	// ATTEMPT-OWNED until the gated publication below: c.internalCancel
	// is written only under lifecycleMu while this attempt still owns
	// modeNormalConnecting. Publishing it here (pre-fix) mutated root
	// state that a timed-out shutdown may already have torn down. The
	// ownership defer releases the ctx on every path that does NOT
	// publish it (failure paths, Close-raced tail); publication
	// transfers ownership to runShutdown.
	internalCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	cancelPublished := false
	defer func() {
		if !cancelPublished {
			cancel()
		}
	}()

	rmgr := c.recoveryManager.Load()
	recoveryCh, recErr := rmgr.Open(ctx)
	if recErr != nil {
		// AMQP is up; rollback the normal-only pieces. When
		// priorMode == modeBrokerOnly, rmq is preserved so existing
		// replay subscriptions keep working; only recoveryManager is
		// replaced. When priorMode == modeNew, full reset.
		c.rollbackPartialNormal(ctx, nil, rmgr, rmq, priorMode)
		err = fmt.Errorf("gosdk: recovery open: %w", recErr)
		return runtime{}, err
	}

	// Internal alive-only session — drives the recovery state machine.
	alive := newSession(
		rmq,
		c.producerManager,
		c.cacheManager,
		c.feedMessageFactory,
		rmgr,
		c.cfg.exchangeName,
		c.cfg.sportIDPrefix,
		false,
		c.logger,
		StrategyCatch,
		c.cfg.amqpPrefetch,
	)
	aliveInterest := types.SystemAliveOnly
	if aErr := alive.Open(ctx, []string{string(types.SystemAliveOnly)}, &aliveInterest, false); aErr != nil {
		// Same priorMode-aware rollback as recovery-open failure,
		// plus close the locally-opened alive session. session.Close
		// after a failed Open is safe (closeOnce + nil checks).
		c.rollbackPartialNormal(ctx, alive, rmgr, rmq, priorMode)
		err = fmt.Errorf("gosdk: alive session open: %w", aErr)
		return runtime{}, err
	}

	settled = true
	c.lifecycleMu.Lock()
	// Race guard + attempt-owned publication: only while this attempt
	// still owns the modeNormalConnecting → modeNormalReady edge may it
	// publish root resources (aliveSession, internalCancel) or admit
	// goroutines to the root WaitGroup. If Close fired during this
	// attempt, its bounded connectDone wait may have EXPIRED — shutdown
	// then already tore down root state, passed its WaitGroup join, and
	// published Closed. Writing root fields or wg.Go-ing at that point
	// (pre-fix: unconditionally, before this mode check) was a real data
	// race, an Add-after-Wait WaitGroup violation, and post-Close
	// runtime mutation. beginClose takes the same lifecycleMu for the
	// mode edge, so publication and shutdown are strictly ordered.
	swapped := c.mode == modeNormalConnecting
	if swapped {
		c.aliveSession = alive
		c.internalCancel = cancel
		cancelPublished = true // ownership transferred to runShutdown
		c.wg.Go(func() {
			for env := range alive.RespCh() {
				// drain — alive messages are consumed by the recovery
				// processor inside the session itself. Anything that leaks
				// through (non-alive traffic on the system key) is terminally
				// handled right here, so fire its ack.
				runAck(env.ack)
			}
		})
		c.wg.Go(func() { c.pumpRecovery(internalCtx, recoveryCh) })
		c.setMode(modeNormalReady)
	}
	rt = c.snapshotRuntime()
	c.lifecycleMu.Unlock()
	if !swapped {
		// Close beat us. Nothing was published (the gated block above
		// refused), so shutdown never saw this attempt's resources —
		// clean them up locally: the alive session, the internal ctx
		// (the ownership defer cancels it), and the recovery-manager
		// generation this attempt opened (CloseCtx is idempotent if
		// runShutdown already closed the shared generation). Then
		// return the matching error so a confused caller doesn't think
		// Connect succeeded.
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), c.cfg.shutdownTimeout)
		alive.Close(cleanupCtx)   //nolint:contextcheck // Background-rooted on purpose: teardown must run even though the caller's ctx may already be done (Close raced this attempt)
		rmgr.CloseCtx(cleanupCtx) //nolint:contextcheck // same Background-rooted teardown rationale
		cleanupCancel()
		err = ErrAlreadyClosed
		return runtime{}, err
	}
	// emitConnConnectedOnce publishes ConnectionConnected at most once
	// per up-edge (see emitConnConnectedOnce doc). If the feed-layer
	// already claimed the gate during rmq.Open, this is a no-op.
	c.emitConnConnectedOnce(nil)
	return rt, nil
}

// onFeedEvent translates the internal feed-layer Event enum to the
// public ConnectionEvent and lossy-pushes it onto ConnectionEvents().
//
// Replay-only AMQP opens trigger feed.EventConnected too, but a
// replay-only client deliberately keeps connectState == NotConnected
// (full feed-SDK pipeline isn't initialised). Surfacing
// ConnectionConnected to consumers in that state would contradict
// ConnectionState() == NotConnected — confusing observability.
//
// Gate: only propagate feed-layer events when connectState has
// transitioned past NotConnected (i.e., a normal Connect is in
// flight or has succeeded, or shutdown is in progress). This pairs
// with the §22.2 fix that prevents replay from poisoning
// connectState.
func (c *Client) onFeedEvent(ev feed.Event) {
	// Track broker liveness BEFORE the public-event gate below: the
	// overlay must stay accurate even when the public event is
	// suppressed (replay-only mode, or a Connected fired while a fresh
	// rmq opens during modeNormalConnecting — clearing here un-wedges a
	// stale flag left by a rolled-back prior attempt). feed.emit calls
	// this synchronously from the dial/reconnect goroutines, so the
	// flag follows the real transition order; only the public channel
	// is lossy. See Client.feedDown and ConnectionState().
	switch ev.Kind {
	case feed.EventConnected:
		c.feedDown.Store(false)
	case feed.EventDisconnected, feed.EventReconnecting:
		c.feedDown.Store(true)
	}

	state := ConnectionState(c.connectState.Load())
	if state == ConnectionStateNotConnected {
		// Replay-only (modeBrokerOnly) and modeNew. No public events.
		return
	}
	switch ev.Kind {
	case feed.EventConnected:
		// Suppress while the full pipeline is still being built
		// (modeNormalConnecting → publicState=Connecting). ensureNormal's
		// success path emits explicitly after the
		// modeNormalConnecting → modeNormalReady CAS. Without this
		// suppression, a feed-layer Connected fired during rmq.Open
		// (or an autoreconnect mid-Connect) would publish
		// ConnectionConnected to consumers BEFORE recovery / alive
		// session setup could still fail. A subsequent rollback would
		// leave consumers having seen Connected with ConnectionState
		// back at NotConnected — observability divergence.
		//
		// Post-Connect natural reconnects (state stays Connected
		// across feed-layer Disconnected → Reconnecting → Connected)
		// flow through here normally: the gate is cleared by the
		// preceding Disconnected/Reconnecting branch.
		if state == ConnectionStateConnected {
			c.emitConnConnectedOnce(ev.Err)
		}
	case feed.EventDisconnected:
		// Clear the up-edge gate BEFORE emitting so a racing reconnect
		// observer can never see "Disconnected emitted but next Connected
		// suppressed". emitConn is non-blocking (drop-oldest), so there
		// is no lock-ordering concern with the atomic store.
		c.connectedEmitted.Store(false)
		c.emitConn(ConnectionDisconnected, ev.Err)
	case feed.EventReconnecting:
		c.connectedEmitted.Store(false)
		c.emitConn(ConnectionReconnecting, ev.Err)
	}
}

// Connect opens the AMQP connection, loads the producers catalog, and
// starts the recovery loop. Idempotent on success; retryable on failure.
//
// Subscribe lazy-connects on first call, so explicit Connect is optional.
// Calling Connect lets you see configuration / network errors up-front
// before adding subscriptions.
func (c *Client) Connect(ctx context.Context) error {
	_, err := c.ensureNormal(ctx)
	return err
}

// Close tears down the client. Idempotent. ctx caps the wait, not the
// shutdown work itself.
//
// Close is ABRUPT for any still-active subscription: it aborts them
// rather than draining. The in-flight (mid-pipeline) delivery is Nacked
// / released, and no NEW deliveries are pumped — but messages ALREADY
// admitted to a subscription's Messages() buffer stay readable until the
// consumer drains them or stops reading and the channel closes (they
// were acked on admission; abort discards nothing — only a graceful
// Subscription.Close that hits its drain deadline discards). To drain
// the in-flight pipeline too, call Subscription.Close(ctx) on each
// subscription FIRST and let it complete, THEN call Client.Close;
// recovery reconciles any gap.
//
//   - On a nil return, shutdown has fully COMPLETED: all subscriptions are
//     terminated, the long-lived worker goroutines (reconnect loop,
//     recovery actors, subscription sessions/pumps, cache refreshers) are
//     joined, and the event channels (ConnectionEvents / RecoveryEvents /
//     APIEvents) are closed. A narrow class of TERMINAL-cleanup workers is
//     NOT separately joined and may finish just after this returns: the
//     detached delivery ACK, the AMQP channel/topology teardown, and any
//     cache load already past its network fetch. Each is bounded by — and
//     unwinds as part of — the connection teardown Close performs (a
//     wedged transport call is failed by conn.CloseDeadline), owns no
//     consumer-visible state once Close has returned (a late cache write
//     lands in a cache no live client reads), and never redelivers or
//     duplicates an external side effect. If you assert zero lingering
//     SDK goroutines immediately after Close (e.g. a leak check), allow
//     for this bounded post-teardown unwind.
//   - If the shutdown budget (WithShutdownTimeout) expires with work still
//     pending — a wedged subscription, actor, or a custom HTTP transport
//     ignoring cancellation — shutdown still publishes Done, but Close
//     returns an error wrapping context.DeadlineExceeded instead of nil:
//     nil is reserved for "everything genuinely finished".
//   - If ctx fires first, Close returns ctx.Err() while shutdown CONTINUES
//     in the background. In that case the subscriptions and event channels
//     are NOT guaranteed closed yet — do not treat a ctx.Err() return as
//     "fully torn down". Call Close again with a fresh context to wait for
//     completion; it joins the same in-flight shutdown and returns nil once
//     done.
//
// If a Connect is in flight when Close is called, Close first waits
// (bounded by ctx) for that attempt to settle so the goroutines/sessions
// it would spawn are observable to runShutdown. Without this wait, a
// late `connectState.Store(Connected)` could overwrite Close's
// `Closed` transition.
func (c *Client) Close(ctx context.Context) error {
	c.beginClose()

	// Start the Background-rooted shutdown chain IMMEDIATELY — do NOT
	// pre-wait on the in-flight connect here. A Close(context.Background())
	// (no deadline) that pre-waited on connectDone would hang FOREVER if a
	// connect is wedged in producerManager.Open on a cancellation-ignoring
	// custom transport: runShutdown — which owns the shutdown budget and
	// cancels the API/producer lifetimes — would never start, so
	// WithShutdownTimeout never applies. runShutdown does its OWN
	// connectDone wait bounded by the shutdown deadline, and the
	// late-connect publication guards (mode-gated under lifecycleMu, set by
	// beginClose) keep a racing connect from publishing past the modeClosing
	// transition. See NEXT.md §8 Close, "Shutdown work budget"; the ctx
	// parameter bounds the caller's wait, not the shutdown work.
	c.closeOnce.Do(func() { go c.runShutdown() }) //nolint:contextcheck // intentional Background root

	// Fast path: already done. Completed shutdown always wins over ctx.
	select {
	case <-c.closed:
		return c.closeErr
	default:
	}
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

// beginClose is the lifecycle-mode transition for Close. Idempotent:
// concurrent Close calls observe modeClosing on the second arrival and
// return whatever in-flight ensureNormal exists. Marks mode=modeClosing
// before runShutdown spawns so ensureNormal's success-path mode-CAS
// refuses to transition past Connecting (returns ErrAlreadyClosed
// instead) and admitSubscription rejects new admissions.
//
// Returns the in-flight ensureNormal connectDone chan if an attempt is
// currently running, nil otherwise. Close no longer waits on it (doing
// so before starting runShutdown could deadlock — see Close); instead
// runShutdown waits on it BOUNDED BY THE SHUTDOWN DEADLINE so it observes
// ensureNormal's full set of writes (aliveSession, internalCancel)
// before tearing them down, without a wedged connect exceeding the
// budget. The returned chan is retained for tests / future callers.
func (c *Client) beginClose() chan struct{} {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	if c.mode != modeClosing && c.mode != modeClosed {
		c.setMode(modeClosing)
		// Cancel the client lifetime at the very START of shutdown —
		// BEFORE any wait for an in-flight connect (here, in Close, and
		// in runShutdown). This aborts detached cache loads, the who-am-i
		// probe, AND an in-flight broker dial (ensureNormal / ensureBroker
		// derive their rmq.Open ctx from lifetimeCtx), so a connect races
		// to settle promptly instead of running for feed.Client's 30s dial
		// fallback and blowing past WithShutdownTimeout.
		if c.lifetimeCancel != nil {
			c.lifetimeCancel()
		}
	}
	if c.connecting {
		return c.connectDone
	}
	return nil
}

// waitBounded blocks until wg completes OR ctx is done, whichever comes
// first — so a stuck wait can't exceed the shutdown budget. The joining
// goroutine outlives a timeout but is harmless: whatever it waits on was
// already signalled to abort. Reports whether the WaitGroup actually
// completed — runShutdown folds a false into the terminal close error so
// Close's nil return keeps meaning "all shutdown work completed".
func waitBounded(ctx context.Context, wg *sync.WaitGroup) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		// Completion-first accounting: the select picks uniformly among
		// READY arms, and with an already-expired ctx the waiter
		// goroutine may not even have been scheduled — either way
		// "timed out with work pending" could be reported for a join
		// that IS complete (the public timeout stays valid; the
		// accounting would be false). Yield once so a ready waiter can
		// publish, then re-check completion before reporting failure.
		// (Aliased import: the local `runtime` snapshot type shadows
		// the stdlib package name in this file.)
		goruntime.Gosched()
		select {
		case <-done:
			return true
		default:
			return false
		}
	}
}

func (c *Client) runShutdown() {
	// ONE work deadline for the ENTIRE shutdown, established BEFORE any
	// wait (including the in-flight-connect wait below) so
	// WithShutdownTimeout (default 5s) is a TOTAL bound — every wait
	// (connect settle, subscription drains, broker-open fence, internal
	// goroutines, broker close) shares it. runShutdown runs in its own
	// goroutine; the caller's Close ctx bounds the caller's *wait*, not
	// this work, so Background() is the intentional root. beginClose
	// already cancelled lifetimeCtx (aborting in-flight loads / dials);
	// re-cancel here is idempotent and covers the modeClosed re-entry.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), c.cfg.shutdownTimeout)
	defer cancel()
	if c.lifetimeCancel != nil {
		c.lifetimeCancel()
	}

	// shutdownComplete tracks whether EVERY bounded wait below actually
	// finished. If any timed out, work is still pending when we publish
	// Done — the terminal close error records that (see the tail), so
	// Close's documented "nil return == fully completed" contract holds.
	shutdownComplete := true

	// If Close()'s caller ctx timed out before an in-flight ensureNormal
	// settled, wait on it here so we observe its writes (aliveSession,
	// internalCancel) before reading them below — BOUNDED by the shutdown
	// budget. The lifetime cancel above aborts the connect's dial, so this
	// normally returns immediately; a stuck connect can't exceed the budget.
	c.lifecycleMu.Lock()
	inflight := c.connectDone
	connecting := c.connecting
	c.lifecycleMu.Unlock()
	if connecting && inflight != nil {
		select {
		case <-inflight:
		case <-shutdownCtx.Done():
			shutdownComplete = false
		}
	}

	// Stop internal pumps first so they don't push to channels we close.
	if c.internalCancel != nil {
		c.internalCancel()
	}

	// Recovery uses close(closeCh) broadcast under sync.Once internally;
	// the actor joins are bounded by the shared deadline so a wedged
	// actor can't pin shutdown.
	if rmgr := c.recoveryManager.Load(); rmgr != nil {
		if !rmgr.CloseCtx(shutdownCtx) {
			shutdownComplete = false
		}
	}

	// Settle in-flight Subscribe session setups BEFORE snapshotting the
	// registry: between session.Open and admitSubscription a session's
	// goroutines exist but the subscription is not yet registered — a
	// snapshot taken in that window would join only tracked work and
	// return nil while the pre-admission session lives on (rolled back
	// only whenever the paused Subscribe resumed). beginClose's mode
	// edge (same lifecycleMu the setup fence uses) guarantees no NEW
	// setups start after this point, so the bounded wait drains the
	// window: each setup either admitted (visible to the snapshot
	// below) or rolled itself back.
	if !waitBounded(shutdownCtx, &c.sessionSetupWG) {
		shutdownComplete = false
	}

	// Tear down user subscriptions. abortWithErr is idempotent; the
	// resulting goroutines drain the legacy session.
	c.subsMu.Lock()
	subs := make([]*Subscription, 0, len(c.subs))
	for _, s := range c.subs {
		subs = append(subs, s)
	}
	c.subsMu.Unlock()
	for _, s := range subs {
		s.abortWithErr(ErrAlreadyClosed)
	}
	// Bounded by the shared deadline: a wedged subscription must not
	// consume the whole budget (or block shutdown forever).
	for _, s := range subs {
		select {
		case <-s.Done():
		case <-shutdownCtx.Done():
			shutdownComplete = false
		}
	}

	if c.aliveSession != nil {
		c.aliveSession.Close(shutdownCtx)
	}

	// Bounded join: a custom HTTP transport that ignores ctx
	// cancellation must not pin shutdown past the budget.
	if c.apiClient != nil {
		if !c.apiClient.CloseCtx(shutdownCtx) {
			shutdownComplete = false
		}
	}

	if c.cacheManager != nil {
		// Bounded: the localized-static refresh goroutine performs
		// synchronous fetch I/O — a cancellation-ignoring custom
		// transport could pin an UNBOUNDED join forever, wedging
		// c.closed and every later Close call.
		if !c.cacheManager.CloseCtx(shutdownCtx) {
			shutdownComplete = false
		}
	}

	// Fence in-flight ensureBroker calls (replay subscribe paths) before
	// we close rmq, but bounded by the shared deadline — an in-flight
	// broker open can otherwise run for feed.Client's 30s dial fallback,
	// blowing past WithShutdownTimeout. lifetimeCancel above already
	// signalled those opens to abort.
	if !waitBounded(shutdownCtx, &c.brokerOpenWG) {
		shutdownComplete = false
	}

	if rmq := c.rabbitMQClient.Load(); rmq != nil {
		// Capture the broker-close error into closeErr so callers of
		// Close(ctx) observe it (instead of silently discarding via
		// `_ = ...`). errors.Join keeps the slot open for future
		// shutdown-stage failures (e.g., once api/cache/session
		// Close paths grow error returns).
		if err := rmq.Close(shutdownCtx); err != nil {
			c.closeErr = errors.Join(c.closeErr, fmt.Errorf("rmq close: %w", err))
		}
	}

	// Join internal goroutines, bounded by the same deadline (internalCancel
	// above already signalled them to exit).
	if !waitBounded(shutdownCtx, &c.wg) {
		shutdownComplete = false
	}

	// Disable API capture so future public API calls don't even queue
	// an emit (defence in depth — the eventsClosed gate below catches
	// any in-flight emit that already snapshotted the callback).
	if c.apiClient != nil {
		c.apiClient.SetEventCapture(api.EventCapture{})
	}

	// Terminal-event ordering: enqueue ConnectionClosed and close the
	// channels inside ONE eventsMu write section. Pre-fix Closed was
	// emitted through the ordinary (read-locked) path first and the
	// channels were closed in a separate critical section — if broker
	// shutdown exceeded its budget, a surviving reconnect goroutine
	// could enqueue Disconnected/Reconnecting in the gap, so Closed was
	// not guaranteed to be the terminal event consumers observe.
	// Setting eventsClosed under the write lock blocks every emitter
	// first; Closed is then enqueued directly (drop-oldest) and the
	// channels close atomically with it.
	c.eventsMu.Lock()
	c.eventsClosed = true
	closedEv := ConnectionEvent{Kind: ConnectionClosed, At: time.Now()}
	select {
	case c.connEvents <- closedEv:
	default:
		select { // drop-oldest, mirroring pushConn
		case <-c.connEvents:
		default:
		}
		select {
		case c.connEvents <- closedEv:
		default:
		}
	}
	close(c.connEvents)
	close(c.recvEvents)
	close(c.apiEvents)
	c.eventsMu.Unlock()

	// Finalise the lifecycle mode: the public observable flips
	// ConnectionStateClosing → ConnectionStateClosed here, at the point
	// the events.go contract for Closed ("Close has completed") is
	// actually true — event channels closed above, subscriptions joined
	// earlier. Publishing Closed at beginClose (pre-fix) let a poller
	// observe "closed" while Done()s hadn't fired yet.
	c.lifecycleMu.Lock()
	c.setMode(modeClosed)
	c.lifecycleMu.Unlock()

	// If any bounded wait above timed out, goroutines/work remain when
	// Done publishes. Record it as the terminal cause so Close returns
	// non-nil — its documented nil contract is "all shutdown work
	// completed", and returning nil here would falsely claim that. The
	// stragglers were all signalled to abort; they unwind on their own.
	if !shutdownComplete {
		c.closeErr = errors.Join(c.closeErr,
			fmt.Errorf("gosdk: shutdown budget %v exceeded with work still pending: %w",
				c.cfg.shutdownTimeout, context.DeadlineExceeded))
	}

	close(c.closed)
}

// resolveSubscribeOptions is THE option-resolution step Subscribe uses
// (extracted so tests exercise the production resolver instead of a
// local copy). Options apply first; AllMessageInterest is the fallback
// ONLY when the caller didn't pick an interest (explicitly or via
// WithSpecificEvents). We can't pre-default messageInterest because
// types.SpecifiedMatchesOnlyMessageInterest is "" — a pre-default of
// All would survive any "is interest empty" check and silently override
// an explicit specified-events choice.
func resolveSubscribeOptions(opts ...SubscribeOption) subscribeConfig {
	var subCfg subscribeConfig
	for _, opt := range opts {
		opt(&subCfg)
	}
	if !subCfg.messageInterestSet {
		subCfg.messageInterest = types.AllMessageInterest
	}
	return subCfg
}

// Subscribe creates a new subscription and returns the *Subscription.
// First call lazy-connects if Connect was not called.
//
// The supplied ctx governs the Subscribe call itself (lazy-connect dial,
// queue declaration). It does NOT govern the subscription's lifetime —
// once Subscribe returns, the subscription lives until the caller calls
// Subscription.Close, the Client closes, or a terminal error occurs.
func (c *Client) Subscribe(ctx context.Context, opts ...SubscribeOption) (*Subscription, error) {
	subCfg := resolveSubscribeOptions(opts...)

	// Validate routing keys BEFORE opening the AMQP pipeline. An
	// invalid subscription config (e.g. SpecifiedMatchesOnly without
	// WithSpecificEvents) used to trigger a lazy-connect first and
	// only fail afterward — wasted dial + rollback.
	routingKeys, err := c.routingKeys(subCfg)
	if err != nil {
		return nil, err
	}

	// Lifecycle entry: ensureBroker for replay (AMQP-only), ensureNormal
	// for the full pipeline. Each returns a generation-paired runtime
	// snapshot so subsequent reads (session construction below) operate
	// on the same (rmq, rmgr) pair, even if a concurrent rollback swaps
	// the atomic pointers.
	var rt runtime
	if subCfg.replay {
		rt, err = c.ensureBroker(ctx)
	} else {
		rt, err = c.ensureNormal(ctx)
	}
	if err != nil {
		return nil, err
	}

	exchangeName := c.cfg.exchangeName
	recoveryProcessor := recoveryMessageProcessor(rt.rmgr)
	if subCfg.replay {
		exchangeName = c.cfg.replayExchangeName
		recoveryProcessor = &recovery.DummyManager{}
	}

	session := newSession(
		rt.rmq,
		c.producerManager,
		c.cacheManager,
		c.feedMessageFactory,
		recoveryProcessor,
		exchangeName,
		c.cfg.sportIDPrefix,
		subCfg.replay,
		c.logger,
		c.cfg.exceptionStrategy,
		c.cfg.amqpPrefetch,
	)

	// Fence the setup window (session.Open → admission/rollback) against
	// Close — see sessionSetupWG. Mode-checked Add under lifecycleMu,
	// mirroring brokerOpenWG's admission discipline.
	c.lifecycleMu.Lock()
	if c.mode == modeClosing || c.mode == modeClosed {
		c.lifecycleMu.Unlock()
		return nil, ErrAlreadyClosed
	}
	c.sessionSetupWG.Add(1)
	c.lifecycleMu.Unlock()
	defer c.sessionSetupWG.Done()

	mi := subCfg.messageInterest
	if err := session.Open(ctx, routingKeys, &mi, c.cfg.reportExtendedData); err != nil {
		return nil, fmt.Errorf("gosdk: session open: %w", err)
	}

	bufSize := c.cfg.subscriptionBuffer
	if bufSize <= 0 {
		bufSize = defaultSubscriptionBuffer
	}
	sub := &Subscription{
		id:              uuid.New(),
		messages:        make(chan types.SessionMessage, bufSize),
		closed:          make(chan struct{}),
		underlying:      session,
		pumpDone:        make(chan struct{}),
		pumpStop:        make(chan struct{}),
		shutdownTimeout: c.cfg.shutdownTimeout,
		client:          c,
	}

	if err := c.admitSubscription(sub); err != nil {
		// Close raced us between ensure* return and admission. Roll
		// back the session we just opened. WithoutCancel(ctx) preserves
		// caller metadata while severing cancellation — the rollback
		// shouldn't be aborted by a tight Subscribe ctx.
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.cfg.shutdownTimeout)
		session.Close(shutdownCtx)
		cancel()
		return nil, err
	}

	go c.pumpSubscription(sub) //nolint:contextcheck // abort path is the documented Background-rooted shutdown chain

	return sub, nil
}

// admitSubscription is the atomic admission step: re-checks the
// Closing/Closed mode under lifecycleMu and, if still admittable,
// inserts the sub + increments wg as a single critical section.
//
// Why one mu for two operations: the early "is client closed?" check
// at the top of Subscribe could pass, Close could run to completion
// (snapshot subs, wait wg, close events), and a late wg.Add(1) would
// then panic with "WaitGroup misuse: Add called concurrently with
// Wait". Holding lifecycleMu across the mode re-check + insert + Add
// makes admission and beginClose mutually exclusive, so either
// admission wins (Close sees the new sub in its snapshot) or Close
// wins (admission returns ErrAlreadyClosed).
func (c *Client) admitSubscription(sub *Subscription) error {
	c.lifecycleMu.Lock()
	if c.mode == modeClosing || c.mode == modeClosed {
		c.lifecycleMu.Unlock()
		return ErrAlreadyClosed
	}
	c.subsMu.Lock()
	c.subs[sub.id] = sub
	c.wg.Add(1)
	c.subsMu.Unlock()
	c.lifecycleMu.Unlock()
	return nil
}

// pumpSubscription forwards legacy SessionMessage values from the
// underlying session to the public Subscription channel. Exits when the
// session's RespCh closes (terminal) or the subscription requests
// shutdown (via Close / abortWithErr / parent Close).
//
// On respCh close, if the session recorded a terminal error (StrategyThrow
// path), the subscription is aborted with that error so the consumer's
// Sub.Err() reflects the cause.
//
// Pump is the *sole sender* on sub.messages; it also owns the close
// (deferred on exit) — runShutdown must not close sub.messages, that
// would race with the in-flight send case below and panic.
func (c *Client) pumpSubscription(sub *Subscription) {
	defer c.wg.Done()
	// Order matters: close(sub.messages) before close(sub.pumpDone) so a
	// caller waiting on pumpDone sees a fully-closed message stream.
	defer func() {
		close(sub.messages)
		close(sub.pumpDone)
	}()

	respCh := sub.underlying.RespCh()
	for {
		select {
		case <-sub.pumpStop:
			// pumpStop, NOT sub.closed: runShutdown must be able to
			// stop+join the pump BEFORE it publishes Done() — the
			// deadline-discard path depends on the sender being gone
			// while Done() is still open (see Subscription.stopPump).
			return
		case env, ok := <-respCh:
			if !ok {
				if err := sub.underlying.Err(); err != nil {
					sub.abortWithErr(err)
				}
				return
			}
			if !c.deliverToSubscription(sub, env) {
				return
			}
		}
	}
}

// slowConsumerWarnInterval is how long a blocked public-buffer send waits
// before each slow-consumer warning (NEXT.md §8: "emits a slog.Warn once
// per detection window if the buffer stays full for >5s — observability,
// not action"). Package variable so tests can shrink the window.
var slowConsumerWarnInterval = 5 * time.Second

// deliverToSubscription sends one envelope into the public Messages()
// buffer and, on success, acks the AMQP delivery — this send IS the
// "admitted to the subscription buffer" moment of the NEXT.md §0.6
// manual-ack contract, so the ack lives here and nowhere earlier in the
// pipeline. While the buffer stays full, the slow-consumer warning fires
// once per detection window — measured at the real blockage point (the
// public channel), so it cannot trigger while Messages() has room but an
// upstream stage is merely slow. Returns false when the pump must exit
// (pumpStop); the undelivered envelope stays unacked — on abrupt
// teardown the broker releases and redelivers it, while on a graceful
// drain the exclusive autoDelete queue is deleted and the message is
// lost. Either way its disposition never fires, so the feed layer's
// settlement accounting reports the drain as not-settled and
// runShutdown surfaces the deadline instead of a clean close.
func (c *Client) deliverToSubscription(sub *Subscription, env sessionEnvelope) bool {
	for {
		timer := time.NewTimer(slowConsumerWarnInterval)
		select {
		case sub.messages <- env.msg:
			timer.Stop()
			return c.settleAck(sub, env.ack)
		case <-sub.pumpStop:
			timer.Stop()
			return false
		case <-timer.C:
			c.logger.
				WithField("subscription_id", sub.id.String()).
				WithField("subscription_buffer_capacity", cap(sub.messages)).
				Warn("feed: subscription buffer full; consumer is not draining Messages() — broker backpressure engaged")
		}
	}
}

// settleAck fires the delivery's broker ACK after a successful public-
// buffer send, returning whether the pump should keep pumping (true) or
// exit (false, on pumpStop).
//
// The ACK is a synchronous amqp091 network write with no ctx. On a
// stalled transport (broker alive but not draining the socket) it can
// block indefinitely — and the ONLY thing that releases it is a
// connection teardown (Client.Close → conn.CloseDeadline). Running it
// inline on the pump goroutine meant a wedged ACK pinned the pump, which
// owns close(sub.messages): Done() would close on the shutdown deadline
// while Messages() stayed open forever, breaking the documented
// "Done ⇒ message stream closed" ordering and deadlocking a consumer
// that waits for Messages() to drain before calling Client.Close.
//
// Decoupled here: the ACK runs on its own goroutine and the pump waits
// on it, so the common case is unchanged (backpressure preserved — the
// pump doesn't pull the next delivery until the ACK settles). But on
// pumpStop the pump ABANDONS the wait and exits, closing sub.messages
// promptly; the detached ACK goroutine completes later when the
// transport teardown fails the write (bounded — it never outlives the
// connection). The feed layer's settlement accounting (unsettledN) still
// tracks the pending ACK, so a graceful close whose ACK never settled is
// reported as not-settled, never a clean drain.
func (c *Client) settleAck(sub *Subscription, ack func()) bool {
	if ack == nil {
		return true
	}
	done := make(chan struct{})
	go func() {
		runAck(ack)
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-sub.pumpStop:
		return false
	}
}

func (c *Client) pumpRecovery(ctx context.Context, in <-chan types.RecoveryMessage) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-in:
			if !ok {
				return
			}
			ev := RecoveryEvent{
				ProducerStatus: msg.ProducerStatus,
				EventRecovery:  msg.EventRecoveryMessage,
				At:             time.Now(),
			}
			// types.RecoveryMessage uses interface fields so the
			// EventRecoveryMessage interface is the only nil-detectable
			// discriminator on the wire. Translate to an explicit Kind
			// at the public boundary.
			if msg.EventRecoveryMessage != nil {
				ev.Kind = RecoveryEventKindEventRecovery
			} else {
				ev.Kind = RecoveryEventKindProducerStatus
			}
			c.pushRecovery(ev)
		}
	}
}

// pushDropOldest is the generic lossy-push primitive: try push; on
// full, drop oldest and push new. Returns true iff a drop occurred.
//
// Two non-blocking selects make this a deliberate try-pattern — if a
// concurrent consumer drains between drops, we won't block.
//
// Accounting is APPROXIMATE under contention: senders run this under
// eventsMu.RLock (shared), so concurrent producers can interleave the
// try/drain/push triple — occasionally more than the oldest entry is
// displaced, or the brand-new event loses its slot to a peer. That is
// acceptable by contract: these channels are lossy observability
// streams (NEXT.md §0.3) and the polling getters are the reliable
// fallback; the drop warn is a signal, not an exact count.
func pushDropOldest[T any](ch chan T, ev T) (dropped bool) {
	select {
	case ch <- ev:
		return false
	default:
	}
	select {
	case <-ch:
		dropped = true
	default:
	}
	select {
	case ch <- ev:
	default:
	}
	return dropped
}

// warnDrop emits a rate-limited warn when an event channel drops.
// One log per channel per dropWarnInterval.
func (c *Client) warnDrop(lastWarn *atomic.Int64, name string, capacity int) {
	now := time.Now().UnixNano()
	prev := lastWarn.Load()
	if now-prev >= int64(dropWarnInterval) && lastWarn.CompareAndSwap(prev, now) {
		c.logger.WithField("channel", name).
			WithField("capacity", capacity).
			Warn("event channel overflow; dropped oldest event")
	}
}

// pushConn pushes a ConnectionEvent under the eventsMu RLock + closed-flag
// gate. See pushAPIEvent for the rationale; the same protection is applied
// to all three event channels for uniformity.
func (c *Client) pushConn(ev ConnectionEvent) {
	c.eventsMu.RLock()
	defer c.eventsMu.RUnlock()
	if c.eventsClosed {
		return
	}
	if pushDropOldest(c.connEvents, ev) {
		c.warnDrop(&c.lastDropWarnConn, "connection", cap(c.connEvents))
	}
}

func (c *Client) pushRecovery(ev RecoveryEvent) {
	c.eventsMu.RLock()
	defer c.eventsMu.RUnlock()
	if c.eventsClosed {
		return
	}
	if pushDropOldest(c.recvEvents, ev) {
		c.warnDrop(&c.lastDropWarnRecv, "recovery", cap(c.recvEvents))
	}
}

func (c *Client) emitConn(kind ConnectionEventKind, err error) {
	ev := ConnectionEvent{Kind: kind, Err: err, At: time.Now()}
	c.pushConn(ev)
}

// emitConnConnectedOnce publishes ConnectionConnected at most once per
// "up edge" — the window between any non-connected state and the next
// Disconnected/Reconnecting event. Returns true if this call published
// the event, false if the gate was already claimed.
//
// Both onFeedEvent (feed-layer EventConnected) and Connect's success
// path call this helper. Whichever side runs first publishes the event
// and claims the gate via CAS; the other call is a no-op. This closes
// two race windows the IsOpen-based predecessor left open:
//
//  1. Replay-then-Connect with a broker reconnect mid-Connect: the
//     feed-layer autoreconnect fires EventConnected while connectState
//     is Connecting (gate passes). Without this helper, both that
//     event and the explicit emit below would publish — duplicate.
//  2. Plain normal Connect: feed-layer EventConnected fires during the
//     real dial; the explicit emit below would have been a duplicate
//     if Connect emitted unconditionally on success.
//
// The gate is cleared by Disconnected/Reconnecting so the *next* up
// edge (post-Connect natural reconnect) emits normally. It is also
// cleared at the top of each Connect attempt so a rolled-back attempt's
// stale "true" cannot mute a fresh attempt's emit.
func (c *Client) emitConnConnectedOnce(err error) bool {
	if !c.connectedEmitted.CompareAndSwap(false, true) {
		return false
	}
	c.emitConn(ConnectionConnected, err)
	return true
}

// installAPICapture wires the api.Client's event emitter to the public
// APIEvents channel based on cfg.apiCallLogging. APILogOff leaves the
// channel quiescent (no emitter installed); higher levels enable
// progressively more detail. The emitter is the lossy push pattern from
// NEXT.md §19.3 — drops on overflow rather than blocking the API call.
func (c *Client) installAPICapture() {
	if c.cfg.apiCallLogging == APILogOff {
		return
	}
	capture := api.EventCapture{
		Emit:         c.pushAPIEvent,
		BodyLimit:    c.cfg.apiCallBodyLimit,
		ResponseBody: c.cfg.apiCallLogging >= APILogResponses,
		RequestBody:  c.cfg.apiCallLogging == APILogFull,
	}
	c.apiClient.SetEventCapture(capture)
}

// pushAPIEvent converts an internal/api.APIEvent into the public
// gosdk.APIEvent and lossy-pushes it onto the APIEvents channel.
//
// Race-safety: api.Client snapshots `emit := c.capture.Emit` under
// its own RLock, releases that lock, then calls emit() (which is
// pushAPIEvent). Between the snapshot and the call, runShutdown can
// run SetEventCapture(zero) AND close(c.apiEvents) — leaving us with
// a stale function pointer pointing at a closed channel.
//
// Gate: take c.eventsMu RLock and check c.eventsClosed before any
// channel access. runShutdown takes Lock + sets the flag + closes
// channels as a single critical section, so an in-flight emitter
// holding RLock either (a) finishes before runShutdown's Lock or (b)
// runs after runShutdown released Lock and observes c.eventsClosed=true,
// returning without touching the channel.
func (c *Client) pushAPIEvent(ev api.APIEvent) {
	// Re-clone the locale pointee at the public boundary too — the
	// internal layer already snapshots per emit, but this keeps the
	// public APIEvent's ownership independent of ANY internal aliasing
	// (snapshot semantics are part of the event contract).
	locale := ev.Locale
	if locale != nil {
		l := *locale
		locale = &l
	}
	out := APIEvent{
		At:               ev.At,
		Method:           ev.Method,
		URL:              ev.URL,
		Status:           ev.Status,
		Latency:          ev.Latency,
		Attempt:          ev.Attempt,
		Locale:           locale,
		Request:          ev.Request,
		RequestTruncated: ev.RequestTruncated,
		Response:         ev.Response,
		Truncated:        ev.Truncated,
		Err:              ev.Err,
	}
	c.eventsMu.RLock()
	defer c.eventsMu.RUnlock()
	if c.eventsClosed {
		return
	}
	if pushDropOldest(c.apiEvents, out) {
		c.warnDrop(&c.lastDropWarnAPI, "api", cap(c.apiEvents))
	}
}

// validRoutingURNPart reports whether s can be interpolated into an
// AMQP topic binding as (part of) a single segment: non-empty, ASCII
// letters/digits/'_'/'-' only. Stricter than types.ParseURN's
// identifier rule — '.' is the topic-segment delimiter and '*'/'#' are
// wildcards, so any of them inside a URN field would change the
// binding's shape instead of being matched literally.
func validRoutingURNPart(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// routingKeys derives the AMQP routing keys for a single subscription's
// message interest. Mirrors the legacy generateKeys logic but scoped to
// one session — multi-session validation is not relevant in the flat
// model where each Subscribe creates an independent consumer.
func (c *Client) routingKeys(cfg subscribeConfig) ([]string, error) {
	// Sink validation, same rationale as the WithSpecificEvents check
	// below: the interest string is interpolated VERBATIM into topic
	// bindings, so an arbitrary value either silently matches nothing
	// (typo) or broadens the binding via wildcards ("#" produces a
	// "#.#" binding). Unknown values would also default-accept every
	// producer scope in IsProducerInScope. Rejected even for replay
	// (where interests are ignored) — a bogus value there is equally a
	// programming error, just a latent one.
	if !cfg.messageInterest.IsKnown() {
		return nil, fmt.Errorf("gosdk: WithMessageInterest: unknown message interest %q — use one of the types.*MessageInterest constants", string(cfg.messageInterest))
	}
	if cfg.replay {
		// Replay sessions consume everything from the replay exchange.
		return []string{string(types.AllMessageInterest)}, nil
	}

	keysSet := make(map[string]struct{})
	var basicKeys []string
	if cfg.messageInterest == types.SpecifiedMatchesOnlyMessageInterest {
		if len(cfg.specificEvents) == 0 {
			return nil, errors.New("gosdk: SpecifiedMatchesOnly requires WithSpecificEvents")
		}
		for urn := range cfg.specificEvents {
			// Sink validation: URN fields are exported, so consumers can
			// construct values ParseURN would reject — and these fields
			// are interpolated into RabbitMQ TOPIC syntax where '.' is
			// the segment delimiter and '*'/'#' are wildcards. An
			// injected whole-segment '#' (Type: "match.#") makes the
			// binding match UNRELATED events (which the pipeline then
			// acks), and a stray '.' changes the eight-segment
			// routing-key shape so intended messages silently never
			// match. Note ParseURN's identifier rule is NOT sufficient
			// here: it deliberately allows '.' as forward-compat
			// headroom for HTTP paths.
			if !validRoutingURNPart(urn.Prefix) || !validRoutingURNPart(urn.Type) || urn.ID < 0 {
				return nil, fmt.Errorf("gosdk: WithSpecificEvents: URN %q is not routing-safe: prefix and type must be non-empty and contain only ASCII letters, digits, '_' or '-', and the id must be non-negative", urn.ToString())
			}
			basicKeys = append(basicKeys, fmt.Sprintf("#.%s:%s.%d", urn.Prefix, urn.Type, urn.ID))
		}
	} else {
		basicKeys = []string{string(cfg.messageInterest)}
	}

	nodeID := c.cfg.SdkNodeID()
	var snapshotKey string
	if nodeID != nil {
		snapshotKey = fmt.Sprintf("%s%d", snapshotKeyTemplate, *nodeID)
	} else {
		snapshotKey = fmt.Sprintf("%s%s", snapshotKeyTemplate, "-")
	}

	for _, base := range basicKeys {
		if nodeID != nil {
			keysSet[fmt.Sprintf("%s.%d.#", base, *nodeID)] = struct{}{}
			keysSet[fmt.Sprintf("%s.-.#", base)] = struct{}{}
		} else {
			keysSet[fmt.Sprintf("%s.#", base)] = struct{}{}
		}
		keysSet[snapshotKey] = struct{}{}
	}
	if cfg.messageInterest != types.SystemAliveOnly {
		keysSet[string(types.SystemAliveOnly)] = struct{}{}
	}

	out := make([]string, 0, len(keysSet))
	for k := range keysSet {
		out = append(out, k)
	}
	return out, nil
}

// --- Connection / observability accessors ---

// ConnectionState returns the current connection state. Polling-friendly
// escape hatch when the lossy ConnectionEvents channel may have dropped
// a transition.
//
// While the pipeline is up (lifecycle-Connected) but the feed layer has
// lost the broker and is redialing, this reports
// ConnectionStateConnecting — mode alone would stay frozen at Connected
// for the whole connected lifetime, and a consumer that missed the
// lossy Reconnecting event could never observe the reconnect window by
// polling (the exact fallback events.go prescribes).
func (c *Client) ConnectionState() ConnectionState {
	s := ConnectionState(c.connectState.Load())
	if s == ConnectionStateConnected && c.feedDown.Load() {
		return ConnectionStateConnecting
	}
	return s
}

// ConnectionEvents returns the lossy event channel for connection state
// transitions. Closed on Close.
func (c *Client) ConnectionEvents() <-chan ConnectionEvent { return c.connEvents }

// RecoveryEvents returns the lossy event channel for recovery state
// transitions. Closed on Close.
func (c *Client) RecoveryEvents() <-chan RecoveryEvent { return c.recvEvents }

// APIEvents returns the lossy channel for HTTP API call events.
// Quiescent unless WithAPICallLogging was set above APILogOff.
// (Phase 6e wires the producing middleware.)
func (c *Client) APIEvents() <-chan APIEvent { return c.apiEvents }

// --- Bookmaker ---

// BookmakerDetails returns the authenticated bookmaker profile.
func (c *Client) BookmakerDetails(ctx context.Context) (types.BookmakerDetail, error) {
	d, err := c.whoAmIManager.BookmakerDetails(ctx)
	if err != nil {
		return nil, fmt.Errorf("gosdk: bookmaker details: %w", err)
	}
	return d, nil
}

// --- Producers ---

// Producers returns all producers known to the SDK.
func (c *Client) Producers(ctx context.Context) ([]types.Producer, error) {
	m, err := c.producerManager.AvailableProducers(ctx)
	if err != nil {
		return nil, fmt.Errorf("gosdk: producers: %w", err)
	}
	return mapToSlice(m), nil
}

// ActiveProducers returns currently-active producers.
func (c *Client) ActiveProducers(ctx context.Context) ([]types.Producer, error) {
	m, err := c.producerManager.ActiveProducers(ctx)
	if err != nil {
		return nil, fmt.Errorf("gosdk: active producers: %w", err)
	}
	return mapToSlice(m), nil
}

// ProducersInScope returns active producers serving the given scope
// (live or prematch).
func (c *Client) ProducersInScope(ctx context.Context, scope types.ProducerScope) ([]types.Producer, error) {
	m, err := c.producerManager.ActiveProducersInScope(ctx, scope)
	if err != nil {
		return nil, fmt.Errorf("gosdk: producers in scope %v: %w", scope, err)
	}
	return mapToSlice(m), nil
}

// Producer returns a single producer by id.
func (c *Client) Producer(ctx context.Context, id int) (types.Producer, error) {
	p, err := c.producerManager.GetProducer(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("gosdk: producer %d: %w", id, err)
	}
	return p, nil
}

// SetProducerEnabled toggles the per-producer enable flag. Disabled
// producers don't trigger recovery and their messages are dropped at
// the session pre-filter.
func (c *Client) SetProducerEnabled(ctx context.Context, id int, enabled bool) error {
	if err := c.producerManager.SetProducerState(ctx, id, enabled); err != nil {
		return fmt.Errorf("gosdk: set producer %d enabled=%v: %w", id, enabled, err)
	}
	return nil
}

// SetProducerRecoveryFromTimestamp pins the next snapshot recovery's
// "after" timestamp for this producer. Useful when consumers persist
// processed timestamps externally and want recovery to resume from a
// specific point on next connect.
func (c *Client) SetProducerRecoveryFromTimestamp(ctx context.Context, id int, t time.Time) error {
	if err := c.producerManager.SetProducerRecoveryFromTimestamp(ctx, id, t); err != nil {
		return fmt.Errorf("gosdk: set producer %d recovery-from %s: %w", id, t.Format(time.RFC3339), err)
	}
	return nil
}

// --- Recovery ---

// RecoveryHandle is returned by RecoverEventOdds / RecoverEventStateful.
// It exposes per-request semantics so callers can wait on a specific
// recovery completing without scanning the lossy RecoveryEvents
// channel. The handle is reliable — even if the channel event is
// dropped, Done() / Result() / Status() reflect the terminal outcome.
//
// This is a type ALIAS to the internal recovery.Handle — a deliberate
// v1.0.0 decision (PR #42 review): the full exported method set of
// recovery.Handle (RequestID / ProducerID / EventID / Done / Status /
// Result / Snapshot / IsTerminal) is locked as the public contract.
// The surface is small and read-only, and the alias avoids a
// forwarding wrapper. Any change to those methods is a public
// breaking change — see the matching note on recovery.Handle.
type RecoveryHandle = recovery.Handle

// RecoverEventOdds initiates an event-odds recovery for a single event
// and returns a *RecoveryHandle that tracks completion reliably.
//
//	h, err := client.RecoverEventOdds(ctx, producerID, eventURN)
//	if err != nil { ... }
//	<-h.Done()
//	res := h.Result()
//	if res.Status == types.RecoveryStatusCompleted { ... }
//
// Handles remain queryable via Client.EventRecoveryStatus for a fixed
// 5-minute retention period after they reach a terminal state, then are
// garbage-collected. The retention period is an internal constant and is
// not currently configurable.
//
// ctx bounds ADMISSION only (the hand-off to the recovery machinery).
// A non-nil error means the recovery was NOT accepted and no request
// was issued — safe to retry. Once accepted you always get the handle,
// even if ctx was cancelled concurrently; cancelling ctx after that
// does not abort the recovery (wait on the handle instead). Requires
// the feed pipeline to be UP: before Connect completes (or while it is
// still connecting) this reports ErrManagerNotOpen, and once Close has
// begun it reports ErrAlreadyClosed (match with errors.Is).
func (c *Client) RecoverEventOdds(ctx context.Context, producerID int, eventID types.URN) (*RecoveryHandle, error) {
	rmgr, err := c.readyRecoveryManager()
	if err != nil {
		return nil, fmt.Errorf("gosdk: recover event odds (producer=%d, event=%s): %w", producerID, eventID.ToString(), err)
	}
	h, err := rmgr.InitiateEventOddsRecoveryHandle(ctx, producerID, eventID)
	if err != nil {
		return nil, fmt.Errorf("gosdk: recover event odds (producer=%d, event=%s): %w", producerID, eventID.ToString(), mapRecoveryErr(err))
	}
	return h, nil
}

// RecoverEventStateful initiates a stateful-recovery for a single event.
// Same ctx/admission/lifecycle contract as RecoverEventOdds.
func (c *Client) RecoverEventStateful(ctx context.Context, producerID int, eventID types.URN) (*RecoveryHandle, error) {
	rmgr, err := c.readyRecoveryManager()
	if err != nil {
		return nil, fmt.Errorf("gosdk: recover event stateful (producer=%d, event=%s): %w", producerID, eventID.ToString(), err)
	}
	h, err := rmgr.InitiateEventStatefulRecoveryHandle(ctx, producerID, eventID)
	if err != nil {
		return nil, fmt.Errorf("gosdk: recover event stateful (producer=%d, event=%s): %w", producerID, eventID.ToString(), mapRecoveryErr(err))
	}
	return h, nil
}

// readyRecoveryManager admits a recovery initiation: under lifecycleMu
// it requires modeNormalReady and captures the MATCHING manager
// generation. Loading the atomic pointer alone (pre-fix) accepted
// recoveries in two windows where no functioning snapshot pipeline
// exists:
//
//   - During Connect: the manager is opened before the alive session,
//     so a recovery could be admitted, the alive-session open then
//     fail, and the rollback REPLACE the manager — orphaning the
//     handle lookup while the server may still process the request. A
//     retry against the fresh generation duplicates the recovery.
//   - During Close: modeClosing is entered before the manager closes,
//     so the teardown window accepted new work.
//
// Connecting/Closing now reject deterministically (retryable
// ErrManagerNotOpen vs terminal ErrAlreadyClosed); the mode check and
// pointer load under one lock pin the returned manager to the Ready
// generation. The actor-level admission handshake (sendCtxCommand)
// still covers Ready-vs-Close races after this gate.
func (c *Client) readyRecoveryManager() (*recovery.Manager, error) {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	switch c.mode {
	case modeNormalReady:
		return c.recoveryManager.Load(), nil
	case modeClosing, modeClosed:
		return nil, ErrAlreadyClosed
	default: // modeNew, modeBrokerOnly, modeNormalConnecting
		return nil, ErrManagerNotOpen
	}
}

// mapRecoveryErr translates internal recovery lifecycle sentinels to
// the public equivalents so consumers can errors.Is against exported
// gosdk errors (doc.go documents the ErrManagerNotOpen pattern; the
// internal/recovery package is unimportable). The internal error stays
// in the chain for diagnostics; %w-wrapping both keeps errors.Is
// working on either.
func mapRecoveryErr(err error) error {
	switch {
	case errors.Is(err, recovery.ErrManagerNotOpen):
		return fmt.Errorf("%w: %w", ErrManagerNotOpen, err)
	case errors.Is(err, recovery.ErrManagerClosed):
		return fmt.Errorf("%w: %w", ErrAlreadyClosed, err)
	default:
		return err
	}
}

// EventRecoveryStatus looks up a recovery by request id. Useful for
// callers that only kept the request id and want to check whether the
// recovery has completed. The second return value is false when the
// id is unknown — never registered or GC'd after the grace period.
func (c *Client) EventRecoveryStatus(requestID int) (types.RecoveryResult, bool) {
	h, ok := c.recoveryManager.Load().LookupHandle(requestID)
	if !ok {
		return types.RecoveryResult{}, false
	}
	return h.Snapshot(), true
}

// ProducerStatus returns the latest ProducerStatus snapshot for the
// producer. Polling fallback for the lossy RecoveryEvents channel —
// even if the consumer missed a transition event, the polling getter
// reflects the most recent state. Second return value is false when
// no actor exists for the id or no status has been emitted yet.
func (c *Client) ProducerStatus(producerID int) (types.ProducerStatus, bool) {
	return c.recoveryManager.Load().ProducerStatus(producerID)
}

// --- Sports info ---

// Sport returns a single sport by URN with every supplied locale
// merged into the returned Sport's Names/Abbreviations maps. When no
// locale is supplied, falls back to the configured default. Mirrors
// the multi-locale shape used by Match/Tournament/Competitor.
func (c *Client) Sport(ctx context.Context, id types.URN, locales ...types.Locale) (types.Sport, error) {
	s, err := c.sportsInfoManager.MultiLocalizedSport(ctx, id, c.localesOrDefault(locales))
	if err != nil {
		return types.Sport{}, fmt.Errorf("gosdk: sport %s (locales=%v): %w", id.ToString(), c.localesOrDefault(locales), err)
	}
	return s, nil
}

// Player returns a player profile by URN. With multiple locales, the
// cache is preloaded for every supplied locale (subsequent
// LocalizedPlayer calls hit the warm cache); the returned snapshot
// itself carries the primary locale (types.Player is currently
// single-locale-per-entry).
func (c *Client) Player(ctx context.Context, id types.URN, locales ...types.Locale) (types.Player, error) {
	p, err := c.sportsInfoManager.MultiLocalizedPlayer(ctx, id, c.localesOrDefault(locales))
	if err != nil {
		return types.Player{}, fmt.Errorf("gosdk: player %s (locales=%v): %w", id.ToString(), c.localesOrDefault(locales), err)
	}
	return p, nil
}

// Sports returns the sports catalog with every supplied locale merged
// into each Sport's Names/Abbreviations maps. Falls back to the
// configured default when no locale is supplied.
func (c *Client) Sports(ctx context.Context, locales ...types.Locale) ([]types.Sport, error) {
	s, err := c.sportsInfoManager.MultiLocalizedSports(ctx, c.localesOrDefault(locales))
	if err != nil {
		return nil, fmt.Errorf("gosdk: sports (locales=%v): %w", c.localesOrDefault(locales), err)
	}
	return s, nil
}

// ActiveTournaments returns active tournaments across all sports.
// All supplied locales are preloaded into each returned tournament's
// Names map (Java/.NET parity) — pass multiple to avoid a per-locale
// re-fetch in multi-language UIs.
func (c *Client) ActiveTournaments(ctx context.Context, locales ...types.Locale) ([]types.Tournament, error) {
	t, err := c.sportsInfoManager.MultiLocalizedActiveTournaments(ctx, c.localesOrDefault(locales))
	if err != nil {
		return nil, fmt.Errorf("gosdk: active tournaments (locales=%v): %w", c.localesOrDefault(locales), err)
	}
	return t, nil
}

// ActiveTournamentsForSport returns active tournaments under a sport
// identified by name. The name lookup runs against every supplied
// locale's catalog (not just the default), mirroring Java's
// SportsInfoManager.getActiveTournaments(sportName, locale) and
// .NET's ISportDataProvider.GetActiveTournaments(name).
func (c *Client) ActiveTournamentsForSport(ctx context.Context, sportName string, locales ...types.Locale) ([]types.Tournament, error) {
	t, err := c.sportsInfoManager.MultiLocalizedSportActiveTournaments(ctx, sportName, c.localesOrDefault(locales))
	if err != nil {
		return nil, fmt.Errorf("gosdk: active tournaments for sport %q (locales=%v): %w", sportName, c.localesOrDefault(locales), err)
	}
	return t, nil
}

// AvailableTournaments returns tournaments under a given sport.
func (c *Client) AvailableTournaments(ctx context.Context, sportID types.URN, locales ...types.Locale) ([]types.Tournament, error) {
	t, err := c.sportsInfoManager.MultiLocalizedAvailableTournaments(ctx, sportID, c.localesOrDefault(locales))
	if err != nil {
		return nil, fmt.Errorf("gosdk: available tournaments for sport %s (locales=%v): %w", sportID.ToString(), c.localesOrDefault(locales), err)
	}
	return t, nil
}

// Tournament returns a single tournament by URN. The owning sport is
// inferred from the cache so callers only need the tournament URN —
// matching the documented migration shape "client.Tournament(ctx,
// urn) for each urn in sport.TournamentIDs". All supplied locales are
// preloaded into the returned Tournament's Names map.
func (c *Client) Tournament(ctx context.Context, id types.URN, locales ...types.Locale) (types.Tournament, error) {
	t, err := c.sportsInfoManager.MultiLocalizedTournament(ctx, id, c.localesOrDefault(locales))
	if err != nil {
		return types.Tournament{}, fmt.Errorf("gosdk: tournament %s (locales=%v): %w", id.ToString(), c.localesOrDefault(locales), err)
	}
	return t, nil
}

// Match returns the match identified by URN.
func (c *Client) Match(ctx context.Context, id types.URN, locales ...types.Locale) (types.Match, error) {
	m, err := c.sportsInfoManager.MultiLocalizedMatch(ctx, id, c.localesOrDefault(locales))
	if err != nil {
		return types.Match{}, fmt.Errorf("gosdk: match %s (locales=%v): %w", id.ToString(), c.localesOrDefault(locales), err)
	}
	return m, nil
}

// MatchesFor returns matches scheduled for a calendar date.
func (c *Client) MatchesFor(ctx context.Context, t time.Time, locales ...types.Locale) ([]types.Match, error) {
	m, err := c.sportsInfoManager.MultiLocalizedMatchesFor(ctx, t, c.localesOrDefault(locales))
	if err != nil {
		return nil, fmt.Errorf("gosdk: matches for %s (locales=%v): %w", t.Format("2006-01-02"), c.localesOrDefault(locales), err)
	}
	return m, nil
}

// LiveMatches returns currently-live matches.
func (c *Client) LiveMatches(ctx context.Context, locales ...types.Locale) ([]types.Match, error) {
	m, err := c.sportsInfoManager.MultiLocalizedLiveMatches(ctx, c.localesOrDefault(locales))
	if err != nil {
		return nil, fmt.Errorf("gosdk: live matches (locales=%v): %w", c.localesOrDefault(locales), err)
	}
	return m, nil
}

// ListMatches paginates through the schedule. start is the offset and
// limit is the page size.
func (c *Client) ListMatches(ctx context.Context, start, limit int, locales ...types.Locale) ([]types.Match, error) {
	m, err := c.sportsInfoManager.MultiLocalizedListOfMatches(ctx, start, limit, c.localesOrDefault(locales))
	if err != nil {
		return nil, fmt.Errorf("gosdk: list matches start=%d limit=%d (locales=%v): %w", start, limit, c.localesOrDefault(locales), err)
	}
	return m, nil
}

// Competitor returns a competitor profile by URN.
func (c *Client) Competitor(ctx context.Context, id types.URN, locales ...types.Locale) (types.Competitor, error) {
	cp, err := c.sportsInfoManager.MultiLocalizedCompetitor(ctx, id, c.localesOrDefault(locales))
	if err != nil {
		return types.Competitor{}, fmt.Errorf("gosdk: competitor %s (locales=%v): %w", id.ToString(), c.localesOrDefault(locales), err)
	}
	return cp, nil
}

// FixtureChanges returns fixture changes since `after`.
func (c *Client) FixtureChanges(ctx context.Context, after time.Time, locales ...types.Locale) ([]types.FixtureChange, error) {
	fc, err := c.sportsInfoManager.MultiLocalizedFixtureChanges(ctx, c.localesOrDefault(locales), after)
	if err != nil {
		return nil, fmt.Errorf("gosdk: fixture changes after=%s (locales=%v): %w", after.Format(time.RFC3339), c.localesOrDefault(locales), err)
	}
	return fc, nil
}

// ClearMatch invalidates every cache entry for a match URN: the
// match summary, fixture, and live status. Mirrors Java
// SportsInfoManager.clearMatch and .NET DeleteMatchFromCache.
func (c *Client) ClearMatch(id types.URN) { c.sportsInfoManager.ClearMatch(id) }

// ClearFixture invalidates only the fixture cache entry. Useful when
// a fixture-only refresh is needed without disturbing the match
// summary or live status.
func (c *Client) ClearFixture(id types.URN) { c.sportsInfoManager.ClearFixture(id) }

// ClearMatchStatus invalidates only the match-status cache entry.
func (c *Client) ClearMatchStatus(id types.URN) { c.sportsInfoManager.ClearMatchStatus(id) }

// ClearTournament invalidates the cached tournament entry.
func (c *Client) ClearTournament(id types.URN) { c.sportsInfoManager.ClearTournament(id) }

// ClearCompetitor invalidates the cached competitor entry.
func (c *Client) ClearCompetitor(id types.URN) { c.sportsInfoManager.ClearCompetitor(id) }

// ClearPlayer invalidates every cached locale entry for a player.
func (c *Client) ClearPlayer(id types.URN) { c.sportsInfoManager.ClearPlayer(id) }

// ClearSport invalidates the cached sport entry.
func (c *Client) ClearSport(id types.URN) { c.sportsInfoManager.ClearSport(id) }

// --- Market descriptions ---

// MarketDescriptions returns all market descriptions with every
// supplied locale preloaded into the cache. The returned slice is
// snapshotted against the primary (first) locale; each entry's
// Names + outcome Names/Descriptions maps include all supplied
// locales (callers can read desc.Names[en] AND desc.Names[ru]
// without refetching). This matches the multi-locale preload
// semantics the rest of the manager surface exposes.
func (c *Client) MarketDescriptions(ctx context.Context, locales ...types.Locale) ([]types.MarketDescription, error) {
	mds, err := c.marketDescriptionManager.MultiLocalizedMarketDescriptions(ctx, c.localesOrDefault(locales))
	if err != nil {
		return nil, fmt.Errorf("gosdk: market descriptions (locales=%v): %w", c.localesOrDefault(locales), err)
	}
	return mds, nil
}

// MarketDescription returns the description for a (marketID, variant)
// tuple in the supplied locales (or the configured default locale when
// omitted). Pass `types.None[string]()` to select the base
// (non-variant) description; pass `types.Some("...")` for the dynamic
// variant catalog.
//
// Multiple locales preload all of them into the cache; the returned
// MarketDescription has its Names map populated for each.
func (c *Client) MarketDescription(ctx context.Context, id int, variant types.Optional[string], locales ...types.Locale) (*types.MarketDescription, error) {
	// Route through localesOrDefault like every other locale-variadic
	// method: the previous len-based branch forwarded the arguments
	// verbatim, so an explicitly empty types.Locale("") reached the API
	// path builder (a malformed request path) and duplicates triggered
	// redundant preloads — the exact defects the helper exists to drop.
	md, err := c.marketDescriptionManager.LocalizedMarketDescriptionByIDAndVariant(ctx, id, variant, c.localesOrDefault(locales)...)
	if err != nil {
		return nil, fmt.Errorf("gosdk: market description %d/%v (locales=%v): %w", id, variant, locales, err)
	}
	return md, nil
}

// MarketVoidReasons returns the void-reasons catalog.
func (c *Client) MarketVoidReasons(ctx context.Context) ([]types.MarketVoidReason, error) {
	r, err := c.marketDescriptionManager.MarketVoidReasons(ctx)
	if err != nil {
		return nil, fmt.Errorf("gosdk: market void reasons: %w", err)
	}
	return r, nil
}

// ReloadMarketVoidReasons forces a refetch of the void-reasons catalog.
func (c *Client) ReloadMarketVoidReasons(ctx context.Context) ([]types.MarketVoidReason, error) {
	r, err := c.marketDescriptionManager.ReloadMarketVoidReasons(ctx)
	if err != nil {
		return nil, fmt.Errorf("gosdk: reload market void reasons: %w", err)
	}
	return r, nil
}

// ClearMarketVoidReasons evicts the void-reasons catalog. The next call
// to MarketVoidReasons / ReloadMarketVoidReasons refetches from the API.
func (c *Client) ClearMarketVoidReasons() {
	c.marketDescriptionManager.ClearMarketVoidReasons()
}

// ClearMarketDescription invalidates a cached description.
// `types.None[string]()` targets the base (non-variant) entry;
// `types.Some(v)` targets the (id, v) tuple.
func (c *Client) ClearMarketDescription(marketID int, variant types.Optional[string]) {
	c.marketDescriptionManager.ClearMarketDescription(marketID, variant)
}

// --- Replay ---

// Replay returns the replay subtype for replay-API operations. Nil is
// never returned; the subtype is bound to this Client's lifetime.
//
// Cache isolation: replay subscriptions share the live Client's cache
// manager (one cache per Client by design), but the session loop
// deliberately skips cacheManager.OnFeedMessageReceived for replay
// traffic — a historical FixtureChange / OddsChange / BetSettlement
// arriving from the replay exchange will not invalidate or mutate
// live cache entries. Replay consumers still receive every message
// via the subscription's Messages() channel; only cache-side
// bookkeeping is gated. Mirrors .NET's ReplayFeed isolation pattern.
// Recovery and the AMQP exchange are also isolated per-session:
// replay subscriptions use the dedicated replay exchange and a
// no-op recovery manager.
func (c *Client) Replay() *Replay { return c.replay }

// Replay groups the replay-API operations under a dedicated subtype.
type Replay struct {
	client *Client
}

// List returns the replay queue contents as Match value snapshots.
// Resolves each queued URN to a Match (one sports-info call per ID).
// Use EventIDs when only the IDs are needed.
func (r *Replay) List(ctx context.Context) ([]types.Match, error) {
	m, err := r.client.replayManager.ReplayList(ctx)
	if err != nil {
		return nil, fmt.Errorf("gosdk: replay list: %w", err)
	}
	return m, nil
}

// EventIDs returns the queued event URNs without resolving them to
// Match values. Mirrors .NET's IReplayManager.GetEventsInQueue —
// useful for "is this URN queued?" checks where building a Match per
// entry would be wasted work.
func (r *Replay) EventIDs(ctx context.Context) ([]types.URN, error) {
	ids, err := r.client.replayManager.ReplayEventIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("gosdk: replay event ids: %w", err)
	}
	return ids, nil
}

// AddEvent adds an event to the replay queue.
func (r *Replay) AddEvent(ctx context.Context, eventID types.URN) error {
	if _, err := r.client.replayManager.AddSportEventID(ctx, eventID); err != nil {
		return fmt.Errorf("gosdk: replay add event %s: %w", eventID.ToString(), err)
	}
	return nil
}

// RemoveEvent removes an event from the replay queue.
func (r *Replay) RemoveEvent(ctx context.Context, eventID types.URN) error {
	if _, err := r.client.replayManager.RemoveSportEventID(ctx, eventID); err != nil {
		return fmt.Errorf("gosdk: replay remove event %s: %w", eventID.ToString(), err)
	}
	return nil
}

// Start begins replay playback with the supplied options.
func (r *Replay) Start(ctx context.Context, opts ...ReplayOption) error {
	params := types.ReplayPlayParams{}
	for _, opt := range opts {
		opt(&params)
	}
	if _, err := r.client.replayManager.Play(ctx, params); err != nil {
		return fmt.Errorf("gosdk: replay start: %w", err)
	}
	return nil
}

// Stop pauses replay playback. Queue contents are preserved.
func (r *Replay) Stop(ctx context.Context) error {
	if _, err := r.client.replayManager.Stop(ctx); err != nil {
		return fmt.Errorf("gosdk: replay stop: %w", err)
	}
	return nil
}

// Clear empties the replay queue.
func (r *Replay) Clear(ctx context.Context) error {
	if _, err := r.client.replayManager.Clear(ctx); err != nil {
		return fmt.Errorf("gosdk: replay clear: %w", err)
	}
	return nil
}

// StopAndClear stops playback and empties the queue. Parity with .NET.
// Reports the failing stage explicitly so callers can tell whether
// playback was paused before the queue clear failed.
func (r *Replay) StopAndClear(ctx context.Context) error {
	if _, err := r.client.replayManager.Stop(ctx); err != nil {
		return fmt.Errorf("gosdk: replay stop-and-clear: stop: %w", err)
	}
	if _, err := r.client.replayManager.Clear(ctx); err != nil {
		return fmt.Errorf("gosdk: replay stop-and-clear: clear: %w", err)
	}
	return nil
}

// Status reports the current replay-engine state. The returned string
// is opaque (set by the engine, typically "playing"/"stopped"/"paused").
// Backed by GET /replay/status. The .NET SDK exposes an equivalent; the
// Java SDK does not publicly surface this endpoint.
func (r *Replay) Status(ctx context.Context) (string, error) {
	s, err := r.client.replayManager.Status(ctx)
	if err != nil {
		return "", fmt.Errorf("gosdk: replay status: %w", err)
	}
	return s, nil
}

// ReplayOption tunes a Replay.Start invocation.
type ReplayOption func(*types.ReplayPlayParams)

// WithReplaySpeed scales playback speed (e.g. 10 = ten times realtime).
func WithReplaySpeed(speed int) ReplayOption {
	return func(p *types.ReplayPlayParams) { p.Speed = types.Some(speed) }
}

// WithReplayMaxDelayMs caps the delay between consecutive messages.
func WithReplayMaxDelayMs(ms int) ReplayOption {
	return func(p *types.ReplayPlayParams) { p.MaxDelayInMs = types.Some(ms) }
}

// WithReplayRunParallel runs events in parallel rather than sequentially.
func WithReplayRunParallel(parallel bool) ReplayOption {
	return func(p *types.ReplayPlayParams) { p.RunParallel = types.Some(parallel) }
}

// WithReplayRewriteTimestamps rewrites historical timestamps to "now".
func WithReplayRewriteTimestamps(rewrite bool) ReplayOption {
	return func(p *types.ReplayPlayParams) { p.RewriteTimestamps = types.Some(rewrite) }
}

// WithReplayProducer narrows replay to a specific producer name.
func WithReplayProducer(producer string) ReplayOption {
	return func(p *types.ReplayPlayParams) { p.Producer = types.Some(producer) }
}

// --- Helpers ---

// localesOrDefault normalises the supplied variadic `locales` before they
// are threaded into the multi-locale manager methods (so every requested
// locale is preloaded into the returned entity's Names map — Java/.NET
// preload parity).
//
// It drops empty locales and de-duplicates while preserving order. An
// empty types.Locale("") passed explicitly would otherwise reach the API
// path builder verbatim (pathSeg("")) and produce a malformed request
// path like "/sports//sports" that fails with a confusing transport error
// instead of doing nothing useful; a duplicate locale would trigger a
// redundant preload. When the result is empty — no locales supplied, or
// every supplied locale was empty — the configured default is used, the
// same fallback the variadic API has always applied for the no-argument
// call.
func (c *Client) localesOrDefault(locales []types.Locale) []types.Locale {
	out := make([]types.Locale, 0, len(locales))
	seen := make(map[types.Locale]struct{}, len(locales))
	for _, l := range locales {
		if l == "" {
			continue
		}
		if _, dup := seen[l]; dup {
			continue
		}
		seen[l] = struct{}{}
		out = append(out, l)
	}
	if len(out) > 0 {
		return out
	}
	return []types.Locale{c.cfg.DefaultLocale()}
}

func mapToSlice(m map[int]types.Producer) []types.Producer {
	out := make([]types.Producer, 0, len(m))
	for _, p := range m {
		out = append(out, p)
	}
	// Sort by ascending producer ID for a DETERMINISTIC order. The source
	// is a Go map, whose iteration order is randomized, so without this the
	// public producer lists (Producers / ActiveProducers /
	// ProducersInScope) would return a different order on every call —
	// making a caller's "pick prods[0]" an arbitrary producer each run.
	slices.SortFunc(out, func(a, b types.Producer) int { return cmp.Compare(a.ID(), b.ID()) })
	return out
}
