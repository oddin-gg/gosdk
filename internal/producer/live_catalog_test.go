package producer

import (
	"testing"

	"github.com/oddin-gg/gosdk/internal/api/xml"
	"github.com/oddin-gg/gosdk/types"
)

// TestProducerHandle_SeesRefreshedCatalog pins the M6 fix: a handle
// built BEFORE a catalog refresh must observe the refreshed catalog
// fields through the shared *data — Manager.Open deliberately reuses the
// same *data (refreshCatalog in place) precisely so retained handles
// stay live. Pre-fix only the runtime accessors read through the
// pointer; Name/Description/IsAvailable/APIEndpoint/ProducerScopes/
// StatefulRecoveryWindowInMinutes returned construction-time snapshots.
func TestProducerHandle_SeesRefreshedCatalog(t *testing.T) {
	d := newData(xml.Producer{
		ID:             3,
		Name:           "old-name",
		Description:    "old-desc",
		Active:         true,
		APIEndpoint:    "old.example.com",
		Scope:          xml.ScopeLive,
		RecoveryWindow: 60,
	})
	handle, err := buildProducerImpl(d)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// Catalog refresh (what Manager.Open does on Connect).
	d.refreshCatalog(xml.Producer{
		ID:             3,
		Name:           "new-name",
		Description:    "new-desc",
		Active:         false,
		APIEndpoint:    "new.example.com",
		Scope:          xml.Scope("live|prematch"),
		RecoveryWindow: 90,
	})

	if got := handle.Name(); got != "new-name" {
		t.Errorf("Name() = %q, want refreshed %q", got, "new-name")
	}
	if got := handle.Description(); got != "new-desc" {
		t.Errorf("Description() = %q, want refreshed %q", got, "new-desc")
	}
	if handle.IsAvailable() {
		t.Error("IsAvailable() = true, want refreshed false")
	}
	if got := handle.APIEndpoint(); got != "new.example.com" {
		t.Errorf("APIEndpoint() = %q, want refreshed %q", got, "new.example.com")
	}
	if got := handle.StatefulRecoveryWindowInMinutes(); got != 90 {
		t.Errorf("StatefulRecoveryWindowInMinutes() = %d, want refreshed 90", got)
	}
	scopes := handle.ProducerScopes()
	if len(scopes) != 2 {
		t.Fatalf("ProducerScopes() = %v, want refreshed [live prematch]", scopes)
	}
	if scopes[0] != types.LiveProducerScope || scopes[1] != types.PrematchProducerScope {
		t.Errorf("ProducerScopes() = %v, want [live prematch]", scopes)
	}

	// A refresh carrying an unparseable scope must not break the
	// accessor — it falls back to the construction-time (validated) set.
	d.refreshCatalog(xml.Producer{ID: 3, Name: "new-name", Scope: xml.Scope("bogus")})
	if got := handle.ProducerScopes(); len(got) != 1 || got[0] != types.LiveProducerScope {
		t.Errorf("ProducerScopes() after bogus refresh = %v, want construction-time [live]", got)
	}
}
