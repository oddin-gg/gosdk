package recovery

import (
	"sync"
	"time"

	"github.com/oddin-gg/gosdk/protocols"
)

// Handle is the per-request handle tracked by recovery.Manager.
// gosdk.Client wraps it as the public *gosdk.RecoveryHandle. The
// internal type lives here so the recovery package can mutate it
// without exposing setters.
type Handle struct {
	requestID  uint
	producerID uint
	eventID    protocols.URN

	done chan struct{}

	mu        sync.RWMutex
	status    protocols.RecoveryRequestStatus
	err       error
	startedAt time.Time
	endedAt   time.Time
}

// NewHandle creates a Pending handle. The Manager registers it before
// the API request is issued.
func NewHandle(requestID, producerID uint, eventID protocols.URN, startedAt time.Time) *Handle {
	return &Handle{
		requestID:  requestID,
		producerID: producerID,
		eventID:    eventID,
		done:       make(chan struct{}),
		status:     protocols.RecoveryStatusPending,
		startedAt:  startedAt,
	}
}

// RequestID returns the recovery request id.
func (h *Handle) RequestID() uint { return h.requestID }

// ProducerID returns the producer that owns this recovery.
func (h *Handle) ProducerID() uint { return h.producerID }

// EventID returns the event under recovery.
func (h *Handle) EventID() protocols.URN { return h.eventID }

// Done returns a channel that closes when the handle reaches a
// terminal state (Completed / Failed / TimedOut).
func (h *Handle) Done() <-chan struct{} { return h.done }

// Status returns the current status without blocking.
func (h *Handle) Status() protocols.RecoveryRequestStatus {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.status
}

// Result returns the terminal result. Blocks until Done.
func (h *Handle) Result() protocols.RecoveryResult {
	<-h.done
	h.mu.RLock()
	defer h.mu.RUnlock()
	return protocols.RecoveryResult{
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
func (h *Handle) Snapshot() protocols.RecoveryResult {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return protocols.RecoveryResult{
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
func (h *Handle) complete(status protocols.RecoveryRequestStatus, err error, endedAt time.Time) {
	h.mu.Lock()
	if h.status != protocols.RecoveryStatusPending {
		h.mu.Unlock()
		return
	}
	h.status = status
	h.err = err
	h.endedAt = endedAt
	h.mu.Unlock()
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
