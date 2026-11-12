package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"commandcode-or-or-cliproxyapi/internal/catalog"
	"commandcode-or-or-cliproxyapi/internal/config"
)

const (
	cooledRefreshKey  = "sk-refresh-cooled-key"
	goodRefreshKey    = "sk-refresh-good-key"
	refreshTwoKeyYAML = "api-keys:\n" +
		"  - value: " + cooledRefreshKey + "\n" +
		"  - value: " + goodRefreshKey + "\n" +
		"catalog-url: https://api.commandcode.ai/catalog/models\n"
)

// authRecorder answers host.http.do, records every Authorization header, and
// authRecorder exposes host-owned test credentials and records every upstream
// Authorization header. The first credential fails with 401; no upstream
// rotation should occur during a catalog refresh.
type authRecorder struct {
	mu    sync.Mutex
	auths []string
	logs  [][]byte
	// body, when non-empty, replaces testCatalogJSON as the served catalog.
	body []byte
	keys []string
}

func (r *authRecorder) call(method string, payload []byte) ([]byte, error) {
	keys := r.keys
	if len(keys) == 0 {
		keys = []string{cooledRefreshKey, goodRefreshKey}
	}
	if method == pluginabi.MethodHostAuthList {
		files := make([]pluginapi.HostAuthFileEntry, len(keys))
		for i := range keys {
			name := fmt.Sprintf("%d.json", i)
			files[i] = pluginapi.HostAuthFileEntry{ID: name, Name: name, AuthIndex: name, Provider: ProviderID, Source: "file"}
		}
		return hostOK(hostAuthListResponse{Files: files}), nil
	}
	if method == pluginabi.MethodHostAuthGet {
		var req pluginapi.HostAuthGetRequest
		_ = json.Unmarshal(payload, &req)
		for i, key := range keys {
			if req.AuthIndex == fmt.Sprintf("%d.json", i) {
				raw, _ := json.Marshal(map[string]any{"type": ProviderID, "api_key": key})
				return hostOK(pluginapi.HostAuthGetResponse{JSON: raw}), nil
			}
		}
		return hostErr("not_found", "missing auth"), nil
	}
	if method == pluginabi.MethodHostLog {
		r.mu.Lock()
		r.logs = append(r.logs, append([]byte(nil), payload...))
		r.mu.Unlock()
		return hostOK(map[string]any{}), nil
	}
	if method != pluginabi.MethodHostHTTPDo {
		return hostOK(map[string]any{}), nil
	}
	var wire struct {
		Headers http.Header `json:"headers"`
	}
	if err := json.Unmarshal(payload, &wire); err != nil {
		return hostErr("test", "undecodable payload"), nil
	}
	auth := wire.Headers.Get("Authorization")
	r.mu.Lock()
	r.auths = append(r.auths, auth)
	r.mu.Unlock()
	if auth == "Bearer "+cooledRefreshKey {
		return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusUnauthorized}), nil
	}
	body := testCatalogJSON
	if len(r.body) > 0 {
		body = string(r.body)
	}
	return hostOK(pluginapi.HTTPResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       []byte(body),
	}), nil
}

func (r *authRecorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.auths...)
}

func (r *authRecorder) logCalls() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]byte(nil), r.logs...)
}

func twoKeyFixture(t *testing.T) (config.Config, *catalog.Manager, *HostBridge, *authRecorder) {
	t.Helper()
	cfg, err := config.Load([]byte(refreshTwoKeyYAML))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	rec := &authRecorder{}
	bridge := NewHostBridge(rec.call)
	return cfg, catalog.New(cfg, bridge), bridge, rec
}

// Catalog discovery uses the first readable, enabled host credential and
// does not rotate on an upstream 401.
func TestRefreshUsesFirstEnabledAuthFile(t *testing.T) {
	_, mgr, bridge, rec := twoKeyFixture(t)
	if err := refreshOnce(context.Background(), mgr, bridge, time.Second); err == nil {
		t.Fatal("expected first enabled auth file key failure")
	}
	auths := rec.seen()
	if len(auths) != 1 || auths[0] != "Bearer "+cooledRefreshKey {
		t.Fatalf("refresh auths = %v; want one first-key attempt", auths)
	}
	if len(mgr.Models()) != 0 {
		t.Fatalf("failed refresh changed catalog: %+v", mgr.Models())
	}
}

// TestRefreshUnsupportedLogQuotesNewlineIDs pins F2: a catalog entry excluded
// from the routable set must reach the host.log "models" field quoted (%q), so
// no raw newline carried by an upstream ID can forge log lines (CWE-117).
// The exclusion is produced by the protocol kill-switch — the default route is
// chat-completions here, so turning that switch off demotes the entry while
// leaving its ID in the diagnostic.
func TestRefreshUnsupportedLogQuotesNewlineIDs(t *testing.T) {
	const evilID = "evil\n2027-01-01 ERROR forged host.log line"
	cfg, err := config.Load([]byte("api-keys:\n  - value: sk-refresh-evil-key\n" +
		"catalog-url: https://api.commandcode.ai/catalog/models\n" +
		"protocols:\n  chat-completions: false\n"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	rec := &authRecorder{keys: []string{"sk-refresh-evil-key"}, body: []byte(`{"data":[{"id":"evil\n2027-01-01 ERROR forged host.log line"}]}`)}
	bridge := NewHostBridge(rec.call)
	mgr := catalog.New(cfg, bridge)
	if err := refreshOnce(context.Background(), mgr, bridge, time.Second); err != nil {
		t.Fatalf("refreshOnce: %v", err)
	}
	logCalls := rec.logCalls()
	if len(logCalls) != 1 {
		t.Fatalf("host.log calls = %d, want exactly one unsupported-models warn", len(logCalls))
	}
	var req struct {
		Level   string         `json:"level"`
		Message string         `json:"message"`
		Fields  map[string]any `json:"fields"`
	}
	if err := json.Unmarshal(logCalls[0], &req); err != nil {
		t.Fatalf("decode host.log payload: %v", err)
	}
	if req.Message != "unsupported models excluded from routable catalog" {
		t.Fatalf("unexpected log message: %q", req.Message)
	}
	modelsField, _ := req.Fields["models"].(string)
	if strings.ContainsRune(modelsField, '\n') {
		t.Fatalf("models log field contains raw newline (log injection): %q", modelsField)
	}
	if want := fmt.Sprintf("%q: ", evilID); !strings.Contains(modelsField, want) {
		t.Fatalf("models field must quote the upstream ID; got %q, want prefix %q", modelsField, want)
	}
}
