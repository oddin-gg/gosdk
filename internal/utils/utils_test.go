package utils

import (
	"bytes"
	"encoding/xml"
	"testing"
	"time"
)

// --- Date ---

func TestDate_RoundTripText(t *testing.T) {
	cases := []string{"2026-01-15", "1999-12-31", "2000-02-29"}
	for _, in := range cases {
		var d Date
		if err := d.UnmarshalText([]byte(in)); err != nil {
			t.Fatalf("Unmarshal(%q): %v", in, err)
		}
		out, err := d.MarshalText()
		if err != nil {
			t.Fatalf("MarshalText: %v", err)
		}
		if string(out) != in {
			t.Errorf("round-trip %q -> %q", in, out)
		}
	}
}

func TestDate_UnmarshalAcceptsZoneFormat(t *testing.T) {
	// Mimics the API returning dates with a timezone suffix.
	var d Date
	if err := d.UnmarshalText([]byte("2026-01-15+00:00")); err != nil {
		t.Fatalf("UnmarshalText with zone failed: %v", err)
	}
	if (time.Time)(d).Year() != 2026 {
		t.Errorf("parsed year wrong: %v", time.Time(d))
	}
}

func TestDate_MarshalXML_OmitsZero(t *testing.T) {
	var d Date
	var buf bytes.Buffer
	enc := xml.NewEncoder(&buf)
	if err := enc.EncodeElement(d, xml.StartElement{Name: xml.Name{Local: "d"}}); err != nil {
		t.Fatalf("EncodeElement: %v", err)
	}
	_ = enc.Flush()
	if buf.Len() != 0 {
		t.Errorf("zero Date should encode to empty, got %q", buf.String())
	}
}

func TestDate_MarshalXMLAttr_OmitsZero(t *testing.T) {
	var d Date
	attr, err := d.MarshalXMLAttr(xml.Name{Local: "d"})
	if err != nil {
		t.Fatalf("MarshalXMLAttr: %v", err)
	}
	if attr != (xml.Attr{}) {
		t.Errorf("zero Date should yield empty attr, got %+v", attr)
	}
}

// --- DateTime ---

func TestDateTime_RoundTrip(t *testing.T) {
	in := "2026-01-15T12:34:56"
	var d DateTime
	if err := d.UnmarshalText([]byte(in)); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	out, err := d.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText: %v", err)
	}
	if string(out) != in {
		t.Errorf("round-trip %q -> %q", in, out)
	}
}

func TestNewDateTime_NilReturnsNil(t *testing.T) {
	if got := NewDateTime(nil); got != nil {
		t.Errorf("NewDateTime(nil) = %v, want nil", got)
	}
}

func TestNewDateTime_PointerCarriesValue(t *testing.T) {
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	d := NewDateTime(&now)
	if d == nil {
		t.Fatal("NewDateTime returned nil")
	}
	if !(time.Time)(*d).Equal(now) {
		t.Errorf("got %v, want %v", time.Time(*d), now)
	}
}

func TestDateTime_MarshalXML_OmitsZero(t *testing.T) {
	var d DateTime
	var buf bytes.Buffer
	enc := xml.NewEncoder(&buf)
	if err := enc.EncodeElement(d, xml.StartElement{Name: xml.Name{Local: "dt"}}); err != nil {
		t.Fatalf("EncodeElement: %v", err)
	}
	_ = enc.Flush()
	if buf.Len() != 0 {
		t.Errorf("zero DateTime should encode to empty, got %q", buf.String())
	}
}

// --- Timestamp ---

func TestTimestamp_UnmarshalUnixMilliseconds(t *testing.T) {
	var ts Timestamp
	if err := ts.UnmarshalText([]byte("1736899200000")); err != nil { // 2025-01-15 00:00:00Z
		t.Fatalf("Unmarshal: %v", err)
	}
	got := time.Time(ts).UTC()
	if got.Year() != 2025 || got.Month() != 1 || got.Day() != 15 {
		t.Errorf("parsed wrong moment: %v", got)
	}
}

func TestTimestamp_UnmarshalRejectsNonNumeric(t *testing.T) {
	var ts Timestamp
	if err := ts.UnmarshalText([]byte("not-a-number")); err == nil {
		t.Error("expected parse error on non-numeric input")
	}
}

func TestTimestamp_MarshalText_ZeroIsNil(t *testing.T) {
	var ts Timestamp
	out, err := ts.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText: %v", err)
	}
	if out != nil {
		t.Errorf("zero Timestamp should marshal to nil, got %q", out)
	}
}

func TestTimestamp_MarshalText_NonZero(t *testing.T) {
	moment := time.Date(2025, 1, 15, 0, 0, 0, 0, time.UTC)
	ts := Timestamp(moment)
	out, err := ts.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText: %v", err)
	}
	if string(out) != "1736899200000" {
		t.Errorf("got %q", out)
	}
}

func TestTimestamp_MarshalXML_OmitsZero(t *testing.T) {
	var ts Timestamp
	var buf bytes.Buffer
	enc := xml.NewEncoder(&buf)
	if err := enc.EncodeElement(ts, xml.StartElement{Name: xml.Name{Local: "ts"}}); err != nil {
		t.Fatalf("EncodeElement: %v", err)
	}
	_ = enc.Flush()
	if buf.Len() != 0 {
		t.Errorf("zero Timestamp should encode to empty, got %q", buf.String())
	}
}

// --- IsMarketVariantWithDynamicOutcomes ---

func TestIsMarketVariantWithDynamicOutcomes(t *testing.T) {
	cases := map[string]bool{
		"od:dynamic_outcomes:1234": true,
		"od:dynamic_outcomes:":     true,
		"od:variant:foo":           false,
		"":                         false,
		"dynamic_outcomes:1":       false,
	}
	for in, want := range cases {
		if got := IsMarketVariantWithDynamicOutcomes(in); got != want {
			t.Errorf("%q: got %v, want %v", in, got, want)
		}
	}
}
