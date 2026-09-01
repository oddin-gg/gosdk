package cache

import (
	"fmt"
	"strings"
	"testing"
	"time"

	data "github.com/oddin-gg/gosdk/internal/api/xml"
	"github.com/oddin-gg/gosdk/types"
)

// exactGoalsCatalog models "Exact Number of Goals" (and the same shape
// as Match Team Exact Score): the catalog lists selections in ascending
// numeric order, but the outcome IDs are strings, so any lexical
// ordering puts 10 and 11 between 1 and 2.
func exactGoalsCatalog(locale types.Locale, reversed bool) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><market_descriptions response_code="OK">`)
	b.WriteString(`<market id="26" name="Exact number of goals"><outcomes>`)
	for n := 0; n <= 11; n++ {
		i := n
		if reversed {
			i = 11 - n
		}
		fmt.Fprintf(&b, `<outcome id="%d" name="%s %d"/>`, i, locale, i)
	}
	b.WriteString(`</outcomes></market></market_descriptions>`)
	return b.String()
}

func outcomeIDs(md types.MarketDescription) []string {
	ids := make([]string, 0, len(md.Outcomes))
	for _, o := range md.Outcomes {
		ids = append(ids, o.ID)
	}
	return ids
}

var wantAscending = []string{"0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11"}

// TestMarketDescription_OutcomesKeepUpstreamOrder is the regression for
// GAD-4104: Match Team Exact Score / Exact Number of Goals selections
// rendered as 0, 1, 10, 11, 2, 3, … The cache stored outcomes in a map
// and Snapshot re-sorted them lexically by ID to get a deterministic
// order back; the upstream catalog order — the one the selections are
// meant to display in — was lost at insertion. Outcomes must come out
// exactly as the catalog listed them, on every call.
func TestMarketDescription_OutcomesKeepUpstreamOrder(t *testing.T) {
	srv := newMarketSrv(t, exactGoalsCatalog(types.EnLocale, false))
	mc, ctx := newMarketCacheForTest(t, srv)

	entry, err := mc.MarketDescriptionByID(ctx, 26, types.None[string](), []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	for call := 0; call < 3; call++ {
		got := outcomeIDs(entry.Snapshot())
		if fmt.Sprint(got) != fmt.Sprint(wantAscending) {
			t.Fatalf("Snapshot #%d outcome order = %v, want upstream order %v (pre-fix: lexical 0,1,10,11,2,…)", call, got, wantAscending)
		}
	}

	// The bulk view projects the same entries — same order there too.
	all, err := mc.LocalizedMarketDescriptions(ctx, types.EnLocale)
	if err != nil {
		t.Fatalf("bulk view: %v", err)
	}
	if got := outcomeIDs(all[CompositeKey{MarketID: 26}].Snapshot()); fmt.Sprint(got) != fmt.Sprint(wantAscending) {
		t.Fatalf("bulk view outcome order = %v, want %v", got, wantAscending)
	}
}

// TestMarketDescription_SecondLocaleMergesIntoSameOrder pins merge
// semantics across locales in the realistic shape — upstream lists the
// same catalog order in every locale — so a later locale merges its
// names into the established entries without disturbing the order.
// (When rows DO disagree on order, the newest row's listing wins — see
// TestMarketDescription_RefreshedCatalogReordersOutcomes — because the
// public contract is the CURRENT upstream catalog order.)
func TestMarketDescription_SecondLocaleMergesIntoSameOrder(t *testing.T) {
	srv := newMarketSrv(t, exactGoalsCatalog(types.EnLocale, false))
	mc, ctx := newMarketCacheForTest(t, srv)

	if _, err := mc.MarketDescriptionByID(ctx, 26, types.None[string](), []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("en lookup: %v", err)
	}

	srv.serve(exactGoalsCatalog(types.RuLocale, false)) // identical listing, ru names
	entry, err := mc.MarketDescriptionByID(ctx, 26, types.None[string](), []types.Locale{types.EnLocale, types.RuLocale})
	if err != nil {
		t.Fatalf("en+ru lookup: %v", err)
	}
	snap := entry.Snapshot()
	if got := outcomeIDs(snap); fmt.Sprint(got) != fmt.Sprint(wantAscending) {
		t.Fatalf("outcome order after ru merge = %v, want %v", got, wantAscending)
	}
	if n := snap.Outcomes[10].Names; n[types.EnLocale] != "en 10" || n[types.RuLocale] != "ru 10" {
		t.Fatalf("outcome 10 names = %v, want en+ru on the same entry", n)
	}
}

// TestMarketDescription_RefreshedCatalogReordersOutcomes pins the order
// as the CURRENT upstream catalog's, not the first-seen one. Fixing
// each position at first sight diverged permanently on an upstream
// insertion — a cached [0, 2] refreshed against [0, 1, 2] yielded
// [0, 2, 1] forever — and ignored deliberate upstream reorders.
func TestMarketDescription_RefreshedCatalogReordersOutcomes(t *testing.T) {
	catalog := func(ids ...string) string {
		var b strings.Builder
		b.WriteString(`<?xml version="1.0"?><market_descriptions response_code="OK">`)
		b.WriteString(`<market id="26" name="Exact number of goals"><outcomes>`)
		for _, id := range ids {
			fmt.Fprintf(&b, `<outcome id="%s" name="n%s"/>`, id, id)
		}
		b.WriteString(`</outcomes></market></market_descriptions>`)
		return b.String()
	}
	srv := newMarketSrv(t, catalog("0", "2"))
	mc, ctx := newMarketCacheForTest(t, srv)
	mc.catalogTTL = 50 * time.Millisecond

	if _, err := mc.MarketDescriptionByID(ctx, 26, types.None[string](), []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("seed lookup: %v", err)
	}

	// Upstream inserts outcome 1 between 0 and 2.
	srv.serve(catalog("0", "1", "2"))
	time.Sleep(80 * time.Millisecond)
	entry, err := mc.MarketDescriptionByID(ctx, 26, types.None[string](), []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("lookup after insertion: %v", err)
	}
	if got := outcomeIDs(entry.Snapshot()); fmt.Sprint(got) != fmt.Sprint([]string{"0", "1", "2"}) {
		t.Fatalf("order after upstream insertion = %v, want [0 1 2] (pre-fix: [0 2 1] forever)", got)
	}

	// A deliberate upstream reorder lands too.
	srv.serve(catalog("2", "1", "0"))
	time.Sleep(80 * time.Millisecond)
	entry, err = mc.MarketDescriptionByID(ctx, 26, types.None[string](), []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("lookup after reorder: %v", err)
	}
	if got := outcomeIDs(entry.Snapshot()); fmt.Sprint(got) != fmt.Sprint([]string{"2", "1", "0"}) {
		t.Fatalf("order after upstream reorder = %v, want [2 1 0]", got)
	}
}

// TestMarketDescription_MergeOrderAuthority pins the two halves of the
// order authority. A NEWEST row's listing defines the order outright —
// its outcomes first, in row order ("12+" never interleaves lexically
// between "11" and "2"), with sweep survivors (outcomes retained on
// fresh cross-locale disagreement) trailing in their previous relative
// order. A STALE-started row has no order authority: its unknown
// additions append at the end and nothing reshuffles, mirroring the
// sweep/metadata monotonic rules — so the final order never depends on
// completion order.
func TestMarketDescription_MergeOrderAuthority(t *testing.T) {
	row := func(ids ...string) data.MarketDescription {
		outcomes := make([]data.MarketDescriptionOutcome, 0, len(ids))
		for _, id := range ids {
			outcomes = append(outcomes, data.MarketDescriptionOutcome{ID: id, Name: "n" + id})
		}
		return data.MarketDescription{
			ID: 26, Name: "Exact number of goals",
			Outcomes: &data.OutcomesWrapper{Outcome: outcomes},
		}
	}
	ids := func(d *LocalizedMarketDescription) []string {
		out := make([]string, 0, len(d.outcomes))
		for _, lo := range d.outcomes {
			out = append(out, lo.id)
		}
		return out
	}

	// NEWEST row: [12+, 0] listing wins; the seeded outcomes 1..11 are
	// retained by the sweep (fresh de names) and trail in order.
	d := &LocalizedMarketDescription{id: 26, name: map[types.Locale]string{}}
	for _, id := range wantAscending {
		d.addOutcomeLocked(id).name[types.DeLocale] = "de " + id
	}
	newer := time.Now()
	d.merge(row("12+", "0"), types.EnLocale, newer, nil)
	want := make([]string, 0, len(wantAscending)+2)
	want = append(want, "12+")
	want = append(want, wantAscending...)
	if got := ids(d); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("order after newest merge = %v, want %v (row order first, survivors trail)", got, want)
	}
	if d.outcomeByID["12+"] == nil || d.outcomeByID["12+"] != d.outcomes[0] {
		t.Fatal("index map and ordered slice diverged for the row-placed outcome")
	}

	// STALE-started row (older loadStarted): adds "13+" at the END, and
	// the newest-established order is untouched.
	d.merge(row("13+", "0", "12+"), types.RuLocale, newer.Add(-time.Second), nil)
	want = append(want, "13+")
	if got := ids(d); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("order after stale-started merge = %v, want %v (no order authority, additions append)", got, want)
	}
}

// TestMarketDescription_RemovalPreservesOrder pins the removal paths
// the replace-semantics rework added AFTER the original ordering fix:
// merge's sweep and removeLocale drop outcomes through
// filterOutcomesLocked, which must compact the ordered slice in place
// (survivors keep their relative catalog order) and keep the index map
// in lockstep — and a later re-add lands at the END, not at its old
// position.
func TestMarketDescription_RemovalPreservesOrder(t *testing.T) {
	catalog := func(ids ...string) string {
		var b strings.Builder
		b.WriteString(`<?xml version="1.0"?><market_descriptions response_code="OK">`)
		b.WriteString(`<market id="26" name="Exact number of goals"><outcomes>`)
		for _, id := range ids {
			fmt.Fprintf(&b, `<outcome id="%s" name="n%s"/>`, id, id)
		}
		b.WriteString(`</outcomes></market></market_descriptions>`)
		return b.String()
	}
	srv := newMarketSrv(t, catalog(wantAscending...))
	mc, ctx := newMarketCacheForTest(t, srv)
	mc.catalogTTL = 50 * time.Millisecond

	if _, err := mc.MarketDescriptionByID(ctx, 26, types.None[string](), []types.Locale{types.EnLocale}); err != nil {
		t.Fatalf("seed lookup: %v", err)
	}

	// Upstream removes 1 and 10 (middle entries): survivors must keep
	// their relative order with no lexical reshuffle.
	srv.serve(catalog("0", "2", "3", "4", "5", "6", "7", "8", "9", "11"))
	time.Sleep(80 * time.Millisecond)
	entry, err := mc.MarketDescriptionByID(ctx, 26, types.None[string](), []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("lookup after removal: %v", err)
	}
	wantAfterRemoval := []string{"0", "2", "3", "4", "5", "6", "7", "8", "9", "11"}
	if got := outcomeIDs(entry.Snapshot()); fmt.Sprint(got) != fmt.Sprint(wantAfterRemoval) {
		t.Fatalf("order after removal = %v, want %v (compaction must preserve survivor order)", got, wantAfterRemoval)
	}

	// Outcome 1 comes back upstream: it re-enters at the END (its old
	// slot is gone — position is fixed at first sight, and this row's
	// listing puts it last anyway).
	srv.serve(catalog("0", "2", "3", "4", "5", "6", "7", "8", "9", "11", "1"))
	time.Sleep(80 * time.Millisecond)
	entry, err = mc.MarketDescriptionByID(ctx, 26, types.None[string](), []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("lookup after re-add: %v", err)
	}
	wantAfterReAdd := []string{"0", "2", "3", "4", "5", "6", "7", "8", "9", "11", "1"}
	if got := outcomeIDs(entry.Snapshot()); fmt.Sprint(got) != fmt.Sprint(wantAfterReAdd) {
		t.Fatalf("order after re-add = %v, want %v", got, wantAfterReAdd)
	}
}
