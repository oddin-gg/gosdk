package gosdk

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/oddin-gg/gosdk/types"
)

// Config is the public, immutable SDK configuration. Construct via NewConfig.
//
// Internal layers consume it through the internal/config.Config
// interface via configAdapter (config_adapter.go) — the legacy
// configuration.go setter-chain type is gone.
type Config struct {
	accessToken          string
	defaultLocale        types.Locale
	preloadLocales       []types.Locale
	maxInactivity        time.Duration
	maxRecoveryExecution time.Duration
	// snapshotCompleteDeadline is how long a snapshot recovery may go
	// without a message of its own (nor its snapshot_complete) before it
	// is re-requested; snapshotDeadlineSet records that an option chose it,
	// so NewConfig can lower the default to fit a small
	// maxRecoveryExecution.
	snapshotCompleteDeadline time.Duration
	snapshotDeadlineSet      bool
	initialSnapshotTime      time.Duration
	staleMessageLimit        time.Duration
	staleMessageWindow       time.Duration
	httpClientTimeout        time.Duration
	messagingPort            int
	sdkNodeID                *int
	selectedEnvironment      types.Environment
	selectedRegion           types.Region
	reportExtendedData       bool
	forcedAPIHost            string
	forcedMQHost             string
	exchangeName             string
	replayExchangeName       string
	sportIDPrefix            string
	exceptionStrategy        ExceptionStrategy
	logger                   *slog.Logger
	apiCallLogging           APILogLevel
	apiCallBodyLimit         int
	amqpPrefetch             int
	subscriptionBuffer       int
	httpClient               *http.Client
	shutdownTimeout          time.Duration
	messageNameResolution    bool
}

// redactedToken is what every Config formatting path shows in place of
// the access token.
const redactedToken = "[REDACTED]"

// String implements fmt.Stringer with the access token REDACTED.
// Config carries the credential in a plain field, so ordinary
// formatting — %v, %+v, %#v, slog.Any("config", cfg), a stray
// log.Printf — would otherwise hand the token to log storage even
// though the field is unexported. Every formatting entry point routes
// through this redacted rendering.
func (c Config) String() string {
	return fmt.Sprintf(
		"gosdk.Config{accessToken:%s env:%v region:%q defaultLocale:%s preloadLocales:%v node:%v apiHost:%q mqHost:%q port:%d exchange:%q replayExchange:%q maxInactivity:%v maxRecoveryExecution:%v snapshotCompleteDeadline:%v staleMessageLimit:%v staleMessageWindow:%v httpTimeout:%v shutdownTimeout:%v strategy:%v extendedData:%v apiLog:%v prefetch:%d subBuffer:%d messageNames:%t}",
		redactedToken, c.selectedEnvironment, c.selectedRegion, c.defaultLocale, c.preloadLocales,
		c.sdkNodeID, c.forcedAPIHost, c.forcedMQHost, c.messagingPort, c.exchangeName,
		c.replayExchangeName, c.maxInactivity, c.maxRecoveryExecution, c.snapshotCompleteDeadline, c.staleMessageLimit, c.staleMessageWindow, c.httpClientTimeout,
		c.shutdownTimeout, c.exceptionStrategy, c.reportExtendedData, c.apiCallLogging,
		c.amqpPrefetch, c.subscriptionBuffer, c.messageNameResolution,
	)
}

// GoString implements fmt.GoStringer so %#v is redacted too (fmt
// prefers GoString over String for %#v).
func (c Config) GoString() string { return c.String() }

// LogValue implements slog.LogValuer so slog.Any("config", cfg) logs
// the redacted rendering instead of walking the raw struct fields.
func (c Config) LogValue() slog.Value { return slog.StringValue(c.String()) }

// Option mutates a Config draft inside NewConfig. Closures don't escape
// NewConfig, so the returned Config is effectively immutable.
type Option func(*Config)

// ExceptionStrategy controls how the SDK handles in-band message-pipeline
// errors. Affects only the AMQP decode-and-route step (see NEXT.md §10);
// API-call methods always return errors directly per Go idiom.
type ExceptionStrategy int

const (
	// StrategyCatch logs the error and emits an Unparsable message into the
	// subscription. Subscription stays alive. Default.
	StrategyCatch ExceptionStrategy = iota

	// StrategyThrow terminates the subscription via Sub.Err().
	StrategyThrow
)

// APILogLevel controls verbosity of API-call observability events.
type APILogLevel int

const (
	// APILogOff emits structured slog at debug only, no APIEvent emission.
	APILogOff APILogLevel = iota
	// APILogMetadata emits method/url/status/latency, no bodies.
	APILogMetadata
	// APILogResponses emits response body bytes (typical debug setting).
	APILogResponses
	// APILogFull emits both request and response bytes (heavy).
	APILogFull
)

// Defaults documented in NEXT.md.
const (
	defaultMaxInactivity        = 20 * time.Second
	defaultMaxRecoveryExecution = 6 * time.Hour
	// defaultSnapshotCompleteDeadline: a snapshot recovery normally ends
	// within seconds; five minutes without a sign of life means the
	// request or its snapshot_complete was lost.
	defaultSnapshotCompleteDeadline = 5 * time.Minute
	// defaultStaleMessageLimit and defaultStaleMessageWindow: a subscription
	// whose messages are two minutes old for a whole minute is too far
	// behind for the broker's queue to be worth keeping.
	defaultStaleMessageLimit    = 2 * time.Minute
	defaultStaleMessageWindow   = time.Minute
	defaultHTTPClientTimeoutPub = 30 * time.Second
	defaultMessagingPort        = 5672
	defaultExchangeName         = "oddinfeed"
	defaultReplayExchangeName   = "oddinreplay"
	defaultSportIDPrefix        = "od:sport:"
	defaultAPIBodyLimitBytes    = 64 * 1024
	defaultAMQPPrefetch         = 1000
	defaultSubscriptionBuffer   = 256
	defaultShutdownTimeout      = 5 * time.Second
)

// NewConfig constructs an SDK Config. The required arguments are the access
// token and the target environment; everything else is supplied via options.
func NewConfig(token string, env types.Environment, opts ...Option) Config {
	cfg := Config{
		accessToken:              token,
		selectedEnvironment:      env,
		defaultLocale:            types.EnLocale,
		maxInactivity:            defaultMaxInactivity,
		maxRecoveryExecution:     defaultMaxRecoveryExecution,
		snapshotCompleteDeadline: defaultSnapshotCompleteDeadline,
		staleMessageLimit:        defaultStaleMessageLimit,
		staleMessageWindow:       defaultStaleMessageWindow,
		httpClientTimeout:        defaultHTTPClientTimeoutPub,
		messagingPort:            defaultMessagingPort,
		exchangeName:             defaultExchangeName,
		replayExchangeName:       defaultReplayExchangeName,
		sportIDPrefix:            defaultSportIDPrefix,
		exceptionStrategy:        StrategyCatch,
		apiCallBodyLimit:         defaultAPIBodyLimitBytes,
		amqpPrefetch:             defaultAMQPPrefetch,
		subscriptionBuffer:       defaultSubscriptionBuffer,
		shutdownTimeout:          defaultShutdownTimeout,
		messageNameResolution:    true,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	// The default deadline must not exceed a smaller MaxRecoveryExecution
	// the caller chose (validation rejects deadline > cap, and callers
	// that only lowered the cap never asked for a deadline).
	if !cfg.snapshotDeadlineSet && cfg.maxRecoveryExecution > 0 && cfg.snapshotCompleteDeadline > cfg.maxRecoveryExecution {
		cfg.snapshotCompleteDeadline = cfg.maxRecoveryExecution
	}
	return cfg
}

// --- Options ---

// WithNodeID sets the optional SDK node id (used in routing keys + recovery).
func WithNodeID(id int) Option { return func(c *Config) { v := id; c.sdkNodeID = &v } }

// WithDefaultLocale sets the locale used when a query method is called
// without an explicit `locales ...types.Locale` argument.
func WithDefaultLocale(l types.Locale) Option {
	return func(c *Config) { c.defaultLocale = l }
}

// WithPreloadLocales lists locales to fetch eagerly when warming static
// catalogs (sports, market descriptions). The warm happens DURING
// gosdk.New — a warm failure fails construction, same as the who-am-i
// probe (NEXT.md §8). Per-event entities are still fetched lazily per
// locale on first request.
func WithPreloadLocales(locales ...types.Locale) Option {
	return func(c *Config) {
		c.preloadLocales = append([]types.Locale(nil), locales...)
	}
}

// WithMessageNameResolution toggles market/outcome name resolution during
// AMQP message construction. Default true.
//
// Resolution costs, per market and on the single session goroutine, one
// description-cache lookup for the English locale and two for every
// other configured locale (the outcome lookup additionally requests the
// English catalog label, the locale-independent identity the home/away
// substitution keys on), plus a locked map read per outcome per locale
// — and I/O when the cache is cold (an unseen market, variant or
// player). Set false when the consumer takes its names from the
// catalog API instead: Names maps are then nil and Market.Name /
// Outcome.Name report None for every locale. Ids, specifiers, odds,
// status and settlement results are unaffected, as is the catalog API.
//
// Market and outcome names are the ONLY thing this gates. Event,
// tournament, sport and competitor names travel on the same message,
// come from their own caches, and resolve either way.
//
// The catalog API is NOT a drop-in replacement for the message names.
// The names the factory writes into Names are COMPOSED: "{specifier}"
// placeholders in the catalog template are filled from the market's
// specifiers, a home/away specifier value and the home/away placeholder
// outcomes are replaced with the event's localized competitor names, and
// player-props entities resolve to player names. Client.MarketDescription
// returns the raw catalog template with none of that applied. Opt out only when the
// consumer needs no market/outcome display names at all, or is prepared
// to compose them itself. Client.New logs at Info level when the option
// is off so a later reader of an unexpected None can find the cause.
func WithMessageNameResolution(enabled bool) Option {
	return func(c *Config) { c.messageNameResolution = enabled }
}

// WithRegion selects the AWS region suffix for the broker / API host
// (e.g. types.APSouthEast1). Defaults to RegionDefault (EU).
func WithRegion(r types.Region) Option { return func(c *Config) { c.selectedRegion = r } }

// WithAPIHost overrides the resolved API host (otherwise derived from
// the environment + region). Pass a bare host like "api.example.com",
// not a full URL — the SDK builds `https://<host>/v1/...` itself. Values
// starting with `http://`, `https://`, `amqp://`, or `amqps://` are
// rejected: they would produce malformed URLs at request time. The
// pre-v2 name `WithAPIURL` is gone — same behaviour as that option,
// just an honest name.
func WithAPIHost(host string) Option {
	return func(c *Config) {
		c.forcedAPIHost = stripOrRejectScheme("WithAPIHost", host)
	}
}

// WithMQHost overrides the resolved AMQP host. Pass a bare host like
// "mq.example.com", not a URL — the SDK builds the AMQP URL itself.
// Values with `http://`, `https://`, `amqp://`, `amqps://` schemes are
// rejected. Pre-v2 name `WithMQURL` is gone.
func WithMQHost(host string) Option {
	return func(c *Config) {
		c.forcedMQHost = stripOrRejectScheme("WithMQHost", host)
	}
}

// stripOrRejectScheme panics with a clear message when a caller passes
// a scheme-prefixed value to WithAPIHost / WithMQHost. Panic at config
// construction is the right severity: the alternative is silently
// producing malformed URLs at every API / AMQP call later, which is
// strictly harder to diagnose. Returning an error from an Option
// would force every caller to check Option errors — far more
// disruptive for what is unambiguously a programmer error.
func stripOrRejectScheme(opt, v string) string {
	for _, prefix := range []string{"http://", "https://", "amqp://", "amqps://"} {
		if len(v) >= len(prefix) && v[:len(prefix)] == prefix {
			// The hint is option-specific: the AMQP dialer appends the
			// port from WithMessagingPort itself, so a "host:port" MQ
			// value would build "host:port:port" and fail to dial. The
			// API URL, by contrast, does accept an explicit :port.
			hint := "pass a bare host like \"api.example.com\" (an explicit :port is allowed); the SDK builds the URL"
			if opt == "WithMQHost" {
				hint = "pass a bare hostname with no scheme and no port; set the AMQP port separately via WithMessagingPort"
			}
			panic(opt + ": expected bare host, got URL with scheme " + prefix + " — " + hint)
		}
	}
	return v
}

// WithMessagingPort overrides the AMQP TLS port (default 5672).
func WithMessagingPort(port int) Option { return func(c *Config) { c.messagingPort = port } }

// WithExchangeName overrides the AMQP exchange name (default "oddinfeed").
func WithExchangeName(name string) Option { return func(c *Config) { c.exchangeName = name } }

// WithReplayExchangeName overrides the replay exchange name (default "oddinreplay").
func WithReplayExchangeName(name string) Option {
	return func(c *Config) { c.replayExchangeName = name }
}

// WithSportIDPrefix overrides the URN prefix used to construct sport URNs
// from routing keys (default "od:sport:").
func WithSportIDPrefix(prefix string) Option { return func(c *Config) { c.sportIDPrefix = prefix } }

// WithMaxInactivity caps the max time without an alive message before a
// producer is considered down (default 20s). It is also how far behind the
// producer a subscription may process its messages before the producer is
// reported down for the processing delay, and how long a client callback may stay
// in one message before the SDK takes the subscription's queue for stuck.
func WithMaxInactivity(d time.Duration) Option { return func(c *Config) { c.maxInactivity = d } }

// WithMaxRecoveryExecution caps the max time a single recovery request may take
// (default 6h): a snapshot recovery or an event recovery that has not completed
// within d of being asked for has failed. A failed snapshot recovery is asked for
// again after 5, 10 and 20 seconds, and then every 60 seconds, with the producer
// down; a failed event recovery ends with RecoveryStatusTimedOut. Enforcement is
// periodic, not exact: the recovery actor looks at the clock once a second, so
// the observed timeout may overshoot d by up to a second.
//
// d does not bound how far back a recovery reaches. A snapshot recovery starts
// from the oldest point a subscription may be missing messages from, as far back
// as the producer's stateful recovery window allows (the SDK logs a warning when
// it has to start later); after a long outage the replay is as large as that
// window.
func WithMaxRecoveryExecution(d time.Duration) Option {
	return func(c *Config) { c.maxRecoveryExecution = d }
}

// WithSnapshotCompleteDeadline sets how long a recovery waits for its
// snapshot_complete after it was accepted, or after a subscription that awaits it
// last took something that says it is on its way (default 5m). When it passes
// with no sign of life the recovery has failed: a snapshot recovery is asked for
// again under a NEW request id from the same point, after a backoff of 5, 10 and
// 20 seconds and then every 60 seconds, and an accepted event recovery ends with
// RecoveryStatusTimedOut. A snapshot_complete of the abandoned request is ignored.
// A subscription whose client callback has been in one message for longer than
// WithMaxInactivity counts as having heard something: its queue is stuck behind
// the message and so is the snapshot, and asking again would only queue a second
// one there. WithMaxRecoveryExecution still bounds a request as a whole. d must
// be > 0 and must not exceed WithMaxRecoveryExecution; when only
// WithMaxRecoveryExecution is lowered below the default, the default follows it.
// Enforcement is periodic (the recovery actor's one-second look at the clock).
//
// A recovery completes only once every subscription that receives the producer
// has taken its snapshot_complete.
func WithSnapshotCompleteDeadline(d time.Duration) Option {
	return func(c *Config) {
		c.snapshotCompleteDeadline = d
		c.snapshotDeadlineSet = true
	}
}

// WithStaleMessageLimit sets how old a subscription's live messages may be, by
// the producer's clock, before the SDK counts it as too far behind (default 2m).
// d must be more than 10s and at most 24h; New rejects any other value as
// ErrInvalidConfig.
//
// A subscription whose messages stay older than d for WithStaleMessageWindow is
// behind by a backlog that only grows in the broker's queue for it. The SDK then
// asks for a recovery of every producer the subscription receives, and once the
// API has accepted them all it replaces the subscription's AMQP channel: the
// queue and what it held are dropped, and the recovery repeats what was dropped.
// Messages that were prefetched but not delivered yet (up to WithAMQPPrefetch,
// 1000 by default) are dropped with the queue, and the recovery repeats them too,
// so a client may see a message twice, never a hole. A subscription is reset at
// most three times in ten minutes, with a pause of 1, 2 and 4 minutes between the
// resets; after that it is only logged as lagging until it catches up or the
// first reset ages out. The producers it receives are down with
// ProcessingQueueDelayViolation while a reset is made.
//
// Age is measured against the offset of the producer's clock taken from its
// alives, and the net is off while that offset is stale.
func WithStaleMessageLimit(d time.Duration) Option {
	return func(c *Config) { c.staleMessageLimit = d }
}

// WithStaleMessageWindow sets how long a subscription's live messages have to stay
// older than WithStaleMessageLimit before the SDK acts on it (default 1m). d must
// be at least 1s, the SDK's own clock tick, and at most 24h; New rejects any other
// value as ErrInvalidConfig.
func WithStaleMessageWindow(d time.Duration) Option {
	return func(c *Config) { c.staleMessageWindow = d }
}

// WithInitialSnapshotTime sets the duration to look back when issuing the
// first snapshot recovery of a producer that has no recovery start (see
// Client.SetProducerRecoveryFromTimestamp). Zero leaves the default, the
// producer's full history.
func WithInitialSnapshotTime(d time.Duration) Option {
	return func(c *Config) { c.initialSnapshotTime = d }
}

// WithHTTPClientTimeout overrides the per-request timeout on the API client.
// Default 30s; valid range is up to 60s in practice.
//
// It does not bound how long a feed message waits for the API: a message
// waits at most 2 s in total for the loads its event needs (see "When the REST
// API is down" in the README), while the loads themselves run on with this
// timeout and the getters' retries and fill the caches for later messages. A
// request that times out after it was sent counts as a failure of the API and
// opens the breaker of its endpoint for 10 s. A cold event build makes several
// sequential requests (summary, tournament, competitors, fixture, market
// catalog per locale), so with an API that answers each in more than roughly
// 250 ms the first messages of a new match may be delivered with a nil event
// and no market names. Use WithPreloadLocales to take the market catalog out
// of that build.
func WithHTTPClientTimeout(d time.Duration) Option {
	return func(c *Config) { c.httpClientTimeout = d }
}

// WithExceptionStrategy controls the in-band message-decode failure mode
// (Catch = log + Unparsable; Throw = terminate subscription). Default Catch.
func WithExceptionStrategy(s ExceptionStrategy) Option {
	return func(c *Config) { c.exceptionStrategy = s }
}

// WithLogger injects the logger used for SDK diagnostics. Pass nil for the
// default text-handler logger on stderr at info level.
func WithLogger(l *slog.Logger) Option { return func(c *Config) { c.logger = l } }

// WithExtendedDataReporting toggles emission of RawFeed messages on
// Subscription.Messages() — the per-message wire bytes for diagnostic tools.
func WithExtendedDataReporting(b bool) Option {
	return func(c *Config) { c.reportExtendedData = b }
}

// WithAPICallLogging enables the APIEvents() channel and selects verbosity.
// Default APILogOff (slog-debug only).
func WithAPICallLogging(level APILogLevel) Option {
	return func(c *Config) { c.apiCallLogging = level }
}

// WithAPICallBodyLimit caps the captured body size on each APIEvent
// (default 64 KiB). Bodies above the cap are truncated; the
// `APIEvent.Truncated` flag is set.
func WithAPICallBodyLimit(bytes int) Option {
	return func(c *Config) { c.apiCallBodyLimit = bytes }
}

// WithAMQPPrefetch sets the broker-side prefetch (max unacked deliveries
// per consumer). Default 1000. Accepted range 0..65535 — 0 selects the
// default, and the upper bound is protocol-level (AMQP basic.qos
// carries the count as uint16; larger values would silently wrap, with
// 65536 becoming UNLIMITED prefetch). Values outside the range are
// rejected by New as ErrInvalidConfig.
func WithAMQPPrefetch(n int) Option { return func(c *Config) { c.amqpPrefetch = n } }

// WithSubscriptionBuffer sets the size of the in-process subscription
// channel buffer. Default 256.
func WithSubscriptionBuffer(n int) Option { return func(c *Config) { c.subscriptionBuffer = n } }

// WithHTTPClient overrides the *http.Client used for REST API calls.
// Useful for custom TLS config, transport-level instrumentation, or
// integration tests that route through an httptest.Server. Pass nil to
// keep the default.
//
// The supplied client MUST set a non-zero Timeout. The SDK detaches
// shared cache loads from caller cancellation (singleflight), so a
// stuck TCP connect on a zero-Timeout client could otherwise hold the
// singleflight slot for the full process lifetime, jamming every other
// caller of the same key. Passing a client with Timeout == 0 panics at
// option-application time — fail loud, fail fast, surface the
// misconfiguration before any production traffic flows.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Config) {
		if h == nil {
			c.httpClient = nil
			return
		}
		if h.Timeout <= 0 {
			panic("WithHTTPClient: client.Timeout must be > 0; the SDK's detached cache loads cannot rely on caller cancellation, so a zero-timeout client risks indefinite goroutine pinning")
		}
		// Shallow-copy: storing (and later returning) the CALLER's
		// pointer let them zero the Timeout after validation, or race
		// field mutation against in-flight requests — violating Config's
		// immutability contract. The copy shares the Transport (so
		// connection pooling is preserved) but pins the validated
		// scalar fields.
		hc := *h
		c.httpClient = &hc
	}
}

// WithShutdownTimeout caps the total time the SDK spends on shutdown
// work (Client.Close teardown, Subscription.Close drain, partial-init
// rollback inside Connect). The same budget is shared across session and
// broker teardown so a stuck broker can't compound across sub-shutdowns.
// Note Client.Close is ABRUPT for active subscriptions — the graceful
// drain is Subscription.Close. Default 5s. Lower in tests for faster
// failure; raise for slow brokers.
func WithShutdownTimeout(d time.Duration) Option {
	return func(c *Config) { c.shutdownTimeout = d }
}

// HTTPClient returns the configured custom http.Client (nil when
// unset). Returns a shallow COPY — Config's stored client must stay
// immutable; the copy shares the Transport, so wiring it preserves
// connection pooling.
func (c Config) HTTPClient() *http.Client {
	if c.httpClient == nil {
		return nil
	}
	hc := *c.httpClient
	return &hc
}

// --- Read-only accessors (some are needed across packages once Client lands) ---

// AccessToken returns the configured token.
func (c Config) AccessToken() string { return c.accessToken }

// DefaultLocale returns the configured default locale.
func (c Config) DefaultLocale() types.Locale { return c.defaultLocale }

// MessageNameResolution reports whether message construction resolves names.
func (c Config) MessageNameResolution() bool { return c.messageNameResolution }

// PreloadLocales returns a copy of the preload locale list.
func (c Config) PreloadLocales() []types.Locale {
	out := make([]types.Locale, len(c.preloadLocales))
	copy(out, c.preloadLocales)
	return out
}

// Environment returns the selected environment.
func (c Config) Environment() types.Environment { return c.selectedEnvironment }

// Region returns the selected region.
func (c Config) Region() types.Region { return c.selectedRegion }

// SdkNodeID returns the configured node id (nil if unset).
func (c Config) SdkNodeID() *int {
	if c.sdkNodeID == nil {
		return nil
	}
	v := *c.sdkNodeID
	return &v
}

// Logger returns the configured *slog.Logger or nil if none was set.
func (c Config) Logger() *slog.Logger { return c.logger }

// MaxInactivity returns the configured inactivity threshold.
func (c Config) MaxInactivity() time.Duration { return c.maxInactivity }

// MaxRecoveryExecution returns the configured recovery cap.
func (c Config) MaxRecoveryExecution() time.Duration { return c.maxRecoveryExecution }

// SnapshotCompleteDeadline returns the configured snapshot-complete
// deadline.
func (c Config) SnapshotCompleteDeadline() time.Duration { return c.snapshotCompleteDeadline }

// StaleMessageLimit returns the configured stale-message limit.
func (c Config) StaleMessageLimit() time.Duration { return c.staleMessageLimit }

// StaleMessageWindow returns the configured stale-message window.
func (c Config) StaleMessageWindow() time.Duration { return c.staleMessageWindow }

// ShutdownTimeout returns the configured graceful-shutdown cap.
func (c Config) ShutdownTimeout() time.Duration { return c.shutdownTimeout }
