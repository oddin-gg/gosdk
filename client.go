package gosdk

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/cache"
	"github.com/oddin-gg/gosdk/internal/factory"
	"github.com/oddin-gg/gosdk/internal/feed"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/internal/market"
	"github.com/oddin-gg/gosdk/internal/producer"
	"github.com/oddin-gg/gosdk/internal/recovery"
	"github.com/oddin-gg/gosdk/internal/replay"
	"github.com/oddin-gg/gosdk/internal/sport"
	"github.com/oddin-gg/gosdk/internal/whoami"
	"github.com/oddin-gg/gosdk/protocols"
)

// Default lossy buffer for event channels (ConnectionEvents,
// RecoveryEvents, APIEvents). Sized for steady-state churn, not
// peak. Drops on overflow rather than back-pressuring producers.
const defaultEventBuffer = 64

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
//   - Close(ctx) terminates everything; idempotent. ctx caps the drain wait.
//
// Concurrency: all methods are safe for concurrent use after New returns.
type Client struct {
	cfg     Config
	cfgAdpt protocols.OddsFeedConfiguration
	logger  *log.Logger

	apiClient                *api.Client
	whoAmIManager            protocols.WhoAmIManager
	producerManager          *producer.Manager
	cacheManager             *cache.Manager
	feedMessageFactory       *factory.FeedMessageFactory
	recoveryManager          *recovery.Manager
	rabbitMQClient           *feed.Client
	marketDescriptionManager protocols.MarketDescriptionManager
	sportsInfoManager        protocols.SportsInfoManager
	replayManager            protocols.ReplayManager
	replay                   *Replay

	// connectOnce + connectErr ensure Connect runs to completion at most
	// once on success but a failed first attempt is retryable (state
	// returns to "not connected"). Per NEXT.md §0: "retryable Connect,
	// not sync.Once."
	connectMu      sync.Mutex
	connectState   atomic.Int32 // ConnectionState; mirrored for fast read
	connecting     bool         // an attempt is in flight
	aliveSession   sdkOddsFeedSession
	internalCancel context.CancelFunc

	// subscriptions tracked for shutdown propagation.
	subsMu sync.Mutex
	subs   map[uuid.UUID]*Subscription

	// Lossy event channels (NEXT.md §19.3).
	connEvents chan ConnectionEvent
	recvEvents chan RecoveryEvent
	apiEvents  chan APIEvent

	// Shutdown state.
	closeOnce sync.Once
	closed    chan struct{}
	closeErr  error
	wg        sync.WaitGroup
}

// RecoveryEvent is the typed event delivered on RecoveryEvents().
//
// Either ProducerStatus or EventRecovery is set, never both. Consumers
// type-switch on the populated field.
type RecoveryEvent struct {
	ProducerStatus protocols.ProducerStatus
	EventRecovery  protocols.EventRecoveryMessage
	At             time.Time
}

// New constructs a Client. It does NOT open AMQP — call Connect or
// Subscribe to do that. The bookmaker-details API call is made eagerly
// so configuration errors surface up-front; pass a ctx with a timeout if
// you want to bound that probe.
func New(ctx context.Context, cfg Config) (*Client, error) {
	c := &Client{
		cfg:        cfg,
		cfgAdpt:    newConfigAdapter(&cfg),
		subs:       make(map[uuid.UUID]*Subscription),
		connEvents: make(chan ConnectionEvent, defaultEventBuffer),
		recvEvents: make(chan RecoveryEvent, defaultEventBuffer),
		apiEvents:  make(chan APIEvent, defaultEventBuffer),
		closed:     make(chan struct{}),
	}
	c.connectState.Store(int32(ConnectionStateNotConnected))

	c.apiClient = api.New(c.cfgAdpt)
	if h := cfg.HTTPClient(); h != nil {
		c.apiClient.SetHTTPClient(h)
	}
	c.installAPICapture()

	c.whoAmIManager = whoami.NewManager(c.cfgAdpt, c.apiClient)
	details, err := c.whoAmIManager.BookmakerDetails(ctx)
	if err != nil {
		return nil, fmt.Errorf("gosdk: who-am-i probe: %w", err)
	}

	logger := cfg.Logger()
	c.logger = log.New(logger).WithField("client_id", details.BookmakerID())

	c.producerManager = producer.NewManager(c.cfgAdpt, c.apiClient, c.logger)
	c.cacheManager = cache.NewManager(c.apiClient, c.cfgAdpt, c.logger)

	entityFactory := factory.NewEntityFactory(c.cacheManager)
	marketDescriptionFactory := factory.NewMarketDescriptionFactory(
		c.cacheManager.MarketDescriptionCache,
		c.cacheManager.MarketVoidReasonsCache,
		c.cacheManager.PlayersCache,
		c.cacheManager.CompetitorCache,
	)
	marketDataFactory := factory.NewMarketDataFactory(c.cfgAdpt, marketDescriptionFactory)
	marketFactory := factory.NewMarketFactory(
		marketDataFactory,
		[]protocols.Locale{c.cfg.DefaultLocale()},
		c.logger,
	)
	c.feedMessageFactory = factory.NewFeedMessageFactory(
		entityFactory,
		marketFactory,
		c.producerManager,
		c.cfgAdpt,
	)

	c.recoveryManager = recovery.NewManager(c.cfgAdpt, c.producerManager, c.apiClient, c.logger)
	c.marketDescriptionManager = market.NewManager(c.cacheManager, marketDescriptionFactory, c.cfgAdpt)
	c.sportsInfoManager = sport.NewManager(entityFactory, c.apiClient, c.cacheManager, c.cfgAdpt)
	c.replayManager = replay.NewManager(c.apiClient, c.cfgAdpt, c.sportsInfoManager)
	c.replay = &Replay{client: c}

	c.rabbitMQClient = feed.NewClient(c.cfgAdpt, c.whoAmIManager, c.logger)
	c.rabbitMQClient.SetEventEmitter(c.onFeedEvent)

	return c, nil
}

// onFeedEvent translates the internal feed-layer Event enum to the
// public ConnectionEvent and lossy-pushes it onto ConnectionEvents().
func (c *Client) onFeedEvent(ev feed.Event) {
	var kind ConnectionEventKind
	switch ev.Kind {
	case feed.EventConnected:
		kind = ConnectionConnected
	case feed.EventDisconnected:
		kind = ConnectionDisconnected
	case feed.EventReconnecting:
		kind = ConnectionReconnecting
	default:
		return
	}
	c.emitConn(kind, ev.Err)
}

// Connect opens the AMQP connection, loads the producers catalog, and
// starts the recovery loop. Idempotent on success; retryable on failure.
//
// Subscribe lazy-connects on first call, so explicit Connect is optional.
// Calling Connect lets you see configuration / network errors up-front
// before adding subscriptions.
func (c *Client) Connect(ctx context.Context) error {
	c.connectMu.Lock()
	switch ConnectionState(c.connectState.Load()) {
	case ConnectionStateConnected:
		c.connectMu.Unlock()
		return nil
	case ConnectionStateClosed:
		c.connectMu.Unlock()
		return errSubscriptionClosed
	}
	if c.connecting {
		c.connectMu.Unlock()
		return errors.New("gosdk: connect already in progress")
	}
	c.connecting = true
	c.connectState.Store(int32(ConnectionStateConnecting))
	c.connectMu.Unlock()

	settled := false
	defer func() {
		c.connectMu.Lock()
		c.connecting = false
		if !settled {
			c.connectState.Store(int32(ConnectionStateNotConnected))
		}
		c.connectMu.Unlock()
	}()

	if err := c.producerManager.Open(ctx); err != nil {
		return fmt.Errorf("gosdk: producer init: %w", err)
	}

	if err := c.rabbitMQClient.Open(ctx); err != nil {
		return fmt.Errorf("gosdk: amqp open: %w", err)
	}

	// Internal ctx is the lifetime ctx for pumps + recovery — cancelled
	// on Close. Created here so the recovery manager and the recovery
	// pump goroutine share the same cancellation root.
	internalCtx, cancel := context.WithCancel(context.Background())
	c.internalCancel = cancel

	recoveryCh, err := c.recoveryManager.Open(internalCtx)
	if err != nil {
		// AMQP is up; attempt rollback so retry from scratch is clean.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = c.rabbitMQClient.Close(shutdownCtx)
		cancel()
		return fmt.Errorf("gosdk: recovery open: %w", err)
	}

	// Internal alive-only session — drives the recovery state machine.
	// Replay-only consumers will get one too (cheap; one queue), but skip
	// when no recovery activity is expected (no producers).
	alive := newSession(
		c.rabbitMQClient,
		c.producerManager,
		c.cacheManager,
		c.feedMessageFactory,
		c.recoveryManager,
		c.cfg.exchangeName,
		c.cfg.sportIDPrefix,
		false,
		c.logger,
	)
	aliveInterest := protocols.SystemAliveOnly
	if err := alive.Open(ctx, []string{string(protocols.SystemAliveOnly)}, &aliveInterest, false); err != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = c.rabbitMQClient.Close(shutdownCtx)
		cancel()
		c.recoveryManager.Close()
		return fmt.Errorf("gosdk: alive session open: %w", err)
	}
	c.aliveSession = alive
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		for range alive.RespCh() {
			// drain — alive messages are consumed by the recovery
			// processor inside the session itself.
		}
	}()

	// Pump recovery events to the lossy public channel using the same
	// internal ctx that gates recovery-manager API calls.
	c.wg.Add(1)
	go c.pumpRecovery(internalCtx, recoveryCh)

	settled = true
	c.connectMu.Lock()
	c.connectState.Store(int32(ConnectionStateConnected))
	c.connectMu.Unlock()
	// Note: ConnectionConnected is emitted by the feed-layer event
	// callback (see onFeedEvent) — single source of truth across the
	// first dial and all subsequent reconnects.
	return nil
}

// Close tears down the client. Idempotent. ctx caps the wait for graceful
// drain — when ctx fires before shutdown completes, returns ctx.Err()
// while shutdown continues in the background. After return, all
// subscriptions are terminated and event channels are closed.
func (c *Client) Close(ctx context.Context) error {
	c.closeOnce.Do(func() { go c.runShutdown() })

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

func (c *Client) runShutdown() {
	c.connectMu.Lock()
	c.connectState.Store(int32(ConnectionStateClosed))
	c.connectMu.Unlock()

	// Stop internal pumps first so they don't push to channels we close.
	if c.internalCancel != nil {
		c.internalCancel()
	}

	// Recovery uses close(closeCh) broadcast under sync.Once internally.
	if c.recoveryManager != nil {
		c.recoveryManager.Close()
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
		s.abortWithErr(errSubscriptionClosed)
	}
	for _, s := range subs {
		<-s.Done()
	}

	if c.aliveSession != nil {
		c.aliveSession.Close()
	}

	if c.apiClient != nil {
		c.apiClient.Close()
	}

	if c.cacheManager != nil {
		c.cacheManager.Close()
	}

	if c.rabbitMQClient != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = c.rabbitMQClient.Close(shutdownCtx)
		cancel()
	}

	c.wg.Wait()

	// Close lossy event channels last, after pumps have stopped.
	c.emitConn(ConnectionClosed, nil)
	close(c.connEvents)
	close(c.recvEvents)
	close(c.apiEvents)

	close(c.closed)
}

// Subscribe creates a new subscription and returns the *Subscription.
// First call lazy-connects if Connect was not called.
//
// The supplied ctx governs the Subscribe call itself (lazy-connect dial,
// queue declaration). It does NOT govern the subscription's lifetime —
// once Subscribe returns, the subscription lives until the caller calls
// Subscription.Close, the Client closes, or a terminal error occurs.
func (c *Client) Subscribe(ctx context.Context, opts ...SubscribeOption) (*Subscription, error) {
	subCfg := subscribeConfig{messageInterest: protocols.AllMessageInterest}
	for _, opt := range opts {
		opt(&subCfg)
	}
	if subCfg.messageInterest == "" {
		subCfg.messageInterest = protocols.AllMessageInterest
	}

	if ConnectionState(c.connectState.Load()) == ConnectionStateClosed {
		return nil, errSubscriptionClosed
	}
	if !subCfg.replay {
		if err := c.Connect(ctx); err != nil {
			return nil, err
		}
	} else {
		// Replay still needs AMQP up; producers/recovery are unused.
		if err := c.rabbitMQClient.Open(ctx); err != nil {
			return nil, fmt.Errorf("gosdk: amqp open (replay): %w", err)
		}
		c.connectState.CompareAndSwap(int32(ConnectionStateNotConnected), int32(ConnectionStateConnected))
	}

	exchangeName := c.cfg.exchangeName
	recoveryProcessor := protocols.RecoveryMessageProcessor(c.recoveryManager)
	if subCfg.replay {
		exchangeName = c.cfg.replayExchangeName
		recoveryProcessor = &recovery.DummyManager{}
	}

	session := newSession(
		c.rabbitMQClient,
		c.producerManager,
		c.cacheManager,
		c.feedMessageFactory,
		recoveryProcessor,
		exchangeName,
		c.cfg.sportIDPrefix,
		subCfg.replay,
		c.logger,
	)

	routingKeys, err := c.routingKeys(subCfg)
	if err != nil {
		return nil, err
	}

	mi := subCfg.messageInterest
	if err := session.Open(ctx, routingKeys, &mi, c.cfg.reportExtendedData); err != nil {
		return nil, fmt.Errorf("gosdk: session open: %w", err)
	}

	bufSize := c.cfg.subscriptionBuffer
	if bufSize <= 0 {
		bufSize = defaultSubscriptionBuffer
	}
	sub := &Subscription{
		id:         uuid.New(),
		messages:   make(chan protocols.SessionMessage, bufSize),
		closed:     make(chan struct{}),
		underlying: session,
		pumpDone:   make(chan struct{}),
	}

	c.subsMu.Lock()
	c.subs[sub.id] = sub
	c.subsMu.Unlock()

	c.wg.Add(1)
	go c.pumpSubscription(sub)

	return sub, nil
}

// pumpSubscription forwards legacy SessionMessage values from the
// underlying session to the public Subscription channel. Exits when the
// session's RespCh closes (terminal) or the subscription requests
// shutdown (via Close / abortWithErr / parent Close).
func (c *Client) pumpSubscription(sub *Subscription) {
	defer c.wg.Done()
	defer close(sub.pumpDone)

	respCh := sub.underlying.RespCh()
	for {
		select {
		case <-sub.closed:
			return
		case msg, ok := <-respCh:
			if !ok {
				return
			}
			select {
			case sub.messages <- msg:
			case <-sub.closed:
				return
			}
		}
	}
}

func (c *Client) pumpRecovery(ctx context.Context, in <-chan protocols.RecoveryMessage) {
	defer c.wg.Done()
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
			// Lossy: drop on overflow rather than blocking the recovery
			// state machine. Document in NEXT.md §19.3.
			select {
			case c.recvEvents <- ev:
			default:
			}
		}
	}
}

func (c *Client) emitConn(kind ConnectionEventKind, err error) {
	ev := ConnectionEvent{Kind: kind, Err: err, At: time.Now()}
	select {
	case c.connEvents <- ev:
	default:
	}
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
	cap := api.EventCapture{
		Emit:         c.pushAPIEvent,
		BodyLimit:    c.cfg.apiCallBodyLimit,
		ResponseBody: c.cfg.apiCallLogging >= APILogResponses,
		RequestBody:  c.cfg.apiCallLogging == APILogFull,
	}
	c.apiClient.SetEventCapture(cap)
}

// pushAPIEvent converts an internal/api.APIEvent into the public
// gosdk.APIEvent and lossy-pushes it onto the APIEvents channel.
func (c *Client) pushAPIEvent(ev api.APIEvent) {
	out := APIEvent{
		At:        ev.At,
		Method:    ev.Method,
		URL:       ev.URL,
		Status:    ev.Status,
		Latency:   ev.Latency,
		Attempt:   ev.Attempt,
		Locale:    ev.Locale,
		Request:   ev.Request,
		Response:  ev.Response,
		Truncated: ev.Truncated,
		Err:       ev.Err,
	}
	select {
	case c.apiEvents <- out:
	default:
	}
}

// routingKeys derives the AMQP routing keys for a single subscription's
// message interest. Mirrors the legacy generateKeys logic but scoped to
// one session — multi-session validation is not relevant in the flat
// model where each Subscribe creates an independent consumer.
func (c *Client) routingKeys(cfg subscribeConfig) ([]string, error) {
	if cfg.replay {
		// Replay sessions consume everything from the replay exchange.
		return []string{string(protocols.AllMessageInterest)}, nil
	}

	keysSet := make(map[string]struct{})
	var basicKeys []string
	if cfg.messageInterest == protocols.SpecifiedMatchesOnlyMessageInterest {
		if len(cfg.specificEvents) == 0 {
			return nil, errors.New("gosdk: SpecifiedMatchesOnly requires WithSpecificEvents")
		}
		for urn := range cfg.specificEvents {
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
	if cfg.messageInterest != protocols.SystemAliveOnly {
		keysSet[string(protocols.SystemAliveOnly)] = struct{}{}
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
func (c *Client) ConnectionState() ConnectionState {
	return ConnectionState(c.connectState.Load())
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
func (c *Client) BookmakerDetails(ctx context.Context) (protocols.BookmakerDetail, error) {
	return c.whoAmIManager.BookmakerDetails(ctx)
}

// --- Producers ---

// Producers returns all producers known to the SDK.
func (c *Client) Producers(ctx context.Context) ([]protocols.Producer, error) {
	m, err := c.producerManager.AvailableProducers(ctx)
	if err != nil {
		return nil, err
	}
	return mapToSlice(m), nil
}

// ActiveProducers returns currently-active producers.
func (c *Client) ActiveProducers(ctx context.Context) ([]protocols.Producer, error) {
	m, err := c.producerManager.ActiveProducers(ctx)
	if err != nil {
		return nil, err
	}
	return mapToSlice(m), nil
}

// ProducersInScope returns active producers serving the given scope
// (live or prematch).
func (c *Client) ProducersInScope(ctx context.Context, scope protocols.ProducerScope) ([]protocols.Producer, error) {
	m, err := c.producerManager.ActiveProducersInScope(ctx, scope)
	if err != nil {
		return nil, err
	}
	return mapToSlice(m), nil
}

// Producer returns a single producer by id.
func (c *Client) Producer(ctx context.Context, id uint) (protocols.Producer, error) {
	return c.producerManager.GetProducer(ctx, id)
}

// SetProducerEnabled toggles the per-producer enable flag. Disabled
// producers don't trigger recovery and their messages are dropped at
// the session pre-filter.
func (c *Client) SetProducerEnabled(ctx context.Context, id uint, enabled bool) error {
	return c.producerManager.SetProducerState(ctx, id, enabled)
}

// SetProducerRecoveryFromTimestamp pins the next snapshot recovery's
// "after" timestamp for this producer. Useful when consumers persist
// processed timestamps externally and want recovery to resume from a
// specific point on next connect.
func (c *Client) SetProducerRecoveryFromTimestamp(ctx context.Context, id uint, t time.Time) error {
	return c.producerManager.SetProducerRecoveryFromTimestamp(ctx, id, t)
}

// --- Recovery ---

// RecoveryHandle is returned by RecoverEventOdds / RecoverEventStateful.
// It exposes per-request semantics so callers can wait on a specific
// recovery completing without scanning the lossy RecoveryEvents
// channel. The handle is reliable — even if the channel event is
// dropped, Done() / Result() / Status() reflect the terminal outcome.
type RecoveryHandle = recovery.Handle

// RecoverEventOdds initiates an event-odds recovery for a single event
// and returns a *RecoveryHandle that tracks completion reliably.
//
//	h, err := client.RecoverEventOdds(ctx, producerID, eventURN)
//	if err != nil { ... }
//	<-h.Done()
//	res := h.Result()
//	if res.Status == protocols.RecoveryStatusCompleted { ... }
//
// Handles remain queryable via Client.EventRecoveryStatus for a
// configurable grace period (recovery.HandleGCGracePeriod, default
// 5 minutes) after they reach a terminal state.
func (c *Client) RecoverEventOdds(ctx context.Context, producerID uint, eventID protocols.URN) (*RecoveryHandle, error) {
	return c.recoveryManager.InitiateEventOddsRecoveryHandle(ctx, producerID, eventID)
}

// RecoverEventStateful initiates a stateful-recovery for a single event.
func (c *Client) RecoverEventStateful(ctx context.Context, producerID uint, eventID protocols.URN) (*RecoveryHandle, error) {
	return c.recoveryManager.InitiateEventStatefulRecoveryHandle(ctx, producerID, eventID)
}

// EventRecoveryStatus looks up a recovery by request id. Useful for
// callers that only kept the request id and want to check whether the
// recovery has completed. The second return value is false when the
// id is unknown — never registered or GC'd after the grace period.
func (c *Client) EventRecoveryStatus(requestID uint) (protocols.RecoveryResult, bool) {
	h, ok := c.recoveryManager.LookupHandle(requestID)
	if !ok {
		return protocols.RecoveryResult{}, false
	}
	return h.Snapshot(), true
}

// --- Sports info ---

// Sports returns the sports catalog. The first variadic locale (or the
// default locale when omitted) drives the entity-method-level locale.
// Multiple locales preload all of them into the cache.
func (c *Client) Sports(ctx context.Context, locales ...protocols.Locale) ([]protocols.Sport, error) {
	loc := c.localeOrDefault(locales)
	if len(locales) > 1 {
		for _, l := range locales {
			if _, err := c.sportsInfoManager.LocalizedSports(ctx, l); err != nil {
				return nil, err
			}
		}
	}
	return c.sportsInfoManager.LocalizedSports(ctx, loc)
}

// ActiveTournaments returns active tournaments across all sports.
func (c *Client) ActiveTournaments(ctx context.Context, locales ...protocols.Locale) ([]protocols.Tournament, error) {
	return c.sportsInfoManager.LocalizedActiveTournaments(ctx, c.localeOrDefault(locales))
}

// AvailableTournaments returns tournaments under a given sport.
func (c *Client) AvailableTournaments(ctx context.Context, sportID protocols.URN, locales ...protocols.Locale) ([]protocols.Tournament, error) {
	return c.sportsInfoManager.LocalizedAvailableTournaments(ctx, sportID, c.localeOrDefault(locales))
}

// Match returns the match identified by URN.
func (c *Client) Match(ctx context.Context, id protocols.URN, locales ...protocols.Locale) (protocols.Match, error) {
	return c.sportsInfoManager.LocalizedMatch(ctx, id, c.localeOrDefault(locales))
}

// MatchesFor returns matches scheduled for a calendar date.
func (c *Client) MatchesFor(ctx context.Context, t time.Time, locales ...protocols.Locale) ([]protocols.Match, error) {
	return c.sportsInfoManager.LocalizedMatchesFor(ctx, t, c.localeOrDefault(locales))
}

// LiveMatches returns currently-live matches.
func (c *Client) LiveMatches(ctx context.Context, locales ...protocols.Locale) ([]protocols.Match, error) {
	return c.sportsInfoManager.LocalizedLiveMatches(ctx, c.localeOrDefault(locales))
}

// ListMatches paginates through the schedule. start is the offset and
// limit is the page size.
func (c *Client) ListMatches(ctx context.Context, start, limit uint, locales ...protocols.Locale) ([]protocols.Match, error) {
	return c.sportsInfoManager.LocalizedListOfMatches(ctx, start, limit, c.localeOrDefault(locales))
}

// Competitor returns a competitor profile by URN.
func (c *Client) Competitor(ctx context.Context, id protocols.URN, locales ...protocols.Locale) (protocols.Competitor, error) {
	return c.sportsInfoManager.LocalizedCompetitor(ctx, id, c.localeOrDefault(locales))
}

// FixtureChanges returns fixture changes since `after`.
func (c *Client) FixtureChanges(ctx context.Context, after time.Time, locales ...protocols.Locale) ([]protocols.FixtureChange, error) {
	return c.sportsInfoManager.LocalizedFixtureChanges(ctx, c.localeOrDefault(locales), after)
}

// ClearMatch invalidates the cached match entry.
func (c *Client) ClearMatch(id protocols.URN) { c.sportsInfoManager.ClearMatch(id) }

// ClearTournament invalidates the cached tournament entry.
func (c *Client) ClearTournament(id protocols.URN) { c.sportsInfoManager.ClearTournament(id) }

// ClearCompetitor invalidates the cached competitor entry.
func (c *Client) ClearCompetitor(id protocols.URN) { c.sportsInfoManager.ClearCompetitor(id) }

// --- Market descriptions ---

// MarketDescriptions returns all market descriptions for the (first
// supplied or default) locale.
func (c *Client) MarketDescriptions(ctx context.Context, locales ...protocols.Locale) ([]protocols.MarketDescription, error) {
	return c.marketDescriptionManager.LocalizedMarketDescriptions(ctx, c.localeOrDefault(locales))
}

// MarketDescription returns the description for a (marketID, variant)
// tuple. variant=nil selects the base (non-variant) description; pass a
// non-nil pointer for the dynamic variant catalog.
func (c *Client) MarketDescription(ctx context.Context, id uint, variant *string) (*protocols.MarketDescription, error) {
	return c.marketDescriptionManager.MarketDescriptionByIDAndVariant(ctx, id, variant)
}

// MarketVoidReasons returns the void-reasons catalog.
func (c *Client) MarketVoidReasons(ctx context.Context) ([]protocols.MarketVoidReason, error) {
	return c.marketDescriptionManager.MarketVoidReasons(ctx)
}

// ReloadMarketVoidReasons forces a refetch of the void-reasons catalog.
func (c *Client) ReloadMarketVoidReasons(ctx context.Context) ([]protocols.MarketVoidReason, error) {
	return c.marketDescriptionManager.ReloadMarketVoidReasons(ctx)
}

// ClearMarketDescription invalidates a cached description. variant=nil
// targets the base entry; non-nil targets the (id, variant) tuple.
func (c *Client) ClearMarketDescription(marketID uint, variant *string) {
	c.marketDescriptionManager.ClearMarketDescription(marketID, variant)
}

// --- Replay ---

// Replay returns the replay subtype for replay-API operations. Nil is
// never returned; the subtype is bound to this Client's lifetime.
func (c *Client) Replay() *Replay { return c.replay }

// Replay groups the replay-API operations under a dedicated subtype.
type Replay struct {
	client *Client
}

// List returns the replay queue contents as Match value snapshots.
func (r *Replay) List(ctx context.Context) ([]protocols.Match, error) {
	return r.client.replayManager.ReplayList(ctx)
}

// AddEvent adds an event to the replay queue.
func (r *Replay) AddEvent(ctx context.Context, eventID protocols.URN) error {
	_, err := r.client.replayManager.AddSportEventID(ctx, eventID)
	return err
}

// RemoveEvent removes an event from the replay queue.
func (r *Replay) RemoveEvent(ctx context.Context, eventID protocols.URN) error {
	_, err := r.client.replayManager.RemoveSportEventID(ctx, eventID)
	return err
}

// Start begins replay playback with the supplied options.
func (r *Replay) Start(ctx context.Context, opts ...ReplayOption) error {
	params := protocols.ReplayPlayParams{}
	for _, opt := range opts {
		opt(&params)
	}
	_, err := r.client.replayManager.Play(ctx, params)
	return err
}

// Stop pauses replay playback. Queue contents are preserved.
func (r *Replay) Stop(ctx context.Context) error {
	_, err := r.client.replayManager.Stop(ctx)
	return err
}

// Clear empties the replay queue.
func (r *Replay) Clear(ctx context.Context) error {
	_, err := r.client.replayManager.Clear(ctx)
	return err
}

// StopAndClear stops playback and empties the queue. Parity with .NET.
func (r *Replay) StopAndClear(ctx context.Context) error {
	if _, err := r.client.replayManager.Stop(ctx); err != nil {
		return err
	}
	_, err := r.client.replayManager.Clear(ctx)
	return err
}

// ReplayOption tunes a Replay.Start invocation.
type ReplayOption func(*protocols.ReplayPlayParams)

// WithReplaySpeed scales playback speed (e.g. 10 = ten times realtime).
func WithReplaySpeed(speed int) ReplayOption {
	return func(p *protocols.ReplayPlayParams) { p.Speed = &speed }
}

// WithReplayMaxDelayMs caps the delay between consecutive messages.
func WithReplayMaxDelayMs(ms int) ReplayOption {
	return func(p *protocols.ReplayPlayParams) { p.MaxDelayInMs = &ms }
}

// WithReplayRunParallel runs events in parallel rather than sequentially.
func WithReplayRunParallel(parallel bool) ReplayOption {
	return func(p *protocols.ReplayPlayParams) { p.RunParallel = &parallel }
}

// WithReplayRewriteTimestamps rewrites historical timestamps to "now".
func WithReplayRewriteTimestamps(rewrite bool) ReplayOption {
	return func(p *protocols.ReplayPlayParams) { p.RewriteTimestamps = &rewrite }
}

// WithReplayProducer narrows replay to a specific producer name.
func WithReplayProducer(producer string) ReplayOption {
	return func(p *protocols.ReplayPlayParams) { p.Producer = &producer }
}

// --- Helpers ---

func (c *Client) localeOrDefault(locales []protocols.Locale) protocols.Locale {
	if len(locales) > 0 {
		return locales[0]
	}
	return c.cfg.DefaultLocale()
}

func mapToSlice(m map[uint]protocols.Producer) []protocols.Producer {
	out := make([]protocols.Producer, 0, len(m))
	for _, p := range m {
		out = append(out, p)
	}
	return out
}
