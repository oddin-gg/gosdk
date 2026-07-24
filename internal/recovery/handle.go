package recovery

import (
	"sync"
	"time"

	"github.com/oddin-gg/gosdk/types"
)

// Handle is the per-request handle tracked by recovery.Manager.
// gosdk exposes it as the public gosdk.RecoveryHandle via a type
// ALIAS — the internal type lives here so the recovery package can
// mutate it without exposing setters, but its exported method set
// (RequestID / ProducerID / EventID / Done / Status / Result /
// Snapshot / IsTerminal) IS the locked v1.0.0 public contract.
// Deliberate decision (PR #42 review): the surface is small, purely
// read-only accessors over stable value types, and the alias avoids
// a forwarding wrapper that could drift. Consequence: adding an
// exported method is a minor-version change; renaming, removing, or
// changing the signature of any exported method is a BREAKING public
// API change, even though the type lives under internal/.
type Handle struct {
	requestID  int
	producerID int
	eventID    types.URN

	done chan struct{}

	mu        sync.RWMutex
	status    types.RecoveryRequestStatus
	err       error
	startedAt time.Time
	endedAt   time.Time
}

// NewHandle creates a Pending handle. The Manager registers it before
// the API request is issued.
func NewHandle(requestID, producerID int, eventID types.URN, startedAt time.Time) *Handle {
	return &Handle{
		requestID:  requestID,
		producerID: producerID,
		eventID:    eventID,
		done:       make(chan struct{}),
		status:     types.RecoveryStatusPending,
		startedAt:  startedAt,
	}
}

// RequestID returns the recovery request id.
func (h *Handle) RequestID() int { return h.requestID }

// ProducerID returns the producer that owns this recovery.
func (h *Handle) ProducerID() int { return h.producerID }

// EventID returns the event under recovery.
func (h *Handle) EventID() types.URN { return h.eventID }

// Done returns a channel that closes when the handle reaches a
// terminal state (Completed / Failed / TimedOut).
func (h *Handle) Done() <-chan struct{} { return h.done }

// Status returns the current status without blocking.
func (h *Handle) Status() types.RecoveryRequestStatus {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.status
}

// Result returns the terminal result. Blocks until Done.
func (h *Handle) Result() types.RecoveryResult {
	<-h.done
	h.mu.RLock()
	defer h.mu.RUnlock()
	return types.RecoveryResult{
		RequestID:  h.requestID,
		ProducerID: h.producerID,
		EventID:    h.eventID,
		Status:     h.status,
		Err:        h.err,
		StartedAt:  h.startedAt,
		EndedAt:    h.endedAt,
	}
}

// Snapshot returns the current state without blocking. Status may be
// Pending if the handle hasn't completed yet; the caller can use Done
// to wait for terminal state.
func (h *Handle) Snapshot() types.RecoveryResult {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return types.RecoveryResult{
		RequestID:  h.requestID,
		ProducerID: h.producerID,
		EventID:    h.eventID,
		Status:     h.status,
		Err:        h.err,
		StartedAt:  h.startedAt,
		EndedAt:    h.endedAt,
	}
}

// complete transitions the handle to a terminal state. Idempotent —
// subsequent calls are no-ops.
//
// done is closed while STILL HOLDING the lock: unlocking first opened a
// window where a preempted completer had published the terminal fields
// but not yet closed done — a reader could see Snapshot() report a
// terminal status while IsTerminal() was false and Done() still
// blocked. One terminal-state boundary, not two.
func (h *Handle) complete(status types.RecoveryRequestStatus, err error, endedAt time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.status != types.RecoveryStatusPending {
		return
	}
	h.status = status
	h.err = err
	h.endedAt = endedAt
	close(h.done)
}

// IsTerminal reports whether the handle has reached a terminal state.
func (h *Handle) IsTerminal() bool {
	select {
	case <-h.done:
		return true
	default:
		return false
	}
}
