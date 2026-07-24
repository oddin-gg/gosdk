// Package gosdk is the Go client SDK for the Oddin betting feed.
//
// # Two usage modes
//
// The SDK supports two distinct usage modes against a single
// Client instance — the choice is made by which methods you call,
// not by configuration.
//
// 1. API-only mode (HTTP catalog/entity reads, replay queue
// management). gosdk.New(ctx, cfg) makes one HTTP whoami probe and
// allocates the in-memory managers. No AMQP connection is opened.
// Subsequent calls to Sport, Match, Tournament, MarketDescriptions,
// BookmakerDetails, Producers, Replay().AddEvent (etc.) are pure
// HTTP — the AMQP feed never opens.
//
// 2. Full feed mode (live AMQP messages + recovery). Calling
// Connect(ctx) or Subscribe(ctx, ...) lazy-opens the AMQP
// connection, starts the recovery state machine, and begins
// dispatching live messages.
//
// Either mode can be used in isolation, or both can be mixed
// against the same Client (catalog reads served from HTTP cache,
// AMQP messages served via Subscribe). Close(ctx) is correct in
// either mode — it nil-checks every connection-layer component
// before tearing it down.
//
// # API-only example
//
// Construct, query, close — no AMQP touched. Use a FRESH bounded ctx
// per operation (and for the deferred Close): reusing one deadline
// across construction, every call, and Close lets earlier steps consume
// the later ones' budget — and hands Close an already-expired ctx.
//
//	cfg := gosdk.NewConfig(token, types.IntegrationEnvironment)
//	bootCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
//	defer cancel()
//	c, err := gosdk.New(bootCtx, cfg)
//	if err != nil { return err }
//	defer func() {
//	    closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
//	    defer cancel()
//	    _ = c.Close(closeCtx)
//	}()
//
//	callCtx := func() (context.Context, context.CancelFunc) {
//	    return context.WithTimeout(context.Background(), 10*time.Second)
//	}
//	ctx1, cancel1 := callCtx()
//	defer cancel1()
//	bd, err := c.BookmakerDetails(ctx1)          // HTTP
//	ctx2, cancel2 := callCtx()
//	defer cancel2()
//	sports, err := c.Sports(ctx2)                // HTTP
//	// ... one fresh ctx per call: Match, MarketDescriptions,
//	// Replay().AddEvent, and so on.
//
// # Methods that do require the feed
//
// These open AMQP (or fail with ErrManagerNotOpen) and should only
// be used in full feed mode:
//
//   - Connect(ctx), Subscribe(ctx, ...) — lazy-open the AMQP feed.
//   - RecoverEventOdds(ctx, producerID, eventID),
//     RecoverEventStateful(ctx, producerID, eventID) — require the
//     recovery manager to be Open (returns ErrManagerNotOpen
//     otherwise; the recovery manager is Open'd as part of
//     Connect / Subscribe).
//   - The ConnectionEvents(), RecoveryEvents() and APIEvents()
//     channels exist on the Client regardless of mode. APIEvents fire
//     as soon as the client makes HTTP calls — including the who-am-i
//     probe during New, before any feed connection — when API-call
//     logging is enabled. ConnectionEvents and RecoveryEvents relate to
//     the feed and only emit once it is connecting/connected.
//
// See examples/api_only for an end-to-end API-only example, and
// examples/basic for the simplest full-feed example.
package gosdk
