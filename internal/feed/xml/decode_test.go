package xml

import (
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
	if a.Subscribed != 1 {
		t.Fatalf("Subscribed = %d, want 1", a.Subscribed)
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
