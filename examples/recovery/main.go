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
	"strconv"
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
		cancel()
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

	// Fresh bounded ctx per independent operation (see doc.go): a shared
	// deadline makes later steps fail for earlier steps' latency.
	callCtx := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), 10*time.Second)
	}

	listCtx, cancelList := callCtx()
	defer cancelList()
	prods, err := c.ProducersInScope(listCtx, types.LiveProducerScope)
	if err != nil {
		log.Fatalf("producers: %v", err)
	}
	if len(prods) == 0 {
		log.Fatal("no live-scope producers available")
	}
	// Select the producer that actually serves the event. Prefer an
	// explicit PRODUCER_ID; otherwise fall back to the lowest-ID live
	// producer. ProducersInScope returns a deterministic, ID-ascending
	// slice, so the fallback is stable across runs (not an arbitrary map
	// pick) — but a real integration should always target a known producer.
	live := prods[0]
	if raw := os.Getenv("PRODUCER_ID"); raw != "" {
		pid, perr := strconv.Atoi(raw)
		if perr != nil {
			log.Fatalf("PRODUCER_ID %q is not an integer: %v", raw, perr)
		}
		lookupCtx, cancelLookup := callCtx()
		defer cancelLookup()
		p, gerr := c.Producer(lookupCtx, pid)
		if gerr != nil {
			log.Fatalf("producer %d: %v", pid, gerr)
		}
		live = p
	}
	log.Printf("requesting odds recovery on producer %d (%s) for %s", live.ID(), live.Name(), eventURN.ToString())

	recoverCtx, cancelRecover := callCtx()
	defer cancelRecover()
	handle, err := c.RecoverEventOdds(recoverCtx, live.ID(), *eventURN)
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
			// Switch on Kind — the documented discriminator (both payload
			// fields are nilable interfaces; Kind removes the guesswork).
			switch ev.Kind {
			case gosdk.RecoveryEventKindProducerStatus:
				log.Printf("producer status: producer=%d down=%v reason=%v",
					ev.ProducerStatus.Producer().ID(),
					ev.ProducerStatus.IsDown(),
					ev.ProducerStatus.ProducerStatusReason())
			case gosdk.RecoveryEventKindEventRecovery:
				log.Printf("event recovery complete: event=%s requestID=%d",
					ev.EventRecovery.EventID().ToString(), ev.EventRecovery.RequestID())
			default:
				// RecoveryEventKindUnknown is never emitted by the SDK;
				// ignore any future kind rather than mishandle it.
			}
		}
	}()

	// Connection events: the feed layer reports Connected /
	// Reconnecting / Disconnected transitions here — a recovery-driven
	// integration should watch them, since a reconnect is exactly when
	// producers flag down and recoveries fire. (Lossy channel — use
	// client.ConnectionState() when only the current state matters.)
	go func() {
		for ev := range c.ConnectionEvents() {
			log.Printf("connection: %v", ev)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh
}

func env() types.Environment {
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
