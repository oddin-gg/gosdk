// basic exercises the smallest working setup: configure, subscribe, and
// log every parsed message until SIGINT.
//
// Env: TOKEN (required), ENV (integration|test|production; default
// integration).
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
	"github.com/oddin-gg/gosdk/types"
)

func main() {
	cfg := gosdk.NewConfig(envOrDie("TOKEN"), parseEnvironment(),
		gosdk.WithLogger(slog.Default()),
	)

	bootCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	c, err := gosdk.New(bootCtx, cfg)
	cancel()
	if err != nil {
		log.Fatalf("gosdk.New: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.Close(ctx)
	}()

	// Subscribe's ctx bounds SETUP only (lazy-connect dial, queue
	// topology); it does not govern the subscription's lifetime.
	subCtx, cancelSub := context.WithTimeout(context.Background(), 30*time.Second)
	sub, err := c.Subscribe(subCtx,
		gosdk.WithMessageInterest(types.AllMessageInterest),
	)
	cancelSub()
	if err != nil {
		log.Fatalf("subscribe: %v", err)
	}

	go func() {
		// SessionMessage is a tagged union (v2.25): exactly one of
		// the embedded EventMessage variants OR UnparsableMessage is
		// non-nil per envelope. Branch with simple nil checks.
		for msg := range sub.Messages() {
			switch {
			case msg.OddsChange != nil:
				log.Printf("odds change: event=%v markets=%d", msg.OddsChange.Event(), len(msg.OddsChange.Markets()))
			case msg.BetSettlement != nil:
				log.Printf("settlement: event=%v", msg.BetSettlement.Event())
			case msg.BetCancel != nil:
				log.Printf("cancel: event=%v", msg.BetCancel.Event())
			case msg.UnparsableMessage != nil:
				log.Println("unparsable message")
			}
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh

	// Graceful drain BEFORE the deferred client.Close (which is abrupt
	// for live subscriptions): sub.Close waits until the consumer
	// goroutine above has read every admitted message, then its
	// Messages() channel closes, ending that goroutine. See
	// examples/graceful for the fully-instrumented version.
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 30*time.Second)
	_ = sub.Close(drainCtx)
	cancelDrain()
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
