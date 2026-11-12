package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type authProvider struct{}

func (authProvider) Identifier() string { return ProviderID }

func (authProvider) ParseAuth(_ context.Context, req pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error) {
	debugTrace("auth parse request provider=%s file=%s raw_bytes=%d", req.Provider, req.FileName, len(req.RawJSON))
	var raw struct {
		Type     string `json:"type"`
		Provider string `json:"provider"`
		ID       string `json:"id"`
		Label    string `json:"label"`
		APIKey   string `json:"api_key"`
		Email    string `json:"email"`
		Disabled bool   `json:"disabled"`
	}
	if err := json.Unmarshal(req.RawJSON, &raw); err != nil {
		if req.Provider == ProviderID {
			return pluginapi.AuthParseResponse{}, fmt.Errorf("commandcode auth record has invalid JSON")
		}
		return pluginapi.AuthParseResponse{}, nil
	}
	if (req.Provider != "" && req.Provider != ProviderID) || (raw.Type != ProviderID && raw.Provider != ProviderID) {
		return pluginapi.AuthParseResponse{Handled: false}, nil
	}
	if strings.TrimSpace(raw.APIKey) == "" {
		return pluginapi.AuthParseResponse{}, fmt.Errorf("commandcode auth record has no api key")
	}
	if raw.ID == "" {
		raw.ID = req.FileName
	}
	debugTrace("auth parse handled provider=%s file=%s id=%s api_key_present=%t api_key_length=%d email_present=%t", req.Provider, req.FileName, raw.ID, strings.TrimSpace(raw.APIKey) != "", len(raw.APIKey), strings.TrimSpace(raw.Email) != "")
	var metadata map[string]any
	_ = json.Unmarshal(req.RawJSON, &metadata)
	auth := pluginapi.AuthData{
		Provider: ProviderID, ID: raw.ID, FileName: req.FileName, Label: raw.Label,
		Disabled: raw.Disabled, StorageJSON: req.RawJSON, Metadata: metadata,
		Attributes: map[string]string{"api_key": strings.TrimSpace(raw.APIKey)},
	}
	return pluginapi.AuthParseResponse{Handled: true, Auth: auth}, nil
}

func (authProvider) RefreshAuth(_ context.Context, req pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
	debugTrace("auth refresh request provider=%s id=%s storage_json_bytes=%d attr_names=%v metadata_names=%v", req.AuthProvider, req.AuthID, len(req.StorageJSON), mapKeys(req.Attributes), mapKeys(req.Metadata))
	return pluginapi.AuthRefreshResponse{Auth: pluginapi.AuthData{Provider: req.AuthProvider, ID: req.AuthID, StorageJSON: req.StorageJSON, Metadata: req.Metadata, Attributes: req.Attributes}}, nil
}

func mapKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

// The host owns the account set. Memory-only/removed records must not return
// as cards or catalog credentials, and provider names never come from the UI.
func commandcodeAuthFiles(ctx context.Context, bridge *HostBridge) ([]pluginapi.HostAuthFileEntry, error) {
	if bridge == nil {
		return nil, fmt.Errorf("host auth list unavailable")
	}
	entries, err := bridge.AuthList(ctx)
	if err != nil {
		return nil, fmt.Errorf("host auth list failed")
	}
	files := make([]pluginapi.HostAuthFileEntry, 0, len(entries))
	for _, entry := range entries {
		if isCommandcodeAuthFile(entry) {
			files = append(files, entry)
		}
	}
	return files, nil
}

func isCommandcodeAuthFile(entry pluginapi.HostAuthFileEntry) bool {
	provider := entry.Provider
	if provider == "" {
		provider = entry.Type
	}
	return provider == ProviderID && entry.Source == "file" && !entry.RuntimeOnly && entry.AuthIndex != ""
}

func authFileKey(ctx context.Context, bridge *HostBridge, entry pluginapi.HostAuthFileEntry) (string, error) {
	resp, err := bridge.AuthGet(ctx, entry.AuthIndex)
	if err != nil {
		return "", err
	}
	parsed, err := (authProvider{}).ParseAuth(ctx, pluginapi.AuthParseRequest{Provider: ProviderID, FileName: entry.Name, RawJSON: resp.JSON})
	if err != nil || !parsed.Handled {
		return "", fmt.Errorf("CommandCode auth file has no valid API key")
	}
	return parsed.Auth.Attributes["api_key"], nil
}

// host.auth.save initially exposes the key in Metadata; the directory watcher
// later parses Attributes. Both representations belong to the SAME selected
// host credential, never to an independent configured key pool.
func selectedAuthKey(attributes map[string]string, metadata map[string]any) string {
	if key := strings.TrimSpace(attributes["api_key"]); key != "" {
		return key
	}
	key, _ := metadata["api_key"].(string)
	return strings.TrimSpace(key)
}

func (m *Manager) addAccount(ctx context.Context, body []byte) (pluginapi.ManagementResponse, error) {
	var req struct {
		APIKey string `json:"api_key"`
	}
	if len(body) > 16*1024 || json.Unmarshal(body, &req) != nil {
		return managementError(http.StatusBadRequest, "API key 请求格式无效")
	}
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	auth, failure := m.prepareCredentialLocked(ctx, req.APIKey, "api_key")
	if failure != nil {
		return managementError(failure.status, failure.message)
	}
	// A timed-out callback may still finish its save. Only observing a matching
	// file reconciles the digest claim, even if AuthSave returned an error.
	if err := m.bridge.AuthSave(ctx, pluginapi.HostAuthSaveRequest{Name: auth.FileName, JSON: auth.StorageJSON}); err != nil {
		return managementError(http.StatusBadGateway, "保存结果尚未确认，请先刷新认证文件列表；不会自动重新保存")
	}
	resp, err := quotaJSON(map[string]string{"file_name": auth.FileName})
	resp.StatusCode = http.StatusCreated
	return resp, err
}

func managementError(status int, message string) (pluginapi.ManagementResponse, error) {
	resp, err := quotaJSON(map[string]string{"error": message})
	resp.StatusCode = status
	return resp, err
}
