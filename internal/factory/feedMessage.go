package factory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/oddin-gg/gosdk/internal/config"
	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/internal/producer"
	"github.com/oddin-gg/gosdk/types"
)

// cloneBytes returns a copy of b so callers of RawMessage() can't
// mutate the SDK's backing buffer (a shared decode artefact retained
// for diagnostic / replay-capture use). RawMessage is a debug API,
// not the message hot path — the per-message allocation is a
// price worth paying for a tamper-proof contract.
func cloneBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return bytes.Clone(b)
}

// FeedMessageFactory ...
//
// producerManager is the concrete *producer.Manager so this
// hot-path code can do a pure-cache lookup via producerCached —
// avoiding hidden HTTP calls from inside AMQP message processing.
type FeedMessageFactory struct {
	entityFactory         *EntityFactory
	marketFactory         *MarketFactory
	producerManager       *producer.Manager
	oddsFeedConfiguration config.Config
	logger                *log.Logger
}

// buildLocales returns the locale set message events are built with:
// the default plus every preload locale — the same list the market
// factory resolves market/outcome names for. Building the event with
// only the default locale starved marketData's home/away substitution:
// match.HomeCompetitor.Name(preloadLocale) had no entry, so the
// substituted market/outcome name for every preloaded non-default
// locale was silently bogus (stored as Some("")) — now the competitor
// names are populated for the full set (entity loads are cached, so
// the extra locales amortize to one fetch per entity per locale).
func (f *FeedMessageFactory) buildLocales() []types.Locale {
	if f.marketFactory != nil && len(f.marketFactory.locales) > 0 {
		return f.marketFactory.locales
	}
	return []types.Locale{f.oddsFeedConfiguration.DefaultLocale()}
}

// BuildMessage ...
func (f *FeedMessageFactory) BuildMessage(ctx context.Context, feedMessage *types.FeedMessage) (interface{}, error) {
	if feedMessage.Message == nil || feedMessage.RawMessage == nil {
		return nil, errors.New("message and raw message is required")
	}

	timestamp := feedMessage.Timestamp
	timestamp.Published = time.Now()

	// Routing-key sanity. BuildMessage handles parsed feed messages
	// (OddsChange, BetStop, BetSettlement, ...) that always address
	// an event in their routing key. Alive / SnapshotComplete carry
	// IsSystemRoutingKey == true and are routed elsewhere — they
	// never reach BuildMessage.
	//
	// parseRoute can produce a non-system RoutingKeyInfo with
	// EventID == nil for malformed 8-part routes (sportID set, but
	// eventType / eventID empty). For an event-addressed message
	// this is data corruption: returning an error lets the session
	// convert it to UnparsableMessage instead of silently dispatching
	// with event == nil (and crashing downstream consumers that rely
	// on the event being populated).
	if feedMessage.RoutingKey == nil {
		return nil, errors.New("feed message: nil routing key")
	}
	rk := feedMessage.RoutingKey
	if rk.EventID == nil {
		return nil, fmt.Errorf("feed message: route %q missing event id", rk.FullRoutingKey)
	}

	var event interface{}
	switch types.EventType(rk.EventID.Type) {
	case types.TournamentEventType:
		if rk.SportID == nil {
			return nil, fmt.Errorf("feed message: tournament route %q missing sport id", rk.FullRoutingKey)
		}
		t, err := f.entityFactory.BuildTournament(ctx, *rk.EventID, *rk.SportID, f.buildLocales())
		switch {
		case err != nil && f.logger != nil:
			// Transient cache/API failures during AMQP decode used to
			// be silently dropped — consumers saw a non-nil message
			// with event==nil and no log entry, indistinguishable
			// from "unsupported event type". Log so the failure is
			// observable; behaviour (dispatch with event==nil)
			// preserved for parity with Java/.NET.
			f.logger.WithError(err).Warnf("feed message: build tournament %s failed for route %q", rk.EventID.ToString(), rk.FullRoutingKey)
		case t != nil:
			event = *t
		}
	case types.MatchEventType:
		match, err := f.entityFactory.BuildMatch(ctx, *rk.EventID, f.buildLocales(), rk.SportID)
		switch {
		case err != nil && f.logger != nil:
			f.logger.WithError(err).Warnf("feed message: build match %s failed for route %q", rk.EventID.ToString(), rk.FullRoutingKey)
		case match != nil:
			event = *match
		}
	default:
		// PlayerEventType + unknown — leave event=nil; downstream
		// message-build callers tolerate that (they don't deref event
		// for non-Match/Tournament types).
	}

	producer, err := f.producerManager.GetProducerCached(feedMessage.Message.Product())
	if err != nil {
		return nil, err
	}

	switch msg := feedMessage.Message.(type) {
	case *feedXML.OddsChange:
		markets := make([]types.MarketWithOdds, len(msg.Odds.Markets))
		for i := range msg.Odds.Markets {
			markets[i] = f.marketFactory.BuildMarketWithOdds(ctx, event, msg.Odds.Markets[i])
		}
		return oddsChangeImpl{
			producer:   producer,
			timestamp:  timestamp,
			rawMessage: feedMessage.RawMessage,
			message:    msg,
			event:      event,
			markets:    markets,
		}, nil
	case *feedXML.BetStop:
		return betStopImpl{
			producer:   producer,
			timestamp:  timestamp,
			requestID:  types.FromPtr(msg.RequestID),
			rawMessage: feedMessage.RawMessage,
			event:      event,
		}, nil
	case *feedXML.BetSettlement:
		markets := make([]types.MarketWithSettlement, len(msg.Markets.Markets))
		for i := range msg.Markets.Markets {
			markets[i] = f.marketFactory.BuildMarketWithSettlement(ctx, event, msg.Markets.Markets[i])
		}
		return betSettlementImpl{
			producer:   producer,
			timestamp:  timestamp,
			rawMessage: feedMessage.RawMessage,
			message:    msg,
			event:      event,
			markets:    markets,
		}, nil
	case *feedXML.BetCancel:
		markets := make([]types.MarketCancel, len(msg.Markets))
		for i := range msg.Markets {
			markets[i] = f.marketFactory.BuildMarketCancel(ctx, event, msg.Markets[i])
		}
		return betCancelImpl{
			producer:   producer,
			timestamp:  timestamp,
			rawMessage: feedMessage.RawMessage,
			message:    msg,
			event:      event,
			markets:    markets,
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
		markets := make([]types.Market, len(msg.Markets))
		for i := range msg.Markets {
			markets[i] = f.marketFactory.BuildMarket(ctx, event, &msg.Markets[i].MarketAttributes)
		}
		return rollbackBetSettlementImpl{
			producer:   producer,
			timestamp:  timestamp,
			rawMessage: feedMessage.RawMessage,
			message:    msg,
			event:      event,
			markets:    markets,
		}, nil
	case *feedXML.RollbackBetCancel:
		markets := make([]types.Market, len(msg.Markets))
		for i := range msg.Markets {
			markets[i] = f.marketFactory.BuildMarket(ctx, event, &msg.Markets[i].MarketAttributes)
		}
		return rollbackBetCancelImpl{
			producer:   producer,
			timestamp:  timestamp,
			rawMessage: feedMessage.RawMessage,
			message:    msg,
			event:      event,
			markets:    markets,
		}, nil
	default:
		return nil, fmt.Errorf("unknown message type %s", msg)
	}
}

// BuildUnparsableMessage constructs the public UnparsableMessage from
// a partially-parsed FeedMessage envelope. Three pre-existing defects
// fixed here:
//
//  1. Routing-key fields can be nil. System routing keys (alive,
//     snapshot_complete, etc.) leave EventID and SportID nil because
//     they don't address an event. The previous implementation
//     unconditionally dereferenced both, so a malformed system message
//     hitting the unparsable path panicked the consumer goroutine.
//     Fixed: nil-check before dereferencing; leave event=nil otherwise.
//
//  2. The function computed `timestamp.Published = time.Now()` then
//     returned `timestamp: types.MessageTimestamp{}` — the empty
//     value, discarding both Published and the upstream Created/Sent/
//     Received from processDelivery. Fixed: return the computed
//     timestamp.
//
//  3. The `producer` field on unparsableMessageImpl was never
//     initialized — UnparsableMessage embeds types.Message which
//     requires Producer(), so any consumer calling
//     `unparsable.Producer().IsDown()` nil-deref'd. Fixed: look up
//     the producer (cache-only) the same way every other Build*
//     method does. A producer-lookup failure is downgraded to a
//     warn-and-continue: the unparsable message is still useful
//     to the consumer (raw bytes + timestamp + event) even when
//     the producer catalog is briefly unavailable.
func (f *FeedMessageFactory) BuildUnparsableMessage(ctx context.Context, feedMessage *types.FeedMessage) types.UnparsableMessage {
	timestamp := feedMessage.Timestamp
	timestamp.Published = time.Now()

	var event interface{}
	if rk := feedMessage.RoutingKey; rk != nil && rk.EventID != nil {
		switch types.EventType(rk.EventID.Type) {
		case types.TournamentEventType:
			if rk.SportID != nil {
				t, err := f.entityFactory.BuildTournament(ctx, *rk.EventID, *rk.SportID, []types.Locale{f.oddsFeedConfiguration.DefaultLocale()})
				switch {
				case err != nil && f.logger != nil:
					f.logger.WithError(err).Warnf("unparsable: build tournament %s failed for route %q", rk.EventID.ToString(), rk.FullRoutingKey)
				case t != nil:
					event = *t
				}
			}
		case types.MatchEventType:
			match, err := f.entityFactory.BuildMatch(ctx, *rk.EventID, []types.Locale{f.oddsFeedConfiguration.DefaultLocale()}, rk.SportID)
			switch {
			case err != nil && f.logger != nil:
				f.logger.WithError(err).Warnf("unparsable: build match %s failed for route %q", rk.EventID.ToString(), rk.FullRoutingKey)
			case match != nil:
				event = *match
			}
		default:
			// PlayerEventType + unknown — leave event=nil; downstream handles it.
		}
	}
	// rk == nil OR rk.EventID == nil (system routing key): event stays nil.

	// Producer lookup: cache-only (no HTTP) — same path BuildMessage uses.
	// feedMessage.Message can be nil for some unparsable origins (e.g. a
	// completely undecodable XML body); guard before .Product().
	var prod types.Producer
	if feedMessage.Message != nil {
		p, err := f.producerManager.GetProducerCached(feedMessage.Message.Product())
		if err != nil {
			if f.logger != nil {
				f.logger.WithError(err).WithField("producer_id", feedMessage.Message.Product()).Warn("unparsable: producer lookup failed; using placeholder producer")
			}
			// Diagnostics path: consumers interrogate
			// unparsable.Producer() (IsDown etc.) and nil-deref'd when
			// it was absent (the C1 finding). A placeholder is correct
			// HERE — the message is already unparsable, nothing routes
			// on the producer — unlike the strict BuildMessage path.
			if placeholder, perr := f.producerManager.UnknownProducerPlaceholder(feedMessage.Message.Product()); perr == nil {
				prod = placeholder
			}
		} else {
			prod = p
		}
	}
	if prod == nil && f.producerManager != nil {
		// Fully-undecodable bodies (Message == nil) and failed
		// placeholder construction must STILL satisfy the non-optional
		// Message.Producer() contract the UnparsableMessage interface
		// embeds — under the default catch strategy consumers receive
		// this value, and u.Producer().ID() or
		// types.ProducerHasScope(u.Producer(), …) panicked on nil.
		// Product id 0 is the unknown-producer sentinel (name
		// "unknown"; permissive both-scopes shape, same as the failed
		// lookup placeholder above).
		if placeholder, perr := f.producerManager.UnknownProducerPlaceholder(0); perr == nil {
			prod = placeholder
		}
	}

	return unparsableMessageImpl{
		event:      event,
		producer:   prod,
		timestamp:  timestamp,
		rawMessage: feedMessage.RawMessage,
	}
}

// BuildProducerStatus ...
func (f *FeedMessageFactory) BuildProducerStatus(producerID int, producerStatusReason types.ProducerStatusReason, isDown bool, isDelayed bool, timestamp time.Time) (types.ProducerStatus, error) {
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
//
// logger is optional — when nil, entity-build failures during AMQP
// decode are silently dropped (matching pre-v2.24 behaviour). Pass a
// logger to make those failures observable via WARN-level entries.
func NewFeedMessageFactory(entityFactory *EntityFactory, marketFactory *MarketFactory, producerManager *producer.Manager, oddsFeedConfiguration config.Config, logger *log.Logger) *FeedMessageFactory {
	return &FeedMessageFactory{
		entityFactory:         entityFactory,
		marketFactory:         marketFactory,
		producerManager:       producerManager,
		oddsFeedConfiguration: oddsFeedConfiguration,
		logger:                logger,
	}
}

// Compile-time interface satisfaction guards. If any concrete impl
// drifts from its public interface (added marker, renamed method,
// missing embed), the build fails here — not at runtime in the
// session loop's type-switch. The original BetStop production bug
// (betStopImpl missing isBetStop() so the message fell into
// default→unparsable) would have been caught by var _ here.
var (
	_ types.Message               = producerStatusImpl{}
	_ types.ProducerStatus        = producerStatusImpl{}
	_ types.UnparsableMessage     = unparsableMessageImpl{}
	_ types.OddsChange            = oddsChangeImpl{}
	_ types.BetStop               = betStopImpl{}
	_ types.BetSettlement         = betSettlementImpl{}
	_ types.BetCancel             = betCancelImpl{}
	_ types.FixtureChangeMessage  = fixtureChangeImpl{}
	_ types.RollbackBetSettlement = rollbackBetSettlementImpl{}
	_ types.RollbackBetCancel     = rollbackBetCancelImpl{}
)

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
	return cloneBytes(u.rawMessage)
}

type oddsChangeImpl struct {
	producer   types.Producer
	timestamp  types.MessageTimestamp
	rawMessage []byte
	message    *feedXML.OddsChange
	event      interface{}
	markets    []types.MarketWithOdds
}

func (m oddsChangeImpl) Producer() types.Producer {
	return m.producer
}

func (m oddsChangeImpl) Timestamp() types.MessageTimestamp {
	return m.timestamp
}

func (m oddsChangeImpl) RequestID() types.Optional[int] {
	return types.FromPtr(m.message.RequestID)
}

func (m oddsChangeImpl) RawMessage() []byte {
	return cloneBytes(m.rawMessage)
}

func (m oddsChangeImpl) Event() interface{} {
	return m.event
}

func (m oddsChangeImpl) Markets() []types.MarketWithOdds {
	return m.markets
}

type betStopImpl struct {
	// BetStopMarker (embedded) satisfies types.BetStop's unexported
	// isBetStop() method via composition. Without this marker, a
	// type-switch case for BetStop would also match every other
	// RequestMessage+WithEvent shape (BetCancel, FixtureChange,
	// Rollback*) — and BuildMessage's bet_stop dispatch would silently
	// fall through to default → unparsable. See types.BetStopMarker.
	types.BetStopMarker
	producer   types.Producer
	timestamp  types.MessageTimestamp
	requestID  types.Optional[int]
	rawMessage []byte
	event      interface{}
}

func (b betStopImpl) Producer() types.Producer {
	return b.producer
}

func (b betStopImpl) Timestamp() types.MessageTimestamp {
	return b.timestamp
}

func (b betStopImpl) RequestID() types.Optional[int] {
	return b.requestID
}

func (b betStopImpl) RawMessage() []byte {
	return cloneBytes(b.rawMessage)
}

func (b betStopImpl) Event() interface{} {
	return b.event
}

type betSettlementImpl struct {
	producer   types.Producer
	timestamp  types.MessageTimestamp
	rawMessage []byte
	message    *feedXML.BetSettlement
	event      interface{}
	markets    []types.MarketWithSettlement
}

func (m betSettlementImpl) Producer() types.Producer {
	return m.producer
}

func (m betSettlementImpl) Timestamp() types.MessageTimestamp {
	return m.timestamp
}

func (m betSettlementImpl) RequestID() types.Optional[int] {
	return types.FromPtr(m.message.RequestID)
}

func (m betSettlementImpl) RawMessage() []byte {
	return cloneBytes(m.rawMessage)
}

func (m betSettlementImpl) Event() interface{} {
	return m.event
}

func (m betSettlementImpl) Markets() []types.MarketWithSettlement {
	return m.markets
}

type betCancelImpl struct {
	producer   types.Producer
	timestamp  types.MessageTimestamp
	rawMessage []byte
	message    *feedXML.BetCancel
	event      interface{}
	markets    []types.MarketCancel
}

func (m betCancelImpl) Producer() types.Producer {
	return m.producer
}

func (m betCancelImpl) Timestamp() types.MessageTimestamp {
	return m.timestamp
}

func (m betCancelImpl) RequestID() types.Optional[int] {
	return types.FromPtr(m.message.RequestID)
}

func (m betCancelImpl) RawMessage() []byte {
	return cloneBytes(m.rawMessage)
}

func (m betCancelImpl) Event() interface{} {
	return m.event
}

func (m betCancelImpl) Markets() []types.MarketCancel {
	return m.markets
}

func (m betCancelImpl) StartTime() *time.Time {
	if m.message.StartTime == nil {
		return nil
	}
	// Wire carries epoch MILLISECONDS (13-digit, same unit as the
	// sibling timestamp attributes) — time.Unix(seconds) put these
	// ~56000 years in the future.
	startTime := time.UnixMilli(*m.message.StartTime)
	return &startTime
}

func (m betCancelImpl) EndTime() *time.Time {
	if m.message.EndTime == nil {
		return nil
	}
	endTime := time.UnixMilli(*m.message.EndTime)
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

func (f fixtureChangeImpl) RequestID() types.Optional[int] {
	return types.FromPtr(f.message.RequestID)
}

func (f fixtureChangeImpl) RawMessage() []byte {
	return cloneBytes(f.rawMessage)
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
	case feedXML.FixtureChangeTypeFormat:
		// Wire 4 = FORMAT in javasdk/netcoresdk; map to
		// OtherChangeFixtureChangeType (Java's OTHER_CHANGE).
		return types.OtherChangeFixtureChangeType
	case feedXML.FixtureChangeTypeCoverage:
		return types.CoverageFixtureChangeType
	case feedXML.FixtureChangeTypeStreamURL:
		return types.StreamURLFixtureChangeType
	default:
		return types.UnknownFixtureChangeType
	}
}

type rollbackBetSettlementImpl struct {
	producer   types.Producer
	timestamp  types.MessageTimestamp
	rawMessage []byte
	message    *feedXML.RollbackBetSettlement
	event      interface{}
	markets    []types.Market
}

func (m rollbackBetSettlementImpl) Producer() types.Producer {
	return m.producer
}

func (m rollbackBetSettlementImpl) Timestamp() types.MessageTimestamp {
	return m.timestamp
}

func (m rollbackBetSettlementImpl) RequestID() types.Optional[int] {
	return types.FromPtr(m.message.RequestID)
}

func (m rollbackBetSettlementImpl) RawMessage() []byte {
	return cloneBytes(m.rawMessage)
}

func (m rollbackBetSettlementImpl) Event() interface{} {
	return m.event
}

func (m rollbackBetSettlementImpl) RolledBackSettledMarkets() []types.Market {
	return m.markets
}

type rollbackBetCancelImpl struct {
	producer   types.Producer
	timestamp  types.MessageTimestamp
	rawMessage []byte
	message    *feedXML.RollbackBetCancel
	event      interface{}
	markets    []types.Market
}

func (m rollbackBetCancelImpl) Producer() types.Producer {
	return m.producer
}

func (m rollbackBetCancelImpl) Timestamp() types.MessageTimestamp {
	return m.timestamp
}

func (m rollbackBetCancelImpl) RequestID() types.Optional[int] {
	return types.FromPtr(m.message.RequestID)
}

func (m rollbackBetCancelImpl) RawMessage() []byte {
	return cloneBytes(m.rawMessage)
}

func (m rollbackBetCancelImpl) Event() interface{} {
	return m.event
}

func (m rollbackBetCancelImpl) RolledBackCanceledMarkets() []types.Market {
	return m.markets
}

func (m rollbackBetCancelImpl) StartTime() *time.Time {
	if m.message.StartTime == nil {
		return nil
	}
	// Epoch milliseconds — see betCancelImpl.StartTime.
	startTime := time.UnixMilli(*m.message.StartTime)
	return &startTime
}

func (m rollbackBetCancelImpl) EndTime() *time.Time {
	if m.message.EndTime == nil {
		return nil
	}
	endTime := time.UnixMilli(*m.message.EndTime)
	return &endTime
}
