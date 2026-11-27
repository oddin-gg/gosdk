package gosdk

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/types"
)

func staleBase() []Option {
	return []Option{WithAPIHost("api.example.com"), WithMQHost("mq.example.com")}
}

func TestConfig_StaleMessage_DefaultsAndOptions(t *testing.T) {
	cfg := NewConfig("token", types.IntegrationEnvironment, staleBase()...)
	if cfg.StaleMessageLimit() != 2*time.Minute || cfg.StaleMessageWindow() != time.Minute {
		t.Fatalf("defaults = %v, %v, want 2m, 1m", cfg.StaleMessageLimit(), cfg.StaleMessageWindow())
	}
	if err := validateConfig(&cfg); err != nil {
		t.Fatalf("default config rejected: %v", err)
	}

	cfg = NewConfig("token", types.IntegrationEnvironment,
		append(staleBase(), WithStaleMessageLimit(30*time.Second), WithStaleMessageWindow(5*time.Second))...)
	if cfg.StaleMessageLimit() != 30*time.Second || cfg.StaleMessageWindow() != 5*time.Second {
		t.Fatalf("options = %v, %v, want 30s, 5s", cfg.StaleMessageLimit(), cfg.StaleMessageWindow())
	}
	if err := validateConfig(&cfg); err != nil {
		t.Fatalf("valid settings rejected: %v", err)
	}
	a := newConfigAdapter(&cfg)
	stale, ok := a.(interface {
		StaleMessageLimit() time.Duration
		StaleMessageWindow() time.Duration
	})
	if !ok || stale.StaleMessageLimit() != 30*time.Second || stale.StaleMessageWindow() != 5*time.Second {
		t.Fatalf("the internal configuration does not carry the settings")
	}
	if got := cfg.String(); !strings.Contains(got, "staleMessageLimit:30s") || !strings.Contains(got, "staleMessageWindow:5s") {
		t.Fatalf("Config.String() = %q lacks the settings", got)
	}
}

func TestConfig_StaleMessage_Bounds(t *testing.T) {
	good := []struct {
		name string
		opts []Option
	}{
		{"limit just over ten seconds", []Option{WithStaleMessageLimit(10*time.Second + time.Millisecond)}},
		{"limit of a day", []Option{WithStaleMessageLimit(24 * time.Hour)}},
		{"window of a second", []Option{WithStaleMessageWindow(time.Second)}},
		{"window of a day", []Option{WithStaleMessageWindow(24 * time.Hour)}},
	}
	for _, tc := range good {
		t.Run(tc.name, func(t *testing.T) {
			cfg := NewConfig("token", types.IntegrationEnvironment, append(staleBase(), tc.opts...)...)
			if err := validateConfig(&cfg); err != nil {
				t.Fatalf("rejected: %v", err)
			}
		})
	}
	bad := []struct {
		name string
		opts []Option
	}{
		{"limit of ten seconds", []Option{WithStaleMessageLimit(10 * time.Second)}},
		{"zero limit", []Option{WithStaleMessageLimit(0)}},
		{"negative limit", []Option{WithStaleMessageLimit(-time.Minute)}},
		{"limit over a day", []Option{WithStaleMessageLimit(24*time.Hour + time.Nanosecond)}},
		{"window under a second", []Option{WithStaleMessageWindow(time.Second - time.Nanosecond)}},
		{"zero window", []Option{WithStaleMessageWindow(0)}},
		{"negative window", []Option{WithStaleMessageWindow(-time.Second)}},
		{"window over a day", []Option{WithStaleMessageWindow(24*time.Hour + time.Nanosecond)}},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			cfg := NewConfig("token", types.IntegrationEnvironment, append(staleBase(), tc.opts...)...)
			if err := validateConfig(&cfg); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("validateConfig = %v, want ErrInvalidConfig", err)
			}
		})
	}
}
