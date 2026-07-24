package recovery

import (
	"sync"
	"time"

	"github.com/oddin-gg/gosdk/types"
)

type recoveryData struct {
	recoveryID                  int
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

func newRecoveryData(recoveryID int, recoveryStartedAt time.Time) *recoveryData {
	return &recoveryData{
		recoveryID:                  recoveryID,
		recoveryStartedAt:           recoveryStartedAt,
		interestsOfSnapshotComplete: make(map[types.MessageInterest]struct{}, 0),
	}
}

type eventRecovery struct {
	*recoveryData
	eventID types.URN
	// stateful distinguishes odds-only vs stateful recovery so pending
	// requests for the same event coalesce only within their own kind.
	stateful bool
	// cancelAPI aborts the detached recovery POST. Set by
	// onRecoverEvent; invoked by the expiry sweep so a timed-out
	// recovery does not keep its HTTP request (and goroutine) running
	// for the remainder of the API timeout. Actor-goroutine-only, like
	// the rest of this struct.
	cancelAPI func()
}

func newEventRecovery(eventID types.URN, recoveryID int, recoveryStartedAt time.Time) *eventRecovery {
	return &eventRecovery{
		recoveryData: newRecoveryData(recoveryID, recoveryStartedAt),
		eventID:      eventID,
	}
}
