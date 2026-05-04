// recovery initiates an event-odds recovery for a specific match URN
// and prints recovery events as they flow.
//
// Env: TOKEN, ENV, EVENT_URN (e.g. "od:match:32109").
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
	token := os.Getenv("TOKEN")
	rawURN := os.Getenv("EVENT_URN")
	if token == "" || rawURN == "" {
		log.Fatal("TOKEN and EVENT_URN required")
	}
	eventURN, err := protocols.ParseURN(rawURN)
	if err != nil {
		log.Fatalf("parse URN %q: %v", rawURN, err)
	}

	cfg := gosdk.NewConfig(token, env(),
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

	// Connect explicitly so the producer catalog is populated before we
	// pick a producer to issue the recovery against.
	connectCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.Connect(connectCtx); err != nil {
		log.Fatalf("connect: %v", err)
	}

	prods, err := c.ProducersInScope(bootCtx, protocols.LiveProducerScope)
	if err != nil {
		log.Fatalf("producers: %v", err)
	}
	if len(prods) == 0 {
		log.Fatal("no live-scope producers available")
	}
	live := prods[0]
	log.Printf("requesting odds recovery on producer %d (%s) for %s", live.ID(), live.Name(), eventURN.ToString())

	reqID, err := c.RecoverEventOdds(bootCtx, live.ID(), *eventURN)
	if err != nil {
		log.Fatalf("recover: %v", err)
	}
	log.Printf("recovery request id: %d", reqID)

	go func() {
		for ev := range c.RecoveryEvents() {
			switch {
			case ev.ProducerStatus != nil:
				log.Printf("producer status: producer=%d down=%v reason=%v",
					ev.ProducerStatus.Producer().ID(),
					ev.ProducerStatus.IsDown(),
					ev.ProducerStatus.ProducerStatusReason())
			case ev.EventRecovery != nil:
				log.Printf("event recovery complete: event=%s requestID=%d",
					ev.EventRecovery.EventID().ToString(), ev.EventRecovery.RequestID())
			}
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh
}

func env() protocols.Environment {
	switch os.Getenv("ENV") {
	case "production":
		return protocols.ProductionEnvironment
	case "test":
		return protocols.TestEnvironment
	default:
		return protocols.IntegrationEnvironment
	}
}
