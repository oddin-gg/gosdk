package factory

import (
	"errors"
	"testing"

	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/protocols"
)

// stubMarketData is a tiny protocols.MarketData implementation for tests.
type stubMarketData struct {
	marketName  string
	marketErr   error
	outcomeName string
	outcomeErr  error
}

func (s *stubMarketData) MarketName(locale protocols.Locale) (*string, error) {
	if s.marketErr != nil {
		return nil, s.marketErr
	}
	if s.marketName == "" {
		return nil, nil
	}
	return &s.marketName, nil
}

func (s *stubMarketData) OutcomeName(id string, locale protocols.Locale) (*string, error) {
	if s.outcomeErr != nil {
		return nil, s.outcomeErr
	}
	if s.outcomeName == "" {
		return nil, nil
	}
	return &s.outcomeName, nil
}

// --- ConvertFeedMarketStatus ---

func TestConvertFeedMarketStatus(t *testing.T) {
	cases := map[feedXML.MarketStatus]protocols.MarketStatus{
		feedXML.MarketStatusActive:     protocols.ActiveMarketStatus,
		feedXML.MarketStatusDeactived:  protocols.DeactivatedMarketStatus,
		feedXML.MarketStatusSuspended:  protocols.SuspendedMarketStatus,
		feedXML.MarketStatusHandedOver: protocols.HandedOverMarketStatus,
		feedXML.MarketStatusSettled:    protocols.SettledMarketStatus,
		feedXML.MarketStatusCancelled:  protocols.CancelledMarketStatus,
		feedXML.MarketStatusDefault:    protocols.UnknownMarketStatus,
	}
	for in, want := range cases {
		s := in
		if got := ConvertFeedMarketStatus(&s); got != want {
			t.Errorf("status %d: got %v, want %v", in, got, want)
		}
	}
	// nil input → Unknown.
	if got := ConvertFeedMarketStatus(nil); got != protocols.UnknownMarketStatus {
		t.Errorf("nil status: got %v, want Unknown", got)
	}
}

// --- resolveMarketName / resolveOutcomeName ---

func TestResolveMarketName(t *testing.T) {
	if got := resolveMarketName(nil, protocols.EnLocale); got != "" {
		t.Errorf("nil md should return empty, got %q", got)
	}
	md := &stubMarketData{marketName: "1x2"}
	if got := resolveMarketName(md, protocols.EnLocale); got != "1x2" {
		t.Errorf("got %q, want 1x2", got)
	}
	mdErr := &stubMarketData{marketErr: errors.New("boom")}
	if got := resolveMarketName(mdErr, protocols.EnLocale); got != "" {
		t.Errorf("error path should return empty, got %q", got)
	}
	mdNil := &stubMarketData{}
	if got := resolveMarketName(mdNil, protocols.EnLocale); got != "" {
		t.Errorf("nil-name path should return empty, got %q", got)
	}
}

func TestResolveOutcomeName(t *testing.T) {
	if got := resolveOutcomeName(nil, "1", protocols.EnLocale); got != "" {
		t.Error("nil md should return empty")
	}
	md := &stubMarketData{outcomeName: "home"}
	if got := resolveOutcomeName(md, "1", protocols.EnLocale); got != "home" {
		t.Errorf("got %q, want home", got)
	}
	mdErr := &stubMarketData{outcomeErr: errors.New("boom")}
	if got := resolveOutcomeName(mdErr, "1", protocols.EnLocale); got != "" {
		t.Errorf("error should return empty, got %q", got)
	}
}

// --- MarketFactory.extractSpecifiers ---

func TestMarketFactory_ExtractSpecifiers(t *testing.T) {
	mf := MarketFactory{logger: log.New(nil)}
	cases := []struct {
		name string
		in   *string
		want map[string]string
	}{
		{
			name: "nil",
			in:   nil,
			want: map[string]string{},
		},
		{
			name: "empty",
			in:   ptrStr(""),
			want: map[string]string{},
		},
		{
			name: "single",
			in:   ptrStr("total=1.5"),
			want: map[string]string{"total": "1.5"},
		},
		{
			name: "multiple",
			in:   ptrStr("score=1:1|sideofthe2nd=home"),
			want: map[string]string{"score": "1:1", "sideofthe2nd": "home"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mf.extractSpecifiers(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for k, v := range c.want {
				if got[k] != v {
					t.Errorf("key %q: got %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

// --- MarketFactory.buildOutcomeOdds ---

func TestMarketFactory_BuildOutcomeOdds(t *testing.T) {
	mf := MarketFactory{logger: log.New(nil)}
	md := &stubMarketData{outcomeName: "home"}
	odds := float32(1.5)
	prob := float32(0.6)
	active := uint(1)

	got := mf.buildOutcomeOdds(feedXML.Outcome{
		ID:            "1",
		Odds:          &odds,
		Probabilities: &prob,
		Active:        &active,
	}, md, protocols.EnLocale)

	if got.ID != "1" {
		t.Errorf("ID = %q", got.ID)
	}
	if got.Name != "home" {
		t.Errorf("Name = %q", got.Name)
	}
	if !got.IsActive {
		t.Error("IsActive should be true when Active=1")
	}
	if got.DecimalOdds == nil || *got.DecimalOdds != 1.5 {
		t.Errorf("DecimalOdds = %v", got.DecimalOdds)
	}
	if got.Probability == nil || *got.Probability != 0.6 {
		t.Errorf("Probability = %v", got.Probability)
	}
}

func TestMarketFactory_BuildOutcomeOdds_InactiveDefault(t *testing.T) {
	mf := MarketFactory{logger: log.New(nil)}
	md := &stubMarketData{}
	got := mf.buildOutcomeOdds(feedXML.Outcome{ID: "1"}, md, protocols.EnLocale)
	if got.IsActive {
		t.Error("IsActive should default to false when Active is nil")
	}
}

// --- MarketFactory.buildOutcomeSettlement ---

func TestMarketFactory_BuildOutcomeSettlement(t *testing.T) {
	mf := MarketFactory{logger: log.New(nil)}
	md := &stubMarketData{outcomeName: "draw"}

	cases := []struct {
		name       string
		feedResult *feedXML.OutcomeResult
		want       protocols.OutcomeResult
	}{
		{"lost", ptrFR(feedXML.OutcomeResultLost), protocols.LostOutcomeResult},
		{"won", ptrFR(feedXML.OutcomeResultWon), protocols.WonOutcomeResult},
		{"undecided", ptrFR(feedXML.OutcomeResultUndecidedYet), protocols.UndecidedYetOutcomeResult},
		{"nil", nil, protocols.UnknownOutcomeResult},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mf.buildOutcomeSettlement(feedXML.Outcome{ID: "1", Result: c.feedResult}, md, protocols.EnLocale)
			if got.OutcomeResult != c.want {
				t.Errorf("got %v, want %v", got.OutcomeResult, c.want)
			}
		})
	}
}

func TestMarketFactory_BuildOutcomeSettlement_VoidFactor(t *testing.T) {
	mf := MarketFactory{logger: log.New(nil)}
	md := &stubMarketData{}
	full := float32(1.0)
	half := float32(0.5)
	other := float32(0.25)

	cases := []struct {
		name string
		vf   *float32
		want *protocols.VoidFactor
	}{
		{"nil", nil, nil},
		{"full", &full, ptrVF(protocols.VoidFactorRefundFull)},
		{"half", &half, ptrVF(protocols.VoidFactorRefundHalf)},
		{"other", &other, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mf.buildOutcomeSettlement(feedXML.Outcome{ID: "1", VoidFactor: c.vf}, md, protocols.EnLocale)
			switch {
			case c.want == nil && got.VoidFactor != nil:
				t.Errorf("want nil, got %v", *got.VoidFactor)
			case c.want != nil && got.VoidFactor == nil:
				t.Errorf("want %v, got nil", *c.want)
			case c.want != nil && *got.VoidFactor != *c.want:
				t.Errorf("want %v, got %v", *c.want, *got.VoidFactor)
			}
		})
	}
}

// BuildMarket / BuildMarketWith* depend on a wired-up
// MarketDescriptionFactory + cache; they're exercised through the
// gosdk client tests (httptest with a real fixture server) rather
// than here. This file covers the pure-helper logic.

// --- helpers ---

func ptrStr(s string) *string                            { return &s }
func ptrFR(r feedXML.OutcomeResult) *feedXML.OutcomeResult { return &r }
func ptrVF(v protocols.VoidFactor) *protocols.VoidFactor { return &v }
