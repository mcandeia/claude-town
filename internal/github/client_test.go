package github

import (
	"testing"
)

func TestNewClientConfig(t *testing.T) {
	tests := []struct {
		name    string
		selfDNS string
		want    string
	}{
		{
			name:    "simple domain",
			selfDNS: "claude-town.example.com",
			want:    "https://claude-town.example.com/webhooks/github",
		},
		{
			name:    "domain with subdomain",
			selfDNS: "hooks.internal.mycompany.io",
			want:    "https://hooks.internal.mycompany.io/webhooks/github",
		},
		{
			name:    "domain with port",
			selfDNS: "localhost:8443",
			want:    "https://localhost:8443/webhooks/github",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{
				SelfDNS: tt.selfDNS,
			}

			got := cfg.WebhookURL()
			if got != tt.want {
				t.Errorf("Config.WebhookURL() = %q, want %q", got, tt.want)
			}
		})
	}
}
