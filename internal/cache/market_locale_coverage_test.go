package cache

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/types"
)

// localeSplitCatalogServer serves a bulk market catalog where market 7
// exists only in en, market 8 only in de, and market 9 in both.
func localeSplitCatalogServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		var body string
		if strings.Contains(r.URL.Path, "/de/") {
			body = `<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="8" name="Nur DE"><outcomes><outcome id="1" name="o1"/></outcomes></market>
  <market id="9" name="Beide DE"><outcomes><outcome id="1" name="o1"/></outcomes></market>
</market_descriptions>`
		} else {
			body = `<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="7" name="EN only"><outcomes><outcome id="1" name="o1"/></outcomes></market>
  <market id="9" name="Both EN"><outcomes><outcome id="1" name="o1"/></outcomes></market>
</market_descriptions>`
		}
		_, _ = io.WriteString(w, body)
	}))
}

// TestMarketDescriptionByID_IncompleteLocaleIsTypedError pins the by-id
// locale-coverage revalidation: when a requested locale's catalog is
// loaded but this particular market is absent in it, the call must
// return ErrMarketLocaleIncomplete instead of an entry that silently
// misses the requested locale (the pre-fix behaviour — loadOne skips
// already-loaded locales globally, so the gap was never closed and
// never reported).
func TestMarketDescriptionByID_IncompleteLocaleIsTypedError(t *testing.T) {
	srv := localeSplitCatalogServer(t)
	defer srv.Close()

	mc := newMarketDescriptionCache(t.Context(), newAPIClientForTest(t, srv), nil)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// en-only request succeeds — market 7 exists in the en catalog.
	if _, err := mc.MarketDescriptionByID(ctx, 7, types.None[string](), []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("by-id(7, [en]): %v", err)
	}

	// [en, de]: the de catalog loads fine but has no market 7 — the
	// entry cannot satisfy de, and the call must say so, typed.
	_, err := mc.MarketDescriptionByID(ctx, 7, types.None[string](), []types.Locale{types.EnLocale, types.DeLocale})
	if err == nil {
		t.Fatal("by-id(7, [en,de]) returned an entry despite de being unavailable upstream")
	}
	if !errors.Is(err, ErrMarketLocaleIncomplete) {
		t.Fatalf("by-id(7, [en,de]) error = %v, want ErrMarketLocaleIncomplete", err)
	}

	// Market 9 exists in both catalogs — full coverage still succeeds.
	entry, err := mc.MarketDescriptionByID(ctx, 9, types.None[string](), []types.Locale{types.EnLocale, types.DeLocale})
	if err != nil {
		t.Fatalf("by-id(9, [en,de]): %v", err)
	}
	if got := entry.missingLocales([]types.Locale{types.EnLocale, types.DeLocale}); len(got) != 0 {
		t.Fatalf("market 9 missing locales %v after full-coverage load", got)
	}
}

// TestMarketDescriptionByID_MalformedRowIsIncompleteNotNotFound is the
// regression for the P2 finding: a malformed bulk row (a market lacking an
// <outcomes> block) is skipped, so if it is the FIRST locale seen no entry
// exists and a by-id lookup returned ErrItemNotFound — while the SAME
// defect on a later locale (entry already created) returned
// ErrMarketLocaleIncomplete. The classification is now consistent: a
// skipped malformed row is ErrMarketLocaleIncomplete regardless of load
// order; a market genuinely absent from the catalog is still
// ErrItemNotFound.
func TestMarketDescriptionByID_MalformedRowIsIncompleteNotNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		// Market 5: malformed (no <outcomes>) → skipped, no entry created.
		// Market 9: well-formed control.
		_, _ = io.WriteString(w, `<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="5" name="No outcomes block"/>
  <market id="9" name="Good"><outcomes><outcome id="1" name="o1"/></outcomes></market>
</market_descriptions>`)
	}))
	defer srv.Close()

	mc := newMarketDescriptionCache(t.Context(), newAPIClientForTest(t, srv), nil)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// Malformed first-locale row → typed incomplete, NOT not-found.
	_, err := mc.MarketDescriptionByID(ctx, 5, types.None[string](), []types.Locale{types.EnLocale})
	if !errors.Is(err, ErrMarketLocaleIncomplete) {
		t.Fatalf("by-id(5) err = %v, want ErrMarketLocaleIncomplete (skipped malformed row)", err)
	}

	// A market genuinely absent from the catalog is still ErrItemNotFound.
	_, err = mc.MarketDescriptionByID(ctx, 404, types.None[string](), []types.Locale{types.EnLocale})
	if !errors.Is(err, ErrItemNotFoundInCache) {
		t.Fatalf("by-id(404) err = %v, want ErrItemNotFoundInCache", err)
	}

	// Control: the well-formed market resolves normally.
	if _, err := mc.MarketDescriptionByID(ctx, 9, types.None[string](), []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("by-id(9): %v", err)
	}
}

// TestMultiLocalizedMarketDescriptions_FiltersPartialCoverage pins the
// bulk-view contract: the returned map contains ONLY entries that carry
// every supplied locale. Pre-fix the filter checked just the primary
// locale, so an entry missing a later locale leaked through in
// violation of the documented all-locales shape.
func TestMultiLocalizedMarketDescriptions_FiltersPartialCoverage(t *testing.T) {
	srv := localeSplitCatalogServer(t)
	defer srv.Close()

	mc := newMarketDescriptionCache(t.Context(), newAPIClientForTest(t, srv), nil)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	view, err := mc.MultiLocalizedMarketDescriptions(ctx, []types.Locale{types.EnLocale, types.DeLocale})
	if err != nil {
		t.Fatalf("multi([en,de]): %v", err)
	}

	ids := map[int]bool{}
	for k := range view {
		ids[k.MarketID] = true
	}
	if ids[7] {
		t.Error("market 7 (en-only) leaked into the [en,de] bulk view")
	}
	if ids[8] {
		t.Error("market 8 (de-only) leaked into the [en,de] bulk view")
	}
	if !ids[9] {
		t.Error("market 9 (full coverage) missing from the [en,de] bulk view")
	}
}
