// multi_locale demonstrates the multi-locale fill-in pattern: preload
// several locales at config time, then call entity methods with a
// specific locale to read the per-locale field.
//
// Env: TOKEN, ENV.
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
	token := os.Getenv("TOKEN")
	if token == "" {
		log.Fatal("TOKEN not set")
	}

	cfg := gosdk.NewConfig(token, parseEnv(),
		gosdk.WithDefaultLocale(types.EnLocale),
		gosdk.WithPreloadLocales(types.EnLocale, types.RuLocale, types.DeLocale),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, err := gosdk.New(ctx, cfg)
	if err != nil {
		cancel()
		log.Fatalf("gosdk.New: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.Close(ctx)
	}()

	// Fresh bounded ctx for this call (see doc.go) — never reuse the
	// construction deadline for later independent operations.
	sportsCtx, cancelSports := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelSports()

	// Asking for sports in three locales fills the cache for all three.
	// The variadic method does the multi-locale fan-out for us.
	sports, err := c.Sports(sportsCtx, types.EnLocale, types.RuLocale, types.DeLocale)
	if err != nil {
		log.Fatalf("sports: %v", err)
	}

	for _, s := range sports {
		// Each cached entry now holds en, ru, de simultaneously.
		// Per-locale lookups don't refetch.
		log.Printf("%s | en=%s ru=%s de=%s",
			s.ID.ToString(),
			s.Name(types.EnLocale),
			s.Name(types.RuLocale),
			s.Name(types.DeLocale))
	}
}

func parseEnv() types.Environment {
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
