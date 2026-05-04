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
		log.Fatalf("gosdk.New: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.Close(ctx)
	}()

	r := c.Replay()
	if err := r.Clear(bootCtx); err != nil {
		log.Printf("clear: %v (continuing)", err)
	}
	if err := r.AddEvent(bootCtx, *eventURN); err != nil {
		log.Fatalf("add event: %v", err)
	}
	if err := r.Start(bootCtx,
		gosdk.WithReplaySpeed(10),
		gosdk.WithReplayMaxDelayMs(50),
	); err != nil {
		log.Fatalf("start: %v", err)
	}
	log.Println("replay started — consuming feed")

	sub, err := c.Subscribe(context.Background(), gosdk.WithReplay())
	if err != nil {
		log.Fatalf("subscribe: %v", err)
	}

	go func() {
		for msg := range sub.Messages() {
			switch m := msg.Message.(type) {
			case types.OddsChange:
				log.Printf("replay odds change: event=%v markets=%d", m.Event(), len(m.Markets()))
			case types.BetSettlement:
				log.Printf("replay settlement: event=%v", m.Event())
			}
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh

	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.StopAndClear(stopCtx); err != nil {
		log.Printf("stop+clear: %v", err)
	}
}

func parseEnv() types.Environment {
	switch os.Getenv("ENV") {
	case "production":
		return types.ProductionEnvironment
	case "test":
		return types.TestEnvironment
	default:
		return types.IntegrationEnvironment
	}
}
