package recovery

import (
	"sync/atomic"
	"time"

	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

// TEMPORARY DIAGNOSTICS — branch temp/recovery-tracing only, do not merge.
//
// Every line this branch adds carries a "trace:" prefix, so one Loki
// line-filter (|= "trace:") returns the whole feed → session → recovery
// picture: what the broker delivered, where the pipeline blocked, and
// every branch that discards a message.
//
// The immediate question this exists to answer: a snapshot_complete the
// feed provably published never reached the recovery actor, and every
// code path that could have swallowed it is silent.

// managerGenSeq numbers Manager instances. Client.resetConnectionLayer
// builds a fresh Manager (with an empty actor map) while an already-open
// session keeps a pointer to the previous one — a session on generation N
// calling into generation N+1 is exactly the split that loses a
// snapshot_complete with no log at all. Every trace line carries the
// generation so the two can be told apart.
var managerGenSeq atomic.Int64

func nextManagerGen() int64 { return managerGenSeq.Add(1) }

// TraceGen identifies this Manager instance for correlation.
func (m *Manager) TraceGen() int64 { return m.gen }

// traceLogger stamps the manager generation onto actor loggers.
func (m *Manager) traceLogger() *log.Logger {
	return m.logger.WithField("mgr_gen", m.gen)
}

func recoveryStateName(s types.RecoveryState) string {
	switch s {
	case types.DefaultRecoveryState:
		return "default"
	case types.NotStartedRecoveryState:
		return "not_started"
	case types.StartedRecoveryState:
		return "started"
	case types.CompletedRecoveryState:
		return "completed"
	case types.InterruptedRecoveryState:
		return "interrupted"
	case types.ErrorRecoveryState:
		return "error"
	default:
		return "unknown"
	}
}

func downReasonName(r types.ProducerDownReason) string {
	switch r {
	case types.DefaultProducerDownReason:
		return "default"
	case types.AliveInternalViolationProducerDownReason:
		return "alive_interval_violation"
	case types.ProcessingQueueDelayViolationProducerDownReason:
		return "processing_queue_delay_violation"
	case types.OtherProducerDownReason:
		return "other"
	default:
		return "unknown"
	}
}

func nodeIDValue(nodeID *int) int {
	if nodeID == nil {
		return -1
	}
	return *nodeID
}

// ageMS renders "how long ago" in milliseconds, or -1 for a zero time,
// so a missing timestamp is distinguishable from a fresh one.
func ageMS(now, t time.Time) int64 {
	if t.IsZero() {
		return -1
	}
	return now.Sub(t).Milliseconds()
}

// traceState is the full recovery-relevant state of one producer actor.
// Actor-goroutine only (it reads unguarded actor fields), which is why
// every caller sits inside dispatch.
func (a *recoveryActor) traceState(now time.Time) []any {
	var (
		requestID   int
		recoveryAge int64 = -1
	)
	if a.currentRecovery != nil {
		requestID = a.currentRecovery.recoveryID
		recoveryAge = ageMS(now, a.lastRecoveryStartedAt())
	}

	lastProcessed, lastProcessedErr := a.lastProcessedMessageGenTimestamp()
	var systemAlive time.Time
	if a.lastSystemAlive != nil {
		systemAlive = *a.lastSystemAlive
	}

	fields := []any{
		"producer_id", a.producerID,
		"recovery_state", recoveryStateName(a.recoveryState),
		"performing_recovery", a.isPerformingRecovery(),
		"request_id", requestID,
		"recovery_age_ms", recoveryAge,
		"max_recovery_execution_ms", a.cfg.MaxRecoveryExecution().Milliseconds(),
		"flagged_down", a.isFlaggedDown(),
		"disabled", a.isDisabled(),
		"down_reason", downReasonName(a.downReason),
		"status_reason", int(a.statusReason),
		"timing_ok", a.calculateTiming(now),
		"max_inactivity_ms", a.cfg.MaxInactivity().Milliseconds(),
		"last_processed_age_ms", ageMS(now, lastProcessed),
		"system_alive_age_ms", ageMS(now, systemAlive),
		"user_alive_age_ms", ageMS(now, a.lastUserSessionAlive),
		"valid_alive_gen_age_ms", ageMS(now, a.lastValidAliveGen),
		"inbox_len", len(a.inbox),
		"inbox_cap", cap(a.inbox),
		"event_recoveries", len(a.eventRecoveries),
		"first_recovery_completed", a.firstRecoveryCompleted,
		"node_id", nodeIDValue(a.cfg.SdkNodeID()),
	}
	if lastProcessedErr != nil {
		fields = append(fields, "last_processed_err", lastProcessedErr.Error())
	}
	return fields
}

// aliveBranchName labels which arm of systemAliveReceived's switch an
// alive took — the arm that can lift a producer-down latch.
func aliveBranchName(backFromInactivity, inRecovery bool) string {
	switch {
	case backFromInactivity:
		return "back_from_inactivity"
	case inRecovery:
		return "in_recovery"
	default:
		return "make_snapshot_recovery"
	}
}
