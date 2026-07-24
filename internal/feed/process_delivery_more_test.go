package feed

import (
	"io"
	"log/slog"
	"strconv"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/oddin-gg/gosdk/internal/factory"
	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/internal/utils"
	"github.com/oddin-gg/gosdk/types"
)

const oddsChangeXML = `<odds_change timestamp="1777832981632" product="2" event_id="od:match:198314" request_id="2049987833"><odds><market id="1" status="1"><outcome id="1" odds="1.5" probabilities="0.65" active="1"/></market></odds></odds_change>`

// newConsumerForProcessDelivery wires the minimum stubs needed for
// processDelivery to run end-to-end on the success path (well-formed
// decode). messageInterest must be non-nil because the success branch
// dereferences it when building RawFeedMessage.
func newConsumerForProcessDelivery() *ChannelConsumer {
	logger := log.New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	mi := types.AllMessageInterest
	return &ChannelConsumer{
		feedMessageFactory: &factory.FeedMessageFactory{},
		logger:             logger,
		sportIDPrefix:      "od:sport:",
		messageInterest:    &mi,
	}
}

// TestProcessDelivery_WellFormed_AdmitsFeedMessage covers the happy
// path: a valid routing key + well-formed XML body produces a
// FeedMessage + RawFeedMessage envelope, no UnparsableMessage.
func TestProcessDelivery_WellFormed_AdmitsFeedMessage(t *testing.T) {
	c := newConsumerForProcessDelivery()
	d := amqp.Delivery{
		RoutingKey: "hi.pre.-.odds_change.1.od:match.198314.1",
		Body:       []byte(oddsChangeXML),
		Timestamp:  time.Unix(1_700_000_000, 0).UTC(),
	}

	qm := c.processDelivery(t.Context(), d)
	if qm == nil {
		t.Fatal("processDelivery returned nil for well-formed delivery")
	}
	if qm.UnparsableMessage != nil {
		t.Fatalf("well-formed delivery produced UnparsableMessage: %+v", qm.UnparsableMessage)
	}
	if qm.FeedMessage == nil {
		t.Fatal("FeedMessage = nil; want populated")
	}
	if qm.RawFeedMessage == nil {
		t.Fatal("RawFeedMessage = nil; want populated")
	}
	if qm.FeedMessage.RoutingKey == nil {
		t.Fatal("FeedMessage.RoutingKey = nil")
	}
	if qm.FeedMessage.RoutingKey.IsSystemRoutingKey {
		t.Errorf("RoutingKey.IsSystemRoutingKey = true on event-addressed route")
	}
}

// TestProcessDelivery_WellFormed_PopulatesTimestamps verifies the
// timestamp wiring on the success path: Sent comes from the AMQP
// delivery, Received from local clock, Created from the XML message,
// Published from local clock after decode.
func TestProcessDelivery_WellFormed_PopulatesTimestamps(t *testing.T) {
	c := newConsumerForProcessDelivery()
	amqpTS := time.Unix(1_700_000_000, 0).UTC()
	d := amqp.Delivery{
		RoutingKey: "hi.pre.-.odds_change.1.od:match.198314.1",
		Body:       []byte(oddsChangeXML),
		Timestamp:  amqpTS,
	}

	qm := c.processDelivery(t.Context(), d)
	if qm == nil || qm.FeedMessage == nil {
		t.Fatal("expected populated FeedMessage")
	}
	ts := qm.FeedMessage.Timestamp
	if !ts.Sent.Equal(amqpTS) {
		t.Errorf("Sent = %v, want %v", ts.Sent, amqpTS)
	}
	if ts.Received.IsZero() {
		t.Error("Received timestamp not set")
	}
	if ts.Created.IsZero() {
		t.Error("Created timestamp not populated from XML")
	}
	if ts.Published.IsZero() {
		t.Error("Published timestamp not set on success")
	}
}

// TestProcessDelivery_WellFormed_AMQPMissingTimestamp_FallsBackToCreated
// covers the v2.x parity fix: when the broker omits a delivery
// timestamp (zero d.Timestamp), Sent falls back to the XML message's
// Created timestamp instead of staying zero.
func TestProcessDelivery_WellFormed_AMQPMissingTimestamp_FallsBackToCreated(t *testing.T) {
	c := newConsumerForProcessDelivery()
	d := amqp.Delivery{
		RoutingKey: "hi.pre.-.odds_change.1.od:match.198314.1",
		Body:       []byte(oddsChangeXML),
		// Timestamp deliberately zero.
	}

	qm := c.processDelivery(t.Context(), d)
	if qm == nil || qm.FeedMessage == nil {
		t.Fatal("expected populated FeedMessage")
	}
	ts := qm.FeedMessage.Timestamp
	if ts.Sent.IsZero() {
		t.Error("Sent stayed zero; should have fallen back to Created")
	}
	if !ts.Sent.Equal(ts.Created) {
		t.Errorf("Sent = %v, want = Created %v (fallback)", ts.Sent, ts.Created)
	}
}

// TestProcessDelivery_EmptyBody_AdmitsUnparsable covers the empty-body
// branch: a valid routing key with no XML body produces an
// UnparsableMessage envelope (not a panic, not a silent drop).
//
// Uses a system routing key (alive) — empty / decode-failure / unknown-
// root branches don't depend on event-vs-system routing semantics for
// the UnparsableMessage admission check, and the system route lets us
// exercise the path with a zero-value FeedMessageFactory (event-
// addressed routes try to look up the entity, which needs the
// factory's wired entityFactory).
func TestProcessDelivery_EmptyBody_AdmitsUnparsable(t *testing.T) {
	c := newConsumerForProcessDelivery()
	d := amqp.Delivery{
		RoutingKey: "-.-.-.alive.-.-.-.-",
		Body:       []byte{},
		Timestamp:  time.Unix(1_700_000_000, 0).UTC(),
	}

	qm := c.processDelivery(t.Context(), d)
	if qm == nil {
		t.Fatal("empty body produced nil queueMessage")
	}
	if qm.UnparsableMessage == nil {
		t.Error("empty body did not produce UnparsableMessage")
	}
	if qm.FeedMessage != nil || qm.RawFeedMessage != nil {
		t.Errorf("empty body produced FeedMessage/RawFeedMessage: %+v / %+v", qm.FeedMessage, qm.RawFeedMessage)
	}
}

// TestProcessDelivery_DecodeFailure_AdmitsUnparsable covers the
// decode-error branch: a valid routing key with an unparseable XML
// body produces an UnparsableMessage envelope.
func TestProcessDelivery_DecodeFailure_AdmitsUnparsable(t *testing.T) {
	c := newConsumerForProcessDelivery()
	d := amqp.Delivery{
		RoutingKey: "-.-.-.alive.-.-.-.-",
		Body:       []byte("not<>actually valid xml"),
		Timestamp:  time.Unix(1_700_000_000, 0).UTC(),
	}

	qm := c.processDelivery(t.Context(), d)
	if qm == nil {
		t.Fatal("decode failure produced nil queueMessage")
	}
	if qm.UnparsableMessage == nil {
		t.Error("decode failure did not produce UnparsableMessage")
	}
	if qm.FeedMessage != nil || qm.RawFeedMessage != nil {
		t.Errorf("decode failure produced FeedMessage/RawFeedMessage: %+v / %+v", qm.FeedMessage, qm.RawFeedMessage)
	}
}

// TestProcessDelivery_UnknownRootElement_AdmitsUnparsable covers the
// ErrUnknownMessage branch: well-formed XML but with a root element
// the decoder doesn't recognise produces an UnparsableMessage.
func TestProcessDelivery_UnknownRootElement_AdmitsUnparsable(t *testing.T) {
	c := newConsumerForProcessDelivery()
	d := amqp.Delivery{
		RoutingKey: "-.-.-.alive.-.-.-.-",
		Body:       []byte(`<unknown_root timestamp="1700000000" product="1" event_id="od:match:1"/>`),
		Timestamp:  time.Unix(1_700_000_000, 0).UTC(),
	}

	qm := c.processDelivery(t.Context(), d)
	if qm == nil {
		t.Fatal("unknown root produced nil queueMessage")
	}
	if qm.UnparsableMessage == nil {
		t.Error("unknown root did not produce UnparsableMessage")
	}
}

// TestProcessDelivery_FixtureChange_SameTimestampDifferentChangeType_BothAdmitted
// pins the contract that the SDK does NOT deduplicate fixture_change
// envelopes by (product, event_id, timestamp_ms). Java and .NET SDKs
// collapse them on that key (their dedupe key omits change_type), so a
// same-ms pair like [DateTime(2), StreamURL(106)] silently drops one
// message — Tipsport hit this on Valhalla Cup events when smoke-master
// emitted both within the same millisecond (CORE-3504). gosdk has no
// such filter, and this test makes sure none gets introduced.
//
// Both deliveries must:
//   - return a non-nil QueueMessage with FeedMessage + RawFeedMessage populated
//   - have no UnparsableMessage envelope
//   - decode to a *feedXML.FixtureChange carrying the original ChangeType
func TestProcessDelivery_FixtureChange_SameTimestampDifferentChangeType_BothAdmitted(t *testing.T) {
	c := newConsumerForProcessDelivery()
	const route = "hi.pre.-.fixture_change.1.od:match.198314.1"
	const sharedTimestampMs = "1777832981632"
	amqpTS := time.Unix(1_700_000_000, 0).UTC()

	mkDelivery := func(changeType feedXML.FixtureChangeType) amqp.Delivery {
		body := `<fixture_change timestamp="` + sharedTimestampMs +
			`" product="2" event_id="od:match:198314" change_type="` +
			strconv.Itoa(int(changeType)) + `"/>`
		return amqp.Delivery{
			RoutingKey: route,
			Body:       []byte(body),
			Timestamp:  amqpTS,
		}
	}

	wants := []feedXML.FixtureChangeType{
		feedXML.FixtureChangeTypeDateTime,  // 2
		feedXML.FixtureChangeTypeStreamURL, // 106
	}

	for _, want := range wants {
		qm := c.processDelivery(t.Context(), mkDelivery(want))
		if qm == nil {
			t.Fatalf("change_type=%d: processDelivery returned nil — message was silently dropped", want)
		}
		if qm.UnparsableMessage != nil {
			t.Fatalf("change_type=%d: produced UnparsableMessage: %+v", want, qm.UnparsableMessage)
		}
		if qm.FeedMessage == nil || qm.RawFeedMessage == nil {
			t.Fatalf("change_type=%d: FeedMessage=%v / RawFeedMessage=%v, want both populated",
				want, qm.FeedMessage, qm.RawFeedMessage)
		}
		fc, ok := qm.FeedMessage.Message.(*feedXML.FixtureChange)
		if !ok {
			t.Fatalf("change_type=%d: FeedMessage.Message = %T, want *feedXML.FixtureChange",
				want, qm.FeedMessage.Message)
		}
		if fc.ChangeType != want {
			t.Errorf("change_type=%d: decoded ChangeType=%d — looks like delivery was coalesced with a prior one",
				want, fc.ChangeType)
		}
	}
}

// TestProcessDelivery_OversizedBody_BoundedRetention is the regression
// for the oversized-retention hole (Codex P2): bodies over
// feedXML.MaxFeedMessageBytes were rejected by the decoder, but the
// resulting UnparsableMessage retained the COMPLETE d.Body — a slow
// consumer with the default 256-slot public buffer could pin gigabytes
// of upstream-controlled bytes (logical AMQP bodies are reassembled
// across frames, and RawMessage() clones per call without releasing the
// retained backing array). Post-fix only a copied bounded prefix is
// kept.
//
// Uses a system routing key for the same zero-value-factory reason as
// TestProcessDelivery_EmptyBody_AdmitsUnparsable.
func TestProcessDelivery_OversizedBody_BoundedRetention(t *testing.T) {
	c := newConsumerForProcessDelivery()

	oversized := make([]byte, feedXML.MaxFeedMessageBytes+1024)
	copy(oversized, "<odds_change ") // looks XML-ish; rejected on size before parsing
	d := amqp.Delivery{
		RoutingKey: "-.-.-.alive.-.-.-.-",
		Body:       oversized,
	}

	qm := c.processDelivery(t.Context(), d)
	if qm == nil || qm.UnparsableMessage == nil {
		t.Fatal("expected UnparsableMessage for oversized body")
	}
	raw := qm.UnparsableMessage.RawMessage()
	if len(raw) == 0 {
		t.Fatal("retained prefix is empty; want bounded diagnostic prefix")
	}
	if len(raw) > oversizedRetainBytes {
		t.Fatalf("retained %d bytes of an oversized body, want <= %d (bounded prefix)", len(raw), oversizedRetainBytes)
	}
	if string(raw[:13]) != "<odds_change " {
		t.Fatalf("retained prefix %q does not match body start", raw[:13])
	}

	// Sub-cap malformed bodies keep their full content — the bound
	// applies ONLY to the oversized-rejection path.
	small := []byte("<not-valid-xml")
	qm = c.processDelivery(t.Context(), amqp.Delivery{
		RoutingKey: "-.-.-.alive.-.-.-.-",
		Body:       small,
	})
	if qm == nil || qm.UnparsableMessage == nil {
		t.Fatal("expected UnparsableMessage for malformed sub-cap body")
	}
	if got := qm.UnparsableMessage.RawMessage(); string(got) != string(small) {
		t.Fatalf("sub-cap unparsable body = %q, want full original %q", got, small)
	}
}

// TestProcessDelivery_OversizedBody_MalformedRoute_BoundedRetention is the
// regression for the P1 hole: routing-key parsing runs BEFORE
// feedXML.Decode, so the malformed-route branch never reached the size
// gate and retained the ENTIRE d.Body — an oversized-plus-malformed-route
// delivery pinned unbounded upstream-controlled bytes (defeating the 8 MiB
// boundary the decode path enforces). The combination
// (oversized body AND unparseable route) was previously untested. Post-fix
// the same bounded, COPIED prefix is retained on this path too, and the
// oversized backing array is not held.
func TestProcessDelivery_OversizedBody_MalformedRoute_BoundedRetention(t *testing.T) {
	c := newConsumerForProcessDelivery()

	oversized := make([]byte, feedXML.MaxFeedMessageBytes+1024)
	copy(oversized, "<odds_change ")
	d := amqp.Delivery{
		RoutingKey: "this.is.not.a.valid.route", // != 8 dot-parts → parseRoute fails
		Body:       oversized,
	}

	qm := c.processDelivery(t.Context(), d)
	if qm == nil || qm.UnparsableMessage == nil {
		t.Fatal("expected UnparsableMessage for malformed route + oversized body")
	}
	raw := qm.UnparsableMessage.RawMessage()
	if len(raw) == 0 {
		t.Fatal("retained prefix is empty; want bounded diagnostic prefix")
	}
	if len(raw) > oversizedRetainBytes {
		t.Fatalf("malformed-route path retained %d bytes of an oversized body, want <= %d", len(raw), oversizedRetainBytes)
	}
	// The retained slice must be a COPY, not a re-slice of the oversized
	// backing array (which would keep the whole allocation alive): a copy
	// of a <=64 KiB prefix has cap well under the original length.
	if cap(raw) >= len(oversized) {
		t.Fatalf("retained slice cap=%d still spans the oversized backing array (len=%d); body not copied", cap(raw), len(oversized))
	}
}

// TestValidateDecodedMessage is the regression for required-attribute
// validation (Codex P2): XML decode checks only syntax, so a MISSING
// attribute becomes a zero value that downstream state/cache/recovery
// processing acts on. These must be rejected (→ UnparsableMessage) so
// they are ACKed but never processed under an unvalidated identity.
func TestValidateDecodedMessage(t *testing.T) {
	sub0, sub1, sub9 := 0, 1, 9
	eventRoute := &types.RoutingKeyInfo{FullRoutingKey: "r", EventID: &types.URN{Prefix: "od", Type: "match", ID: 1}}
	sysRoute := &types.RoutingKeyInfo{FullRoutingKey: "s", IsSystemRoutingKey: true}

	ts := feedXML.MessageWithTimestamp{Timestamp: utils.Timestamp(time.UnixMilli(1_700_000_000_000))}
	// oc builds an otherwise-valid odds_change (event id, product, and
	// timestamp all present) so a case can vary a single required
	// attribute in isolation.
	oc := func(mut func(*feedXML.OddsChange)) *feedXML.OddsChange {
		m := &feedXML.OddsChange{MessageWithTimestamp: ts, EventID: "od:match:1", ProductID: 2}
		if mut != nil {
			mut(m)
		}
		return m
	}
	mkt := func(id int, outcomeIDs ...string) *feedXML.MarketWithOutcome {
		m := &feedXML.MarketWithOutcome{MarketAttributes: feedXML.MarketAttributes{ID: id}}
		for _, oid := range outcomeIDs {
			m.Outcomes = append(m.Outcomes, feedXML.Outcome{ID: oid})
		}
		return m
	}

	cases := []struct {
		name    string
		rk      *types.RoutingKeyInfo
		msg     types.BasicMessage
		wantErr bool
	}{
		{"alive missing subscribed", sysRoute, &feedXML.Alive{MessageWithTimestamp: ts, ProductID: 1}, true},
		{"alive subscribed out of range", sysRoute, &feedXML.Alive{MessageWithTimestamp: ts, ProductID: 1, Subscribed: &sub9}, true},
		{"alive subscribed 0", sysRoute, &feedXML.Alive{MessageWithTimestamp: ts, ProductID: 1, Subscribed: &sub0}, false},
		{"alive subscribed 1", sysRoute, &feedXML.Alive{MessageWithTimestamp: ts, ProductID: 1, Subscribed: &sub1}, false},
		{"alive missing product", sysRoute, &feedXML.Alive{MessageWithTimestamp: ts, Subscribed: &sub1}, true},
		{"alive missing timestamp", sysRoute, &feedXML.Alive{ProductID: 1, Subscribed: &sub1}, true},
		{"snapshot zero request_id", sysRoute, &feedXML.SnapshotComplete{MessageWithTimestamp: ts, ProductID: 1, RequestID: 0}, true},
		{"snapshot negative request_id", sysRoute, &feedXML.SnapshotComplete{MessageWithTimestamp: ts, ProductID: 1, RequestID: -1}, true},
		{"snapshot positive request_id", sysRoute, &feedXML.SnapshotComplete{MessageWithTimestamp: ts, ProductID: 1, RequestID: 7}, false},
		{"snapshot missing timestamp", sysRoute, &feedXML.SnapshotComplete{ProductID: 1, RequestID: 7}, true},
		{"event missing event_id", eventRoute, oc(func(m *feedXML.OddsChange) { m.EventID = "" }), true},
		{"event unparseable event_id", eventRoute, oc(func(m *feedXML.OddsChange) { m.EventID = "not a urn" }), true},
		{"event valid, product+timestamp present", eventRoute, oc(nil), false},
		{"event missing product", eventRoute, oc(func(m *feedXML.OddsChange) { m.ProductID = 0 }), true},
		{"event missing timestamp", eventRoute, oc(func(m *feedXML.OddsChange) { m.MessageWithTimestamp = feedXML.MessageWithTimestamp{} }), true},
		{"odds_change market id zero", eventRoute, oc(func(m *feedXML.OddsChange) { m.Odds.Markets = []*feedXML.MarketWithOutcome{mkt(0, "1")} }), true},
		{"odds_change outcome id empty", eventRoute, oc(func(m *feedXML.OddsChange) { m.Odds.Markets = []*feedXML.MarketWithOutcome{mkt(1, "")} }), true},
		{"odds_change valid markets+outcomes", eventRoute, oc(func(m *feedXML.OddsChange) { m.Odds.Markets = []*feedXML.MarketWithOutcome{mkt(1, "1", "2")} }), false},
		{"bet_settlement market id zero", eventRoute, &feedXML.BetSettlement{MessageWithTimestamp: ts, EventID: "od:match:1", ProductID: 2, Markets: feedXML.MarketsWrapper{Markets: []*feedXML.MarketWithOutcome{mkt(0, "1")}}}, true},
		{"bet_cancel market id zero", eventRoute, &feedXML.BetCancel{MessageAttributes: feedXML.MessageAttributes{MessageWithTimestamp: ts, EventID: "od:match:1", Product: 2}, Markets: []*feedXML.MarketWithoutOutcome{{MarketAttributes: feedXML.MarketAttributes{ID: 0}}}}, true},
		{"bet_cancel valid", eventRoute, &feedXML.BetCancel{MessageAttributes: feedXML.MessageAttributes{MessageWithTimestamp: ts, EventID: "od:match:1", Product: 2}, Markets: []*feedXML.MarketWithoutOutcome{{MarketAttributes: feedXML.MarketAttributes{ID: 7}}}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDecodedMessage(tc.rk, tc.msg)
			if tc.wantErr != (err != nil) {
				t.Fatalf("validateDecodedMessage = %v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

// TestValidateRouteIdentity_LeadingZeroURN is the regression for the P3
// finding: ParseURN accepts and canonicalizes leading zeros
// ("od:match:007" → "od:match:7"). The route id arrives already
// canonical, so comparing the payload id as RAW TEXT rejected a
// syntactically valid message. The compare now parses the payload id and
// compares URN values.
func TestValidateRouteIdentity_LeadingZeroURN(t *testing.T) {
	rk := &types.RoutingKeyInfo{EventID: &types.URN{Prefix: "od", Type: "match", ID: 7}}

	if err := validateRouteIdentity(rk, &feedXML.OddsChange{EventID: "od:match:007"}); err != nil {
		t.Fatalf("leading-zero payload id rejected as mismatch: %v", err)
	}
	// A genuine id mismatch must still be rejected.
	if err := validateRouteIdentity(rk, &feedXML.OddsChange{EventID: "od:match:8"}); err == nil {
		t.Fatal("genuine event-id mismatch was accepted")
	}
}

// TestProcessDelivery_MissingSubscribed_AdmitsUnparsable pins the
// end-to-end consequence: an alive with no subscribed attribute becomes
// an UnparsableMessage (no decoded Message), so OnAliveReceived never
// runs with a fabricated "not subscribed".
func TestProcessDelivery_MissingSubscribed_AdmitsUnparsable(t *testing.T) {
	c := newConsumerForProcessDelivery()
	d := amqp.Delivery{
		RoutingKey: "-.-.-.alive.-.-.-.-",
		Body:       []byte(`<alive product="1" timestamp="1700000000"/>`), // no subscribed
	}
	qm := c.processDelivery(t.Context(), d)
	if qm == nil || qm.UnparsableMessage == nil {
		t.Fatal("alive without subscribed should produce an UnparsableMessage")
	}
	if qm.FeedMessage != nil {
		t.Error("alive without subscribed produced a decoded FeedMessage; validation bypassed")
	}
}
