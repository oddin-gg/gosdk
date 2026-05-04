package types

import (
	"strings"
	"testing"
)

func TestEnvironment_APIEndpoint(t *testing.T) {
	tests := []struct {
		name    string
		env     Environment
		region  Region
		want    string
		wantErr bool
	}{
		{name: "integration default", env: IntegrationEnvironment, region: RegionDefault, want: "api-mq.integration.oddin.gg"},
		{name: "integration ap", env: IntegrationEnvironment, region: APSouthEast1, want: "api-mq.integration.ap-southeast-1.oddin.gg"},
		{name: "production default", env: ProductionEnvironment, region: RegionDefault, want: "api-mq.oddin.gg"},
		{name: "production ap", env: ProductionEnvironment, region: APSouthEast1, want: "api-mq.ap-southeast-1.oddin.gg"},
		{name: "test default", env: TestEnvironment, region: RegionDefault, want: "api-mq-test.integration.oddin.dev"},
		{name: "unknown", env: UnknownEnvironment, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.env.APIEndpoint(tt.region)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEnvironment_MQEndpoint(t *testing.T) {
	tests := []struct {
		name    string
		env     Environment
		region  Region
		want    string
		wantErr bool
	}{
		{name: "integration default", env: IntegrationEnvironment, region: RegionDefault, want: "mq.integration.oddin.gg"},
		{name: "production default", env: ProductionEnvironment, region: RegionDefault, want: "mq.oddin.gg"},
		{name: "test default", env: TestEnvironment, region: RegionDefault, want: "mq-test.integration.oddin.dev"},
		{name: "unknown", env: UnknownEnvironment, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.env.MQEndpoint(tt.region)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAllLocales_Coverage(t *testing.T) {
	want := []Locale{
		EnLocale, BrLocale, DeLocale, EsLocale, FiLocale, FrLocale,
		PlLocale, PtLocale, RuLocale, ThLocale, ViLocale, ZhLocale,
	}
	if len(AllLocales) != len(want) {
		t.Fatalf("AllLocales length: got %d, want %d", len(AllLocales), len(want))
	}
	seen := make(map[Locale]bool, len(AllLocales))
	for _, l := range AllLocales {
		if seen[l] {
			t.Fatalf("duplicate locale in AllLocales: %q", l)
		}
		seen[l] = true
		if !strings.ContainsRune("aoeuibfnp", rune(l[0])) && len(l) != 2 {
			t.Fatalf("unexpected locale shape %q (expected 2-char ISO code)", l)
		}
	}
	for _, w := range want {
		if !seen[w] {
			t.Fatalf("missing %q from AllLocales", w)
		}
	}
}
