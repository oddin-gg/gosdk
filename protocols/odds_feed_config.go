package protocols

import (
	"fmt"
)

// Environment ...
type Environment int

// Environments
const (
	UnknownEnvironment Environment = iota
	IntegrationEnvironment
	ProductionEnvironment
	// Used for internal purposes
	TestEnvironment
)

// APIEndpoint ...
func (e Environment) APIEndpoint(region Region) (string, error) {
	switch e {
	case IntegrationEnvironment:
		return fmt.Sprintf("api-mq.integration.%soddin.gg", region), nil
	case ProductionEnvironment:
		return fmt.Sprintf("api-mq.%soddin.gg", region), nil
	case TestEnvironment:
		return fmt.Sprintf("api-mq-test.integration.%soddin.dev", region), nil
	default:
		return "", fmt.Errorf("unknown environment %d", e)
	}
}

// MQEndpoint ...
func (e Environment) MQEndpoint(region Region) (string, error) {
	switch e {
	case IntegrationEnvironment:
		return fmt.Sprintf("mq.integration.%soddin.gg", region), nil
	case ProductionEnvironment:
		return fmt.Sprintf("mq.%soddin.gg", region), nil
	case TestEnvironment:
		return fmt.Sprintf("mq-test.integration.%soddin.dev", region), nil
	default:
		return "", fmt.Errorf("unknown environment %d", e)
	}
}

// Region ...
type Region string

// Regions
const (
	// RegionDefault is the canonical name for the default (EU) region.
	RegionDefault Region = ""
	APSouthEast1  Region = "ap-southeast-1."

	// DefaulRegion is preserved for source compatibility.
	//
	// Deprecated: misspelling kept as an alias; use RegionDefault.
	DefaulRegion Region = RegionDefault
)

// Locale ...
type Locale string

// Locales — full set matching .NET / Java SDKs.
const (
	EnLocale Locale = "en"
	BrLocale Locale = "br"
	DeLocale Locale = "de"
	EsLocale Locale = "es"
	FiLocale Locale = "fi"
	FrLocale Locale = "fr"
	PlLocale Locale = "pl"
	PtLocale Locale = "pt"
	RuLocale Locale = "ru"
	ThLocale Locale = "th"
	ViLocale Locale = "vi"
	ZhLocale Locale = "zh"
)

// AllLocales lists every Locale constant exposed by the SDK.
// Useful for callers that want to preload everything via WithPreloadLocales(...).
var AllLocales = []Locale{
	EnLocale, BrLocale, DeLocale, EsLocale, FiLocale, FrLocale,
	PlLocale, PtLocale, RuLocale, ThLocale, ViLocale, ZhLocale,
}

// OddsFeedConfiguration ...
type OddsFeedConfiguration interface {
	AccessToken() *string
	DefaultLocale() Locale
	MaxInactivitySeconds() int
	MaxRecoveryExecutionMinutes() int
	MessagingPort() int
	SdkNodeID() *int
	SelectedEnvironment() *Environment
	SelectedRegion() Region
	SetRegion(region Region) OddsFeedConfiguration
	ExchangeName() string
	SetExchangeName(exchangeName string) OddsFeedConfiguration
	ReplayExchangeName() string
	ReportExtendedData() bool
	SetAPIURL(url string) OddsFeedConfiguration
	SetMQURL(url string) OddsFeedConfiguration
	SetMessagingPort(port int) OddsFeedConfiguration
	APIURL() (string, error)
	MQURL() (string, error)
	SportIDPrefix() string
	SetSportIDPrefix(prefix string) OddsFeedConfiguration
}
