package protocols

import (
	"fmt"

	"github.com/pkg/errors"
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
		return fmt.Sprintf("api-mq-test.integration.%soddin.gg", region), nil
	default:
		return "", errors.Errorf("unknown environment %d", e)
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
		return fmt.Sprintf("mq-test.integration.%soddin.gg", region), nil
	default:
		return "", errors.Errorf("unknown environment %d", e)
	}
}

// Region ...
type Region string

// Regions
const (
	DefaulRegion Region = ""
	APSouthEast1 Region = "ap-southeast-1."
)

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
	SelectedRegion() Region
	SetRegion(region Region) OddsFeedConfiguration
	ExchangeName() string
	ReplayExchangeName() string
	ReportExtendedData() bool
	SetAPIURL(url string) OddsFeedConfiguration
	SetMQURL(url string) OddsFeedConfiguration
	SetMessagingPort(port int) OddsFeedConfiguration
	APIURL() (string, error)
	MQURL() (string, error)
}
