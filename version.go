package gosdk

import "github.com/oddin-gg/gosdk/internal/version"

// Version returns the running SDK version — the module version resolved
// from build info when available (e.g. "1.0.0"), or the compiled-in
// fallback marked "-dev" for unstamped/local builds (e.g. "1.0.0-dev").
// It is the same value the SDK reports to the API (the User-Agent and
// X-Oddin-SDK-Version headers) and to the broker (the AMQP connection
// properties), exposed so consumers can log the version they are
// running.
func Version() string {
	return version.Version()
}
