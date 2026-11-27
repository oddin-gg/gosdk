package recovery

import (
	"testing"
	"time"
)

// The safety net's settings come from the configuration when it carries them, and
// are the defaults when it does not.
func TestSettingsOf_StaleMessageSettingsComeFromTheConfigurationOrTheDefaults(t *testing.T) {
	plain := settingsOf(&minimalCfg{}, 0)
	if plain.staleLimit != 2*time.Minute || plain.staleWindow != time.Minute {
		t.Fatalf("defaults = %v, %v, want 2m, 1m", plain.staleLimit, plain.staleWindow)
	}
	set := settingsOf(&staleCfg{}, 0)
	if set.staleLimit != 45*time.Second || set.staleWindow != 7*time.Second {
		t.Fatalf("configured = %v, %v, want 45s, 7s", set.staleLimit, set.staleWindow)
	}
}

type staleCfg struct{ minimalCfg }

func (*staleCfg) StaleMessageLimit() time.Duration  { return 45 * time.Second }
func (*staleCfg) StaleMessageWindow() time.Duration { return 7 * time.Second }
