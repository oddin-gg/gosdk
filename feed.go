package gosdk

import (
	"fmt"

	"github.com/google/uuid"
	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/cache"
	"github.com/oddin-gg/gosdk/internal/factory"
	"github.com/oddin-gg/gosdk/internal/feed"
	"github.com/oddin-gg/gosdk/internal/market"
	"github.com/oddin-gg/gosdk/internal/producer"
	"github.com/oddin-gg/gosdk/internal/recovery"
	"github.com/oddin-gg/gosdk/internal/replay"
	"github.com/oddin-gg/gosdk/internal/sport"
	"github.com/oddin-gg/gosdk/internal/whoami"
	"github.com/oddin-gg/gosdk/protocols"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

const (
	snapshotKeyTemplate = "-.-.-.snapshot_complete.-.-.-."
)

type oddsFeedImpl struct {
	feedInitialized          bool
	whoAmIManager            protocols.WhoAmIManager
	producerManager          *producer.Manager
	recoveryManager          *recovery.Manager
	marketDescriptionManager protocols.MarketDescriptionManager
	sportsInfoManager        protocols.SportsInfoManager
	replayManager            protocols.ReplayManager
	cfg                      protocols.OddsFeedConfiguration
	logger                   *log.Logger
	apiClient                *api.Client
	opened                   bool
	cacheManager             *cache.Manager
	rabbitMQClient           *feed.Client
	feedMessageFactory       *factory.FeedMessageFactory
	sessionMap               map[uuid.UUID]*sessionData
	msgCh                    chan protocols.GlobalMessage
	closeCh                  chan bool
}

func (o *oddsFeedImpl) SessionBuilder() (protocols.OddsFeedSessionBuilder, error) {
	if err := o.init(); err != nil {
		return nil, err
	}

	return &builderImpl{
		oddsFeedConfiguration:    o.cfg,
		sessionMap:               o.sessionMap,
		rabbitMQClient:           o.rabbitMQClient,
		producerManager:          o.producerManager,
		cacheManager:             o.cacheManager,
		feedMessageFactory:       o.feedMessageFactory,
		recoveryMessageProcessor: o.recoveryManager,
		logger:                   o.logger,
	}, nil
}

func (o *oddsFeedImpl) BookmakerDetails() (protocols.BookmakerDetail, error) {
	if err := o.init(); err != nil {
		return nil, err
	}

	return o.whoAmIManager.BookmakerDetails()
}

func (o *oddsFeedImpl) ProducerManager() (protocols.ProducerManager, error) {
	if err := o.init(); err != nil {
		return nil, err
	}

	return o.producerManager, nil
}

func (o *oddsFeedImpl) MarketDescriptionManager() (protocols.MarketDescriptionManager, error) {
	if err := o.init(); err != nil {
		return nil, err
	}

	return o.marketDescriptionManager, nil
}

func (o *oddsFeedImpl) SportsInfoManager() (protocols.SportsInfoManager, error) {
	if err := o.init(); err != nil {
		return nil, err
	}

	return o.sportsInfoManager, nil
}

func (o *oddsFeedImpl) RecoveryManager() (protocols.RecoveryManager, error) {
	if err := o.init(); err != nil {
		return nil, err
	}

	return o.recoveryManager, nil
}

func (o *oddsFeedImpl) ReplayManager() (protocols.ReplayManager, error) {
	if err := o.init(); err != nil {
		return nil, err
	}

	return o.replayManager, nil
}

func (o *oddsFeedImpl) Close() error {
	o.opened = false
	if o.recoveryManager != nil {
		o.recoveryManager.Close()
	}

	if o.apiClient != nil {
		o.apiClient.Close()
	}

	if o.sessionMap != nil {
		for _, value := range o.sessionMap {
			value.session.Close()
		}
	}

	if o.closeCh != nil {
		close(o.closeCh)
	}

	if o.cacheManager != nil {
		o.cacheManager.Close()
	}

	if o.rabbitMQClient != nil {
		o.rabbitMQClient.Close()
	}

	if o.msgCh != nil {
		close(o.msgCh)
	}

	return nil
}

func (o *oddsFeedImpl) Open() (protocols.GlobalMessageDelivery, error) {
	if o.opened {
		return nil, errors.New("already opened")
	}

	o.opened = true

	err := o.producerManager.Open()
	if err != nil {
		return nil, err
	}

	if o.sessionMap == nil || len(o.sessionMap) == 0 {
		return nil, errors.New("cannot open feed without sessions")
	}

	availableProducers, err := o.producerManager.AvailableProducers()
	if err != nil {
		return nil, err
	}
	requestedProducers := make(map[uint]struct{})
	for _, value := range o.sessionMap {
		producers := value.messageInterest.PossibleSourceProducers(availableProducers)
		for i := range producers {
			producerID := producers[i]
			requestedProducers[producerID] = struct{}{}
		}
	}

	for key := range availableProducers {
		_, ok := requestedProducers[key]
		if ok {
			continue
		}

		// Producer is not requested - disable
		err := o.producerManager.SetProducerState(key, false)
		if err != nil {
			return nil, err
		}
	}

	var hasReplay bool
	var hasAliveMessageInterest bool
	keyMap := make(map[uuid.UUID]keyData, len(o.sessionMap))
	for key, value := range o.sessionMap {
		keyMap[key] = keyData{
			messageInterest: *value.messageInterest,
			eventIDs:        value.eventIDs,
		}

		if value.session.IsReplay() {
			hasReplay = true
		}

		if *value.messageInterest == protocols.SystemAliveOnly {
			hasAliveMessageInterest = true
		}
	}

	sessionRoutingKeys, err := o.generateKeys(keyMap)
	if err != nil {
		return nil, err
	}

	replayOnly := hasReplay && len(o.sessionMap) == 1
	// Add system alive only interest if needed
	if !hasAliveMessageInterest && !replayOnly {
		session := newSession(
			o.rabbitMQClient,
			o.producerManager,
			o.cacheManager,
			o.feedMessageFactory,
			o.recoveryManager,
			o.cfg.ExchangeName(),
			false,
			o.logger,
		)
		sessionData := &sessionData{
			session:     session,
			isAliveOnly: true,
		}

		o.sessionMap[session.ID()] = sessionData
		go func() {
			// no-op message consumption
			for range session.RespCh() {
			}
		}()
	}

	err = o.rabbitMQClient.Open()
	if err != nil {
		return nil, err
	}

	for _, value := range o.sessionMap {
		var err error
		if value.isAliveOnly {
			messageInterest := protocols.SystemAliveOnly
			err = value.session.Open([]string{string(protocols.SystemAliveOnly)}, &messageInterest, false)
		} else {
			routingKeys := sessionRoutingKeys[value.session.ID()]
			err = value.session.Open(routingKeys, value.messageInterest, o.cfg.ReportExtendedData())
		}

		if err != nil {
			return nil, err
		}
	}

	recoveryCh, err := o.recoveryManager.Open()
	apiCh := o.apiClient.Open()

	o.msgCh = make(chan protocols.GlobalMessage, 0)
	o.closeCh = make(chan bool, 1)
	go func() {
		for {
			select {
			case recoveryMsg := <-recoveryCh:
				if !o.opened {
					return
				}

				o.msgCh <- protocols.GlobalMessage{
					Recovery: &recoveryMsg,
				}

			case <-o.closeCh:
				return
			}
		}
	}()

	go func() {
		for {
			select {
			case apiMsg := <-apiCh:
				if !o.opened {
					return
				}

				if o.cfg.ReportExtendedData() {
					o.msgCh <- protocols.GlobalMessage{
						APIMessage: &apiMsg,
					}
				}

			case <-o.closeCh:
				return
			}
		}
	}()

	return o.msgCh, nil
}

func (o *oddsFeedImpl) init() error {
	if o.feedInitialized {
		return nil
	}

	o.apiClient = api.New(o.cfg)

	o.whoAmIManager = whoami.NewManager(o.cfg, o.apiClient, o.logger)
	// Try to fetch bookmaker details
	_, err := o.whoAmIManager.BookmakerDetails()
	if err != nil {
		return err
	}

	o.producerManager = producer.NewManager(o.cfg, o.apiClient, o.logger)

	o.cacheManager = cache.NewManager(o.apiClient, o.cfg, o.logger)

	entityFactory := factory.NewEntityFactory(o.cacheManager)

	marketDescriptionFactory := factory.NewMarketDescriptionFactory(o.cacheManager.MarketDescriptionCache)
	marketDataFactory := factory.NewMarketDataFactory(o.cfg, marketDescriptionFactory)
	marketFactory := factory.NewMarketFactory(
		marketDataFactory,
		[]protocols.Locale{o.cfg.DefaultLocale()},
		o.logger,
	)
	o.feedMessageFactory = factory.NewFeedMessageFactory(
		entityFactory,
		marketFactory,
		o.producerManager,
		o.cfg,
	)

	o.recoveryManager = recovery.NewManager(
		o.cfg,
		o.producerManager,
		o.apiClient,
		o.logger,
	)
	o.marketDescriptionManager = market.NewManager(o.cacheManager, marketDescriptionFactory, o.cfg)
	o.sportsInfoManager = sport.NewManager(entityFactory, o.apiClient, o.cacheManager, o.cfg)
	o.replayManager = replay.NewManager(o.apiClient, o.cfg, o.sportsInfoManager)

	o.rabbitMQClient = feed.NewClient(o.cfg, o.whoAmIManager, o.logger)

	o.feedInitialized = true

	return nil
}

func (o *oddsFeedImpl) generateKeys(sessionsData map[uuid.UUID]keyData) (map[uuid.UUID][]string, error) {
	err := o.validateInterestCombination(sessionsData)
	if err != nil {
		return nil, err
	}
	var hasLowPriorityInterest bool
	var hasHighPriorityInterest bool
	for _, value := range sessionsData {
		if value.messageInterest == protocols.LowPriorityOnlyMessageInterest {
			hasLowPriorityInterest = true
		}

		if value.messageInterest == protocols.HiPriorityOnlyMessageInterest {
			hasHighPriorityInterest = true
		}
	}
	bothLowAndHigh := hasLowPriorityInterest && hasHighPriorityInterest

	var snapshotRoutingKey string
	if o.cfg.SdkNodeID() != nil {
		snapshotRoutingKey = fmt.Sprintf("%s%d", snapshotKeyTemplate, *o.cfg.SdkNodeID())
	} else {
		snapshotRoutingKey = fmt.Sprintf("%s%s", snapshotKeyTemplate, "-")
	}

	result := make(map[uuid.UUID][]string)
	for id, value := range sessionsData {
		sessionRoutingKeysMap := make(map[string]struct{})

		basicRoutingKeys := make([]string, 0)
		if value.messageInterest == protocols.SpecifiedMatchesOnlyMessageInterest {
			for urn := range value.eventIDs {
				basicRoutingKeys = append(basicRoutingKeys, fmt.Sprintf("#.%s:%s.%d", urn.Prefix, urn.Type, urn.ID))
			}
		} else {
			basicRoutingKeys = append(basicRoutingKeys, string(value.messageInterest))
		}

		for i := range basicRoutingKeys {
			basicRoutingKey := basicRoutingKeys[i]

			if o.cfg.SdkNodeID() != nil {
				sessionRoutingKeysMap[fmt.Sprintf("%s.%d.#", basicRoutingKey, *o.cfg.SdkNodeID())] = struct{}{}
				basicRoutingKey = fmt.Sprintf("%s.-.#", basicRoutingKey)
			} else {
				basicRoutingKey = fmt.Sprintf("%s.#", basicRoutingKey)
			}

			if bothLowAndHigh && value.messageInterest == protocols.LowPriorityOnlyMessageInterest {
				sessionRoutingKeysMap[basicRoutingKey] = struct{}{}
			} else {
				sessionRoutingKeysMap[snapshotRoutingKey] = struct{}{}
				sessionRoutingKeysMap[basicRoutingKey] = struct{}{}
			}
		}

		if value.messageInterest != protocols.SystemAliveOnly {
			sessionRoutingKeysMap[string(protocols.SystemAliveOnly)] = struct{}{}
		}

		sessionRoutingKeys := make([]string, 0)
		for key := range sessionRoutingKeysMap {
			sessionRoutingKeys = append(sessionRoutingKeys, key)
		}
		result[id] = sessionRoutingKeys
	}

	return result, nil
}

func (o *oddsFeedImpl) validateInterestCombination(sessionsData map[uuid.UUID]keyData) error {
	if len(sessionsData) <= 1 {
		return nil
	}

	userInterests := make(map[protocols.MessageInterest]struct{})
	var hasAll bool
	var hasPriority bool
	var hasMessages bool
	for _, value := range sessionsData {
		userInterests[value.messageInterest] = struct{}{}

		if value.messageInterest == protocols.AllMessageInterest {
			hasAll = true
		}

		if value.messageInterest == protocols.HiPriorityOnlyMessageInterest || value.messageInterest == protocols.LowPriorityOnlyMessageInterest {
			hasPriority = true
		}

		if value.messageInterest == protocols.PrematchOnlyMessageInterest || value.messageInterest == protocols.LiveOnlyMessageInterest {
			hasMessages = true
		}
	}

	switch {
	case len(userInterests) != len(sessionsData):
		return errors.New("found duplicate message interest")
	case hasAll:
		return errors.New("all messages can be used only for single session configuration")
	case hasPriority && hasMessages:
		return errors.New("cannot combine priority messages with other types")
	}

	return nil
}

// NewOddsFeed ...
func NewOddsFeed(configuration protocols.OddsFeedConfiguration) protocols.OddsFeed {
	return &oddsFeedImpl{
		cfg:        configuration,
		logger:     log.New(),
		sessionMap: make(map[uuid.UUID]*sessionData),
	}
}

type keyData struct {
	messageInterest protocols.MessageInterest
	eventIDs        map[protocols.URN]struct{}
}
