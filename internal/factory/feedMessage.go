package factory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/oddin-gg/gosdk/internal/apipolicy"
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

	// buildFailThrottles holds one log throttle per kind of message-path
	// build failure ("match", "tournament"), created on first use so the
	// zero-value factory still works. Keyed per kind, not per event id:
	// an API outage fails the build for every event, and one line per
	// message is what made the log unreadable.
	buildFailThrottles sync.Map // string -> *log.Throttle
	// throttleNow is the throttles' clock; nil means time.Now (tests).
	throttleNow func() time.Time

	// apiFailures remembers events whose message-path build just failed
	// against the API; see apiFailureWindow.
	apiFailures apiFailureWindow
}

// apiSuccessCount is the REST API's success counter (0 without an entity
// factory); see apiFailureWindow.
func (f *FeedMessageFactory) apiSuccessCount() uint64 {
	return f.entityFactory.APISuccessCount()
}

// messagePolicy returns the ONE call-policy context of a message build and
// whether the event is inside the failure window. Everything the build does
// that may reach the REST API (the event and its parts, market and outcome
// name resolution) runs under it: one 2 s budget in all, one attempt per
// request, no retries; and, for an event whose build failed within the last
// few seconds, no API request at all (caches may still answer). Public
// getters are not affected: they keep their own policy.
func (f *FeedMessageFactory) messagePolicy(ctx context.Context, eventID *types.URN) (context.Context, bool) {
	skip := eventID != nil && f.apiFailures.blocked(*eventID, f.apiSuccessCount())
	if skip {
		return apipolicy.With(ctx, apipolicy.MessageSkip), true
	}
	return apipolicy.With(ctx, apipolicy.Message), false
}

// afterEventBuild returns the context the rest of the build (markets) runs
// under: the same budget while the event built or failed in a way that says
// nothing about the API (a 404 for an event not in REST yet: the description
// API is fine and names still resolve), and no API requests once the event's
// build said the API is unavailable (the window is open for it from then on,
// and one failed attempt per message is all the API gets).
func afterEventBuild(ctx, pctx context.Context, eventUnavailable bool) context.Context {
	if eventUnavailable {
		return apipolicy.With(ctx, apipolicy.MessageSkip)
	}
	return pctx
}

// buildMatch builds the event of a feed message under the policy context
// from messagePolicy. A failure is remembered until the window expires or
// the API answers any call successfully.
func (f *FeedMessageFactory) buildMatch(pctx context.Context, skip bool, id types.URN, locales []types.Locale, sportID *types.URN) (*types.Match, error) {
	m, err := f.entityFactory.BuildMatch(pctx, id, locales, sportID)
	if err != nil && !skip {
		f.noteBuildFailure(pctx, id, err)
	}
	return m, err
}

// buildTournament is buildMatch for a tournament event.
func (f *FeedMessageFactory) buildTournament(pctx context.Context, skip bool, id types.URN, sportID types.URN, locales []types.Locale) (*types.Tournament, error) {
	t, err := f.entityFactory.BuildTournament(pctx, id, sportID, locales)
	if err != nil && !skip {
		f.noteBuildFailure(pctx, id, err)
	}
	return t, err
}

// noteBuildFailure opens the window for id, unless the failure says nothing
// about the API: the caller's own cancellation, a closed client (shutdown), a
// request refused because the shared budget was already spent (nothing was
// sent; an earlier request of the same build that timed out is recorded on
// its own), or an answer of a healthy API such as a 404 for an event that is
// not in REST yet (see saysAPIUnavailable).
//
// The entry is bound to the success counter as read NOW, after the failed
// attempt, not before the build: one build makes several calls, and an
// earlier one that succeeded (the match summary, before the tournament
// fails) already advanced the counter — binding to the pre-build value would
// void the entry at once and the window would never open in a partial
// outage. Any success from here on still clears it.
func (f *FeedMessageFactory) noteBuildFailure(ctx context.Context, id types.URN, err error) {
	if ctx.Err() != nil || !saysAPIUnavailable(err) {
		return
	}
	f.apiFailures.record(id, f.apiSuccessCount())
}

// buildFailWarnInterval bounds each build-failure warn kind to one line
// per interval; the line carries the number of warns held back.
const buildFailWarnInterval = time.Minute

// warnBuildFailed logs a failed message-path build at WARN, throttled per
// kind: the first failure goes out at once, later ones at most once per
// buildFailWarnInterval with a suppressed count.
func (f *FeedMessageFactory) warnBuildFailed(kind string, err error, eventID types.URN, routingKey string) {
	if f.logger == nil {
		return
	}
	th, ok := f.buildFailThrottles.Load(kind)
	if !ok {
		th, _ = f.buildFailThrottles.LoadOrStore(kind, log.NewThrottle(buildFailWarnInterval, f.throttleNow))
	}
	suppressed, emit := th.(*log.Throttle).Allow()
	if !emit {
		return
	}
	l := f.logger.WithError(err)
	if suppressed > 0 {
		l = l.WithField("suppressed", suppressed)
	}
	l.Warnf("feed message: build %s %s failed for route %q", kind, eventID.ToString(), routingKey)
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
	// eventUnavailable: the event build failed in a way that says the API is
	// unavailable (see saysAPIUnavailable).
	eventUnavailable := false
	pctx, skip := f.messagePolicy(ctx, rk.EventID)
	switch types.EventType(rk.EventID.Type) {
	case types.TournamentEventType:
		if rk.SportID == nil {
			return nil, fmt.Errorf("feed message: tournament route %q missing sport id", rk.FullRoutingKey)
		}
		t, err := f.buildTournament(pctx, skip, *rk.EventID, *rk.SportID, f.buildLocales())
		switch {
		case err != nil:
			eventUnavailable = saysAPIUnavailable(err)
			// Transient cache/API failures during AMQP decode used to
			// be silently dropped — consumers saw a non-nil message
			// with event==nil and no log entry, indistinguishable
			// from "unsupported event type". Log so the failure is
			// observable; behaviour (dispatch with event==nil)
			// preserved for parity with Java/.NET.
			f.warnBuildFailed("tournament", err, *rk.EventID, rk.FullRoutingKey)
		case t != nil:
			event = *t
		}
	case types.MatchEventType:
		match, err := f.buildMatch(pctx, skip, *rk.EventID, f.buildLocales(), rk.SportID)
		switch {
		case err != nil:
			eventUnavailable = saysAPIUnavailable(err)
			f.warnBuildFailed("match", err, *rk.EventID, rk.FullRoutingKey)
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

	// Market and outcome name resolution may fetch descriptions: same
	// budget and policy as the event build (and no requests at all once the
	// event's build failed).
	mctx, nameFails := withNameFailures(afterEventBuild(ctx, pctx, eventUnavailable))
	defer func() {
		// Name resolution swallows API failures; a message whose event is
		// not already in the failure window records one, so the next
		// messages of the event skip the API instead of each paying the
		// budget again (the first success anywhere clears it). An event
		// build that already said the API is unavailable has recorded it.
		if err := nameFails.first(); err != nil && !skip && !eventUnavailable {
			f.noteBuildFailure(pctx, *rk.EventID, err)
		}
	}()
	switch msg := feedMessage.Message.(type) {
	case *feedXML.OddsChange:
		markets := make([]types.MarketWithOdds, len(msg.Odds.Markets))
		for i := range msg.Odds.Markets {
			markets[i] = f.marketFactory.BuildMarketWithOdds(mctx, event, msg.Odds.Markets[i])
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
			markets[i] = f.marketFactory.BuildMarketWithSettlement(mctx, event, msg.Markets.Markets[i])
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
			markets[i] = f.marketFactory.BuildMarketCancel(mctx, event, msg.Markets[i])
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
			markets[i] = f.marketFactory.BuildMarket(mctx, event, &msg.Markets[i].MarketAttributes)
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
			markets[i] = f.marketFactory.BuildMarket(mctx, event, &msg.Markets[i].MarketAttributes)
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
		pctx, skip := f.messagePolicy(ctx, rk.EventID)
		switch types.EventType(rk.EventID.Type) {
		case types.TournamentEventType:
			if rk.SportID != nil {
				t, err := f.buildTournament(pctx, skip, *rk.EventID, *rk.SportID, []types.Locale{f.oddsFeedConfiguration.DefaultLocale()})
				switch {
				case err != nil && f.logger != nil:
					f.logger.WithError(err).Warnf("unparsable: build tournament %s failed for route %q", rk.EventID.ToString(), rk.FullRoutingKey)
				case t != nil:
					event = *t
				}
			}
		case types.MatchEventType:
			match, err := f.buildMatch(pctx, skip, *rk.EventID, []types.Locale{f.oddsFeedConfiguration.DefaultLocale()}, rk.SportID)
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
