package recovery

import (
	"github.com/oddin-gg/gosdk/protocols"
	"sync"
	"time"
)

type recoveryData struct {
	recoveryID                  uint
	recoveryStartedAt           time.Time
	lock                        sync.Mutex
	interestsOfSnapshotComplete map[protocols.MessageInterest]struct{}
}

func (r *recoveryData) snapshotComplete(messageInterest protocols.MessageInterest) []protocols.MessageInterest {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.interestsOfSnapshotComplete[messageInterest] = struct{}{}
	result := make([]protocols.MessageInterest, len(r.interestsOfSnapshotComplete))

	count := 0
	for key := range r.interestsOfSnapshotComplete {
		result[count] = key
		count++
	}

	return result
}

func newRecoveryData(recoveryID uint, recoveryStartedAt time.Time) recoveryData {
	return recoveryData{
		recoveryID:                  recoveryID,
		recoveryStartedAt:           recoveryStartedAt,
		interestsOfSnapshotComplete: make(map[protocols.MessageInterest]struct{}, 0),
	}
}

type eventRecovery struct {
	recoveryData
	eventID protocols.URN
}

func newEventRecovery(eventID protocols.URN, recoveryID uint, recoveryStartedAt time.Time) eventRecovery {
	return eventRecovery{
		recoveryData: newRecoveryData(recoveryID, recoveryStartedAt),
		eventID:      eventID,
	}
}
