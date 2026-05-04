// basic exercises the smallest working setup: configure, subscribe, and
// log every parsed message until SIGINT.
//
// Env: TOKEN, ENV (integration|test|production), REGION (eu|ap), NODE.
package main

import (
	"context"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/oddin-gg/gosdk"
	"github.com/oddin-gg/gosdk/protocols"
)

func main() {
	cfg := gosdk.NewConfig(envOrDie("TOKEN"), parseEnvironment(),
		gosdk.WithLogger(slog.Default()),
	)

	bootCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, err := gosdk.New(bootCtx, cfg)
	if err != nil {
		log.Fatalf("gosdk.New: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.Close(ctx)
	}()

	sub, err := c.Subscribe(context.Background(),
		gosdk.WithMessageInterest(protocols.AllMessageInterest),
	)
	if err != nil {
		log.Fatalf("subscribe: %v", err)
	}

	go func() {
		for msg := range sub.Messages() {
			switch m := msg.Message.(type) {
			case protocols.OddsChange:
				log.Printf("odds change: event=%v markets=%d", m.Event(), len(m.Markets()))
			case protocols.BetSettlement:
				log.Printf("settlement: event=%v", m.Event())
			case protocols.BetCancel:
				log.Printf("cancel: event=%v", m.Event())
			default:
				if msg.UnparsableMessage != nil {
					log.Println("unparsable message")
				}
			}
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh
}

func envOrDie(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("%s not set", key)
	}
	return v
}

func parseEnvironment() protocols.Environment {
	switch os.Getenv("ENV") {
	case "production":
		return protocols.ProductionEnvironment
	case "test":
		return protocols.TestEnvironment
	default:
		return protocols.IntegrationEnvironment
	}
}
