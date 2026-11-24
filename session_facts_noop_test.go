package gosdk

import (
	"time"

	"github.com/google/uuid"

	"github.com/oddin-gg/gosdk/internal/recovery"
	"github.com/oddin-gg/gosdk/types"
)

// The recorders of the session tests that do not look at the per-session
// facts take them and drop them.

func (*lossRecordingProcessor) OnMessageProcessed(uuid.UUID, int, time.Time, recovery.Stamp, int) {}
func (*lossRecordingProcessor) OnSessionAlive(uuid.UUID, int, time.Time, recovery.Stamp, bool, types.MessageInterest) {
}

func (*fakeSnapshotProcessor) OnMessageProcessed(uuid.UUID, int, time.Time, recovery.Stamp, int) {}
func (*fakeSnapshotProcessor) OnSessionAlive(uuid.UUID, int, time.Time, recovery.Stamp, bool, types.MessageInterest) {
}

func (*recordingRecoveryProcessor) OnMessageProcessed(uuid.UUID, int, time.Time, recovery.Stamp, int) {
}
func (*recordingRecoveryProcessor) OnSessionAlive(uuid.UUID, int, time.Time, recovery.Stamp, bool, types.MessageInterest) {
}

func (noopRecoveryProcessor) OnMessageProcessed(uuid.UUID, int, time.Time, recovery.Stamp, int) {}
func (noopRecoveryProcessor) OnSessionAlive(uuid.UUID, int, time.Time, recovery.Stamp, bool, types.MessageInterest) {
}

func (*countingRecoveryProcessor) OnMessageProcessed(uuid.UUID, int, time.Time, recovery.Stamp, int) {
}
func (*countingRecoveryProcessor) OnSessionAlive(uuid.UUID, int, time.Time, recovery.Stamp, bool, types.MessageInterest) {
}

func (*sessionHookRecorder) OnMessageProcessed(uuid.UUID, int, time.Time, recovery.Stamp, int) {}
func (*sessionHookRecorder) OnSessionAlive(uuid.UUID, int, time.Time, recovery.Stamp, bool, types.MessageInterest) {
}
