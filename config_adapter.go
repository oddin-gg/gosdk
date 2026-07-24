package gosdk

import (
	"time"

	"github.com/oddin-gg/gosdk/internal/config"
	"github.com/oddin-gg/gosdk/types"
)

// configAdapter exposes gosdk.Config through the internal config.Config
// interface so the internal/* managers (api.Client, recovery.Manager,
// feed.Client, ...) keep a single import for their config dependency.
//
// The adapter is a thin shim — every method is a forward to the
// underlying *Config. It exists because internal/* cannot import the
// top-level gosdk package (cycle), so the interface lives in a neutral
// internal/config package and both sides import it.
type configAdapter struct {
	cfg *Config
}

func newConfigAdapter(cfg *Config) config.Config { return &configAdapter{cfg: cfg} }

func (a *configAdapter) AccessToken() *string {
	v := a.cfg.accessToken
	return &v
}

func (a *configAdapter) DefaultLocale() types.Locale         { return a.cfg.defaultLocale }
func (a *configAdapter) MaxInactivity() time.Duration        { return a.cfg.maxInactivity }
func (a *configAdapter) MaxRecoveryExecution() time.Duration { return a.cfg.maxRecoveryExecution }
func (a *configAdapter) MessagingPort() int                  { return a.cfg.messagingPort }
func (a *configAdapter) SdkNodeID() *int                     { return a.cfg.SdkNodeID() }

func (a *configAdapter) SelectedEnvironment() *types.Environment {
	v := a.cfg.selectedEnvironment
	return &v
}

func (a *configAdapter) SelectedRegion() types.Region { return a.cfg.selectedRegion }
func (a *configAdapter) ExchangeName() string         { return a.cfg.exchangeName }
func (a *configAdapter) ReplayExchangeName() string   { return a.cfg.replayExchangeName }
func (a *configAdapter) ReportExtendedData() bool     { return a.cfg.reportExtendedData }
func (a *configAdapter) SportIDPrefix() string        { return a.cfg.sportIDPrefix }

func (a *configAdapter) APIURL() (string, error) {
	if len(a.cfg.forcedAPIHost) > 0 {
		return a.cfg.forcedAPIHost, nil
	}
	return a.cfg.selectedEnvironment.APIEndpoint(a.cfg.selectedRegion)
}

func (a *configAdapter) MQURL() (string, error) {
	if len(a.cfg.forcedMQHost) > 0 {
		return a.cfg.forcedMQHost, nil
	}
	return a.cfg.selectedEnvironment.MQEndpoint(a.cfg.selectedRegion)
}
