package factory

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/cache"
	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

// These tests drive marketDataImpl through a REAL description cache fed
// by an httptest catalog — the path an odds_change takes — and pin the
// CORE-4213 rewrite: names are read straight off the live cache entry
// (no Snapshot() copy per outcome per locale) and the by-id lookup runs
// once per market per locale shape, not once per outcome.

// catalogSrv serves a per-locale bulk market catalog, a player profile,
// and counts per-variant fetches (which it always fails — the dynamic
// variant here is deliberately unresolvable).
type catalogSrv struct {
	*httptest.Server
	bulk        map[types.Locale]string
	variantHits atomic.Int64
	bulkHits    atomic.Int64
}

func newCatalogSrv(t *testing.T, bulk map[types.Locale]string) *catalogSrv {
	t.Helper()
	s := &catalogSrv{bulk: bulk}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		path := r.URL.Path
		switch {
		case strings.Contains(path, "/variants/"):
			s.variantHits.Add(1)
			http.Error(w, "no such variant", http.StatusNotFound)
		case strings.HasSuffix(path, "/markets"):
			s.bulkHits.Add(1)
			// /v1/descriptions/{locale}/markets
			parts := strings.Split(path, "/")
			locale := types.Locale(parts[len(parts)-2])
			body, ok := bulk[locale]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = io.WriteString(w, body)
		case strings.HasSuffix(path, "/profile") && strings.Contains(path, "/players/"):
			parts := strings.Split(path, "/")
			id := parts[len(parts)-2]
			locale := parts[len(parts)-4]
			_, _ = fmt.Fprintf(w, `<?xml version="1.0"?>
<player_profile><player id="%s" name="Striker %s" full_name="Striker %s" sport="od:sport:1"/></player_profile>`, id, locale, locale)
		default:
			t.Logf("catalogSrv: unhandled %s %s", r.Method, path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

type rewriteTransport struct {
	target string
	base   http.RoundTripper
}

func (rt *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u, err := url.Parse(rt.target)
	if err != nil {
		return nil, err
	}
	req.URL.Scheme = u.Scheme
	req.URL.Host = u.Host
	return rt.base.RoundTrip(req)
}

// newMarketFactoryForTest wires the real cache manager + description /
// market-data factories to srv, the way client.New does.
func newMarketFactoryForTest(t *testing.T, srv *catalogSrv, locales []types.Locale) (*MarketFactory, context.Context) {
	t.Helper()
	apiClient := api.New(minimalCfg{})
	apiClient.SetHTTPClient(&http.Client{
		Timeout: 2 * time.Second,
		Transport: &rewriteTransport{
			target: srv.URL,
			base:   &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		},
	})
	mgr := cache.NewManager(t.Context(), apiClient, minimalCfg{}, log.New(nil), locales)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		mgr.CloseCtx(ctx)
	})
	mdf := NewMarketDescriptionFactory(mgr.MarketDescriptionCache, mgr.MarketVoidReasonsCache, mgr.PlayersCache, mgr.CompetitorCache)
	mf := NewMarketFactory(NewMarketDataFactory(minimalCfg{}, mdf), locales, true, log.New(nil))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	return mf, ctx
}

const catalogEn = `<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="1" name="1x2" groups="all">
    <outcomes><outcome id="1" name="home"/><outcome id="2" name="draw"/><outcome id="3" name="away"/></outcomes>
  </market>
  <market id="5" name="Total {total} goals" groups="all">
    <specifiers><specifier name="total" type="decimal"/></specifiers>
    <outcomes><outcome id="12" name="over {total}"/><outcome id="13" name="under {total}"/></outcomes>
  </market>
  <market id="7" name="{player} kills" groups="all|player_props">
    <specifiers><specifier name="player" type="string"/></specifiers>
    <outcomes><outcome id="1" name="yes"/></outcomes>
  </market>
  <market id="8" name="Top scorer" outcome_type="player" groups="all">
    <outcomes><outcome id="1" name="nobody"/></outcomes>
  </market>
  <market id="9" name="{player} assists" groups="all">
    <specifiers><specifier name="player" type="string"/></specifiers>
    <outcomes><outcome id="1" name="yes"/></outcomes>
  </market>
</market_descriptions>`

const catalogRu = `<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="1" name="1х2" groups="all">
    <outcomes><outcome id="1" name="Хозяева"/><outcome id="2" name="Ничья"/><outcome id="3" name="Гости"/></outcomes>
  </market>
  <market id="5" name="Тотал {total}" groups="all">
    <specifiers><specifier name="total" type="decimal"/></specifiers>
    <outcomes><outcome id="12" name="больше {total}"/><outcome id="13" name="меньше {total}"/></outcomes>
  </market>
  <market id="7" name="{player} убийств" groups="all|player_props">
    <specifiers><specifier name="player" type="string"/></specifiers>
    <outcomes><outcome id="1" name="да"/></outcomes>
  </market>
  <market id="8" name="Лучший бомбардир" outcome_type="player" groups="all">
    <outcomes><outcome id="1" name="никто"/></outcomes>
  </market>
  <market id="9" name="{player} передач" groups="all">
    <specifiers><specifier name="player" type="string"/></specifiers>
    <outcomes><outcome id="1" name="да"/></outcomes>
  </market>
</market_descriptions>`

var twoLocales = []types.Locale{types.EnLocale, types.RuLocale}

func testMatch() types.Match {
	return types.Match{
		HomeCompetitor: &types.TeamCompetitor{Competitor: types.Competitor{
			Names: map[types.Locale]string{types.EnLocale: "Team A", types.RuLocale: "Команда А"},
		}},
		AwayCompetitor: &types.TeamCompetitor{Competitor: types.Competitor{
			Names: map[types.Locale]string{types.EnLocale: "Team B", types.RuLocale: "Команда Б"},
		}},
	}
}

func feedMarket(id int, specifiers string, outcomeIDs ...string) *feedXML.MarketWithOutcome {
	m := &feedXML.MarketWithOutcome{}
	m.ID = id
	if specifiers != "" {
		m.Specifiers = &specifiers
	}
	for _, o := range outcomeIDs {
		m.Outcomes = append(m.Outcomes, feedXML.Outcome{ID: o})
	}
	return m
}

func wantNames(t *testing.T, what string, got map[types.Locale]string, want map[types.Locale]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: Names = %v, want %v", what, got, want)
	}
	for l, w := range want {
		if got[l] != w {
			t.Fatalf("%s: Names[%s] = %q, want %q (all: %v)", what, l, got[l], w, got)
		}
	}
}

// TestMarketData_StaticOutcomes_ResolveAcrossLocales: catalog names for
// every configured locale, with the home/away placeholders replaced by
// the event's competitor names in each locale (keyed on the canonical
// English label, so the ru row's translated "Хозяева" substitutes too).
func TestMarketData_StaticOutcomes_ResolveAcrossLocales(t *testing.T) {
	srv := newCatalogSrv(t, map[types.Locale]string{types.EnLocale: catalogEn, types.RuLocale: catalogRu})
	mf, ctx := newMarketFactoryForTest(t, srv, twoLocales)

	m := mf.BuildMarketWithOdds(ctx, testMatch(), feedMarket(1, "", "1", "2", "3"))

	wantNames(t, "market", m.Names, map[types.Locale]string{types.EnLocale: "1x2", types.RuLocale: "1х2"})
	if len(m.OutcomeOdds) != 3 {
		t.Fatalf("outcomes = %d", len(m.OutcomeOdds))
	}
	wantNames(t, "home", m.OutcomeOdds[0].Names, map[types.Locale]string{types.EnLocale: "Team A", types.RuLocale: "Команда А"})
	wantNames(t, "draw", m.OutcomeOdds[1].Names, map[types.Locale]string{types.EnLocale: "draw", types.RuLocale: "Ничья"})
	wantNames(t, "away", m.OutcomeOdds[2].Names, map[types.Locale]string{types.EnLocale: "Team B", types.RuLocale: "Команда Б"})

	// Settlement builds share the resolver.
	s := mf.BuildMarketWithSettlement(ctx, testMatch(), feedMarket(1, "", "3"))
	wantNames(t, "settled away", s.OutcomeSettlements[0].Names, map[types.Locale]string{types.EnLocale: "Team B", types.RuLocale: "Команда Б"})
}

// TestMarketData_SpecifierTemplates: "{specifier}" placeholders in the
// market AND outcome templates are filled from the message's
// specifiers; a player_props market resolves a player URN specifier to
// the player's localized name via the player cache.
func TestMarketData_SpecifierTemplates(t *testing.T) {
	srv := newCatalogSrv(t, map[types.Locale]string{types.EnLocale: catalogEn, types.RuLocale: catalogRu})
	mf, ctx := newMarketFactoryForTest(t, srv, twoLocales)

	total := mf.BuildMarketWithOdds(ctx, testMatch(), feedMarket(5, "total=2.5", "12", "13"))
	wantNames(t, "total market", total.Names, map[types.Locale]string{types.EnLocale: "Total 2.5 goals", types.RuLocale: "Тотал 2.5"})
	// Outcome templates are NOT filled by the factory (unchanged
	// behaviour — the placeholder is the catalog's, consumers format).
	wantNames(t, "over", total.OutcomeOdds[0].Names, map[types.Locale]string{types.EnLocale: "over {total}", types.RuLocale: "больше {total}"})

	props := mf.BuildMarketWithOdds(ctx, testMatch(), feedMarket(7, "player=od:player:100", "1"))
	wantNames(t, "props market", props.Names, map[types.Locale]string{types.EnLocale: "Striker en kills", types.RuLocale: "Striker ru убийств"})

	// The player_props group is what enables the URN → player-name
	// substitution. Market 9 carries the same "{player}" template and
	// the same URN specifier WITHOUT the group, so the raw URN must
	// survive into the name: the gate is at the makeMarketName call
	// site, and this is the only test that pins its negative side.
	notProps := mf.BuildMarketWithOdds(ctx, testMatch(), feedMarket(9, "player=od:player:100", "1"))
	wantNames(t, "non-props market with a player URN specifier", notProps.Names, map[types.Locale]string{
		types.EnLocale: "od:player:100 assists",
		types.RuLocale: "od:player:100 передач",
	})
}

// TestMarketData_DynamicOutcomes: on an outcome_type="player" market an
// outcome id the catalog does not list is a player URN and resolves via
// the player cache, while a listed outcome keeps its catalog name. This
// is the exists-vs-locale-hit distinction OutcomeName carries.
func TestMarketData_DynamicOutcomes(t *testing.T) {
	srv := newCatalogSrv(t, map[types.Locale]string{types.EnLocale: catalogEn, types.RuLocale: catalogRu})
	mf, ctx := newMarketFactoryForTest(t, srv, twoLocales)

	m := mf.BuildMarketWithOdds(ctx, testMatch(), feedMarket(8, "", "1", "od:player:100"))
	wantNames(t, "static outcome on dynamic market", m.OutcomeOdds[0].Names, map[types.Locale]string{types.EnLocale: "nobody", types.RuLocale: "никто"})
	wantNames(t, "player outcome", m.OutcomeOdds[1].Names, map[types.Locale]string{types.EnLocale: "Striker en", types.RuLocale: "Striker ru"})

	// A market WITHOUT an outcome type: an unknown outcome is simply
	// nameless (None in every locale), never an error, never a fetch.
	plain := mf.BuildMarketWithOdds(ctx, testMatch(), feedMarket(1, "", "99"))
	if len(plain.OutcomeOdds[0].Names) != 0 {
		t.Fatalf("unknown outcome on plain market: Names = %v, want none", plain.OutcomeOdds[0].Names)
	}
}

// TestMarketData_UnknownMarket_NamelessNotFailing: a market the catalog
// lacks yields nameless markets and outcomes (ids, specifiers intact);
// the description lookup runs once per locale shape for the whole
// market, not once per outcome — the memo also caps the failure cost.
func TestMarketData_UnknownMarket_NamelessNotFailing(t *testing.T) {
	srv := newCatalogSrv(t, map[types.Locale]string{types.EnLocale: catalogEn, types.RuLocale: catalogRu})
	mf, ctx := newMarketFactoryForTest(t, srv, twoLocales)

	m := mf.BuildMarketWithOdds(ctx, testMatch(), feedMarket(404, "total=1", "1", "2", "3", "4"))
	if m.ID != 404 || m.Specifiers["total"] != "1" || len(m.OutcomeOdds) != 4 {
		t.Fatalf("market shape lost: %+v", m)
	}
	if len(m.Names) != 0 {
		t.Fatalf("unknown market Names = %v, want none", m.Names)
	}
	for _, o := range m.OutcomeOdds {
		if len(o.Names) != 0 {
			t.Fatalf("unknown market outcome %s Names = %v, want none", o.ID, o.Names)
		}
	}
}

// TestMarketData_DescriptionLookupOncePerMarket pins the memo. A
// DYNAMIC variant (od:dynamic_outcomes:*) is fetched per key on every
// miss, and the server here refuses it, so every lookup that reaches
// the cache is one counted HTTP request: the request count IS the
// lookup count. Two locales, one market name and twenty outcomes used
// to be 2 + 2×20 = 42 lookups; the memo makes it one per locale shape.
func TestMarketData_DescriptionLookupOncePerMarket(t *testing.T) {
	srv := newCatalogSrv(t, map[types.Locale]string{types.EnLocale: catalogEn, types.RuLocale: catalogRu})
	mf, ctx := newMarketFactoryForTest(t, srv, twoLocales)

	ids := make([]string, 20)
	for i := range ids {
		ids[i] = fmt.Sprint(i)
	}
	m := mf.BuildMarketWithOdds(ctx, testMatch(), feedMarket(3, "variant=od:dynamic_outcomes:99", ids...))
	if len(m.Names) != 0 || len(m.OutcomeOdds) != 20 {
		t.Fatalf("unresolvable variant: Names=%v outcomes=%d", m.Names, len(m.OutcomeOdds))
	}
	// Shapes: (en), (ru), (ru + en canonical). Each is ONE fetch —
	// loadOne stops at the first failing locale.
	if hits := srv.variantHits.Load(); hits > 3 {
		t.Fatalf("variant endpoint hit %d times for one market, want ≤ 3 (one per locale shape)", hits)
	} else if hits == 0 {
		t.Fatal("variant endpoint never hit — test is not exercising the lookup")
	}
}

// BenchmarkMarketFactory_BuildMarketWithOdds is the message-path cost of
// one market carrying every outcome of a 350-outcome description, two
// locales, through the real cache — the shape Corwyn measured at ~16 ms
// / 51 MB per market before CORE-4213. Run with -benchmem.
func BenchmarkMarketFactory_BuildMarketWithOdds(b *testing.B) {
	for _, n := range []int{2, 121, 348} {
		var sb strings.Builder
		fmt.Fprintf(&sb, `<?xml version="1.0"?><market_descriptions response_code="OK"><market id="%d" name="Exact {total}" groups="all"><specifiers><specifier name="total" type="decimal"/></specifiers><outcomes>`, n)
		ids := make([]string, n)
		for i := range ids {
			ids[i] = fmt.Sprint(i)
			fmt.Fprintf(&sb, `<outcome id="%d" name="o%d"/>`, i, i)
		}
		sb.WriteString(`</outcomes></market></market_descriptions>`)
		body := sb.String()

		srv := &catalogSrv{}
		srv.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(w, body)
		}))
		apiClient := api.New(minimalCfg{})
		apiClient.SetHTTPClient(&http.Client{Timeout: 2 * time.Second, Transport: &rewriteTransport{target: srv.URL, base: http.DefaultTransport}})
		mgr := cache.NewManager(b.Context(), apiClient, minimalCfg{}, log.New(nil), twoLocales)
		mdf := NewMarketDescriptionFactory(mgr.MarketDescriptionCache, mgr.MarketVoidReasonsCache, mgr.PlayersCache, mgr.CompetitorCache)
		mf := NewMarketFactory(NewMarketDataFactory(minimalCfg{}, mdf), twoLocales, true, log.New(nil))
		market := feedMarket(n, "total=1", ids...)
		match := testMatch()

		// Warm the cache so the loop measures the hot path only.
		if warm := mf.BuildMarketWithOdds(b.Context(), match, market); len(warm.Names) != 2 || len(warm.OutcomeOdds[n-1].Names) != 2 {
			b.Fatalf("warm-up did not resolve: %v / %v", warm.Names, warm.OutcomeOdds[n-1].Names)
		}
		b.Run(fmt.Sprintf("outcomes=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = mf.BuildMarketWithOdds(b.Context(), match, market)
			}
		})
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		mgr.CloseCtx(ctx)
		cancel()
		srv.Close()
	}
}
