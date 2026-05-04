package gosdk

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/oddin-gg/gosdk/types"
)

// SubscribeOption tunes a Subscribe call.
type SubscribeOption func(*subscribeConfig)

type subscribeConfig struct {
	messageInterest types.MessageInterest
	specificEvents  map[types.URN]struct{}
	replay          bool
}

// WithMessageInterest selects which messages the subscription receives.
// Default: types.AllMessageInterest.
func WithMessageInterest(m types.MessageInterest) SubscribeOption {
	return func(c *subscribeConfig) { c.messageInterest = m }
}

// WithSpecificEvents narrows the subscription to a fixed set of event URNs.
// Implies SpecifiedMatchesOnlyMessageInterest if no other interest is set.
func WithSpecificEvents(events ...types.URN) SubscribeOption {
	return func(c *subscribeConfig) {
		c.specificEvents = make(map[types.URN]struct{}, len(events))
		for _, e := range events {
			c.specificEvents[e] = struct{}{}
		}
		if c.messageInterest == "" {
			c.messageInterest = types.SpecifiedMatchesOnlyMessageInterest
		}
	}
}

// WithReplay marks the subscription as replay-mode (uses the replay
// exchange and the dummy recovery manager). Equivalent to
// SessionBuilder.BuildReplay() in the legacy API.
func WithReplay() SubscribeOption { return func(c *subscribeConfig) { c.replay = true } }

// Subscription is the v1.0.0 replacement for OddsFeedSession + the
// channel split (session/global). See NEXT.md §4 / §8 Subscriptions.
//
// Lifecycle:
//   - Messages() returns the message stream; the channel closes after a
//     graceful drain or abrupt termination.
//   - Close(ctx) requests a graceful drain; ctx is the drain deadline.
//   - Done() closes when the subscription terminates (any reason).
//   - Err() returns the cause: nil for graceful close, non-nil otherwise.
type Subscription struct {
	id       uuid.UUID
	messages chan types.SessionMessage

	closeOnce sync.Once
	closed    chan struct{}
	closeErr  error

	// underlying is the legacy session this subscription wraps. The
	// adapter goroutine pumps the legacy SessionMessage channel into our
	// outgoing channel.
	underlying sdkOddsFeedSession
	pumpDone   chan struct{}
}

// Messages returns the message stream. Closes after termination.
//
// The envelope's exact-one-of fields (FeedMessage / RawFeedMessage /
// UnparsableMessage) reflect what was decoded; consumers type-switch on
// `Message` for the parsed payload.
func (s *Subscription) Messages() <-chan types.SessionMessage { return s.messages }

// Done closes when the subscription terminates for any reason.
func (s *Subscription) Done() <-chan struct{} { return s.closed }

// Err returns the cause of termination. Nil on graceful close.
// Non-nil only after Done() is closed.
func (s *Subscription) Err() error {
	select {
	case <-s.closed:
		return s.closeErr
	default:
		return nil
	}
}

// Close requests a graceful drain and waits up to the supplied ctx.
// Idempotent. After return Done() is closed; Err() reflects the result.
func (s *Subscription) Close(ctx context.Context) error {
	s.closeOnce.Do(func() { go s.runShutdown(nil) })

	// Fast path: already done. Completed shutdown always wins over ctx.
	select {
	case <-s.closed:
		return s.closeErr
	default:
	}
	select {
	case <-s.closed:
		return s.closeErr
	case <-ctx.Done():
		select {
		case <-s.closed:
			return s.closeErr
		default:
			return ctx.Err()
		}
	}
}

// abortWithErr is called by the parent Client on abrupt shutdown
// (ctx-cancel / client.Close / terminal error). The subscription terminates
// without draining; the legacy session does its own Nack-on-cancel.
func (s *Subscription) abortWithErr(err error) {
	s.closeOnce.Do(func() { go s.runShutdown(err) })
}

func (s *Subscription) runShutdown(terminalErr error) {
	s.closeErr = terminalErr
	if s.underlying != nil {
		s.underlying.Close()
	}
	if s.pumpDone != nil {
		// Wait for the pump goroutine to finish observing the session's
		// closed message channel, with a backstop deadline so we don't
		// hang forever on a stuck legacy session.
		select {
		case <-s.pumpDone:
		case <-time.After(5 * time.Second):
		}
	}
	close(s.messages)
	close(s.closed)
}

// errSubscriptionClosed is returned by Client.Subscribe after the client
// itself has closed.
var errSubscriptionClosed = errors.New("gosdk: subscription closed")
