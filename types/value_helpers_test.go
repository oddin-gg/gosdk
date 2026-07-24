package types

import (
	"math"
	"testing"
	"time"
)

// --- SportSummary / Sport ---

func TestSportSummary_NameAndAbbreviation(t *testing.T) {
	s := SportSummary{
		Names:         map[Locale]string{EnLocale: "Soccer", DeLocale: "Fußball"},
		Abbreviations: map[Locale]string{EnLocale: "SOC", DeLocale: "FUS"},
	}
	if got := s.Name(EnLocale); got.ValueOr("") != "Soccer" {
		t.Errorf("Name(en) = %v", got)
	}
	if got := s.Name(DeLocale); got.ValueOr("") != "Fußball" {
		t.Errorf("Name(de) = %v", got)
	}
	if got := s.Name(RuLocale); got.IsSet() {
		t.Errorf("Name(unloaded) = %v, want None", got)
	}
	if got := s.Abbreviation(EnLocale); got.ValueOr("") != "SOC" {
		t.Errorf("Abbreviation(en) = %v", got)
	}
	if got := s.Abbreviation(RuLocale); got.IsSet() {
		t.Errorf("Abbreviation(unloaded) = %v, want None", got)
	}
}

// --- Match ---

func TestMatch_NameAndExtraInfoFor(t *testing.T) {
	m := Match{
		Names: map[Locale]string{EnLocale: "Foo vs Bar"},
		ExtraInfo: map[Locale]map[string]string{
			EnLocale: {"key": "value"},
		},
	}
	if got := m.Name(EnLocale); got.ValueOr("") != "Foo vs Bar" {
		t.Errorf("Name(en) = %v", got)
	}
	if got := m.Name(RuLocale); got.IsSet() {
		t.Errorf("Name(unloaded) = %v, want None", got)
	}
	info := m.ExtraInfoFor(EnLocale)
	if info["key"] != "value" {
		t.Errorf("ExtraInfoFor(en) = %v", info)
	}
	if got := m.ExtraInfoFor(RuLocale); got != nil {
		t.Errorf("ExtraInfoFor(unloaded) = %v, want nil", got)
	}
}

// --- Tournament ---

func TestTournament_NameAndAbbreviation(t *testing.T) {
	tn := Tournament{
		Names:         map[Locale]string{EnLocale: "Premier League"},
		Abbreviations: map[Locale]string{EnLocale: "PL"},
	}
	if got := tn.Name(EnLocale); got.ValueOr("") != "Premier League" {
		t.Errorf("Name(en) = %v", got)
	}
	if got := tn.Abbreviation(EnLocale); got.ValueOr("") != "PL" {
		t.Errorf("Abbreviation(en) = %v", got)
	}
}

// --- Competitor ---

func TestCompetitor_NameAbbreviationPlayersFor(t *testing.T) {
	p := Player{ID: "p1", Name: "Player One"}
	c := Competitor{
		Names:         map[Locale]string{EnLocale: "Team A"},
		Abbreviations: map[Locale]string{EnLocale: "A"},
		Players: map[Locale][]Player{
			EnLocale: {p},
		},
	}
	if got := c.Name(EnLocale); got.ValueOr("") != "Team A" {
		t.Errorf("Name = %v", got)
	}
	if got := c.Abbreviation(EnLocale); got.ValueOr("") != "A" {
		t.Errorf("Abbreviation = %v", got)
	}
	players := c.PlayersFor(EnLocale)
	if len(players) != 1 || players[0].ID != "p1" {
		t.Errorf("PlayersFor(en) = %v", players)
	}
	if got := c.PlayersFor(RuLocale); got != nil {
		t.Errorf("PlayersFor(unloaded) = %v, want nil", got)
	}
}

// --- LocalizedStaticData / StaticData ---

func TestStaticData_Accessors(t *testing.T) {
	s := StaticData{ID: 7, Description: Some("match started")}
	if s.GetID() != 7 {
		t.Errorf("GetID = %d", s.GetID())
	}
	if v, ok := s.GetDescription().Get(); !ok || v != "match started" {
		t.Errorf("GetDescription = %v, want Some(\"match started\")", s.GetDescription())
	}
}

func TestLocalizedStaticData_Accessors(t *testing.T) {
	enDesc := "Started"
	deDesc := "Begonnen"
	l := LocalizedStaticData{
		ID:          7,
		Description: Some(enDesc),
		Descriptions: map[Locale]string{
			EnLocale: enDesc,
			DeLocale: deDesc,
		},
	}
	if l.GetID() != 7 {
		t.Errorf("GetID = %d", l.GetID())
	}
	if v, ok := l.GetDescription().Get(); !ok || v != "Started" {
		t.Errorf("GetDescription = %v, want Some(\"Started\")", l.GetDescription())
	}

	if v, ok := l.LocalizedDescription(EnLocale).Get(); !ok || v != "Started" {
		t.Errorf("LocalizedDescription(en) = %v", l.LocalizedDescription(EnLocale))
	}
	if v, ok := l.LocalizedDescription(DeLocale).Get(); !ok || v != "Begonnen" {
		t.Errorf("LocalizedDescription(de) = %v", l.LocalizedDescription(DeLocale))
	}
	if l.LocalizedDescription(RuLocale).IsSet() {
		t.Errorf("LocalizedDescription(unloaded) = %v, want None", l.LocalizedDescription(RuLocale))
	}
}

// --- OutcomeOdds.Odds (decimal / american conversion) ---

func TestOutcomeOdds_DecimalDisplay(t *testing.T) {
	o := OutcomeOdds{DecimalOdds: Some[float32](2.5)}
	got := o.Odds(DecimalOddsDisplayType)
	if v, ok := got.Get(); !ok || v != 2.5 {
		t.Errorf("Decimal = %v, want Some(2.5)", got)
	}
}

func TestOutcomeOdds_AmericanDisplay_PositiveOdds(t *testing.T) {
	// (decimal - 1) * 100 — the standard moneyline formula.
	// 3.0 → +200, 2.5 → +150, 2.0 → +100.
	cases := []struct {
		decimal float32
		want    float32
	}{
		{2.0, 100},
		{2.5, 150},
		{3.0, 200},
	}
	for _, c := range cases {
		o := OutcomeOdds{DecimalOdds: Some(c.decimal)}
		got := o.Odds(AmericanOddsDisplayType)
		v, ok := got.Get()
		if !ok {
			t.Fatalf("decimal=%v: None result", c.decimal)
		}
		if v != c.want {
			t.Errorf("decimal=%v: got %v, want %v", c.decimal, v, c.want)
		}
	}
}

func TestOutcomeOdds_AmericanDisplay_FavouriteOdds(t *testing.T) {
	o := OutcomeOdds{DecimalOdds: Some[float32](1.5)}
	got := o.Odds(AmericanOddsDisplayType)
	v, ok := got.Get()
	if !ok {
		t.Fatal("None result")
	}
	want := float32(-100.0 / (1.5 - 1))
	if math.Abs(float64(v-want)) > 0.01 {
		t.Errorf("got %v, want %v", v, want)
	}
}

func TestOutcomeOdds_AmericanDisplay_OneIsNil(t *testing.T) {
	o := OutcomeOdds{DecimalOdds: Some[float32](1.0)}
	got := o.Odds(AmericanOddsDisplayType)
	if got.IsSet() {
		t.Errorf("decimal=1.0 american = %v, want None", got)
	}
}

func TestOutcomeOdds_AmericanDisplay_NaN(t *testing.T) {
	// Domain hardening: non-finite decimal odds (NaN, ±Inf) and values
	// <= 1 are malformed upstream data and must map to None — NaN used
	// to pass through, 0 mapped to a plausible-looking +100, and 0.5 to
	// +200.
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), 0, 0.5, 1.0, -2} {
		o := OutcomeOdds{DecimalOdds: Some[float32](float32(v))}
		if got := o.Odds(AmericanOddsDisplayType); got.IsSet() {
			t.Errorf("decimal=%v american = %v, want None (out of domain)", v, got)
		}
	}
}

func TestOutcomeOdds_NilOdds(t *testing.T) {
	o := OutcomeOdds{}
	if o.Odds(DecimalOddsDisplayType).IsSet() {
		t.Errorf("None odds Decimal should remain None")
	}
	if o.Odds(AmericanOddsDisplayType).IsSet() {
		t.Errorf("None odds American should remain None")
	}
}

// --- VoidFactor.String ---

func TestVoidFactor_String(t *testing.T) {
	cases := map[VoidFactor]string{
		VoidFactorRefundFull: "REFUND_FULL",
		VoidFactorRefundHalf: "REFUND_HALF",
		VoidFactor(0.25):     "",
	}
	for in, want := range cases {
		if got := in.String(); got != want {
			t.Errorf("VoidFactor(%v).String() = %q, want %q", float64(in), got, want)
		}
	}
}

// --- OutcomeDescription.LocalizedName / Description ---

func TestOutcomeDescription_LocalizedName(t *testing.T) {
	d := OutcomeDescription{
		ID:           "1",
		Names:        map[Locale]string{EnLocale: "home"},
		Descriptions: map[Locale]string{EnLocale: "Home team wins"},
	}
	if got := d.LocalizedName(EnLocale); got.ValueOr("") != "home" {
		t.Errorf("LocalizedName(en) = %v", got)
	}
	if got := d.LocalizedName(RuLocale); got.IsSet() {
		t.Errorf("LocalizedName(unloaded) = %v, want None", got)
	}
	if got := d.Description(EnLocale); got.ValueOr("") != "Home team wins" {
		t.Errorf("Description(en) = %v", got)
	}
	if got := d.Description(RuLocale); got.IsSet() {
		t.Errorf("Description(unloaded) = %v, want None", got)
	}
}

// --- MarketDescription.LocalizedName ---

func TestMarketDescription_LocalizedName(t *testing.T) {
	m := MarketDescription{
		ID:    1,
		Names: map[Locale]string{EnLocale: "1x2"},
	}
	if got := m.LocalizedName(EnLocale); got.ValueOr("") != "1x2" {
		t.Errorf("LocalizedName(en) = %v", got)
	}
	if got := m.LocalizedName(RuLocale); got.IsSet() {
		t.Errorf("LocalizedName(unloaded) = %v, want None", got)
	}
}

// --- ConnectionEventKind.String / ConnectionState.String — sanity ---
// (in events.go but tied to the value-helper theme)

func TestRecoveryRequestStatus_String(t *testing.T) {
	cases := map[RecoveryRequestStatus]string{
		RecoveryStatusPending:     "pending",
		RecoveryStatusCompleted:   "completed",
		RecoveryStatusFailed:      "failed",
		RecoveryStatusTimedOut:    "timed_out",
		RecoveryRequestStatus(99): "unknown",
	}
	for in, want := range cases {
		if got := in.String(); got != want {
			t.Errorf("Status(%v).String() = %q, want %q", in, got, want)
		}
	}
}

// --- ProducerDownReason / ProducerUpReason → ProducerStatusReason ---

func TestProducerDownReason_ToProducerStatusReason(t *testing.T) {
	cases := map[ProducerDownReason]ProducerStatusReason{
		AliveInternalViolationProducerDownReason:        AliveIntervalViolationProducerStatusReason,
		ProcessingQueueDelayViolationProducerDownReason: ProcessingQueueDelayViolationProducerStatusReason,
		OtherProducerDownReason:                         OtherProducerStatusReason,
		DefaultProducerDownReason:                       ErrorProducerStatusReason,
	}
	for in, want := range cases {
		if got := in.ToProducerStatusReason(); got != want {
			t.Errorf("DownReason %v: got %v, want %v", in, got, want)
		}
	}
}

func TestProducerUpReason_ToProducerStatusReason(t *testing.T) {
	cases := map[ProducerUpReason]ProducerStatusReason{
		FirstRecoveryCompletedProducerUpReason:         FirstRecoveryCompletedProducerStatusReason,
		ProcessingQueueDelayStabilizedProducerUpReason: ProcessingQueueDelayStabilizedProducerStatusReason,
		ReturnedFromInactivityProducerUpReason:         ReturnedFromInactivityProducerStatusReason,
		DefaultProducerUpReason:                        ErrorProducerStatusReason,
	}
	for in, want := range cases {
		if got := in.ToProducerStatusReason(); got != want {
			t.Errorf("UpReason %v: got %v, want %v", in, got, want)
		}
	}
}

// --- Sanity: zero-value structs behave reasonably ---

func TestZeroValueAccessors_DontPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("zero-value accessor panicked: %v", r)
		}
	}()
	_ = SportSummary{}.Name(EnLocale)
	_ = Match{}.Name(EnLocale)
	_ = Tournament{}.Name(EnLocale)
	_ = Competitor{}.Name(EnLocale)
	_ = LocalizedStaticData{}.LocalizedDescription(EnLocale)
	_ = OutcomeDescription{}.LocalizedName(EnLocale)
	_ = MarketDescription{}.LocalizedName(EnLocale)
}

// Sanity: time package is referenced by some entity types — a simple
// time-typed value-struct construction shouldn't error.
func TestEntities_TimePointers(t *testing.T) {
	now := time.Now()
	m := Match{ScheduledTime: &now}
	if !m.ScheduledTime.Equal(now) {
		t.Errorf("Match.ScheduledTime = %v", m.ScheduledTime)
	}
}
