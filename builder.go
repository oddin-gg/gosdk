package gosdk

import (
	"errors"

	"github.com/google/uuid"
	"github.com/oddin-gg/gosdk/internal/cache"
	"github.com/oddin-gg/gosdk/internal/factory"
	"github.com/oddin-gg/gosdk/internal/feed"
	"github.com/oddin-gg/gosdk/internal/producer"
	"github.com/oddin-gg/gosdk/internal/recovery"
	"github.com/oddin-gg/gosdk/protocols"
	log "github.com/sirupsen/logrus"
)

type builderImpl struct {
	sessionMap               map[uuid.UUID]*sessionData
	messageInterest          *protocols.MessageInterest
	eventIDS                 map[protocols.URN]struct{}
	oddsFeedConfiguration    protocols.OddsFeedConfiguration
	rabbitMQClient           *feed.Client
	producerManager          *producer.Manager
	cacheManager             *cache.Manager
	feedMessageFactory       *factory.FeedMessageFactory
	recoveryMessageProcessor protocols.RecoveryMessageProcessor
	logger                   *log.Entry
}

func (b *builderImpl) SetMessageInterest(messageInterest protocols.MessageInterest) protocols.OddsFeedSessionBuilder {
	b.messageInterest = &messageInterest
	return b
}

func (b *builderImpl) SetSpecificEventsOnly(specificEvents []protocols.URN) protocols.OddsFeedSessionBuilder {
	b.eventIDS = make(map[protocols.URN]struct{}, len(specificEvents))
	for i := range specificEvents {
		event := specificEvents[i]
		b.eventIDS[event] = struct{}{}
	}

	return b
}

func (b *builderImpl) SetSpecificEventOnly(specificEventOnly protocols.URN) protocols.OddsFeedSessionBuilder {
	b.eventIDS = make(map[protocols.URN]struct{}, 1)
	b.eventIDS[specificEventOnly] = struct{}{}
	return b
}

func (b *builderImpl) Build() (protocols.SessionMessageDelivery, error) {
	if b.messageInterest == nil {
		return nil, errors.New("message interest is not specified")
	}

	session := newSession(
		b.rabbitMQClient,
		b.producerManager,
		b.cacheManager,
		b.feedMessageFactory,
		b.recoveryMessageProcessor,
		b.oddsFeedConfiguration.ExchangeName(),
		false,
		b.logger,
	)
	sessionData := &sessionData{
		session:         session,
		messageInterest: b.messageInterest,
		eventIDs:        b.eventIDS,
	}
	b.sessionMap[session.ID()] = sessionData

	return session.RespCh(), nil
}

func (b *builderImpl) BuildReplay() (protocols.SessionMessageDelivery, error) {
	session := newSession(
		b.rabbitMQClient,
		b.producerManager,
		b.cacheManager,
		b.feedMessageFactory,
		&recovery.DummyManager{},
		b.oddsFeedConfiguration.ReplayExchangeName(),
		true,
		b.logger,
	)

	messageInterest := protocols.AllMessageInterest
	sessionData := &sessionData{
		session:         session,
		messageInterest: &messageInterest,
		eventIDs:        b.eventIDS,
	}
	b.sessionMap[session.ID()] = sessionData

	return session.RespCh(), nil
}

type sessionData struct {
	session         sdkOddsFeedSession
	messageInterest *protocols.MessageInterest
	eventIDs        map[protocols.URN]struct{}
	isAliveOnly     bool
}
