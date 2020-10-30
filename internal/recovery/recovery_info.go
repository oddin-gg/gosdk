package recovery

import (
	"github.com/oddin-gg/gosdk/protocols"
	"time"
)

type recoveryInfoImpl struct {
	after      time.Time
	timestamp  time.Time
	requestID  uint
	successful bool
	nodeID     *int
}

func (r recoveryInfoImpl) After() time.Time {
	return r.after
}

func (r recoveryInfoImpl) Timestamp() time.Time {
	return r.timestamp
}

func (r recoveryInfoImpl) RequestID() uint {
	return r.requestID
}

func (r recoveryInfoImpl) Successful() bool {
	return r.successful
}

func (r recoveryInfoImpl) NodeID() *int {
	return r.nodeID
}

func newRecoveryInfoImpl(
	after time.Time,
	timestamp time.Time,
	requestID uint,
	successful bool,
	nodeID *int) protocols.RecoveryInfo {
	return &recoveryInfoImpl{
		after:      after,
		timestamp:  timestamp,
		requestID:  requestID,
		successful: successful,
		nodeID:     nodeID,
	}
}
