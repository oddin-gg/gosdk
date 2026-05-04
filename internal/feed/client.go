package feed

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cenkalti/backoff/v5"
	"github.com/oddin-gg/gosdk/protocols"
	amqp "github.com/rabbitmq/amqp091-go"
	log "github.com/oddin-gg/gosdk/internal/log"
)

// Client manages a single AMQP connection with automatic reconnection.
//
// Phase 4 rewrite: replaces the recursive-reconnect pyramid with a single
// long-lived reconnect goroutine, atomic.Pointer for connection access (no
// data race on c.connection), backoff/v5 for exponential retry, and ctx-
// driven shutdown. Open is idempotent, Close is idempotent. Callers wait
// for a usable connection via Channel(ctx) instead of poking c.connection.
type Client struct {
	cfg           protocols.OddsFeedConfiguration
	whoAmIManager protocols.WhoAmIManager
	logger        *log.Logger

	// conn holds the current *amqp.Connection. Nil while disconnected.
	conn atomic.Pointer[amqp.Connection]

	// state machine
	mu      sync.Mutex
	opening bool
	opened  bool
	closeFn context.CancelFunc

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

// NewClient ...
func NewClient(cfg protocols.OddsFeedConfiguration, whoAmIManager protocols.WhoAmIManager, logger *log.Logger) *Client {
	return &Client{
		cfg:           cfg,
		whoAmIManager: whoAmIManager,
		logger:        logger,
		connectedCh:   make(chan struct{}),
		closed:        make(chan struct{}),
	}
}

// Open establishes the AMQP connection and starts the reconnect goroutine.
//
// Concurrent callers serialize on the manager mutex; once `opened` is true,
// subsequent Open calls return nil immediately. A failed first Open does NOT
// poison subsequent attempts — the state returns to "not opened" so the
// next call retries from scratch.
func (c *Client) Open(ctx context.Context) error {
	c.mu.Lock()
	if c.opened {
		c.mu.Unlock()
		return nil
	}
	if c.opening {
		c.mu.Unlock()
		return errors.New("feed: open already in progress")
	}
	c.opening = true
	c.mu.Unlock()

	// Reset opening on exit if we didn't reach opened=true.
	settled := false
	defer func() {
		c.mu.Lock()
		c.opening = false
		if !settled {
			c.opened = false
		}
		c.mu.Unlock()
	}()

	// First dial — synchronous so callers see a usable connection on return.
	conn, err := c.dial(ctx)
	if err != nil {
		return err
	}
	c.conn.Store(conn)
	c.signalConnected()

	// Spawn reconnect goroutine for the lifetime of the connection.
	loopCtx, cancel := context.WithCancel(context.Background())
	c.mu.Lock()
	c.closeFn = cancel
	c.opened = true
	settled = true
	c.mu.Unlock()

	c.wg.Add(1)
	go c.reconnectLoop(loopCtx, conn)
	return nil
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
	c.mu.Lock()
	cancel := c.closeFn
	c.closeFn = nil
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}

	if conn := c.conn.Load(); conn != nil {
		_ = conn.Close()
		c.conn.Store(nil)
	}
	c.wg.Wait()
	close(c.closed)
}

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

	channel, err := conn.Channel()
	if err != nil {
		return nil, nil, fmt.Errorf("feed: open channel: %w", err)
	}

	if prefetch > 0 {
		if err := channel.Qos(prefetch, 0, false); err != nil {
			_ = channel.Close()
			return nil, nil, fmt.Errorf("feed: set qos: %w", err)
		}
	}

	queue, err := channel.QueueDeclare("", false, true, true, false, nil)
	if err != nil {
		_ = channel.Close()
		return nil, nil, fmt.Errorf("feed: declare queue: %w", err)
	}

	for _, routingKey := range routingKeys {
		if err := channel.QueueBind(queue.Name, routingKey, exchangeName, false, nil); err != nil {
			_ = channel.Close()
			return nil, nil, fmt.Errorf("feed: bind %q: %w", routingKey, err)
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
		_ = channel.Close()
		return nil, nil, fmt.Errorf("feed: consume: %w", err)
	}
	return deliveries, channel, nil
}

// connection waits for a usable AMQP connection. Returns ctx.Err() if ctx
// expires first, or ErrAlreadyClosed if the client is closed.
func (c *Client) connection(ctx context.Context) (*amqp.Connection, error) {
	for {
		if conn := c.conn.Load(); conn != nil && !conn.IsClosed() {
			return conn, nil
		}
		select {
		case <-c.closed:
			return nil, ErrAlreadyClosed
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.snapshotConnectedCh():
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
func (c *Client) reconnectLoop(ctx context.Context, initial *amqp.Connection) {
	defer c.wg.Done()

	conn := initial
	for {
		notify := conn.NotifyClose(make(chan *amqp.Error, 1))

		select {
		case <-ctx.Done():
			return
		case amqpErr := <-notify:
			if amqpErr == nil {
				// Graceful close from us — exit.
				return
			}
			c.logger.WithField("error", amqpErr.Error()).Warn("feed: connection lost; reconnecting")
		}

		// Dial with exponential backoff until ctx cancels or we succeed.
		exp := backoff.NewExponentialBackOff()
		exp.InitialInterval = 500 * time.Millisecond
		exp.MaxInterval = 30 * time.Second
		exp.RandomizationFactor = 0.3

		newConn, err := backoff.Retry(ctx, func() (*amqp.Connection, error) {
			return c.dial(ctx)
		}, backoff.WithBackOff(exp))
		if err != nil {
			// ctx cancelled.
			return
		}
		c.conn.Store(newConn)
		conn = newConn
		c.signalConnected()
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

	conn, err := amqp.DialConfig(u.String(), amqp.Config{
		Vhost:      details.VirtualHost(),
		Properties: amqp.Table{"SDK": "go"},
	})
	if err != nil {
		return nil, fmt.Errorf("feed: dial %s: %w", u.Host, err)
	}
	return conn, nil
}

// ErrAlreadyClosed is returned when callers attempt to use a closed client.
var ErrAlreadyClosed = errors.New("feed: client closed")
