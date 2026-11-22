package factory

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/internal/apipolicy"
	"github.com/oddin-gg/gosdk/internal/cache"
	"github.com/oddin-gg/gosdk/internal/cache/lru"
	"github.com/oddin-gg/gosdk/types"
)

// Each loader of the message path names the endpoint of its own request. A
// request that gets no answer opens the breaker of exactly that endpoint (the
// client derives it from the request path), and the loader then skips the key
// while that breaker is open (the loader gates on the endpoint it asked). A
// loader that named a neighbour's endpoint, or a path that mapped to one,
// fails one of the two checks.
func TestMessagePath_EachLoaderGatesOnTheEndpointOfItsOwnRequest(t *testing.T) {
	en := []types.Locale{types.EnLocale}
	match := types.URN{Prefix: "od", Type: "match", ID: 4001}
	competitor := types.URN{Prefix: "od", Type: "competitor", ID: 4002}
	tournament := types.URN{Prefix: "od", Type: "tournament", ID: 4003}
	sport := types.URN{Prefix: "od", Type: "sport", ID: 1}

	cases := []struct {
		name string
		ep   apipolicy.Endpoint
		call func(ctx context.Context, cm *cache.Manager) error
	}{
		{"match summary", apipolicy.EndpointSummary, func(ctx context.Context, cm *cache.Manager) error {
			_, err := cm.MatchCache.Match(ctx, match, en)
			return err
		}},
		{"match status", apipolicy.EndpointSummary, func(ctx context.Context, cm *cache.Manager) error {
			_, err := cm.MatchStatusCache.MatchStatus(ctx, match)
			return err
		}},
		{"fixture", apipolicy.EndpointFixture, func(ctx context.Context, cm *cache.Manager) error {
			_, err := cm.FixtureCache.Fixture(ctx, match, en)
			return err
		}},
		{"competitor", apipolicy.EndpointCompetitor, func(ctx context.Context, cm *cache.Manager) error {
			_, err := cm.CompetitorCache.Competitor(ctx, competitor, en)
			return err
		}},
		{"competitor icon", apipolicy.EndpointCompetitor, func(ctx context.Context, cm *cache.Manager) error {
			_, err := cm.CompetitorCache.CompetitorIcon(ctx, competitor, types.EnLocale)
			return err
		}},
		{"player", apipolicy.EndpointPlayer, func(ctx context.Context, cm *cache.Manager) error {
			_, err := cm.PlayersCache.GetPlayer(ctx, cache.PlayerCacheKey{PlayerID: "od:player:4004", Locale: types.EnLocale})
			return err
		}},
		{"tournament", apipolicy.EndpointTournament, func(ctx context.Context, cm *cache.Manager) error {
			_, err := cm.TournamentCache.Tournament(ctx, tournament, en)
			return err
		}},
		{"tournament icon", apipolicy.EndpointTournament, func(ctx context.Context, cm *cache.Manager) error {
			_, err := cm.TournamentCache.TournamentIcon(ctx, tournament, types.EnLocale)
			return err
		}},
		{"market catalog", apipolicy.EndpointCatalog, func(ctx context.Context, cm *cache.Manager) error {
			_, err := cm.MarketDescriptionCache.MarketDescriptionByID(ctx, 7, types.None[string](), en)
			return err
		}},
		{"market variant", apipolicy.EndpointVariant, func(ctx context.Context, cm *cache.Manager) error {
			_, err := cm.MarketDescriptionCache.MarketDescriptionByID(ctx, 3, types.Some("od:dynamic_outcomes:99"), en)
			return err
		}},
		{"sports", apipolicy.EndpointStatic, func(ctx context.Context, cm *cache.Manager) error {
			_, err := cm.SportDataCache.Sports(ctx, en)
			return err
		}},
		{"sport tournaments", apipolicy.EndpointStatic, func(ctx context.Context, cm *cache.Manager) error {
			_, err := cm.SportDataCache.SportTournaments(ctx, sport, types.EnLocale)
			return err
		}},
		{"match status descriptions", apipolicy.EndpointStatic, func(ctx context.Context, cm *cache.Manager) error {
			_, err := cm.LocalizedStaticMatchStatus.LocalizedItem(ctx, 1, en)
			return err
		}},
		{"void reasons", apipolicy.EndpointStatic, func(ctx context.Context, cm *cache.Manager) error {
			_, err := cm.MarketVoidReasonsCache.MarketVoidReasons(ctx)
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A request that gets no answer opens its endpoint's breaker and
			// no other.
			failing := newMessagePathRig(t)
			failing.ctl.failing.Store(true) // every request but the producers' gets no answer
			ctx := apipolicy.WithMessage(t.Context(), failing.api.Breaker(), apipolicy.MessageTimeout)
			if err := tc.call(ctx, failing.cm); err == nil {
				t.Fatal("the load of a failing API succeeded")
			}
			for ep := apipolicy.Endpoint(0); ep < apipolicy.EndpointCount; ep++ {
				if got, want := failing.api.Breaker().Open(ep), ep == tc.ep; got != want {
					t.Fatalf("breaker of %v: open = %v, want %v (only %v may open)", ep, got, want, tc.ep)
				}
			}

			// While its endpoint's breaker is open the loader neither asks nor
			// registers on a flight.
			healthy := newMessagePathRig(t)
			healthy.api.Breaker().Trip(tc.ep)
			before := healthy.ctl.all.Load()
			var registered atomic.Int32
			restore := lru.SetFlightRegisteredHook(func() { registered.Add(1) })
			ctx = apipolicy.WithMessage(t.Context(), healthy.api.Breaker(), apipolicy.MessageTimeout)
			err := tc.call(ctx, healthy.cm)
			restore()
			if !errors.Is(err, apipolicy.ErrSkipped) {
				t.Fatalf("err = %v, want ErrSkipped: the loader must gate on %v", err, tc.ep)
			}
			if got := registered.Load(); got != 0 {
				t.Fatalf("the loader registered on %d flight(s) while %v was open: it must be skipped before", got, tc.ep)
			}
			if got := healthy.ctl.all.Load(); got != before {
				t.Fatalf("the loader made %d request(s) while %v was open", got-before, tc.ep)
			}

			// A message that waits its whole time on the loader's request
			// records that endpoint as the one it waited on.
			hung := newMessagePathRig(t)
			hung.ctl.hanging.Store(true)
			waitCtx, cancel := context.WithTimeout(t.Context(), 60*time.Millisecond)
			defer cancel()
			ctx = apipolicy.WithMessage(waitCtx, hung.api.Breaker(), 60*time.Millisecond)
			if err := tc.call(ctx, hung.cm); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("err = %v, want a deadline: the request hangs", err)
			}
			if got := apipolicy.TimedOut(ctx); len(got) != 1 || got[0] != tc.ep {
				t.Fatalf("the message waited on %v, want only %v", got, tc.ep)
			}
		})
	}
}
