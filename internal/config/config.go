// Package config defines the internal config interface consumed by every
// internal/* manager. The public *gosdk.Config struct (in the top-level
// gosdk package) is adapted to satisfy this interface via configAdapter.
//
// Why an interface and not a direct *gosdk.Config dependency: the
// top-level gosdk package imports internal/* packages, so internal/*
// cannot import gosdk back without a cycle. The interface lives in a
// neutral internal/ package that both sides can import.
//
// Why this lives in internal/ rather than types/: this contract is only
// ever consumed by internal/* packages — kollector-esport / ots-odds-bridge
// construct their config via gosdk.NewConfig(...) + functional options
// and never implement the interface. Keeping it internal matches the
// "default to internal" principle in NEXT.md §3.6.
package config

import (
	"time"

	"github.com/oddin-gg/gosdk/types"
)

// Config is the interface every internal manager depends on for config
// access. All values are immutable from the manager's perspective —
// the legacy Set* methods on the pre-v2 interface are gone (they were
// no-ops on the adapter and unused by every internal caller).
type Config interface {
	AccessToken() *string
	DefaultLocale() types.Locale
	MaxInactivity() time.Duration
	MaxRecoveryExecution() time.Duration
	MessagingPort() int
	SdkNodeID() *int
	SelectedEnvironment() *types.Environment
	SelectedRegion() types.Region
	ExchangeName() string
	ReplayExchangeName() string
	ReportExtendedData() bool
	APIURL() (string, error)
	MQURL() (string, error)
	SportIDPrefix() string
}
