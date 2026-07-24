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
	initialSnapshotTime  time.Duration
	httpClientTimeout    time.Duration
	messagingPort        int
	sdkNodeID            *int
	selectedEnvironment  types.Environment
	selectedRegion       types.Region
	reportExtendedData   bool
	forcedAPIHost        string
	forcedMQHost         string
	exchangeName         string
	replayExchangeName   string
	sportIDPrefix        string
	exceptionStrategy    ExceptionStrategy
	logger               *slog.Logger
	apiCallLogging       APILogLevel
	apiCallBodyLimit     int
	amqpPrefetch         int
	subscriptionBuffer   int
	httpClient           *http.Client
	shutdownTimeout      time.Duration
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
		"gosdk.Config{accessToken:%s env:%v region:%q defaultLocale:%s preloadLocales:%v node:%v apiHost:%q mqHost:%q port:%d exchange:%q replayExchange:%q maxInactivity:%v maxRecoveryExecution:%v httpTimeout:%v shutdownTimeout:%v strategy:%v extendedData:%v apiLog:%v prefetch:%d subBuffer:%d}",
		redactedToken, c.selectedEnvironment, c.selectedRegion, c.defaultLocale, c.preloadLocales,
		c.sdkNodeID, c.forcedAPIHost, c.forcedMQHost, c.messagingPort, c.exchangeName,
		c.replayExchangeName, c.maxInactivity, c.maxRecoveryExecution, c.httpClientTimeout,
		c.shutdownTimeout, c.exceptionStrategy, c.reportExtendedData, c.apiCallLogging,
		c.amqpPrefetch, c.subscriptionBuffer,
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
		accessToken:          token,
		selectedEnvironment:  env,
		defaultLocale:        types.EnLocale,
		maxInactivity:        defaultMaxInactivity,
		maxRecoveryExecution: defaultMaxRecoveryExecution,
		httpClientTimeout:    defaultHTTPClientTimeoutPub,
		messagingPort:        defaultMessagingPort,
		exchangeName:         defaultExchangeName,
		replayExchangeName:   defaultReplayExchangeName,
		sportIDPrefix:        defaultSportIDPrefix,
		exceptionStrategy:    StrategyCatch,
		apiCallBodyLimit:     defaultAPIBodyLimitBytes,
		amqpPrefetch:         defaultAMQPPrefetch,
		subscriptionBuffer:   defaultSubscriptionBuffer,
		shutdownTimeout:      defaultShutdownTimeout,
	}
	for _, opt := range opts {
		opt(&cfg)
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
// producer is considered down (default 20s).
func WithMaxInactivity(d time.Duration) Option { return func(c *Config) { c.maxInactivity = d } }

// WithMaxRecoveryExecution caps the max time a single recovery may run
// (default 6h). Enforcement is periodic, not exact: a stuck recovery is
// swept to RecoveryStatusTimedOut on the recovery manager's tick (every
// ~10s), so the observed timeout may overshoot d by up to one tick
// interval. Values below the tick interval are therefore honoured only to
// tick granularity — pick a value comfortably above ~10s.
//
// d ALSO bounds the maximum snapshot-recovery lookback: a recovery cursor
// older than d is clamped forward to now-d, so the recovery service is
// never asked to replay further back than a single recovery may run.
func WithMaxRecoveryExecution(d time.Duration) Option {
	return func(c *Config) { c.maxRecoveryExecution = d }
}

// WithInitialSnapshotTime sets the duration to look back when issuing the
// first snapshot recovery on connect. Zero leaves the default.
func WithInitialSnapshotTime(d time.Duration) Option {
	return func(c *Config) { c.initialSnapshotTime = d }
}

// WithHTTPClientTimeout overrides the per-request timeout on the API client.
// Default 30s; valid range is up to 60s in practice.
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

// ShutdownTimeout returns the configured graceful-shutdown cap.
func (c Config) ShutdownTimeout() time.Duration { return c.shutdownTimeout }
