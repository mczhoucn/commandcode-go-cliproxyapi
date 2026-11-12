package config

import (
	"strings"
	"testing"
)

// TestLoadRejectsCrossHostCatalogURL pins Finding 14: catalog-url host must
// match base-url host (receives the bearer key).
func TestLoadRejectsCrossHostCatalogURL(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{
			"cross-host with same scheme",
			"base-url: https://api.commandcode.ai/provider/v1\ncatalog-url: https://mirror.example.com/models\n",
		},
		{
			"cross-host different schemes",
			"allow-http: true\nbase-url: http://localhost:8080/v1\ncatalog-url: https://production.example.com/models\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load([]byte(tc.yaml))
			if err == nil {
				t.Fatal("expected validation error for cross-host catalog-url")
			}
			if !strings.Contains(err.Error(), "catalog-url") || !strings.Contains(err.Error(), "host") {
				t.Errorf("error = %q, want catalog-url host validation message", err)
			}
		})
	}
}

// TestLoadRejectsOverrideEndpointQueryFragmentDotDot pins Finding 14: override
// endpoints with ?, #, or .. are rejected.
func TestLoadRejectsOverrideEndpointQueryFragmentDotDot(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		wantErr  string
	}{
		{"query string", "/v1/chat?debug=1", "must not contain"},
		{"fragment", "/v1/responses#section", "must not contain"},
		{"path traversal ..", "/v1/../admin", "must not contain"},
		{".. in segment", "/v1/sub../x", "must not contain"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			yaml := "route-overrides:\n  test-model:\n    protocol: chat-completions\n    endpoint: " + tc.endpoint + "\n"
			_, err := Load([]byte(yaml))
			if err == nil {
				t.Fatalf("expected validation error for endpoint %q", tc.endpoint)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want substring %q", err, tc.wantErr)
			}
		})
	}
}

// TestLoadRejectsAllProtocolsDisabled pins Finding 14: error when all three
// protocols are disabled.
func TestLoadRejectsAllProtocolsDisabled(t *testing.T) {
	yaml := `
protocols:
  chat-completions: false
  messages: false
  responses: false
`
	_, err := Load([]byte(yaml))
	if err == nil {
		t.Fatal("expected error when all protocols disabled")
	}
	if !strings.Contains(err.Error(), "protocol") || !strings.Contains(err.Error(), "enabled") {
		t.Errorf("error = %q, want all-protocols-disabled message", err)
	}
}

// TestLoadAllowsSameHostCatalogURL pins the positive case: same-host catalog-url
// with different path is allowed.
func TestLoadAllowsSameHostCatalogURL(t *testing.T) {
	yaml := `
base-url: https://api.commandcode.ai/provider/v1
catalog-url: https://api.commandcode.ai/api/models
`
	c, err := Load([]byte(yaml))
	if err != nil {
		t.Fatalf("same-host catalog-url must be allowed: %v", err)
	}
	if c.CatalogURL != "https://api.commandcode.ai/api/models" {
		t.Errorf("CatalogURL = %q", c.CatalogURL)
	}
}
