package cache

import (
	"context"
	"testing"

	apiXML "github.com/oddin-gg/gosdk/internal/api/xml"
	"github.com/oddin-gg/gosdk/types"
)

func categoryTournamentPayload(categoryName string) apiXML.Tournament {
	return apiXML.Tournament{
		ID:    "od:tournament:1",
		Sport: apiXML.Sport{ID: "od:sport:1", Name: "Soccer"},
		Name:  "Bundesliga",
		Category: &apiXML.Category{
			ID:   "od:category:1",
			Name: categoryName,
		},
	}
}

func TestTournamentSnapshot_CategoryNameFollowsRequestedLocale(t *testing.T) {
	l := newTestLocalizedTournament()

	if err := l.merge(types.DeLocale, categoryTournamentPayload("Deutschland")); err != nil {
		t.Fatalf("merge de: %v", err)
	}
	if err := l.merge(types.EnLocale, categoryTournamentPayload("Germany")); err != nil {
		t.Fatalf("merge en: %v", err)
	}

	ctx := context.Background()

	de := l.tournamentSnapshot(ctx, nil, types.SportSummary{}, []types.Locale{types.DeLocale})
	if de.Category == nil || de.Category.Name != "Deutschland" {
		t.Errorf("de snapshot category = %+v, want name Deutschland (last-merged locale must not leak into other locales)", de.Category)
	}

	en := l.tournamentSnapshot(ctx, nil, types.SportSummary{}, []types.Locale{types.EnLocale})
	if en.Category == nil || en.Category.Name != "Germany" {
		t.Errorf("en snapshot category = %+v, want name Germany", en.Category)
	}

	fr := l.tournamentSnapshot(ctx, nil, types.SportSummary{}, []types.Locale{types.FrLocale})
	if fr.Category == nil || fr.Category.Name != "Germany" {
		t.Errorf("fr snapshot category = %+v, want fallback name Germany for a never-loaded locale", fr.Category)
	}
}

func TestCloneForUpdate_CopiesCategoryNames(t *testing.T) {
	l := newTestLocalizedTournament()
	if err := l.merge(types.DeLocale, categoryTournamentPayload("Deutschland")); err != nil {
		t.Fatalf("merge de: %v", err)
	}

	c := l.cloneForUpdate()
	if err := c.merge(types.EnLocale, categoryTournamentPayload("Germany")); err != nil {
		t.Fatalf("merge en on clone: %v", err)
	}

	if got := c.categoryName[types.DeLocale]; got != "Deutschland" {
		t.Errorf("clone lost de category name: %q", got)
	}
	if got, ok := l.categoryName[types.EnLocale]; ok {
		t.Errorf("clone merge leaked en category name %q into the original", got)
	}
}
