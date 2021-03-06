package gosdk

import "github.com/oddin-gg/gosdk/protocols"

type configuration struct {
	accessToken                 *string
	defaultLocale               protocols.Locale
	maxInactivitySeconds        int
	maxRecoveryExecutionMinutes int
	messagingPort               int
	sdkNodeID                   *int
	selectedEnvironment         *protocols.Environment
	reportExtendedData          bool
}

func (o configuration) ExchangeName() string {
	return "oddinfeed"
}

func (o configuration) ReplayExchangeName() string {
	return "oddinreplay"
}

func (o configuration) AccessToken() *string {
	return o.accessToken
}

func (o configuration) DefaultLocale() protocols.Locale {
	return o.defaultLocale
}

func (o configuration) MaxInactivitySeconds() int {
	return o.maxInactivitySeconds
}

func (o configuration) MaxRecoveryExecutionMinutes() int {
	return o.maxRecoveryExecutionMinutes
}

func (o configuration) MessagingPort() int {
	return o.messagingPort
}

func (o configuration) SdkNodeID() *int {
	return o.sdkNodeID
}

func (o configuration) SelectedEnvironment() *protocols.Environment {
	return o.selectedEnvironment
}

func (o configuration) ReportExtendedData() bool {
	return o.reportExtendedData
}

// NewConfiguration ...
func NewConfiguration(accessToken string, environment protocols.Environment, nodeID int, reportExtendedData bool) protocols.OddsFeedConfiguration {
	return &configuration{
		defaultLocale:               protocols.EnLocale,
		maxInactivitySeconds:        20,
		maxRecoveryExecutionMinutes: 360,
		messagingPort:               5672,
		accessToken:                 &accessToken,
		selectedEnvironment:         &environment,
		sdkNodeID:                   &nodeID,
		reportExtendedData:          reportExtendedData,
	}
}
