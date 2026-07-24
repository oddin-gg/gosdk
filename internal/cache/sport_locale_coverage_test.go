package cache

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

// asymmetricSportServer serves sports {1,2} for every locale EXCEPT "de",
// which carries only sport 1 — the asymmetric-locale catalog the P2
// finding describes.
func asymmetricSportServer(t *testing.T) *httptest.Server {
	t.Helper()
	const en = `<?xml version="1.0"?>
<sports><sport id="od:sport:1" name="Football" abbreviation="FB"/><sport id="od:sport:2" name="Tennis" abbreviation="TN"/></sports>`
	const de = `<?xml version="1.0"?>
<sports><sport id="od:sport:1" name="Fußball" abbreviation="FB"/></sports>`
	// Empty tournament lists (top-level sport must match the requested id).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		switch {
		case strings.Contains(r.URL.Path, "/tournaments"):
			// Echo whichever sport id the path carries so FetchTournaments'
			// top-level identity check passes.
			id := "od:sport:1"
			if strings.Contains(r.URL.Path, "od:sport:2") {
				id = "od:sport:2"
			}
			_, _ = io.WriteString(w, `<?xml version="1.0"?><sport_tournaments><sport id="`+id+`" name="X"/></sport_tournaments>`)
		case strings.Contains(r.URL.Path, "/de/"):
			_, _ = io.WriteString(w, de)
		default:
			_, _ = io.WriteString(w, en)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestSportCache_AsymmetricLocales is the regression for the P2 finding:
// loadedLocales is global to the catalog, so loading two asymmetric
// locales marks both loaded even though a sport present in only one keeps
// a per-locale gap. Pre-fix Sports(en,de) returned sport 2 (no German
// name) and Sport(2,en,de) succeeded — both silently violating the
// all-requested-locales promise. Now Sports returns only the intersection
// and Sport(by id) reports ErrSportLocaleIncomplete. Both locale orders
// are exercised.
func TestSportCache_AsymmetricLocales(t *testing.T) {
	srv := asymmetricSportServer(t)
	sc := newSportDataCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))

	en, de := types.Locale("en"), types.Locale("de")
	sport1 := types.URN{Prefix: "od", Type: "sport", ID: 1}
	sport2 := types.URN{Prefix: "od", Type: "sport", ID: 2}

	for _, order := range [][]types.Locale{{en, de}, {de, en}} {
		// Sports: only the sport present in EVERY requested locale.
		ids, err := sc.Sports(t.Context(), order)
		if err != nil {
			t.Fatalf("Sports(%v): %v", order, err)
		}
		if len(ids) != 1 || ids[0] != sport1 {
			t.Fatalf("Sports(%v) = %v, want only %s (intersection)", order, ids, sport1.ToString())
		}

		// Sport present in both → ok.
		if _, err := sc.Sport(t.Context(), sport1, order); err != nil {
			t.Fatalf("Sport(1,%v) = %v, want nil", order, err)
		}

		// Sport present only in en → typed incomplete-locale error.
		_, err = sc.Sport(t.Context(), sport2, order)
		if !errors.Is(err, ErrSportLocaleIncomplete) {
			t.Fatalf("Sport(2,%v) err = %v, want ErrSportLocaleIncomplete", order, err)
		}

		sc.Purge() // reset locale marks so the next order re-loads cleanly
	}
}
