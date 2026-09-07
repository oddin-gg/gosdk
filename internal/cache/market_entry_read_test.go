package cache

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	data "github.com/oddin-gg/gosdk/internal/api/xml"
	"github.com/oddin-gg/gosdk/types"
)

// These tests pin the direct, lock-scoped reads on a cached market
// description (Name / OutcomeName / ReadOutcomeName / OutcomeTypeValue
// / HasGroup) that
// the message-build path uses instead of Snapshot() — CORE-4213.
// Snapshot() copied the WHOLE description (a map per outcome) to read
// one string, per outcome, per locale; on production traffic that was
// ~4 MB of garbage per odds_change on the session goroutine.

// newEntryForRead builds an unpublished entry with n outcomes named in
// every given locale, the way upsert+merge would leave it.
func newEntryForRead(n int, locales ...types.Locale) *LocalizedMarketDescription {
	playerType := "player"
	d := &LocalizedMarketDescription{
		id:          7,
		OutcomeType: &playerType,
		outcomeByID: make(map[string]*LocalizedOutcomeDescription, n),
		name:        make(map[types.Locale]string, len(locales)),
		groups:      []string{"all", types.MarketGroupPlayerProps},
	}
	for _, l := range locales {
		d.name[l] = "Market " + string(l)
	}
	for i := 0; i < n; i++ {
		lo := d.addOutcomeLocked(fmt.Sprint(i))
		for _, l := range locales {
			lo.name[l] = fmt.Sprintf("Outcome %d %s", i, l)
		}
	}
	return d
}

func TestLocalizedMarketDescription_Name(t *testing.T) {
	d := newEntryForRead(2, types.EnLocale)
	d.name[types.RuLocale] = "" // loaded-but-empty catalog name

	if got, ok := d.Name(types.EnLocale); !ok || got != "Market en" {
		t.Fatalf("Name(en) = %q, %v; want Market en, true", got, ok)
	}
	// Loaded-but-empty is a hit — the factory turns it into Some("").
	if got, ok := d.Name(types.RuLocale); !ok || got != "" {
		t.Fatalf("Name(ru) = %q, %v; want \"\", true", got, ok)
	}
	if got, ok := d.Name(types.DeLocale); ok {
		t.Fatalf("Name(de) = %q, %v; want miss", got, ok)
	}
	// Agrees with the projection the catalog API hands out.
	if snap := d.Snapshot(); snap.Names[types.EnLocale] != "Market en" {
		t.Fatalf("Snapshot().Names = %v", snap.Names)
	}
}

func TestLocalizedMarketDescription_OutcomeName(t *testing.T) {
	d := newEntryForRead(3, types.EnLocale)

	name, exists, ok := d.OutcomeName("1", types.EnLocale)
	if !exists || !ok || name != "Outcome 1 en" {
		t.Fatalf("OutcomeName(1, en) = %q, exists=%v ok=%v", name, exists, ok)
	}
	// Known outcome, locale not loaded: exists but no hit — the factory
	// reports None and must NOT take the dynamic-outcome branch.
	name, exists, ok = d.OutcomeName("1", types.RuLocale)
	if !exists || ok || name != "" {
		t.Fatalf("OutcomeName(1, ru) = %q, exists=%v ok=%v; want exists, miss", name, exists, ok)
	}
	// Unknown outcome: the dynamic-outcome branch's trigger.
	name, exists, ok = d.OutcomeName("od:player:100", types.EnLocale)
	if exists || ok || name != "" {
		t.Fatalf("OutcomeName(unknown) = %q, exists=%v ok=%v; want neither", name, exists, ok)
	}
	// Same answer the Snapshot() scan gave.
	for _, o := range d.Snapshot().Outcomes {
		if got, _, _ := d.OutcomeName(o.ID, types.EnLocale); got != o.Names[types.EnLocale] {
			t.Fatalf("outcome %s: direct %q vs snapshot %q", o.ID, got, o.Names[types.EnLocale])
		}
	}
}

// TestLocalizedMarketDescription_ReadOutcomeName pins the combined
// read the message path takes: it must answer exactly what the separate
// OutcomeName / OutcomeTypeValue accessors do, only in one lock scope,
// so a concurrent merge cannot mix two catalog revisions into one
// outcome's answer.
func TestLocalizedMarketDescription_ReadOutcomeName(t *testing.T) {
	d := newEntryForRead(3, types.EnLocale, types.RuLocale)
	d.outcomeByID["1"].name[types.RuLocale] = "" // loaded-but-empty
	delete(d.outcomeByID["2"].name, types.RuLocale)

	read := d.ReadOutcomeName("1", types.RuLocale, types.EnLocale)
	if !read.Exists || !read.NameOK || read.Name != "" {
		t.Fatalf("ru name of outcome 1 = %q, ok=%v exists=%v; want loaded-but-empty", read.Name, read.NameOK, read.Exists)
	}
	if !read.CanonicalOK || read.Canonical != "Outcome 1 en" {
		t.Fatalf("canonical = %q, ok=%v; want Outcome 1 en", read.Canonical, read.CanonicalOK)
	}
	if got, ok := read.OutcomeType.Get(); !ok || got != "player" {
		t.Fatalf("OutcomeType = %q, %v; want player", got, ok)
	}

	// Locale not loaded on the outcome: exists, no hit — the factory
	// reports None and must NOT take the dynamic branch.
	read = d.ReadOutcomeName("2", types.RuLocale, types.EnLocale)
	if !read.Exists || read.NameOK || !read.CanonicalOK {
		t.Fatalf("outcome 2 in ru: exists=%v nameOK=%v canonicalOK=%v", read.Exists, read.NameOK, read.CanonicalOK)
	}

	// canonical == locale collapses to a single map read.
	read = d.ReadOutcomeName("0", types.EnLocale, types.EnLocale)
	if read.Name != read.Canonical || read.NameOK != read.CanonicalOK || read.Name != "Outcome 0 en" {
		t.Fatalf("en read = %+v; want name == canonical", read)
	}

	// Unknown outcome: the dynamic branch's trigger, with the
	// outcome_type that decides it out of the same lock scope.
	read = d.ReadOutcomeName("od:player:100", types.RuLocale, types.EnLocale)
	if read.Exists || read.NameOK || read.CanonicalOK {
		t.Fatalf("unknown outcome = %+v; want no hit", read)
	}
	if got, ok := read.OutcomeType.Get(); !ok || got != "player" {
		t.Fatalf("unknown outcome OutcomeType = %q, %v; want player", got, ok)
	}

	// Agrees with the separate accessors it replaces.
	for _, id := range []string{"0", "1", "2", "od:player:100"} {
		name, exists, ok := d.OutcomeName(id, types.RuLocale)
		read := d.ReadOutcomeName(id, types.RuLocale, types.EnLocale)
		if read.Name != name || read.Exists != exists || read.NameOK != ok {
			t.Fatalf("outcome %s: combined %+v vs OutcomeName(%q, %v, %v)", id, read, name, exists, ok)
		}
	}
}

func TestLocalizedMarketDescription_OutcomeTypeAndGroups(t *testing.T) {
	d := newEntryForRead(1, types.EnLocale)
	if got, ok := d.OutcomeTypeValue().Get(); !ok || got != "player" {
		t.Fatalf("OutcomeTypeValue = %q, %v", got, ok)
	}
	d.OutcomeType = nil
	if _, ok := d.OutcomeTypeValue().Get(); ok {
		t.Fatal("OutcomeTypeValue with nil field = Some, want None")
	}
	if !d.HasGroup(types.MarketGroupPlayerProps) || d.HasGroup("nope") {
		t.Fatalf("HasGroup: player_props=%v nope=%v", d.HasGroup(types.MarketGroupPlayerProps), d.HasGroup("nope"))
	}
}

// TestLocalizedMarketDescription_DirectReadsAllocateNothing is the
// point of the change: none of the hot-path reads may allocate,
// whatever the description's size. Snapshot() allocated 2N+4 maps and
// slices for the same answer.
func TestLocalizedMarketDescription_DirectReadsAllocateNothing(t *testing.T) {
	d := newEntryForRead(350, types.EnLocale, types.RuLocale)
	reads := map[string]func(){
		"Name":             func() { d.Name(types.RuLocale) },
		"OutcomeName":      func() { d.OutcomeName("249", types.RuLocale) },
		"OutcomeName miss": func() { d.OutcomeName("od:player:1", types.RuLocale) },
		"ReadOutcomeName":  func() { d.ReadOutcomeName("249", types.RuLocale, types.EnLocale) },
		"OutcomeTypeValue": func() { d.OutcomeTypeValue() },
		"HasGroup":         func() { d.HasGroup(types.MarketGroupPlayerProps) },
	}
	for name, read := range reads {
		if allocs := testing.AllocsPerRun(100, read); allocs != 0 {
			t.Errorf("%s: %.0f allocs/op, want 0", name, allocs)
		}
	}
	if allocs := testing.AllocsPerRun(10, func() { d.Snapshot() }); allocs < 700 {
		t.Errorf("Snapshot(): %.0f allocs/op — expected the copy this test exists to avoid (≥ 2 per outcome)", allocs)
	}
}

// BenchmarkOutcomeNameRead compares the two ways of answering "what is
// outcome X called in locale L" on a 350-outcome description: the
// Snapshot() projection the message path used to take, and the direct
// read it takes now. -benchmem shows the difference that matters.
func BenchmarkOutcomeNameRead(b *testing.B) {
	for _, n := range []int{2, 121, 348} {
		d := newEntryForRead(n, types.EnLocale, types.RuLocale)
		id := fmt.Sprint(n / 2)
		b.Run(fmt.Sprintf("snapshot/outcomes=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				snap := d.Snapshot()
				for _, o := range snap.Outcomes {
					if o.ID == id {
						_ = o.Names[types.RuLocale]
						break
					}
				}
			}
		})
		b.Run(fmt.Sprintf("direct/outcomes=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, _, _ = d.OutcomeName(id, types.RuLocale)
			}
		})
	}
}

// BenchmarkMarketDescriptionByID_WarmHit measures the by-id cache
// lookup that still precedes every read: its locale-coverage check
// walks every outcome (twice), which is why the factory memoizes the
// lookup per market (see marketDataImpl.description).
func BenchmarkMarketDescriptionByID_WarmHit(b *testing.B) {
	mc := newMarketDescriptionCache(b.Context(), nil, nil)
	for _, n := range []int{2, 348} {
		id := 100 + n
		outcomes := make([]data.MarketDescriptionOutcome, n)
		for i := range outcomes {
			outcomes[i] = data.MarketDescriptionOutcome{ID: fmt.Sprint(i), Name: fmt.Sprintf("o%d", i)}
		}
		for _, l := range []types.Locale{types.EnLocale, types.RuLocale} {
			if err := mc.upsert(data.MarketDescription{ID: id, Name: "m", Outcomes: &data.OutcomesWrapper{Outcome: outcomes}}, l, time.Now()); err != nil {
				b.Fatal(err)
			}
			mc.mu.Lock()
			mc.loadedLocales[l] = time.Now()
			mc.mu.Unlock()
		}
		b.Run(fmt.Sprintf("outcomes=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			locales := []types.Locale{types.RuLocale, types.EnLocale}
			for i := 0; i < b.N; i++ {
				if _, err := mc.MarketDescriptionByID(b.Context(), id, types.None[string](), locales); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// readRevision builds one catalog revision of market 7 for the
// concurrency test below. rev tags every string, so a name can be
// traced back to the revision and the locale that produced it; dynamic
// swings the market between its two shapes — outcome "3" listed with
// outcome_type="competitor", or absent with outcome_type="player" and
// the player_props group. Those are exactly the fields merge rewrites
// in place under one d.mu.Lock, so they are the ones ReadOutcomeName
// must never mix across revisions.
func readRevision(rev string, locale types.Locale, dynamic bool) data.MarketDescription {
	outcomes := []data.MarketDescriptionOutcome{
		{ID: "1", Name: fmt.Sprintf("o1-%s-%s", locale, rev)},
		{ID: "2", Name: fmt.Sprintf("o2-%s-%s", locale, rev)},
	}
	outcomeType, groups := "competitor", "all"
	if dynamic {
		outcomeType, groups = "player", "all|"+types.MarketGroupPlayerProps
	} else {
		outcomes = append(outcomes, data.MarketDescriptionOutcome{ID: "3", Name: fmt.Sprintf("o3-%s-%s", locale, rev)})
	}
	return data.MarketDescription{
		ID:          7,
		Name:        fmt.Sprintf("market-%s-%s", locale, rev),
		OutcomeType: &outcomeType,
		Groups:      groups,
		Outcomes:    &data.OutcomesWrapper{Outcome: outcomes},
	}
}

// TestLocalizedMarketDescription_ReadsRaceWithMerge runs the direct
// reads against an entry a catalog refresh is rewriting underneath
// them — the situation every RLock in the accessors exists for, and
// which the rest of this file (single-threaded, hand-built entries)
// does not reach. Two things are under test: the reads are race-clean
// against merge under -race, and ReadOutcomeName's cross-field answer
// comes from ONE revision (the point of c6d5df2 — composing it from
// separate OutcomeName / OutcomeTypeValue calls let a merge land in
// between and fire the dynamic player branch for a listed outcome).
func TestLocalizedMarketDescription_ReadsRaceWithMerge(t *testing.T) {
	const merges = 400
	notStale := func(types.Locale) bool { return false }

	// readLoop runs read until done closes, so the readers cover the
	// whole merge sequence rather than a fixed number of iterations. A
	// reader that reports a failure stops, so one torn read does not
	// bury the log under a few thousand copies of itself.
	readLoop := func(wg *sync.WaitGroup, done <-chan struct{}, readers int, read func() bool) {
		for i := 0; i < readers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-done:
						return
					default:
					}
					if !read() {
						return
					}
				}
			}()
		}
	}

	t.Run("outcome set and outcome_type stay one revision", func(t *testing.T) {
		// One locale: the outcome set, outcome_type and groups all move
		// in a single merge call, so "outcome 3 is listed" and "this is
		// a player market" must agree in every observed read.
		d := &LocalizedMarketDescription{id: 7, name: map[types.Locale]string{}}
		base := time.Now()
		d.merge(readRevision("A", types.EnLocale, false), types.EnLocale, base, notStale)

		var wg sync.WaitGroup
		done := make(chan struct{})
		readLoop(&wg, done, 4, func() bool {
			read := d.ReadOutcomeName("3", types.EnLocale, types.EnLocale)
			outcomeType, _ := read.OutcomeType.Get()
			switch {
			case read.Exists && outcomeType != "competitor":
				t.Errorf("outcome 3 listed but outcome_type = %q; the pair straddled a merge", outcomeType)
				return false
			case !read.Exists && outcomeType != "player":
				t.Errorf("outcome 3 absent but outcome_type = %q; the pair straddled a merge", outcomeType)
				return false
			case read.Exists && (!read.NameOK || !strings.HasPrefix(read.Name, "o3-en-")):
				t.Errorf("outcome 3 name = %q, ok=%v; want an o3-en-* hit", read.Name, read.NameOK)
				return false
			}
			// canonical == locale collapses to the localized read.
			if read.Name != read.Canonical || read.NameOK != read.CanonicalOK {
				t.Errorf("collapsed read disagrees with itself: %+v", read)
				return false
			}
			// An outcome no revision drops is always there and named.
			if o1 := d.ReadOutcomeName("1", types.EnLocale, types.EnLocale); !o1.Exists || !o1.NameOK || !strings.HasPrefix(o1.Name, "o1-en-") {
				t.Errorf("outcome 1 = %+v; want a permanent o1-en-* hit", o1)
				return false
			}
			if name, ok := d.Name(types.EnLocale); !ok || !strings.HasPrefix(name, "market-en-") {
				t.Errorf("market name = %q, ok=%v; want a market-en-* hit", name, ok)
				return false
			}
			_, _, _ = d.OutcomeName("2", types.EnLocale)
			_ = d.OutcomeTypeValue()
			_ = d.HasGroup(types.MarketGroupPlayerProps)
			return true
		})

		for i := 1; i <= merges; i++ {
			rev, dynamic := "A", false
			if i%2 == 1 {
				rev, dynamic = "B", true
			}
			// loadStarted must strictly increase: an equal timestamp is
			// not "newest", and merge would then apply the row's
			// own-locale outcome removal without its metadata — a
			// legitimate mixed state, not the tear under test.
			d.merge(readRevision(rev, types.EnLocale, dynamic), types.EnLocale, base.Add(time.Duration(i)*time.Millisecond), notStale)
		}
		close(done)
		wg.Wait()
	})

	t.Run("localized and canonical reads never bleed across locales", func(t *testing.T) {
		// Two locales refreshing concurrently: ReadOutcomeName reads the
		// localized name and the canonical English label the home/away
		// substitution keys on out of one lock scope, and each must come
		// from its own locale whichever revision is landing.
		d := &LocalizedMarketDescription{id: 7, name: map[types.Locale]string{}}
		base := time.Now()
		for _, l := range []types.Locale{types.EnLocale, types.RuLocale} {
			d.merge(readRevision("A", l, false), l, base, notStale)
		}

		var wg sync.WaitGroup
		done := make(chan struct{})
		readLoop(&wg, done, 4, func() bool {
			// Outcome 1 is in every revision of both locales, so both
			// reads are always hits.
			read := d.ReadOutcomeName("1", types.RuLocale, types.EnLocale)
			if !read.Exists || !read.NameOK || !strings.HasPrefix(read.Name, "o1-ru-") {
				t.Errorf("localized read = %q, ok=%v exists=%v; want an o1-ru-* hit", read.Name, read.NameOK, read.Exists)
				return false
			}
			if !read.CanonicalOK || !strings.HasPrefix(read.Canonical, "o1-en-") {
				t.Errorf("canonical read = %q, ok=%v; want an o1-en-* hit", read.Canonical, read.CanonicalOK)
				return false
			}
			// The swing outcome may be gone in either locale, but a hit
			// is never the other locale's string.
			if swing := d.ReadOutcomeName("3", types.RuLocale, types.EnLocale); swing.Exists {
				if swing.NameOK && !strings.HasPrefix(swing.Name, "o3-ru-") {
					t.Errorf("outcome 3 localized name = %q; want o3-ru-*", swing.Name)
					return false
				}
				if swing.CanonicalOK && !strings.HasPrefix(swing.Canonical, "o3-en-") {
					t.Errorf("outcome 3 canonical name = %q; want o3-en-*", swing.Canonical)
					return false
				}
			}
			if name, ok := d.Name(types.RuLocale); !ok || !strings.HasPrefix(name, "market-ru-") {
				t.Errorf("ru market name = %q, ok=%v; want a market-ru-* hit", name, ok)
				return false
			}
			_ = d.Snapshot()
			return true
		})

		var mergers sync.WaitGroup
		for _, l := range []types.Locale{types.EnLocale, types.RuLocale} {
			mergers.Add(1)
			go func(locale types.Locale) {
				defer mergers.Done()
				for i := 1; i <= merges; i++ {
					rev, dynamic := "A", false
					if i%2 == 1 {
						rev, dynamic = "B", true
					}
					d.merge(readRevision(rev, locale, dynamic), locale, base.Add(time.Duration(i)*time.Millisecond), notStale)
				}
			}(l)
		}
		mergers.Wait()
		close(done)
		wg.Wait()
	})
}
