package xml

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/oddin-gg/gosdk/types"
)

// readTestdata loads a fixture from internal/feed/xml/testdata/.
func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

func TestDecode_Alive(t *testing.T) {
	data := readTestdata(t, "alive.xml")
	msg, err := Decode(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	a, ok := msg.(*Alive)
	if !ok {
		t.Fatalf("got %T, want *Alive", msg)
	}
	if a.Product() != 1 {
		t.Fatalf("Product = %d, want 1", a.Product())
	}
	if a.Subscribed == nil || *a.Subscribed != 1 {
		t.Fatalf("Subscribed = %v, want 1", a.Subscribed)
	}
}

func TestDecode_SnapshotComplete(t *testing.T) {
	data := readTestdata(t, "snapshot_complete.xml")
	msg, err := Decode(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	sc, ok := msg.(*SnapshotComplete)
	if !ok {
		t.Fatalf("got %T, want *SnapshotComplete", msg)
	}
	if sc.Product() != 1 {
		t.Fatalf("Product = %d, want 1", sc.Product())
	}
	if sc.RequestID != 2049987833 {
		t.Fatalf("RequestID = %d, want 2049987833", sc.RequestID)
	}
}

func TestDecode_OddsChange(t *testing.T) {
	data := readTestdata(t, "odds_change.xml")
	msg, err := Decode(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	oc, ok := msg.(*OddsChange)
	if !ok {
		t.Fatalf("got %T, want *OddsChange", msg)
	}
	if oc.Product() != 2 {
		t.Fatalf("Product = %d, want 2", oc.Product())
	}
	if oc.EventID != "od:match:198314" {
		t.Fatalf("EventID = %q, want od:match:198314", oc.EventID)
	}
	if oc.RequestID == nil || *oc.RequestID != 2049987833 {
		t.Fatalf("RequestID = %v, want 2049987833", oc.RequestID)
	}
	if len(oc.Odds.Markets) != 2 {
		t.Fatalf("Markets = %d, want 2", len(oc.Odds.Markets))
	}
	m0 := oc.Odds.Markets[0]
	if m0.ID != 1 {
		t.Fatalf("first market ID = %d, want 1", m0.ID)
	}
}

func TestDecode_BetStop(t *testing.T) {
	data := readTestdata(t, "bet_stop.xml")
	msg, err := Decode(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	bs, ok := msg.(*BetStop)
	if !ok {
		t.Fatalf("got %T, want *BetStop", msg)
	}
	if bs.Product() != 2 {
		t.Fatalf("Product = %d, want 2", bs.Product())
	}
	if bs.MessageAttributes.EventID != "od:match:198314" {
		t.Fatalf("EventID = %q", bs.MessageAttributes.EventID)
	}
	if bs.Groups != "all" {
		t.Fatalf("Groups = %q, want all", bs.Groups)
	}
}

func TestDecode_BetCancel(t *testing.T) {
	data := readTestdata(t, "bet_cancel.xml")
	msg, err := Decode(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	bc, ok := msg.(*BetCancel)
	if !ok {
		t.Fatalf("got %T, want *BetCancel", msg)
	}
	if bc.Product() != 2 {
		t.Fatalf("Product = %d, want 2", bc.Product())
	}
	if bc.StartTime == nil || bc.EndTime == nil {
		t.Fatalf("StartTime/EndTime nil; want both populated")
	}
	if len(bc.Markets) != 2 {
		t.Fatalf("Markets = %d, want 2", len(bc.Markets))
	}
}

func TestDecode_BetSettlement(t *testing.T) {
	data := readTestdata(t, "bet_settlement.xml")
	msg, err := Decode(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	bs, ok := msg.(*BetSettlement)
	if !ok {
		t.Fatalf("got %T, want *BetSettlement", msg)
	}
	if bs.EventID != "od:match:198314" {
		t.Fatalf("EventID = %q", bs.EventID)
	}
	if len(bs.Markets.Markets) != 1 {
		t.Fatalf("Markets = %d, want 1", len(bs.Markets.Markets))
	}
}

// TestDecode_BetSettlement_VoidReason covers the v2.x parity fix:
// bet_settlement markets carry void_reason_id and void_reason_params
// on the wire (per .NET's betSettlementMarket schema). Pre-fix Go's
// MarketWithOutcome XML struct didn't decode them, so the public
// types.MarketWithSettlement had no void-metadata surface — a parity
// gap vs .NET's IMarketWithSettlement.
func TestDecode_BetSettlement_VoidReason(t *testing.T) {
	body := []byte(`<?xml version="1.0"?>
<bet_settlement event_id="od:match:1" product="1" timestamp="1700000000000">
  <outcomes>
    <market id="42" void_reason_id="7" void_reason_params="abc">
      <outcome id="1" result="1"/>
    </market>
  </outcomes>
</bet_settlement>`)
	msg, err := Decode(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	bs, ok := msg.(*BetSettlement)
	if !ok {
		t.Fatalf("got %T, want *BetSettlement", msg)
	}
	if len(bs.Markets.Markets) != 1 {
		t.Fatalf("Markets = %d, want 1", len(bs.Markets.Markets))
	}
	m := bs.Markets.Markets[0]
	if m.VoidReasonID == nil || *m.VoidReasonID != 7 {
		t.Errorf("VoidReasonID = %v, want *uint=7", m.VoidReasonID)
	}
	if m.VoidReasonParams == nil || *m.VoidReasonParams != "abc" {
		t.Errorf("VoidReasonParams = %v, want *string=\"abc\"", m.VoidReasonParams)
	}
}

func TestDecode_RollbackBetSettlement(t *testing.T) {
	data := readTestdata(t, "rollback_bet_settlement.xml")
	msg, err := Decode(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	r, ok := msg.(*RollbackBetSettlement)
	if !ok {
		t.Fatalf("got %T, want *RollbackBetSettlement", msg)
	}
	if r.GetEventID() != "od:match:198314" {
		t.Fatalf("EventID = %q", r.GetEventID())
	}
	if len(r.Markets) != 2 {
		t.Fatalf("Markets = %d, want 2", len(r.Markets))
	}
}

func TestDecode_RollbackBetCancel(t *testing.T) {
	data := readTestdata(t, "rollback_bet_cancel.xml")
	msg, err := Decode(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	r, ok := msg.(*RollbackBetCancel)
	if !ok {
		t.Fatalf("got %T, want *RollbackBetCancel", msg)
	}
	if r.GetEventID() != "od:match:198314" {
		t.Fatalf("EventID = %q", r.GetEventID())
	}
	if r.StartTime == nil || r.EndTime == nil {
		t.Fatalf("StartTime/EndTime nil; want both populated")
	}
}

func TestDecode_FixtureChange(t *testing.T) {
	data := readTestdata(t, "fixture_change.xml")
	msg, err := Decode(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	fc, ok := msg.(*FixtureChange)
	if !ok {
		t.Fatalf("got %T, want *FixtureChange", msg)
	}
	if fc.EventID != "od:match:198314" {
		t.Fatalf("EventID = %q", fc.EventID)
	}
	if fc.ChangeType != FixtureChangeTypeNew {
		t.Fatalf("ChangeType = %d, want %d", fc.ChangeType, FixtureChangeTypeNew)
	}
}

// TestDecode_FixtureChange_Format covers wire change_type=4 (FORMAT
// in javasdk/netcoresdk). Pre-fix the constant was missing from the
// xml package's FixtureChangeType list and the factory mapper, so a
// real change_type=4 decoded as FixtureChangeTypeUnknown — silent
// feature gap vs the reference SDKs.
func TestDecode_FixtureChange_Format(t *testing.T) {
	body := []byte(`<?xml version="1.0"?>
<fixture_change event_id="od:match:1" product="1" change_type="4" timestamp="1700000000000"/>`)
	msg, err := Decode(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	fc, ok := msg.(*FixtureChange)
	if !ok {
		t.Fatalf("got %T, want *FixtureChange", msg)
	}
	if fc.ChangeType != FixtureChangeTypeFormat {
		t.Fatalf("ChangeType = %d, want %d (Format)", fc.ChangeType, FixtureChangeTypeFormat)
	}
}

// Error paths.

func TestDecode_Empty(t *testing.T) {
	if _, err := Decode(nil); !errors.Is(err, ErrEmptyPayload) {
		t.Fatalf("got %v, want ErrEmptyPayload", err)
	}
	if _, err := Decode([]byte{}); !errors.Is(err, ErrEmptyPayload) {
		t.Fatalf("got %v, want ErrEmptyPayload", err)
	}
}

func TestDecode_Unknown(t *testing.T) {
	_, err := Decode([]byte(`<wat product="1" timestamp="0"/>`))
	if !errors.Is(err, ErrUnknownMessage) {
		t.Fatalf("got %v, want ErrUnknownMessage", err)
	}
}

func TestDecode_Malformed(t *testing.T) {
	_, err := Decode([]byte(`<odds_change timestamp="bad"`)) // unterminated
	if err == nil {
		t.Fatal("expected error on malformed payload")
	}
	if errors.Is(err, ErrEmptyPayload) || errors.Is(err, ErrUnknownMessage) {
		t.Fatalf("got %v, want a generic parse error", err)
	}
}

// Round-trip: decode every fixture, verify it implements types.BasicMessage,
// and Product()/Timestamp() return non-zero.
func TestDecode_BasicMessageContract(t *testing.T) {
	fixtures := []string{
		"alive.xml",
		"snapshot_complete.xml",
		"odds_change.xml",
		"bet_stop.xml",
		"bet_cancel.xml",
		"bet_settlement.xml",
		"rollback_bet_settlement.xml",
		"rollback_bet_cancel.xml",
		"fixture_change.xml",
	}
	for _, f := range fixtures {
		t.Run(f, func(t *testing.T) {
			data := readTestdata(t, f)
			msg, err := Decode(data)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			var _ types.BasicMessage = msg // compile-time guarantee
			if msg.Product() == 0 {
				t.Fatalf("Product = 0, expected non-zero")
			}
			if msg.Timestamp().IsZero() {
				t.Fatalf("Timestamp is zero")
			}
		})
	}
}

// TestDecode_RejectsTrailingDocument pins the multi-root rejection:
// "<alive/><odds_change/>" used to decode "successfully" as the FIRST
// document, silently discarding the trailer while the delivery could
// still be acked. Trailing whitespace/comments stay accepted.
func TestDecode_RejectsTrailingDocument(t *testing.T) {
	if _, err := Decode([]byte(`<alive product="1" timestamp="1" subscribed="1"/><odds_change product="1" timestamp="1"/>`)); err == nil {
		t.Fatal("trailing second document accepted")
	}
	if _, err := Decode([]byte(`<alive product="1" timestamp="1" subscribed="1"/>trailing garbage`)); err == nil {
		t.Fatal("trailing character data accepted")
	}
	if _, err := Decode([]byte("<alive product=\"1\" timestamp=\"1\" subscribed=\"1\"/>\n  <!-- trace -->\n")); err != nil {
		t.Fatalf("benign trailer rejected: %v", err)
	}
}

// TestDecode_RejectsOversizedPayload pins the feed payload cap.
func TestDecode_RejectsOversizedPayload(t *testing.T) {
	orig := MaxFeedMessageBytes
	MaxFeedMessageBytes = 64
	defer func() { MaxFeedMessageBytes = orig }()

	big := append([]byte(`<alive product="1" timestamp="1" subscribed="1" pad="`), bytes.Repeat([]byte("x"), 128)...)
	big = append(big, []byte(`"/>`)...)
	if _, err := Decode(big); !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("oversized payload err = %v, want ErrPayloadTooLarge", err)
	}
	if _, err := Decode([]byte(`<alive product="1" timestamp="1" subscribed="1"/>`)); err != nil {
		t.Fatalf("in-cap payload rejected: %v", err)
	}
}

// TestEventMessages_AllCarryGetEventID is the table regression for the
// identity/invalidation gap (Codex P2): BetStop, BetCancel, and
// BetSettlement carry event ids but lacked the GetEventID accessor —
// route/payload identity mismatches bypassed validation for those
// kinds, and valid bet-stop/cancel/settlement traffic skipped ALL
// feed-driven fixture/match/tournament/status invalidation (the cache
// manager keys on the same accessor). Every event-addressed message
// kind must expose its payload event id.
func TestEventMessages_AllCarryGetEventID(t *testing.T) {
	type eventIDer interface{ GetEventID() string }
	const id = "od:match:198314"
	cases := []struct {
		name string
		msg  any
	}{
		{"odds_change", OddsChange{EventID: id}},
		{"bet_stop", BetStop{MessageAttributes: MessageAttributes{EventID: id}}},
		{"bet_cancel", BetCancel{MessageAttributes: MessageAttributes{EventID: id}}},
		{"bet_settlement", BetSettlement{EventID: id}},
		{"rollback_bet_cancel", RollbackBetCancel{MessageAttributes: MessageAttributes{EventID: id}}},
		{"fixture_change", FixtureChange{EventID: id}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			em, ok := tc.msg.(eventIDer)
			if !ok {
				t.Fatalf("%T does not implement GetEventID — identity validation and cache invalidation skip it", tc.msg)
			}
			if got := em.GetEventID(); got != id {
				t.Fatalf("GetEventID() = %q, want %q", got, id)
			}
		})
	}
}
