package factory

import (
	"context"
	"errors"
	"fmt"
	"time"

	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	"github.com/oddin-gg/gosdk/internal/producer"
	"github.com/oddin-gg/gosdk/types"
)

// FeedMessageFactory ...
//
// producerManager is the concrete *producer.Manager (not the
// types.ProducerManager interface) so this hot-path code can do a
// pure-cache lookup via producerCached — avoiding hidden HTTP calls
// from inside AMQP message processing.
type FeedMessageFactory struct {
	entityFactory         *EntityFactory
	marketFactory         *MarketFactory
	producerManager       *producer.Manager
	oddsFeedConfiguration types.OddsFeedConfiguration
}

// BuildMessage ...
func (f *FeedMessageFactory) BuildMessage(feedMessage *types.FeedMessage) (interface{}, error) {
	if feedMessage.Message == nil || feedMessage.RawMessage == nil {
		return nil, errors.New("message and raw message is required")
	}

	timestamp := feedMessage.Timestamp
	timestamp.Published = time.Now()

	var event interface{}
	switch types.EventType(feedMessage.RoutingKey.EventID.Type) {
	case types.TournamentEventType:
		// Hot path: AMQP message decode. Uses context.Background() because
		// BuildMessage doesn't carry a caller ctx today; the data is
		// expected to be cached after the first encounter.
		t, err := f.entityFactory.BuildTournament(context.Background(), *feedMessage.RoutingKey.EventID, *feedMessage.RoutingKey.SportID, []types.Locale{f.oddsFeedConfiguration.DefaultLocale()})
		if err == nil && t != nil {
			event = *t
		}
	case types.MatchEventType:
		match, err := f.entityFactory.BuildMatch(context.Background(), *feedMessage.RoutingKey.EventID, []types.Locale{f.oddsFeedConfiguration.DefaultLocale()}, feedMessage.RoutingKey.SportID)
		if err == nil && match != nil {
			event = *match
		}
	}

	producer, err := f.producerManager.GetProducerCached(feedMessage.Message.Product())
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
		return nil, fmt.Errorf("unknown message type %s", msg)
	}
}

// BuildUnparsableMessage ...
func (f *FeedMessageFactory) BuildUnparsableMessage(feedMessage *types.FeedMessage) types.UnparsableMessage {
	timestamp := feedMessage.Timestamp
	timestamp.Published = time.Now()

	var event interface{}
	switch types.EventType(feedMessage.RoutingKey.EventID.Type) {
	case types.TournamentEventType:
		// Hot path: AMQP message decode. Uses context.Background() because
		// BuildMessage doesn't carry a caller ctx today; the data is
		// expected to be cached after the first encounter.
		t, err := f.entityFactory.BuildTournament(context.Background(), *feedMessage.RoutingKey.EventID, *feedMessage.RoutingKey.SportID, []types.Locale{f.oddsFeedConfiguration.DefaultLocale()})
		if err == nil && t != nil {
			event = *t
		}
	case types.MatchEventType:
		match, err := f.entityFactory.BuildMatch(context.Background(), *feedMessage.RoutingKey.EventID, []types.Locale{f.oddsFeedConfiguration.DefaultLocale()}, feedMessage.RoutingKey.SportID)
		if err == nil && match != nil {
			event = *match
		}
	}

	return unparsableMessageImpl{
		event:      event,
		timestamp:  types.MessageTimestamp{},
		rawMessage: feedMessage.RawMessage,
	}
}

// BuildProducerStatus ...
func (f *FeedMessageFactory) BuildProducerStatus(producerID uint, producerStatusReason types.ProducerStatusReason, isDown bool, isDelayed bool, timestamp time.Time) (types.ProducerStatus, error) {
	producer, err := f.producerManager.GetProducerCached(producerID)
	if err != nil {
		return nil, err
	}

	return producerStatusImpl{
		producer: producer,
		timestamp: types.MessageTimestamp{
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
func NewFeedMessageFactory(entityFactory *EntityFactory, marketFactory *MarketFactory, producerManager *producer.Manager, oddsFeedConfiguration types.OddsFeedConfiguration) *FeedMessageFactory {
	return &FeedMessageFactory{
		entityFactory:         entityFactory,
		marketFactory:         marketFactory,
		producerManager:       producerManager,
		oddsFeedConfiguration: oddsFeedConfiguration,
	}
}

type producerStatusImpl struct {
	producer             types.Producer
	timestamp            types.MessageTimestamp
	isDown               bool
	isDelayed            bool
	producerStatusReason types.ProducerStatusReason
}

func (p producerStatusImpl) Producer() types.Producer {
	return p.producer
}

func (p producerStatusImpl) Timestamp() types.MessageTimestamp {
	return p.timestamp
}

func (p producerStatusImpl) IsDown() bool {
	return p.isDown
}

func (p producerStatusImpl) IsDelayed() bool {
	return p.isDelayed
}

func (p producerStatusImpl) ProducerStatusReason() types.ProducerStatusReason {
	return p.producerStatusReason
}

type unparsableMessageImpl struct {
	event      interface{}
	producer   types.Producer
	timestamp  types.MessageTimestamp
	rawMessage []byte
}

func (u unparsableMessageImpl) Event() interface{} {
	return u.event
}

func (u unparsableMessageImpl) Producer() types.Producer {
	return u.producer
}

func (u unparsableMessageImpl) Timestamp() types.MessageTimestamp {
	return u.timestamp
}

func (u unparsableMessageImpl) RawMessage() []byte {
	return u.rawMessage
}

type oddsChangeImpl struct {
	producer      types.Producer
	timestamp     types.MessageTimestamp
	rawMessage    []byte
	message       *feedXML.OddsChange
	event         interface{}
	marketFactory *MarketFactory
	markets       []types.MarketWithOdds
}

func (m oddsChangeImpl) Producer() types.Producer {
	return m.producer
}

func (m oddsChangeImpl) Timestamp() types.MessageTimestamp {
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

func (m oddsChangeImpl) Markets() []types.MarketWithOdds {
	if m.markets == nil {
		m.markets = make([]types.MarketWithOdds, len(m.message.Odds.Markets))
		for i := range m.message.Odds.Markets {
			market := m.message.Odds.Markets[i]
			m.markets[i] = m.marketFactory.BuildMarketWithOdds(m.event, market)
		}
	}

	return m.markets
}

type betStopImpl struct {
	producer   types.Producer
	timestamp  types.MessageTimestamp
	requestID  *uint
	rawMessage []byte
	event      interface{}
}

func (b betStopImpl) Producer() types.Producer {
	return b.producer
}

func (b betStopImpl) Timestamp() types.MessageTimestamp {
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
	producer      types.Producer
	timestamp     types.MessageTimestamp
	rawMessage    []byte
	message       *feedXML.BetSettlement
	event         interface{}
	marketFactory *MarketFactory
	markets       []types.MarketWithSettlement
}

func (m betSettlementImpl) Producer() types.Producer {
	return m.producer
}

func (m betSettlementImpl) Timestamp() types.MessageTimestamp {
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

func (m betSettlementImpl) Markets() []types.MarketWithSettlement {
	if m.markets == nil {
		m.markets = make([]types.MarketWithSettlement, len(m.message.Markets.Markets))
		for i := range m.message.Markets.Markets {
			market := m.message.Markets.Markets[i]
			m.markets[i] = m.marketFactory.BuildMarketWithSettlement(m.event, market)
		}
	}

	return m.markets
}

type betCancelImpl struct {
	producer      types.Producer
	timestamp     types.MessageTimestamp
	rawMessage    []byte
	message       *feedXML.BetCancel
	event         interface{}
	marketFactory *MarketFactory
	markets       []types.MarketCancel
}

func (m betCancelImpl) Producer() types.Producer {
	return m.producer
}

func (m betCancelImpl) Timestamp() types.MessageTimestamp {
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

func (m betCancelImpl) Markets() []types.MarketCancel {
	if m.markets == nil {
		m.markets = make([]types.MarketCancel, len(m.message.Markets))
		for i := range m.message.Markets {
			market := m.message.Markets[i]
			m.markets[i] = m.marketFactory.BuildMarketCancel(m.event, market)
		}
	}

	return m.markets
}

func (m betCancelImpl) StartTime() *time.Time {
	if m.message.StartTime == nil {
		return nil
	}
	startTime := time.Unix(int64(*m.message.StartTime), 0)
	return &startTime
}

func (m betCancelImpl) EndTime() *time.Time {
	if m.message.EndTime == nil {
		return nil
	}
	endTime := time.Unix(int64(*m.message.EndTime), 0)
	return &endTime
}

type fixtureChangeImpl struct {
	producer   types.Producer
	timestamp  types.MessageTimestamp
	rawMessage []byte
	message    *feedXML.FixtureChange
	event      interface{}
}

func (f fixtureChangeImpl) Producer() types.Producer {
	return f.producer
}

func (f fixtureChangeImpl) Timestamp() types.MessageTimestamp {
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

func (f fixtureChangeImpl) ChangeType() types.FixtureChangeType {
	switch f.message.ChangeType {
	case feedXML.FixtureChangeTypeNew:
		return types.NewFixtureChangeType
	case feedXML.FixtureChangeTypeDateTime:
		return types.TimeUpdateChangeType
	case feedXML.FixtureChangeTypeCancelled:
		return types.CancelledFixtureChangeType
	case feedXML.FixtureChangeTypeCoverage:
		return types.CoverageFixtureChangeType
	case feedXML.FixtureChangeTypeStreamURL:
		return types.StreamURLFixtureChangeType
	default:
		return types.UnknownFixtureChangeType
	}
}

type rollbackBetSettlementImpl struct {
	producer      types.Producer
	timestamp     types.MessageTimestamp
	rawMessage    []byte
	message       *feedXML.RollbackBetSettlement
	event         interface{}
	marketFactory *MarketFactory
	markets       []types.Market
}

func (m rollbackBetSettlementImpl) Producer() types.Producer {
	return m.producer
}

func (m rollbackBetSettlementImpl) Timestamp() types.MessageTimestamp {
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

func (m rollbackBetSettlementImpl) RolledBackSettledMarkets() []types.Market {
	if m.markets == nil {
		m.markets = make([]types.Market, len(m.message.Markets))
		for i, market := range m.message.Markets {
			m.markets[i] = m.marketFactory.BuildMarket(m.event, &market.MarketAttributes)
		}
	}

	return m.markets
}

type rollbackBetCancelImpl struct {
	producer      types.Producer
	timestamp     types.MessageTimestamp
	rawMessage    []byte
	message       *feedXML.RollbackBetCancel
	event         interface{}
	marketFactory *MarketFactory
	markets       []types.Market
}

func (m rollbackBetCancelImpl) Producer() types.Producer {
	return m.producer
}

func (m rollbackBetCancelImpl) Timestamp() types.MessageTimestamp {
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

func (m rollbackBetCancelImpl) RolledBackCanceledMarkets() []types.Market {
	if m.markets == nil {
		m.markets = make([]types.Market, len(m.message.Markets))
		for i, market := range m.message.Markets {
			m.markets[i] = m.marketFactory.BuildMarket(m.event, &market.MarketAttributes)
		}
	}

	return m.markets
}

func (m rollbackBetCancelImpl) StartTime() *time.Time {
	if m.message.StartTime == nil {
		return nil
	}
	startTime := time.Unix(int64(*m.message.StartTime), 0)
	return &startTime
}

func (m rollbackBetCancelImpl) EndTime() *time.Time {
	if m.message.EndTime == nil {
		return nil
	}
	endTime := time.Unix(int64(*m.message.EndTime), 0)
	return &endTime
}
