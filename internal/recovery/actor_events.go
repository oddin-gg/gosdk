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

// evAliveNudge signals that pendingAlive holds a coalesced alive.
// Carries no payload — dispatch reads the latest from the atomic slot.
type evAliveNudge struct{}

func (evAliveNudge) isActorEvent() {}

// evSnapshotComplete: a snapshot-complete arrived. The actor decides
// whether it terminates a snapshot recovery, an event recovery, or is
// stale/unknown.
type evSnapshotComplete struct {
	requestID       int
	messageInterest types.MessageInterest
}

// --- inbound commands (synchronous via reply channel) ---

// evRecoverEvent triggers a per-event recovery API call. The reply
// channel carries the resulting *Handle (or an error).
type evRecoverEvent struct {
	// ctx is the caller's ADMISSION ctx: it bounds the send into the
	// actor inbox and the wait for the reply (in dispatchRecoverEvent).
	// It deliberately does NOT bound the detached recovery API call —
	// onRecoverEvent roots that at the actor lifetime so a caller's
	// post-handle `defer cancel()` can't abort an accepted recovery.
	ctx              context.Context
	eventID          types.URN
	statefulRecovery bool
	reply            chan recoverEventReply
}

// recoverEventReply is the response payload sent back on the
// evRecoverEvent.reply channel.
//
// The reply is sent SYNCHRONOUSLY from the actor as soon as the
// per-event recovery is registered (handle created, eventRecoveries
// map populated) — BEFORE the API call runs. Two consequences: (1)
// callers always observe a handle (or a synchronous failure) within
// microseconds, never blocked on a slow API; (2) the API result feeds
// back to the actor as a typed evRecoverEventCompleted so state
// mutation stays single-threaded.
type recoverEventReply struct {
	handle *Handle
	err    error
}

// evRecoverEventCompleted carries the API call's outcome back to the
// actor goroutine. Mutates eventRecoveries and the handle terminal
// state on the actor goroutine (no shared-state writes from the
// detached API goroutine).
type evRecoverEventCompleted struct {
	requestID int
	success   bool
	err       error
}

// evSnapshotRecoveryAPICompleted carries the outcome of a detached
// PostRecovery call back to the actor goroutine. Pre-fix the
// PostRecovery HTTP call ran inline inside makeSnapshotRecovery on
// the actor goroutine — a slow upstream queued every alive/tick/
// recover-event for that producer behind it. Now the API call is
// detached the same way evRecoverEvent does for event recovery; this
// event delivers the result back so SetProducerRecoveryInfo + state
// transitions happen single-threaded on the actor.
type evSnapshotRecoveryAPICompleted struct {
	requestID    int
	recoverFrom  time.Time
	startedAt    time.Time
	success      bool
	err          error
	producerName string
}

// --- internal events ---

// evTick is the periodic inactivity check. Fans out from the manager's
// ticker; each actor receives its own copy.
// evTick drives the periodic actor scans. inactivityArmed is false during
// the manager's warm-up window (before initialDelay elapses): the
// MaxRecoveryExecution expiry scan runs on EVERY tick regardless, but the
// producer-down inactivity check is suppressed until armed so a producer
// isn't flagged down before its first alive can arrive.
type evTick struct {
	now             time.Time
	inactivityArmed bool
}

func (evMsgProcessingStarted) isActorEvent()         {}
func (evMsgProcessingEnded) isActorEvent()           {}
func (evAlive) isActorEvent()                        {}
func (evSnapshotComplete) isActorEvent()             {}
func (evRecoverEvent) isActorEvent()                 {}
func (evRecoverEventCompleted) isActorEvent()        {}
func (evSnapshotRecoveryAPICompleted) isActorEvent() {}
func (evTick) isActorEvent()                         {}
