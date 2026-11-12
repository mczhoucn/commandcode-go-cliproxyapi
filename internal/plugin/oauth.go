package plugin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const loginLifetime = 10 * time.Minute
const maxLoginSessions = 64

// All session/claim access is serialized by lifeMu. Delivered sessions retain
// only a digest: CPA owns the only save and does not acknowledge its outcome.
type loginSession struct {
	expires time.Time
	phase   string
	auth    pluginapi.AuthData
	hash    string
	message string
}

func (m *Manager) startLogin(req pluginapi.AuthLoginStartRequest) (pluginapi.AuthLoginStartResponse, error) {
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	if req.Provider != "" && req.Provider != ProviderID {
		return pluginapi.AuthLoginStartResponse{}, fmt.Errorf("不支持此登录提供商")
	}
	m.pruneLoginsLocked()
	if len(m.logins) >= maxLoginSessions {
		return pluginapi.AuthLoginStartResponse{}, fmt.Errorf("登录会话过多，请等待旧会话过期")
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return pluginapi.AuthLoginStartResponse{}, fmt.Errorf("无法创建登录会话")
	}
	state := hex.EncodeToString(random[:])
	expires := time.Now().Add(loginLifetime)
	if m.logins == nil {
		m.logins = make(map[string]*loginSession)
	}
	m.logins[state] = &loginSession{expires: expires, phase: "pending"}
	// Relative to management.html, including a reverse proxy's external prefix.
	// Ignore BaseURL: the host supplies its own loopback callback, not a browser URL.
	return pluginapi.AuthLoginStartResponse{
		Provider: ProviderID, State: state, ExpiresAt: expires,
		URL: "./v0/resource/plugins/" + pluginName + "/oauth#state=" + state,
	}, nil
}

func officialAuthorizationURL(state string) string {
	q := url.Values{"callback": {"http://localhost:6000/callback"}, "state": {state}}
	// Deliberately omit mode=redirect: that branch submits a form instead of
	// executing the fetch failure handler which exposes the official fallback.
	return "https://commandcode.ai/studio/auth/cli?" + q.Encode()
}

func (m *Manager) releaseUndeliveredLocked(s *loginSession) {
	if s.phase != "delivered" && s.hash != "" {
		delete(m.credentialClaims, s.hash)
		s.hash = "" // Release at most once; later cleanup must not delete a newer claim.
	}
	s.auth = pluginapi.AuthData{}
}

func (m *Manager) pruneLoginsLocked() {
	now := time.Now()
	for state, s := range m.logins {
		if !now.Before(s.expires) {
			m.releaseUndeliveredLocked(s)
			delete(m.logins, state)
		}
	}
}

func (m *Manager) clearPendingLoginsLocked() {
	for state, s := range m.logins {
		if s.phase == "delivered" {
			// A reconfigure can follow the host's save on older CPA versions.
			// Retain digest-only confirmation until expiry; never re-deliver.
			continue
		}
		m.releaseUndeliveredLocked(s)
		delete(m.logins, state)
	}
	// Keep unresolved delivered/manual-save digests across reconfiguration.
	// A timeout or empty list is NOT evidence that a host save cannot still land.
}

func loginFailure(message string) pluginapi.AuthLoginPollResponse {
	return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: message}
}

func (m *Manager) pollLogin(ctx context.Context, req pluginapi.AuthLoginPollRequest) pluginapi.AuthLoginPollResponse {
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	m.pruneLoginsLocked()
	s := m.logins[req.State]
	if (req.Provider != "" && req.Provider != ProviderID) || s == nil {
		return loginFailure("登录会话无效或已过期，请重新开始")
	}
	switch s.phase {
	case "error", "cancelled":
		return loginFailure(s.message)
	case "ready":
		key := s.auth.Attributes["api_key"]
		if failure := m.checkExistingCredentialLocked(ctx, key); failure != nil {
			m.releaseUndeliveredLocked(s)
			s.phase, s.message = "error", failure.message
			return loginFailure(s.message)
		}
		if !time.Now().Before(s.expires) {
			m.releaseUndeliveredLocked(s)
			s.phase, s.message = "error", "登录会话已过期，请重新开始"
			return loginFailure(s.message)
		}
		auth := s.auth
		s.auth = pluginapi.AuthData{}
		s.phase = "delivered"
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusSuccess, Auth: auth}
	default:
		// Never re-deliver or return an error while another host poll may save.
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending}
	}
}

func (m *Manager) handleLoginManagement(ctx context.Context, action string, body []byte) (pluginapi.ManagementResponse, error) {
	var req struct {
		State  string `json:"state"`
		APIKey string `json:"api_key"`
	}
	if len(body) > 16*1024 || json.Unmarshal(body, &req) != nil || len(req.State) != 64 {
		return managementError(http.StatusBadRequest, "登录请求格式无效")
	}
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	m.pruneLoginsLocked()
	s := m.logins[req.State]
	if s == nil {
		return managementError(http.StatusGone, "登录会话无效或已过期，请重新开始")
	}
	switch action {
	case "complete":
		if s.phase != "pending" {
			return managementError(http.StatusConflict, "此登录已提交、取消或交付，请先查看 CPA 状态和认证文件，不要重复提交")
		}
		auth, failure := m.prepareCredentialLocked(ctx, req.APIKey, "browser_authorization")
		if failure != nil {
			return managementError(failure.status, failure.message)
		}
		s.auth, s.hash = auth, authKeyHash(auth.Attributes["api_key"])
		s.phase = "ready"
		if !time.Now().Before(s.expires) {
			m.releaseUndeliveredLocked(s)
			s.phase, s.message = "error", "登录会话已过期，请重新开始"
			return managementError(http.StatusGone, s.message)
		}
		resp, err := quotaJSON(map[string]string{"status": "ready", "message": "已验证，等待 CPA 保存", "file_name": auth.FileName})
		resp.StatusCode = http.StatusAccepted
		return resp, err
	case "cancel":
		if s.phase == "delivered" {
			return managementError(http.StatusConflict, "认证已交付 CPA，不能保证取消能撤回保存；请先查看认证文件")
		}
		m.releaseUndeliveredLocked(s)
		s.phase, s.message = "cancelled", "登录已取消"
		return quotaJSON(map[string]string{"status": "cancelled"})
	case "session":
		result := map[string]any{"status": s.phase, "expires_at": s.expires, "message": s.message}
		if s.phase == "pending" {
			result["authorization_url"] = officialAuthorizationURL(req.State)
		}
		if s.hash != "" {
			result["file_name"] = authFileNameFromHash(s.hash)
		}
		if s.phase == "delivered" {
			// Independent confirmation for the page. File existence alone isn't
			// enough; inspect the provider-owned credential and compare digests.
			files, err := commandcodeAuthFiles(ctx, m.bridge)
			if err != nil {
				return managementError(http.StatusBadGateway, "无法确认保存结果，请刷新认证文件列表，不要重复提交")
			}
			for _, entry := range files {
				key, err := authFileKey(ctx, m.bridge, entry)
				if err != nil {
					return managementError(http.StatusBadGateway, "无法核对保存结果，请刷新认证文件列表")
				}
				if authKeyHash(key) == s.hash {
					result["status"] = "present"
					delete(m.credentialClaims, s.hash)
					break
				}
			}
		}
		return quotaJSON(result)
	default:
		return managementError(http.StatusNotFound, "not found")
	}
}
