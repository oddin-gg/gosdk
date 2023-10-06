package factory

import (
	"time"

	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	"github.com/oddin-gg/gosdk/protocols"
	"github.com/pkg/errors"
)

// FeedMessageFactory ...
type FeedMessageFactory struct {
	entityFactory         *EntityFactory
	marketFactory         *MarketFactory
	producerManager       protocols.ProducerManager
	oddsFeedConfiguration protocols.OddsFeedConfiguration
}

// BuildMessage ...
func (f *FeedMessageFactory) BuildMessage(feedMessage *protocols.FeedMessage) (interface{}, error) {
	if feedMessage.Message == nil || feedMessage.RawMessage == nil {
		return nil, errors.New("message and raw message is required")
	}

	timestamp := feedMessage.Timestamp
	timestamp.Published = time.Now()

	var event interface{}
	switch protocols.EventType(feedMessage.RoutingKey.EventID.Type) {
	case protocols.TournamentEventType:
		event = f.entityFactory.BuildTournament(*feedMessage.RoutingKey.EventID, *feedMessage.RoutingKey.SportID, []protocols.Locale{f.oddsFeedConfiguration.DefaultLocale()})
	case protocols.MatchEventType:
		event = f.entityFactory.BuildMatch(*feedMessage.RoutingKey.EventID, []protocols.Locale{f.oddsFeedConfiguration.DefaultLocale()}, feedMessage.RoutingKey.SportID)
	}

	producer, err := f.producerManager.GetProducer(feedMessage.Message.Product())
	if err != nil {
		return nil, err
	}

	switch msg := feedMessage.Message.(type) {
	case *feedXML.OddsChange:
		return oddsChangeImpl{
			producer:      producer,
			timestamp:     timestamp,
			rawMessage:    feedMessage.RawMessage,
			message:       msg,
			event:         event,
			marketFactory: f.marketFactory,
		}, nil
	case *feedXML.BetStop:
		return betStopImpl{
			producer:   producer,
			timestamp:  timestamp,
			requestID:  msg.RequestID,
			rawMessage: feedMessage.RawMessage,
			event:      event,
		}, nil
	case *feedXML.BetSettlement:
		return betSettlementImpl{
			producer:      producer,
			timestamp:     timestamp,
			rawMessage:    feedMessage.RawMessage,
			message:       msg,
			event:         event,
			marketFactory: f.marketFactory,
		}, nil
	case *feedXML.BetCancel:
		return betCancelImpl{
			producer:      producer,
			timestamp:     timestamp,
			rawMessage:    feedMessage.RawMessage,
			message:       msg,
			event:         event,
			marketFactory: f.marketFactory,
		}, nil
	case *feedXML.FixtureChange:
		return fixtureChangeImpl{
			producer:   producer,
			timestamp:  timestamp,
			rawMessage: feedMessage.RawMessage,
			message:    msg,
			event:      event,
		}, nil
	case *feedXML.RollbackBetSettlement:
		return rollbackBetSettlementImpl{
			producer:      producer,
			timestamp:     timestamp,
			rawMessage:    feedMessage.RawMessage,
			message:       msg,
			event:         event,
			marketFactory: f.marketFactory,
		}, nil
	case *feedXML.RollbackBetCancel:
		return rollbackBetCancelImpl{
			producer:      producer,
			timestamp:     timestamp,
			rawMessage:    feedMessage.RawMessage,
			message:       msg,
			event:         event,
			marketFactory: f.marketFactory,
		}, nil
	default:
		return nil, errors.Errorf("unknown message type %s", msg)
	}
}

// BuildUnparsableMessage ...
func (f *FeedMessageFactory) BuildUnparsableMessage(feedMessage *protocols.FeedMessage) protocols.UnparsableMessage {
	timestamp := feedMessage.Timestamp
	timestamp.Published = time.Now()

	var event interface{}
	switch protocols.EventType(feedMessage.RoutingKey.EventID.Type) {
	case protocols.TournamentEventType:
		event = f.entityFactory.BuildTournament(*feedMessage.RoutingKey.EventID, *feedMessage.RoutingKey.SportID, []protocols.Locale{f.oddsFeedConfiguration.DefaultLocale()})
	case protocols.MatchEventType:
		event = f.entityFactory.BuildMatch(*feedMessage.RoutingKey.EventID, []protocols.Locale{f.oddsFeedConfiguration.DefaultLocale()}, feedMessage.RoutingKey.SportID)
	}

	return unparsableMessageImpl{
		event:      event,
		timestamp:  protocols.MessageTimestamp{},
		rawMessage: feedMessage.RawMessage,
	}
}

// BuildProducerStatus ...
func (f *FeedMessageFactory) BuildProducerStatus(producerID uint, producerStatusReason protocols.ProducerStatusReason, isDown bool, isDelayed bool, timestamp time.Time) (protocols.ProducerStatus, error) {
	producer, err := f.producerManager.GetProducer(producerID)
	if err != nil {
		return nil, err
	}

	return producerStatusImpl{
		producer: producer,
		timestamp: protocols.MessageTimestamp{
			Created:   timestamp,
			Sent:      timestamp,
			Received:  timestamp,
			Published: timestamp,
		},
		isDown:               isDown,
		isDelayed:            isDelayed,
		producerStatusReason: producerStatusReason,
	}, nil
}

// NewFeedMessageFactory ...
func NewFeedMessageFactory(entityFactory *EntityFactory, marketFactory *MarketFactory, producerManager protocols.ProducerManager, oddsFeedConfiguration protocols.OddsFeedConfiguration) *FeedMessageFactory {
	return &FeedMessageFactory{
		entityFactory:         entityFactory,
		marketFactory:         marketFactory,
		producerManager:       producerManager,
		oddsFeedConfiguration: oddsFeedConfiguration,
	}
}

type producerStatusImpl struct {
	producer             protocols.Producer
	timestamp            protocols.MessageTimestamp
	isDown               bool
	isDelayed            bool
	producerStatusReason protocols.ProducerStatusReason
}

func (p producerStatusImpl) Producer() protocols.Producer {
	return p.producer
}

func (p producerStatusImpl) Timestamp() protocols.MessageTimestamp {
	return p.timestamp
}

func (p producerStatusImpl) IsDown() bool {
	return p.isDown
}

func (p producerStatusImpl) IsDelayed() bool {
	return p.isDelayed
}

func (p producerStatusImpl) ProducerStatusReason() protocols.ProducerStatusReason {
	return p.producerStatusReason
}

type unparsableMessageImpl struct {
	event      interface{}
	producer   protocols.Producer
	timestamp  protocols.MessageTimestamp
	rawMessage []byte
}

func (u unparsableMessageImpl) Event() interface{} {
	return u.event
}

func (u unparsableMessageImpl) Producer() protocols.Producer {
	return u.producer
}

func (u unparsableMessageImpl) Timestamp() protocols.MessageTimestamp {
	return u.timestamp
}

func (u unparsableMessageImpl) RawMessage() []byte {
	return u.rawMessage
}

type oddsChangeImpl struct {
	producer      protocols.Producer
	timestamp     protocols.MessageTimestamp
	rawMessage    []byte
	message       *feedXML.OddsChange
	event         interface{}
	marketFactory *MarketFactory
	markets       []protocols.MarketWithOdds
}

func (m oddsChangeImpl) Producer() protocols.Producer {
	return m.producer
}

func (m oddsChangeImpl) Timestamp() protocols.MessageTimestamp {
	return m.timestamp
}

func (m oddsChangeImpl) RequestID() *uint {
	return m.message.RequestID
}

func (m oddsChangeImpl) RawMessage() []byte {
	return m.rawMessage
}

func (m oddsChangeImpl) Event() interface{} {
	return m.event
}

func (m oddsChangeImpl) Markets() []protocols.MarketWithOdds {
	if m.markets == nil {
		m.markets = make([]protocols.MarketWithOdds, len(m.message.Odds.Markets))
		for i := range m.message.Odds.Markets {
			market := m.message.Odds.Markets[i]
			m.markets[i] = m.marketFactory.BuildMarketWithOdds(m.event, market)
		}
	}

	return m.markets
}

type betStopImpl struct {
	producer   protocols.Producer
	timestamp  protocols.MessageTimestamp
	requestID  *uint
	rawMessage []byte
	event      interface{}
}

func (b betStopImpl) Producer() protocols.Producer {
	return b.producer
}

func (b betStopImpl) Timestamp() protocols.MessageTimestamp {
	return b.timestamp
}

func (b betStopImpl) RequestID() *uint {
	return b.requestID
}

func (b betStopImpl) RawMessage() []byte {
	return b.rawMessage
}

func (b betStopImpl) Event() interface{} {
	return b.event
}

type betSettlementImpl struct {
	producer      protocols.Producer
	timestamp     protocols.MessageTimestamp
	rawMessage    []byte
	message       *feedXML.BetSettlement
	event         interface{}
	marketFactory *MarketFactory
	markets       []protocols.MarketWithSettlement
}

func (m betSettlementImpl) Producer() protocols.Producer {
	return m.producer
}

func (m betSettlementImpl) Timestamp() protocols.MessageTimestamp {
	return m.timestamp
}

func (m betSettlementImpl) RequestID() *uint {
	return m.message.RequestID
}

func (m betSettlementImpl) RawMessage() []byte {
	return m.rawMessage
}

func (m betSettlementImpl) Event() interface{} {
	return m.event
}

func (m betSettlementImpl) Markets() []protocols.MarketWithSettlement {
	if m.markets == nil {
		m.markets = make([]protocols.MarketWithSettlement, len(m.message.Markets.Markets))
		for i := range m.message.Markets.Markets {
			market := m.message.Markets.Markets[i]
			m.markets[i] = m.marketFactory.BuildMarketWithSettlement(m.event, market)
		}
	}

	return m.markets
}

type betCancelImpl struct {
	producer      protocols.Producer
	timestamp     protocols.MessageTimestamp
	rawMessage    []byte
	message       *feedXML.BetCancel
	event         interface{}
	marketFactory *MarketFactory
	markets       []protocols.MarketCancel
}

func (m betCancelImpl) Producer() protocols.Producer {
	return m.producer
}

func (m betCancelImpl) Timestamp() protocols.MessageTimestamp {
	return m.timestamp
}

func (m betCancelImpl) RequestID() *uint {
	return m.message.RequestID
}

func (m betCancelImpl) RawMessage() []byte {
	return m.rawMessage
}

func (m betCancelImpl) Event() interface{} {
	return m.event
}

func (m betCancelImpl) Markets() []protocols.MarketCancel {
	if m.markets == nil {
		m.markets = make([]protocols.MarketCancel, len(m.message.Markets))
		for i := range m.message.Markets {
			market := m.message.Markets[i]
			m.markets[i] = m.marketFactory.BuildMarketCancel(m.event, market)
		}
	}

	return m.markets
}

type fixtureChangeImpl struct {
	producer   protocols.Producer
	timestamp  protocols.MessageTimestamp
	rawMessage []byte
	message    *feedXML.FixtureChange
	event      interface{}
}

func (f fixtureChangeImpl) Producer() protocols.Producer {
	return f.producer
}

func (f fixtureChangeImpl) Timestamp() protocols.MessageTimestamp {
	return f.timestamp
}

func (f fixtureChangeImpl) RequestID() *uint {
	return f.message.RequestID
}

func (f fixtureChangeImpl) RawMessage() []byte {
	return f.rawMessage
}

func (f fixtureChangeImpl) Event() interface{} {
	return f.event
}

func (f fixtureChangeImpl) ChangeType() protocols.FixtureChangeType {
	switch f.message.ChangeType {
	case feedXML.FixtureChangeTypeNew:
		return protocols.NewFixtureChangeType
	case feedXML.FixtureChangeTypeDateTime:
		return protocols.TimeUpdateChangeType
	case feedXML.FixtureChangeTypeCancelled:
		return protocols.CancelledFixtureChangeType
	case feedXML.FixtureChangeTypeCoverage:
		return protocols.CoverageFixtureChangeType
	case feedXML.FixtureChangeTypeStreamURL:
		return protocols.StreamURLFixtureChangeType
	default:
		return protocols.UnknownFixtureChangeType
	}
}

type rollbackBetSettlementImpl struct {
	producer      protocols.Producer
	timestamp     protocols.MessageTimestamp
	rawMessage    []byte
	message       *feedXML.RollbackBetSettlement
	event         interface{}
	marketFactory *MarketFactory
	markets       []protocols.Market
}

func (m rollbackBetSettlementImpl) Producer() protocols.Producer {
	return m.producer
}

func (m rollbackBetSettlementImpl) Timestamp() protocols.MessageTimestamp {
	return m.timestamp
}

func (m rollbackBetSettlementImpl) RequestID() *uint {
	return m.message.RequestID
}

func (m rollbackBetSettlementImpl) RawMessage() []byte {
	return m.rawMessage
}

func (m rollbackBetSettlementImpl) Event() interface{} {
	return m.event
}

func (m rollbackBetSettlementImpl) RolledBackSettledMarkets() []protocols.Market {
	if m.markets == nil {
		m.markets = make([]protocols.Market, len(m.message.Markets))
		for i, market := range m.message.Markets {
			m.markets[i] = m.marketFactory.BuildMarket(m.event, &market.MarketAttributes)
		}
	}

	return m.markets
}

type rollbackBetCancelImpl struct {
	producer      protocols.Producer
	timestamp     protocols.MessageTimestamp
	rawMessage    []byte
	message       *feedXML.RollbackBetCancel
	event         interface{}
	marketFactory *MarketFactory
	markets       []protocols.Market
}

func (m rollbackBetCancelImpl) Producer() protocols.Producer {
	return m.producer
}

func (m rollbackBetCancelImpl) Timestamp() protocols.MessageTimestamp {
	return m.timestamp
}

func (m rollbackBetCancelImpl) RequestID() *uint {
	return m.message.RequestID
}

func (m rollbackBetCancelImpl) RawMessage() []byte {
	return m.rawMessage
}

func (m rollbackBetCancelImpl) Event() interface{} {
	return m.event
}

func (m rollbackBetCancelImpl) RolledBackCanceledMarkets() []protocols.Market {
	if m.markets == nil {
		m.markets = make([]protocols.Market, len(m.message.Markets))
		for i, market := range m.message.Markets {
			m.markets[i] = m.marketFactory.BuildMarket(m.event, &market.MarketAttributes)
		}
	}

	return m.markets
}
