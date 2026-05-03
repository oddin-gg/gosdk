package feed

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oddin-gg/gosdk/internal/factory"
	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	"github.com/oddin-gg/gosdk/protocols"
	amqp "github.com/rabbitmq/amqp091-go"
	log "github.com/sirupsen/logrus"
)

const (
	emptyPosition = "-"
)

// ChannelConsumer ...
type ChannelConsumer struct {
	client             *Client
	outgoing           chan *protocols.QueueMessage
	feedMessageFactory *factory.FeedMessageFactory
	logger             *log.Entry
	exchangeName       string
	sportIDPrefix      string
	messageInterest    *protocols.MessageInterest
	routingKeys        []string
	closed             bool
}

// Open ...
func (c *ChannelConsumer) Open(routingKeys []string, messageInterest *protocols.MessageInterest) (chan *protocols.QueueMessage, error) {
	ch, err := c.client.CreateChannel(routingKeys, c.exchangeName)
	if err != nil {
		return nil, err
	}

	c.routingKeys = routingKeys
	c.messageInterest = messageInterest
	c.outgoing = make(chan *protocols.QueueMessage)

	c.consumeMessage(ch)

	return c.outgoing, nil
}

// Close ...
func (c *ChannelConsumer) Close() {
	c.closed = true
}

func (c *ChannelConsumer) reconnect() {
	if c.closed {
		return
	}

	c.logger.Warnf("channel closed, trying reconnect...")

	ch, err := c.client.CreateChannel(c.routingKeys, c.exchangeName)
	if err != nil {
		c.logger.WithError(err).Error("failed to reconnect channel, retrying ...")
		time.Sleep(5 * time.Second)
		go c.reconnect()
		return
	}

	c.consumeMessage(ch)
}

func (c *ChannelConsumer) consumeMessage(ch <-chan amqp.Delivery) {
	go func() {
		for msg := range ch {
			if c.closed {
				return
			}

			c.processMessage(msg)
		}

		c.reconnect()
	}()
}

func (c *ChannelConsumer) processMessage(msg amqp.Delivery) {
	timestamp := protocols.MessageTimestamp{
		Created:   msg.Timestamp,
		Sent:      msg.Timestamp,
		Received:  time.Now(),
		Published: time.Time{},
	}

	routingKeyInfo, err := c.parseRoute(msg.RoutingKey)
	if err != nil {
		c.logger.WithError(err).Errorf("failed to parse route %s", msg.RoutingKey)
		return
	}

	queueMessage := &protocols.QueueMessage{}

	if msg.Body == nil || len(msg.Body) == 0 {
		c.logger.Warnf("received message without proper body from %s", msg.RoutingKey)
		message := protocols.FeedMessage{
			BasicFeedMessage: protocols.BasicFeedMessage{
				RawMessage: msg.Body,
				RoutingKey: routingKeyInfo,
				Timestamp:  timestamp,
			},
			Message: nil,
		}
		queueMessage.UnparsableMessage = c.feedMessageFactory.BuildUnparsableMessage(&message)
		c.outgoing <- queueMessage
		return
	}

	message, err := feedXML.Decode(msg.Body)
	if err != nil {
		switch {
		case errors.Is(err, feedXML.ErrUnknownMessage):
			c.logger.Errorf("unknown message - %s", string(msg.Body))
		default:
			c.logger.WithError(err).Errorf("failed to unmarshall %s", string(msg.Body))
		}
		fm := protocols.FeedMessage{
			BasicFeedMessage: protocols.BasicFeedMessage{
				RawMessage: msg.Body,
				RoutingKey: routingKeyInfo,
				Timestamp:  timestamp,
			},
			Message: nil,
		}
		queueMessage.UnparsableMessage = c.feedMessageFactory.BuildUnparsableMessage(&fm)
		c.outgoing <- queueMessage
		return
	}

	timestamp.Published = time.Now()
	basicMessage := protocols.BasicFeedMessage{
		RawMessage: msg.Body,
		RoutingKey: routingKeyInfo,
		Timestamp:  timestamp,
	}

	queueMessage.RawFeedMessage = &protocols.RawFeedMessage{
		BasicFeedMessage: basicMessage,
		Message:          message,
		MessageInterest:  *c.messageInterest,
	}

	queueMessage.FeedMessage = &protocols.FeedMessage{
		BasicFeedMessage: basicMessage,
		Message:          message,
	}

	c.outgoing <- queueMessage
}

func (c *ChannelConsumer) parseRoute(route string) (*protocols.RoutingKeyInfo, error) {
	parts := strings.Split(route, ".")
	if len(parts) != 8 {
		return nil, fmt.Errorf("incorrect route %s", route)
	}

	sportID := parts[4]
	eventID := parts[6]
	var hasID bool
	if sportID != emptyPosition || eventID != emptyPosition {
		hasID = true
	}

	if !hasID {
		return &protocols.RoutingKeyInfo{
			FullRoutingKey:     route,
			SportID:            nil,
			EventID:            nil,
			IsSystemRoutingKey: true,
		}, nil
	}

	var err error

	var sportURN *protocols.URN
	if sportID != emptyPosition {
		sportURN, err = protocols.ParseURN(c.sportIDPrefix + sportID)
		if err != nil {
			return nil, err
		}
	}

	eventType := parts[5]
	var eventURN *protocols.URN
	if eventType != emptyPosition && eventID != emptyPosition {
		eventURN, err = protocols.ParseURN(eventType + ":" + eventID)
		if err != nil {
			return nil, err
		}
	}

	return &protocols.RoutingKeyInfo{
		FullRoutingKey:     route,
		SportID:            sportURN,
		EventID:            eventURN,
		IsSystemRoutingKey: false,
	}, nil
}

// NewChannelConsumer ...
func NewChannelConsumer(
	client *Client,
	feedMessageFactory *factory.FeedMessageFactory,
	logger *log.Entry,
	exchangeName string,
	sportIDPrefix string,
) *ChannelConsumer {
	return &ChannelConsumer{
		client:             client,
		feedMessageFactory: feedMessageFactory,
		logger:             logger,
		exchangeName:       exchangeName,
		sportIDPrefix:      sportIDPrefix,
	}
}
