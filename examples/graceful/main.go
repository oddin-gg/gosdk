// graceful demonstrates a clean shutdown: drain the subscription on
// SIGINT, wait up to a deadline, then close the client. The subscription
// surfaces termination via Done()+Err() so callers can distinguish
// graceful drain from abrupt failure.
//
// Env: TOKEN, ENV.
package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/oddin-gg/gosdk"
	"github.com/oddin-gg/gosdk/types"
)

const drainDeadline = 10 * time.Second

func main() {
	token := os.Getenv("TOKEN")
	if token == "" {
		log.Fatal("TOKEN not set")
	}
	cfg := gosdk.NewConfig(token, parseEnv(),
		gosdk.WithMaxInactivity(20*time.Second),
		// Align the total shutdown budget with the drain deadline this
		// example advertises — otherwise the drain is silently capped at
		// the 5s WithShutdownTimeout default (min(caller, shutdownTimeout)).
		gosdk.WithShutdownTimeout(drainDeadline),
	)

	bootCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	c, err := gosdk.New(bootCtx, cfg)
	cancel()
	if err != nil {
		log.Fatalf("gosdk.New: %v", err)
	}

	// Subscribe's ctx bounds SETUP only (lazy-connect dial, queue
	// topology); the subscription's lifetime is governed by sub.Close /
	// client.Close below.
	subCtx, cancelSub := context.WithTimeout(context.Background(), 30*time.Second)
	sub, err := c.Subscribe(subCtx,
		gosdk.WithMessageInterest(types.AllMessageInterest),
	)
	cancelSub()
	if err != nil {
		log.Fatalf("subscribe: %v", err)
	}

	consumed := make(chan struct{})
	go func() {
		defer close(consumed)
		for msg := range sub.Messages() {
			handle(msg)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh
	log.Println("signal received — draining")

	drainCtx, drainCancel := context.WithTimeout(context.Background(), drainDeadline)
	defer drainCancel()

	if err := sub.Close(drainCtx); err != nil {
		log.Printf("subscription drain incomplete: %v", err)
	}

	// Do NOT block on the consumer here. A clean drain closes Messages()
	// once the public buffer has been emptied, so THIS example's range-
	// over-Messages consumer then exits (a slower per-message handler would
	// still be finishing its last item). On a deadline-bounded drain
	// Messages() may still be open (e.g. a broker ACK wedged on a stalled
	// transport keeps the pump from closing it). An unconditional
	// `<-consumed` here would deadlock: the pump only closes Messages()
	// once the transport is torn down, and the teardown is client.Close
	// below — which we would never reach.
	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Close(closeCtx); err != nil {
		log.Printf("client close: %v", err)
	}

	// After a SUCCESSFUL client.Close the transport is down, so any wedged
	// ACK is released and Messages() has closed — the consumer goroutine
	// finishes. If Close returned an error above (deadline/ctx), teardown
	// may still be completing in the background, which is exactly why the
	// join below is BOUNDED, never unconditional.
	select {
	case <-consumed:
	case <-time.After(5 * time.Second):
		log.Println("consumer goroutine did not finish within the join deadline")
	}

	if subErr := sub.Err(); subErr != nil && !errors.Is(subErr, context.Canceled) {
		log.Printf("subscription terminated abruptly: %v", subErr)
	} else {
		log.Println("subscription drained gracefully")
	}
}

func handle(msg types.SessionMessage) {
	switch {
	case msg.UnparsableMessage != nil:
		log.Println("unparsable message")
	case msg.OddsChange != nil:
		log.Printf("odds change: event=%v", msg.OddsChange.Event())
	case msg.BetStop != nil:
		log.Printf("bet stop: event=%v", msg.BetStop.Event())
	case msg.BetSettlement != nil:
		log.Printf("settlement: event=%v", msg.BetSettlement.Event())
	case msg.BetCancel != nil:
		log.Printf("cancel: event=%v", msg.BetCancel.Event())
	case msg.FixtureChange != nil:
		log.Printf("fixture change: event=%v", msg.FixtureChange.Event())
	case msg.RollbackBetSettlement != nil:
		log.Printf("rollback settlement: event=%v", msg.RollbackBetSettlement.Event())
	case msg.RollbackBetCancel != nil:
		log.Printf("rollback cancel: event=%v", msg.RollbackBetCancel.Event())
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
