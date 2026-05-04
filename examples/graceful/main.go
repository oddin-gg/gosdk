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
	)

	bootCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, err := gosdk.New(bootCtx, cfg)
	if err != nil {
		log.Fatalf("gosdk.New: %v", err)
	}

	sub, err := c.Subscribe(context.Background(),
		gosdk.WithMessageInterest(types.AllMessageInterest),
	)
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
		log.Printf("subscription drain: %v", err)
	}
	<-consumed

	if subErr := sub.Err(); subErr != nil && !errors.Is(subErr, context.Canceled) {
		log.Printf("subscription terminated abruptly: %v", subErr)
	} else {
		log.Println("subscription drained gracefully")
	}

	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Close(closeCtx); err != nil {
		log.Printf("client close: %v", err)
	}
}

func handle(msg types.SessionMessage) {
	if msg.UnparsableMessage != nil {
		log.Println("unparsable message")
		return
	}
	log.Printf("message: %T", msg.Message)
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
