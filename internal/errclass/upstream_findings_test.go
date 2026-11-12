package errclass

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// TestCredentialScopedReports429 pins Finding 1: every credential-scoped outcome
// must report HTTP status 429 (not 402/403 from upstream) because the host's
// conductor_cooldown only triggers credential-wide cooldown when HTTP status
// is 429 AND CredentialScoped is true.
func TestCredentialScopedReports429(t *testing.T) {
	cases := []struct {
		name         string
		upstreamCode int
		body         []byte
		wantWireCode string
	}{
		{
			"USAGE_EXCEEDED 402",
			http.StatusPaymentRequired,
			[]byte(`{"error": {"code": "USAGE_EXCEEDED", "message": "out of credits"}}`),
			pluginabi.ErrorCodeCredentialQuota,
		},
		{
			"spend cap 403",
			http.StatusForbidden,
			[]byte(`{"error": {"message": "spend cap exceeded"}}`),
			pluginabi.ErrorCodeCredentialQuota,
		},
		{
			"subscription expired 403",
			http.StatusForbidden,
			[]byte(`{"error": {"message": "subscription expired"}}`),
			pluginabi.ErrorCodeCredentialQuota,
		},
		{
			"plan inactive 403",
			http.StatusForbidden,
			[]byte(`{"error": {"message": "plan inactive"}}`),
			pluginabi.ErrorCodeCredentialQuota,
		},
		{
			"RATE_LIMITED at 403",
			http.StatusForbidden,
			[]byte(`{"error": {"code": "RATE_LIMITED", "message": "weekly limit", "rateLimit": {"window": "weekly", "reset": 1234567890}}}`),
			pluginabi.ErrorCodeCredentialQuota,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := FromUpstream(tc.upstreamCode, tc.body)
			if e.StatusCode != http.StatusTooManyRequests {
				t.Errorf("StatusCode = %d, want %d (429) for credential-scoped", e.StatusCode, http.StatusTooManyRequests)
			}
			if e.WireCode != tc.wantWireCode {
				t.Errorf("WireCode = %q, want %q", e.WireCode, tc.wantWireCode)
			}
			if !e.CredentialScoped {
				t.Error("CredentialScoped = false, want true")
			}
		})
	}
}

// TestMaxRetryAfter32Days pins Finding 2: maxRetryAfterSeconds raised to 32 days
// to cover monthly billing periods, and parseRetryAfterHeader clamps instead of rejecting.
func TestMaxRetryAfter32Days(t *testing.T) {
	const monthlySeconds = 32 * 24 * 60 * 60
	if maxRetryAfterSeconds != monthlySeconds {
		t.Errorf("maxRetryAfterSeconds = %d, want %d (32 days)", maxRetryAfterSeconds, monthlySeconds)
	}
	// Large value: clamped, not rejected.
	// reset is a Unix timestamp, so add the delta to the current time.
	largeReset := time.Now().Unix() + int64(monthlySeconds + 1000)
	body := map[string]any{
		"error": map[string]any{
			"code":      "RATE_LIMITED",
			"rateLimit": map[string]any{"reset": largeReset, "window": "monthly"},
		},
	}
	raw, _ := json.Marshal(body)
	e := FromUpstream(http.StatusTooManyRequests, raw)
	if e.RetryAfterSeconds != maxRetryAfterSeconds {
		t.Errorf("RetryAfterSeconds = %d, want clamped to %d", e.RetryAfterSeconds, maxRetryAfterSeconds)
	}
}

// TestWindowExhaustionBroadened pins Finding 3: accept error.rateLimit presence,
// RATE_LIMITED at non-429 status, and broader message patterns.
func TestWindowExhaustionBroadened(t *testing.T) {
	cases := []struct {
		name         string
		status       int
		body         []byte
		wantDetected bool
		wantWindow   string
	}{
		{
			"rateLimit present with window",
			http.StatusTooManyRequests,
			[]byte(`{"error": {"rateLimit": {"window": "five_hour", "reset": 1234567890}}}`),
			true,
			"5-hour",
		},
		{
			"rateLimit present with reset only",
			http.StatusTooManyRequests,
			[]byte(`{"error": {"rateLimit": {"reset": 1234567890}}}`),
			true,
			"", // no window string
		},
		{
			"RATE_LIMITED at 403",
			http.StatusForbidden,
			[]byte(`{"error": {"code": "RATE_LIMITED", "message": "weekly usage exceeded", "rateLimit": {"window": "weekly", "reset": 1234567890}}}`),
			true,
			"weekly",
		},
		{
			"monthly usage wording",
			http.StatusTooManyRequests,
			[]byte(`{"error": {"message": "monthly usage exceeded"}}`),
			true,
			"monthly",
		},
		{
			"billing period wording",
			http.StatusTooManyRequests,
			[]byte(`{"error": {"message": "billing period limit reached"}}`),
			true,
			"monthly",
		},
		{
			"window_limit wording",
			http.StatusTooManyRequests,
			[]byte(`{"error": {"message": "window_limit reached"}}`),
			true,
			"", // generic window
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := FromUpstream(tc.status, tc.body)
			if tc.wantDetected {
				if e.WireCode != pluginabi.ErrorCodeCredentialQuota {
					t.Errorf("WireCode = %q, want %q (window exhaustion)", e.WireCode, pluginabi.ErrorCodeCredentialQuota)
				}
				if !e.CredentialScoped {
					t.Error("CredentialScoped = false, want true for window exhaustion")
				}
				if tc.wantWindow != "" && e.Message != "" && !strings.Contains(e.Message, tc.wantWindow) {
					t.Errorf("Message = %q, want substring %q", e.Message, tc.wantWindow)
				}
			}
		})
	}
}

// TestTolerantJSONDecoding pins Finding 4: ccErrorBody tolerates type drift
// (string code, numeric string reset, ISO reset).
func TestTolerantJSONDecoding(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantParsed bool
	}{
		{"code as string", `{"error": {"code": "RATE_LIMITED", "message": "x"}}`, true},
		{"code as number", `{"error": {"code": 429001, "message": "x"}}`, true},
		{"reset as number", `{"error": {"rateLimit": {"reset": 1234567890}}}`, true},
		{"reset as numeric string", `{"error": {"rateLimit": {"reset": "1234567890"}}}`, true},
		{"reset as RFC3339", `{"error": {"rateLimit": {"reset": "2025-01-15T10:00:00Z"}}}`, true},
		{"error as string (malformed)", `{"error": "plain text"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := FromUpstream(http.StatusTooManyRequests, []byte(tc.body))
			// Tolerant parsing never panics; it may return a fallback envelope.
			// If wantParsed, the message should not be empty.
			if tc.wantParsed && e.Message == "" {
				t.Errorf("Message empty after parsing %q", tc.body)
			}
		})
	}
}

// TestMillisecondResetValues pins Finding 5: reset > 1e11 treated as milliseconds.
func TestMillisecondResetValues(t *testing.T) {
	// Use actual future timestamp: now + 3600 seconds, in milliseconds.
	nowUnix := time.Now().Unix()
	msReset := (nowUnix + 3600) * 1000
	body := map[string]any{
		"error": map[string]any{
			"rateLimit": map[string]any{"reset": msReset, "window": "five_hour"},
		},
	}
	raw, _ := json.Marshal(body)
	e := FromUpstream(http.StatusTooManyRequests, raw)
	// The reset in ms should be converted to seconds: delta ~3600s.
	if e.RetryAfterSeconds < 3500 || e.RetryAfterSeconds > 3700 {
		t.Errorf("RetryAfterSeconds = %d, want ~3600 (ms treated as ms)", e.RetryAfterSeconds)
	}
}

// TestCodeless429GetsTransientRateLimit pins Finding 7: plain 429 without JSON
// or without a code gets transient_rate_limit WireCode.
func TestCodeless429GetsTransientRateLimit(t *testing.T) {
	cases := []struct {
		name string
		body []byte
	}{
		{"non-JSON body", []byte("Rate limit exceeded")},
		{"JSON without code", []byte(`{"error": {"message": "too many requests"}}`)},
		{"empty body", []byte{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := FromUpstream(http.StatusTooManyRequests, tc.body)
			if e.WireCode != pluginabi.ErrorCodeTransientRateLimit {
				t.Errorf("WireCode = %q, want %q", e.WireCode, pluginabi.ErrorCodeTransientRateLimit)
			}
		})
	}
}

// TestWindowCheckedBeforeModelNotFound pins Finding 8: window/credit exhaustion
// takes precedence over model_not_found even when message says "upgrade your plan".
func TestWindowCheckedBeforeModelNotFound(t *testing.T) {
	body := []byte(`{"error": {"code": "RATE_LIMITED", "message": "usage limit exceeded. upgrade your plan for more", "rateLimit": {"window": "weekly", "reset": 1234567890}}}`)
	e := FromUpstream(http.StatusTooManyRequests, body)
	if e.WireCode != pluginabi.ErrorCodeCredentialQuota {
		t.Errorf("WireCode = %q, want credential_quota (window exhaustion beats upgrade wording)", e.WireCode)
	}
	if !e.CredentialScoped {
		t.Error("CredentialScoped = false, want true")
	}
}

// TestRequestDefectTightened pins Finding 9: bare "max_tokens" in rate-limit
// 429 is NOT a request defect.
func TestRequestDefectTightened(t *testing.T) {
	rateLimitBody := []byte(`{"error": {"message": "rate limit exceeded for max_tokens per minute"}}`)
	e := FromUpstream(http.StatusTooManyRequests, rateLimitBody)
	if e.WireCode == pluginabi.ErrorCodeRequestScoped {
		t.Errorf("WireCode = request_scoped for rate-limit 429 mentioning max_tokens, should be transient_rate_limit")
	}
	// Actual prompt-too-long defect stays request_scoped.
	defectBody := []byte(`{"error": {"message": "prompt exceeds max_tokens: 128000"}}`)
	e2 := FromUpstream(http.StatusBadRequest, defectBody)
	if e2.WireCode != pluginabi.ErrorCodeRequestScoped {
		t.Errorf("WireCode = %q, want request_scoped for genuine defect", e2.WireCode)
	}
}
