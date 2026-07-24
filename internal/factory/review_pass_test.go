package factory

import (
	"testing"
	"time"

	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	"github.com/oddin-gg/gosdk/types"
)

// TestBetCancel_TimesDecodeAsMilliseconds is the regression for the
// ms-as-seconds bug: bet_cancel / rollback_bet_cancel start_time and
// end_time carry epoch MILLISECONDS on the wire (13-digit, same unit as
// the sibling timestamp attributes), but were decoded with
// time.Unix(seconds) — producing timestamps ~56,000 years in the future.
func TestBetCancel_TimesDecodeAsMilliseconds(t *testing.T) {
	const ms = int64(1777800000000) // 2026-05-03T20:00:00Z
	want := time.UnixMilli(ms)

	start, end := ms, ms+60_000
	bc := betCancelImpl{message: &feedXML.BetCancel{StartTime: &start, EndTime: &end}}
	if got := bc.StartTime(); got == nil || !got.Equal(want) {
		t.Fatalf("BetCancel.StartTime() = %v, want %v", got, want)
	}
	if got := bc.EndTime(); got == nil || !got.Equal(want.Add(time.Minute)) {
		t.Fatalf("BetCancel.EndTime() = %v, want %v", got, want.Add(time.Minute))
	}
	if y := bc.StartTime().Year(); y != 2026 {
		t.Fatalf("BetCancel.StartTime().Year() = %d, want 2026 (seconds-vs-ms regression)", y)
	}

	rbc := rollbackBetCancelImpl{message: &feedXML.RollbackBetCancel{StartTime: &start, EndTime: &end}}
	if got := rbc.StartTime(); got == nil || !got.Equal(want) {
		t.Fatalf("RollbackBetCancel.StartTime() = %v, want %v", got, want)
	}
	if got := rbc.EndTime(); got == nil || !got.Equal(want.Add(time.Minute)) {
		t.Fatalf("RollbackBetCancel.EndTime() = %v, want %v", got, want.Add(time.Minute))
	}
}

// TestMakeOutcomeName_LocaleMissSkipsInsteadOfEmpty is the regression
// for the Some("") bug: when a "home"/"away" catalog outcome name is
// substituted with the competitor's name and the competitor has no name
// in the requested locale, the substitution used to store ValueOr("") —
// so outcome.Name(locale) returned a bogus Some(""), indistinguishable
// from a legitimately empty name. It must now return nil so the resolve
// layer skips the locale and the accessor reports None.
func TestMakeOutcomeName_LocaleMissSkipsInsteadOfEmpty(t *testing.T) {
	match := types.Match{
		HomeCompetitor: &types.TeamCompetitor{Competitor: types.Competitor{
			Names: map[types.Locale]string{types.EnLocale: "Team A"},
		}},
		AwayCompetitor: &types.TeamCompetitor{Competitor: types.Competitor{
			Names: map[types.Locale]string{types.EnLocale: "Team B"},
		}},
	}
	md := marketDataImpl{event: match}

	home := "home"
	// Loaded locale: substituted with the competitor name.
	got, err := md.makeOutcomeName(&home, types.Some("home"), types.EnLocale)
	if err != nil || got == nil || *got != "Team A" {
		t.Fatalf("en substitution = %v, %v; want Team A", got, err)
	}
	// Missing locale: nil (skip), NOT a pointer to "".
	got, err = md.makeOutcomeName(&home, types.Some("home"), types.RuLocale)
	if err != nil {
		t.Fatalf("ru substitution err: %v", err)
	}
	if got != nil {
		t.Fatalf("ru substitution = %q, want nil (locale skipped, no Some(\"\"))", *got)
	}
	// Non-substituted names pass through untouched regardless of locale.
	other := "Over 2.5"
	got, err = md.makeOutcomeName(&other, types.Some("Over 2.5"), types.RuLocale)
	if err != nil || got == nil || *got != "Over 2.5" {
		t.Fatalf("passthrough = %v, %v; want Over 2.5", got, err)
	}
}

// TestFeedMessageFactory_BuildLocales pins the event-build locale set:
// message events are built with the SAME locale list the market factory
// enriches names for (default + preloads). Pre-fix events carried only
// the default locale, starving the home/away substitution for every
// preloaded non-default locale.
func TestFeedMessageFactory_BuildLocales(t *testing.T) {
	mf := NewMarketFactory(nil, []types.Locale{types.EnLocale, types.RuLocale}, nil)
	f := &FeedMessageFactory{marketFactory: mf, oddsFeedConfiguration: minimalCfg{}}
	got := f.buildLocales()
	if len(got) != 2 || got[0] != types.EnLocale || got[1] != types.RuLocale {
		t.Fatalf("buildLocales = %v, want [en ru]", got)
	}

	// Without a market factory, falls back to the default locale.
	bare := &FeedMessageFactory{oddsFeedConfiguration: minimalCfg{}}
	got = bare.buildLocales()
	if len(got) != 1 || got[0] != types.EnLocale {
		t.Fatalf("fallback buildLocales = %v, want [en]", got)
	}
}

// TestMakeOutcomeName_TranslatedLabelSubstitutes is the regression for
// the English-only home/away detection: the substitution used to trigger
// only when the LOCALIZED outcome label literally equalled "home"/"away",
// so a non-English catalog (label already translated, e.g. "Хозяева")
// returned its generic translated label instead of the localized team
// name. Detection now keys on the outcome's CANONICAL (English catalog)
// label, which OutcomeName fetches alongside the requested locale.
func TestMakeOutcomeName_TranslatedLabelSubstitutes(t *testing.T) {
	match := types.Match{
		HomeCompetitor: &types.TeamCompetitor{Competitor: types.Competitor{
			Names: map[types.Locale]string{types.EnLocale: "Team A", types.RuLocale: "Команда А"},
		}},
		AwayCompetitor: &types.TeamCompetitor{Competitor: types.Competitor{
			Names: map[types.Locale]string{types.EnLocale: "Team B", types.RuLocale: "Команда Б"},
		}},
	}
	md := marketDataImpl{event: match}

	// Translated home label + canonical "home": substituted with the
	// localized competitor name.
	ruHome := "Хозяева"
	got, err := md.makeOutcomeName(&ruHome, types.Some("home"), types.RuLocale)
	if err != nil || got == nil || *got != "Команда А" {
		t.Fatalf("ru home substitution = %v, %v; want Команда А", got, err)
	}
	ruAway := "Гости"
	got, err = md.makeOutcomeName(&ruAway, types.Some("away"), types.RuLocale)
	if err != nil || got == nil || *got != "Команда Б" {
		t.Fatalf("ru away substitution = %v, %v; want Команда Б", got, err)
	}

	// Canonical label unavailable (defensive fallback to the localized
	// label): a translated label no longer matches — passes through.
	got, err = md.makeOutcomeName(&ruHome, types.None[string](), types.RuLocale)
	if err != nil || got == nil || *got != "Хозяева" {
		t.Fatalf("fallback passthrough = %v, %v; want Хозяева", got, err)
	}

	// A non-placeholder canonical label never substitutes even when the
	// localized label collides with a competitor-ish word.
	over := "Тотал больше 2.5"
	got, err = md.makeOutcomeName(&over, types.Some("Over 2.5"), types.RuLocale)
	if err != nil || got == nil || *got != "Тотал больше 2.5" {
		t.Fatalf("non-placeholder = %v, %v; want passthrough", got, err)
	}
}
