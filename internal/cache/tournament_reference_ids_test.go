package cache

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	apiXML "github.com/oddin-gg/gosdk/internal/api/xml"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

// Wire path: reference_ids on /tournaments/{id}/info must decode and
// land on the cached entry.
func TestTournamentCache_ReferenceIDsDecode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<?xml version="1.0"?>
<tournament_info generated_at="2026-01-01T00:00:00Z">
  <tournament id="od:tournament:7" name="T">
    <sport id="od:sport:1" name="Football"/>
    <reference_ids>
      <reference_id name="oddin" value="od:tournament:14342"/>
      <reference_id name="external" value="xyz789"/>
    </reference_ids>
  </tournament>
  <competitors><competitor id="od:competitor:1"/></competitors>
</tournament_info>`)
	}))
	defer srv.Close()

	tc := newTournamentCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))
	id := types.URN{Prefix: "od", Type: "tournament", ID: 7}

	entry, err := tc.Tournament(t.Context(), id, []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("Tournament: %v", err)
	}

	entry.mu.RLock()
	got := entry.referenceIDs
	entry.mu.RUnlock()

	if want := "od:tournament:14342"; got["oddin"] != want {
		t.Errorf("referenceIDs[oddin] = %q, want %q", got["oddin"], want)
	}
	if want := "xyz789"; got["external"] != want {
		t.Errorf("referenceIDs[external] = %q, want %q", got["external"], want)
	}
	if len(got) != 2 {
		t.Errorf("referenceIDs size = %d, want 2", len(got))
	}
}

// Public projection: the decoded map reaches types.Tournament, and the
// map handed to the caller must not alias the cache entry.
func TestBuildTournament_ReferenceIDsSurfaceOnSnapshot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<?xml version="1.0"?>
<tournament_info generated_at="2026-01-01T00:00:00Z">
  <tournament id="od:tournament:7" name="T">
    <sport id="od:sport:1" name="Football"/>
    <reference_ids>
      <reference_id name="oddin" value="od:tournament:14342"/>
    </reference_ids>
  </tournament>
  <competitors><competitor id="od:competitor:1"/></competitors>
</tournament_info>`)
	}))
	defer srv.Close()

	tc := newTournamentCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))
	id := types.URN{Prefix: "od", Type: "tournament", ID: 7}
	sportID := types.URN{Prefix: "od", Type: "sport", ID: 1}

	out, err := BuildTournament(t.Context(), tc, &recordingSportFactory{}, id, sportID, []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("BuildTournament: %v", err)
	}
	if want := "od:tournament:14342"; out.ReferenceIDs["oddin"] != want {
		t.Fatalf("ReferenceIDs[oddin] = %q, want %q", out.ReferenceIDs["oddin"], want)
	}

	// Mutating the snapshot must not reach the cache.
	out.ReferenceIDs["oddin"] = "tampered"
	again, err := BuildTournament(t.Context(), tc, &recordingSportFactory{}, id, sportID, []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("BuildTournament (second): %v", err)
	}
	if want := "od:tournament:14342"; again.ReferenceIDs["oddin"] != want {
		t.Errorf("ReferenceIDs[oddin] = %q after caller mutation, want %q (snapshot must copy out)", again.ReferenceIDs["oddin"], want)
	}
}

// An empty block is a statement ("no mappings"), an absent one isn't, so
// it must project a non-nil empty map. Guards the `block != nil` test in
// merge() against being narrowed to `len(block.ReferenceID) > 0`.
func TestBuildTournament_EmptyReferenceIDsBlockProjectsEmptyMap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<?xml version="1.0"?>
<tournament_info generated_at="2026-01-01T00:00:00Z">
  <tournament id="od:tournament:7" name="T">
    <sport id="od:sport:1" name="Football"/>
    <reference_ids></reference_ids>
  </tournament>
  <competitors><competitor id="od:competitor:1"/></competitors>
</tournament_info>`)
	}))
	defer srv.Close()

	tc := newTournamentCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))
	id := types.URN{Prefix: "od", Type: "tournament", ID: 7}
	sportID := types.URN{Prefix: "od", Type: "sport", ID: 1}

	out, err := BuildTournament(t.Context(), tc, &recordingSportFactory{}, id, sportID, []types.Locale{types.EnLocale})
	if err != nil {
		t.Fatalf("BuildTournament: %v", err)
	}
	if out.ReferenceIDs == nil {
		t.Fatal("ReferenceIDs = nil, want a non-nil empty map (an empty block is not an absent one)")
	}
	if got := len(out.ReferenceIDs); got != 0 {
		t.Errorf("len(ReferenceIDs) = %d, want 0", got)
	}
}

// A payload with no reference_ids must leave a known mapping intact.
func TestMerge_MissingReferenceIDsDoesNotClobber(t *testing.T) {
	l := newTestLocalizedTournament()

	withRefs := apiXML.TournamentExtended{
		Tournament: apiXML.Tournament{
			ID:    "od:tournament:1",
			Sport: apiXML.Sport{ID: "od:sport:1"},
			Name:  "Premier League",
			ReferenceIDs: &apiXML.ReferenceIDs{
				ReferenceID: []apiXML.ReferenceID{{Name: "oddin", Value: "od:tournament:14342"}},
			},
		},
	}
	if err := l.merge(types.EnLocale, withRefs); err != nil {
		t.Fatalf("merge (with refs): %v", err)
	}

	// A list-shaped payload: same tournament, no reference_ids block.
	withoutRefs := apiXML.Tournament{
		ID:    "od:tournament:1",
		Sport: apiXML.Sport{ID: "od:sport:1"},
		Name:  "Premier League",
	}
	if err := l.merge(types.EnLocale, withoutRefs); err != nil {
		t.Fatalf("merge (without refs): %v", err)
	}

	l.mu.RLock()
	got := l.referenceIDs["oddin"]
	l.mu.RUnlock()
	if want := "od:tournament:14342"; got != want {
		t.Errorf("referenceIDs[oddin] = %q after a payload with no reference_ids, want %q (must not clobber)", got, want)
	}
}

// A payload that does carry the block replaces the whole set, so a
// withdrawn reference id disappears.
func TestMerge_ReferenceIDsRefreshReplacesSet(t *testing.T) {
	l := newTestLocalizedTournament()

	base := apiXML.Tournament{
		ID:    "od:tournament:1",
		Sport: apiXML.Sport{ID: "od:sport:1"},
		Name:  "Premier League",
		ReferenceIDs: &apiXML.ReferenceIDs{
			ReferenceID: []apiXML.ReferenceID{
				{Name: "oddin", Value: "od:tournament:14342"},
				{Name: "stale", Value: "gone-next-time"},
			},
		},
	}
	if err := l.merge(types.EnLocale, base); err != nil {
		t.Fatalf("merge (base): %v", err)
	}

	refreshed := base
	refreshed.ReferenceIDs = &apiXML.ReferenceIDs{
		ReferenceID: []apiXML.ReferenceID{{Name: "oddin", Value: "od:tournament:99999"}},
	}
	if err := l.merge(types.EnLocale, refreshed); err != nil {
		t.Fatalf("merge (refreshed): %v", err)
	}

	l.mu.RLock()
	got := l.referenceIDs
	l.mu.RUnlock()
	if want := "od:tournament:99999"; got["oddin"] != want {
		t.Errorf("referenceIDs[oddin] = %q, want %q", got["oddin"], want)
	}
	if _, ok := got["stale"]; ok {
		t.Error("referenceIDs still carries the withdrawn \"stale\" entry; refresh must replace the whole set")
	}
}
