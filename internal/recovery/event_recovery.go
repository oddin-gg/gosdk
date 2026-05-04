package recovery

import (
	"github.com/oddin-gg/gosdk/types"
	"sync"
	"time"
)

type recoveryData struct {
	recoveryID                  uint
	recoveryStartedAt           time.Time
	lock                        sync.Mutex
	interestsOfSnapshotComplete map[types.MessageInterest]struct{}
}

func (r *recoveryData) snapshotComplete(messageInterest types.MessageInterest) []types.MessageInterest {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.interestsOfSnapshotComplete[messageInterest] = struct{}{}
	result := make([]types.MessageInterest, len(r.interestsOfSnapshotComplete))

	count := 0
	for key := range r.interestsOfSnapshotComplete {
		result[count] = key
		count++
	}

	return result
}

func newRecoveryData(recoveryID uint, recoveryStartedAt time.Time) *recoveryData {
	return &recoveryData{
		recoveryID:                  recoveryID,
		recoveryStartedAt:           recoveryStartedAt,
		interestsOfSnapshotComplete: make(map[types.MessageInterest]struct{}, 0),
	}
}

type eventRecovery struct {
	*recoveryData
	eventID types.URN
}

func newEventRecovery(eventID types.URN, recoveryID uint, recoveryStartedAt time.Time) *eventRecovery {
	return &eventRecovery{
		recoveryData: newRecoveryData(recoveryID, recoveryStartedAt),
		eventID:      eventID,
	}
}
