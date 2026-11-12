package plugin

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// raiseChatError scripts the mock so every chat-completions call fails with the
// given status/body/Retry-After, leaving the catalog and other routes intact.
func raiseChatError(m *mockCommandCode, status int, body, retryAfter string) {
	m.setChatPlan(func(int) (int, string, string) { return status, retryAfter, body })
}

// The full executor path must forward the vendor's usage-window refusal as a
// credential-scoped quota error with the upstream reset hint — that is what
// makes CPA apply its own configured cooling policy instead of guessing.
func TestExecuteUsageWindowExhaustionIsCredentialScoped(t *testing.T) {
	m, _, st, _ := newIntegrationManager(t)
	reset := int64(4102444800) // far-future but still inside the accepted bound
	raiseChatError(st, http.StatusTooManyRequests,
		`{"code":"RATE_LIMITED","error":{"rateLimit":{"window":"fiveHour","reset":`+
			itoa(reset)+`}},"message":"You have hit the 5-hour usage limit for your plan."}`, "")

	env := mustExecute(t, m, "commandcode/glm-5.2", "openai", []byte(ccRequestBody))
	if env.OK || env.Error == nil {
		t.Fatalf("envelope = %+v", env)
	}
	if env.Error.Code != pluginabi.ErrorCodeCredentialQuota {
		t.Errorf("code = %q, want %q", env.Error.Code, pluginabi.ErrorCodeCredentialQuota)
	}
	if !env.Error.CredentialScoped {
		t.Error("credential_scoped = false; the whole account is out of its usage window")
	}
	if env.Error.HTTPStatus != http.StatusTooManyRequests {
		t.Errorf("http_status = %d", env.Error.HTTPStatus)
	}
}

// A plain 429 cools only the reporting model, so CPA keeps serving the rest of
// the credential's catalog.
func TestExecutePlain429IsModelScoped(t *testing.T) {
	m, _, st, _ := newIntegrationManager(t)
	raiseChatError(st, http.StatusTooManyRequests, `{"error":{"message":"Too many requests, slow down"}}`, "")

	env := mustExecute(t, m, "commandcode/glm-5.2", "openai", []byte(ccRequestBody))
	if env.OK || env.Error == nil {
		t.Fatalf("envelope = %+v", env)
	}
	if env.Error.Code != pluginabi.ErrorCodeTransientRateLimit {
		t.Errorf("code = %q, want %q", env.Error.Code, pluginabi.ErrorCodeTransientRateLimit)
	}
	if env.Error.CredentialScoped {
		t.Error("a plain 429 must not be reported as credential-wide")
	}
}

// A Retry-After header survives the executor path so the host can honour the
// upstream's own deadline.
func TestExecuteForwardsRetryAfterHeader(t *testing.T) {
	m, _, st, _ := newIntegrationManager(t)
	raiseChatError(st, http.StatusTooManyRequests, `{"error":{"message":"busy"}}`, "42")

	env := mustExecute(t, m, "commandcode/glm-5.2", "openai", []byte(ccRequestBody))
	if env.Error == nil || env.Error.RetryAfterSeconds != 42 {
		t.Fatalf("retry_after_seconds = %+v, want 42", env.Error)
	}
}

// A failure after the first downstream byte must close with structured detail
// carrying stream_truncated, so CPA neither cools the credential nor replays.
func TestStreamPostFirstByteFailureCarriesStructuredDetail(t *testing.T) {
	m, f, st, _ := newIntegrationManager(t)
	st.setChatFrames([]string{
		`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}` + "\n\n",
		`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n",
	})
	// The upstream aborts after those frames: the downstream close must explain
	// why without condemning the credential.
	st.setChatTruncate(true)

	if _, err := m.HandleCall("executor.execute_stream",
		execStreamReqBodyForKey("commandcode/glm-5.2", "openai",
			[]byte(`{"model":"commandcode/glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":true}`),
			"down-trunc", "sk-test-1")); err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	m.bridge.WaitForInFlight(5 * time.Second)

	closes := f.callsOf(pluginabi.MethodHostStreamClose)
	if len(closes) != 1 {
		t.Fatalf("downstream closes = %d, want 1", len(closes))
	}
	var wire struct {
		ErrorDetail *pluginabi.Error `json:"error_detail"`
	}
	if err := json.Unmarshal(closes[0].payload, &wire); err != nil {
		t.Fatalf("decode close payload: %v", err)
	}
	if wire.ErrorDetail == nil {
		t.Fatalf("close carried no structured error_detail: %s", closes[0].payload)
	}
	if wire.ErrorDetail.Code != pluginabi.ErrorCodeStreamTruncated {
		t.Errorf("detail code = %q, want %q", wire.ErrorDetail.Code, pluginabi.ErrorCodeStreamTruncated)
	}
	if wire.ErrorDetail.Retryable {
		t.Error("a truncated stream must not be retryable")
	}
}

// itoa keeps the JSON literal readable without importing strconv twice.
func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var digits [20]byte
	i := len(digits)
	for v > 0 {
		i--
		digits[i] = byte('0' + v%10)
		v /= 10
	}
	return string(digits[i:])
}
