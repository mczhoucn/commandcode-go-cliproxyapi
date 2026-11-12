package errclass

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// envelopeClass renders e the way the host sees it.
func envelopeClass(e *Error) pluginabi.Error { return ToEnvelopeError(e) }

// The vendor CLI (command-code 1.88.5) treats `code: RATE_LIMITED` plus
// error.rateLimit.window as a usage-window exhaustion. Every model shares the
// window, so CPA must cool the whole credential — not just one model.
func TestUpstreamRateLimitedCoolsCredential(t *testing.T) {
	reset := float64(time.Now().Unix() + 1800)
	body, _ := json.Marshal(map[string]any{
		"code":    "RATE_LIMITED",
		"error":   map[string]any{"rateLimit": map[string]any{"window": "fiveHour", "reset": reset}},
		"message": "You have hit the 5-hour usage limit for your plan.",
	})
	wire := envelopeClass(FromUpstream(http.StatusTooManyRequests, body))

	if wire.Code != pluginabi.ErrorCodeCredentialQuota {
		t.Errorf("code = %q, want %q", wire.Code, pluginabi.ErrorCodeCredentialQuota)
	}
	if !wire.CredentialScoped {
		t.Error("credential_scoped = false; a shared usage window must cool the credential")
	}
	if wire.RetryAfterSeconds < 1795 || wire.RetryAfterSeconds > 1805 {
		t.Errorf("retry_after_seconds = %d, want ~1800 from the reset instant", wire.RetryAfterSeconds)
	}
	if !strings.Contains(wire.Message, "5-hour") {
		t.Errorf("message = %q, want the exhausted window named", wire.Message)
	}
}

// The weekly window uses the same shape with a different name.
func TestUpstreamWeeklyWindowMessage(t *testing.T) {
	body := []byte(`{"code":"RATE_LIMITED","error":{"rateLimit":{"window":"weekly","reset":9999999999}}}`)
	wire := envelopeClass(FromUpstream(http.StatusTooManyRequests, body))
	if !wire.CredentialScoped || !strings.Contains(wire.Message, "weekly") {
		t.Fatalf("envelope = %+v", wire)
	}
}

// A 429 with no window detail is a per-model admission limit: cool only that
// model and do not record quota exhaustion.
func TestUpstreamPlain429IsTransientModelLimit(t *testing.T) {
	wire := envelopeClass(FromUpstream(http.StatusTooManyRequests, []byte(`{"error":{"message":"Too many requests"}}`)))
	if wire.Code != pluginabi.ErrorCodeTransientRateLimit {
		t.Errorf("code = %q, want %q", wire.Code, pluginabi.ErrorCodeTransientRateLimit)
	}
	if wire.CredentialScoped {
		t.Error("a plain 429 must not condemn the whole credential")
	}
}

// Spend cap and credit exhaustion are account-wide.
func TestUpstreamAccountExhaustion(t *testing.T) {
	for _, body := range []string{
		`{"code":"USAGE_EXCEEDED","message":"Spend limit reached"}`,
		`{"error":{"message":"insufficient credits"}}`,
		`{"message":"premium_credits_exhausted"}`,
	} {
		wire := envelopeClass(FromUpstream(http.StatusForbidden, []byte(body)))
		if !wire.CredentialScoped {
			t.Errorf("%s: credential_scoped = false, want true", body)
		}
		if wire.Code != pluginabi.ErrorCodeCredentialQuota {
			t.Errorf("%s: code = %q", body, wire.Code)
		}
	}
}

// A model outside the plan is model-scoped: CPA cools only it.
func TestUpstreamModelNotInPlan(t *testing.T) {
	for _, body := range []string{
		`{"error":{"message":"model_not_in_plan"}}`,
		`{"error":{"message":"This model is not available in your plan"}}`,
		`{"error":{"message":"Model \"qwen3\" is not supported on this endpoint"}}`,
	} {
		wire := envelopeClass(FromUpstream(http.StatusNotFound, []byte(body)))
		if wire.Code != pluginabi.ErrorCodeModelNotFound {
			t.Errorf("%s: code = %q, want model_not_found", body, wire.Code)
		}
		if wire.CredentialScoped {
			t.Error("a plan-entitlement failure must not cool the whole credential")
		}
	}
}

// Client-caused failures skip cooldown so one bad payload cannot black out a
// healthy account.
func TestUpstreamRequestDefectSkipsCooldown(t *testing.T) {
	wire := envelopeClass(FromUpstream(http.StatusBadRequest, []byte(`{"error":{"message":"prompt is too long"}}`)))
	if wire.Code != pluginabi.ErrorCodeRequestScoped {
		t.Errorf("code = %q, want request_scoped", wire.Code)
	}
	if wire.Retryable {
		t.Error("a request defect must not be retryable")
	}
}

// A standard Retry-After header is the upstream's own hint and must survive.
func TestUpstreamRetryAfterHeader(t *testing.T) {
	headers := http.Header{"Retry-After": []string{"120"}}
	wire := envelopeClass(FromUpstreamHeaders(http.StatusServiceUnavailable, []byte(`{"error":{"message":"overloaded"}}`), headers))
	if wire.RetryAfterSeconds != 120 {
		t.Errorf("retry_after_seconds = %d, want 120", wire.RetryAfterSeconds)
	}

	dateHeaders := http.Header{"Retry-After": []string{time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat)}}
	if got := FromUpstreamHeaders(http.StatusServiceUnavailable, nil, dateHeaders).RetryAfterSeconds; got < 85 || got > 95 {
		t.Errorf("HTTP-date retry-after = %d, want ~90", got)
	}
}

// A bogus or absurdly large hint is CLAMPED to maxRetryAfterSeconds rather
// than silently dropped: a present-but-huge Retry-After is still more useful
// as a bounded hint than as zero. Zero and negative values remain rejected.
func TestUpstreamRetryAfterClampsLargeValues(t *testing.T) {
	// Very large positive value: clamped to cap, not rejected.
	large := fmt.Sprintf("%d", maxRetryAfterSeconds+1)
	e := FromUpstreamHeaders(http.StatusServiceUnavailable, nil, http.Header{"Retry-After": []string{large}})
	if e.RetryAfterSeconds != maxRetryAfterSeconds {
		t.Errorf("Retry-After %q -> %d, want clamped to %d", large, e.RetryAfterSeconds, maxRetryAfterSeconds)
	}
	// Zero and negative are still rejected.
	for _, raw := range []string{"0", "-5", "not-a-date"} {
		e := FromUpstreamHeaders(http.StatusServiceUnavailable, nil, http.Header{"Retry-After": []string{raw}})
		if e.RetryAfterSeconds != 0 {
			t.Errorf("Retry-After %q -> %d, want rejected (zero)", raw, e.RetryAfterSeconds)
		}
	}
}

// Non-JSON bodies keep the previous status-only classification.
func TestUpstreamNonJSONFallsBackToStatus(t *testing.T) {
	e := FromUpstream(http.StatusTooManyRequests, []byte("Bearer sk-secret-123 quota exhausted"))
	if e.Class != ClassQuota {
		t.Errorf("class = %q, want quota (the historical heuristic)", e.Class)
	}
	if strings.Contains(e.Message, "sk-secret-123") {
		t.Errorf("message leaked the key: %q", e.Message)
	}
	if e.CredentialScoped {
		t.Error("an unstructured 429 must not be assumed credential-wide")
	}
}

// Post-first-byte failures must never be retried or cooled.
func TestPostFirstByteFailuresSkipCooldown(t *testing.T) {
	truncated := envelopeClass(PostFirstByteTruncated("stream died"))
	if truncated.Code != pluginabi.ErrorCodeStreamTruncated || truncated.Retryable {
		t.Errorf("truncated = %+v", truncated)
	}
	lifecycle := envelopeClass(PostFirstByteConnectionLifecycle("client left"))
	if lifecycle.Code != pluginabi.ErrorCodeConnectionLifecycle || lifecycle.Retryable {
		t.Errorf("lifecycle = %+v", lifecycle)
	}
}

// Transport failures stay retryable without cooling the credential.
func TestNetworkFailureIsTransientTransport(t *testing.T) {
	wire := envelopeClass(FromNetwork(errString("connection refused")))
	if wire.Code != pluginabi.ErrorCodeTransientTransport {
		t.Errorf("code = %q, want %q", wire.Code, pluginabi.ErrorCodeTransientTransport)
	}
	if !wire.Retryable || wire.CredentialScoped {
		t.Errorf("envelope = %+v; want retryable and not credential-scoped", wire)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// EnvelopeCode falls back to Class when no host-recognized code applies.
func TestEnvelopeCodeFallsBackToClass(t *testing.T) {
	if got := (&Error{Class: ClassTranslation}).EnvelopeCode(); got != string(ClassTranslation) {
		t.Errorf("EnvelopeCode = %q", got)
	}
	if got := (&Error{Class: ClassTranslation, WireCode: "x"}).EnvelopeCode(); got != "x" {
		t.Errorf("EnvelopeCode = %q, want the wire override", got)
	}
}
