package feed

import (
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/oddin-gg/gosdk/internal/factory"
	log "github.com/oddin-gg/gosdk/internal/log"
)

// recordingAcknowledger fails any Ack/Nack call and counts attempts.
// processDelivery must NOT call ack/nack on its error paths — that's
// the caller's job after admission.
type recordingAcknowledger struct {
	acks  atomic.Int32
	nacks atomic.Int32
}

func (r *recordingAcknowledger) Ack(uint64, bool) error {
	r.acks.Add(1)
	return errors.New("test: Ack should not be called from processDelivery")
}
func (r *recordingAcknowledger) Nack(uint64, bool, bool) error {
	r.nacks.Add(1)
	return errors.New("test: Nack should not be called from processDelivery")
}
func (r *recordingAcknowledger) Reject(uint64, bool) error { return nil }

// TestProcessDelivery_MalformedRouteAdmitsAsUnparsable verifies the
// v2.23 fix to finding F2: the doc says "routing-key parse failures
// are admitted to the buffer" but the implementation acked-and-
// dropped. Pre-fix, a corrupted route silently disappeared; post-fix
// it surfaces as UnparsableMessage with a minimal RoutingKeyInfo
// carrying just the raw route. Acking is the caller's job after
// admission — processDelivery itself must not touch the
// Acknowledger on this path.
func TestProcessDelivery_MalformedRouteAdmitsAsUnparsable(t *testing.T) {
	logger := log.New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	c := &ChannelConsumer{
		feedMessageFactory: &factory.FeedMessageFactory{},
		logger:             logger,
		// messageInterest is only read on the success path; nil here
		// is fine because malformed routes short-circuit before that.
	}

	ack := &recordingAcknowledger{}
	d := amqp.Delivery{
		Acknowledger: ack,
		// 5 parts — fails parseRoute's "8 parts" check.
		RoutingKey: "garbage.route",
		Body:       []byte(`<oddsChange/>`),
		Timestamp:  time.Unix(1_700_000_000, 0).UTC(),
	}

	qm := c.processDelivery(t.Context(), d)
	if qm == nil {
		t.Fatal("malformed route was acked-and-dropped (qm == nil) — F2 not in effect")
	}
	if qm.UnparsableMessage == nil {
		t.Fatal("qm.UnparsableMessage = nil; want populated unparsable envelope")
	}
	if qm.FeedMessage != nil || qm.RawFeedMessage != nil {
		t.Errorf("malformed route produced FeedMessage/RawFeedMessage: %+v / %+v", qm.FeedMessage, qm.RawFeedMessage)
	}
	if ack.acks.Load() != 0 {
		t.Errorf("Acknowledger.Ack was called %d time(s) — should be deferred to caller", ack.acks.Load())
	}
	if ack.nacks.Load() != 0 {
		t.Errorf("Acknowledger.Nack was called %d time(s) — malformed routes should NOT nack", ack.nacks.Load())
	}
}
