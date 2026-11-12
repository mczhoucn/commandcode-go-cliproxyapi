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
// IMPORTANT: the host's credential-wide cooldown path (conductor_cooldown.go
// ~1450-1900, auth.Quota.Exceeded reason "credential_quota") is ONLY
// reachable when the envelope HTTP status is 429 AND CredentialScoped is set.
// A 402/403 with CredentialScoped merely cools one model, not the credential.
// Therefore every credential-scoped outcome in this file reports StatusCode 429,
// regardless of the upstream's actual status code.
//
// Everything here is bounded by snippetBound at the call site.

package errclass

import (
	"encoding/json"
	"math"
	"net/http"
	"regexp"
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
// credential out of rotation for years. 32 days covers the longest possible
// reset window: a monthly billing period can span up to ~31 days, and we
// allow one extra day of margin. Values above this cap are clamped (not
// rejected) so a legitimate long reset still produces a useful hint rather
// than silently zeroing the field.
const maxRetryAfterSeconds = 32 * 24 * 60 * 60

// ccRawError is the tolerantly decoded CommandCode error envelope. Fields use
// json.RawMessage so the decoder never fails on type drift: code/reset can
// arrive as strings, numbers, or ISO8601 timestamps depending on the upstream
// version and error path.
type ccRawError struct {
	// top-level fields
	Code    json.RawMessage `json:"code"`
	Message json.RawMessage `json:"message"`
	// nested error object
	Error json.RawMessage `json:"error"`
}

// ccErrorBody is the parsed, normalised form after tolerant decoding.
type ccErrorBody struct {
	Code    string
	Message string
	// RateLimit is non-nil when the nested error carried a rateLimit object.
	RateLimit *ccRateLimit
}

type ccRateLimit struct {
	Window string
	// Reset is a Unix timestamp in SECONDS (vendor CLI multiplies by 1000).
	// Tolerantly decoded: may arrive as number, numeric string, or RFC3339.
	Reset *float64
	Model string
}

// parseUpstreamBody decodes a CommandCode error body tolerantly. It returns
// (body, true) on success; (zero, false) if the input is not a JSON object or
// carries neither a code nor a message. The raw nested-error object can carry
// code/message/rateLimit in any type combination the upstream emits.
func parseUpstreamBody(raw []byte) (ccErrorBody, bool) {
	var outer ccRawError
	if err := json.Unmarshal(raw, &outer); err != nil {
		return ccErrorBody{}, false
	}

	out := ccErrorBody{}

	// code: accept string "RATE_LIMITED" or numeric 429-style code
	out.Code = extractStringOrNumber(outer.Code)
	out.Message = extractStringOrNumber(outer.Message)

	// nested error object can be {"code":..,"message":..,"rateLimit":{..}}
	// or bare {"type":"error","error":{..}} shape
	if outer.Error != nil {
		var nested struct {
			Code      json.RawMessage `json:"code"`
			Message   json.RawMessage `json:"message"`
			RateLimit json.RawMessage `json:"rateLimit"`
			// Anthropic-Messages-style wrapper
			Type  json.RawMessage `json:"type"`
			Inner json.RawMessage `json:"error"`
		}
		if json.Unmarshal(outer.Error, &nested) == nil {
			if out.Code == "" {
				out.Code = extractStringOrNumber(nested.Code)
			}
			if out.Message == "" {
				out.Message = extractStringOrNumber(nested.Message)
			}
			// Try rateLimit; also try inner error if the wrapper is Anthropic-style
			if nested.RateLimit != nil {
				out.RateLimit = parseRateLimit(nested.RateLimit)
			}
			if out.RateLimit == nil && nested.Inner != nil {
				// {"type":"error","error":{"code":..,"message":..,"rateLimit":..}}
				var inner2 struct {
					Code      json.RawMessage `json:"code"`
					Message   json.RawMessage `json:"message"`
					RateLimit json.RawMessage `json:"rateLimit"`
				}
				if json.Unmarshal(nested.Inner, &inner2) == nil {
					if out.Code == "" {
						out.Code = extractStringOrNumber(inner2.Code)
					}
					if out.Message == "" {
						out.Message = extractStringOrNumber(inner2.Message)
					}
					if inner2.RateLimit != nil {
						out.RateLimit = parseRateLimit(inner2.RateLimit)
					}
				}
			}
		}
	}

	// A body with rateLimit present is valid even without code/message, per
	// Finding 3 (window exhaustion detection must honor rateLimit presence).
	if rateLimitObjectPresent(out.RateLimit) {
		return out, true
	}
	if out.Code == "" && out.Message == "" {
		return ccErrorBody{}, false
	}
	return out, true
}

// parseRateLimit decodes a rateLimit object tolerantly.
func parseRateLimit(raw json.RawMessage) *ccRateLimit {
	var rl struct {
		Window json.RawMessage `json:"window"`
		Reset  json.RawMessage `json:"reset"`
		Model  json.RawMessage `json:"model"`
	}
	if json.Unmarshal(raw, &rl) != nil {
		return nil
	}
	out := &ccRateLimit{
		Window: extractStringOrNumber(rl.Window),
		Model:  extractStringOrNumber(rl.Model),
	}
	if rl.Reset != nil {
		if r := extractResetSeconds(rl.Reset); r > 0 {
			out.Reset = &r
		}
	}
	return out
}

// extractStringOrNumber returns the string content of a JSON string or number
// token; returns "" for null/boolean/object/array/nil.
func extractStringOrNumber(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	// JSON string: unquote
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	// JSON number: return as text
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return strings.TrimSpace(n.String())
	}
	return ""
}

// extractResetSeconds parses a reset field that can be:
//   - a JSON number (Unix seconds or milliseconds)
//   - a numeric JSON string (same)
//   - an RFC3339/ISO8601 string ("2024-06-01T12:00:00Z")
//
// Returns the value in SECONDS as a float64 (> 0 means valid).
// Values > 1e11 are treated as milliseconds (ms epoch ~year 5138, so any
// value >1e11 today is ms-domain).
func extractResetSeconds(raw json.RawMessage) float64 {
	str := extractStringOrNumber(raw)
	if str == "" {
		return 0
	}

	// Try numeric first
	if f, err := strconv.ParseFloat(str, 64); err == nil && f > 0 {
		return normalizeResetEpoch(f)
	}

	// Try RFC3339/ISO8601 string
	for _, layout := range []string{
		time.RFC3339,
		time.RFC3339Nano,
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02T15:04:05.999999999Z07:00",
	} {
		if t, err := time.Parse(layout, str); err == nil {
			return float64(t.Unix())
		}
	}
	return 0
}

// normalizeResetEpoch converts a numeric reset value to Unix seconds.
// Values > 1e11 are milliseconds (1e11 ms = year ~1973 epoch, so any value
// above this today is unambiguously milliseconds).
func normalizeResetEpoch(v float64) float64 {
	if v > 1e11 {
		return v / 1000.0
	}
	return v
}

// FromUpstream classifies one upstream >=400 response using both the status
// and the body. Body shapes are parsed tolerantly: a body that is not the
// vendor's JSON error object (plain text, an HTML page, a proxy error) falls
// back to the historical status-only classification with the body text as the
// message, so non-JSON failures keep their previous wording and class.
//
// Adapters also call this for in-stream SSE error objects, so both
//   {"error":{...}}                   (vendor envelope)
//   {"code":..,"message":..}          (bare error object)
//   {"type":"error","error":{...}}    (Anthropic Messages wrapper)
// shapes are accepted.
func FromUpstream(status int, body []byte) *Error {
	parsed, jsonOK := parseUpstreamBody(body)
	if !jsonOK {
		// Not the vendor's structured error: classify on status plus the raw
		// (bounded) body, exactly as before this file existed. For a 429
		// without a parseable body we still want transient_rate_limit so the
		// model cools without the credential being benched.
		e := FromStatus(status, string(body))
		if status == http.StatusTooManyRequests && e.WireCode == "" {
			e.WireCode = pluginabi.ErrorCodeTransientRateLimit
		}
		return e
	}

	code := strings.TrimSpace(parsed.Code)
	message := strings.TrimSpace(parsed.Message)
	rl := parsed.RateLimit

	redacted := Redact(message)
	lower := strings.ToLower(code + " " + redacted)
	base := FromStatus(status, redacted)

	// --- Ordering note -------------------------------------------------------
	// Window/credit exhaustion is checked BEFORE model-plan and request-defect
	// guards: a 429 "usage limit … upgrade your plan" must become
	// credential_quota, not model_not_found; and a 429 mentioning "max_tokens
	// per minute" must become transient_rate_limit, not request_scoped.
	// -------------------------------------------------------------------------

	// Usage-window exhaustion: a rateLimit object with any non-trivial field
	// is the canonical signal (regardless of HTTP status or code), because the
	// vendor wraps it only for window-exhaustion events. RATE_LIMITED is also
	// accepted at non-429 status (vendor parseWindowLimitError checks code
	// first, then falls through to the status check).
	//
	// Credential-scoped because every model shares the usage window.
	// StatusCode is forced to 429: the host's credential-wide cooldown path
	// (conductor_cooldown.go) is only reachable when HTTP status == 429.
	isWindow := rateLimitObjectPresent(rl) ||
		strings.EqualFold(code, "RATE_LIMITED") ||
		isWindowExhaustion(lower)
	if isWindow {
		out := &Error{
			Class:            ClassQuota,
			Message:          redacted,
			StatusCode:       http.StatusTooManyRequests, // forced 429 — see package doc
			Retryable:        true,
			CredentialScoped: true,
			WireCode:         pluginabi.ErrorCodeCredentialQuota,
		}
		if rl != nil {
			out.RetryAfterSeconds = retryAfterFromReset(rl.Reset)
			if rl.Window != "" {
				out.Message = windowMessage(rl.Window, redacted)
			} else if out.RetryAfterSeconds > 0 {
				// reset hint but no window name — keep message, add retry
				out.Message = redacted
			}
		}
		// If no explicit window name, try to extract it from the message text
		// so "billing period" / "monthly usage" wording names the window.
		if rl == nil || rl.Window == "" {
			if extracted := extractWindowFromMessage(message); extracted != "" {
				out.Message = windowMessage(extracted, redacted)
			}
		}
		// Also try to extract reset from the message text if no reset in rl.
		if out.RetryAfterSeconds == 0 {
			out.RetryAfterSeconds = retryAfterFromMessageText(message)
		}
		// A window exhaustion with neither a window name nor an upstream message
		// still needs a non-empty, meaningful message for the client envelope.
		if strings.TrimSpace(out.Message) == "" {
			out.Message = "usage window exhausted"
		}
		return out
	}

	// Spend cap / credit exhaustion: account cannot serve anything.
	// StatusCode forced to 429 — same reasoning as window exhaustion above.
	// Guard: 401 is a genuine auth failure, not quota; 403 with RATE_LIMITED
	// was already handled above. We only flip 402/403 for account-exhaustion
	// wording or USAGE_EXCEEDED; 401 never becomes credential_quota.
	isExhausted := isAccountExhausted(lower) ||
		strings.EqualFold(code, "USAGE_EXCEEDED") ||
		isSpendCap(lower) ||
		(status == http.StatusForbidden && strings.EqualFold(code, "RATE_LIMITED"))
	if isExhausted && status != http.StatusUnauthorized {
		return &Error{
			Class:            ClassBilling,
			Message:          redacted,
			StatusCode:       http.StatusTooManyRequests, // forced 429 — see package doc
			Retryable:        true,
			CredentialScoped: true,
			WireCode:         pluginabi.ErrorCodeCredentialQuota,
		}
	}

	// The model is not part of this plan: cool only it. This check runs AFTER
	// window/exhaustion so "usage limit ... upgrade your plan" on a 429 goes
	// to credential_quota, not model_not_found.
	if isModelNotInPlan(lower) {
		return &Error{
			Class:      ClassInvalidModel,
			Message:    redacted,
			StatusCode: status,
			Retryable:  true,
			WireCode:   pluginabi.ErrorCodeModelNotFound,
		}
	}

	// A request the upstream will never accept on any credential. This check
	// runs AFTER window/exhaustion so a 429 mentioning token counts stays a
	// rate-limit rather than a request defect.
	if isRequestDefect(status, lower) {
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
		if rl != nil {
			base.RetryAfterSeconds = retryAfterFromReset(rl.Reset)
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
// HTTP-date. Values outside (0, maxRetryAfterSeconds] are CLAMPED to the cap
// rather than rejected: an absurdly large hint from upstream is clamped to the
// maximum allowed window rather than silently dropped, so callers always get
// a useful (if bounded) hint when the header is present.
func parseRetryAfterHeader(raw string) (int, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(raw); err == nil {
		if secs <= 0 {
			return 0, false
		}
		if secs > maxRetryAfterSeconds {
			return maxRetryAfterSeconds, true
		}
		return secs, true
	}
	if when, err := http.ParseTime(raw); err == nil {
		delta := int(time.Until(when).Seconds())
		if delta <= 0 {
			return 0, false
		}
		if delta > maxRetryAfterSeconds {
			return maxRetryAfterSeconds, true
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

// resetDHMRe matches "Resets in Xd Yh Zm" / "Xh Ym" / "Ym" in vendor CLI
// extractResetAtMs message patterns.
var resetDHMRe = regexp.MustCompile(`(?i)resets?\s+in\s+(?:(\d+)d\s*)?(?:(\d+)h\s*)?(?:(\d+)m)?`)

// resetAtRe matches "resets at <ISO8601>" from vendor CLI extractResetAtMs.
var resetAtRe = regexp.MustCompile(`(?i)resets?\s+at\s+(\S+)`)

// retryAfterFromMessageText tries to extract a reset hint from plain-English
// message text, mirroring the vendor CLI's extractResetAtMs behaviour:
//   - "resets at 2024-06-01T12:00:00Z" (ISO 8601)
//   - "Resets in 2h 41m" / "1d 2h 3m" / "41m"
//
// Returns 0 if no hint is found.
func retryAfterFromMessageText(msg string) int {
	// Try "resets at <ISO>" first
	if m := resetAtRe.FindStringSubmatch(msg); len(m) >= 2 {
		f := extractResetSeconds(json.RawMessage(`"` + strings.ReplaceAll(m[1], `"`, `\"`) + `"`))
		if f > 0 {
			delta := f - float64(time.Now().Unix())
			if delta > 0 {
				if delta > maxRetryAfterSeconds {
					return maxRetryAfterSeconds
				}
				return int(math.Ceil(delta))
			}
		}
	}
	// Try "resets in Xd Yh Zm"
	if m := resetDHMRe.FindStringSubmatch(msg); len(m) >= 2 {
		d, _ := strconv.Atoi(m[1])
		h, _ := strconv.Atoi(m[2])
		mi, _ := strconv.Atoi(m[3])
		total := (d*24*60 + h*60 + mi) * 60
		if total > 0 {
			if total > maxRetryAfterSeconds {
				return maxRetryAfterSeconds
			}
			return total
		}
	}
	return 0
}

// rateLimitObjectPresent reports whether a rateLimit object was present and
// carries at least one meaningful field (window name or reset instant). An
// empty rateLimit object is not a window-exhaustion signal.
func rateLimitObjectPresent(rl *ccRateLimit) bool {
	if rl == nil {
		return false
	}
	return rl.Window != "" || rl.Reset != nil
}

// windowMessage names the exhausted window so the credential card and client
// error say which limit was hit rather than a generic "rate limited".
func windowMessage(window, fallback string) string {
	normalized := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(window), "_", ""))
	switch normalized {
	case "fivehour", "5hour":
		return "5-hour usage window exhausted" + suffix(fallback)
	case "weekly":
		return "weekly usage window exhausted" + suffix(fallback)
	case "daily":
		return "daily usage window exhausted" + suffix(fallback)
	case "monthly", "month":
		return "monthly usage window exhausted" + suffix(fallback)
	}
	if fallback == "" {
		return "usage window exhausted"
	}
	return fallback
}

// extractWindowFromMessage tries to infer the window type from message text.
// Returns empty if no recognized pattern is found.
func extractWindowFromMessage(msg string) string {
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "5-hour"), strings.Contains(lower, "5hour"):
		return "five_hour"
	case strings.Contains(lower, "weekly"):
		return "weekly"
	case strings.Contains(lower, "daily"):
		return "daily"
	case strings.Contains(lower, "monthly"), strings.Contains(lower, "billing period"):
		return "monthly"
	}
	return ""
}

func suffix(rest string) string {
	if strings.TrimSpace(rest) == "" {
		return ""
	}
	return ": " + rest
}

// isWindowExhaustion matches the CLI's usage-limit vocabulary, broadened to
// cover the vendor CLI's regex /usage (window|limit)|5-?hour|weekly limit|
// window_limit/ plus monthly variants observed in production.
func isWindowExhaustion(lowered string) bool {
	switch {
	case strings.Contains(lowered, "window_limit"),
		strings.Contains(lowered, "usage window"),
		strings.Contains(lowered, "usage limit"),
		strings.Contains(lowered, "5-hour"),
		strings.Contains(lowered, "5hour"),
		strings.Contains(lowered, "weekly limit"),
		strings.Contains(lowered, "weekly usage"),
		strings.Contains(lowered, "monthly limit"),
		strings.Contains(lowered, "monthly usage"),
		strings.Contains(lowered, "monthly credits"),
		strings.Contains(lowered, "billing period"):
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

// isSpendCap matches spend-cap / plan-inactive wording that makes the account
// unable to serve any request (credential-wide exhaustion), but is NOT an auth
// failure (invalid key). Called only for non-401 responses.
func isSpendCap(lowered string) bool {
	switch {
	case strings.Contains(lowered, "spend cap"),
		strings.Contains(lowered, "spend limit"),
		strings.Contains(lowered, "subscription expired"),
		strings.Contains(lowered, "subscription inactive"),
		strings.Contains(lowered, "plan inactive"),
		strings.Contains(lowered, "account inactive"),
		strings.Contains(lowered, "account suspended"):
		return true
	}
	return false
}

// isModelNotInPlan matches the CLI's plan-entitlement vocabulary. Called AFTER
// window/credit exhaustion so "upgrade your plan" on a 429 goes to
// credential_quota, not model_not_found.
func isModelNotInPlan(lowered string) bool {
	if strings.Contains(lowered, "model_not_in_plan") {
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

// isRequestDefect matches failures caused by the request body itself. It is
// deliberately NOT called for 429 responses (rate-limit wording such as "rate
// limit exceeded for max_tokens per minute" must not become request_scoped).
// The status parameter enforces that guard.
func isRequestDefect(status int, lowered string) bool {
	// 429 responses are NEVER request defects — they are admission/capacity
	// failures and must remain in the rate-limit family.
	if status == http.StatusTooManyRequests {
		return false
	}
	switch {
	case strings.Contains(lowered, "prompt is too long"),
		strings.Contains(lowered, "context length"),
		strings.Contains(lowered, "maximum number of tokens"):
		return true
	case strings.Contains(lowered, "max_tokens"):
		// Only a defect when the message is about the token limit itself, not
		// a rate-limit counter that happens to mention the token bucket name
		// (e.g. "rate limit exceeded for max_tokens per minute").
		return !strings.Contains(lowered, "rate limit") &&
			!strings.Contains(lowered, "rate_limit") &&
			!strings.Contains(lowered, "per minute") &&
			!strings.Contains(lowered, "per hour")
	}
	return false
}
