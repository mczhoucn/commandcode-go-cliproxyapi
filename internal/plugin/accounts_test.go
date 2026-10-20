package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Exercise the same management route used by the page, with the host owning
// the current account list. No configured key may bring a deleted account back.
func TestAccountListFollowsAuthFiles(t *testing.T) {
	files := []pluginapi.HostAuthFileEntry{
		{AuthIndex: "a", ID: "custom-a", Name: "personal.json", Provider: ProviderID, Source: "file", Email: "a@example.test"},
		{AuthIndex: "b", ID: "custom-b", Name: "work.json", Provider: ProviderID, Source: "file", Disabled: true, Status: "disabled"},
		{AuthIndex: "c", ID: "custom-c", Name: "backup.json", Provider: ProviderID, Source: "file"},
		{AuthIndex: "other", Name: "other.json", Provider: "other", Source: "file"},
		{AuthIndex: "memory", Name: "removed.json", Provider: ProviderID, Source: "memory"},
	}
	f := &fakeCaller{responder: func(method string, _ []byte) ([]byte, error) {
		if method == pluginabi.MethodHostAuthList {
			return hostOK(hostAuthListResponse{Files: files}), nil
		}
		return hostOK(map[string]any{}), nil
	}}
	m := NewManager(NewHostBridge(f.call))
	resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/" + pluginName + "/quota-usage", Body: []byte("{}")})
	if err != nil {
		t.Fatal(err)
	}
	// Decode with the response wire names.
	var wire struct {
		Cards []struct {
			KeyID    string `json:"key_id"`
			Label    string `json:"label"`
			FileName string `json:"file_name"`
			Disabled bool   `json:"disabled"`
		} `json:"cards"`
	}
	if err := json.Unmarshal(resp.Body, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Cards) != 3 {
		t.Fatalf("cards = %s; want only 3 current CommandCode files", resp.Body)
	}
	if wire.Cards[0].KeyID != "a" || wire.Cards[0].Label != "a@example.test" || wire.Cards[0].FileName != "personal.json" || !wire.Cards[1].Disabled {
		t.Fatalf("account identity/status missing: %s", resp.Body)
	}
	files = files[1:]
	resp, err = m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/" + pluginName + "/quota-usage", Body: []byte("{}")})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(resp.Body, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Cards) != 2 {
		t.Fatalf("deleted file still shown: %s", resp.Body)
	}
	if len(f.callsOf(pluginabi.MethodHostHTTPDo)) != 0 {
		t.Fatal("listing accounts called upstream")
	}
	resp, err = m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/" + pluginName + "/quota-usage", Body: []byte(`{"key_id":"a"}`)})
	if err != nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("deleted account can still refresh: %d %v", resp.StatusCode, err)
	}
}

func TestAccountAddSavesOnlyAuthFile(t *testing.T) {
	m, f := oauthTestManager()
	resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodPost, Path: "/v0/management/plugins/" + pluginName + "/accounts", Body: []byte(`{"api_key":"  new-account-secret  "}`),
	})
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("add = %d %s, err=%v", resp.StatusCode, resp.Body, err)
	}
	saves := f.callsOf(pluginabi.MethodHostAuthSave)
	if len(saves) != 1 {
		t.Fatalf("auth saves = %d", len(saves))
	}
	var save pluginapi.HostAuthSaveRequest
	if err := json.Unmarshal(saves[0].payload, &save); err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(save.JSON, &record); err != nil {
		t.Fatal(err)
	}
	if record["type"] != ProviderID || record["api_key"] != "new-account-secret" {
		t.Fatal("wrong auth content")
	}
	if id, ok := record["id"].(string); ok && id != save.Name {
		t.Fatal("saved runtime/file identities would differ")
	}
	if strings.Contains(string(resp.Body), "new-account-secret") {
		t.Fatal("add response leaked key")
	}
	if len(f.callsOf(pluginabi.MethodHostHTTPDo)) != 2 || record["credential_source"] != "api_key" || record["plan_id"] != "individual-goat" {
		t.Fatal("account was not verified before save")
	}
}

func TestAuthParsePreservesDisabledAndMetadata(t *testing.T) {
	resp, err := (authProvider{}).ParseAuth(context.Background(), pluginapi.AuthParseRequest{
		FileName: "disabled.json", RawJSON: []byte(`{"type":"commandcode","api_key":"disabled-key","disabled":true,"note":"keep","priority":7}`),
	})
	if err != nil || !resp.Auth.Disabled || resp.Auth.Metadata["note"] != "keep" || resp.Auth.Metadata["priority"] != float64(7) {
		t.Fatalf("lost host-managed auth state: disabled=%v metadata=%v err=%v", resp.Auth.Disabled, resp.Auth.Metadata, err)
	}
}

func TestAccountAddNeverOverwritesOtherProviderFile(t *testing.T) {
	name := authFileNameFromHash(authKeyHash("new-key"))
	f := &fakeCaller{responder: func(method string, _ []byte) ([]byte, error) {
		if method == pluginabi.MethodHostAuthList {
			return hostOK(hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{{Name: name, Provider: "other", Source: "file"}}}), nil
		}
		return hostOK(map[string]any{}), nil
	}}
	m := NewManager(NewHostBridge(f.call))
	resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/" + pluginName + "/accounts", Body: []byte(`{"api_key":"new-key"}`)})
	if err != nil || resp.StatusCode != http.StatusConflict || len(f.callsOf(pluginabi.MethodHostAuthSave)) != 0 {
		t.Fatal("foreign auth file would be overwritten")
	}
}

func TestEmailSyncPreservesAuthStateAndDoesNotRecreateDeletedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "custom.json")
	raw := []byte(`{"type":"commandcode","api_key":"email-key","id":"legacy-id","disabled":true,"note":"keep","priority":7}`)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	m := NewManager(nil)
	entry := pluginapi.HostAuthFileEntry{Name: "custom.json", Path: path}
	if err := m.syncAccountEmail(context.Background(), entry, "email-key", "dev@example.test"); err != nil {
		t.Fatal(err)
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(updated, &got); err != nil {
		t.Fatal(err)
	}
	if got["api_key"] != "email-key" || got["disabled"] != true || got["note"] != "keep" || got["priority"] != float64(7) || got["id"] != "legacy-id" || got["email"] != "dev@example.test" {
		t.Fatal("email sync changed credential identity or metadata")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := m.syncAccountEmail(context.Background(), entry, "email-key", "another@example.test"); err == nil {
		t.Fatal("deleted credential email was saved")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("quota refresh recreated a deleted credential")
	}
}

func TestAccountAddRejectsDuplicateAndPreservesMetadata(t *testing.T) {
	const key = "already-saved-key"
	f := &fakeCaller{}
	// A user-uploaded filename must be detected too, not just our hash names.
	f.authFiles = map[string]json.RawMessage{"custom.json": json.RawMessage(`{"type":"commandcode","api_key":"already-saved-key","disabled":true,"note":"keep me"}`)}
	m := NewManager(NewHostBridge(f.call))
	resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/" + pluginName + "/accounts", Body: []byte(`{"api_key":"` + key + `"}`)})
	if err != nil || resp.StatusCode != http.StatusConflict || len(f.callsOf(pluginabi.MethodHostAuthSave)) != 0 {
		t.Fatalf("duplicate save: status=%d err=%v", resp.StatusCode, err)
	}
	if !strings.Contains(string(f.authFiles["custom.json"]), "keep me") {
		t.Fatal("duplicate overwrote metadata")
	}
}

func TestAccountAddErrorsNeverSaveOrLeakSecrets(t *testing.T) {
	for name, body := range map[string]string{
		"empty":          `{"api_key":""}`,
		"spaces":         `{"api_key":"  "}`,
		"header newline": `{"api_key":"injected\r\nsecret"}`,
		"control":        string(mustJSON(map[string]string{"api_key": "credential" + string(rune(0)) + "secret"})),
		"delete control": string(mustJSON(map[string]string{"api_key": "credential" + string(rune(127)) + "secret"})),
		"malformed":      `{"api_key":"malformed-secret"`,
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeCaller{}
			m := NewManager(NewHostBridge(f.call))
			resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/" + pluginName + "/accounts", Body: []byte(body)})
			if err != nil || resp.StatusCode != http.StatusBadRequest || len(f.callsOf(pluginabi.MethodHostAuthSave)) != 0 || strings.Contains(string(resp.Body), "secret") {
				t.Fatalf("invalid input: status=%d body=%s err=%v", resp.StatusCode, resp.Body, err)
			}
		})
	}
	for _, method := range []string{pluginabi.MethodHostAuthList, pluginabi.MethodHostAuthGet, pluginabi.MethodHostAuthSave} {
		t.Run(method, func(t *testing.T) {
			_, valid := oauthTestManager()
			f := &fakeCaller{responder: func(actual string, payload []byte) ([]byte, error) {
				if actual == method {
					return hostErr("denied", "credential-secret echoed by host"), nil
				}
				return valid.responder(actual, payload)
			}}
			f.setAuthFiles("existing-unrelated-key")
			m := NewManager(NewHostBridge(f.call))
			resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/" + pluginName + "/accounts", Body: []byte(`{"api_key":"new-safe-test-key"}`)})
			if err != nil || resp.StatusCode != http.StatusBadGateway || strings.Contains(string(resp.Body), "credential-secret") {
				t.Fatalf("host failure: status=%d body=%s err=%v", resp.StatusCode, resp.Body, err)
			}
			if len(f.callsOf(method)) == 0 {
				t.Fatal("test did not reach failing host callback")
			}
			if method != pluginabi.MethodHostAuthSave && len(f.callsOf(pluginabi.MethodHostAuthSave)) != 0 {
				t.Fatal("failed duplicate check still saved")
			}
		})
	}
}

func TestAccountAddConcurrentDuplicateWritesOnce(t *testing.T) {
	m, f := oauthTestManager()
	var wg sync.WaitGroup
	results := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, _ := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/" + pluginName + "/accounts", Body: []byte(`{"api_key":"concurrent-key"}`)})
			results <- resp.StatusCode
		}()
	}
	wg.Wait()
	close(results)
	statuses := map[int]int{}
	for status := range results {
		statuses[status]++
	}
	if statuses[http.StatusCreated] != 1 || statuses[http.StatusConflict] != 1 || len(f.callsOf(pluginabi.MethodHostAuthSave)) != 1 {
		t.Fatalf("concurrent additions: statuses=%v saves=%d", statuses, len(f.callsOf(pluginabi.MethodHostAuthSave)))
	}
}

func TestFirstAccountDiscoversModelsWithoutRestart(t *testing.T) {
	f := &fakeCaller{responder: catalogResponder(true, testCatalogJSON)}
	f.setAuthFiles()
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	decodeEnv(t, mustHandle(t, m, pluginabi.MethodPluginRegister, lifecycleRequestBody("")))
	if len(f.callsOf(pluginabi.MethodHostHTTPDo)) != 0 {
		t.Fatal("keyless startup called upstream")
	}
	body, _ := json.Marshal(pluginapi.AuthModelRequest{
		AuthProvider: ProviderID, Metadata: map[string]any{"api_key": "first-account-key"},
	})
	var models pluginapi.ModelResponse
	decodeResult(t, mustHandle(t, m, pluginabi.MethodModelForAuth, body), &models)
	if len(models.Models) != 1 {
		t.Fatalf("first account has no models: %+v", models)
	}
	calls := f.callsOf(pluginabi.MethodHostHTTPDo)
	if len(calls) != 1 || !strings.Contains(string(calls[0].payload), "Bearer first-account-key") {
		t.Fatal("discovery did not use the newly selected credential")
	}
}

func TestSelectedSavedMetadataKeyExecutesImmediately(t *testing.T) {
	m, f := newExecManager(t)
	body, _ := json.Marshal(executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		AuthProvider: ProviderID, AuthMetadata: map[string]any{"api_key": "just-saved-key"},
		Model: "commandcode/glm-5.3", SourceFormat: "openai", OriginalRequest: []byte(ccRequestBody),
	}})
	env := decodeEnv(t, mustHandle(t, m, pluginabi.MethodExecutorExecute, body))
	if !env.OK {
		t.Fatalf("newly saved key rejected: %+v", env.Error)
	}
	wire := lastWire(t, f, pluginabi.MethodHostHTTPDo)
	if wire["headers"].(map[string]any)["Authorization"].([]any)[0] != "Bearer just-saved-key" {
		t.Fatal("execution used another account instead of the selected saved key")
	}
}

func TestCatalogDiscoverySkipsDisabledAuthFiles(t *testing.T) {
	f := &fakeCaller{responder: catalogResponder(true, testCatalogJSON)}
	f.authFiles = map[string]json.RawMessage{
		"0-disabled.json": json.RawMessage(`{"type":"commandcode","api_key":"disabled-secret","disabled":true}`),
		"1-enabled.json":  json.RawMessage(`{"type":"commandcode","api_key":"enabled-secret"}`),
	}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	mustHandle(t, m, pluginabi.MethodPluginRegister, lifecycleRequestBody(testValidYAML))
	calls := f.callsOf(pluginabi.MethodHostHTTPDo)
	if len(calls) != 1 || !strings.Contains(string(calls[0].payload), "Bearer enabled-secret") {
		t.Fatal("catalog did not use enabled auth file")
	}
}
