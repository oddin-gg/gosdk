package factory

import (
	"context"
	"errors"
	"testing"

	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

// stubMarketData is a tiny types.MarketData implementation for tests.
type stubMarketData struct {
	marketName  string
	marketErr   error
	outcomeName string
	outcomeErr  error
}

func (s *stubMarketData) MarketName(_ context.Context, _ types.Locale) (*string, error) {
	if s.marketErr != nil {
		return nil, s.marketErr
	}
	if s.marketName == "" {
		return nil, nil
	}
	return &s.marketName, nil
}

func (s *stubMarketData) OutcomeName(_ context.Context, _ string, _ types.Locale) (*string, error) {
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
	cases := map[feedXML.MarketStatus]types.MarketStatus{
		feedXML.MarketStatusActive:     types.ActiveMarketStatus,
		feedXML.MarketStatusDeactived:  types.DeactivatedMarketStatus,
		feedXML.MarketStatusSuspended:  types.SuspendedMarketStatus,
		feedXML.MarketStatusHandedOver: types.HandedOverMarketStatus,
		feedXML.MarketStatusSettled:    types.SettledMarketStatus,
		feedXML.MarketStatusCancelled:  types.CancelledMarketStatus,
		feedXML.MarketStatusDefault:    types.UnknownMarketStatus,
	}
	for in, want := range cases {
		s := in
		if got := ConvertFeedMarketStatus(&s); got != want {
			t.Errorf("status %d: got %v, want %v", in, got, want)
		}
	}
	// nil input → Unknown.
	if got := ConvertFeedMarketStatus(nil); got != types.UnknownMarketStatus {
		t.Errorf("nil status: got %v, want Unknown", got)
	}
}

// --- resolveMarketName / resolveOutcomeName ---

func TestResolveMarketName(t *testing.T) {
	ctx := t.Context()
	if got, ok := resolveMarketName(ctx, nil, types.EnLocale); ok {
		t.Errorf("nil md should return ok=false, got %q ok=%v", got, ok)
	}
	md := &stubMarketData{marketName: "1x2"}
	if got, ok := resolveMarketName(ctx, md, types.EnLocale); !ok || got != "1x2" {
		t.Errorf("got (%q, %v), want (\"1x2\", true)", got, ok)
	}
	mdErr := &stubMarketData{marketErr: errors.New("boom")}
	if got, ok := resolveMarketName(ctx, mdErr, types.EnLocale); ok {
		t.Errorf("error path should return ok=false, got %q ok=%v", got, ok)
	}
	mdNil := &stubMarketData{}
	if got, ok := resolveMarketName(ctx, mdNil, types.EnLocale); ok {
		t.Errorf("nil-name path should return ok=false, got %q ok=%v", got, ok)
	}
}

func TestResolveOutcomeName(t *testing.T) {
	ctx := t.Context()
	if _, ok := resolveOutcomeName(ctx, nil, "1", types.EnLocale); ok {
		t.Error("nil md should return ok=false")
	}
	md := &stubMarketData{outcomeName: "home"}
	if got, ok := resolveOutcomeName(ctx, md, "1", types.EnLocale); !ok || got != "home" {
		t.Errorf("got (%q, %v), want (\"home\", true)", got, ok)
	}
	mdErr := &stubMarketData{outcomeErr: errors.New("boom")}
	if got, ok := resolveOutcomeName(ctx, mdErr, "1", types.EnLocale); ok {
		t.Errorf("error should return ok=false, got %q ok=%v", got, ok)
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
		{
			// v2.24 F3: pre-fix the parser used strings.Split(part, "=")
			// which yielded a 3-element slice and rejected the whole
			// specifier. strings.Cut splits on the FIRST '=' so values
			// with '=' (opaque base64-ish payloads etc.) survive.
			name: "value contains equals",
			in:   ptrStr("opaque=a=b=c"),
			want: map[string]string{"opaque": "a=b=c"},
		},
		{
			// Defence: an empty key (leading '=') stays rejected.
			name: "leading equals rejected",
			in:   ptrStr("=lonelyvalue"),
			want: map[string]string{},
		},
		{
			// Defence: a part with no '=' at all is still rejected
			// (key without value is meaningless).
			name: "no equals rejected",
			in:   ptrStr("total"),
			want: map[string]string{},
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
	mf := MarketFactory{logger: log.New(nil), locales: []types.Locale{types.EnLocale}}
	md := &stubMarketData{outcomeName: "home"}
	odds := float32(1.5)
	prob := float32(0.6)
	active := int(1)

	got := mf.buildOutcomeOdds(t.Context(), feedXML.Outcome{
		ID:            "1",
		Odds:          &odds,
		Probabilities: &prob,
		Active:        &active,
	}, md)

	if got.ID != "1" {
		t.Errorf("ID = %q", got.ID)
	}
	if got.Name(types.EnLocale).ValueOr("") != "home" {
		t.Errorf("Name = %v", got.Name(types.EnLocale))
	}
	if !got.IsActive {
		t.Error("IsActive should be true when Active=1")
	}
	if v, ok := got.DecimalOdds.Get(); !ok || v != 1.5 {
		t.Errorf("DecimalOdds = %v, want Some(1.5)", got.DecimalOdds)
	}
	if v, ok := got.Probability.Get(); !ok || v != 0.6 {
		t.Errorf("Probability = %v, want Some(0.6)", got.Probability)
	}
}

func TestMarketFactory_BuildOutcomeOdds_InactiveDefault(t *testing.T) {
	mf := MarketFactory{logger: log.New(nil), locales: []types.Locale{types.EnLocale}}
	md := &stubMarketData{}
	got := mf.buildOutcomeOdds(t.Context(), feedXML.Outcome{ID: "1"}, md)
	if got.IsActive {
		t.Error("IsActive should default to false when Active is nil")
	}
}

// --- MarketFactory.buildOutcomeSettlement ---

func TestMarketFactory_BuildOutcomeSettlement(t *testing.T) {
	mf := MarketFactory{logger: log.New(nil), locales: []types.Locale{types.EnLocale}}
	md := &stubMarketData{outcomeName: "draw"}

	cases := []struct {
		name       string
		feedResult *feedXML.OutcomeResult
		want       types.OutcomeResult
	}{
		{"lost", ptrFR(feedXML.OutcomeResultLost), types.LostOutcomeResult},
		{"won", ptrFR(feedXML.OutcomeResultWon), types.WonOutcomeResult},
		{"undecided", ptrFR(feedXML.OutcomeResultUndecidedYet), types.UndecidedYetOutcomeResult},
		{"nil", nil, types.UnknownOutcomeResult},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mf.buildOutcomeSettlement(t.Context(), feedXML.Outcome{ID: "1", Result: c.feedResult}, md)
			if got.OutcomeResult != c.want {
				t.Errorf("got %v, want %v", got.OutcomeResult, c.want)
			}
		})
	}
}

// TestMarketWithSettlement_VoidReason_Plumbing is the regression for
// the v2.x parity fix: bet_settlement markets carry void_reason_id /
// void_reason_params, and MarketWithSettlement must surface them as
// Optional[int] / Optional[string] (parity with .NET's
// IMarketWithSettlement which inherits from IMarketCancel).
//
// Verifies the FromPtr → Optional conversion the factory uses (without
// going through the full BuildMarketWithSettlement pipeline, which
// requires a fully-wired MarketDataFactory + cache).
func TestMarketWithSettlement_VoidReason_Plumbing(t *testing.T) {
	t.Run("present", func(t *testing.T) {
		id := int(7)
		params := "k=v"
		market := &feedXML.MarketWithOutcome{
			MarketAttributes: feedXML.MarketAttributes{ID: 42},
			VoidReasonID:     &id,
			VoidReasonParams: &params,
		}
		out := types.MarketWithSettlement{
			Market:           types.Market{ID: market.ID},
			VoidReasonID:     types.FromPtr(market.VoidReasonID),
			VoidReasonParams: types.FromPtr(market.VoidReasonParams),
		}
		if v, ok := out.VoidReasonID.Get(); !ok || v != 7 {
			t.Errorf("VoidReasonID = %v, want Some(7)", out.VoidReasonID)
		}
		if v, ok := out.VoidReasonParams.Get(); !ok || v != "k=v" {
			t.Errorf("VoidReasonParams = %v, want Some(\"k=v\")", out.VoidReasonParams)
		}
	})
	t.Run("absent", func(t *testing.T) {
		market := &feedXML.MarketWithOutcome{
			MarketAttributes: feedXML.MarketAttributes{ID: 42},
		}
		out := types.MarketWithSettlement{
			Market:           types.Market{ID: market.ID},
			VoidReasonID:     types.FromPtr(market.VoidReasonID),
			VoidReasonParams: types.FromPtr(market.VoidReasonParams),
		}
		if out.VoidReasonID.IsSet() {
			t.Errorf("VoidReasonID = %v, want None", out.VoidReasonID)
		}
		if out.VoidReasonParams.IsSet() {
			t.Errorf("VoidReasonParams = %v, want None", out.VoidReasonParams)
		}
	})
}

func TestMarketFactory_BuildOutcomeSettlement_VoidFactor(t *testing.T) {
	mf := MarketFactory{logger: log.New(nil), locales: []types.Locale{types.EnLocale}}
	md := &stubMarketData{}
	full := float32(1.0)
	half := float32(0.5)
	other := float32(0.25)

	cases := []struct {
		name string
		vf   *float32
		want types.Optional[types.VoidFactor]
	}{
		{"nil", nil, types.None[types.VoidFactor]()},
		{"full", &full, types.Some(types.VoidFactorRefundFull)},
		{"half", &half, types.Some(types.VoidFactorRefundHalf)},
		{"other", &other, types.None[types.VoidFactor]()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mf.buildOutcomeSettlement(t.Context(), feedXML.Outcome{ID: "1", VoidFactor: c.vf}, md)
			switch {
			case !c.want.IsSet() && got.VoidFactor.IsSet():
				t.Errorf("want None, got %v", got.VoidFactor)
			case c.want.IsSet() && !got.VoidFactor.IsSet():
				t.Errorf("want %v, got None", c.want)
			case c.want.IsSet() && got.VoidFactor.Value() != c.want.Value():
				t.Errorf("want %v, got %v", c.want, got.VoidFactor)
			}
		})
	}
}

// BuildMarket / BuildMarketWith* depend on a wired-up
// MarketDescriptionFactory + cache; they're exercised through the
// gosdk client tests (httptest with a real fixture server) rather
// than here. This file covers the pure-helper logic.

// --- helpers ---

func ptrStr(s string) *string                              { return &s }
func ptrFR(r feedXML.OutcomeResult) *feedXML.OutcomeResult { return &r }

// countingMarketData counts cache lookups, so the opt-out test can
// assert the work was SKIPPED and not merely discarded.
type countingMarketData struct {
	marketCalls  int
	outcomeCalls int
}

func (c *countingMarketData) MarketName(_ context.Context, _ types.Locale) (*string, error) {
	c.marketCalls++
	name := "1x2"
	return &name, nil
}

func (c *countingMarketData) OutcomeName(_ context.Context, _ string, _ types.Locale) (*string, error) {
	c.outcomeCalls++
	name := "home"
	return &name, nil
}

// TestResolveNames_OptOut pins WithMessageNameResolution(false). The stub
// returns a good name, so a broken gate fails on both the call count and
// the non-nil map rather than passing by accident.
func TestResolveNames_OptOut(t *testing.T) {
	locales := []types.Locale{types.EnLocale, types.RuLocale}

	off := NewMarketFactory(nil, locales, false, log.New(nil))
	md := &countingMarketData{}

	if names := off.resolveMarketNames(t.Context(), md); names != nil {
		t.Fatalf("market names = %v, want nil", names)
	}
	if names := off.resolveOutcomeNames(t.Context(), md, "1"); names != nil {
		t.Fatalf("outcome names = %v, want nil", names)
	}

	// Through the build path a message actually takes, not just the
	// resolvers: the outcome comes out with a nil map, and Name reports
	// None over it — the shape a locale the catalog cannot supply
	// already produces.
	odds := off.buildOutcomeOdds(t.Context(), feedXML.Outcome{ID: "1"}, md)
	if odds.Names != nil {
		t.Fatalf("built outcome Names = %v, want nil", odds.Names)
	}
	if _, ok := odds.Name(types.EnLocale).Get(); ok {
		t.Fatal("built outcome Name = Some, want None")
	}
	settled := off.buildOutcomeSettlement(t.Context(), feedXML.Outcome{ID: "1"}, md)
	if settled.Names != nil {
		t.Fatalf("settled outcome Names = %v, want nil", settled.Names)
	}

	if md.marketCalls != 0 || md.outcomeCalls != 0 {
		t.Fatalf("description cache consulted %d/%d times, want 0/0", md.marketCalls, md.outcomeCalls)
	}
}

// TestResolveNames_DefaultResolves: the default stays ON.
func TestResolveNames_DefaultResolves(t *testing.T) {
	locales := []types.Locale{types.EnLocale, types.RuLocale}

	on := NewMarketFactory(nil, locales, true, log.New(nil))
	md := &countingMarketData{}

	if names := on.resolveMarketNames(t.Context(), md); len(names) != 2 {
		t.Fatalf("market names = %v, want an entry per locale", names)
	}
	if names := on.resolveOutcomeNames(t.Context(), md, "1"); len(names) != 2 {
		t.Fatalf("outcome names = %v, want an entry per locale", names)
	}
	if md.marketCalls != 2 || md.outcomeCalls != 2 {
		t.Fatalf("description cache consulted %d/%d times, want 2/2", md.marketCalls, md.outcomeCalls)
	}

	// And through the build path, on its own stub so the counts stay
	// readable: one lookup per locale for the one outcome built.
	built := &countingMarketData{}
	odds := on.buildOutcomeOdds(t.Context(), feedXML.Outcome{ID: "1"}, built)
	if len(odds.Names) != 2 {
		t.Fatalf("built outcome Names = %v, want an entry per locale", odds.Names)
	}
	if built.outcomeCalls != 2 {
		t.Fatalf("built outcome consulted the cache %d times, want 2", built.outcomeCalls)
	}
}
