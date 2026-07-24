package cache

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

// TestMatchCache_FailedMultiLocaleLoadDoesNotMutateLiveEntry pins the
// copy-on-write contract of the load/admit transaction: when a multi-
// locale load fails on a LATER locale, the locales it already fetched
// must NOT become visible on the live cached entry — the loader merges
// into a clone that is admitted only after every locale succeeds.
//
// Scenario: entry cached with [en]; a request for [en, de, fr] loads
// missing [de, fr] where de succeeds and fr fails. Pre-fix the loader
// mutated the cached pointer in place, so de leaked into the live entry
// despite the returned error.
func TestMatchCache_FailedMultiLocaleLoadDoesNotMutateLiveEntry(t *testing.T) {
	const matchURN = "od:match:55"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Locale is a path segment: /v1/sports/{locale}/sport_events/...
		switch {
		case strings.Contains(r.URL.Path, "/fr/"):
			http.Error(w, "boom", http.StatusInternalServerError)
		case strings.Contains(r.URL.Path, "/de/"):
			w.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(w, matchSummaryBody(matchURN, "de"))
		default:
			w.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(w, matchSummaryBody(matchURN, "en"))
		}
	}))
	defer srv.Close()

	mc := newMatchCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))
	id, _ := types.ParseURN(matchURN)

	// Populate [en] — admitted.
	if _, err := mc.Match(t.Context(), *id, []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("seed load(en): %v", err)
	}

	// Multi-locale load: de succeeds, fr fails → the whole load errors.
	_, err := mc.Match(t.Context(), *id, []types.Locale{types.EnLocale, types.DeLocale, types.FrLocale})
	if err == nil {
		t.Fatal("expected the multi-locale load to fail on fr")
	}

	// The LIVE cached entry must be exactly as before the failed load:
	// en only — de (fetched successfully before fr failed) must not leak.
	entry, ok := mc.lru.Peek(*id)
	if !ok {
		t.Fatal("cached entry vanished after failed load")
	}
	locales := entry.Locales()
	if len(locales) != 1 || locales[0] != types.EnLocale {
		t.Fatalf("live entry mutated by failed load: locales = %v, want [en]", locales)
	}
}
