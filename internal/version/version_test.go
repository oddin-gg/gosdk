package version

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

// releasableVersion returns nil iff v is a version this module can
// actually be released at and consumers can `go get`. Two rules:
//   - module.Check: valid semver AND the major-version-vs-module-path
//     suffix rule (so "v2.0.0" on the bare, non-/v2 path is rejected).
//   - no build metadata: the "+incompatible" suffix is Go's escape for
//     LEGACY modules that have no go.mod. module.Check accepts it, but
//     THIS repo has a go.mod — a "+incompatible" tag would produce a
//     misleading, unusable release. Any build metadata is rejected.
func releasableVersion(v string) error {
	if err := module.Check(modulePath, v); err != nil {
		return err
	}
	if b := semver.Build(v); b != "" {
		return fmt.Errorf("%s carries build metadata %q (e.g. +incompatible); invalid for a module with a go.mod", v, b)
	}
	return nil
}

// TestSDK_IsValidModuleVersion validates the release baseline with the
// canonical Go module checker (+ build-metadata rejection) rather than a
// hand-rolled regex. The release CI guard runs exactly this test on a
// tag build, so a bad SDK constant fails the release, not the tag.
func TestSDK_IsValidModuleVersion(t *testing.T) {
	if err := releasableVersion("v" + SDK); err != nil {
		t.Fatalf("SDK %q is not a releasable module version for %q: %v", SDK, modulePath, err)
	}
	if Language == "" {
		t.Fatal("Language constant is empty")
	}
}

// TestReleasableVersion_RejectsInvalid demonstrates the guard rejects
// exactly the versions a loose regex would have let through — a v2 major
// on the bare (non-/v2) path, malformed semver, and the "+incompatible"
// legacy escape.
func TestReleasableVersion_RejectsInvalid(t *testing.T) {
	bad := []string{
		"v01.0.0",             // leading zero
		"v1.0.0-01",           // numeric prerelease with leading zero
		"v1.0.0-alpha..1",     // empty prerelease identifier
		"v2.0.0",              // major 2 requires a /v2 module path suffix
		"v2.0.0+incompatible", // legacy escape — invalid for a repo with go.mod
		"v1.0.0+incompatible", // build metadata not allowed on a release tag
	}
	for _, v := range bad {
		if err := releasableVersion(v); err == nil {
			t.Errorf("releasableVersion(%q) = nil, want rejection", v)
		}
	}
}

// TestReportedVersion pins the runtime-first resolution: a real build-info
// module version is reported (leading "v" trimmed); an absent or
// "(devel)" version falls back to the compiled-in SDK constant marked
// "-dev", so telemetry can tell a stamped release from an unstamped build.
func TestReportedVersion(t *testing.T) {
	cases := []struct {
		name      string
		moduleVer string
		want      string
	}{
		{"tagged release", "v1.2.3", "1.2.3"},
		{"tagged without v", "1.2.3", "1.2.3"},
		{"pseudo-version", "v1.2.3-0.20260101000000-abcdef123456", "1.2.3-0.20260101000000-abcdef123456"},
		{"devel main module", "(devel)", SDK + devSuffix},
		{"no build info", "", SDK + devSuffix},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reportedVersion(tc.moduleVer); got != tc.want {
				t.Fatalf("reportedVersion(%q) = %q, want %q", tc.moduleVer, got, tc.want)
			}
		})
	}
}

// TestVersion_NonEmpty confirms the resolved value is always populated.
func TestVersion_NonEmpty(t *testing.T) {
	if Version() == "" {
		t.Fatal("Version() is empty")
	}
}

// TestUserAgent_Format pins the User-Agent shape the server parses:
// "oddin-gosdk/<version> (<go toolchain>)".
func TestUserAgent_Format(t *testing.T) {
	ua := UserAgent()
	if want := "oddin-gosdk/" + Version(); !strings.HasPrefix(ua, want) {
		t.Fatalf("UserAgent = %q, want prefix %q", ua, want)
	}
	if !strings.Contains(ua, runtime.Version()) {
		t.Fatalf("UserAgent = %q, want it to carry the Go toolchain %q", ua, runtime.Version())
	}
}
