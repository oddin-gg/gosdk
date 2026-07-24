package recovery

import (
	"time"

	"github.com/oddin-gg/gosdk/types"
)

type recoveryInfoImpl struct {
	after      time.Time
	timestamp  time.Time
	requestID  int
	successful bool
	nodeID     types.Optional[int]
}

func (r recoveryInfoImpl) After() time.Time {
	return r.after
}

func (r recoveryInfoImpl) Timestamp() time.Time {
	return r.timestamp
}

func (r recoveryInfoImpl) RequestID() int {
	return r.requestID
}

func (r recoveryInfoImpl) Successful() bool {
	return r.successful
}

func (r recoveryInfoImpl) NodeID() types.Optional[int] {
	return r.nodeID
}

// newRecoveryInfoImpl accepts *int from upstream call sites
// (cfg.SdkNodeID() returns *int) and converts at the boundary.
func newRecoveryInfoImpl(
	after time.Time,
	timestamp time.Time,
	requestID int,
	successful bool,
	nodeID *int) types.RecoveryInfo {
	return &recoveryInfoImpl{
		after:      after,
		timestamp:  timestamp,
		requestID:  requestID,
		successful: successful,
		nodeID:     types.FromPtr(nodeID),
	}
}
