package protocols

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
	if got := s.Name(EnLocale); got != "Soccer" {
		t.Errorf("Name(en) = %q", got)
	}
	if got := s.Name(DeLocale); got != "Fußball" {
		t.Errorf("Name(de) = %q", got)
	}
	if got := s.Name(RuLocale); got != "" {
		t.Errorf("Name(unloaded) = %q, want empty", got)
	}
	if got := s.Abbreviation(EnLocale); got != "SOC" {
		t.Errorf("Abbreviation(en) = %q", got)
	}
	if got := s.Abbreviation(RuLocale); got != "" {
		t.Errorf("Abbreviation(unloaded) = %q, want empty", got)
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
	if got := m.Name(EnLocale); got != "Foo vs Bar" {
		t.Errorf("Name(en) = %q", got)
	}
	if got := m.Name(RuLocale); got != "" {
		t.Errorf("Name(unloaded) = %q, want empty", got)
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
	if got := tn.Name(EnLocale); got != "Premier League" {
		t.Errorf("Name(en) = %q", got)
	}
	if got := tn.Abbreviation(EnLocale); got != "PL" {
		t.Errorf("Abbreviation(en) = %q", got)
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
	if got := c.Name(EnLocale); got != "Team A" {
		t.Errorf("Name = %q", got)
	}
	if got := c.Abbreviation(EnLocale); got != "A" {
		t.Errorf("Abbreviation = %q", got)
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
	desc := "match started"
	s := StaticData{ID: 7, Description: &desc}
	if s.GetID() != 7 {
		t.Errorf("GetID = %d", s.GetID())
	}
	got := s.GetDescription()
	if got == nil || *got != "match started" {
		t.Errorf("GetDescription = %v", got)
	}
}

func TestLocalizedStaticData_Accessors(t *testing.T) {
	enDesc := "Started"
	deDesc := "Begonnen"
	l := LocalizedStaticData{
		ID:          7,
		Description: &enDesc,
		Descriptions: map[Locale]string{
			EnLocale: enDesc,
			DeLocale: deDesc,
		},
	}
	if l.GetID() != 7 {
		t.Errorf("GetID = %d", l.GetID())
	}
	if got := l.GetDescription(); got == nil || *got != "Started" {
		t.Errorf("GetDescription = %v", got)
	}

	if got := l.LocalizedDescription(EnLocale); got == nil || *got != "Started" {
		t.Errorf("LocalizedDescription(en) = %v", got)
	}
	if got := l.LocalizedDescription(DeLocale); got == nil || *got != "Begonnen" {
		t.Errorf("LocalizedDescription(de) = %v", got)
	}
	if got := l.LocalizedDescription(RuLocale); got != nil {
		t.Errorf("LocalizedDescription(unloaded) = %v, want nil", got)
	}
}

// --- OutcomeOdds.Odds (decimal / american conversion) ---

func TestOutcomeOdds_DecimalDisplay(t *testing.T) {
	d := float32(2.5)
	o := OutcomeOdds{DecimalOdds: &d}
	got := o.Odds(DecimalOddsDisplayType)
	if got == nil || *got != 2.5 {
		t.Errorf("Decimal = %v, want 2.5", got)
	}
}

func TestOutcomeOdds_AmericanDisplay_PositiveOdds(t *testing.T) {
	d := float32(3.0) // decimal 3.0 → american -100*3 = +200 (>=2.0 path: 3-100=−97)
	o := OutcomeOdds{DecimalOdds: &d}
	got := o.Odds(AmericanOddsDisplayType)
	if got == nil {
		t.Fatal("nil result")
	}
	// Implementation: decimal >= 2.0 → result = decimal - 100. So 3.0 → -97.
	want := float32(3.0 - 100.0)
	if *got != want {
		t.Errorf("got %v, want %v", *got, want)
	}
}

func TestOutcomeOdds_AmericanDisplay_FavouriteOdds(t *testing.T) {
	d := float32(1.5) // decimal 1.5 → american -100/(1.5-1) = -200
	o := OutcomeOdds{DecimalOdds: &d}
	got := o.Odds(AmericanOddsDisplayType)
	if got == nil {
		t.Fatal("nil result")
	}
	want := float32(-100.0 / (1.5 - 1))
	if math.Abs(float64(*got-want)) > 0.01 {
		t.Errorf("got %v, want %v", *got, want)
	}
}

func TestOutcomeOdds_AmericanDisplay_OneIsNil(t *testing.T) {
	d := float32(1.0)
	o := OutcomeOdds{DecimalOdds: &d}
	got := o.Odds(AmericanOddsDisplayType)
	if got != nil {
		t.Errorf("decimal=1.0 american = %v, want nil", got)
	}
}

func TestOutcomeOdds_AmericanDisplay_NaN(t *testing.T) {
	d := float32(math.NaN())
	o := OutcomeOdds{DecimalOdds: &d}
	got := o.Odds(AmericanOddsDisplayType)
	if got == nil || !math.IsNaN(float64(*got)) {
		t.Errorf("NaN should pass through, got %v", got)
	}
}

func TestOutcomeOdds_NilOdds(t *testing.T) {
	o := OutcomeOdds{}
	if got := o.Odds(DecimalOddsDisplayType); got != nil {
		t.Errorf("nil odds Decimal = %v", got)
	}
	if got := o.Odds(AmericanOddsDisplayType); got != nil {
		t.Errorf("nil odds American = %v", got)
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
	if got := d.LocalizedName(EnLocale); got == nil || *got != "home" {
		t.Errorf("LocalizedName(en) = %v", got)
	}
	if got := d.LocalizedName(RuLocale); got != nil {
		t.Errorf("LocalizedName(unloaded) = %v, want nil", got)
	}
	if got := d.Description(EnLocale); got == nil || *got != "Home team wins" {
		t.Errorf("Description(en) = %v", got)
	}
	if got := d.Description(RuLocale); got != nil {
		t.Errorf("Description(unloaded) = %v, want nil", got)
	}
}

// --- MarketDescription.LocalizedName ---

func TestMarketDescription_LocalizedName(t *testing.T) {
	m := MarketDescription{
		ID:    1,
		Names: map[Locale]string{EnLocale: "1x2"},
	}
	if got := m.LocalizedName(EnLocale); got == nil || *got != "1x2" {
		t.Errorf("LocalizedName(en) = %v", got)
	}
	if got := m.LocalizedName(RuLocale); got != nil {
		t.Errorf("LocalizedName(unloaded) = %v, want nil", got)
	}
}

// --- ConnectionEventKind.String / ConnectionState.String — sanity ---
// (in events.go but tied to the value-helper theme)

func TestRecoveryRequestStatus_String(t *testing.T) {
	cases := map[RecoveryRequestStatus]string{
		RecoveryStatusPending:   "pending",
		RecoveryStatusCompleted: "completed",
		RecoveryStatusFailed:    "failed",
		RecoveryStatusTimedOut:  "timed_out",
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
