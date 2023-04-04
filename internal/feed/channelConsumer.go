package feed

import (
	"encoding/xml"
	"strings"
	"sync"
	"time"

	"github.com/oddin-gg/gosdk/internal/factory"
	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	"github.com/oddin-gg/gosdk/protocols"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/streadway/amqp"
)

const (
	sportIDPrefix = "od:sport:"
	emptyPosition = "-"
)

type envelope struct {
	SnapshotComplete      *feedXML.SnapshotComplete      `xml:"snapshot_complete"`
	Alive                 *feedXML.Alive                 `xml:"alive"`
	BetCancel             *feedXML.BetCancel             `xml:"bet_cancel"`
	BetStop               *feedXML.BetStop               `xml:"bet_stop"`
	FixtureChange         *feedXML.FixtureChange         `xml:"fixture_change"`
	OddsChange            *feedXML.OddsChange            `xml:"odds_change"`
	BetSettlement         *feedXML.BetSettlement         `xml:"bet_settlement"`
	RollbackBetSettlement *feedXML.RollbackBetSettlement `xml:"rollback_bet_settlement"`
	RollbackBetCancel     *feedXML.RollbackBetCancel     `xml:"rollback_bet_cancel"`
}

// ChannelConsumer ...
type ChannelConsumer struct {
	client             *Client
	outgoing           chan *protocols.QueueMessage
	feedMessageFactory *factory.FeedMessageFactory
	logger             *log.Logger
	mux                sync.RWMutex
	exchangeName       string
	messageInterest    *protocols.MessageInterest
	routingKeys        []string
	closed             bool
}

// Open ...
func (c *ChannelConsumer) Open(routingKeys []string, messageInterest *protocols.MessageInterest, exchangeName string) (chan *protocols.QueueMessage, error) {
	ch, err := c.client.CreateChannel(routingKeys, exchangeName)
	if err != nil {
		return nil, err
	}

	c.routingKeys = routingKeys
	c.messageInterest = messageInterest
	c.exchangeName = exchangeName

	c.outgoing = make(chan *protocols.QueueMessage, 0)

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

	//c.logger.Infof(string(msg.Body))

	envelope := envelope{}
	envelopeBytes := []byte(`<envelope>` + string(msg.Body) + `</envelope>`)
	err = xml.Unmarshal(envelopeBytes, &envelope)
	if err != nil {
		c.logger.Errorf("failed to unmarshall %s", string(msg.Body))
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

	timestamp.Published = time.Now()
	basicMessage := protocols.BasicFeedMessage{
		RawMessage: msg.Body,
		RoutingKey: routingKeyInfo,
		Timestamp:  timestamp,
	}

	var message protocols.BasicMessage
	switch {
	case envelope.BetSettlement != nil:
		message = envelope.BetSettlement
	case envelope.OddsChange != nil:
		message = envelope.OddsChange
	case envelope.FixtureChange != nil:
		message = envelope.FixtureChange
	case envelope.BetStop != nil:
		message = envelope.BetStop
	case envelope.BetCancel != nil:
		message = envelope.BetCancel
	case envelope.Alive != nil:
		message = envelope.Alive
	case envelope.SnapshotComplete != nil:
		message = envelope.SnapshotComplete
	case envelope.RollbackBetSettlement != nil:
		message = envelope.RollbackBetSettlement
	case envelope.RollbackBetCancel != nil:
		message = envelope.RollbackBetCancel
	default:
		c.logger.Errorf("unknown message - %s", string(msg.Body))
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
		return nil, errors.Errorf("incorrect route %s", route)
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
		sportURN, err = protocols.ParseURN(sportIDPrefix + sportID)
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
func NewChannelConsumer(client *Client, feedMessageFactory *factory.FeedMessageFactory, logger *log.Logger) *ChannelConsumer {
	return &ChannelConsumer{
		client:             client,
		feedMessageFactory: feedMessageFactory,
		logger:             logger,
	}
}
