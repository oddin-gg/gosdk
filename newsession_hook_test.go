package gosdk

import (
	"testing"

	log "github.com/oddin-gg/gosdk/internal/log"
)

// TestNewSession_InstallsChannelLostHookForLiveSessionsOnly pins the one
// line of wiring between a lost consumer queue and the recovery that
// closes its gap: a live session's consumer carries the channel-lost
// hook, a replay session's does not (historical traffic has no live gap
// to recover, and flagging live producers down for a replay queue rebind
// would force spurious snapshot recoveries).
func TestNewSession_InstallsChannelLostHookForLiveSessionsOnly(t *testing.T) {
	for _, tc := range []struct {
		name     string
		isReplay bool
		want     bool
	}{
		{"live session installs the hook", false, true},
		{"replay session stays silent", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSession(nil, nil, nil, nil, noopRecoveryProcessor{}, "ex", "od:sport:", tc.isReplay, log.New(nil), StrategyCatch, 0)
			impl, ok := s.(*oddsFeedSessionImpl)
			if !ok {
				t.Fatalf("newSession returned %T", s)
			}
			if got := impl.channelConsumer.ChannelLostHookInstalled(); got != tc.want {
				t.Fatalf("hook installed = %v, want %v", got, tc.want)
			}
		})
	}
}
