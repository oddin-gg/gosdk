// Package version is the single source of truth for the SDK version.
// It is shared by the API client (User-Agent / X-Oddin-SDK-Version
// headers on every request) and the feed client (AMQP connection
// Properties), so version reporting is consistent across every
// self-identification channel. It has no dependencies beyond the
// stdlib, so any layer can import it without an import cycle.
//
// Version resolution is runtime-first: the actual module version the
// consumer resolved (via `go get`) is read from the build info, so a
// consumer on v1.0.3 reports 1.0.3 without anyone bumping a constant.
// The compiled-in SDK constant is only the fallback for unstamped
// builds (local `go run`, a `replace` directive, `go test`); when the
// fallback is used the reported value is suffixed "-dev" so it is
// unmistakably NOT a real release in telemetry. A CI guard keeps the
// SDK constant in lockstep with the release tag (see .github/workflows).
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

// SDK is the release baseline version. It MUST stay in lockstep with the
// git release tag — the CI tag guard fails a `v*` tag build unless the
// tag equals "v"+SDK — and MUST be valid semver. It is the fallback
// reported (suffixed "-dev") when build info carries no real module
// version.
const SDK = "1.2.0"

// devSuffix marks a version derived from the compiled-in fallback rather
// than resolved from build info — i.e. an unstamped / dev build.
const devSuffix = "-dev"

// Language identifies this SDK's implementation language in cross-SDK
// telemetry. It is the value reported in the AMQP "SDK" connection
// property (unchanged from before).
const Language = "go"

// modulePath is this module's import path, matched against build info to
// find the SDK's own resolved version.
const modulePath = "github.com/oddin-gg/gosdk"

// resolved + userAgent are computed once at init — the module version
// and Go toolchain are fixed for the life of the process.
var (
	resolved  = reportedVersion(moduleVersion())
	userAgent = fmt.Sprintf("oddin-gosdk/%s (%s)", resolved, runtime.Version())
)

// Version returns the SDK version reported to servers: the module
// version resolved from build info when available (e.g. "1.0.0"), else
// the compiled-in fallback marked "-dev" (e.g. "1.0.0-dev").
func Version() string {
	return resolved
}

// UserAgent is the HTTP User-Agent the SDK sends on every API request,
// e.g. "oddin-gosdk/1.0.0 (go1.26.5)". The Go toolchain suffix is
// included so support can see what built a given agent.
func UserAgent() string {
	return userAgent
}

// reportedVersion maps a raw build-info module version to the reported
// value. A real version is returned with its leading "v" trimmed; an
// absent or "(devel)" version falls back to the compiled-in SDK
// constant marked "-dev". Pure (no build-info read) so it is
// deterministically unit-testable.
func reportedVersion(moduleVer string) string {
	if moduleVer != "" && moduleVer != "(devel)" {
		return strings.TrimPrefix(moduleVer, "v")
	}
	return SDK + devSuffix
}

// moduleVersion returns this module's version as recorded in the binary's
// build info, or "" when unavailable. It handles the SDK being either
// the main module (building/testing this repo) or a dependency (the
// normal consumer case). A replaced module is treated as unstamped.
func moduleVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	if bi.Main.Path == modulePath {
		return bi.Main.Version
	}
	for _, dep := range bi.Deps {
		if dep.Path != modulePath {
			continue
		}
		if dep.Replace != nil {
			return "" // locally replaced → treat as dev
		}
		return dep.Version
	}
	return ""
}
