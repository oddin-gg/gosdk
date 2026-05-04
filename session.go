package gosdk

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/oddin-gg/gosdk/internal/cache"
	"github.com/oddin-gg/gosdk/internal/factory"
	"github.com/oddin-gg/gosdk/internal/feed"
	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	"github.com/oddin-gg/gosdk/internal/producer"
	"github.com/oddin-gg/gosdk/types"
	log "github.com/oddin-gg/gosdk/internal/log"
)

// sdkOddsFeedSession is the internal interface the legacy session impl
// satisfies. It used to embed a public types.OddsFeedSession; that
// public interface was retired alongside the manager-of-managers shape.
type sdkOddsFeedSession interface {
	ID() uuid.UUID
	RespCh() types.SessionMessageDelivery
	Open(
		ctx context.Context,
		routingKeys []string,
		messageInterest *types.MessageInterest,
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
	recoveryMessageProcessor types.RecoveryMessageProcessor
	exchangeName             string
	sportIDPrefix            string
	sessionID                uuid.UUID
	logger                   *log.Logger
	closeCh                  chan bool
	msgCh                    chan types.SessionMessage
	isReplay                 bool
}

func (o *oddsFeedSessionImpl) RespCh() types.SessionMessageDelivery {
	return o.msgCh
}

func (o *oddsFeedSessionImpl) IsReplay() bool {
	return o.isReplay
}

func (o *oddsFeedSessionImpl) Open(
	ctx context.Context,
	routingKeys []string,
	messageInterest *types.MessageInterest,
	reportExtendedData bool) error {
	if o.closeCh != nil {
		return errors.New("session is already opened")
	}

	ch, err := o.channelConsumer.Open(ctx, routingKeys, messageInterest)
	if err != nil {
		return err
	}

	o.closeCh = make(chan bool, 1)

	go func(messageInterest *types.MessageInterest) {
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
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = o.channelConsumer.Close(shutdownCtx)
	cancel()
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

func (o *oddsFeedSessionImpl) processMessage(msg *types.QueueMessage, messageInterest *types.MessageInterest, reportExtendedData bool) {
	if msg.UnparsableMessage != nil {
		o.msgCh <- types.SessionMessage{
			UnparsableMessage: msg.UnparsableMessage,
		}
		return
	}

	if msg.RawFeedMessage != nil && reportExtendedData {
		o.msgCh <- types.SessionMessage{
			RawFeedMessage: msg.RawFeedMessage,
		}
	}

	if msg.FeedMessage == nil {
		return
	}

	producerID := msg.FeedMessage.Message.Product()
	// Hot path: producers map is populated at SDK startup; these calls are
	// in-memory cache reads after init. context.Background() is acceptable
	// here because the call paths cannot block on I/O at message-processing time.
	bg := context.Background()
	producerData, err := o.producerManager.GetProducer(bg, producerID)
	if err != nil {
		o.logger.WithError(err).Errorf("failed to get producer %d", producerID)
	}

	isProducerEnabled, err := o.producerManager.IsProducerEnabled(bg, producerID)
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

func (o *oddsFeedSessionImpl) processFeedMessage(feedMessage *types.FeedMessage, messageInterest types.MessageInterest) {
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
		o.logger.WithError(err).Errorf("failed to build message from feed message %v", feedMessage)
		unparsableMsg := o.feedMessageFactory.BuildUnparsableMessage(feedMessage)
		o.msgCh <- types.SessionMessage{
			UnparsableMessage: unparsableMsg,
		}
		return
	}

	var timestamp time.Time
	switch msg := message.(type) {
	case types.OddsChange:
		timestamp = msg.Timestamp().Created
		o.msgCh <- types.SessionMessage{
			Message: msg,
		}
	case types.BetStop:
		timestamp = msg.Timestamp().Created
		o.msgCh <- types.SessionMessage{
			Message: msg,
		}
	case types.BetCancel:
		o.msgCh <- types.SessionMessage{
			Message: msg,
		}
	case types.BetSettlement:
		o.msgCh <- types.SessionMessage{
			Message: msg,
		}
	case types.FixtureChangeMessage:
		o.msgCh <- types.SessionMessage{
			Message: msg,
		}
	case types.RollbackBetSettlement:
		o.msgCh <- types.SessionMessage{
			Message: msg,
		}
	case types.RollbackBetCancel:
		o.msgCh <- types.SessionMessage{
			Message: msg,
		}
	default:
		unparsableMsg := o.feedMessageFactory.BuildUnparsableMessage(feedMessage)
		o.msgCh <- types.SessionMessage{
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
	recoverMessageProcessor types.RecoveryMessageProcessor,
	exchangeName string,
	sportIDPrefix string,
	isReplay bool,
	logger *log.Logger,
) sdkOddsFeedSession {
	return &oddsFeedSessionImpl{
		channelConsumer: feed.NewChannelConsumer(
			rabbitMQClient,
			feedMessageFactory,
			logger,
			exchangeName,
			sportIDPrefix,
		),
		producerManager:          producerManager,
		cacheManager:             cacheManager,
		feedMessageFactory:       feedMessageFactory,
		recoveryMessageProcessor: recoverMessageProcessor,
		exchangeName:             exchangeName,
		sportIDPrefix:            sportIDPrefix,
		sessionID:                uuid.New(),
		isReplay:                 isReplay,
		logger:                   logger,
		msgCh:                    make(chan types.SessionMessage),
	}
}
