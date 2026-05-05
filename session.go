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
	Close(ctx context.Context)
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
	closeFn                  context.CancelFunc
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
	if o.closeFn != nil {
		return errors.New("session is already opened")
	}

	ch, err := o.channelConsumer.Open(ctx, routingKeys, messageInterest)
	if err != nil {
		return err
	}

	// Loop ctx must outlive the caller's Open ctx (which only bounds the
	// consumer's queue declaration). WithoutCancel propagates caller
	// metadata while severing the cancellation chain; closeFn cancels at
	// Close() time.
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	o.closeFn = cancel

	go func(messageInterest *types.MessageInterest) {
		for {
			select {
			case <-loopCtx.Done():
				return
			case msg := <-ch:
				o.processMessage(loopCtx, msg, messageInterest, reportExtendedData)
			}
		}
	}(messageInterest)

	return nil
}

// Close drains the consumer and tears down the session. ctx bounds
// the wait for the consumer goroutine to exit (cancellation of the
// session's loop ctx is unconditional; ctx only caps how long we
// wait for cleanup to complete).
func (o *oddsFeedSessionImpl) Close(ctx context.Context) {
	o.cacheManager.Close()
	_ = o.channelConsumer.Close(ctx)
	if o.msgCh != nil {
		close(o.msgCh)
	}

	if o.closeFn != nil {
		o.closeFn()
	}
	o.closeFn = nil
}

func (o *oddsFeedSessionImpl) ID() uuid.UUID {
	return o.sessionID
}

func (o *oddsFeedSessionImpl) processMessage(ctx context.Context, msg *types.QueueMessage, messageInterest *types.MessageInterest, reportExtendedData bool) {
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
	// Producers map is populated at SDK startup; these are in-memory
	// cache reads after init, but ctx is still propagated so any future
	// I/O fallback or instrumentation hooks observe the loop ctx.
	producerData, err := o.producerManager.GetProducer(ctx, producerID)
	if err != nil {
		o.logger.WithError(err).Errorf("failed to get producer %d", producerID)
	}

	isProducerEnabled, err := o.producerManager.IsProducerEnabled(ctx, producerID)
	switch {
	case err != nil:
		o.logger.WithError(err).Errorf("failed to check if producer is enabled %d", producerID)
	case !isProducerEnabled:
		return
	case !messageInterest.IsProducerInScope(producerData):
		return
	}

	o.processFeedMessage(ctx, msg.FeedMessage, *messageInterest)

}

func (o *oddsFeedSessionImpl) processFeedMessage(ctx context.Context, feedMessage *types.FeedMessage, messageInterest types.MessageInterest) {
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

	message, err := o.feedMessageFactory.BuildMessage(ctx, feedMessage)
	if err != nil {
		o.logger.WithError(err).Errorf("failed to build message from feed message %v", feedMessage)
		unparsableMsg := o.feedMessageFactory.BuildUnparsableMessage(ctx, feedMessage)
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
		unparsableMsg := o.feedMessageFactory.BuildUnparsableMessage(ctx, feedMessage)
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
