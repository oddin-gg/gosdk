package feed

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cenkalti/backoff/v5"
	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/oddin-gg/gosdk/internal/config"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/internal/version"
	"github.com/oddin-gg/gosdk/types"
)

// dialFallbackTimeout is the bound applied to the AMQP TCP dial +
// protocol handshake when the caller's ctx has no deadline. Matches
// the amqp091-go library default so behaviour is unchanged for
// callers who pass context.Background().
const dialFallbackTimeout = 30 * time.Second

// EventKind enumerates AMQP-level lifecycle transitions emitted to
// observers via the EventEmitter callback. Independent of the public
// gosdk.ConnectionEventKind to keep internal/feed self-contained.
type EventKind int

const (
	// EventConnected fires after a successful dial — both the first dial
	// from Open and every successful reconnect.
	EventConnected EventKind = iota
	// EventDisconnected fires when the broker drops the connection.
	// Err is populated.
	EventDisconnected
	// EventReconnecting fires once per drop, when the reconnect backoff
	// loop begins.
	EventReconnecting
)

// Event is delivered to the optional EventEmitter callback for each
// connection-state transition observed by the reconnect loop.
type Event struct {
	Kind EventKind
	Err  error
}

// EventEmitter is invoked synchronously from the reconnect goroutine.
// It MUST NOT block — gosdk wraps the callback with a select+default
// lossy push.
type EventEmitter func(Event)

// WhoAmIProbe is the narrow surface this package needs from
// gosdk's whoAmIManager — just the bookmaker-details probe used at
// dial time to resolve the AMQP virtual host. Defined locally
// because internal/feed can't import the gosdk root package
// (cycle); the concrete *whoami.Manager satisfies it structurally.
type WhoAmIProbe interface {
	BookmakerDetails(ctx context.Context) (types.BookmakerDetail, error)
}

// Client manages a single AMQP connection with automatic reconnection.
//
// Phase 4 rewrite: replaces the recursive-reconnect pyramid with a single
// long-lived reconnect goroutine, atomic.Pointer for connection access (no
// data race on c.connection), backoff/v5 for exponential retry, and ctx-
// driven shutdown. Open is idempotent, Close is idempotent. Callers wait
// for a usable connection via Channel(ctx) instead of poking c.connection.
type Client struct {
	cfg           config.Config
	whoAmIManager WhoAmIProbe
	logger        *log.Logger
	emitter       EventEmitter

	// conn holds the current *amqp.Connection. Nil while disconnected.
	conn atomic.Pointer[amqp.Connection]

	// state machine
	mu      sync.Mutex
	opening bool
	opened  bool
	// openAttempt is the IMMUTABLE result record of the in-flight (or
	// most recent) Open attempt: the owner writes err/opened exactly
	// once before closing done; waiters read the object they captured
	// under mu with no re-lock after waking. Pre-fix waiters re-read
	// the Client's mutable openErr/opened after <-done — a fresh Open
	// starting in that window (clearing openErr, replacing openDone)
	// made them observe the LATER attempt's state: a generic error, or
	// even the new attempt's eventual success, instead of their own
	// attempt's outcome.
	openAttempt *openResult

	closeFn         context.CancelFunc
	shutdownStarted bool // set by runShutdown under mu BEFORE wg.Wait

	// connectedCh is closed by the reconnect goroutine each time a fresh
	// connection becomes available (re-created on every successful dial).
	// Subscribers waiting in Channel(ctx) read from a snapshot taken under mu.
	connectedMu sync.Mutex
	connectedCh chan struct{}

	// closeOnce + closed implement the per-call-waits-for-completion pattern
	// from NEXT.md §8.
	closeOnce sync.Once
	closed    chan struct{}
	wg        sync.WaitGroup
}

// openResult is one Open attempt's outcome record — see openAttempt.
type openResult struct {
	done   chan struct{}
	err    error
	opened bool
}

// NewClient ...
func NewClient(cfg config.Config, whoAmIManager WhoAmIProbe, logger *log.Logger) *Client {
	return &Client{
		cfg:           cfg,
		whoAmIManager: whoAmIManager,
		logger:        logger,
		connectedCh:   make(chan struct{}),
		closed:        make(chan struct{}),
	}
}

// SetEventEmitter installs the connection-event callback. Pass nil to
// disable. Should be called before Open.
func (c *Client) SetEventEmitter(e EventEmitter) {
	c.mu.Lock()
	c.emitter = e
	c.mu.Unlock()
}

// emit invokes the event callback if set. Snapshots under RLock so a
// concurrent SetEventEmitter doesn't tear the dispatch.
func (c *Client) emit(kind EventKind, err error) {
	c.mu.Lock()
	emit := c.emitter
	c.mu.Unlock()
	if emit == nil {
		return
	}
	emit(Event{Kind: kind, Err: err})
}

// Open establishes the AMQP connection and starts the reconnect goroutine.
//
// Concurrent callers wait on the in-flight attempt and observe the same
// outcome (mirrors gosdk.Client.Connect). Once `opened` is true,
// subsequent Open calls return nil immediately. A failed first Open does
// NOT poison subsequent attempts — the state returns to "not opened" so
// the next call retries from scratch.
func (c *Client) Open(ctx context.Context) (err error) {
	c.mu.Lock()
	if c.opened {
		c.mu.Unlock()
		return nil
	}
	if c.opening {
		// Concurrent caller: capture THIS attempt's immutable result
		// record, wait for it, and read the record — never the Client's
		// mutable state, which a fresh attempt may already have
		// replaced by the time we wake (see openAttempt).
		res := c.openAttempt
		c.mu.Unlock()
		select {
		case <-res.done:
		case <-ctx.Done():
			return ctx.Err()
		}
		if res.err != nil {
			return res.err
		}
		if res.opened {
			return nil
		}
		return errors.New("feed: open attempt did not yield a connection")
	}
	c.opening = true
	res := &openResult{done: make(chan struct{})}
	c.openAttempt = res
	c.mu.Unlock()

	// Reset opening on exit if we didn't reach opened=true. The defer
	// publishes the named-return err to waiters via the attempt record:
	// fields are written BEFORE close(res.done) and never again.
	settled := false
	defer func() {
		c.mu.Lock()
		c.opening = false
		if !settled {
			c.opened = false
		}
		res.err = err
		res.opened = settled
		close(res.done)
		c.mu.Unlock()
	}()

	// First dial — synchronous so callers see a usable connection on return.
	conn, err := c.dial(ctx)
	if err != nil {
		return err
	}

	// Spawn reconnect goroutine for the lifetime of the connection.
	// WithoutCancel(ctx) preserves caller metadata (logger fields,
	// trace ids) on the loop ctx but severs the cancellation chain:
	// the loop must outlive Open's caller ctx (which only bounds the
	// dial) and is cancelled by closeFn at Close() time.
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))

	// Atomic publish under mu: if Close ran while we were dialing,
	// runShutdown already saw closeFn==nil + wg.Wait()==0 and closed
	// c.closed. Spawning the loop now would leak it forever (no one
	// will cancel loopCtx, no one will Wait on us). Instead: tear down
	// the freshly-built conn ourselves and report ErrAlreadyClosed.
	//
	// runShutdown sets shutdownStarted=true under mu BEFORE wg.Wait,
	// so once we've taken mu and observed false we are guaranteed
	// runShutdown either hasn't run yet or will see our wg.Add(1)
	// before Wait returns.
	c.mu.Lock()
	if c.shutdownStarted {
		c.mu.Unlock()
		cancel()
		// CloseDeadline, not Close: rejecting a late connection performs
		// the blocking AMQP close handshake, and a broker that keeps
		// heartbeats flowing while withholding close-ok would otherwise
		// pin Open here indefinitely — same bound runShutdown applies.
		_ = conn.CloseDeadline(time.Now().Add(closeHandshakeBudget))
		return ErrAlreadyClosed
	}
	c.conn.Store(conn)
	c.closeFn = cancel
	c.opened = true
	settled = true
	c.wg.Add(1)
	c.mu.Unlock()

	c.signalConnected()
	c.emit(EventConnected, nil)

	go c.reconnectLoop(loopCtx, conn)
	return nil
}

// IsOpen reports whether the client currently holds a live (opened)
// AMQP connection. Used by the gosdk lifecycle layer to reconcile
// concurrent broker-open attempts without racing on the feed client's
// internal state.
func (c *Client) IsOpen() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.opened
}

// Close cancels the reconnect goroutine, closes the connection, and blocks
// until cleanup completes — but the supplied ctx caps the wait. If shutdown
// has already completed, returns nil immediately even with a cancelled ctx
// (the "completed shutdown always wins" rule from NEXT.md §8 Close).
func (c *Client) Close(ctx context.Context) error {
	c.closeOnce.Do(func() { go c.runShutdown() })

	// Fast path: already done.
	select {
	case <-c.closed:
		return nil
	default:
	}
	select {
	case <-c.closed:
		return nil
	case <-ctx.Done():
		select {
		case <-c.closed:
			return nil
		default:
			return ctx.Err()
		}
	}
}

func (c *Client) runShutdown() {
	// Mark shutdownStarted BEFORE wg.Wait so a concurrent first Open
	// observing it under mu after a successful dial will tear its
	// freshly-built conn down itself instead of spawning a reconnect
	// goroutine we'd never reap. Pairs with Open's atomic-publish
	// critical section.
	c.mu.Lock()
	c.shutdownStarted = true
	cancel := c.closeFn
	c.closeFn = nil
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}

	if conn := c.conn.Load(); conn != nil {
		// CloseDeadline, not Close: the close handshake is a blocking
		// send-and-wait — on a half-open transport that withholds
		// responses a plain Close parks until heartbeat teardown,
		// holding runShutdown (and with it every pending channel RPC's
		// unwind) hostage. The deadline force-closes the socket after
		// the grace period, which also fails every parked channel-level
		// RPC (topology workers, channel closes, blocked acks) with
		// ErrClosed.
		_ = conn.CloseDeadline(time.Now().Add(closeHandshakeBudget))
		c.conn.Store(nil)
	}
	c.wg.Wait()
	close(c.closed)
}

// closeHandshakeBudget bounds the connection close handshake during
// shutdown — deliberately shorter than the heartbeat unwind (~2×10s) so
// a stalled transport cannot dominate WithShutdownTimeout.
const closeHandshakeBudget = 3 * time.Second

// CreateChannel waits for a usable connection (or ctx cancellation), then
// declares an exclusive queue, binds the supplied routing keys, and starts
// consuming with manual-ack semantics. Returns the delivery channel.
//
// Phase 4 change: noAck=false (was: noAck=true) so broker prefetch becomes
// meaningful for backpressure. Callers MUST Ack each delivery.
func (c *Client) CreateChannel(ctx context.Context, routingKeys []string, exchangeName string, prefetch int) (<-chan amqp.Delivery, *amqp.Channel, error) {
	conn, err := c.connection(ctx)
	if err != nil {
		return nil, nil, err
	}

	// conn.Channel() is a blocking channel.open RPC with no ctx
	// parameter, and there is nothing request-scoped to close yet to
	// unblock it (the shared connection must not be torn down
	// per-caller). Run it in a worker so the caller's ctx bounds the
	// WAIT: on ctx expiry we abandon the RPC and return ctx.Err(); a
	// reaper drains the (buffered) result and closes a late-arriving
	// channel so nothing leaks. The worker itself unwinds at heartbeat
	// teardown worst-case — invisible to the caller.
	type chanResult struct {
		ch  *amqp.Channel
		err error
	}
	chanCh := make(chan chanResult, 1)
	go func() {
		ch, chErr := conn.Channel()
		chanCh <- chanResult{ch: ch, err: chErr}
	}()
	var channel *amqp.Channel
	select {
	case r := <-chanCh:
		if r.err != nil {
			return nil, nil, fmt.Errorf("feed: open channel: %w", r.err)
		}
		channel = r.ch
	case <-ctx.Done():
		go func() { // reap a late success
			if r := <-chanCh; r.ch != nil {
				_ = r.ch.Close()
			}
		}()
		return nil, nil, fmt.Errorf("feed: open channel: %w", ctx.Err())
	}

	// The channel-level RPCs below (Qos, QueueDeclare, QueueBind,
	// Consume) are blocking amqp091 calls with no ctx parameter — once a
	// live connection exists, a broker that stalls mid-topology would
	// otherwise hold the caller (Open → Connect/Subscribe) past its
	// deadline. Run the WHOLE topology sequence in a detached worker and
	// select on result-vs-ctx: the caller's deadline is honored
	// STRUCTURALLY, not by relaying ctx into channel.Close — Close is
	// itself a blocking send-and-wait RPC, so on a half-open transport
	// that withholds responses the pre-fix relay blocked right alongside
	// the topology RPC and the caller's "bounded" wait wasn't.
	//
	// Worker unwind guarantee: amqp091's heartbeat monitor tears the
	// connection down after ~2 missed intervals (read-deadline based, so
	// it fires even when writes stall), failing every pending RPC with
	// ErrClosed; client shutdown's CloseDeadline does the same, harder.
	// Until then an abandoned worker parks on the dead RPC — invisible
	// to the caller and reaped below.
	type setupResult struct {
		deliveries <-chan amqp.Delivery
		err        error
	}
	setupCh := make(chan setupResult, 1)
	go func() {
		fail := func(step string, err error) {
			_ = channel.Close() // best-effort; may itself park until conn teardown
			setupCh <- setupResult{err: fmt.Errorf("feed: %s: %w", step, err)}
		}
		if prefetch > 0 {
			if err := channel.Qos(prefetch, 0, false); err != nil {
				fail("set qos", err)
				return
			}
		}
		queue, err := channel.QueueDeclare("", false, true, true, false, nil)
		if err != nil {
			fail("declare queue", err)
			return
		}
		for _, routingKey := range routingKeys {
			if err := channel.QueueBind(queue.Name, routingKey, exchangeName, false, nil); err != nil {
				fail(fmt.Sprintf("bind %q", routingKey), err)
				return
			}
		}
		deliveries, err := channel.Consume(
			queue.Name,
			"",    // consumer tag
			false, // autoAck — Phase 4: manual ack
			true,  // exclusive
			false, // noLocal
			false, // noWait
			nil,
		)
		if err != nil {
			fail("consume", err)
			return
		}
		setupCh <- setupResult{deliveries: deliveries}
	}()

	select {
	case r := <-setupCh:
		if r.err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				// Interrupt-close raced the RPC — surface the deadline
				// as the cause, keep the amqp error as detail.
				return nil, nil, fmt.Errorf("feed: create channel: %w (amqp: %w)", ctxErr, r.err)
			}
			return nil, nil, r.err
		}
		return r.deliveries, channel, nil
	case <-ctx.Done():
		// Deadline honored NOW. Interrupt + reap from a separate
		// goroutine: channel.Close nudges a responsive broker to fail
		// the worker's pending RPC promptly, and if the worker had
		// already succeeded, closing the channel also closes its
		// deliveries — nothing leaks. On a fully stalled transport both
		// park until the heartbeat/connection teardown above.
		go func() {
			_ = channel.Close()
			<-setupCh
		}()
		return nil, nil, fmt.Errorf("feed: create channel: %w", ctx.Err())
	}
}

// connection waits for a usable AMQP connection. Returns ctx.Err() if ctx
// expires first, or ErrAlreadyClosed if the client is closed.
//
// The wake channel is snapshotted BEFORE loading c.conn so a reconnect
// that publishes between our two reads cannot leave us holding a fresh
// (unfired) wake channel while a usable connection is already live.
// Writers do Store(conn) → signalConnected() (close old wake, install
// new); reading in reverse order — wake snapshot first, then conn —
// means any signalConnected after our snapshot either fires our held
// wake (if it was the pre-existing one) or our subsequent Load picks
// up the freshly stored conn.
func (c *Client) connection(ctx context.Context) (*amqp.Connection, error) {
	for {
		wake := c.snapshotConnectedCh()
		if conn := c.conn.Load(); conn != nil && !conn.IsClosed() {
			return conn, nil
		}
		select {
		case <-c.closed:
			return nil, ErrAlreadyClosed
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-wake:
			// loop and re-check
		}
	}
}

// snapshotConnectedCh returns the current connectedCh under lock.
func (c *Client) snapshotConnectedCh() <-chan struct{} {
	c.connectedMu.Lock()
	defer c.connectedMu.Unlock()
	return c.connectedCh
}

// signalConnected closes the current connectedCh and creates a fresh one.
// Wakes everyone waiting in connection(ctx).
func (c *Client) signalConnected() {
	c.connectedMu.Lock()
	defer c.connectedMu.Unlock()
	close(c.connectedCh)
	c.connectedCh = make(chan struct{})
}

// reconnectLoop runs for the lifetime of the client. It listens on the
// connection's NotifyClose channel; on any drop, it dials a fresh
// connection with exponential backoff (capped) and atomically swaps it.
//
// Emits Disconnected on each broker drop, Reconnecting once per drop
// at the start of the backoff loop, and Connected after each
// successful re-dial. Per-attempt detail goes to slog at debug.
func (c *Client) reconnectLoop(ctx context.Context, initial *amqp.Connection) {
	defer c.wg.Done()

	conn := initial
	for {
		notify := conn.NotifyClose(make(chan *amqp.Error, 1))

		var amqpErr *amqp.Error
		select {
		case <-ctx.Done():
			return
		case <-c.closed:
			// Defence in depth: shutdown signal even if loopCtx wasn't cancelled.
			return
		case amqpErr = <-notify:
			if amqpErr == nil {
				// A nil error arrives in TWO cases and only one is terminal:
				//   (a) WE closed the connection (graceful shutdown).
				//       runShutdown cancels loopCtx BEFORE conn.CloseDeadline,
				//       so ctx.Done()/c.closed is already observable here.
				//   (b) The connection dropped in the window between the dial
				//       and this NotifyClose registration. amqp091's
				//       Connection.shutdown notifies only already-registered
				//       receivers and then sets noNotify, so a late registrant
				//       receives an immediately-closed channel yielding a nil
				//       *amqp.Error — with NO shutdown requested.
				// Treating (b) as terminal (the pre-fix behaviour) silently
				// killed the feed forever: no EventDisconnected, no reconnect,
				// connection(ctx) waiters parked until process restart. Only
				// (a) is terminal — distinguish by the shutdown signals.
				select {
				case <-ctx.Done():
					return
				case <-c.closed:
					return
				default:
				}
				// Case (b): synthesize a cause so observers still see a
				// disconnect, then fall through to reconnect.
				amqpErr = &amqp.Error{
					Code:   amqp.ConnectionForced,
					Reason: "connection closed before close-notification was registered",
				}
				c.logger.Warn("feed: connection closed before close-notification was registered; reconnecting")
			} else {
				c.logger.WithField("error", amqpErr.Error()).Warn("feed: connection lost; reconnecting")
			}
		}

		c.emit(EventDisconnected, amqpErr)
		c.emit(EventReconnecting, nil)

		// Dial with exponential backoff until ctx cancels or we succeed.
		exp := backoff.NewExponentialBackOff()
		exp.InitialInterval = 500 * time.Millisecond
		exp.MaxInterval = 30 * time.Second
		exp.RandomizationFactor = 0.3

		// WithMaxElapsedTime(0) disables backoff/v5's DEFAULT 15-minute
		// total-retry cap. Without it, any broker outage longer than
		// 15 minutes makes Retry return a non-ctx error and the loop
		// below exits permanently — feed dead until process restart.
		// NEXT.md §8 Reconnect: "Backs off forever (or until Close)."
		newConn, err := backoff.Retry(ctx, func() (*amqp.Connection, error) {
			return c.dial(ctx)
		}, backoff.WithBackOff(exp), backoff.WithMaxElapsedTime(0))
		if err != nil {
			// Reconnect loop is exiting. Pre-v2.31 this branch was
			// silent — consumer saw the feed simply stop with no
			// way to tell whether reconnect was abandoned cleanly
			// (Close cancelled ctx) or starved on a permanent
			// error. Log the cause; if the exit isn't a clean
			// shutdown, also surface via the EventDisconnected
			// event so consumers wired to ConnectionEvents() see
			// a final terminal cause.
			if ctx.Err() == nil {
				c.logger.WithError(err).Warn("feed: reconnect abandoned (non-ctx error)")
				c.emit(EventDisconnected, fmt.Errorf("feed: reconnect abandoned: %w", err))
			} else {
				c.logger.WithError(ctx.Err()).Debug("feed: reconnect loop exiting (ctx done)")
			}
			return
		}
		// Atomic publish, mirroring Open's shutdownStarted critical
		// section: runShutdown sets shutdownStarted under mu BEFORE it
		// Loads+Closes c.conn, so either we publish first (runShutdown
		// then closes newConn via its Load) or we observe the flag and
		// close newConn ourselves. Without this, a Close racing a
		// successful re-dial leaked a live connection (TCP + heartbeat
		// goroutine) that connection() would keep handing out after
		// the client reported closed.
		c.mu.Lock()
		if c.shutdownStarted {
			c.mu.Unlock()
			// CloseDeadline for the same reason as the late-Open reject:
			// runShutdown JOINS this goroutine, so an unbounded close
			// handshake here would turn a stalled broker into a shutdown
			// timeout while retaining the connection.
			_ = newConn.CloseDeadline(time.Now().Add(closeHandshakeBudget))
			return
		}
		c.conn.Store(newConn)
		c.mu.Unlock()
		conn = newConn
		c.signalConnected()
		c.emit(EventConnected, nil)
		c.logger.Info("feed: reconnected")
	}
}

// dial opens a fresh AMQP connection. Token + bookmaker details are looked
// up via the configuration and whoAmIManager — these may fetch on first call.
func (c *Client) dial(ctx context.Context) (*amqp.Connection, error) {
	mqURL, err := c.cfg.MQURL()
	if err != nil {
		return nil, fmt.Errorf("feed: resolve mq url: %w", err)
	}
	details, err := c.whoAmIManager.BookmakerDetails(ctx)
	if err != nil {
		return nil, fmt.Errorf("feed: bookmaker details: %w", err)
	}

	tok := ""
	if t := c.cfg.AccessToken(); t != nil {
		tok = *t
	}
	// Build URL safely; the access token can in principle contain URL-special chars.
	u := url.URL{
		Scheme: "amqps",
		User:   url.UserPassword(tok, ""),
		Host:   fmt.Sprintf("%s:%d", mqURL, c.cfg.MessagingPort()),
	}

	// Carry vhost in the diagnostic so AMQP auth-scope failures
	// (which are vhost-bound) are immediately diagnosable from the
	// error message alone. Sanitized first: the vhost is SERVER text
	// (who-am-i only checks non-empty), so a malformed endpoint could
	// otherwise inject unbounded / control-character content — or a
	// reflection of the access token — into caller-facing errors and
	// logs the REST layer carefully redacts everywhere else.
	vhost := details.VirtualHost()
	target := fmt.Sprintf("%s vhost=%s", u.Host, sanitizeVHostForError(vhost, tok))
	return orchestrateCtxBoundedDial(ctx, target, func(capture func(net.Conn)) (*amqp.Connection, error) {
		return amqp.DialConfig(u.String(), amqp.Config{
			Vhost:      vhost,
			Properties: amqpClientProperties(),
			Dial:       newCtxBoundDialerWithCapture(ctx, capture),
		})
	})
}

// vhostErrorMaxLen caps how much of the server-supplied virtual host a
// dial error carries. Real Oddin vhosts are short path-like strings;
// anything longer only bloats the diagnostic.
const vhostErrorMaxLen = 64

// sanitizeVHostForError makes the server-controlled virtual host safe
// to embed in error messages: the access token is redacted in case the
// endpoint reflects it back, the value is length-capped, and the result
// is quoted so control characters render as escapes instead of
// corrupting logs.
func sanitizeVHostForError(vhost, token string) string {
	if token != "" {
		vhost = strings.ReplaceAll(vhost, token, "[REDACTED]")
	}
	if len(vhost) > vhostErrorMaxLen {
		vhost = vhost[:vhostErrorMaxLen] + "…"
	}
	return strconv.Quote(vhost)
}

// amqpClientProperties is the SDK self-identification sent to the broker
// on connect. "SDK" (the language) is unchanged for backward
// compatibility; "SDK_version" is added so the version shows per live
// connection in the RabbitMQ management UI and broker logs.
func amqpClientProperties() amqp.Table {
	return amqp.Table{
		"SDK":         version.Language,
		"SDK_version": version.Version(),
	}
}

// orchestrateCtxBoundedDial runs `do` (the AMQP dial+handshake) in a
// goroutine and races its result against ctx.Done().
//
// Why this shape (race-free across the success/cancel boundary):
//
//   - Naive watcher patterns leak a goroutine-level race at two
//     boundaries: (1) ctx cancels after the underlying TCP connect
//     succeeds but before the Dial closure has captured the conn,
//     and (2) the AMQP handshake completes successfully but ctx
//     cancels before the main goroutine has stopped its watcher.
//     A v2.21 review flagged both as real (if narrow) windows that
//     can either let an in-flight handshake run for the SetDeadline
//     fallback (~30s no-deadline ctxs) or hand the caller a
//     successfully-opened-then-closed conn with err==nil.
//
//   - This shape eliminates both. The cancellation-aware `capture`
//     closure stores the conn under a mutex AND re-checks the
//     `cancelled` flag — if cancellation arrived before capture, the
//     closure closes the conn itself, aborting the handshake
//     promptly. The main goroutine selects on resultCh vs ctx.Done;
//     after a branch commits, the other side is well-defined.
//
//   - In particular: if ctx-win races with resultCh-win, the select
//     atomically picks one. If ctx wins, we close any captured raw
//     conn, then drain resultCh — and if the library returned a
//     fully-built *amqp.Connection in that tiny window we close it
//     too so its heartbeat goroutine doesn't leak. If resultCh wins,
//     we return its outcome verbatim — no late cancel can touch the
//     handed-off conn.
func orchestrateCtxBoundedDial(
	ctx context.Context,
	host string,
	do func(capture func(net.Conn)) (*amqp.Connection, error),
) (*amqp.Connection, error) {
	type dialState struct {
		mu        sync.Mutex
		conn      net.Conn
		cancelled bool
	}
	state := &dialState{}
	capture := func(c net.Conn) {
		state.mu.Lock()
		state.conn = c
		cancelled := state.cancelled
		state.mu.Unlock()
		if cancelled {
			_ = c.Close()
		}
	}

	type dialResult struct {
		conn *amqp.Connection
		err  error
	}
	resultCh := make(chan dialResult, 1)
	go func() {
		conn, err := do(capture)
		resultCh <- dialResult{conn: conn, err: err}
	}()

	select {
	case r := <-resultCh:
		if r.err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, fmt.Errorf("feed: dial %s: %w", host, ctxErr)
			}
			return nil, fmt.Errorf("feed: dial %s: %w", host, r.err)
		}
		return r.conn, nil
	case <-ctx.Done():
		state.mu.Lock()
		state.cancelled = true
		c := state.conn
		state.mu.Unlock()
		if c != nil {
			_ = c.Close()
		}
		r := <-resultCh
		if r.err == nil && r.conn != nil {
			_ = r.conn.Close()
		}
		return nil, fmt.Errorf("feed: dial %s: %w", host, ctx.Err())
	}
}

// newCtxBoundDialer returns an amqp.Config.Dial closure that bounds
// both the TCP connect AND the AMQP protocol handshake by the
// caller's ctx (or a fallback default when ctx has no deadline).
//
// The amqp091-go library, by default, uses its own 30 s dial deadline
// and ignores the ctx we threaded into Open/dial, so a short
// Connect(ctx) / lazy Subscribe(ctx) could block much longer than
// the caller asked for. net.Dialer.DialContext bounds the TCP
// connect; SetDeadline on the returned net.Conn bounds the post-TCP
// AMQP handshake too (the library clears the deadline once it
// installs heartbeat-based I/O timeouts, so steady-state reads aren't
// affected).
//
// SetDeadline alone, however, does NOT cover a cancelable-without-
// deadline ctx — the deadline is a wall-clock time that doesn't move
// when a caller's ctx Cancel() fires. dial() pairs this with the
// orchestrateCtxBoundedDial helper (result-channel + cancellation-
// aware capture pattern, see v2.22 / MIGRATION §31): the capture
// closure stores the dialed conn under a mutex and re-checks a
// shared cancelled flag, closing the conn itself when cancellation
// arrived before capture; the main goroutine selects on the dial
// result vs ctx.Done() and closes any captured (or fully-built)
// conn on the cancel branch. Together these eliminate the two race
// windows the v2.21 detached-watcher pattern had.
//
// Unexported package-internal access so v2.20 F1 / v2.22 regression
// tests can verify the caller-ctx bound without standing up a real
// AMQP broker.
func newCtxBoundDialer(ctx context.Context) func(network, addr string) (net.Conn, error) {
	return newCtxBoundDialerWithCapture(ctx, nil)
}

// newCtxBoundDialerWithCapture is newCtxBoundDialer with a hook that
// hands the dialed net.Conn back to the caller (dial() via
// orchestrateCtxBoundedDial) so the cancel branch can Close() it
// when ctx fires mid-handshake.
func newCtxBoundDialerWithCapture(ctx context.Context, capture func(net.Conn)) func(network, addr string) (net.Conn, error) {
	return func(network, addr string) (net.Conn, error) {
		var d net.Dialer
		conn, err := d.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		var deadline time.Time
		if dl, ok := ctx.Deadline(); ok {
			deadline = dl
		} else {
			deadline = time.Now().Add(dialFallbackTimeout)
		}
		// Best-effort: SetDeadline failure on a freshly dialled
		// net.Conn is rare; the library will still apply its own
		// post-handshake timeouts.
		_ = conn.SetDeadline(deadline)
		if capture != nil {
			capture(conn)
		}
		return conn, nil
	}
}

// ErrAlreadyClosed is returned when callers attempt to use a closed client.
var ErrAlreadyClosed = errors.New("feed: client closed")
