package types

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

// AllLocales returns every Locale constant exposed by the SDK — useful
// for callers that want to preload everything via WithPreloadLocales(...).
// Each call returns a FRESH slice the caller owns and may freely mutate.
// (Previously an exported package-level var: any consumer could reorder
// or overwrite the shared backing array, corrupting the list for every
// other reader — and racing concurrent ones.)
func AllLocales() []Locale {
	return []Locale{
		EnLocale, BrLocale, DeLocale, EsLocale, FiLocale, FrLocale,
		PlLocale, PtLocale, RuLocale, ThLocale, ViLocale, ZhLocale,
	}
}
