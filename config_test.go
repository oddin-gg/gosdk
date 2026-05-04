package gosdk

import (
	"log/slog"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/protocols"
)

func TestNewConfig_Defaults(t *testing.T) {
	cfg := NewConfig("token", protocols.IntegrationEnvironment)

	if cfg.AccessToken() != "token" {
		t.Errorf("token: got %q", cfg.AccessToken())
	}
	if cfg.Environment() != protocols.IntegrationEnvironment {
		t.Errorf("env: got %v", cfg.Environment())
	}
	if cfg.DefaultLocale() != protocols.EnLocale {
		t.Errorf("default locale: got %q", cfg.DefaultLocale())
	}
	if cfg.MaxInactivity() != defaultMaxInactivity {
		t.Errorf("max inactivity: got %v", cfg.MaxInactivity())
	}
	if cfg.MaxRecoveryExecution() != defaultMaxRecoveryExecution {
		t.Errorf("max recovery: got %v", cfg.MaxRecoveryExecution())
	}
	if cfg.SdkNodeID() != nil {
		t.Errorf("sdkNodeID should be nil by default")
	}
	if cfg.Region() != protocols.RegionDefault {
		t.Errorf("region: got %q", cfg.Region())
	}
	if cfg.Logger() != nil {
		t.Errorf("logger should be nil by default")
	}
}

func TestNewConfig_AllOptions(t *testing.T) {
	logger := slog.Default()
	cfg := NewConfig("tok", protocols.TestEnvironment,
		WithNodeID(42),
		WithDefaultLocale(protocols.RuLocale),
		WithPreloadLocales(protocols.EnLocale, protocols.DeLocale),
		WithRegion(protocols.APSouthEast1),
		WithAPIURL("api.example.test"),
		WithMQURL("mq.example.test"),
		WithMessagingPort(5673),
		WithExchangeName("custom_exchange"),
		WithReplayExchangeName("custom_replay"),
		WithSportIDPrefix("sr:sport:"),
		WithMaxInactivity(45*time.Second),
		WithMaxRecoveryExecution(2*time.Hour),
		WithInitialSnapshotTime(15*time.Minute),
		WithHTTPClientTimeout(45*time.Second),
		WithExceptionStrategy(StrategyThrow),
		WithLogger(logger),
		WithExtendedDataReporting(true),
		WithAPICallLogging(APILogResponses),
		WithAPICallBodyLimit(128*1024),
		WithAMQPPrefetch(2000),
		WithSubscriptionBuffer(512),
	)

	if got := cfg.SdkNodeID(); got == nil || *got != 42 {
		t.Errorf("nodeID: got %v", got)
	}
	if cfg.DefaultLocale() != protocols.RuLocale {
		t.Errorf("default locale: got %q", cfg.DefaultLocale())
	}
	preload := cfg.PreloadLocales()
	if len(preload) != 2 || preload[0] != protocols.EnLocale || preload[1] != protocols.DeLocale {
		t.Errorf("preload locales: got %v", preload)
	}
	if cfg.Region() != protocols.APSouthEast1 {
		t.Errorf("region: got %q", cfg.Region())
	}
	if cfg.MaxInactivity() != 45*time.Second {
		t.Errorf("max inactivity: got %v", cfg.MaxInactivity())
	}
	if cfg.MaxRecoveryExecution() != 2*time.Hour {
		t.Errorf("max recovery: got %v", cfg.MaxRecoveryExecution())
	}
	if cfg.Logger() != logger {
		t.Errorf("logger: not propagated")
	}
}

// TestConfig_PreloadLocales_ReturnsCopy verifies that callers can't mutate
// the config's internal locale slice through the returned slice header.
func TestConfig_PreloadLocales_ReturnsCopy(t *testing.T) {
	cfg := NewConfig("t", protocols.TestEnvironment,
		WithPreloadLocales(protocols.EnLocale, protocols.RuLocale),
	)
	preload := cfg.PreloadLocales()
	preload[0] = protocols.DeLocale // mutate the returned copy
	again := cfg.PreloadLocales()
	if again[0] != protocols.EnLocale {
		t.Fatalf("internal slice was mutated through returned copy: got %v", again)
	}
}

// TestConfig_NodeID_ReturnsCopy verifies the internal *int isn't aliased.
func TestConfig_NodeID_ReturnsCopy(t *testing.T) {
	cfg := NewConfig("t", protocols.TestEnvironment, WithNodeID(7))
	id := cfg.SdkNodeID()
	if id == nil || *id != 7 {
		t.Fatalf("got %v", id)
	}
	*id = 99 // mutate through returned pointer
	again := cfg.SdkNodeID()
	if again == nil || *again != 7 {
		t.Fatalf("internal nodeID was mutated through returned pointer: got %v", again)
	}
}

// TestNewConfig_OptionOrderApplies confirms later options override earlier
// ones (consistent with the functional-options idiom).
func TestNewConfig_OptionOrderApplies(t *testing.T) {
	cfg := NewConfig("t", protocols.TestEnvironment,
		WithMaxInactivity(10*time.Second),
		WithMaxInactivity(99*time.Second),
	)
	if cfg.MaxInactivity() != 99*time.Second {
		t.Fatalf("got %v, want 99s (last option wins)", cfg.MaxInactivity())
	}
}
