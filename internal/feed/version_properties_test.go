package feed

import (
	"testing"

	"github.com/oddin-gg/gosdk/internal/version"
)

// TestAMQPClientProperties_CarriesVersion pins the broker
// self-identification: the legacy "SDK" (language) key is preserved for
// backward compatibility and "SDK_version" is added so the version is
// visible per connection in the RabbitMQ management UI.
func TestAMQPClientProperties_CarriesVersion(t *testing.T) {
	props := amqpClientProperties()

	if got := props["SDK"]; got != version.Language {
		t.Errorf(`Properties["SDK"] = %v, want %q`, got, version.Language)
	}
	if got := props["SDK_version"]; got != version.Version() {
		t.Errorf(`Properties["SDK_version"] = %v, want %q`, got, version.Version())
	}
}
