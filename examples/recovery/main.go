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

	prods, err := c.ProducersInScope(bootCtx, types.LiveProducerScope)
	if err != nil {
		log.Fatalf("producers: %v", err)
	}
	if len(prods) == 0 {
		log.Fatal("no live-scope producers available")
	}
	live := prods[0]
	log.Printf("requesting odds recovery on producer %d (%s) for %s", live.ID(), live.Name(), eventURN.ToString())

	handle, err := c.RecoverEventOdds(bootCtx, live.ID(), *eventURN)
	if err != nil {
		log.Fatalf("recover: %v", err)
	}
	log.Printf("recovery request id: %d", handle.RequestID())

	// Reliable per-request completion: even if the lossy
	// RecoveryEvents channel drops the event, handle.Done() unblocks
	// when the corresponding SnapshotComplete arrives.
	go func() {
		<-handle.Done()
		res := handle.Result()
		log.Printf("recovery %d %s in %v (err=%v)",
			res.RequestID, res.Status, res.EndedAt.Sub(res.StartedAt), res.Err)
	}()

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

func env() types.Environment {
	switch os.Getenv("ENV") {
	case "production":
		return types.ProductionEnvironment
	case "test":
		return types.TestEnvironment
	default:
		return types.IntegrationEnvironment
	}
}
