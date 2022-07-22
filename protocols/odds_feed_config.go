package protocols

import "github.com/pkg/errors"

// Environment ...
type Environment int

// Environments
const (
	UnknownEnvironment     Environment = 0
	IntegrationEnvironment Environment = 1
	ProductionEnvironment  Environment = 2
	// Used for internal purposes
	TestEnvironment Environment = 3
)

// APIEndpoint ...
func (e Environment) APIEndpoint() (string, error) {
	switch e {
	case IntegrationEnvironment:
		return "api-mq.integration.oddin.gg", nil
	case ProductionEnvironment:
		return "api-mq.oddin.gg", nil
	case TestEnvironment:
		return "api-mq-test.integration.oddin.gg", nil
	default:
		return "", errors.Errorf("unknown environment %d", e)
	}
}

// MQEndpoint ...
func (e Environment) MQEndpoint() (string, error) {
	switch e {
	case IntegrationEnvironment:
		return "mq.integration.oddin.gg", nil
	case ProductionEnvironment:
		return "mq.oddin.gg", nil
	case TestEnvironment:
		return "mq-test.integration.oddin.gg", nil
	default:
		return "", errors.Errorf("unknown environment %d", e)
	}
}

// Locale ...
type Locale string

// Locales
const (
	EnLocale Locale = "en"
	RuLocale Locale = "ru"
	ZhLocale Locale = "zh"
)

// OddsFeedConfiguration ...
type OddsFeedConfiguration interface {
	AccessToken() *string
	DefaultLocale() Locale
	MaxInactivitySeconds() int
	MaxRecoveryExecutionMinutes() int
	MessagingPort() int
	SdkNodeID() *int
	SelectedEnvironment() *Environment
	ExchangeName() string
	ReplayExchangeName() string
	ReportExtendedData() bool
	SetAPIURL(url string)
	SetMQURL(url string)
	APIURL() (string, error)
	MQURL() (string, error)
}
