package gosdk

import "github.com/oddin-gg/gosdk/types"

// configAdapter exposes gosdk.Config through the legacy
// types.OddsFeedConfiguration interface so the existing internal
// machinery (api.Client, recovery.Manager, feed.Client, ...) keeps working
// while the new public Client builds out alongside the legacy NewOddsFeed.
//
// Phase 7a removes both the adapter and the legacy configuration.go once
// internal layers take Config directly.
type configAdapter struct {
	cfg *Config
}

func newConfigAdapter(cfg *Config) types.OddsFeedConfiguration { return &configAdapter{cfg: cfg} }

func (a *configAdapter) AccessToken() *string {
	v := a.cfg.accessToken
	return &v
}

func (a *configAdapter) DefaultLocale() types.Locale { return a.cfg.defaultLocale }
func (a *configAdapter) MaxInactivitySeconds() int       { return int(a.cfg.maxInactivity.Seconds()) }
func (a *configAdapter) MaxRecoveryExecutionMinutes() int {
	return int(a.cfg.maxRecoveryExecution.Minutes())
}
func (a *configAdapter) MessagingPort() int { return a.cfg.messagingPort }
func (a *configAdapter) SdkNodeID() *int    { return a.cfg.SdkNodeID() }

func (a *configAdapter) SelectedEnvironment() *types.Environment {
	v := a.cfg.selectedEnvironment
	return &v
}

func (a *configAdapter) SelectedRegion() types.Region { return a.cfg.selectedRegion }

// SetX methods on the legacy interface — unused by the new Client. The
// adapter ignores mutations because Config is immutable. Returning the
// same interface satisfies the contract.
func (a *configAdapter) SetRegion(types.Region) types.OddsFeedConfiguration { return a }
func (a *configAdapter) SetExchangeName(string) types.OddsFeedConfiguration     { return a }
func (a *configAdapter) SetAPIURL(string) types.OddsFeedConfiguration           { return a }
func (a *configAdapter) SetMQURL(string) types.OddsFeedConfiguration            { return a }
func (a *configAdapter) SetMessagingPort(int) types.OddsFeedConfiguration       { return a }
func (a *configAdapter) SetSportIDPrefix(string) types.OddsFeedConfiguration    { return a }

func (a *configAdapter) ExchangeName() string         { return a.cfg.exchangeName }
func (a *configAdapter) ReplayExchangeName() string   { return a.cfg.replayExchangeName }
func (a *configAdapter) ReportExtendedData() bool     { return a.cfg.reportExtendedData }
func (a *configAdapter) SportIDPrefix() string        { return a.cfg.sportIDPrefix }

func (a *configAdapter) APIURL() (string, error) {
	if len(a.cfg.forcedAPIURL) > 0 {
		return a.cfg.forcedAPIURL, nil
	}
	return a.cfg.selectedEnvironment.APIEndpoint(a.cfg.selectedRegion)
}

func (a *configAdapter) MQURL() (string, error) {
	if len(a.cfg.forcedMQURL) > 0 {
		return a.cfg.forcedMQURL, nil
	}
	return a.cfg.selectedEnvironment.MQEndpoint(a.cfg.selectedRegion)
}
