package gosdk

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/oddin-gg/gosdk/internal/cache"
	"github.com/oddin-gg/gosdk/internal/factory"
	"github.com/oddin-gg/gosdk/internal/feed"
	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	"github.com/oddin-gg/gosdk/internal/producer"
	"github.com/oddin-gg/gosdk/protocols"
	log "github.com/sirupsen/logrus"
)

type sdkOddsFeedSession interface {
	protocols.OddsFeedSession
	Open(
		routingKeys []string,
		messageInterest *protocols.MessageInterest,
		reportExtendedData bool,
	) error
	Close()
	IsReplay() bool
}

type oddsFeedSessionImpl struct {
	channelConsumer          *feed.ChannelConsumer
	producerManager          *producer.Manager
	cacheManager             *cache.Manager
	feedMessageFactory       *factory.FeedMessageFactory
	recoveryMessageProcessor protocols.RecoveryMessageProcessor
	exchangeName             string
	sessionID                uuid.UUID
	logger                   *log.Logger
	closeCh                  chan bool
	msgCh                    chan protocols.SessionMessage
	isReplay                 bool
	messageInterest          interface{}
}

func (o *oddsFeedSessionImpl) RespCh() protocols.SessionMessageDelivery {
	return o.msgCh
}

func (o *oddsFeedSessionImpl) IsReplay() bool {
	return o.isReplay
}

func (o *oddsFeedSessionImpl) Open(
	routingKeys []string,
	messageInterest *protocols.MessageInterest,
	reportExtendedData bool) error {
	if o.closeCh != nil {
		return errors.New("session is already opened")
	}

	ch, err := o.channelConsumer.Open(routingKeys, messageInterest, o.exchangeName)
	if err != nil {
		return err
	}

	o.closeCh = make(chan bool, 1)

	go func(messageInterest *protocols.MessageInterest) {
		for {
			select {
			case <-o.closeCh:
				return
			case msg := <-ch:
				o.processMessage(msg, messageInterest, reportExtendedData)
			}
		}
	}(messageInterest)

	return nil
}

func (o *oddsFeedSessionImpl) Close() {
	o.cacheManager.Close()
	o.channelConsumer.Close()
	if o.msgCh != nil {
		close(o.msgCh)
	}

	if o.closeCh != nil {
		o.closeCh <- true
	}

	o.closeCh = nil
}

func (o *oddsFeedSessionImpl) ID() uuid.UUID {
	return o.sessionID
}

func (o *oddsFeedSessionImpl) processMessage(msg *protocols.QueueMessage, messageInterest *protocols.MessageInterest, reportExtendedData bool) {
	if msg.UnparsableMessage != nil {
		o.msgCh <- protocols.SessionMessage{
			UnparsableMessage: msg.UnparsableMessage,
		}
		return
	}

	if msg.RawFeedMessage != nil && reportExtendedData {
		o.msgCh <- protocols.SessionMessage{
			RawFeedMessage: msg.RawFeedMessage,
		}
	}

	if msg.FeedMessage == nil {
		return
	}

	producerID := msg.FeedMessage.Message.Product()
	producerData, err := o.producerManager.GetProducer(producerID)
	if err != nil {
		o.logger.WithError(err).Errorf("failed to get producer %d", producerID)
	}

	isProducerEnabled, err := o.producerManager.IsProducerEnabled(producerID)
	switch {
	case err != nil:
		o.logger.WithError(err).Errorf("failed to check if producer is enabled %d", producerID)
	case !isProducerEnabled:
		return
	case !messageInterest.IsProducerInScope(producerData):
		return
	}

	o.processFeedMessage(msg.FeedMessage, *messageInterest)

}

func (o *oddsFeedSessionImpl) processFeedMessage(feedMessage *protocols.FeedMessage, messageInterest protocols.MessageInterest) {
	producerID := feedMessage.Message.Product()
	o.recoveryMessageProcessor.OnMessageProcessingStarted(o.sessionID, producerID, time.Now())

	o.cacheManager.OnFeedMessageReceived(feedMessage)

	switch msg := feedMessage.Message.(type) {
	case *feedXML.Alive:
		o.recoveryMessageProcessor.OnAliveReceived(producerID, feedMessage.Timestamp, msg.Subscribed == 1, messageInterest)
		o.recoveryMessageProcessor.OnMessageProcessingEnded(o.sessionID, producerID, feedMessage.Timestamp.Created)
		return
	case *feedXML.SnapshotComplete:
		o.recoveryMessageProcessor.OnSnapshotCompleteReceived(producerID, msg.RequestID, messageInterest)
		o.recoveryMessageProcessor.OnMessageProcessingEnded(o.sessionID, producerID, time.Time{})
		return
	}

	message, err := o.feedMessageFactory.BuildMessage(feedMessage)
	if err != nil {
		o.logger.WithError(err).Errorf("failed to build message from feed message %s", feedMessage)
		unparsableMsg := o.feedMessageFactory.BuildUnparsableMessage(feedMessage)
		o.msgCh <- protocols.SessionMessage{
			UnparsableMessage: unparsableMsg,
		}
		return
	}

	var timestamp time.Time
	switch msg := message.(type) {
	case protocols.OddsChange:
		timestamp = msg.Timestamp().Created
		o.msgCh <- protocols.SessionMessage{
			Message: msg,
		}
	case protocols.BetStop:
		timestamp = msg.Timestamp().Created
		o.msgCh <- protocols.SessionMessage{
			Message: msg,
		}
	case protocols.BetCancel:
		o.msgCh <- protocols.SessionMessage{
			Message: msg,
		}
	case protocols.BetSettlement:
		o.msgCh <- protocols.SessionMessage{
			Message: msg,
		}
	case protocols.FixtureChangeMessage:
		o.msgCh <- protocols.SessionMessage{
			Message: msg,
		}
	case protocols.RollbackBetSettlement:
		o.msgCh <- protocols.SessionMessage{
			Message: msg,
		}
	case protocols.RollbackBetCancel:
		o.msgCh <- protocols.SessionMessage{
			Message: msg,
		}
	default:
		unparsableMsg := o.feedMessageFactory.BuildUnparsableMessage(feedMessage)
		o.msgCh <- protocols.SessionMessage{
			UnparsableMessage: unparsableMsg,
		}
	}

	o.recoveryMessageProcessor.OnMessageProcessingEnded(o.sessionID, producerID, timestamp)
}

func newSession(
	rabbitMQClient *feed.Client,
	producerManager *producer.Manager,
	cacheManager *cache.Manager,
	feedMessageFactory *factory.FeedMessageFactory,
	recoverMessageProcessor protocols.RecoveryMessageProcessor,
	exchangeName string,
	isReplay bool,
	logger *log.Logger) sdkOddsFeedSession {
	return &oddsFeedSessionImpl{
		channelConsumer:          feed.NewChannelConsumer(rabbitMQClient, feedMessageFactory, logger),
		producerManager:          producerManager,
		cacheManager:             cacheManager,
		feedMessageFactory:       feedMessageFactory,
		recoveryMessageProcessor: recoverMessageProcessor,
		exchangeName:             exchangeName,
		sessionID:                uuid.New(),
		isReplay:                 isReplay,
		logger:                   logger,
		msgCh:                    make(chan protocols.SessionMessage, 0),
	}
}
