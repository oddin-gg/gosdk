package cache

import (
	"encoding/xml"
	"testing"
	"time"

	apiXML "github.com/oddin-gg/gosdk/internal/api/xml"
	"github.com/oddin-gg/gosdk/types"
)

// newTestLocalizedTournament returns a fresh entry suitable for
// merge() — name/abbreviation maps initialised so merge can write
// per-locale values.
func newTestLocalizedTournament() *LocalizedTournament {
	return &LocalizedTournament{
		name:         map[types.Locale]string{},
		abbreviation: map[types.Locale]string{},
	}
}

// TestMerge_ExtendedPayloadFlagsCompetitorsLoaded verifies the v2.23
// fix to finding F3: merge() now sets competitorsLoaded=true when
// the wrapper is a TournamentExtendedWrapper, even if its competitor
// list is empty. Pre-fix, BuildTournament keyed on
// `len(competitorIDs) == 0` to detect "not yet loaded", which
// false-negativing every tournament that genuinely has zero
// competitors and triggered a clear-and-refetch on every build.
func TestMerge_ExtendedPayloadFlagsCompetitorsLoaded(t *testing.T) {
	l := newTestLocalizedTournament()

	ext := apiXML.TournamentExtended{
		Tournament: apiXML.Tournament{
			ID:    "od:tournament:1",
			Sport: apiXML.Sport{ID: "od:sport:1", Name: "Soccer"},
			Name:  "Premier League",
		},
		// Competitors == nil → GetCompetitors returns empty slice.
		Competitors: nil,
	}

	if err := l.merge(types.EnLocale, ext); err != nil {
		t.Fatalf("merge: %v", err)
	}

	if !l.competitorsAreLoaded() {
		t.Error("competitorsAreLoaded() = false after extended-payload merge with 0 competitors; pre-v2.23 false-negative regressed")
	}
	if got := len(l.competitorIDList()); got != 0 {
		t.Errorf("competitor list size = %d, want 0 (merged from empty payload)", got)
	}
}

// TestMerge_ExtendedPayloadWithCompetitorsLoadsList verifies the
// happy path of the same fix: when the extended payload DOES carry
// competitors, the list is populated AND the flag is set.
func TestMerge_ExtendedPayloadWithCompetitorsLoadsList(t *testing.T) {
	l := newTestLocalizedTournament()

	ext := apiXML.TournamentExtended{
		Tournament: apiXML.Tournament{
			ID:    "od:tournament:1",
			Sport: apiXML.Sport{ID: "od:sport:1"},
			Name:  "Premier League",
		},
		Competitors: &apiXML.CompetitorsWrapper{
			Competitor: []apiXML.Team{
				{ID: "od:competitor:10"},
				{ID: "od:competitor:20"},
			},
		},
	}

	if err := l.merge(types.EnLocale, ext); err != nil {
		t.Fatalf("merge: %v", err)
	}

	if !l.competitorsAreLoaded() {
		t.Error("competitorsAreLoaded() = false after extended-payload merge")
	}
	if got := len(l.competitorIDList()); got != 2 {
		t.Errorf("competitor list size = %d, want 2", got)
	}
}

// TestMerge_NonExtendedPayloadDoesNotFlagLoaded verifies that a
// non-extended payload (the bare /tournaments/{id}/info response)
// leaves competitorsLoaded false so BuildTournament still forces a
// /competitors fetch on first use.
func TestMerge_NonExtendedPayloadDoesNotFlagLoaded(t *testing.T) {
	l := newTestLocalizedTournament()

	// apiXML.Tournament implements TournamentWrapper but NOT
	// TournamentExtendedWrapper (no GetCompetitors).
	tour := apiXML.Tournament{
		ID:    "od:tournament:1",
		Sport: apiXML.Sport{ID: "od:sport:1"},
		Name:  "Premier League",
	}

	if err := l.merge(types.EnLocale, tour); err != nil {
		t.Fatalf("merge: %v", err)
	}

	if l.competitorsAreLoaded() {
		t.Error("competitorsAreLoaded() = true after non-extended merge — flag must not lie about /competitors knowledge")
	}
}

// TestTournamentSnapshot_AbsentLengthDatesStayNil: tournament_length may
// carry only one of its two dates (or none). The decoded zero value used
// to surface as a non-nil year-one date, so the public Tournament reported
// an absent date as a present one. An absent date must reach the snapshot
// as nil; a present one is kept.
func TestTournamentSnapshot_AbsentLengthDatesStayNil(t *testing.T) {
	cases := []struct {
		name               string
		length             string
		wantStart, wantEnd string // "" = nil
	}{
		{"no tournament_length", ``, "", ""},
		{"empty tournament_length", `<tournament_length/>`, "", ""},
		{"start only", `<tournament_length start_date="2026-03-01"/>`, "2026-03-01", ""},
		{"end only", `<tournament_length end_date="2026-04-02"/>`, "", "2026-04-02"},
		{"both", `<tournament_length start_date="2026-03-01" end_date="2026-04-02"/>`, "2026-03-01", "2026-04-02"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := `<tournament id="od:tournament:1" name="Cup" abbreviation="C" risk_tier="1">` +
				tc.length + `<sport id="od:sport:1" name="Soccer"/></tournament>`
			var tour apiXML.Tournament
			if err := xml.Unmarshal([]byte(payload), &tour); err != nil {
				t.Fatalf("decode: %v", err)
			}
			l := newTestLocalizedTournament()
			if err := l.merge(types.EnLocale, tour); err != nil {
				t.Fatalf("merge: %v", err)
			}
			got := l.tournamentSnapshot(t.Context(), nil, types.SportSummary{})
			for _, d := range []struct {
				label string
				got   *time.Time
				want  string
			}{{"StartDate", got.StartDate, tc.wantStart}, {"EndDate", got.EndDate, tc.wantEnd}} {
				switch {
				case d.want == "" && d.got != nil:
					t.Errorf("%s = %v, want nil for an absent date", d.label, *d.got)
				case d.want != "" && d.got == nil:
					t.Errorf("%s = nil, want %s", d.label, d.want)
				case d.want != "" && d.got.Format("2006-01-02") != d.want:
					t.Errorf("%s = %v, want %s", d.label, *d.got, d.want)
				}
			}
		})
	}
}
