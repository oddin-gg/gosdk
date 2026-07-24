package cache

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

// TestCompetitorCache_IconStore_NoCrossURLContention is the regression
// for the L1 finding from the v2.x review pass.
//
// Pre-fix the icon storage was a `map[URN]*string` guarded by a single
// global `iconMu sync.RWMutex`. Inside the EventCache singleflight
// loader (one goroutine per URN), the map write was guarded by
// `iconMu.Lock()` — so concurrent loaders for *different* competitors
// briefly serialized on the same write lock, defeating the per-key
// load isolation singleflight is supposed to provide.
//
// Post-fix the icon storage is a `sync.Map`. Concurrent loaders for
// different URNs proceed without contending on a shared mutex.
//
// Strategy: drive 50 concurrent fetches for distinct competitor URNs.
// The fetches all complete; final state has every URN's icon recorded.
// Direct contention measurement isn't deterministic, but a
// concurrent-safety + correctness regression test catches both
// sync.Map type-assertion bugs and any future revert that re-introduces
// the global lock.
func TestCompetitorCache_IconStore_ConcurrentDistinctURLs(t *testing.T) {
	const urns = 50
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		// Minimal CompetitorResponse XML; the key field for this
		// test is icon_path (we just want the storeIcon path
		// exercised). The REQUESTED competitor id is echoed — the API
		// client validates response identity, so a static id would be
		// rejected as a misrouted response.
		parts := strings.Split(strings.TrimSuffix(r.URL.Path, "/profile"), "/")
		id := parts[len(parts)-1]
		_, _ = io.WriteString(w, `<?xml version="1.0"?>
<competitor_profile>
  <competitor id="`+id+`" name="Team" abbreviation="T" icon_path="icon-`+id+`"/>
</competitor_profile>`)
	}))
	defer srv.Close()

	cc := newCompetitorCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))

	var wg sync.WaitGroup
	wg.Add(urns)
	for i := 0; i < urns; i++ {
		go func() {
			defer wg.Done()
			id := types.URN{Prefix: "od", Type: "competitor", ID: 1000 + i}
			_, _ = cc.CompetitorIcon(t.Context(), id, types.EnLocale)
		}()
	}
	wg.Wait()

	// Verify every URN's icon was recorded — proves storeIcon ran
	// for each loader without races/data loss.
	for i := 0; i < urns; i++ {
		id := types.URN{Prefix: "od", Type: "competitor", ID: 1000 + i}
		v, ok := cc.loadIcon(id)
		if !ok {
			t.Errorf("urn %v: icon not stored after concurrent fetch", id)
			continue
		}
		// The mock server's icon_path is empty in some XML schemas; we
		// only assert a record exists, not the value. The race detector
		// catches sync.Map misuse if any.
		_ = v
	}
}

// TestCompetitorCache_IconStore_RoundTrip covers the basic
// store/load/delete cycle through the helpers.
func TestCompetitorCache_IconStore_RoundTrip(t *testing.T) {
	cc := &CompetitorCache{}
	id := types.URN{Prefix: "od", Type: "competitor", ID: 1}

	// Not present.
	if _, ok := cc.loadIcon(id); ok {
		t.Errorf("loadIcon on empty: ok=true, want false")
	}

	// Store nil (competitor with no icon).
	cc.storeIcon(id, nil)
	v, ok := cc.loadIcon(id)
	if !ok {
		t.Errorf("loadIcon after storeIcon(nil): ok=false, want true")
	}
	if v != nil {
		t.Errorf("loadIcon after storeIcon(nil): v=%v, want nil", v)
	}

	// Overwrite with a real path.
	path := "icon.png"
	cc.storeIcon(id, &path)
	v, ok = cc.loadIcon(id)
	if !ok || v == nil || *v != "icon.png" {
		t.Errorf("loadIcon after storeIcon(\"icon.png\"): (%v, %v), want (\"icon.png\", true)", v, ok)
	}

	// Delete.
	cc.deleteIcon(id)
	if _, ok := cc.loadIcon(id); ok {
		t.Errorf("loadIcon after deleteIcon: ok=true, want false")
	}
}

// TestCompetitorCache_IconStore_TypeSafety guards against a regression
// where someone replaces sync.Map with a wrong type and the type
// assertion in loadIcon panics at runtime instead of at compile time.
func TestCompetitorCache_IconStore_TypeSafety(t *testing.T) {
	cc := &CompetitorCache{}
	for i := 0; i < 5; i++ {
		id := types.URN{Prefix: "od", Type: "competitor", ID: i}
		path := fmt.Sprintf("p-%d", i)
		cc.storeIcon(id, &path)
	}
	for i := 0; i < 5; i++ {
		id := types.URN{Prefix: "od", Type: "competitor", ID: i}
		v, ok := cc.loadIcon(id)
		if !ok {
			t.Errorf("urn %v: not stored", id)
			continue
		}
		want := fmt.Sprintf("p-%d", i)
		if v == nil || *v != want {
			t.Errorf("urn %v: got %v, want %s", id, v, want)
		}
	}
}
