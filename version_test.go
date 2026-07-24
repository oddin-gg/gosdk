package gosdk

import (
	"testing"

	"github.com/oddin-gg/gosdk/internal/version"
)

// TestVersion_MatchesInternalSource pins the public Version() accessor to
// the single internal source of truth, so consumers reading
// gosdk.Version() see exactly what the SDK reports to the API and broker.
func TestVersion_MatchesInternalSource(t *testing.T) {
	if Version() != version.Version() {
		t.Fatalf("gosdk.Version() = %q, want %q (internal/version.Version())", Version(), version.Version())
	}
	if Version() == "" {
		t.Fatal("gosdk.Version() is empty")
	}
}
