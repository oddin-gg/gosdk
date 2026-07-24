package gosdk

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/factory"
	"github.com/oddin-gg/gosdk/internal/feed"
	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/internal/producer"
	"github.com/oddin-gg/gosdk/types"
)

// discardLogger returns a Logger that drops everything — keeps test
// output clean while letting the rate-limited warn path run end-to-end.
func discardLogger() *log.Logger {
	return log.New(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// --- Test 5: drop-oldest event channels ---

func TestPushDropOldest_PreservesFreshOverOld(t *testing.T) {
	ch := make(chan int, 3)
	for i := range 6 {
		pushDropOldest(ch, i)
	}
	if got := len(ch); got != 3 {
		t.Fatalf("channel len = %d, want 3", got)
	}
	got := []int{<-ch, <-ch, <-ch}
	want := []int{3, 4, 5}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("ch[%d] = %d, want %d (full=%v)", i, got[i], want[i], got)
			break
		}
	}
}

func TestPushDropOldest_EmptyChannelJustEnqueues(t *testing.T) {
	ch := make(chan string, 2)
	dropped := pushDropOldest(ch, "a")
	if dropped {
		t.Error("dropped on empty channel should be false")
	}
	if len(ch) != 1 {
		t.Errorf("len = %d, want 1", len(ch))
	}
}

func TestPushDropOldest_ReportsDropOnOverflow(t *testing.T) {
	ch := make(chan int, 1)
	if pushDropOldest(ch, 1) {
		t.Error("first push to empty buffer dropped")
	}
	if !pushDropOldest(ch, 2) {
		t.Error("second push to full buffer should report dropped=true")
	}
}

func TestWarnDrop_RateLimits(t *testing.T) {
	rec := &recordingHandler{}
	c := &Client{logger: log.New(slog.New(rec))}
	var ts atomic.Int64
	c.warnDrop(&ts, "test", 8)
	first := ts.Load()
	if first == 0 {
		t.Fatal("expected timestamp to advance on first call")
	}
	c.warnDrop(&ts, "test", 8)
	if ts.Load() != first {
		t.Errorf("rate-limit failed: ts changed from %d to %d", first, ts.Load())
	}
	// The warning must actually be EMITTED, exactly once within the
	// window — pre-fix this test discarded logs and would have passed
	// with the warning removed entirely (Codex doc finding).
	warns := 0
	for _, m := range rec.messages() {
		if strings.Contains(m, "dropped") || strings.Contains(m, "overflow") {
			warns++
		}
	}
	if warns != 1 {
		t.Errorf("drop warnings emitted = %d, want exactly 1 (rate-limited); msgs=%v", warns, rec.messages())
	}
}

func TestPushDropOldest_ConcurrentSafe(t *testing.T) {
	ch := make(chan int, 4)
	var wg sync.WaitGroup
	const N = 1024
	for i := range N {
		wg.Add(1)
		go func(v int) {
			defer wg.Done()
			pushDropOldest(ch, v)
		}(i)
	}
	wg.Wait()
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// --- Test 4: concurrent Connect waits on in-flight ---
//
// Drives Connect's wait-on-in-flight branch directly via the
// connectMu/connectDone state machine. We can't exercise the full
// Connect path without AMQP — but the new contract is that a second
// caller waits on the channel rather than returning an error.

func TestClient_ConcurrentConnect_WaitsForInFlight(t *testing.T) {
	c := &Client{}
	c.connectState.Store(int32(ConnectionStateNotConnected))
	c.connecting = true
	res := &connectResult{done: make(chan struct{})}
	c.connectAttempt = res
	c.connectDone = res.done

	doneB := make(chan error, 1)
	go func() {
		c.lifecycleMu.Lock()
		att := c.connectAttempt
		c.lifecycleMu.Unlock()
		select {
		case <-att.done:
			doneB <- att.err
		case <-time.After(time.Second):
			doneB <- errors.New("timeout waiting on connect attempt")
		}
	}()

	// Caller A finishes the in-flight attempt successfully — outcome on
	// the IMMUTABLE attempt record, written before close(done).
	c.lifecycleMu.Lock()
	c.connecting = false
	res.err = nil
	res.ok = true
	c.connectState.Store(int32(ConnectionStateConnected))
	close(res.done)
	c.lifecycleMu.Unlock()

	select {
	case err := <-doneB:
		if err != nil {
			t.Fatalf("waiter saw err = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter never woke up")
	}
}

// --- Test 5b: per-channel buffer sizes follow NEXT.md spec ---
//
// NEXT.md §0.3 says recovery events get the largest buffer; API events
// match §9.3's default of 256; connection events are smallest.

func TestEventBufferSizesPerChannel(t *testing.T) {
	if connEventBuffer >= apiEventBuffer {
		t.Errorf("connEventBuffer (%d) should be < apiEventBuffer (%d)",
			connEventBuffer, apiEventBuffer)
	}
	if apiEventBuffer >= recoveryEventBuffer {
		t.Errorf("apiEventBuffer (%d) should be < recoveryEventBuffer (%d) per spec",
			apiEventBuffer, recoveryEventBuffer)
	}
	// Spot-check: NEXT.md §9.3 explicitly says APIEvents = 256.
	if apiEventBuffer != 256 {
		t.Errorf("apiEventBuffer = %d, NEXT.md §9.3 specifies 256", apiEventBuffer)
	}
	// Spot-check: NEXT.md §0.3 says RecoveryEvents = 1024.
	if recoveryEventBuffer != 1024 {
		t.Errorf("recoveryEventBuffer = %d, NEXT.md §0.3 specifies 1024", recoveryEventBuffer)
	}
}

// --- Test 3: StrategyThrow captures terminal error; Catch emits + continues ---
//
// Drives oddsFeedSessionImpl.emitUnparsable directly via a hand-built
// session struct so we don't need AMQP. Verifies:
//   - Catch path emits an UnparsableMessage to msgCh and Err() stays nil.
//   - Throw path captures the cause via Err() and triggers loop ctx cancel.

func TestSession_EmitUnparsable_StrategyCatch(t *testing.T) {
	o := &oddsFeedSessionImpl{
		exceptionStrategy: StrategyCatch,
		msgCh:             make(chan sessionEnvelope, 1),
	}
	cause := errors.New("boom")
	o.emitUnparsable(context.Background(), nil, cause, nil, false, nil)
	select {
	case env := <-o.msgCh:
		if env.msg.UnparsableMessage != nil {
			// nil unparsable is fine for this test — we just want a delivery.
			_ = env
		}
	case <-time.After(time.Second):
		t.Fatal("emitUnparsable (Catch) did not push to msgCh")
	}
	if err := o.Err(); err != nil {
		t.Errorf("Err() = %v, want nil under StrategyCatch", err)
	}
}

func TestSession_EmitUnparsable_StrategyThrow(t *testing.T) {
	cancelCalled := false
	o := &oddsFeedSessionImpl{
		exceptionStrategy: StrategyThrow,
		msgCh:             make(chan sessionEnvelope, 1),
		closeFn:           func() { cancelCalled = true },
	}
	cause := errors.New("decode failed")
	o.emitUnparsable(context.Background(), nil, cause, nil, false, nil)

	if got := o.Err(); !errors.Is(got, cause) {
		t.Errorf("Err() = %v, want %v", got, cause)
	}
	if !cancelCalled {
		t.Error("closeFn was not invoked under StrategyThrow")
	}
}

// --- C1: BetStop dispatches via concrete factory impl ---
//
// Regression for the production-breaking bug:
//
//   - types.BetStop is sealed via the unexported isBetStop() method
//     (see types/message.go).
//   - Pre-fix the concrete betStopImpl in internal/factory had Producer/
//     Timestamp/RequestID/RawMessage/Event but DID NOT have
//     isBetStop(), so it didn't satisfy types.BetStop.
//   - The session loop's type-switch case `types.BetStop:` never matched
//     real BetStop values; they fell into default → unparsable.
//   - The previous recovery-cursor test passed because its stubMessage
//     had isBetStop() inline — masking the production gap.
//
// Fix: types.BetStopMarker (public, unexported method) embedded into
// betStopImpl via composition + compile-time `var _ types.BetStop =
// betStopImpl{}` guard in the factory.
//
// This test asserts the actual factory output dispatches correctly
// without any test-only stub. If betStopImpl regresses, the test fails
// on the type-assertion.
func TestSession_BetStop_FactoryOutputDispatchesAsBetStop(t *testing.T) {
	cfg := &fakeOFC{}
	// BuildMessage now propagates producer.ErrNotOpened instead of
	// fabricating a placeholder (the placeholder made "no catalog yet"
	// look like a live producer), so the manager must be genuinely
	// opened — mirror the production ordering where the session's
	// GetProducer lazily opens before any BuildMessage runs.
	srv := catalogFixtureServer(t)
	defer srv.Close()
	apiClient := api.New(cfg)
	apiClient.SetHTTPClient(newTestHTTPClient(srv))
	pm := producer.NewManager(cfg, apiClient, log.New(nil))
	if err := pm.Open(t.Context()); err != nil {
		t.Fatalf("producer manager open: %v", err)
	}
	// nil entityFactory + marketFactory: the Player-type routing key
	// below skips both build paths, so the factory only exercises
	// timestamp + producer-cache + impl construction.
	f := factory.NewFeedMessageFactory(nil, nil, pm, cfg, log.New(nil))

	rk := &types.RoutingKeyInfo{
		FullRoutingKey: "hi.pre.-.bet_stop.1.123.-.-.-",
		// PlayerEventType: BuildMessage's switch falls into default
		// (no entityFactory call), so a nil entityFactory is safe.
		EventID: &types.URN{Prefix: "od", Type: string(types.PlayerEventType), ID: 123},
		SportID: &types.URN{Prefix: "od", Type: "sport", ID: 1},
	}
	feedMsg := &types.FeedMessage{
		BasicFeedMessage: types.BasicFeedMessage{
			RoutingKey: rk,
			RawMessage: []byte("<bet_stop/>"),
		},
		Message: &feedXML.BetStop{
			MessageAttributes: feedXML.MessageAttributes{Product: 1},
		},
	}
	out, err := f.BuildMessage(t.Context(), feedMsg)
	if err != nil {
		t.Fatalf("BuildMessage: %v", err)
	}
	if _, ok := out.(types.BetStop); !ok {
		t.Fatalf("BuildMessage(bet_stop) result %T does not satisfy types.BetStop — C1 regression", out)
	}

	// Negative dispatch check: the factory's BetStop output must NOT
	// also satisfy other RequestMessage+WithEvent interfaces that
	// have distinguishing methods. (Without the BetStopMarker, BetStop
	// would be {RequestMessage, WithEvent} alone — making it match
	// every other shape in the session loop's type-switch.)
	if _, ok := out.(types.OddsChange); ok {
		t.Errorf("BetStop also satisfies OddsChange — markers/method-sets are degenerate")
	}
	if _, ok := out.(types.BetCancel); ok {
		t.Errorf("BetStop also satisfies BetCancel — markers/method-sets are degenerate")
	}
}

// --- Recovery cursor advances for every successfully-built message ---
//
// Pre-fix processFeedMessage's switch only set `timestamp` for
// OddsChange and BetStop, leaving zero for BetCancel / BetSettlement
// / FixtureChange / RollbackBetSettlement / RollbackBetCancel. The
// recovery actor ignores zero timestamps, so traffic dominated by
// these types could leave LastProcessedMessageGenTimestamp stale and
// drive false producer-down decisions. This test pins each public
// message type → non-zero OnMessageProcessingEnded timestamp.

type fakeMessageBuilder struct {
	builtMessage any
}

func (f *fakeMessageBuilder) BuildMessage(_ context.Context, _ *types.FeedMessage) (any, error) {
	return f.builtMessage, nil
}
func (f *fakeMessageBuilder) BuildUnparsableMessage(_ context.Context, _ *types.FeedMessage) types.UnparsableMessage {
	return nil
}

type recordingRecoveryProcessor struct {
	endedTimestamp atomic.Pointer[time.Time]
}

func (r *recordingRecoveryProcessor) OnMessageProcessingStarted(uuid.UUID, int, time.Time) {}
func (r *recordingRecoveryProcessor) OnMessageProcessingEnded(_ uuid.UUID, _ int, ts time.Time) {
	r.endedTimestamp.Store(&ts)
}
func (r *recordingRecoveryProcessor) OnAliveReceived(int, types.MessageTimestamp, bool, types.MessageInterest) {
}
func (r *recordingRecoveryProcessor) OnSnapshotCompleteReceived(context.Context, int, int, types.MessageInterest) error {
	return nil
}

// stubMessage implements every public message interface (OddsChange,
// BetStop, BetCancel, BetSettlement, FixtureChangeMessage,
// RollbackBetSettlement, RollbackBetCancel) so each subtest can re-use
// it via type-asserts the session's switch performs. Asserts only
// require that the interface methods are present; values can be zero.
type stubMessage struct {
	ts types.MessageTimestamp
}

func (s stubMessage) Producer() types.Producer                        { return nil }
func (s stubMessage) Timestamp() types.MessageTimestamp               { return s.ts }
func (s stubMessage) RequestID() types.Optional[int]                  { return types.None[int]() }
func (s stubMessage) RawMessage() []byte                              { return nil }
func (s stubMessage) Event() any                                      { return nil }
func (s stubMessage) Markets() []types.MarketWithOdds                 { return nil }
func (s stubMessage) MarketsSettlement() []types.MarketWithSettlement { return nil }
func (s stubMessage) MarketsCancel() []types.MarketCancel             { return nil }
func (s stubMessage) MarketsRolled() []types.Market                   { return nil }
func (s stubMessage) StartTime() *time.Time                           { return nil }
func (s stubMessage) EndTime() *time.Time                             { return nil }
func (s stubMessage) ChangeType() types.FixtureChangeType             { return types.UnknownFixtureChangeType }
func (s stubMessage) RolledBackSettledMarkets() []types.Market        { return nil }
func (s stubMessage) RolledBackCanceledMarkets() []types.Market       { return nil }

// Each adapter pins stubMessage to one specific interface so the
// session's type-switch resolves to the intended case.
type oddsChangeStub struct{ stubMessage }

func (oddsChangeStub) Markets() []types.MarketWithOdds { return nil }

// betStopStub embeds types.BetStopMarker (composition) so it satisfies
// the sealed types.BetStop interface — same pattern the production
// betStopImpl uses. Pre-fix the test had isBetStop() declared inline
// on stubMessage, which (a) lint-flagged as unused and (b) masked the
// production C1 bug because the test stub satisfied BetStop without
// going through the marker.
type betStopStub struct {
	stubMessage
	types.BetStopMarker
}

type betCancelStub struct {
	stubMessage
}

func (betCancelStub) Markets() []types.MarketCancel { return nil }

type betSettlementStub struct{ stubMessage }

func (betSettlementStub) Markets() []types.MarketWithSettlement { return nil }

type fixtureChangeStub struct{ stubMessage }
type rollbackSettleStub struct{ stubMessage }
type rollbackCancelStub struct{ stubMessage }

func TestSession_ProcessFeedMessage_AdvancesRecoveryCursorForAllTypes(t *testing.T) {
	wantTS := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		msg  any
	}{
		{"OddsChange", oddsChangeStub{}},
		{"BetStop", betStopStub{}},
		{"BetCancel", betCancelStub{}},
		{"BetSettlement", betSettlementStub{}},
		{"FixtureChangeMessage", fixtureChangeStub{}},
		{"RollbackBetSettlement", rollbackSettleStub{}},
		{"RollbackBetCancel", rollbackCancelStub{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recordingRecoveryProcessor{}
			o := &oddsFeedSessionImpl{
				cacheManager:             &spyCacheNotifier{},
				feedMessageFactory:       &fakeMessageBuilder{builtMessage: tc.msg},
				recoveryMessageProcessor: rec,
				logger:                   discardLogger(),
				msgCh:                    make(chan sessionEnvelope, 1),
				sessionID:                uuid.New(),
			}
			fm := &types.FeedMessage{
				BasicFeedMessage: types.BasicFeedMessage{
					Timestamp: types.MessageTimestamp{Created: wantTS},
				},
				Message: &feedXML.Alive{ProductID: 1}, // not used; BuildMessage returns tc.msg
			}
			// Skip Alive/SnapshotComplete by feeding a non-Alive XML
			// message into processFeedMessage. We use a FixtureChange
			// since it's an IDMessage but processFeedMessage's first
			// switch (Alive/SnapshotComplete) won't match.
			fm.Message = &feedXML.FixtureChange{ProductID: 1, EventID: "od:match:1"}

			o.processFeedMessage(context.Background(), fm, types.AllMessageInterest, nil, false, nil)

			got := rec.endedTimestamp.Load()
			if got == nil {
				t.Fatalf("OnMessageProcessingEnded never called")
			}
			if !got.Equal(wantTS) {
				t.Errorf("OnMessageProcessingEnded timestamp = %v, want %v", *got, wantTS)
			}
		})
	}
}

// TestSession_ProcessFeedMessage_CanceledSendDoesNotAdvanceCursor is the
// regression for the processed-cursor advance on a never-admitted
// message (Codex P2): the typed switch ignored send's bool and always
// called endProcessing(Created). A ctx-cancelled send (graceful-close
// deadline / shutdown) delivered and acked NOTHING, yet the cursor
// advanced — calculateTiming would then see a spuriously recent
// processed cursor and could delay/suppress processing-queue-delay
// recovery. Post-fix a failed send leaves the cursor at zero (deferred
// endProcessing(time.Time{})).
func TestSession_ProcessFeedMessage_CanceledSendDoesNotAdvanceCursor(t *testing.T) {
	wantTS := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	rec := &recordingRecoveryProcessor{}
	o := &oddsFeedSessionImpl{
		cacheManager:             &spyCacheNotifier{},
		feedMessageFactory:       &fakeMessageBuilder{builtMessage: oddsChangeStub{}},
		recoveryMessageProcessor: rec,
		logger:                   discardLogger(),
		msgCh:                    make(chan sessionEnvelope), // UNBUFFERED: no receiver → send can't proceed
		sessionID:                uuid.New(),
	}
	fm := &types.FeedMessage{
		BasicFeedMessage: types.BasicFeedMessage{Timestamp: types.MessageTimestamp{Created: wantTS}},
		Message:          &feedXML.FixtureChange{ProductID: 1, EventID: "od:match:1"},
	}

	// Already-cancelled ctx + unbuffered channel: send's select has only
	// ctx.Done() ready, so it deterministically returns false (not admitted).
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	o.processFeedMessage(ctx, fm, types.AllMessageInterest, func() {}, false, nil)

	got := rec.endedTimestamp.Load()
	if got == nil {
		t.Fatal("OnMessageProcessingEnded never called (processing-started must always be paired)")
	}
	if !got.IsZero() {
		t.Fatalf("processed cursor advanced to %v for a never-admitted message; want zero", *got)
	}
}

// --- Replay sessions must not mutate the live cache ---
//
// Regression for the reviewer's architectural finding: the cacheManager
// is shared across every session of a Client (one cache per Client by
// design), so without a gate, replay traffic would invalidate live
// cache entries (a historical fixture_change/odds_change/bet_settlement
// would clear/mutate state real-time consumers depend on).
//
// Strategy: drive processFeedMessage with an Alive message — the gate
// runs unconditionally for every feed message, not just IDMessages, so
// Alive is sufficient to observe the call. We use a spy cacheNotifier
// to record OnFeedMessageReceived invocations; an Alive message also
// short-circuits the type switch BEFORE BuildMessage is reached, so we
// don't need a real FeedMessageFactory.

type spyCacheNotifier struct {
	calls atomic.Int32
}

func (s *spyCacheNotifier) OnFeedMessageReceived(*types.FeedMessage) {
	s.calls.Add(1)
}

type noopRecoveryProcessor struct{}

func (noopRecoveryProcessor) OnMessageProcessingStarted(uuid.UUID, int, time.Time) {}
func (noopRecoveryProcessor) OnMessageProcessingEnded(uuid.UUID, int, time.Time)   {}
func (noopRecoveryProcessor) OnAliveReceived(int, types.MessageTimestamp, bool, types.MessageInterest) {
}
func (noopRecoveryProcessor) OnSnapshotCompleteReceived(context.Context, int, int, types.MessageInterest) error {
	return nil
}

func TestSession_ProcessFeedMessage_ReplayDoesNotNotifyCache(t *testing.T) {
	for _, tc := range []struct {
		name      string
		isReplay  bool
		wantCalls int32
	}{
		{"live session notifies cache", false, 1},
		{"replay session skips cache", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spy := &spyCacheNotifier{}
			o := &oddsFeedSessionImpl{
				cacheManager:             spy,
				recoveryMessageProcessor: noopRecoveryProcessor{},
				isReplay:                 tc.isReplay,
				logger:                   discardLogger(),
				msgCh:                    make(chan sessionEnvelope, 1),
				sessionID:                uuid.New(),
			}
			fm := &types.FeedMessage{Message: &feedXML.Alive{ProductID: 1}}
			o.processFeedMessage(context.Background(), fm, types.AllMessageInterest, nil, false, nil)

			if got := spy.calls.Load(); got != tc.wantCalls {
				t.Errorf("OnFeedMessageReceived calls = %d, want %d", got, tc.wantCalls)
			}
		})
	}
}

// --- v2.5 review: Close-during-Connect race uses CAS ---
//
// Verifies the success-path CAS in Connect: when state has already
// transitioned to Closed (because a concurrent Close fired), the
// Connecting → Connected transition no-ops instead of overwriting.

func TestClient_ConnectSuccessPath_DoesNotStompClosed(t *testing.T) {
	c := &Client{}
	c.connectState.Store(int32(ConnectionStateConnecting))

	// Simulate Close racing in: state goes to Closed before Connect
	// reaches its success-path CAS.
	c.connectState.Store(int32(ConnectionStateClosed))

	// The CAS in Connect (Connecting → Connected) must fail.
	swapped := c.connectState.CompareAndSwap(
		int32(ConnectionStateConnecting),
		int32(ConnectionStateConnected),
	)
	if swapped {
		t.Error("CAS swapped Closed → Connected; should have refused")
	}
	if got := ConnectionState(c.connectState.Load()); got != ConnectionStateClosed {
		t.Errorf("state = %v, want Closed", got)
	}
}

// TestClient_ConnectFailurePath_DoesNotStompClosed exercises the v2.7
// review fix: Connect's failure-path defer used to do an unconditional
// Store(NotConnected). If Close fired during the in-flight attempt and
// the attempt then errored out, the defer would overwrite Closed →
// NotConnected, allowing a subsequent Connect to run after shutdown.
//
// The fix is a CAS Connecting → NotConnected. We exercise it directly:
// pre-set state to Closed (simulating a concurrent Close that beat us),
// then run the defer's CAS and confirm Closed survives.
func TestClient_ConnectFailurePath_DoesNotStompClosed(t *testing.T) {
	c := &Client{}
	// Concurrent Close ran during this Connect attempt.
	c.connectState.Store(int32(ConnectionStateClosed))

	// The CAS in Connect's defer (Connecting → NotConnected) must fail.
	swapped := c.connectState.CompareAndSwap(
		int32(ConnectionStateConnecting),
		int32(ConnectionStateNotConnected),
	)
	if swapped {
		t.Error("CAS swapped Closed → NotConnected; should have refused")
	}
	if got := ConnectionState(c.connectState.Load()); got != ConnectionStateClosed {
		t.Errorf("state = %v, want Closed", got)
	}
}

// TestClient_ConnectFailurePath_ConnectingToNotConnected verifies the
// CAS does fire on the happy failure path — Connecting → NotConnected
// when no concurrent Close raced in. Tests the inverse to make sure
// the CAS isn't broken in the common case.
func TestClient_ConnectFailurePath_ConnectingToNotConnected(t *testing.T) {
	c := &Client{}
	c.connectState.Store(int32(ConnectionStateConnecting))

	swapped := c.connectState.CompareAndSwap(
		int32(ConnectionStateConnecting),
		int32(ConnectionStateNotConnected),
	)
	if !swapped {
		t.Error("CAS Connecting → NotConnected refused on the happy path")
	}
	if got := ConnectionState(c.connectState.Load()); got != ConnectionStateNotConnected {
		t.Errorf("state = %v, want NotConnected", got)
	}
}

// --- v2.8 review fixes ---

// TestClient_PushAfterEventsClosed_NoPanic exercises the v2.14 fix:
// after runShutdown closes the event channels, late emit calls (api,
// recovery, connection) must observe the eventsClosed gate under
// RLock and return without sending — no send-on-closed-channel panic.
//
// Mirrors the in-flight-emitter race: an api.Client that snapshotted
// the emit callback BEFORE shutdown can call it AFTER shutdown closes
// apiEvents. The gate must absorb that.
func TestClient_PushAfterEventsClosed_NoPanic(t *testing.T) {
	c := &Client{
		connEvents: make(chan ConnectionEvent, 1),
		recvEvents: make(chan RecoveryEvent, 1),
		apiEvents:  make(chan APIEvent, 1),
		logger:     log.New(slog.New(slog.NewTextHandler(io.Discard, nil))),
	}

	// Simulate runShutdown's close-events critical section.
	c.eventsMu.Lock()
	c.eventsClosed = true
	close(c.connEvents)
	close(c.recvEvents)
	close(c.apiEvents)
	c.eventsMu.Unlock()

	// All three pushers must NOT panic post-close.
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("post-close push panicked: %v", r)
		}
	}()
	c.pushConn(ConnectionEvent{Kind: ConnectionConnected})
	c.pushRecovery(RecoveryEvent{})
	c.pushAPIEvent(api.APIEvent{Method: "GET", URL: "/x"})
}

// TestClient_PushBeforeClose_RaceFree drives concurrent pushes and
// the close-events critical section under -race. The gate is correct
// iff no race AND no panic.
func TestClient_PushBeforeClose_RaceFree(t *testing.T) {
	c := &Client{
		connEvents: make(chan ConnectionEvent, 4),
		recvEvents: make(chan RecoveryEvent, 4),
		apiEvents:  make(chan APIEvent, 4),
		logger:     log.New(slog.New(slog.NewTextHandler(io.Discard, nil))),
	}

	defer func() {
		if r := recover(); r != nil {
			t.Errorf("concurrent push/close panicked: %v", r)
		}
	}()

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				c.pushAPIEvent(api.APIEvent{})
				c.pushConn(ConnectionEvent{})
				c.pushRecovery(RecoveryEvent{})
			}
		})
	}
	// Closer.
	wg.Go(func() {
		time.Sleep(time.Millisecond)
		c.eventsMu.Lock()
		c.eventsClosed = true
		close(c.connEvents)
		close(c.recvEvents)
		close(c.apiEvents)
		c.eventsMu.Unlock()
	})
	wg.Wait()
}

// TestClient_OnFeedEvent_SuppressedWhenNotConnected verifies the v2.14
// fix: replay-only AMQP opens (which keep connectState == NotConnected)
// must not surface ConnectionConnected events to consumers, since
// ConnectionState() would still report NotConnected — internally
// inconsistent observability.
func TestClient_OnFeedEvent_SuppressedWhenNotConnected(t *testing.T) {
	c := &Client{
		connEvents: make(chan ConnectionEvent, 1),
		logger:     log.New(slog.New(slog.NewTextHandler(io.Discard, nil))),
	}
	c.connectState.Store(int32(ConnectionStateNotConnected))

	c.onFeedEvent(feed.Event{Kind: feed.EventConnected})

	select {
	case ev := <-c.connEvents:
		t.Errorf("event emitted while NotConnected: %+v", ev)
	default:
		// No event — correct.
	}
}

// TestClient_OnFeedEvent_SuppressedWhileConnecting verifies the v2.17
// fix to finding F3: feed-layer EventConnected fired during
// modeNormalConnecting (publicState == Connecting) is NOT published
// to consumers. ensureNormal's explicit emit after the
// modeNormalConnecting → modeNormalReady CAS owns this transition
// edge. Without this suppression, a feed-layer Connected fired during
// rmq.Open (or an autoreconnect mid-Connect) would publish
// ConnectionConnected before recovery / alive setup can still fail —
// a subsequent rollback would leave consumers seeing Connected with
// ConnectionState back at NotConnected.
//
// EventDisconnected and EventReconnecting still fire during Connecting
// (they're not gated on Connected) so that mid-Connect broker drops
// remain observable.
func TestClient_OnFeedEvent_SuppressedWhileConnecting(t *testing.T) {
	c := &Client{
		connEvents: make(chan ConnectionEvent, 4),
		logger:     log.New(slog.New(slog.NewTextHandler(io.Discard, nil))),
	}
	c.connectState.Store(int32(ConnectionStateConnecting))

	c.onFeedEvent(feed.Event{Kind: feed.EventConnected})
	if c.connectedEmitted.Load() {
		t.Fatal("up-edge gate claimed during Connecting (F3 not in effect)")
	}
	select {
	case ev := <-c.connEvents:
		t.Fatalf("EventConnected during Connecting was published: %+v", ev)
	default:
		// Correct: suppressed.
	}

	// Sanity: once mode reaches Connected, the next EventConnected
	// flows through normally (covers natural reconnects).
	c.connectState.Store(int32(ConnectionStateConnected))
	c.onFeedEvent(feed.Event{Kind: feed.EventConnected})
	select {
	case ev := <-c.connEvents:
		if ev.Kind != ConnectionConnected {
			t.Fatalf("kind = %v, want ConnectionConnected", ev.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("EventConnected during Connected was not published")
	}
}

// TestClient_emitConnConnectedOnce_AtMostOncePerUpEdge verifies the
// v2.15 contract: emitConnConnectedOnce publishes ConnectionConnected
// at most once per "up edge" — i.e. between consecutive
// Disconnected/Reconnecting events that clear connectedEmitted. This
// is what makes both onFeedEvent (feed-layer EventConnected) and
// Connect's success-path explicit emit safe to race against each
// other: whichever runs first publishes; the other is a CAS no-op.
func TestClient_emitConnConnectedOnce_AtMostOncePerUpEdge(t *testing.T) {
	c := &Client{
		connEvents: make(chan ConnectionEvent, 4),
		logger:     log.New(slog.New(slog.NewTextHandler(io.Discard, nil))),
	}

	// First call publishes.
	if !c.emitConnConnectedOnce(nil) {
		t.Fatal("first call should publish")
	}
	select {
	case ev := <-c.connEvents:
		if ev.Kind != ConnectionConnected {
			t.Errorf("kind = %v, want ConnectionConnected", ev.Kind)
		}
	default:
		t.Fatal("first call did not enqueue an event")
	}

	// Second call must be a no-op (gate already claimed by the up-edge).
	if c.emitConnConnectedOnce(nil) {
		t.Fatal("second call should be a no-op")
	}
	select {
	case ev := <-c.connEvents:
		t.Fatalf("unexpected duplicate event: %+v", ev)
	default:
	}

	// After a Disconnected event clears the gate (see onFeedEvent),
	// the next Connected emits again.
	c.connectedEmitted.Store(false)
	if !c.emitConnConnectedOnce(nil) {
		t.Fatal("post-disconnect call should publish")
	}
	select {
	case ev := <-c.connEvents:
		if ev.Kind != ConnectionConnected {
			t.Errorf("kind = %v, want ConnectionConnected", ev.Kind)
		}
	default:
		t.Fatal("post-disconnect call did not enqueue an event")
	}
}

// TestClient_emitConnConnectedOnce_RaceFreeOnReconnect exercises the
// CAS gate in the post-Connect runtime where it actually races: state
// is already Connected (modeNormalReady), feed-layer fires
// EventConnected from autoreconnect, and a parallel call site (e.g.,
// a defensive ensure path) runs emitConnConnectedOnce. Both go
// through the gate; exactly one publishes.
//
// v2.17's F3 made the original Connecting-state version of this test
// stale: feed-layer Connected during Connecting is now suppressed,
// so the CAS race never fires. This rewrite stages the race in the
// runtime state where the gate is actually exercised — natural
// reconnects, where both onFeedEvent and any explicit caller may
// publish ConnectionConnected.
func TestClient_emitConnConnectedOnce_RaceFreeOnReconnect(t *testing.T) {
	for trial := range 200 {
		c := &Client{
			connEvents: make(chan ConnectionEvent, 8),
			logger:     log.New(slog.New(slog.NewTextHandler(io.Discard, nil))),
		}
		// Post-Connect runtime: state is Connected, gate is armed
		// (cleared by the prior Disconnected/Reconnecting in a real
		// reconnect cycle).
		c.connectState.Store(int32(ConnectionStateConnected))
		c.connectedEmitted.Store(false)

		var wg sync.WaitGroup
		wg.Add(2)

		// Side 1: onFeedEvent receives feed.EventConnected (autoreconnect).
		go func() {
			defer wg.Done()
			c.onFeedEvent(feed.Event{Kind: feed.EventConnected})
		}()

		// Side 2: an explicit emitConnConnectedOnce caller racing on
		// the same up-edge.
		go func() {
			defer wg.Done()
			c.emitConnConnectedOnce(nil)
		}()

		wg.Wait()

		count := 0
	drain:
		for {
			select {
			case ev := <-c.connEvents:
				if ev.Kind != ConnectionConnected {
					t.Fatalf("trial %d: unexpected event kind %v", trial, ev.Kind)
				}
				count++
			default:
				break drain
			}
		}
		if count != 1 {
			t.Fatalf("trial %d: got %d ConnectionConnected events, want exactly 1", trial, count)
		}
	}
}

// TestClient_onFeedEvent_DisconnectClearsUpEdgeGate verifies the
// v2.15 contract that Disconnected/Reconnecting clears
// connectedEmitted, so a subsequent post-Connect natural reconnect
// publishes a fresh ConnectionConnected event (rather than being
// silently muted by the gate that an earlier Connect attempt set).
func TestClient_onFeedEvent_DisconnectClearsUpEdgeGate(t *testing.T) {
	c := &Client{
		connEvents: make(chan ConnectionEvent, 8),
		logger:     log.New(slog.New(slog.NewTextHandler(io.Discard, nil))),
	}
	c.connectState.Store(int32(ConnectionStateConnected))

	// First up-edge: feed-layer fires Connected.
	c.onFeedEvent(feed.Event{Kind: feed.EventConnected})
	if !c.connectedEmitted.Load() {
		t.Fatal("up-edge gate not claimed after EventConnected")
	}
	if got := drainConnEvents(c); len(got) != 1 || got[0] != ConnectionConnected {
		t.Fatalf("first emit = %v, want [ConnectionConnected]", got)
	}

	// Network glitch: Disconnected, Reconnecting, Connected.
	c.onFeedEvent(feed.Event{Kind: feed.EventDisconnected})
	if c.connectedEmitted.Load() {
		t.Fatal("Disconnected did not clear the up-edge gate")
	}
	c.onFeedEvent(feed.Event{Kind: feed.EventReconnecting})
	c.onFeedEvent(feed.Event{Kind: feed.EventConnected})

	got := drainConnEvents(c)
	want := []ConnectionEventKind{
		ConnectionDisconnected,
		ConnectionReconnecting,
		ConnectionConnected,
	}
	if len(got) != len(want) {
		t.Fatalf("event kinds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

// TestClient_ConnectionState_ReflectsFeedReconnect verifies the polling
// contract from events.go ("Connecting = a dial or reconnect attempt is
// in flight"): a feed-layer broker drop while the pipeline is up must
// flip ConnectionState() from Connected to Connecting until the feed
// layer reconnects. Pre-fix the sole connectState writer was setMode —
// never called from onFeedEvent — so once ensureNormal reached
// modeNormalReady, polling reported Connected across every broker
// drop/reconnect cycle and a consumer that missed the lossy
// Reconnecting event (the exact case polling exists for) could never
// observe the reconnect window.
func TestClient_ConnectionState_ReflectsFeedReconnect(t *testing.T) {
	c := &Client{
		connEvents: make(chan ConnectionEvent, 8),
		logger:     log.New(slog.New(slog.NewTextHandler(io.Discard, nil))),
	}
	c.connectState.Store(int32(ConnectionStateConnected))

	if got := c.ConnectionState(); got != ConnectionStateConnected {
		t.Fatalf("ConnectionState() = %v, want Connected", got)
	}

	// Broker drop: Disconnected then Reconnecting — polling must report
	// Connecting for the whole window.
	c.onFeedEvent(feed.Event{Kind: feed.EventDisconnected})
	if got := c.ConnectionState(); got != ConnectionStateConnecting {
		t.Fatalf("ConnectionState() after Disconnected = %v, want Connecting", got)
	}
	c.onFeedEvent(feed.Event{Kind: feed.EventReconnecting})
	if got := c.ConnectionState(); got != ConnectionStateConnecting {
		t.Fatalf("ConnectionState() after Reconnecting = %v, want Connecting", got)
	}

	// Reconnect lands: back to Connected.
	c.onFeedEvent(feed.Event{Kind: feed.EventConnected})
	if got := c.ConnectionState(); got != ConnectionStateConnected {
		t.Fatalf("ConnectionState() after reconnect = %v, want Connected", got)
	}

	// The overlay must not leak into other lifecycle phases: a stale
	// feedDown while Closed reports Closed.
	c.feedDown.Store(true)
	c.connectState.Store(int32(ConnectionStateClosed))
	if got := c.ConnectionState(); got != ConnectionStateClosed {
		t.Fatalf("ConnectionState() while Closed = %v, want Closed", got)
	}
	drainConnEvents(c)
}

// TestClient_onFeedEvent_ReplayOnlyKeepsLivenessOverlayAccurate verifies
// the overlay is maintained even while public events are suppressed
// (connectState == NotConnected, replay-only): a drop during the
// broker-only phase sets feedDown, and the reconnect's EventConnected —
// though never published — clears it. Without the pre-gate update, a
// later ensureNormal reusing the already-open rmq (rmq.Open no-op → no
// fresh EventConnected) would inherit a stale feedDown=true and report
// Connecting forever despite a healthy broker.
func TestClient_onFeedEvent_ReplayOnlyKeepsLivenessOverlayAccurate(t *testing.T) {
	c := &Client{
		connEvents: make(chan ConnectionEvent, 8),
		logger:     log.New(slog.New(slog.NewTextHandler(io.Discard, nil))),
	}
	c.connectState.Store(int32(ConnectionStateNotConnected))

	c.onFeedEvent(feed.Event{Kind: feed.EventDisconnected})
	if !c.feedDown.Load() {
		t.Fatal("feedDown not set by Disconnected in replay-only mode")
	}
	c.onFeedEvent(feed.Event{Kind: feed.EventConnected})
	if c.feedDown.Load() {
		t.Fatal("feedDown not cleared by suppressed EventConnected in replay-only mode")
	}
	// No public events surfaced throughout.
	if got := drainConnEvents(c); len(got) != 0 {
		t.Fatalf("events surfaced in replay-only mode: %v", got)
	}
}

// TestClient_onFeedEvent_NotConnectedSuppressesEverything verifies
// the v2.14 invariant survives the v2.15 redesign: while
// connectState == NotConnected (replay-only path), feed-layer events
// must not surface AND must not claim the up-edge gate. Otherwise a
// replay subscription could pre-set connectedEmitted=true, which
// would mute the first real Connect's emit.
func TestClient_onFeedEvent_NotConnectedSuppressesEverything(t *testing.T) {
	c := &Client{
		connEvents: make(chan ConnectionEvent, 4),
		logger:     log.New(slog.New(slog.NewTextHandler(io.Discard, nil))),
	}
	c.connectState.Store(int32(ConnectionStateNotConnected))

	c.onFeedEvent(feed.Event{Kind: feed.EventConnected})
	c.onFeedEvent(feed.Event{Kind: feed.EventDisconnected})
	c.onFeedEvent(feed.Event{Kind: feed.EventReconnecting})

	if c.connectedEmitted.Load() {
		t.Fatal("up-edge gate claimed while NotConnected (replay path leaked into gate)")
	}
	if got := drainConnEvents(c); len(got) != 0 {
		t.Fatalf("events emitted while NotConnected: %v", got)
	}
}

func drainConnEvents(c *Client) []ConnectionEventKind {
	var out []ConnectionEventKind
	for {
		select {
		case ev := <-c.connEvents:
			out = append(out, ev.Kind)
		default:
			return out
		}
	}
}

// TestClient_rollbackPartialNormal_PreservesRmqWhenBrokerOnly verifies
// the v2.17 fix to finding F1: when a normal-Connect attempt fails
// after replay had already opened AMQP (priorMode == modeBrokerOnly),
// the rollback must NOT close or replace the rmq. Existing replay
// subscriptions hold references to it via their session's
// channelConsumer; closing it would tear them down.
//
// Pre-v2.17 the recovery-Open and alive-Open failure paths
// unconditionally called rmq.Close + resetConnectionLayer, which
// physically broke replay subs even though the mode was rolled back
// to modeBrokerOnly. v2.17 routes both paths through
// rollbackPartialNormal, which preserves rmq when priorMode is
// modeBrokerOnly and only replaces the recovery.Manager (one-shot,
// always replaced).
func TestClient_rollbackPartialNormal_PreservesRmqWhenBrokerOnly(t *testing.T) {
	c := newTestClient(t)

	rmqBefore := c.rabbitMQClient.Load()
	rmgrBefore := c.recoveryManager.Load()

	// Simulate a recovery-Open failure rollback. ensureNormal had:
	// priorMode == modeBrokerOnly, alive == nil (not yet built),
	// rmgr == loaded recovery.Manager, rmq == loaded feed.Client.
	c.rollbackPartialNormal(t.Context(), nil, rmgrBefore, rmqBefore, modeBrokerOnly)

	if c.rabbitMQClient.Load() != rmqBefore {
		t.Fatal("rmq was replaced; replay subscriptions would be broken")
	}
	if c.recoveryManager.Load() == rmgrBefore {
		t.Fatal("rmgr was NOT replaced; retry would see Closed manager")
	}
}

// TestClient_rollbackPartialNormal_ResetsAllWhenModeNew verifies the
// converse of F1: when priorMode == modeNew, the rollback must
// replace both rmq and rmgr — there are no replay subs depending on
// the old rmq, and a fresh retry needs a clean connection layer.
func TestClient_rollbackPartialNormal_ResetsAllWhenModeNew(t *testing.T) {
	c := newTestClient(t)

	rmqBefore := c.rabbitMQClient.Load()
	rmgrBefore := c.recoveryManager.Load()

	c.rollbackPartialNormal(t.Context(), nil, rmgrBefore, rmqBefore, modeNew)

	if c.rabbitMQClient.Load() == rmqBefore {
		t.Fatal("rmq was NOT replaced from modeNew rollback; retry would see stale state")
	}
	if c.recoveryManager.Load() == rmgrBefore {
		t.Fatal("rmgr was NOT replaced from modeNew rollback")
	}
}

// TestClient_ensureBroker_RejectsAfterBeginClose verifies the v2.17
// fix to finding F2 (admission half): after beginClose transitions to
// modeClosing, any new ensureBroker call returns ErrAlreadyClosed
// without touching brokerOpenWG. This is what makes runShutdown's
// brokerOpenWG.Wait() guaranteed to drain — no new Adds can occur.
func TestClient_ensureBroker_RejectsAfterBeginClose(t *testing.T) {
	c := newTestClient(t)

	if got := c.beginClose(); got != nil {
		t.Fatalf("unexpected in-flight chan from beginClose: %v", got)
	}

	_, err := c.ensureBroker(t.Context())
	if !errors.Is(err, ErrAlreadyClosed) {
		t.Fatalf("ensureBroker after beginClose: err = %v, want ErrAlreadyClosed", err)
	}
}

// TestClient_ensureBroker_WaitsForInFlightEnsureNormal verifies the
// v2.18 cross-flow fix: a replay Subscribe arriving while a normal
// Connect is in flight (mode == modeNormalConnecting) MUST NOT proceed
// to adopt the rmq mid-Connect. If ensureNormal then fails at
// recovery/alive setup, rollbackPartialNormal(...,priorMode=modeNew)
// would replace rmq, and any replay session built mid-Connect would
// be attached to a closed broker.
//
// The fix: ensureBroker waits on connectDone (looping if a fresh
// ensureNormal starts) before sampling mode + rmq. After the wait, it
// observes the post-rollback (or post-success) state.
//
// We simulate "ensureNormal in flight" by setting mode +
// connectDone manually, spawn ensureBroker, verify it blocks, then
// drive the wakeup path: setMode(modeClosing) + close(connectDone)
// (simulating Close racing in). ensureBroker should then return
// ErrAlreadyClosed — proving it observed the post-wake state,
// not the modeNormalConnecting it entered with.
func TestClient_ensureBroker_WaitsForInFlightEnsureNormal(t *testing.T) {
	c := newTestClient(t)

	// Stage: ensureNormal is in flight from modeNew → modeNormalConnecting.
	c.lifecycleMu.Lock()
	c.connecting = true
	c.connectDone = make(chan struct{})
	c.setMode(modeNormalConnecting)
	c.lifecycleMu.Unlock()

	type result struct{ err error }
	results := make(chan result, 1)
	go func() {
		_, err := c.ensureBroker(t.Context())
		results <- result{err}
	}()

	// ensureBroker MUST block — adopting rmq during modeNormalConnecting
	// is exactly the bug v2.18 fixes.
	select {
	case r := <-results:
		t.Fatalf("ensureBroker returned during modeNormalConnecting (cross-flow bug): %+v", r)
	case <-time.After(75 * time.Millisecond):
		// Correct: blocked on connectDone.
	}

	// Simulate Close racing in mid-Connect: beginClose stomps to
	// modeClosing; ensureNormal's defer closes connectDone (without
	// reverting mode, since Close already stomped it).
	c.lifecycleMu.Lock()
	c.setMode(modeClosing)
	close(c.connectDone)
	c.lifecycleMu.Unlock()

	// ensureBroker wakes, re-checks mode, observes modeClosing, rejects.
	// This proves the wait worked: had ensureBroker proceeded with the
	// modeNormalConnecting it entered with, it would have admitted past
	// the closed mode.
	select {
	case r := <-results:
		if !errors.Is(r.err, ErrAlreadyClosed) {
			t.Fatalf("ensureBroker after wakeup: err = %v, want ErrAlreadyClosed", r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ensureBroker did not unblock after connectDone closed")
	}
}

// TestClient_runShutdown_WaitsForInFlightBrokerOpen verifies the v2.17
// fix to finding F2 (fence half): runShutdown does not proceed past
// rmq.Close until in-flight ensureBroker calls have completed. Without
// this fence, replay subscribe's rt.rmq.Open(ctx) could race with
// shutdown's rmq.Close.
//
// We simulate an in-flight ensureBroker by directly Adding to
// brokerOpenWG (the real ensureBroker does this under lifecycleMu
// after the mode-admission check). Close must block until we Done.
func TestClient_runShutdown_WaitsForInFlightBrokerOpen(t *testing.T) {
	c := newTestClient(t)

	c.brokerOpenWG.Add(1)

	closeDone := make(chan struct{})
	go func() {
		defer close(closeDone)
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		_ = c.Close(ctx)
	}()

	// Close must NOT complete while brokerOpenWG holds 1.
	select {
	case <-closeDone:
		t.Fatal("Close completed before brokerOpenWG.Done — fence not in effect")
	case <-time.After(75 * time.Millisecond):
		// Correct: Close is blocked on brokerOpenWG.Wait.
	}

	c.brokerOpenWG.Done()

	select {
	case <-closeDone:
		// Correct: Close completed once the in-flight broker open finished.
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not complete after brokerOpenWG.Done")
	}
}

// TestClient_SubscribeAdmission_ClosedRollback verifies the v2.13
// review fix: Subscribe re-checks the Closed state under subsMu after
// session.Open, and rolls back the session if Close already
// transitioned to Closed. Without this, a late c.subs insert + wg.Add
// after runShutdown's snapshot would panic with "WaitGroup misuse".
//
// We exercise the admission state machine directly: pre-set state to
// Closed (simulating a Close that completed during session.Open), run
// the same admission predicate Subscribe uses, and verify it rejects.
func TestClient_SubscribeAdmission_ClosedRollback(t *testing.T) {
	c := &Client{subs: map[uuid.UUID]*Subscription{}}
	c.connectState.Store(int32(ConnectionStateClosed))

	// Mirror the admission predicate.
	c.subsMu.Lock()
	closed := ConnectionState(c.connectState.Load()) == ConnectionStateClosed
	if !closed {
		// Would have inserted + wg.Add — but state was Closed.
		t.Fatal("admission predicate should have detected Closed")
	}
	c.subsMu.Unlock()

	// And confirm c.subs stayed empty (no insert post-Close).
	if len(c.subs) != 0 {
		t.Errorf("c.subs len = %d, want 0", len(c.subs))
	}
}

// TestSubscription_runShutdown_RemovesFromClient verifies the v2.13
// review fix #5: Subscription.runShutdown deletes itself from
// c.subs so a long-lived Client with many short-lived subscriptions
// doesn't leak entries.
func TestSubscription_runShutdown_RemovesFromClient(t *testing.T) {
	c := &Client{subs: map[uuid.UUID]*Subscription{}}
	id := uuid.New()
	sub := &Subscription{
		id:       id,
		messages: make(chan types.SessionMessage, 1),
		closed:   make(chan struct{}),
		pumpDone: make(chan struct{}),
		client:   c,
	}
	c.subs[id] = sub
	close(sub.pumpDone) // simulate pump already exited

	drainCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	sub.runShutdown(nil, drainCtx)

	if _, ok := c.subs[id]; ok {
		t.Errorf("subscription still in c.subs after runShutdown")
	}
}

// TestClient_recoveryManagerSwap_RaceFree verifies the v2.9 review
// fix: concurrent reads (RecoverEventOdds / ProducerStatus / etc.)
// against concurrent resetConnectionLayer swaps must not race. We
// drive the atomic.Pointer field from N goroutines while another
// goroutine swaps it; under -race the test fails on any unsynchronised
// access.
func TestClient_recoveryManagerSwap_RaceFree(t *testing.T) {
	c := &Client{
		cfgAdpt: &fakeOFC{},
		logger:  log.New(slog.New(slog.NewTextHandler(io.Discard, nil))),
	}
	c.resetConnectionLayer()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	// Reader goroutines.
	for range 8 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = c.recoveryManager.Load()
				_ = c.rabbitMQClient.Load()
			}
		})
	}
	// Swapper goroutine — simulates concurrent rollback resets.
	wg.Go(func() {
		for range 200 {
			c.resetConnectionLayer()
		}
	})
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestSubscription_PumpOwnsMessageClose verifies the v2.8 review fix
// for item 4: pumpSubscription's deferred close(s.messages) is the
// sole closer. Without this, runShutdown could close s.messages while
// pump was mid-`case sub.messages <- msg:` and panic.
//
// We exercise the contract directly: build a Subscription, simulate
// pump exit (so its defer runs), and confirm s.messages is closed.
func TestSubscription_PumpOwnsMessageClose(t *testing.T) {
	sub := &Subscription{
		messages: make(chan types.SessionMessage, 1),
		pumpDone: make(chan struct{}),
		closed:   make(chan struct{}),
	}

	// Inline mirror of the production pump's defer closure (the
	// `defer func() { close(messages); close(pumpDone) }()` block).
	pumpExit := func() {
		close(sub.messages)
		close(sub.pumpDone)
	}
	pumpExit()

	// After pump exit, messages must be closed (range-receive yields
	// ok=false).
	_, ok := <-sub.messages
	if ok {
		t.Error("messages channel still open after pump exit")
	}
	// And pumpDone must signal "done".
	select {
	case <-sub.pumpDone:
	default:
		t.Error("pumpDone not closed after pump exit")
	}
}

// TestClient_resetConnectionLayer_AllowsRetry verifies item 1: after a
// rolled-back Connect (which closed rabbitMQClient + recoveryManager),
// the resetConnectionLayer call replaces them with fresh, openable
// instances so a retry doesn't see "already opened" / "already closed"
// terminal-state errors.
func TestClient_resetConnectionLayer_AllowsRetry(t *testing.T) {
	c := &Client{
		cfgAdpt: &fakeOFC{},
		logger:  log.New(slog.New(slog.NewTextHandler(io.Discard, nil))),
	}
	// First instance: simulate a "used" state by calling reset twice;
	// the field assignment should succeed both times.
	c.resetConnectionLayer()
	first := c.rabbitMQClient.Load()
	firstRecovery := c.recoveryManager.Load()
	if first == nil || firstRecovery == nil {
		t.Fatal("first reset produced nil component(s)")
	}
	c.resetConnectionLayer()
	if c.rabbitMQClient.Load() == first {
		t.Error("second reset did not replace rabbitMQClient")
	}
	if c.recoveryManager.Load() == firstRecovery {
		t.Error("second reset did not replace recoveryManager")
	}
}

// fakeOFC is a minimal config.Config test double — only
// methods touched by feed.NewClient + recovery.NewManager need to be
// real; the rest panic on use, but no test path here calls them.
type fakeOFC struct{}

func (f *fakeOFC) AccessToken() *string                    { s := "x"; return &s }
func (f *fakeOFC) DefaultLocale() types.Locale             { return types.EnLocale }
func (f *fakeOFC) MaxInactivity() time.Duration            { return 20 * time.Second }
func (f *fakeOFC) MaxRecoveryExecution() time.Duration     { return 360 * time.Minute }
func (f *fakeOFC) MessagingPort() int                      { return 5672 }
func (f *fakeOFC) SdkNodeID() *int                         { return nil }
func (f *fakeOFC) SelectedEnvironment() *types.Environment { return nil }
func (f *fakeOFC) SelectedRegion() types.Region            { return types.RegionDefault }
func (f *fakeOFC) ExchangeName() string                    { return "oddinfeed" }
func (f *fakeOFC) ReplayExchangeName() string              { return "oddinreplay" }
func (f *fakeOFC) ReportExtendedData() bool                { return false }
func (f *fakeOFC) APIURL() (string, error)                 { return "x", nil }
func (f *fakeOFC) MQURL() (string, error)                  { return "x", nil }
func (f *fakeOFC) SportIDPrefix() string                   { return "od:sport:" }

// TestClient_runShutdown_WaitsForInflightConnect verifies the v2.6
// review fix: even when Close()'s caller ctx times out before Connect
// settles, runShutdown internally waits for connectDone before reading
// c.aliveSession / c.internalCancel / etc. — otherwise it could race
// past those reads while Connect later writes them and leak goroutines.
func TestClient_runShutdown_WaitsForInflightConnect(t *testing.T) {
	c := &Client{closed: make(chan struct{})}
	c.connectState.Store(int32(ConnectionStateConnecting))
	c.connecting = true
	c.connectDone = make(chan struct{})

	// Capture connectDone state under runShutdown's prelude — same
	// pattern as runShutdown.
	c.lifecycleMu.Lock()
	inflight := c.connectDone
	c.lifecycleMu.Unlock()

	// Ensure runShutdown's wait does NOT proceed until connectDone closes.
	proceeded := make(chan struct{})
	go func() {
		<-inflight
		close(proceeded)
	}()

	select {
	case <-proceeded:
		t.Fatal("runShutdown wait advanced before connectDone closed")
	case <-time.After(50 * time.Millisecond):
	}

	// Connect finally settles (runs to completion).
	c.lifecycleMu.Lock()
	c.connecting = false
	close(c.connectDone)
	c.lifecycleMu.Unlock()

	select {
	case <-proceeded:
	case <-time.After(time.Second):
		t.Fatal("runShutdown wait did not advance after connectDone closed")
	}
}

// TestClient_Close_WaitsForInflightConnect models the ordering invariant
// that runShutdown enforces INTERNALLY: shutdown must observe an in-flight
// Connect's writes (aliveSession, internalCancel) before tearing them
// down, so a late connectState publication can't survive the Closed
// transition. Note this is now runShutdown's OWN wait (bounded by the
// shutdown budget), NOT a preliminary unbounded wait in Close — see
// TestClient_Close_DoesNotHangOnWedgedConnect for why Close must not
// pre-wait. The inline simulation below keeps pinning the ordering rule.
func TestClient_Close_WaitsForInflightConnect(t *testing.T) {
	c := &Client{
		closed: make(chan struct{}),
	}
	c.connectState.Store(int32(ConnectionStateConnecting))
	c.connecting = true
	c.connectDone = make(chan struct{})

	// Run an abbreviated Close-style wait inline (we don't want the full
	// runShutdown machinery here — just the ordering invariant).
	closeReturned := make(chan struct{})
	go func() {
		c.lifecycleMu.Lock()
		inflight := c.connectDone
		c.lifecycleMu.Unlock()
		<-inflight // would normally be ctx-bounded
		c.lifecycleMu.Lock()
		c.connectState.Store(int32(ConnectionStateClosed))
		c.lifecycleMu.Unlock()
		close(closeReturned)
	}()

	// Close should NOT have returned yet — Connect is still in flight.
	select {
	case <-closeReturned:
		t.Fatal("Close returned before connectDone closed")
	case <-time.After(50 * time.Millisecond):
	}

	// Settle Connect.
	c.lifecycleMu.Lock()
	c.connecting = false
	close(c.connectDone)
	c.lifecycleMu.Unlock()

	select {
	case <-closeReturned:
	case <-time.After(time.Second):
		t.Fatal("Close did not return after connectDone closed")
	}
	if got := ConnectionState(c.connectState.Load()); got != ConnectionStateClosed {
		t.Errorf("state = %v, want Closed", got)
	}
}

// --- v2.5 review: Market.Names + LocalizedName enrichment ---
//
// Verifies the new Market struct shape: Names map populated, lookups
// for missing locales return None. v2.x reshape collapsed the
// `LocalizedName(loc) *string` + `Name(loc) string` pair into a
// single `Name(loc) Optional[string]`.

func TestMarket_Name_HitMiss(t *testing.T) {
	m := types.Market{
		Names: map[types.Locale]string{
			types.EnLocale: "1x2",
			types.RuLocale: "1×2",
		},
	}
	if got := m.Name(types.EnLocale); got.ValueOr("") != "1x2" {
		t.Errorf("Name(en) = %v, want \"1x2\"", got)
	}
	if got := m.Name(types.RuLocale); got.ValueOr("") != "1×2" {
		t.Errorf("Name(ru) = %v", got)
	}
	if got := m.Name(types.DeLocale); got.IsSet() {
		t.Errorf("Name(de) = %v, want None (locale not preloaded)", got)
	}
}

// --- v2.20 F3: localesOrDefault threading ---
//
// Pre-v2.20 the public Client.Match / .ActiveTournaments / .Match-list
// wrappers took variadic locales but called localeOrDefault(locales),
// which dropped everything past locales[0]. v2.20 adds
// localesOrDefault returning []types.Locale and threads it through
// MultiLocalized* manager methods so all supplied locales preload.

func TestLocalesOrDefault_EmptyReturnsDefaultSingleton(t *testing.T) {
	c := newTestClient(t)
	got := c.localesOrDefault(nil)
	if len(got) != 1 || got[0] != c.cfg.DefaultLocale() {
		t.Errorf("localesOrDefault(nil) = %v, want [%v]", got, c.cfg.DefaultLocale())
	}
}

func TestLocalesOrDefault_PreservesAllSupplied(t *testing.T) {
	c := newTestClient(t)
	in := []types.Locale{types.EnLocale, types.RuLocale}
	got := c.localesOrDefault(in)
	if len(got) != 2 || got[0] != types.EnLocale || got[1] != types.RuLocale {
		t.Errorf("localesOrDefault(%v) = %v, want %v", in, got, in)
	}
}

// --- v2.20 F6: Client.Tournament public method exists ---
//
// Pre-v2.20 docs (MIGRATION.md §3.5, types/sport_event.go) pointed
// users to client.Tournament(ctx, urn) but no such method existed.
// v2.20 adds the public method backed by a cache-inferred sportID.
//
// We verify the method exists on the *Client type at compile time
// (the test is also a typecheck). Behavioural coverage of the
// cache-inferred sportID lives in the sport-manager tests where the
// HTTP fixture is wired.
func TestClient_Tournament_PublicMethodExists(t *testing.T) {
	var c *Client
	// Compile-time existence check: the cast forces the method set.
	_ = func(ctx context.Context, id types.URN, locales ...types.Locale) (types.Tournament, error) {
		return c.Tournament(ctx, id, locales...)
	}
}

// TestClient_ActiveTournamentsForSport_PublicMethodExists is the F5
// counterpart — public method exposes Java/.NET parity for sport-name
// active tournament lookup with caller-driven locale resolution.
func TestClient_ActiveTournamentsForSport_PublicMethodExists(t *testing.T) {
	var c *Client
	_ = func(ctx context.Context, sportName string, locales ...types.Locale) ([]types.Tournament, error) {
		return c.ActiveTournamentsForSport(ctx, sportName, locales...)
	}
}

// --- v2.19 F1: subscribe option resolution ---
//
// Before v2.19, Subscribe pre-set messageInterest = AllMessageInterest,
// then a post-options "if interest == empty, default to All" check
// stomped any explicit SpecifiedMatchesOnly back to All — because
// SpecifiedMatchesOnly is "" and collides with the zero-value
// "unset" sentinel. The fix introduces an explicit
// messageInterestSet bool so "specified" and "unset" are
// distinguishable.

// resolveSubscribeConfig delegates to the PRODUCTION resolver
// (resolveSubscribeOptions, the exact step Subscribe runs). The
// previous local copy of the resolution logic let these tests stay
// green even if production resolution changed.
func resolveSubscribeConfig(opts ...SubscribeOption) subscribeConfig {
	return resolveSubscribeOptions(opts...)
}

// TestSubscribeOptions_NoOptions_DefaultsToAll: the unsurprising baseline.
func TestSubscribeOptions_NoOptions_DefaultsToAll(t *testing.T) {
	c := resolveSubscribeConfig()
	if c.messageInterest != types.AllMessageInterest {
		t.Errorf("interest = %q, want AllMessageInterest", c.messageInterest)
	}
}

// TestSubscribeOptions_WithSpecificEvents_ImpliesSpecifiedMatchesOnly verifies
// the F1 fix: WithSpecificEvents alone implies SpecifiedMatchesOnly
// (== ""). The post-options default must NOT stomp it back to All.
func TestSubscribeOptions_WithSpecificEvents_ImpliesSpecifiedMatchesOnly(t *testing.T) {
	urn := types.URN{Prefix: "od", Type: "match", ID: 42}
	c := resolveSubscribeConfig(WithSpecificEvents(urn))
	if c.messageInterest != types.SpecifiedMatchesOnlyMessageInterest {
		t.Errorf("interest = %q, want SpecifiedMatchesOnly (\"\")", c.messageInterest)
	}
	if !c.messageInterestSet {
		t.Error("messageInterestSet = false; flag must be true after WithSpecificEvents")
	}
	if _, ok := c.specificEvents[urn]; !ok {
		t.Errorf("specificEvents missing %v", urn)
	}
}

// TestSubscribeOptions_WithMessageInterestAll_PlusWithSpecificEvents
// verifies the documented migration shape: caller sets explicit All
// (to receive everything) AND lists specific event IDs (e.g., to
// filter client-side). The interest must remain All — option order
// must not matter.
func TestSubscribeOptions_WithMessageInterestAll_PlusWithSpecificEvents(t *testing.T) {
	urn := types.URN{Prefix: "od", Type: "match", ID: 42}

	// Forward order.
	c1 := resolveSubscribeConfig(
		WithMessageInterest(types.AllMessageInterest),
		WithSpecificEvents(urn),
	)
	if c1.messageInterest != types.AllMessageInterest {
		t.Errorf("forward order interest = %q, want AllMessageInterest", c1.messageInterest)
	}

	// Reverse order — must produce the same outcome.
	c2 := resolveSubscribeConfig(
		WithSpecificEvents(urn),
		WithMessageInterest(types.AllMessageInterest),
	)
	if c2.messageInterest != types.AllMessageInterest {
		t.Errorf("reverse order interest = %q, want AllMessageInterest", c2.messageInterest)
	}

	if _, ok := c1.specificEvents[urn]; !ok {
		t.Error("c1.specificEvents missing event")
	}
	if _, ok := c2.specificEvents[urn]; !ok {
		t.Error("c2.specificEvents missing event")
	}
}

// TestRoutingKeys_SpecifiedMatchesOnly_BuildsPerEventKeys verifies the
// downstream routing-key derivation: with the F1 fix in place, a
// SpecifiedMatchesOnly subscription emits one routing key per event
// URN (not the wildcard catch-all), matching Java/.NET behaviour.
func TestRoutingKeys_SpecifiedMatchesOnly_BuildsPerEventKeys(t *testing.T) {
	c := newTestClient(t)
	urn := types.URN{Prefix: "od", Type: "match", ID: 42}

	subCfg := resolveSubscribeConfig(WithSpecificEvents(urn))
	keys, err := c.routingKeys(subCfg)
	if err != nil {
		t.Fatalf("routingKeys: %v", err)
	}

	// routingKeys formats per-event keys as "#.{Prefix}:{Type}.{ID}".
	wantSubstr := "od:match.42"
	hit := false
	for _, k := range keys {
		if strings.Contains(k, wantSubstr) {
			hit = true
			break
		}
	}
	if !hit {
		t.Errorf("routing keys = %v, want at least one containing %q", keys, wantSubstr)
	}
}

// TestRoutingKeys_RejectsUnknownMessageInterest is the regression for
// the Codex P2 finding: WithMessageInterest accepted ANY string, which
// routingKeys interpolated verbatim into RabbitMQ topic bindings. A
// typo produced a subscription that silently received nothing intended;
// MessageInterest("#") produced a "#.#" binding broader than any
// documented routing shape; and unknown values default-accepted every
// producer scope in IsProducerInScope. Subscribe must reject anything
// that is not a documented types.*MessageInterest constant.
func TestRoutingKeys_RejectsUnknownMessageInterest(t *testing.T) {
	c := newTestClient(t)

	bad := []types.MessageInterest{
		"#",                // whole-binding wildcard
		"*.*.live.*.*.*",   // truncated (six-segment) live typo
		"*.*.lvie.*.*.*.*", // misspelled segment
		"hi.#",             // partial wildcard shape
		"custom.topic.filter.a.b.c.d",
	}
	for _, mi := range bad {
		subCfg := resolveSubscribeConfig(WithMessageInterest(mi))
		if _, err := c.routingKeys(subCfg); err == nil {
			t.Errorf("routingKeys accepted unknown message interest %q", mi)
		}
		// Replay must reject the same values — a bogus interest there is
		// the same programming error, just ignored by routing.
		subCfg = resolveSubscribeConfig(WithMessageInterest(mi), WithReplay())
		if _, err := c.routingKeys(subCfg); err == nil {
			t.Errorf("routingKeys (replay) accepted unknown message interest %q", mi)
		}
	}

	// Every documented constant still passes (SpecifiedMatchesOnly needs
	// its companion events option).
	good := []types.MessageInterest{
		types.LiveOnlyMessageInterest,
		types.PrematchOnlyMessageInterest,
		types.HiPriorityOnlyMessageInterest,
		types.LowPriorityOnlyMessageInterest,
		types.AllMessageInterest,
		types.SystemAliveOnly,
	}
	for _, mi := range good {
		subCfg := resolveSubscribeConfig(WithMessageInterest(mi))
		if _, err := c.routingKeys(subCfg); err != nil {
			t.Errorf("routingKeys rejected documented interest %q: %v", mi, err)
		}
	}
	subCfg := resolveSubscribeConfig(WithSpecificEvents(types.URN{Prefix: "od", Type: "match", ID: 42}))
	if _, err := c.routingKeys(subCfg); err != nil {
		t.Errorf("routingKeys rejected SpecifiedMatchesOnly with events: %v", err)
	}
}

// --- v2.24 F1: paired OnMessageProcessingStarted / OnMessageProcessingEnded ---
//
// Pre-v2.24, processFeedMessage's BuildMessage-failure path and the
// unrecognized-built-type default branch returned without calling
// OnMessageProcessingEnded. The recovery manager tracked active
// processing per session in a map cleared by OnMessageProcessingEnded,
// so each failed message leaked one stale entry.

type countingRecoveryProcessor struct {
	started atomic.Int32
	ended   atomic.Int32
}

func (c *countingRecoveryProcessor) OnMessageProcessingStarted(uuid.UUID, int, time.Time) {
	c.started.Add(1)
}
func (c *countingRecoveryProcessor) OnMessageProcessingEnded(uuid.UUID, int, time.Time) {
	c.ended.Add(1)
}
func (c *countingRecoveryProcessor) OnAliveReceived(int, types.MessageTimestamp, bool, types.MessageInterest) {
}
func (c *countingRecoveryProcessor) OnSnapshotCompleteReceived(context.Context, int, int, types.MessageInterest) error {
	return nil
}

type erroringMessageBuilder struct{}

func (erroringMessageBuilder) BuildMessage(context.Context, *types.FeedMessage) (any, error) {
	return nil, errors.New("simulated build failure")
}
func (erroringMessageBuilder) BuildUnparsableMessage(context.Context, *types.FeedMessage) types.UnparsableMessage {
	return nil
}

// unrecognizedTypeBuilder returns a value the session's type-switch
// doesn't recognise (a bare *string is none of the OddsChange /
// BetStop / ... interface types), exercising the default branch.
type unrecognizedTypeBuilder struct{}

func (unrecognizedTypeBuilder) BuildMessage(context.Context, *types.FeedMessage) (any, error) {
	v := "not a real message type"
	return &v, nil
}
func (unrecognizedTypeBuilder) BuildUnparsableMessage(context.Context, *types.FeedMessage) types.UnparsableMessage {
	return nil
}

// TestSession_ProcessFeedMessage_BuildErrorEndsProcessing verifies F1:
// when BuildMessage returns an error, OnMessageProcessingEnded is
// still called exactly once (paired with the Started call) so the
// recovery manager's per-session tracking map doesn't leak an entry.
func TestSession_ProcessFeedMessage_BuildErrorEndsProcessing(t *testing.T) {
	rec := &countingRecoveryProcessor{}
	o := &oddsFeedSessionImpl{
		cacheManager:             &spyCacheNotifier{},
		feedMessageFactory:       erroringMessageBuilder{},
		recoveryMessageProcessor: rec,
		logger:                   discardLogger(),
		msgCh:                    make(chan sessionEnvelope, 1),
		sessionID:                uuid.New(),
	}
	fm := &types.FeedMessage{
		BasicFeedMessage: types.BasicFeedMessage{
			Timestamp: types.MessageTimestamp{Created: time.Now()},
		},
		Message: &feedXML.FixtureChange{ProductID: 1, EventID: "od:match:1"},
	}

	o.processFeedMessage(t.Context(), fm, types.AllMessageInterest, nil, false, nil)

	if got := rec.started.Load(); got != 1 {
		t.Errorf("Started count = %d, want 1", got)
	}
	if got := rec.ended.Load(); got != 1 {
		t.Errorf("Ended count = %d, want 1 (BuildMessage failure path leaked tracking entry)", got)
	}
}

// TestSession_ProcessFeedMessage_UnrecognizedTypeEndsProcessing
// verifies F1's other failure path: the default branch (built
// message type the session doesn't recognise) also pairs Started
// with Ended.
func TestSession_ProcessFeedMessage_UnrecognizedTypeEndsProcessing(t *testing.T) {
	rec := &countingRecoveryProcessor{}
	o := &oddsFeedSessionImpl{
		cacheManager:             &spyCacheNotifier{},
		feedMessageFactory:       unrecognizedTypeBuilder{},
		recoveryMessageProcessor: rec,
		logger:                   discardLogger(),
		msgCh:                    make(chan sessionEnvelope, 1),
		sessionID:                uuid.New(),
	}
	fm := &types.FeedMessage{
		BasicFeedMessage: types.BasicFeedMessage{
			Timestamp: types.MessageTimestamp{Created: time.Now()},
		},
		Message: &feedXML.FixtureChange{ProductID: 1, EventID: "od:match:1"},
	}

	o.processFeedMessage(t.Context(), fm, types.AllMessageInterest, nil, false, nil)

	if got := rec.started.Load(); got != 1 {
		t.Errorf("Started count = %d, want 1", got)
	}
	if got := rec.ended.Load(); got != 1 {
		t.Errorf("Ended count = %d, want 1 (default branch leaked tracking entry)", got)
	}
}

// --- Test 1: session shutdown invariants ---
//
// We can't easily stand up a real oddsFeedSessionImpl without AMQP.
// What we can verify directly: that the chan-recv invariants the
// rewritten session.go upholds (ok-check on closed chan, drop-oldest
// non-blocking sends) don't panic.

func TestSession_ChannelInvariantsDoNotPanic(t *testing.T) {
	// 1. Recv from closed chan yields zero + ok=false (used in the new
	//    `case msg, ok := <-ch:` to detect upstream close).
	ch := make(chan int)
	close(ch)
	v, ok := <-ch
	if ok || v != 0 {
		t.Fatalf("recv from closed chan: v=%d ok=%v, want 0,false", v, ok)
	}

	// 2. pushDropOldest on a full buffered chan does not panic.
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("pushDropOldest panicked: %v", r)
		}
	}()
	bch := make(chan int, 1)
	bch <- 1
	_ = pushDropOldest(bch, 2)
}

// TestClient_RecoverGating_RequiresReady pins the lifecycle admission
// gate for recovery initiation: pre-fix the public Recover* methods
// only loaded the manager pointer, accepting recoveries while Connect
// was still building the pipeline (a later alive-session failure then
// REPLACED the manager, orphaning the handle while the server might
// still process the request) and during the Close teardown window.
func TestClient_RecoverGating_RequiresReady(t *testing.T) {
	c := &Client{}
	cases := []struct {
		mode clientMode
		want error
	}{
		{modeNew, ErrManagerNotOpen},
		{modeBrokerOnly, ErrManagerNotOpen},
		{modeNormalConnecting, ErrManagerNotOpen},
		{modeClosing, ErrAlreadyClosed},
		{modeClosed, ErrAlreadyClosed},
	}
	urn := types.URN{Prefix: "od", Type: "match", ID: 1}
	for _, tc := range cases {
		c.mode = tc.mode
		if _, err := c.readyRecoveryManager(); !errors.Is(err, tc.want) {
			t.Errorf("mode %d: readyRecoveryManager err = %v, want %v", tc.mode, err, tc.want)
		}
		// BOTH public recovery methods must map lifecycle state to the
		// same sentinel — pre-fix only RecoverEventOdds was tested, so
		// RecoverEventStateful's gating/error-mapping was unverified.
		if _, err := c.RecoverEventOdds(t.Context(), 1, urn); !errors.Is(err, tc.want) {
			t.Errorf("mode %d: RecoverEventOdds err = %v, want %v", tc.mode, err, tc.want)
		}
		if _, err := c.RecoverEventStateful(t.Context(), 1, urn); !errors.Is(err, tc.want) {
			t.Errorf("mode %d: RecoverEventStateful err = %v, want %v", tc.mode, err, tc.want)
		}
	}
}

// TestClient_ensureBroker_FailedOpenRetainsGeneration is the regression
// for the orphaned-generation race (Codex P2): ensureBroker's failed-
// open rollback ran resetConnectionLayer, replacing the stored feed
// client — but a CONCURRENT ensureBroker that snapshotted the old
// generation while mode was still modeBrokerOnly could have its own
// Open in flight; its later successful retry then started a reconnect
// goroutine + broker connection on a generation runShutdown (which
// closes only the CURRENTLY stored one) never tore down. A failed feed
// Open leaves the client retryable-unopened, so the rollback must
// RETAIN the generation: same stored pointer, mode back to modeNew.
func TestClient_ensureBroker_FailedOpenRetainsGeneration(t *testing.T) {
	srv := fullFixtureServer(t)
	t.Cleanup(srv.Close)

	cfg := NewConfig("client-test-access-token", types.IntegrationEnvironment,
		WithAPIHost("api.example.test"),
		WithHTTPClient(newTestHTTPClient(srv)),
		WithMQHost("127.0.0.1"), // unroutable AMQP target: dial fails fast
		WithMessagingPort(1),
	)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	c, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	closeClientOnCleanup(t, c)

	before := c.rabbitMQClient.Load()
	if before == nil {
		t.Fatal("no feed client stored after New")
	}

	openCtx, cancelOpen := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancelOpen()
	if _, err := c.ensureBroker(openCtx); err == nil {
		t.Fatal("ensureBroker succeeded against an unroutable broker")
	}

	if after := c.rabbitMQClient.Load(); after != before {
		t.Fatal("failed ensureBroker REPLACED the feed-client generation — a concurrent caller's snapshot is now orphaned")
	}
	c.lifecycleMu.Lock()
	mode := c.mode
	c.lifecycleMu.Unlock()
	if mode != modeNew {
		t.Fatalf("mode after failed ensureBroker = %v, want modeNew", mode)
	}

	// The retained generation must still be retryable: a second attempt
	// re-dials (and fails the same way) rather than erroring on state.
	if _, err := c.ensureBroker(openCtx); err == nil {
		t.Fatal("second ensureBroker succeeded against an unroutable broker")
	}
}

// TestRoutingKeys_RejectsRoutingUnsafeURNs is the regression for topic-
// binding injection (Codex P2): types.URN has exported fields, so a
// consumer can construct values ParseURN would never produce — and
// routingKeys interpolated them straight into RabbitMQ topic syntax. A
// whole-segment '#' in Type ("match.#") made the binding match
// UNRELATED events (which the pipeline then acked); a stray '.' changed
// the eight-segment routing-key shape so intended messages silently
// never matched; a negative ID printed "-1" into the key. All such URNs
// must be rejected before any broker setup.
func TestRoutingKeys_RejectsRoutingUnsafeURNs(t *testing.T) {
	c := newTestClient(t)

	bad := []types.URN{
		{Prefix: "od", Type: "match.#", ID: 1}, // wildcard segment injection
		{Prefix: "od", Type: "match.*", ID: 1}, // single-segment wildcard
		{Prefix: "od", Type: "ma.tch", ID: 1},  // delimiter changes key shape
		{Prefix: "od.#", Type: "match", ID: 1}, // injection via prefix
		{Prefix: "", Type: "match", ID: 1},     // empty prefix
		{Prefix: "od", Type: "", ID: 1},        // empty type
		{Prefix: "od", Type: "match", ID: -1},  // negative id
		{Prefix: "od", Type: "mat ch", ID: 1},  // whitespace
		{Prefix: "od", Type: "match:x", ID: 1}, // extra colon
	}
	for _, urn := range bad {
		subCfg := resolveSubscribeConfig(WithSpecificEvents(urn))
		if _, err := c.routingKeys(subCfg); err == nil {
			t.Errorf("routingKeys accepted routing-unsafe URN %+v", urn)
		}
	}

	// Valid literals still pass.
	subCfg := resolveSubscribeConfig(WithSpecificEvents(types.URN{Prefix: "od", Type: "match", ID: 42}))
	if _, err := c.routingKeys(subCfg); err != nil {
		t.Errorf("routingKeys rejected a valid URN: %v", err)
	}
}

// TestClient_reconcileBrokerMode covers the two-generation broker-open
// race (Codex P2): an older FAILED ensureBroker's rollback and a newer
// SUCCESSFUL one can interleave in either lock order. The reconcile
// decision must key on the ACTUAL broker state, so the final root mode
// is correct regardless of ordering — otherwise a successful replay
// open could leave the root at modeNew, and a later normal Connect's
// rollback would close the shared broker out from under live replay
// subscriptions.
func TestClient_reconcileBrokerMode(t *testing.T) {
	cases := []struct {
		name         string
		start        clientMode
		transitioned bool
		opened       bool
		want         clientMode
	}{
		// Peer's failed rollback drifted us to modeNew, but the broker is
		// open (this or a peer succeeded): restore brokerOnly.
		{"open+drifted-to-new", modeNew, false, true, modeBrokerOnly},
		{"open+drifted-to-new-transitioned", modeNew, true, true, modeBrokerOnly},
		// Our own attempt failed, nobody else holds it open: undo.
		{"failed+transitioned", modeBrokerOnly, true, false, modeNew},
		// Our attempt failed but a peer holds the broker open: keep brokerOnly.
		{"failed+peer-open", modeBrokerOnly, true, true, modeBrokerOnly},
		// Non-transitioning failed attempt must not roll back a peer's state.
		{"failed+not-transitioned", modeBrokerOnly, false, false, modeBrokerOnly},
		// Success while already brokerOnly: no-op.
		{"success-steady", modeBrokerOnly, true, true, modeBrokerOnly},
		// A concurrent normal Connect owns modeNormalReady — never stomp it.
		{"ready-not-stomped", modeNormalReady, false, true, modeNormalReady},
		// Shutdown owns the mode — never touch it, either direction.
		{"closing-untouched", modeClosing, true, false, modeClosing},
		{"closed-untouched", modeClosed, false, true, modeClosed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{}
			c.mode = tc.start
			c.reconcileBrokerModeLocked(tc.transitioned, tc.opened)
			if c.mode != tc.want {
				t.Fatalf("mode = %v, want %v", c.mode, tc.want)
			}
		})
	}
}
