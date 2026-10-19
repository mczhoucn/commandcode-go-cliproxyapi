package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type authProvider struct{}

var _ pluginapi.AuthProvider = authProvider{}

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

func (authProvider) StartLogin(context.Context, pluginapi.AuthLoginStartRequest) (pluginapi.AuthLoginStartResponse, error) {
	return pluginapi.AuthLoginStartResponse{}, fmt.Errorf("commandcode login is unsupported; configure a manual api key")
}

func (authProvider) PollLogin(context.Context, pluginapi.AuthLoginPollRequest) (pluginapi.AuthLoginPollResponse, error) {
	return pluginapi.AuthLoginPollResponse{}, fmt.Errorf("commandcode login is unsupported; configure a manual api key")
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
	if json.Unmarshal(body, &req) != nil {
		return managementError(http.StatusBadRequest, "API key 请求格式无效")
	}
	key := strings.TrimSpace(req.APIKey)
	if key == "" || strings.IndexFunc(key, unicode.IsSpace) >= 0 {
		return managementError(http.StatusBadRequest, "请输入不含空白字符的 API key")
	}
	// Serialize additions with each other and with lifecycle mutations. A
	// duplicate submission must not overwrite disabled state or user metadata.
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	if m.bridge == nil {
		return managementError(http.StatusBadGateway, "无法读取认证文件，请稍后重试")
	}
	files, err := m.bridge.AuthList(ctx)
	if err != nil {
		return managementError(http.StatusBadGateway, "无法读取认证文件，请稍后重试")
	}
	name := authFileNameFromHash(authKeyHash(key))
	for _, entry := range files {
		if entry.Source == "file" && entry.Name == name {
			return managementError(http.StatusConflict, "同名认证文件已存在，请在认证文件页面管理")
		}
		if !isCommandcodeAuthFile(entry) {
			continue
		}
		existingKey, err := authFileKey(ctx, m.bridge, entry)
		if err != nil {
			return managementError(http.StatusBadGateway, "无法核对现有认证文件，未保存 API key")
		}
		if existingKey == key {
			return managementError(http.StatusConflict, "此 API key 已存在，请在认证文件页面管理")
		}
	}
	// Omit an independent JSON id: both host.auth.save and ParseAuth derive
	// the same runtime ID from the filename, avoiding duplicate credentials.
	record, _ := json.Marshal(map[string]any{"type": ProviderID, "api_key": key})
	if err := m.bridge.AuthSave(ctx, pluginapi.HostAuthSaveRequest{Name: name, JSON: record}); err != nil {
		return managementError(http.StatusBadGateway, "保存认证文件失败，请刷新账户列表确认后重试")
	}
	resp, err := quotaJSON(map[string]string{"file_name": name})
	resp.StatusCode = http.StatusCreated
	return resp, err
}

func managementError(status int, message string) (pluginapi.ManagementResponse, error) {
	resp, err := quotaJSON(map[string]string{"error": message})
	resp.StatusCode = status
	return resp, err
}
