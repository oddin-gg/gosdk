// replay drives the Oddin replay API: queue an event, start playback,
// consume the resulting feed, then stop+clear on shutdown.
//
// Env: TOKEN, ENV, EVENT_URN.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/oddin-gg/gosdk"
	"github.com/oddin-gg/gosdk/types"
)

func main() {
	token := os.Getenv("TOKEN")
	rawURN := os.Getenv("EVENT_URN")
	if token == "" || rawURN == "" {
		log.Fatal("TOKEN and EVENT_URN required")
	}
	eventURN, err := types.ParseURN(rawURN)
	if err != nil {
		log.Fatalf("parse URN %q: %v", rawURN, err)
	}

	cfg := gosdk.NewConfig(token, parseEnv())

	bootCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := gosdk.New(bootCtx, cfg)
	if err != nil {
		cancel()
		log.Fatalf("gosdk.New: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.Close(ctx)
	}()

	// Fresh bounded ctx per independent operation (see doc.go): one
	// shared deadline across steps makes the later ones fail for the
	// earlier ones' latency.
	callCtx := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), 10*time.Second)
	}

	r := c.Replay()
	clearCtx, cancelClear := callCtx()
	defer cancelClear()
	if err := r.Clear(clearCtx); err != nil {
		log.Printf("clear: %v (continuing)", err)
	}
	addCtx, cancelAdd := callCtx()
	defer cancelAdd()
	if err := r.AddEvent(addCtx, *eventURN); err != nil {
		log.Fatalf("add event: %v", err)
	}

	// Subscribe and start consuming BEFORE starting playback. Replay
	// begins emitting the moment r.Start returns; if the subscription
	// weren't already draining, the earliest messages could be produced
	// before any consumer is attached and be lost.
	// Subscribe's ctx bounds SETUP only (broker open, queue topology).
	subCtx, cancelSub := callCtx()
	sub, err := c.Subscribe(subCtx, gosdk.WithReplay())
	cancelSub()
	if err != nil {
		log.Fatalf("subscribe: %v", err)
	}

	go func() {
		// SessionMessage is a tagged union (v2.25): branch on the
		// embedded EventMessage variants directly.
		for msg := range sub.Messages() {
			switch {
			case msg.OddsChange != nil:
				log.Printf("replay odds change: event=%v markets=%d", msg.OddsChange.Event(), len(msg.OddsChange.Markets()))
			case msg.BetSettlement != nil:
				log.Printf("replay settlement: event=%v", msg.BetSettlement.Event())
			}
		}
	}()

	startCtx, cancelStart := callCtx()
	defer cancelStart()
	if err := r.Start(startCtx,
		gosdk.WithReplaySpeed(10),
		gosdk.WithReplayMaxDelayMs(50),
	); err != nil {
		log.Fatalf("start: %v", err)
	}
	log.Println("replay started — consuming feed")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh

	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.StopAndClear(stopCtx); err != nil {
		log.Printf("stop+clear: %v", err)
	}

	// Drain the subscription BEFORE the deferred client.Close (which is
	// abrupt for live subscriptions): sub.Close waits until the consumer
	// goroutine has read every admitted replay message, then Messages()
	// closes and that goroutine ends.
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 10*time.Second)
	_ = sub.Close(drainCtx)
	cancelDrain()
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
