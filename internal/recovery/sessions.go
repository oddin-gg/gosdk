package recovery

import (
	"github.com/oddin-gg/gosdk/types"
)

// hasLane reports whether the session has a lane of its own to recover: it
// receives messages, and so takes snapshot_complete (every session is bound to
// it, fixed when the session opens). The alive-only interest has none, so it
// holds no recovery and loses nothing.
func hasLane(interest types.MessageInterest) bool {
	return interest != types.SystemAliveOnly
}

// clock is the manager's clock; a manager built without one reads the
// system's.
func (m *Manager) clock() Clock {
	if m.clk == nil {
		return systemClock{}
	}
	return m.clk
}
