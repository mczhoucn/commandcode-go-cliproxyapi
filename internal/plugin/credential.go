package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"commandcode-or-or-cliproxyapi/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type credentialError struct {
	status  int
	message string
}

// checkExistingCredentialLocked reads the actual host set, including custom
// filenames and disabled credentials. lifeMu protects only our two entry points,
// never the host's independent uploads/edits. Do not mistake this for create-CAS.
func (m *Manager) checkExistingCredentialLocked(ctx context.Context, key string) *credentialError {
	if m.bridge == nil {
		return &credentialError{http.StatusBadGateway, "无法读取认证文件，请稍后重试"}
	}
	files, err := m.bridge.AuthList(ctx)
	if err != nil {
		return &credentialError{http.StatusBadGateway, "无法读取认证文件，请稍后重试"}
	}
	name := authFileNameFromHash(authKeyHash(key))
	duplicate := false
	for _, entry := range files {
		// Duplicate detection spans file-backed AND runtime-only commandcode
		// credentials (finding 7), so use the broad predicate here.
		if entry.Source == "file" && entry.Name == name && !isCommandcodeCredential(entry) {
			return &credentialError{http.StatusConflict, "同名认证文件已存在，请在认证文件页面管理"}
		}
		if !isCommandcodeCredential(entry) {
			continue
		}
		existing, err := authFileKey(ctx, m.bridge, entry)
		if err != nil {
			// An unreadable CommandCode auth file cannot be compared by key, but
			// can still prevent a duplicate-name collision. Skip it for key
			// comparison and continue checking others.
			debugTrace("checkExisting unreadable_file=%s auth_index=%s error=%v", entry.Name, entry.AuthIndex, err)
			if entry.Name == name {
				duplicate = true
			}
			continue
		}
		// Confirmed files reconcile digest-only reservations, not credential data.
		delete(m.credentialClaims, authKeyHash(existing))
		if existing == key || entry.Name == name {
			duplicate = true
		}
	}
	if duplicate {
		return &credentialError{http.StatusConflict, "此 API key 已存在，请在认证文件页面管理"}
	}
	return nil
}

// verifyCredential performs the upstream whoami/subscription checks WITHOUT
// holding lifeMu, so upstream latency cannot block other operations. Returns
// the verified email, plan ID, and any failure.
func (m *Manager) verifyCredential(ctx context.Context, key string) (email, planID string, failure *credentialError) {
	m.mu.RLock()
	baseURL, timeout := m.cfg.BaseURL, m.cfg.RequestTimeout
	m.mu.RUnlock()
	if baseURL == "" {
		defaults, _ := config.Load(nil)
		baseURL = defaults.BaseURL
	}
	base, err := AccountAPIBase(baseURL)
	if err != nil {
		return "", "", &credentialError{http.StatusBadGateway, "账号接口配置无效，未保存 API key"}
	}
	if timeout <= 0 || timeout > quotaMaxTimeout {
		timeout = quotaMaxTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	get := func(path string, target any) *credentialError {
		resp, err := m.bridge.Do(ctx, pluginapi.HTTPRequest{Method: http.MethodGet, URL: base + path,
			Headers: http.Header{"Authorization": {"Bearer " + key}, "Accept": {"application/json"}}})
		if err != nil {
			return &credentialError{http.StatusBadGateway, "身份或订阅核验请求失败，请稍后重试；未保存"}
		}
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			return &credentialError{http.StatusUnauthorized, "API key 无效或无权访问账号接口，未保存"}
		}
		if resp.StatusCode == 429 {
			return &credentialError{http.StatusTooManyRequests, "账号核验被限流，请稍后重试；未保存"}
		}
		if resp.StatusCode != http.StatusOK || len(resp.Body) > 1024*1024 || json.Unmarshal(resp.Body, target) != nil {
			return &credentialError{http.StatusBadGateway, "账号核验返回异常，无法确认；未保存"}
		}
		return nil
	}
	var who accountWhoami
	if failure := get(accountWhoamiPath, &who); failure != nil {
		return "", "", failure
	}
	verifiedEmail := strings.TrimSpace(who.User.Email)
	address, emailErr := mail.ParseAddress(verifiedEmail)
	if !who.Success || emailErr != nil || address.Address != verifiedEmail {
		return "", "", &credentialError{http.StatusBadGateway, "无法确认账号身份，未保存 API key"}
	}
	var sub accountSubscription
	if failure := get(accountSubscriptionPath, &sub); failure != nil {
		return "", "", failure
	}
	allowed := sub.Data.PlanID == "individual-goat" || sub.Data.PlanID == "individual-pro" || sub.Data.PlanID == "individual-pro-v1" || sub.Data.PlanID == "individual-max"
	// Finding 6: reuse quota.go's tolerant periodEndRFC3339 (RFC3339/date-only/…)
	// and distinguish two cases. An empty or explicit-null currentPeriodEnd on an
	// active, allowed plan is treated as NON-EXPIRING (no renewal instant to
	// enforce). A non-empty value is required to parse to a future instant;
	// otherwise the subscription cannot be confirmed valid and is rejected.
	rawEnd := strings.TrimSpace(sub.Data.CurrentPeriodEnd)
	nonExpiring := rawEnd == "" || rawEnd == "null"
	if !sub.Success || !allowed || sub.Data.Status != "active" {
		return "", "", &credentialError{http.StatusUnprocessableEntity, "仅支持可确认的有效 GOAT / Pro / Max 订阅；Go、未知或已失效套餐不保存"}
	}
	if !nonExpiring {
		// periodEndRFC3339 returns "" for an unparseable value; rawEnd is non-empty
		// here, so "" means "provided but unparseable" → reject.
		periodEnd := periodEndRFC3339(rawEnd)
		endTime, endErr := time.Parse(time.RFC3339, periodEnd)
		if periodEnd == "" || endErr != nil || !time.Now().Before(endTime) {
			return "", "", &credentialError{http.StatusUnprocessableEntity, "仅支持可确认的有效 GOAT / Pro / Max 订阅；Go、未知或已失效套餐不保存"}
		}
	}
	if ctx.Err() != nil {
		return "", "", &credentialError{http.StatusBadGateway, "核验已超时，未保存 API key"}
	}
	return verifiedEmail, sub.Data.PlanID, nil
}

// prepareCredentialLocked is the admission seam shared by manual entry and the
// browser guide. Caller should call verifyCredential first WITHOUT holding
// lifeMu, then call this WITH lifeMu for duplicate check + claim + save.
func (m *Manager) prepareCredentialLocked(ctx context.Context, key, email, planID, source string) (pluginapi.AuthData, *credentialError) {
	m.pruneLoginsLocked()
	if failure := m.checkExistingCredentialLocked(ctx, key); failure != nil {
		return pluginapi.AuthData{}, failure
	}
	hash := authKeyHash(key)
	if _, claimed := m.credentialClaims[hash]; claimed {
		return pluginapi.AuthData{}, &credentialError{http.StatusConflict, "此 API key 正在保存或保存结果尚未确认；请先查看认证文件，不要重复提交"}
	}
	if len(m.credentialClaims) >= 64 {
		return pluginapi.AuthData{}, &credentialError{http.StatusConflict, "未确认的保存过多，请先核对认证文件"}
	}
	metadata := map[string]any{"type": ProviderID, "api_key": key, "email": email, "plan_id": planID, "credential_source": source}
	record, _ := json.Marshal(metadata)
	name := authFileNameFromHash(hash)
	if m.credentialClaims == nil {
		m.credentialClaims = make(map[string]credentialClaim)
	}
	m.credentialClaims[hash] = credentialClaim{claimed: true, createdAt: time.Now()}
	return pluginapi.AuthData{Provider: ProviderID, ID: name, FileName: name, Label: email, StorageJSON: record, Metadata: metadata, Attributes: map[string]string{"api_key": key}}, nil
}
