package factory

import (
	"strings"
	"testing"
	"time"

	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/internal/producer"
	"github.com/oddin-gg/gosdk/types"
)

// minimalCfg is the smallest config.Config that satisfies the
// producer.Manager unknown-producer fallback (which needs APIURL).
type minimalCfg struct{}

func (minimalCfg) AccessToken() *string                    { s := "tok"; return &s }
func (minimalCfg) DefaultLocale() types.Locale             { return types.EnLocale }
func (minimalCfg) MaxInactivity() time.Duration            { return 20 * time.Second }
func (minimalCfg) MaxRecoveryExecution() time.Duration     { return 360 * time.Minute }
func (minimalCfg) MessagingPort() int                      { return 5672 }
func (minimalCfg) SdkNodeID() *int                         { return nil }
func (minimalCfg) SelectedEnvironment() *types.Environment { return nil }
func (minimalCfg) SelectedRegion() types.Region            { return types.RegionDefault }
func (minimalCfg) ExchangeName() string                    { return "oddinfeed" }
func (minimalCfg) ReplayExchangeName() string              { return "oddinreplay" }
func (minimalCfg) ReportExtendedData() bool                { return false }
func (minimalCfg) APIURL() (string, error)                 { return "api.example.test", nil }
func (minimalCfg) MQURL() (string, error)                  { return "", nil }
func (minimalCfg) SportIDPrefix() string                   { return "od:sport:" }

// TestBuildUnparsableMessage_SystemRoutingKey_NilSafe verifies the v2.19
// fix to finding F3: a malformed system message (alive,
// snapshot_complete, etc.) yields a RoutingKeyInfo with nil EventID
// and SportID. The previous BuildUnparsableMessage unconditionally
// dereferenced both, panicking the consumer goroutine.
func TestBuildUnparsableMessage_SystemRoutingKey_NilSafe(t *testing.T) {
	// Empty FeedMessageFactory: with the v2.19 nil-check, the system
	// routing-key path never reaches entityFactory, so nil deps are
	// fine for this test.
	f := &FeedMessageFactory{}

	rk := &types.RoutingKeyInfo{
		FullRoutingKey:     "-.-.-.alive.-.-.-.-",
		IsSystemRoutingKey: true,
		// EventID and SportID intentionally nil — system routing keys
		// don't address an event.
	}
	msg := &types.FeedMessage{
		BasicFeedMessage: types.BasicFeedMessage{
			RoutingKey: rk,
			RawMessage: []byte("garbage"),
		},
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("BuildUnparsableMessage panicked on system routing key: %v", r)
		}
	}()
	out := f.BuildUnparsableMessage(t.Context(), msg)
	if out == nil {
		t.Fatal("BuildUnparsableMessage returned nil")
	}
	// Event must be nil for system routing keys.
	if out.Event() != nil {
		t.Errorf("Event() = %v, want nil for system routing key", out.Event())
	}
}

// TestBuildUnparsableMessage_NilRoutingKey_NilSafe — same defensive
// shape: a future code path that hands BuildUnparsableMessage a
// FeedMessage with a nil RoutingKey at all must not panic.
func TestBuildUnparsableMessage_NilRoutingKey_NilSafe(t *testing.T) {
	f := &FeedMessageFactory{}
	msg := &types.FeedMessage{
		BasicFeedMessage: types.BasicFeedMessage{
			RoutingKey: nil,
			RawMessage: []byte("garbage"),
		},
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("BuildUnparsableMessage panicked on nil RoutingKey: %v", r)
		}
	}()
	out := f.BuildUnparsableMessage(t.Context(), msg)
	if out.Event() != nil {
		t.Errorf("Event() = %v, want nil", out.Event())
	}
}

// TestBuildMessage_NilRoutingKeyReturnsError verifies the v2.20 fix
// to finding F4: BuildMessage previously dereferenced
// RoutingKey.EventID and SportID without nil-checks. parseRoute can
// return non-system RoutingKeyInfo with EventID == nil for malformed
// 8-part routes (sportID present, eventType/eventID empty), which
// crashed the consumer goroutine. Returning an error here lets the
// session convert the delivery into UnparsableMessage instead of
// panicking.
func TestBuildMessage_NilRoutingKeyReturnsError(t *testing.T) {
	f := &FeedMessageFactory{}
	msg := &types.FeedMessage{
		BasicFeedMessage: types.BasicFeedMessage{
			RoutingKey: nil,
			RawMessage: []byte("<oddsChange/>"),
		},
		Message: &feedXML.OddsChange{},
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("BuildMessage panicked on nil RoutingKey: %v", r)
		}
	}()
	out, err := f.BuildMessage(t.Context(), msg)
	if err == nil {
		t.Fatal("BuildMessage returned nil err for nil RoutingKey")
	}
	if out != nil {
		t.Errorf("returned message %+v, want nil", out)
	}
	if !strings.Contains(err.Error(), "routing key") {
		t.Errorf("err = %v, want to mention routing key", err)
	}
}

// TestBuildMessage_NilEventIDReturnsError verifies the v2.21 fix to
// the v2.20 F4 followup: BuildMessage on a non-system route with
// EventID == nil must return an error, not silently dispatch with
// event == nil.
//
// Bug shape: parseRoute can return RoutingKeyInfo{IsSystemRoutingKey:
// false, SportID: <urn>, EventID: nil} for a malformed 8-part route
// like "hi.pre.-.odds_change.6.-.-.-.-". Alive / SnapshotComplete
// (the only legitimate nil-EventID feed messages) are routed
// elsewhere and never reach BuildMessage, so a nil EventID here is
// always corruption. The v2.20 attempt left event=nil and continued
// — and the regression test covered the panic path with a deferred
// recover() that masked the underlying behavioural issue. v2.21
// returns an error so the session converts the delivery to
// UnparsableMessage.
//
// The F1-fix returns the error BEFORE reaching producerManager, so
// the test no longer needs the fixture stack — a bare
// FeedMessageFactory{} suffices.
func TestBuildMessage_NilEventIDReturnsError(t *testing.T) {
	f := &FeedMessageFactory{}
	rk := &types.RoutingKeyInfo{
		FullRoutingKey: "hi.pre.-.odds_change.6.-.-.-.-",
		SportID:        &types.URN{Prefix: "od", Type: "sport", ID: 6},
		// EventID nil — this is the malformed shape.
	}
	msg := &types.FeedMessage{
		BasicFeedMessage: types.BasicFeedMessage{
			RoutingKey: rk,
			RawMessage: []byte("<oddsChange/>"),
		},
		Message: &feedXML.OddsChange{},
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("BuildMessage panicked on nil EventID: %v", r)
		}
	}()
	out, err := f.BuildMessage(t.Context(), msg)
	if err == nil {
		t.Fatal("BuildMessage returned nil err for non-system route with nil EventID")
	}
	if out != nil {
		t.Errorf("returned message %+v, want nil", out)
	}
	if !strings.Contains(err.Error(), "event id") {
		t.Errorf("err = %v, want to mention event id", err)
	}
}

// TestBuildUnparsableMessage_PopulatesProducer is the regression for
// the C1 finding: BuildUnparsableMessage left the `producer` field
// uninitialized, so consumers calling unparsable.Producer().IsDown()
// nil-deref'd. Verify the field is populated for any FeedMessage with
// a non-nil .Message (the only path callers actually exercise — see
// the BuildMessage failure handlers in session.go which always carry
// a populated FeedMessage forward).
func TestBuildUnparsableMessage_PopulatesProducer(t *testing.T) {
	pm := producer.NewManager(minimalCfg{}, nil, log.New(nil))
	f := &FeedMessageFactory{
		producerManager: pm,
		logger:          log.New(nil),
	}

	msg := &types.FeedMessage{
		BasicFeedMessage: types.BasicFeedMessage{
			RoutingKey: &types.RoutingKeyInfo{IsSystemRoutingKey: true},
			RawMessage: []byte("garbage"),
		},
		Message: &feedXML.OddsChange{ProductID: 1},
	}
	out := f.BuildUnparsableMessage(t.Context(), msg)
	if out == nil {
		t.Fatal("BuildUnparsableMessage returned nil")
	}
	if out.Producer() == nil {
		t.Fatal("Producer() = nil — C1 regression: unparsableMessageImpl.producer was never initialized")
	}
	// Producer-manager isn't opened, so we get the unknown-producer
	// fallback. That's still a valid types.Producer; the consumer can
	// safely call methods on it without a panic.
	if id := out.Producer().ID(); id != 1 {
		t.Errorf("Producer().ID() = %d, want 1 (id of the message's product)", id)
	}
}

// TestBuildUnparsableMessage_NilMessage_NoProducerLookup verifies the
// defensive guard in BuildUnparsableMessage: when feedMessage.Message
// is nil (e.g. a completely undecodable XML body), the producer field
// stays nil rather than panicking on Message.Product().
func TestBuildUnparsableMessage_NilMessage_NoProducerLookup(t *testing.T) {
	pm := producer.NewManager(minimalCfg{}, nil, log.New(nil))
	f := &FeedMessageFactory{
		producerManager: pm,
		logger:          log.New(nil),
	}
	msg := &types.FeedMessage{
		BasicFeedMessage: types.BasicFeedMessage{
			RoutingKey: &types.RoutingKeyInfo{IsSystemRoutingKey: true},
			RawMessage: []byte("garbage"),
		},
		// Message intentionally nil.
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("BuildUnparsableMessage panicked on nil Message: %v", r)
		}
	}()
	out := f.BuildUnparsableMessage(t.Context(), msg)
	if out == nil {
		t.Fatal("BuildUnparsableMessage returned nil")
	}
	// No Message → no Product() to look up — but Producer() embeds the
	// NON-OPTIONAL Message contract, and under the default catch
	// strategy consumers receive this value: a nil here panicked
	// u.Producer().ID() / types.ProducerHasScope(u.Producer(), …).
	// The unknown-producer sentinel (id 0) must be returned instead.
	p := out.Producer()
	if p == nil {
		t.Fatal("Producer() = nil for fully-undecodable message; want unknown-producer sentinel (id 0)")
	}
	if p.ID() != 0 {
		t.Errorf("Producer().ID() = %d, want 0 (unknown-producer sentinel)", p.ID())
	}
	// The sentinel must be safe to interrogate the way consumers do —
	// these panicked on the pre-fix nil producer. (The placeholder is
	// deliberately permissive: name "unknown", both scopes.)
	_ = types.ProducerHasScope(p, types.LiveProducerScope)
	if p.Name() != "unknown" {
		t.Errorf("Producer().Name() = %q, want \"unknown\"", p.Name())
	}
}

// TestBuildUnparsableMessage_PreservesUpstreamTimestamp verifies the
// other half of the v2.19 F3 fix: BuildUnparsableMessage returned a
// zero-valued MessageTimestamp despite computing Published. Now it
// returns the upstream Created/Sent/Received plus its own Published.
func TestBuildUnparsableMessage_PreservesUpstreamTimestamp(t *testing.T) {
	f := &FeedMessageFactory{}

	created := time.Date(2026, 5, 5, 12, 0, 0, 123_000_000, time.UTC)
	sent := time.Date(2026, 5, 5, 12, 0, 1, 0, time.UTC)
	received := time.Date(2026, 5, 5, 12, 0, 2, 0, time.UTC)
	msg := &types.FeedMessage{
		BasicFeedMessage: types.BasicFeedMessage{
			RoutingKey: &types.RoutingKeyInfo{IsSystemRoutingKey: true},
			Timestamp: types.MessageTimestamp{
				Created:  created,
				Sent:     sent,
				Received: received,
			},
		},
	}
	out := f.BuildUnparsableMessage(t.Context(), msg)
	ts := out.Timestamp()
	if !ts.Created.Equal(created) {
		t.Errorf("Created = %v, want %v", ts.Created, created)
	}
	if !ts.Sent.Equal(sent) {
		t.Errorf("Sent = %v, want %v", ts.Sent, sent)
	}
	if !ts.Received.Equal(received) {
		t.Errorf("Received = %v, want %v", ts.Received, received)
	}
	if ts.Published.IsZero() {
		t.Error("Published not set; v2.19 fix should populate it")
	}
}
