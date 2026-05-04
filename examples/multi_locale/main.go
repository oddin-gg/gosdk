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
	"github.com/oddin-gg/gosdk/protocols"
)

func main() {
	token := os.Getenv("TOKEN")
	if token == "" {
		log.Fatal("TOKEN not set")
	}

	cfg := gosdk.NewConfig(token, parseEnv(),
		gosdk.WithDefaultLocale(protocols.EnLocale),
		gosdk.WithPreloadLocales(protocols.EnLocale, protocols.RuLocale, protocols.DeLocale),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, err := gosdk.New(ctx, cfg)
	if err != nil {
		log.Fatalf("gosdk.New: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.Close(ctx)
	}()

	// Asking for sports in three locales fills the cache for all three.
	// The variadic method does the multi-locale fan-out for us.
	sports, err := c.Sports(ctx, protocols.EnLocale, protocols.RuLocale, protocols.DeLocale)
	if err != nil {
		log.Fatalf("sports: %v", err)
	}

	for _, s := range sports {
		// Each cached entry now holds en, ru, de simultaneously.
		// Per-locale lookups don't refetch.
		log.Printf("%s | en=%s ru=%s de=%s",
			s.ID.ToString(),
			s.Name(protocols.EnLocale),
			s.Name(protocols.RuLocale),
			s.Name(protocols.DeLocale))
	}
}

func parseEnv() protocols.Environment {
	switch os.Getenv("ENV") {
	case "production":
		return protocols.ProductionEnvironment
	case "test":
		return protocols.TestEnvironment
	default:
		return protocols.IntegrationEnvironment
	}
}

