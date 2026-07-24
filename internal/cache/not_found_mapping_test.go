package cache

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/oddin-gg/gosdk/internal/api"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

// notFoundServer always answers 404 with an upstream error envelope.
func notFoundServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<?xml version="1.0"?><error><message>no such event</message></error>`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestMatchStatusCache_Upstream404_MapsToNotFound pins the council P1
// contract gap: the exported ErrItemNotFound documentation explicitly
// names match-status reads, but the summary-fetch error was never
// passed through notFoundIfAbsent — consumers could not distinguish a
// permanent 404 from a transport failure and retried it forever.
func TestMatchStatusCache_Upstream404_MapsToNotFound(t *testing.T) {
	srv := notFoundServer(t)
	msc := newMatchStatusCache(t.Context(), newAPIClientForTest(t, srv), &fakeCacheCfg{}, log.New(nil))
	id := types.URN{Prefix: "od", Type: "match", ID: 404}

	_, err := msc.MatchStatus(context.Background(), id)
	if !errors.Is(err, ErrItemNotFoundInCache) {
		t.Fatalf("MatchStatus on upstream 404 = %v, want ErrItemNotFoundInCache", err)
	}
	// The original APIError must stay in the chain for errors.As.
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		t.Fatalf("MatchStatus on upstream 404: APIError with Status=404 not in chain: %v", err)
	}
}

// TestFixtureCache_Upstream404_MapsToNotFound pins the same contract
// for fixtures: a definitive upstream 404 on the fixture fetch must
// classify as ErrItemNotFoundInCache (parity with match/tournament/
// competitor/player), not propagate as a bare retryable APIError.
func TestFixtureCache_Upstream404_MapsToNotFound(t *testing.T) {
	srv := notFoundServer(t)
	fc := newFixtureCache(t.Context(), newAPIClientForTest(t, srv))
	id := types.URN{Prefix: "od", Type: "match", ID: 404}

	_, err := fc.Fixture(context.Background(), id, []types.Locale{types.EnLocale})
	if !errors.Is(err, ErrItemNotFoundInCache) {
		t.Fatalf("Fixture on upstream 404 = %v, want ErrItemNotFoundInCache", err)
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		t.Fatalf("Fixture on upstream 404: APIError with Status=404 not in chain: %v", err)
	}
}
