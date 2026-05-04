package recovery

import (
	"context"
	"time"

	"github.com/oddin-gg/gosdk/types"
)

// actorEvent is the marker interface for messages on a recoveryActor's
// inbox. Phase 5 v2 actor model: per-producer state is owned by a
// single goroutine; everything that mutates that state arrives as a
// typed event on the inbox.
type actorEvent interface{ isActorEvent() }

// --- inbound feed events ---

// evMsgProcessingStarted: an AMQP message for this producer entered the
// processing pipeline.
type evMsgProcessingStarted struct{ timestamp time.Time }

// evMsgProcessingEnded: the message finished processing. timestamp is
// the message's gen timestamp (zero when not applicable, e.g. alive).
type evMsgProcessingEnded struct{ timestamp time.Time }

// evAlive: an alive heartbeat arrived for this producer.
type evAlive struct {
	timestamp       types.MessageTimestamp
	isSubscribed    bool
	messageInterest types.MessageInterest
}

// evSnapshotComplete: a snapshot-complete arrived. The actor decides
// whether it terminates a snapshot recovery, an event recovery, or is
// stale/unknown.
type evSnapshotComplete struct {
	requestID       uint
	messageInterest types.MessageInterest
}

// --- inbound commands (synchronous via reply channel) ---

// evRecoverEvent triggers a per-event recovery API call. The reply
// channel carries the resulting *Handle (or an error).
type evRecoverEvent struct {
	ctx              context.Context
	eventID          types.URN
	statefulRecovery bool
	reply            chan recoverEventReply
}

// recoverEventReply is the response payload sent back on the
// evRecoverEvent.reply channel.
type recoverEventReply struct {
	handle *Handle
	err    error
}

// --- internal events ---

// evTick is the periodic inactivity check. Fans out from the manager's
// ticker; each actor receives its own copy.
type evTick struct{ now time.Time }

func (evMsgProcessingStarted) isActorEvent() {}
func (evMsgProcessingEnded) isActorEvent()   {}
func (evAlive) isActorEvent()                {}
func (evSnapshotComplete) isActorEvent()     {}
func (evRecoverEvent) isActorEvent()         {}
func (evTick) isActorEvent()                 {}
