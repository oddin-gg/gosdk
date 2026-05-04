package gosdk

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/oddin-gg/gosdk/protocols"
)

// Config is the public, immutable SDK configuration. Construct via NewConfig.
//
// Phase 6 introduces this in addition to the legacy OddsFeedConfiguration
// (in configuration.go), which is still used by internal layers. Once the
// new gosdk.Client API is wired (subsequent commit), the legacy
// configuration drops.
type Config struct {
	accessToken          string
	defaultLocale        protocols.Locale
	preloadLocales       []protocols.Locale
	maxInactivity        time.Duration
	maxRecoveryExecution time.Duration
	initialSnapshotTime  time.Duration
	httpClientTimeout    time.Duration
	messagingPort        int
	sdkNodeID            *int
	selectedEnvironment  protocols.Environment
	selectedRegion       protocols.Region
	reportExtendedData   bool
	forcedAPIURL         string
	forcedMQURL          string
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
}

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
	// APILogOff: structured slog at debug only, no APIEvent emission.
	APILogOff APILogLevel = iota
	// APILogMetadata: emit method/url/status/latency, no bodies.
	APILogMetadata
	// APILogResponses: emit response body bytes (typical debug setting).
	APILogResponses
	// APILogFull: emit both request and response bytes (heavy).
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
)

// NewConfig constructs an SDK Config. The required arguments are the access
// token and the target environment; everything else is supplied via options.
func NewConfig(token string, env protocols.Environment, opts ...Option) Config {
	cfg := Config{
		accessToken:          token,
		selectedEnvironment:  env,
		defaultLocale:        protocols.EnLocale,
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
// without an explicit `locales ...protocols.Locale` argument.
func WithDefaultLocale(l protocols.Locale) Option {
	return func(c *Config) { c.defaultLocale = l }
}

// WithPreloadLocales lists locales to fetch eagerly when warming static
// catalogs (sports, market descriptions). Per-event entities are still
// fetched lazily per locale on first request.
func WithPreloadLocales(locales ...protocols.Locale) Option {
	return func(c *Config) {
		c.preloadLocales = append([]protocols.Locale(nil), locales...)
	}
}

// WithRegion selects the AWS region suffix for the broker / API host
// (e.g. protocols.APSouthEast1). Defaults to RegionDefault (EU).
func WithRegion(r protocols.Region) Option { return func(c *Config) { c.selectedRegion = r } }

// WithAPIURL overrides the resolved API host (otherwise derived from the
// environment + region).
func WithAPIURL(url string) Option { return func(c *Config) { c.forcedAPIURL = url } }

// WithMQURL overrides the resolved AMQP host.
func WithMQURL(url string) Option { return func(c *Config) { c.forcedMQURL = url } }

// WithMessagingPort overrides the AMQP TLS port (default 5672).
func WithMessagingPort(port int) Option { return func(c *Config) { c.messagingPort = port } }

// WithExchangeName overrides the AMQP exchange name (default "oddinfeed").
func WithExchangeName(name string) Option { return func(c *Config) { c.exchangeName = name } }

// WithReplayExchangeName overrides the replay exchange name (default "oddinreplay").
func WithReplayExchangeName(name string) Option { return func(c *Config) { c.replayExchangeName = name } }

// WithSportIDPrefix overrides the URN prefix used to construct sport URNs
// from routing keys (default "od:sport:").
func WithSportIDPrefix(prefix string) Option { return func(c *Config) { c.sportIDPrefix = prefix } }

// WithMaxInactivity caps the max time without an alive message before a
// producer is considered down (default 20s).
func WithMaxInactivity(d time.Duration) Option { return func(c *Config) { c.maxInactivity = d } }

// WithMaxRecoveryExecution caps the max time a single recovery may run
// (default 6h).
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
// per consumer). Default 1000.
func WithAMQPPrefetch(n int) Option { return func(c *Config) { c.amqpPrefetch = n } }

// WithSubscriptionBuffer sets the size of the in-process subscription
// channel buffer. Default 256.
func WithSubscriptionBuffer(n int) Option { return func(c *Config) { c.subscriptionBuffer = n } }

// WithHTTPClient overrides the *http.Client used for REST API calls.
// Useful for custom TLS config, transport-level instrumentation, or
// integration tests that route through an httptest.Server. Pass nil to
// keep the default.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Config) { c.httpClient = h }
}

// HTTPClient returns the configured custom http.Client (nil when unset).
func (c Config) HTTPClient() *http.Client { return c.httpClient }

// --- Read-only accessors (some are needed across packages once Client lands) ---

// AccessToken returns the configured token.
func (c Config) AccessToken() string { return c.accessToken }

// DefaultLocale returns the configured default locale.
func (c Config) DefaultLocale() protocols.Locale { return c.defaultLocale }

// PreloadLocales returns a copy of the preload locale list.
func (c Config) PreloadLocales() []protocols.Locale {
	out := make([]protocols.Locale, len(c.preloadLocales))
	copy(out, c.preloadLocales)
	return out
}

// Environment returns the selected environment.
func (c Config) Environment() protocols.Environment { return c.selectedEnvironment }

// Region returns the selected region.
func (c Config) Region() protocols.Region { return c.selectedRegion }

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
