// CommandCode upstream error classification for CPA's cooldown pipeline.
//
// The shapes below were recovered from the vendor CLI's own error handling
// (command-code 1.88.5, dist/cli.mjs): parseWindowLimitError, classifyKind,
// parseSpendCapError, and the RATE_LIMITED / USAGE_EXCEEDED code constants.
// They are the authoritative source this plugin classifies against — the
// plugin does not invent cooldown policy, it only reports what upstream said.
//
// CPA decides cooldown scope and duration from pluginabi.Error:
//   CredentialScoped      -> cool the whole credential (reason "credential_quota")
//   Code transient_rate_limit -> cool only this model until RetryAfterSeconds
//   Code request_scoped   -> skip cooldown and stop credential rotation
//   Code model_not_found  -> cool only this model as unsupported
//   Code stream_truncated -> skip cooldown, never replay
//
// Everything here is bounded by snippetBound at the call site.

package errclass

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// PostFirstByteTruncated classifies an upstream stream that failed after the
// first byte reached the client. CPA skips cooldown and never replays it: the
// generation was already submitted upstream, so a retry risks duplicate
// delivery. Exposed as pluginabi.ErrorCodeStreamTruncated.
func PostFirstByteTruncated(msg string) *Error {
	return &Error{
		Class:      ClassNetwork,
		Message:    Redact(msg),
		StatusCode: http.StatusBadGateway,
		Retryable:  false,
		WireCode:   pluginabi.ErrorCodeStreamTruncated,
	}
}

// PostFirstByteConnectionLifecycle classifies a downstream write failure — the
// client disconnected or the sink stopped draining. It says nothing about
// upstream health, so CPA must not cool the credential for it.
func PostFirstByteConnectionLifecycle(msg string) *Error {
	return &Error{
		Class:     ClassNetwork,
		Message:   Redact(msg),
		Retryable: false,
		WireCode:  pluginabi.ErrorCodeConnectionLifecycle,
	}
}

// WindowMessage names the exhausted usage window for a credential-scoped 429.
func WindowMessage(window, fallback string) string { return windowMessage(window, fallback) }

// maxRetryAfterSeconds bounds an upstream hint so a bogus value cannot pin a
// credential out of rotation for years. 24h is far beyond any CommandCode
// window (the longest is weekly) and still finite.
const maxRetryAfterSeconds = 24 * 60 * 60

// ccErrorBody is the tolerant decode of a CommandCode error envelope. The
// vendor CLI reads `code` from the outer object or the nested error, and the
// rate-limit detail from the nested error.
type ccErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Error   *struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RateLimit *struct {
			// Window is one of fiveHour | weekly | daily.
			Window string `json:"window"`
			// Reset is a Unix timestamp in SECONDS (the CLI multiplies by 1000).
			Reset *float64 `json:"reset"`
			Model string   `json:"model"`
		} `json:"rateLimit"`
	} `json:"error"`
}

// FromUpstream classifies one upstream >=400 response using both the status
// and the body. Body shapes are parsed tolerantly: a body that is not the
// vendor's JSON error object (plain text, an HTML page, a proxy error) falls
// back to the historical status-only classification with the body text as the
// message, so non-JSON failures keep their previous wording and class.
func FromUpstream(status int, body []byte) *Error {
	var parsed ccErrorBody
	jsonOK := json.Unmarshal(body, &parsed) == nil

	code := strings.TrimSpace(parsed.Code)
	if code == "" && parsed.Error != nil {
		code = strings.TrimSpace(parsed.Error.Code)
	}
	message := strings.TrimSpace(parsed.Message)
	if message == "" && parsed.Error != nil {
		message = strings.TrimSpace(parsed.Error.Message)
	}
	var rateLimit *struct {
		Window string   `json:"window"`
		Reset  *float64 `json:"reset"`
		Model  string   `json:"model"`
	}
	if parsed.Error != nil {
		rateLimit = parsed.Error.RateLimit
	}
	if !jsonOK || (code == "" && message == "") {
		// Not the vendor's structured error: classify on status plus the raw
		// (bounded) body, exactly as before this file existed.
		return FromStatus(status, string(body))
	}

	redacted := Redact(message)
	lower := strings.ToLower(code + " " + redacted)
	base := FromStatus(status, redacted)

	// Usage-window exhaustion: CommandCode reports a named window plus the
	// instant it resets. It is credential-wide — every model shares the window
	// — so CPA must cool the credential, not one model.
	if status == http.StatusTooManyRequests && (strings.EqualFold(code, "RATE_LIMITED") || isWindowExhaustion(lower)) {
		out := &Error{
			Class:            ClassQuota,
			Message:          redacted,
			StatusCode:       status,
			Retryable:        true,
			CredentialScoped: true,
			WireCode:         pluginabi.ErrorCodeCredentialQuota,
		}
		if rateLimit != nil {
			out.RetryAfterSeconds = retryAfterFromReset(rateLimit.Reset)
			out.Message = windowMessage(rateLimit.Window, redacted)
		}
		return out
	}

	// Spend cap / insufficient credits: the account cannot serve anything.
	if isAccountExhausted(lower) || strings.EqualFold(code, "USAGE_EXCEEDED") {
		return &Error{
			Class:            ClassBilling,
			Message:          redacted,
			StatusCode:       status,
			Retryable:        true,
			CredentialScoped: true,
			WireCode:         pluginabi.ErrorCodeCredentialQuota,
		}
	}

	// The model is not part of this plan: cool only it, and let the host pick
	// another model/credential instead of benching the account.
	if isModelNotInPlan(lower) {
		return &Error{
			Class:      ClassInvalidModel,
			Message:    redacted,
			StatusCode: status,
			Retryable:  true,
			WireCode:   pluginabi.ErrorCodeModelNotFound,
		}
	}

	// A request the upstream will never accept on any credential: skipping
	// cooldown keeps a bad client payload from blacking out a healthy account.
	if isRequestDefect(lower) {
		return &Error{
			Class:      ClassUnsupported,
			Message:    redacted,
			StatusCode: status,
			Retryable:  false,
			WireCode:   pluginabi.ErrorCodeRequestScoped,
		}
	}

	// Remaining 429s are per-model admission/capacity limits: cool only the
	// reporting model until the hint (if any), without recording exhaustion.
	if status == http.StatusTooManyRequests {
		base.WireCode = pluginabi.ErrorCodeTransientRateLimit
		if rateLimit != nil {
			base.RetryAfterSeconds = retryAfterFromReset(rateLimit.Reset)
		}
		return base
	}
	return base
}

// FromUpstreamHeaders is FromUpstream plus a standard Retry-After header. The
// header wins when present because it is the upstream's own HTTP-level hint.
func FromUpstreamHeaders(status int, body []byte, headers http.Header) *Error {
	e := FromUpstream(status, body)
	if e == nil || headers == nil {
		return e
	}
	if secs, ok := parseRetryAfterHeader(headers.Get("Retry-After")); ok {
		e.RetryAfterSeconds = secs
	}
	return e
}

// parseRetryAfterHeader reads both Retry-After forms: delta-seconds and an
// HTTP-date. Values outside (0, maxRetryAfterSeconds] are rejected rather than
// clamped, since a zero or absurd hint tells us nothing trustworthy.
func parseRetryAfterHeader(raw string) (int, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(raw); err == nil {
		if secs <= 0 || secs > maxRetryAfterSeconds {
			return 0, false
		}
		return secs, true
	}
	if when, err := http.ParseTime(raw); err == nil {
		delta := int(time.Until(when).Seconds())
		if delta <= 0 || delta > maxRetryAfterSeconds {
			return 0, false
		}
		return delta, true
	}
	return 0, false
}

// retryAfterFromReset converts CommandCode's SECONDS-based reset instant into
// the remaining-seconds hint CPA expects.
func retryAfterFromReset(reset *float64) int {
	if reset == nil || *reset <= 0 {
		return 0
	}
	delta := *reset - float64(time.Now().Unix())
	if math.IsNaN(delta) || delta <= 0 {
		return 0
	}
	if delta > maxRetryAfterSeconds {
		return maxRetryAfterSeconds
	}
	return int(math.Ceil(delta))
}

// windowMessage names the exhausted window so the credential card and client
// error say which limit was hit rather than a generic "rate limited".
func windowMessage(window, fallback string) string {
	switch strings.ToLower(strings.TrimSpace(window)) {
	case "fivehour":
		return "5-hour usage window exhausted" + suffix(fallback)
	case "weekly":
		return "weekly usage window exhausted" + suffix(fallback)
	case "daily":
		return "daily usage window exhausted" + suffix(fallback)
	}
	if fallback == "" {
		return "usage window exhausted"
	}
	return fallback
}

func suffix(rest string) string {
	if strings.TrimSpace(rest) == "" {
		return ""
	}
	return ": " + rest
}

// isWindowExhaustion matches the CLI's usage-limit vocabulary.
func isWindowExhaustion(lowered string) bool {
	switch {
	case strings.Contains(lowered, "window_limit"),
		strings.Contains(lowered, "usage window"),
		strings.Contains(lowered, "usage limit for your plan"),
		strings.Contains(lowered, "5-hour"),
		strings.Contains(lowered, "weekly limit"):
		return true
	}
	return false
}

// isAccountExhausted matches credit/billing exhaustion wording.
func isAccountExhausted(lowered string) bool {
	switch {
	case strings.Contains(lowered, "premium_credits_exhausted"),
		strings.Contains(lowered, "insufficient credits"),
		strings.Contains(lowered, "insufficient balance"),
		strings.Contains(lowered, "out of credit"):
		return true
	}
	return false
}

// isModelNotInPlan matches the CLI's plan-entitlement vocabulary.
func isModelNotInPlan(lowered string) bool {
	if strings.Contains(lowered, "model_not_in_plan") {
		return true
	}
	if strings.Contains(lowered, "upgrade your plan") {
		return true
	}
	if strings.Contains(lowered, "not supported on this endpoint") {
		// CommandCode rejects OSS ids on /messages with this wording; the model
		// exists but not on that route, which is still model-scoped.
		return true
	}
	// "not available in your plan" / "not included in your plan"
	return strings.Contains(lowered, "not available") && strings.Contains(lowered, "plan") ||
		strings.Contains(lowered, "not included") && strings.Contains(lowered, "plan")
}

// isRequestDefect matches failures caused by the request body itself.
func isRequestDefect(lowered string) bool {
	switch {
	case strings.Contains(lowered, "prompt is too long"),
		strings.Contains(lowered, "context length"),
		strings.Contains(lowered, "maximum number of tokens"),
		strings.Contains(lowered, "max_tokens"):
		return true
	}
	return false
}
