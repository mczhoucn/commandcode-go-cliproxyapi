package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func oauthTestManager() (*Manager, *fakeCaller) {
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		if method == pluginabi.MethodHostHTTPDo {
			var req hostHTTPReq
			_ = json.Unmarshal(payload, &req)
			body := `{"success":true,"user":{"email":"oauth@example.test"}}`
			if strings.Contains(req.URL, accountSubscriptionPath) {
				body = `{"success":true,"data":{"planId":"individual-goat","status":"active","currentPeriodEnd":"2099-01-01T00:00:00Z"}}`
			}
			return hostOK(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(body)}), nil
		}
		return hostOK(map[string]any{}), nil
	}}
	f.setAuthFiles()
	return NewManager(NewHostBridge(f.call)), f
}

func loginStart(t *testing.T, m *Manager) pluginapi.AuthLoginStartResponse {
	t.Helper()
	var start pluginapi.AuthLoginStartResponse
	decodeResult(t, mustHandle(t, m, pluginabi.MethodAuthLoginStart, mustJSON(pluginapi.AuthLoginStartRequest{Provider: ProviderID, BaseURL: "http://127.0.0.1:8317/callback"})), &start)
	return start
}

func loginRequest(t *testing.T, m *Manager, action, state, key string) pluginapi.ManagementResponse {
	t.Helper()
	resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost,
		Path: "/v0/management/plugins/" + pluginName + "/oauth/" + action, Body: mustJSON(map[string]string{"state": state, "api_key": key})})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestOAuthStartAndPublicResource(t *testing.T) {
	m, _ := oauthTestManager()
	start := loginStart(t, m)
	other := loginStart(t, m)
	if len(start.State) != 64 || start.State == other.State || !strings.HasPrefix(start.URL, "./v0/resource/") || strings.Contains(start.URL, "127.0.0.1") {
		t.Fatal("login URL/state contract not met")
	}
	u, err := url.Parse(officialAuthorizationURL(start.State))
	if err != nil || u.Host != "commandcode.ai" || u.Query().Get("callback") != "http://localhost:6000/callback" || u.Query().Get("mode") != "" || u.Query().Get("state") != start.State {
		t.Fatal("wrong official fallback configuration")
	}
	resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/resource/plugins/" + pluginName + "/oauth"})
	if err != nil || resp.Headers.Get("Cache-Control") != "no-store" || !strings.Contains(string(resp.Body), "type=\"password\"") || strings.Contains(string(resp.Body), start.State) {
		t.Fatal("public page contains session material or lacks secure headers")
	}
}

func TestOAuthCompletionOnlyStagesAndPollDeliversOnce(t *testing.T) {
	m, f := oauthTestManager()
	start := loginStart(t, m)
	if got := m.pollLogin(context.Background(), pluginapi.AuthLoginPollRequest{State: start.State}); got.Status != pluginapi.AuthLoginStatusPending {
		t.Fatal("fresh session not pending")
	}
	if resp := loginRequest(t, m, "complete", start.State, "browser-key"); resp.StatusCode != http.StatusAccepted || strings.Contains(string(resp.Body), "browser-key") {
		t.Fatalf("completion failed: %d %s", resp.StatusCode, resp.Body)
	}
	if len(f.callsOf(pluginabi.MethodHostAuthSave)) != 0 {
		t.Fatal("completion saved instead of staging")
	}
	resp := loginRequest(t, m, "complete", start.State, "other-key")
	if resp.StatusCode != http.StatusConflict {
		t.Fatal("duplicate submission was accepted")
	}
	manual, _ := m.addAccount(context.Background(), mustJSON(map[string]string{"api_key": "browser-key"}))
	if manual.StatusCode != http.StatusConflict {
		t.Fatal("manual path bypassed OAuth key claim")
	}
	var wg sync.WaitGroup
	results := make(chan pluginapi.AuthLoginPollResponse, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- m.pollLogin(context.Background(), pluginapi.AuthLoginPollRequest{Provider: ProviderID, State: start.State})
		}()
	}
	wg.Wait()
	close(results)
	success := 0
	var delivered pluginapi.AuthData
	for got := range results {
		if got.Status == pluginapi.AuthLoginStatusSuccess {
			success++
			delivered = got.Auth
		} else if got.Status != pluginapi.AuthLoginStatusPending {
			t.Fatal("concurrent poll poisoned save")
		}
	}
	if success != 1 || delivered.Attributes["api_key"] != "browser-key" || delivered.FileName != authFileNameFromHash(authKeyHash("browser-key")) {
		t.Fatal("credential delivery was not exactly once")
	}
	var metadata map[string]any
	_ = json.Unmarshal(delivered.StorageJSON, &metadata)
	if metadata["credential_source"] != "browser_authorization" || metadata["email"] != "oauth@example.test" {
		t.Fatal("missing verified credential metadata")
	}
	if m.logins[start.State].auth.StorageJSON != nil || m.logins[start.State].auth.Attributes != nil {
		t.Fatal("delivered session retained raw key")
	}
	if loginRequest(t, m, "cancel", start.State, "").StatusCode != http.StatusConflict {
		t.Fatal("claimed cancellation after delivery")
	}
	// Simulate the host's save, not a second plugin-side AuthSave.
	_, err := f.call(pluginabi.MethodHostAuthSave, mustJSON(pluginapi.HostAuthSaveRequest{Name: delivered.FileName, JSON: delivered.StorageJSON}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(loginRequest(t, m, "session", start.State, "").Body), "\"status\":\"present\"") {
		t.Fatal("did not independently confirm saved credential")
	}
}

func TestOAuthCancelExpireAndReconfigureClearUndelivered(t *testing.T) {
	for _, action := range []string{"cancel", "expire", "reconfigure", "shutdown"} {
		t.Run(action, func(t *testing.T) {
			m, _ := oauthTestManager()
			start := loginStart(t, m)
			if loginRequest(t, m, "complete", start.State, "clear-key").StatusCode != http.StatusAccepted {
				t.Fatal("stage failed")
			}
			switch action {
			case "cancel":
				loginRequest(t, m, "cancel", start.State, "")
			case "expire":
				m.logins[start.State].expires = time.Now().Add(-time.Second)
				loginRequest(t, m, "session", start.State, "")
			case "reconfigure":
				mustHandle(t, m, pluginabi.MethodPluginReconfigure, lifecycleRequestBody(""))
				t.Cleanup(func() { _, _ = m.HandleCall(pluginabi.MethodPluginShutdown, nil) })
			case "shutdown":
				mustHandle(t, m, pluginabi.MethodPluginShutdown, nil)
			}
			if m.credentialClaims[authKeyHash("clear-key")] {
				t.Fatal("undelivered claim survived cleanup")
			}
			if got := m.pollLogin(context.Background(), pluginapi.AuthLoginPollRequest{State: start.State}); got.Status != pluginapi.AuthLoginStatusError {
				t.Fatal("cancelled/expired login delivered")
			}
		})
	}
}

func TestOAuthRechecksDuplicatesBeforeDelivery(t *testing.T) {
	m, f := oauthTestManager()
	start := loginStart(t, m)
	loginRequest(t, m, "complete", start.State, "duplicate-key")
	f.mu.Lock()
	f.authFiles["custom.json"] = json.RawMessage(`{"type":"commandcode","api_key":"duplicate-key","disabled":true,"note":"keep"}`)
	f.mu.Unlock()
	got := m.pollLogin(context.Background(), pluginapi.AuthLoginPollRequest{State: start.State})
	if got.Status != pluginapi.AuthLoginStatusError || len(f.callsOf(pluginabi.MethodHostAuthSave)) != 0 {
		t.Fatal("overwrote auth created while login pending")
	}
	if !strings.Contains(string(f.authFiles["custom.json"]), "\"disabled\":true") {
		t.Fatal("changed disabled state")
	}
}

func TestOAuthIdenticalReconfigurePreservesActiveSessions(t *testing.T) {
	for _, phase := range []string{"pending", "ready", "delivered"} {
		t.Run(phase, func(t *testing.T) {
			m, _ := oauthTestManager()
			mustHandle(t, m, pluginabi.MethodPluginRegister, lifecycleRequestBody(""))
			t.Cleanup(func() { _, _ = m.HandleCall(pluginabi.MethodPluginShutdown, nil) })
			start := loginStart(t, m)
			if phase != "pending" {
				loginRequest(t, m, "complete", start.State, "same-config-key")
			}
			if phase == "delivered" {
				m.pollLogin(context.Background(), pluginapi.AuthLoginPollRequest{State: start.State})
			}
			mustHandle(t, m, pluginabi.MethodPluginReconfigure, lifecycleRequestBody(""))
			if m.logins[start.State] == nil || m.logins[start.State].phase != phase {
				t.Fatal("identical config erased active login")
			}
		})
	}
}
func TestOAuthDeliveredClaimSurvivesUncertainSaveAndReconfigure(t *testing.T) {
	m, _ := oauthTestManager()
	start := loginStart(t, m)
	loginRequest(t, m, "complete", start.State, "uncertain-key")
	m.pollLogin(context.Background(), pluginapi.AuthLoginPollRequest{State: start.State})
	mustHandle(t, m, pluginabi.MethodPluginReconfigure, lifecycleRequestBody(""))
	t.Cleanup(func() { _, _ = m.HandleCall(pluginabi.MethodPluginShutdown, nil) })
	if got := loginRequest(t, m, "session", start.State, ""); got.StatusCode != 0 && got.StatusCode != http.StatusOK {
		t.Fatalf("delivered session lost across reconfigure: %d", got.StatusCode)
	}
	resp, _ := m.addAccount(context.Background(), mustJSON(map[string]string{"api_key": "uncertain-key"}))
	if resp.StatusCode != http.StatusConflict {
		t.Fatal("reconfigure/empty host list released uncertain save claim")
	}
}

func TestOAuthOldCancelledSessionCannotReleaseNewClaim(t *testing.T) {
	for _, cleanup := range []string{"cancel", "expiry", "reconfigure"} {
		t.Run(cleanup, func(t *testing.T) {
			m, f := oauthTestManager()
			start := loginStart(t, m)
			loginRequest(t, m, "complete", start.State, "reused-key")
			loginRequest(t, m, "cancel", start.State, "")
			original := f.responder
			f.responder = func(method string, payload []byte) ([]byte, error) {
				if method == pluginabi.MethodHostAuthSave {
					return hostErr("timeout", "redacted"), nil
				}
				return original(method, payload)
			}
			first, _ := m.addAccount(context.Background(), mustJSON(map[string]string{"api_key": "reused-key"}))
			if first.StatusCode != http.StatusBadGateway {
				t.Fatal("save uncertainty not reached")
			}
			switch cleanup {
			case "cancel":
				loginRequest(t, m, "cancel", start.State, "")
			case "expiry":
				m.logins[start.State].expires = time.Now().Add(-time.Second)
				loginStart(t, m)
			case "reconfigure":
				mustHandle(t, m, pluginabi.MethodPluginReconfigure, lifecycleRequestBody(""))
				t.Cleanup(func() { _, _ = m.HandleCall(pluginabi.MethodPluginShutdown, nil) })
			}
			second, _ := m.addAccount(context.Background(), mustJSON(map[string]string{"api_key": "reused-key"}))
			if second.StatusCode != http.StatusConflict || len(f.callsOf(pluginabi.MethodHostAuthSave)) != 1 {
				t.Fatal("old session released another save's claim")
			}
		})
	}
}

func TestOAuthExpiredReadyAllowsDirectManualButDeliveredStaysClaimed(t *testing.T) {
	for _, delivered := range []bool{false, true} {
		t.Run(fmt.Sprint(delivered), func(t *testing.T) {
			m, _ := oauthTestManager()
			start := loginStart(t, m)
			loginRequest(t, m, "complete", start.State, "expiry-key")
			if delivered {
				m.pollLogin(context.Background(), pluginapi.AuthLoginPollRequest{State: start.State})
			}
			m.logins[start.State].expires = time.Now().Add(-time.Second)
			resp, _ := m.addAccount(context.Background(), mustJSON(map[string]string{"api_key": "expiry-key"}))
			want := http.StatusCreated
			if delivered {
				want = http.StatusConflict
			}
			if resp.StatusCode != want {
				t.Fatalf("manual add after expiry=%d want=%d", resp.StatusCode, want)
			}
		})
	}
}

func TestOAuthIdleRefreshLoopPrunesExpiredReady(t *testing.T) {
	m, _ := oauthTestManager()
	mustHandle(t, m, pluginabi.MethodPluginRegister, lifecycleRequestBody(""))
	if old := m.closeStop(); old != nil {
		<-old
	}
	start := loginStart(t, m)
	loginRequest(t, m, "complete", start.State, "idle-key")
	m.logins[start.State].expires = time.Now().Add(-time.Second)
	stop, done := make(chan struct{}), make(chan struct{})
	m.startRefreshLoop(m.cfg, m.mgr, time.Millisecond, stop, done)
	defer func() { close(stop); <-done }()
	deadline := time.Now().Add(time.Second)
	for {
		m.lifeMu.Lock()
		retained := m.logins[start.State] != nil || m.credentialClaims[authKeyHash("idle-key")]
		m.lifeMu.Unlock()
		if !retained {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("idle background loop retained expired ready credential")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestOAuthRejectsUnknownSessionsAndCapsActiveSessions(t *testing.T) {
	m, _ := oauthTestManager()
	if loginRequest(t, m, "complete", strings.Repeat("a", 64), "new-key").StatusCode != http.StatusGone {
		t.Fatal("unknown state accepted")
	}
	for i := 0; i < maxLoginSessions; i++ {
		loginStart(t, m)
	}
	if _, err := m.startLogin(pluginapi.AuthLoginStartRequest{}); err == nil {
		t.Fatal("unbounded session accumulation")
	}
}
