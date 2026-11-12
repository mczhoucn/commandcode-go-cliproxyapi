package catalog

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"commandcode-or-or-cliproxyapi/internal/config"
)

// TestRouteOverrideAcceptsPrefixedPublicID pins Finding 12: route-overrides
// accept keys written as either bare upstream ID or prefixed public ID.
func TestRouteOverrideAcceptsPrefixedPublicID(t *testing.T) {
	body := `{"data":[{"id":"glm-5.2"},{"id":"gpt-5.6-luna"}]}`
	cfg := testCfg()
	cfg.RouteOverrides = map[string]config.RouteOverride{
		// Both bare and prefixed keys should work.
		"glm-5.2":                    {Protocol: "messages", Endpoint: "/v1/messages"},
		"commandcode/gpt-5.6-luna": {Protocol: "responses", Endpoint: "/v1/responses"},
	}
	fc := &fakeClient{resp: pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(body)}}
	m := newManager(cfg, fc)
	mustRefresh(t, m)

	glm := findModel(t, m.Models(), "glm-5.2")
	if glm.Protocol != RouteMessages {
		t.Errorf("glm-5.2 Protocol = %q, want messages (bare key)", glm.Protocol)
	}
	luna := findModel(t, m.Models(), "gpt-5.6-luna")
	if luna.Protocol != RouteResponses {
		t.Errorf("gpt-5.6-luna Protocol = %q, want responses (prefixed key)", luna.Protocol)
	}
}

// TestStaleOverrideKeyWarning pins Finding 12: config validation warns about
// override keys that never match any catalog model.
func TestStaleOverrideKeyWarning(t *testing.T) {
	body := `{"data":[{"id":"glm-5.2"}]}`
	cfg := testCfg()
	cfg.RouteOverrides = map[string]config.RouteOverride{
		"glm-5.2":        {Protocol: "chat-completions"},
		"nonexistent-1":  {Protocol: "messages"},
		"commandcode/nonexistent-2": {Protocol: "responses"},
	}
	fc := &fakeClient{resp: pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(body)}}
	m := newManager(cfg, fc)
	mustRefresh(t, m)

	warns := m.Warnings()
	foundStale := 0
	for _, w := range warns {
		if strings.Contains(w, "nonexistent-1") || strings.Contains(w, "nonexistent-2") {
			foundStale++
		}
		if strings.Contains(w, "never matched") || strings.Contains(w, "stale") {
			// Good: the warning mentions the key is stale/unused.
		}
	}
	if foundStale < 2 {
		t.Errorf("warnings = %v, want stale-key warnings for nonexistent-1 and nonexistent-2", warns)
	}
}
