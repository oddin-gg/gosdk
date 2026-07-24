// api_only demonstrates using gosdk for catalog / entity reads
// without ever opening the AMQP feed.
//
// gosdk.New() makes one HTTP whoami probe and allocates the
// in-memory managers; no AMQP connection is opened. As long as
// you don't call Connect() or Subscribe(), every method below is
// HTTP-only.
//
// Use this mode when:
//   - you only need the catalog (sports, tournaments, matches,
//     fixtures, market descriptions);
//   - you're managing the replay queue (Replay().AddEvent / List
//     / Start / Stop);
//   - you're checking bookmaker / producer info without consuming
//     a live feed.
//
// Env: TOKEN, ENV (integration|test|production), MATCH (optional
// match URN to fetch, e.g. "od:match:42").
package main

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/oddin-gg/gosdk"
	"github.com/oddin-gg/gosdk/types"
)

func main() {
	cfg := gosdk.NewConfig(envOrDie("TOKEN"), parseEnvironment(),
		// Preload locales so every Sport / Match / etc. that gets
		// cached carries names in BOTH locales from the first read.
		gosdk.WithDefaultLocale(types.EnLocale),
		gosdk.WithPreloadLocales(types.EnLocale, types.RuLocale),
	)

	bootCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	c, err := gosdk.New(bootCtx, cfg)
	cancel()
	if err != nil {
		log.Fatalf("gosdk.New: %v", err)
	}
	// Close is correct in API-only mode — it nil-checks the
	// connection-layer components that were never opened.
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.Close(ctx)
	}()

	// A fresh per-call context bounds each independent HTTP round-trip —
	// don't share one deadline across sequential calls (a slow early call
	// would eat the budget of the later ones).
	callCtx := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), 10*time.Second)
	}

	// 1. Bookmaker details (cached after first call).
	ctx, cancel := callCtx()
	bd, err := c.BookmakerDetails(ctx)
	cancel()
	if err != nil {
		log.Fatalf("bookmaker details: %v", err)
	}
	log.Printf("bookmaker id=%d vhost=%s expires=%s",
		bd.BookmakerID(), bd.VirtualHost(), bd.ExpireAt().Format(time.RFC3339))

	// 2. Producers catalog.
	ctx, cancel = callCtx()
	prods, err := c.ActiveProducers(ctx)
	cancel()
	if err != nil {
		log.Fatalf("active producers: %v", err)
	}
	log.Printf("active producers (%d):", len(prods))
	for _, p := range prods {
		log.Printf("  id=%d name=%-8s scopes=%v", p.ID(), p.Name(), p.ProducerScopes())
	}

	// 3. Sports catalog. Multi-locale variadic populates Names
	// for every supplied locale on every entry.
	ctx, cancel = callCtx()
	sports, err := c.Sports(ctx, types.EnLocale, types.RuLocale)
	cancel()
	if err != nil {
		log.Fatalf("sports: %v", err)
	}
	log.Printf("sports (%d):", len(sports))
	for _, s := range sports {
		log.Printf("  %s | en=%s ru=%s",
			s.ID.ToString(), s.Name(types.EnLocale).ValueOr(""), s.Name(types.RuLocale).ValueOr(""))
	}

	// 4. Market descriptions catalog.
	ctx, cancel = callCtx()
	mds, err := c.MarketDescriptions(ctx, types.EnLocale)
	cancel()
	if err != nil {
		log.Fatalf("market descriptions: %v", err)
	}
	log.Printf("market descriptions (%d) — first 5:", len(mds))
	for i, md := range mds {
		if i >= 5 {
			break
		}
		name := md.LocalizedName(types.EnLocale).ValueOr("")
		log.Printf("  id=%d name=%q outcomes=%d", md.ID, name, len(md.Outcomes))
	}

	// 5. Optional: fetch a specific match if MATCH env var is set.
	if matchID := os.Getenv("MATCH"); matchID != "" {
		urn, err := types.ParseURN(matchID)
		if err != nil || urn == nil {
			log.Fatalf("parse MATCH=%q: %v", matchID, err)
		}
		ctx, cancel = callCtx()
		match, err := c.Match(ctx, *urn, types.EnLocale, types.RuLocale)
		cancel()
		if err != nil {
			log.Fatalf("match %s: %v", matchID, err)
		}
		log.Printf("match %s | en=%s ru=%s scheduled=%v",
			match.ID.ToString(),
			match.Name(types.EnLocale).ValueOr(""),
			match.Name(types.RuLocale).ValueOr(""),
			match.ScheduledTime)
		log.Printf("  tournament=%s sport=%s competitors=%d",
			match.Tournament.ID.ToString(),
			match.Tournament.Sport.ID.ToString(),
			len(match.Competitors))
	}

	// No Connect, no Subscribe, no AMQP — the program exits and
	// Close tears down only the HTTP-side managers.
	log.Println("done")
}

func envOrDie(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("%s not set", key)
	}
	return v
}

func parseEnvironment() types.Environment {
	switch v := os.Getenv("ENV"); v {
	case "production":
		return types.ProductionEnvironment
	case "test":
		return types.TestEnvironment
	case "integration", "":
		return types.IntegrationEnvironment
	default:
		log.Fatalf("ENV=%q not recognized (want production / test / integration)", v)
		return types.UnknownEnvironment // unreachable
	}
}
