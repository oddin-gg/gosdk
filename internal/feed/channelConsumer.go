package feed

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/oddin-gg/gosdk/internal/factory"
	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	"github.com/oddin-gg/gosdk/types"
	amqp "github.com/rabbitmq/amqp091-go"
	log "github.com/oddin-gg/gosdk/internal/log"
)

const (
	emptyPosition = "-"

	// defaultPrefetch caps the unacked-deliveries window per consumer. With
	// auto-ack the broker considered every delivery acked instantly, so this
	// had no effect. With manual-ack (Phase 4 §0.6), this is the real
	// backpressure knob — beyond this, the broker stops delivering until the
	// consumer drains and acks.
	defaultPrefetch = 1000

	// defaultBufferSize is the in-process subscription channel size.
	defaultBufferSize = 256
)

// ChannelConsumer drains AMQP deliveries, decodes them, admits decoded
// messages into an outgoing channel under ctx-cancellable backpressure, and
// acks the broker only after admission. On connection drops the AMQP
// delivery channel closes; the consumer loop transparently waits for the
// Client to reconnect and re-creates its consumer channel.
type ChannelConsumer struct {
	client             *Client
	feedMessageFactory *factory.FeedMessageFactory
	logger             *log.Logger
	exchangeName       string
	sportIDPrefix      string

	prefetch   int
	bufferSize int

	mu              sync.Mutex
	outgoing        chan *types.QueueMessage
	closeFn         context.CancelFunc
	messageInterest *types.MessageInterest
	routingKeys     []string
	loopCtx         context.Context
	closeOnce       sync.Once
	closed          chan struct{}
	wg              sync.WaitGroup
}

// NewChannelConsumer constructs an unstarted consumer. Call Open to begin.
func NewChannelConsumer(
	client *Client,
	feedMessageFactory *factory.FeedMessageFactory,
	logger *log.Logger,
	exchangeName string,
	sportIDPrefix string,
) *ChannelConsumer {
	return &ChannelConsumer{
		client:             client,
		feedMessageFactory: feedMessageFactory,
		logger:             logger,
		exchangeName:       exchangeName,
		sportIDPrefix:      sportIDPrefix,
		prefetch:           defaultPrefetch,
		bufferSize:         defaultBufferSize,
		closed:             make(chan struct{}),
	}
}

// Open subscribes to the configured routing keys and starts the consumer
// loop. The returned channel emits decoded messages until Close cancels.
//
// On AMQP-level reconnect (handled by Client) the consumer re-creates its
// channel transparently — callers don't need to react.
func (c *ChannelConsumer) Open(ctx context.Context, routingKeys []string, messageInterest *types.MessageInterest) (<-chan *types.QueueMessage, error) {
	c.mu.Lock()
	if c.outgoing != nil {
		c.mu.Unlock()
		return nil, errors.New("feed: consumer already opened")
	}

	c.routingKeys = append([]string(nil), routingKeys...)
	c.messageInterest = messageInterest
	c.outgoing = make(chan *types.QueueMessage, c.bufferSize)

	// Loop ctx must outlive the caller's Open ctx (which bounds queue
	// declaration only). WithoutCancel propagates caller metadata while
	// severing the cancellation chain; closeFn cancels at Close() time.
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	c.loopCtx = loopCtx
	c.closeFn = cancel
	out := c.outgoing
	c.mu.Unlock()

	c.wg.Add(1)
	go c.run(loopCtx)
	return out, nil
}

// Close terminates the consumer loop, waits for it to exit, and closes the
// outgoing channel. Idempotent. Cap the wait via the supplied ctx.
func (c *ChannelConsumer) Close(ctx context.Context) error {
	c.closeOnce.Do(func() { go c.runShutdown() })

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

func (c *ChannelConsumer) runShutdown() {
	c.mu.Lock()
	cancel := c.closeFn
	c.closeFn = nil
	out := c.outgoing
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	c.wg.Wait()
	if out != nil {
		close(out)
	}
	close(c.closed)
}

// run is the single consumer goroutine. It (1) opens an AMQP channel,
// (2) loops over deliveries, decoding + admitting + acking, (3) on
// delivery-channel close (connection drop), it waits for the Client to
// reconnect and reopens its AMQP channel. The loop terminates on ctx.Done.
func (c *ChannelConsumer) run(ctx context.Context) {
	defer c.wg.Done()

	for {
		// Open a fresh AMQP channel (waits for the connection to be up).
		deliveries, ch, err := c.client.CreateChannel(ctx, c.routingKeys, c.exchangeName, c.prefetch)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// Transient failure (e.g., brief disconnect window). Retry shortly.
			c.logger.WithError(err).Warn("feed: open consumer channel failed; retrying")
			select {
			case <-ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
				continue
			}
		}

		c.consume(ctx, deliveries, ch)

		// consume returned: either ctx cancelled or delivery channel closed.
		// Close the AMQP channel and either exit (ctx) or loop to reopen.
		_ = ch.Close()
		if ctx.Err() != nil {
			return
		}
	}
}

// consume drives a single AMQP-channel session. Returns when the delivery
// channel closes (typical reason: connection drop) or ctx cancels.
func (c *ChannelConsumer) consume(ctx context.Context, deliveries <-chan amqp.Delivery, ch *amqp.Channel) {
	for {
		select {
		case <-ctx.Done():
			return
		case d, ok := <-deliveries:
			if !ok {
				// Channel closed (connection drop or broker close).
				return
			}
			qm := c.processDelivery(ctx, d)
			if qm == nil {
				// processDelivery already nack'd or ack'd as appropriate.
				continue
			}

			// Manual ack semantics: send to subscription buffer first, then ack.
			// If ctx cancels mid-flight, Nack(requeue=false) — recovery
			// handles the gap.
			select {
			case c.outgoing <- qm:
				if err := d.Ack(false); err != nil {
					c.logger.WithError(err).Warn("feed: ack failed")
				}
			case <-ctx.Done():
				_ = d.Nack(false, false)
				return
			}
		}
	}
}

// processDelivery decodes a single AMQP delivery into a *types.QueueMessage.
// On decode failure or routing-key parse failure it returns an unparsable
// message; the caller still admits it to the buffer (consumer wants to know).
// Returns nil only in the empty-body fast path that ack's and skips.
func (c *ChannelConsumer) processDelivery(ctx context.Context, d amqp.Delivery) *types.QueueMessage {
	timestamp := types.MessageTimestamp{
		Created:  d.Timestamp,
		Sent:     d.Timestamp,
		Received: time.Now(),
	}

	routingKeyInfo, err := c.parseRoute(d.RoutingKey)
	if err != nil {
		c.logger.WithError(err).Errorf("failed to parse route %s", d.RoutingKey)
		// Unparsable routing key — ack and skip; nothing useful to admit.
		_ = d.Ack(false)
		return nil
	}

	queueMessage := &types.QueueMessage{}

	if len(d.Body) == 0 {
		c.logger.Warnf("received message without proper body from %s", d.RoutingKey)
		queueMessage.UnparsableMessage = c.feedMessageFactory.BuildUnparsableMessage(ctx, &types.FeedMessage{
			BasicFeedMessage: types.BasicFeedMessage{
				RawMessage: d.Body,
				RoutingKey: routingKeyInfo,
				Timestamp:  timestamp,
			},
		})
		return queueMessage
	}

	message, err := feedXML.Decode(d.Body)
	if err != nil {
		switch {
		case errors.Is(err, feedXML.ErrUnknownMessage):
			c.logger.Errorf("unknown message - %s", string(d.Body))
		default:
			c.logger.WithError(err).Errorf("failed to unmarshall %s", string(d.Body))
		}
		queueMessage.UnparsableMessage = c.feedMessageFactory.BuildUnparsableMessage(ctx, &types.FeedMessage{
			BasicFeedMessage: types.BasicFeedMessage{
				RawMessage: d.Body,
				RoutingKey: routingKeyInfo,
				Timestamp:  timestamp,
			},
		})
		return queueMessage
	}

	timestamp.Published = time.Now()
	basicMessage := types.BasicFeedMessage{
		RawMessage: d.Body,
		RoutingKey: routingKeyInfo,
		Timestamp:  timestamp,
	}

	queueMessage.RawFeedMessage = &types.RawFeedMessage{
		BasicFeedMessage: basicMessage,
		Message:          message,
		MessageInterest:  *c.messageInterest,
	}
	queueMessage.FeedMessage = &types.FeedMessage{
		BasicFeedMessage: basicMessage,
		Message:          message,
	}
	return queueMessage
}

func (c *ChannelConsumer) parseRoute(route string) (*types.RoutingKeyInfo, error) {
	parts := strings.Split(route, ".")
	if len(parts) != 8 {
		return nil, fmt.Errorf("incorrect route %s", route)
	}

	sportID := parts[4]
	eventID := parts[6]
	hasID := sportID != emptyPosition || eventID != emptyPosition
	if !hasID {
		return &types.RoutingKeyInfo{
			FullRoutingKey:     route,
			IsSystemRoutingKey: true,
		}, nil
	}

	var (
		err      error
		sportURN *types.URN
		eventURN *types.URN
	)
	if sportID != emptyPosition {
		sportURN, err = types.ParseURN(c.sportIDPrefix + sportID)
		if err != nil {
			return nil, err
		}
	}

	eventType := parts[5]
	if eventType != emptyPosition && eventID != emptyPosition {
		eventURN, err = types.ParseURN(eventType + ":" + eventID)
		if err != nil {
			return nil, err
		}
	}

	return &types.RoutingKeyInfo{
		FullRoutingKey:     route,
		SportID:            sportURN,
		EventID:            eventURN,
		IsSystemRoutingKey: false,
	}, nil
}
