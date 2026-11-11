package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/internal/cache/lru"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

const (
	iconCompetitorXML = `<?xml version="1.0"?>
<competitor_profile generated_at="2026-01-01T00:00:00Z">
  <competitor id="od:competitor:3" name="Team" abbreviation="TM" icon_path="/icons/c3.png"/>
</competitor_profile>`
	iconTournamentXML = `<?xml version="1.0"?>
<tournament_info generated_at="2026-01-01T00:00:00Z">
  <tournament id="od:tournament:7" name="T" icon_path="/icons/t7.png">
    <sport id="od:sport:1" name="Football"/>
  </tournament>
</tournament_info>`
)

// iconFixture is one icon lookup (competitor or tournament) over a server
// that holds every request until gate is closed.
type iconFixture struct {
	name    string
	icon    func(ctx context.Context) (*string, error)
	cached  func() bool
	clear   func()
	want    string
	hits    *atomic.Int32
	entered chan struct{}
	gate    chan struct{}
}

func (f *iconFixture) release() {
	select {
	case <-f.gate:
	default:
		close(f.gate)
	}
}

func newIconFixtures(t *testing.T) []*iconFixture {
	t.Helper()
	newServer := func(body string) (*httptest.Server, *iconFixture) {
		f := &iconFixture{hits: new(atomic.Int32), entered: make(chan struct{}, 64), gate: make(chan struct{})}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			f.hits.Add(1)
			f.entered <- struct{}{}
			select {
			case <-f.gate:
			case <-r.Context().Done():
				return
			}
			w.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(w, body)
		}))
		t.Cleanup(srv.Close)
		t.Cleanup(f.release)
		return srv, f
	}

	csrv, cf := newServer(iconCompetitorXML)
	cc := newCompetitorCache(t.Context(), newAPIClientForTest(t, csrv), log.New(nil))
	cid := types.URN{Prefix: "od", Type: "competitor", ID: 3}
	cf.name, cf.want = "competitor", "/icons/c3.png"
	cf.icon = func(ctx context.Context) (*string, error) { return cc.CompetitorIcon(ctx, cid, types.EnLocale) }
	cf.cached = func() bool { _, ok := cc.loadIcon(cid); return ok }
	cf.clear = func() { cc.ClearCacheItem(cid) }

	tsrv, tf := newServer(iconTournamentXML)
	tc := newTournamentCache(t.Context(), newAPIClientForTest(t, tsrv), log.New(nil))
	tid := types.URN{Prefix: "od", Type: "tournament", ID: 7}
	tf.name, tf.want = "tournament", "/icons/t7.png"
	tf.icon = func(ctx context.Context) (*string, error) { return tc.TournamentIcon(ctx, tid, types.EnLocale) }
	tf.cached = func() bool { _, ok := tc.loadIcon(tid); return ok }
	tf.clear = func() { tc.ClearCacheItem(tid) }

	return []*iconFixture{cf, tf}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// Concurrent builds that need the same icon wait on one request.
func TestIconLookup_ConcurrentCallersFetchOnce(t *testing.T) {
	for _, f := range newIconFixtures(t) {
		t.Run(f.name, func(t *testing.T) {
			const callers = 8
			var registered atomic.Int32
			restore := lru.SetFlightRegisteredHook(func() { registered.Add(1) })
			defer restore()

			var wg sync.WaitGroup
			results := make([]*string, callers)
			errs := make([]error, callers)
			for i := range callers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					results[i], errs[i] = f.icon(t.Context())
				}()
			}
			// All callers are parked on the flight before the answer
			// arrives, so a caller that fetched on its own would show as
			// an extra request.
			waitUntil(t, "all callers to join the flight", func() bool { return registered.Load() == callers })
			f.release()
			wg.Wait()

			for i := range callers {
				if errs[i] != nil || results[i] == nil || *results[i] != f.want {
					t.Fatalf("caller %d: icon=%v err=%v, want %q", i, results[i], errs[i], f.want)
				}
			}
			if got := f.hits.Load(); got != 1 {
				t.Fatalf("requests = %d, want 1", got)
			}
		})
	}
}

// A caller that gives up does not cancel the fetch: the icon still arrives
// in the cache for the next caller, which then needs no request.
func TestIconLookup_FlightSurvivesTheCallersCancellation(t *testing.T) {
	for _, f := range newIconFixtures(t) {
		t.Run(f.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() {
				_, err := f.icon(ctx)
				done <- err
			}()
			<-f.entered
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want the caller's cancellation", err)
			}

			f.release()
			waitUntil(t, "the flight to fill the cache", f.cached)
			got, err := f.icon(t.Context())
			if err != nil || got == nil || *got != f.want {
				t.Fatalf("icon = %v err = %v, want %q", got, err, f.want)
			}
			if hits := f.hits.Load(); hits != 1 {
				t.Fatalf("requests = %d, want 1", hits)
			}
		})
	}
}

// A Clear that lands while the icon is being fetched still wins: the fetch
// hands the value to its callers but does not store it.
func TestIconLookup_ClearDuringTheFetchSuppressesTheIcon(t *testing.T) {
	for _, f := range newIconFixtures(t) {
		t.Run(f.name, func(t *testing.T) {
			done := make(chan error, 1)
			go func() {
				got, err := f.icon(context.Background())
				if err == nil && (got == nil || *got != f.want) {
					err = errors.New("the caller did not get the fetched icon")
				}
				done <- err
			}()
			<-f.entered
			f.clear()
			f.release()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if f.cached() {
				t.Fatal("the icon was stored although a Clear landed during the fetch")
			}
		})
	}
}

// A caller that arrives after a Clear starts its own fetch: it does not join
// the one that began before the Clear and receive the icon the Clear
// invalidated.
func TestIconLookup_ACallerAfterAClearDoesNotJoinThePreClearFetch(t *testing.T) {
	const competitorFmt = `<?xml version="1.0"?>
<competitor_profile generated_at="2026-01-01T00:00:00Z">
  <competitor id="od:competitor:3" name="Team" abbreviation="TM" icon_path="%s"/>
</competitor_profile>`
	const tournamentFmt = `<?xml version="1.0"?>
<tournament_info generated_at="2026-01-01T00:00:00Z">
  <tournament id="od:tournament:7" name="T" icon_path="%s">
    <sport id="od:sport:1" name="Football"/>
  </tournament>
</tournament_info>`
	for _, tc := range []struct {
		name, format string
		icon         func(t *testing.T, srv *httptest.Server) (get func(context.Context) (*string, error), forget func())
	}{
		{"competitor", competitorFmt, func(t *testing.T, srv *httptest.Server) (func(context.Context) (*string, error), func()) {
			cc := newCompetitorCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))
			id := types.URN{Prefix: "od", Type: "competitor", ID: 3}
			return func(ctx context.Context) (*string, error) { return cc.CompetitorIcon(ctx, id, types.EnLocale) },
				func() { cc.ClearCacheItem(id) }
		}},
		{"tournament", tournamentFmt, func(t *testing.T, srv *httptest.Server) (func(context.Context) (*string, error), func()) {
			tc := newTournamentCache(t.Context(), newAPIClientForTest(t, srv), log.New(nil))
			id := types.URN{Prefix: "od", Type: "tournament", ID: 7}
			return func(ctx context.Context) (*string, error) { return tc.TournamentIcon(ctx, id, types.EnLocale) },
				func() { tc.ClearCacheItem(id) }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			entered := make(chan struct{}, 4)
			gate := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				icon := "/icons/new.png"
				if requests.Add(1) == 1 {
					icon = "/icons/old.png"
					entered <- struct{}{}
					select {
					case <-gate:
					case <-r.Context().Done():
						return
					}
				}
				w.Header().Set("Content-Type", "application/xml")
				_, _ = fmt.Fprintf(w, tc.format, icon)
			}))
			defer srv.Close()
			defer close(gate)
			get, forget := tc.icon(t, srv)

			first := make(chan *string, 1)
			go func() {
				got, _ := get(context.Background())
				first <- got
			}()
			<-entered
			forget()

			// Bounded: joining the held fetch would wait for it.
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			got, err := get(ctx)
			if err != nil || got == nil || *got != "/icons/new.png" {
				t.Fatalf("icon after the clear = %v err = %v, want the fresh /icons/new.png", got, err)
			}
			if n := requests.Load(); n != 2 {
				t.Fatalf("requests = %d, want 2", n)
			}
		})
	}
}
