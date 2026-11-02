package gosdk

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/types"
)

func snapshotDeadlineBase() []Option {
	return []Option{WithAPIHost("api.example.com"), WithMQHost("mq.example.com")}
}

func TestConfig_SnapshotCompleteDeadline_DefaultAndOption(t *testing.T) {
	cfg := NewConfig("token", types.IntegrationEnvironment, snapshotDeadlineBase()...)
	if got := cfg.SnapshotCompleteDeadline(); got != 5*time.Minute {
		t.Fatalf("default = %v, want 5m", got)
	}
	if err := validateConfig(&cfg); err != nil {
		t.Fatalf("default config rejected: %v", err)
	}

	cfg = NewConfig("token", types.IntegrationEnvironment,
		append(snapshotDeadlineBase(), WithSnapshotCompleteDeadline(90*time.Second))...)
	if got := cfg.SnapshotCompleteDeadline(); got != 90*time.Second {
		t.Fatalf("option = %v, want 90s", got)
	}
	if err := validateConfig(&cfg); err != nil {
		t.Fatalf("valid deadline rejected: %v", err)
	}
	// Boundary: equal to MaxRecoveryExecution is allowed.
	cfg = NewConfig("token", types.IntegrationEnvironment,
		append(snapshotDeadlineBase(), WithMaxRecoveryExecution(time.Hour), WithSnapshotCompleteDeadline(time.Hour))...)
	if err := validateConfig(&cfg); err != nil {
		t.Fatalf("deadline == MaxRecoveryExecution rejected: %v", err)
	}
}

func TestConfig_SnapshotCompleteDeadline_Validation(t *testing.T) {
	bad := []struct {
		name string
		opts []Option
	}{
		{"zero", []Option{WithSnapshotCompleteDeadline(0)}},
		{"negative", []Option{WithSnapshotCompleteDeadline(-time.Second)}},
		{"above the default cap", []Option{WithSnapshotCompleteDeadline(7 * time.Hour)}},
		{"above an explicit cap", []Option{WithMaxRecoveryExecution(time.Hour), WithSnapshotCompleteDeadline(2 * time.Hour)}},
		{"explicit deadline above a lowered cap, either order", []Option{WithSnapshotCompleteDeadline(10 * time.Minute), WithMaxRecoveryExecution(time.Minute)}},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			cfg := NewConfig("token", types.IntegrationEnvironment, append(snapshotDeadlineBase(), tc.opts...)...)
			err := validateConfig(&cfg)
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("validateConfig = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

// A caller that only lowered MaxRecoveryExecution below the 5 minute
// default never asked for a deadline: the default follows the cap instead
// of turning their existing config invalid.
func TestConfig_SnapshotCompleteDeadline_DefaultFollowsLoweredCap(t *testing.T) {
	cfg := NewConfig("token", types.IntegrationEnvironment,
		append(snapshotDeadlineBase(), WithMaxRecoveryExecution(2*time.Minute))...)
	if got := cfg.SnapshotCompleteDeadline(); got != 2*time.Minute {
		t.Fatalf("deadline = %v, want it lowered to the 2m cap", got)
	}
	if err := validateConfig(&cfg); err != nil {
		t.Fatalf("config that only lowered MaxRecoveryExecution rejected: %v", err)
	}
	// A non-positive cap keeps its own (earlier) error.
	cfg = NewConfig("token", types.IntegrationEnvironment, append(snapshotDeadlineBase(), WithMaxRecoveryExecution(0))...)
	if err := validateConfig(&cfg); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("validateConfig = %v, want ErrInvalidConfig", err)
	}
}

func TestConfig_SnapshotCompleteDeadline_AdapterAndString(t *testing.T) {
	cfg := NewConfig("token", types.IntegrationEnvironment, WithSnapshotCompleteDeadline(42*time.Second))
	if got := newConfigAdapter(&cfg).SnapshotCompleteDeadline(); got != 42*time.Second {
		t.Fatalf("adapter = %v, want 42s", got)
	}
	if got := cfg.String(); !strings.Contains(got, "snapshotCompleteDeadline:42s") {
		t.Fatalf("Config.String() = %q, want it to show snapshotCompleteDeadline:42s", got)
	}
}
