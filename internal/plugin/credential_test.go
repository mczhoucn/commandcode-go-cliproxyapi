package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestCredentialAdmissionPlansAndSources(t *testing.T) {
	for _, plan := range []string{"individual-goat", "individual-pro", "individual-pro-v1", "individual-max"} {
		for _, source := range []string{"api_key", "browser_authorization"} {
			t.Run(plan+"/"+source, func(t *testing.T) {
				m, f := oauthTestManager()
				old := f.responder
				f.responder = func(method string, payload []byte) ([]byte, error) {
					if method == pluginabi.MethodHostHTTPDo {
						var req hostHTTPReq
						_ = json.Unmarshal(payload, &req)
						if strings.Contains(req.URL, accountSubscriptionPath) {
							return hostOK(pluginapi.HTTPResponse{StatusCode: 200, Body: mustJSON(map[string]any{"success": true, "data": map[string]string{"planId": plan, "status": "active", "currentPeriodEnd": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)}})}), nil
						}
					}
					return old(method, payload)
				}
				m.lifeMu.Lock()
				auth, failure := m.prepareCredentialLocked(context.Background(), "  verified-key  ", source)
				m.lifeMu.Unlock()
				if failure != nil {
					t.Fatal(failure.message)
				}
				if auth.Attributes["api_key"] != "verified-key" || auth.Metadata["plan_id"] != plan || auth.Metadata["credential_source"] != source {
					t.Fatal("wrong credential metadata")
				}
				if len(f.callsOf(pluginabi.MethodHostAuthSave)) != 0 {
					t.Fatal("verification saved")
				}
			})
		}
	}
}

func TestCredentialAdmissionRejectsUnverifiableResponses(t *testing.T) {
	cases := []struct {
		name, path, body string
		status           int
	}{
		{"who-false", accountWhoamiPath, `{"success":false,"user":{"email":"a@example.test"}}`, 200},
		{"who-empty", accountWhoamiPath, `{"success":true,"user":{}}`, 200},
		{"who-malformed", accountWhoamiPath, `{"success":true,"user":{"email":"not-an-email"}}`, 200},
		{"who-invalid", accountWhoamiPath, "credential-secret", 401},
		{"who-forbidden", accountWhoamiPath, "credential-secret", 403},
		{"who-limited", accountWhoamiPath, "credential-secret", 429},
		{"who-error", accountWhoamiPath, "credential-secret", 503},
		{"who-json", accountWhoamiPath, "credential-secret", 200},
		{"sub-json", accountSubscriptionPath, "credential-secret", 200},
		{"sub-false", accountSubscriptionPath, `{"success":false,"data":{"planId":"individual-goat","status":"active","currentPeriodEnd":"2099-01-01T00:00:00Z"}}`, 200},
	}
	for _, plan := range []string{"individual-go", "individual-goat-future", "individual-pro-v2", "individual-provider", "individual-ultra", "teams-pro", "unknown", ""} {
		cases = append(cases, struct {
			name, path, body string
			status           int
		}{"plan-" + plan, accountSubscriptionPath, string(mustJSON(map[string]any{"success": true, "data": map[string]string{"planId": plan, "status": "active", "currentPeriodEnd": "2099-01-01T00:00:00Z"}})), 200})
	}
	for _, status := range []string{"expired", "cancelled", "trialing", "past_due", "ACTIVE", ""} {
		cases = append(cases, struct {
			name, path, body string
			status           int
		}{"status-" + status, accountSubscriptionPath, string(mustJSON(map[string]any{"success": true, "data": map[string]string{"planId": "individual-goat", "status": status, "currentPeriodEnd": "2099-01-01T00:00:00Z"}})), 200})
	}
	for _, end := range []string{"2020-01-01T00:00:00Z", "invalid", ""} {
		cases = append(cases, struct {
			name, path, body string
			status           int
		}{"end-" + end, accountSubscriptionPath, string(mustJSON(map[string]any{"success": true, "data": map[string]string{"planId": "individual-goat", "status": "active", "currentPeriodEnd": end}})), 200})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, entry := range []string{"api_key", "browser_authorization"} {
				m, f := oauthTestManager()
				old := f.responder
				f.responder = func(method string, payload []byte) ([]byte, error) {
					if method == pluginabi.MethodHostHTTPDo {
						var req hostHTTPReq
						_ = json.Unmarshal(payload, &req)
						if strings.HasSuffix(req.URL, tc.path) {
							return hostOK(pluginapi.HTTPResponse{StatusCode: tc.status, Body: []byte(tc.body)}), nil
						}
					}
					return old(method, payload)
				}
				var resp pluginapi.ManagementResponse
				if entry == "api_key" {
					resp, _ = m.addAccount(context.Background(), mustJSON(map[string]string{"api_key": "credential-secret"}))
				} else {
					start := loginStart(t, m)
					resp = loginRequest(t, m, "complete", start.State, "credential-secret")
				}
				if resp.StatusCode < 400 || len(f.callsOf(pluginabi.MethodHostAuthSave)) != 0 || strings.Contains(string(resp.Body), "credential-secret") || m.credentialClaims[authKeyHash("credential-secret")] {
					t.Fatalf("unverifiable %s accepted or leaked key: %d", entry, resp.StatusCode)
				}
			}
		})
	}
}

func TestCredentialTimeoutAndUnknownSaveKeepCorrectClaims(t *testing.T) {
	t.Run("verification timeout", func(t *testing.T) {
		m, f := oauthTestManager()
		old := f.responder
		m.cfg.RequestTimeout = time.Millisecond
		f.responder = func(method string, payload []byte) ([]byte, error) {
			if method == pluginabi.MethodHostHTTPDo {
				time.Sleep(10 * time.Millisecond)
				return nil, errors.New("credential-secret")
			}
			return old(method, payload)
		}
		resp, _ := m.addAccount(context.Background(), mustJSON(map[string]string{"api_key": "timeout-key"}))
		if resp.StatusCode < 400 || len(f.callsOf(pluginabi.MethodHostAuthSave)) != 0 || len(m.credentialClaims) != 0 {
			t.Fatal("timed out validation saved or occupied")
		}
		if !m.bridge.WaitForInFlight(time.Second) {
			t.Fatal("host callback not drained")
		}
	})
	t.Run("save uncertainty", func(t *testing.T) {
		m, f := oauthTestManager()
		old := f.responder
		f.responder = func(method string, payload []byte) ([]byte, error) {
			if method == pluginabi.MethodHostAuthSave {
				return nil, errors.New("credential-secret")
			}
			return old(method, payload)
		}
		resp, _ := m.addAccount(context.Background(), mustJSON(map[string]string{"api_key": "uncertain-save"}))
		if resp.StatusCode != http.StatusBadGateway || !m.credentialClaims[authKeyHash("uncertain-save")] {
			t.Fatal("uncertain save lost reservation")
		}
		resp, _ = m.addAccount(context.Background(), mustJSON(map[string]string{"api_key": "uncertain-save"}))
		if resp.StatusCode != http.StatusConflict || len(f.callsOf(pluginabi.MethodHostAuthSave)) != 1 {
			t.Fatal("uncertain save retried")
		}
	})
}
