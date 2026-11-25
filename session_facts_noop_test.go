package gosdk

import (
	"time"

	"github.com/google/uuid"

	"github.com/oddin-gg/gosdk/internal/recovery"
)

// The recorders of the session tests that do not look at the per-session
// facts take them and drop them.

func (*lossRecordingProcessor) OnMessageProcessed(uuid.UUID, int, time.Time, recovery.Stamp, int) {}
func (*lossRecordingProcessor) OnSessionAlive(uuid.UUID, int, time.Time, recovery.Stamp, bool) {
}

func (*fakeSnapshotProcessor) OnMessageProcessed(uuid.UUID, int, time.Time, recovery.Stamp, int) {}
func (*fakeSnapshotProcessor) OnSessionAlive(uuid.UUID, int, time.Time, recovery.Stamp, bool) {
}

func (*recordingRecoveryProcessor) OnMessageProcessed(uuid.UUID, int, time.Time, recovery.Stamp, int) {
}
func (*recordingRecoveryProcessor) OnSessionAlive(uuid.UUID, int, time.Time, recovery.Stamp, bool) {
}

func (noopRecoveryProcessor) OnMessageProcessed(uuid.UUID, int, time.Time, recovery.Stamp, int) {}
func (noopRecoveryProcessor) OnSessionAlive(uuid.UUID, int, time.Time, recovery.Stamp, bool) {
}

func (*countingRecoveryProcessor) OnMessageProcessed(uuid.UUID, int, time.Time, recovery.Stamp, int) {
}
func (*countingRecoveryProcessor) OnSessionAlive(uuid.UUID, int, time.Time, recovery.Stamp, bool) {
}

func (*sessionHookRecorder) OnMessageProcessed(uuid.UUID, int, time.Time, recovery.Stamp, int) {}
func (*sessionHookRecorder) OnSessionAlive(uuid.UUID, int, time.Time, recovery.Stamp, bool) {
}
