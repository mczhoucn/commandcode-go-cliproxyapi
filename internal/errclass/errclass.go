// Package errclass implements FR-009 error classification and the
// resolved §7 retry semantics (07-open-questions.md).
package errclass

import (
	"regexp"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// Class is a stable error-category identifier (FR-009) used as the
// envelope error Code.
type Class string

const (
	ClassAuth         Class = "auth_failure"
	ClassInvalidModel Class = "invalid_model"
	ClassUnsupported  Class = "unsupported_protocol_or_parameter"
	ClassRateLimit    Class = "rate_limit"
	ClassQuota        Class = "quota_exhaustion"
	ClassBilling      Class = "billing"
	ClassUpstream     Class = "upstream_server_failure"
	ClassNetwork      Class = "timeout_or_network_failure"
	ClassTranslation  Class = "translation_failure"
)

// Error describes classified request errors and retryability for CPA execution.
//
// The three trailing fields drive CPA's cooldown pipeline (pluginabi.Error):
// Code, RetryAfterSeconds, and CredentialScoped. Leaving them zero makes the
// host fall back to its own defaults, which is what produced wrong cooldowns
// before they were populated — see errclass_cooldown.go.
type Error struct {
	Class      Class
	Message    string
	StatusCode int
	Retryable  bool
	// WireCode overrides Class on the envelope when the host recognizes a
	// machine-readable code (pluginabi.ErrorCode*). Empty means Class is used.
	WireCode string
	// RetryAfterSeconds is upstream retry guidance. Admitted only when the
	// response carried it; a fabricated value would be worse than none.
	RetryAfterSeconds int
	// CredentialScoped marks failures that condemn the whole credential
	// (account-wide quota/window exhaustion) rather than one model.
	CredentialScoped bool
}

func (e *Error) Error() string {
	return string(e.Class) + ": " + e.Message
}

// EnvelopeCode is the Code written to the wire: WireCode when set, else Class.
func (e *Error) EnvelopeCode() string {
	if e.WireCode != "" {
		return e.WireCode
	}
	return string(e.Class)
}

// FromStatus classifies an upstream HTTP status per resolved §7.
// Network/timeout failures (no status) use FromNetwork instead. A redacted
// message containing "quota" (case-insensitive) upgrades 429/402/403 to
// ClassQuota — a bounded heuristic given no documented upstream quota
// taxonomy.
func FromStatus(status int, message string) *Error {
	msg := Redact(message)
	lowerMsg := strings.ToLower(msg)
	quota := strings.Contains(lowerMsg, "quota")
	quotaClass := func(base Class) Class {
		if quota {
			return ClassQuota
		}
		return base
	}
	switch status {
	case 401:
		return &Error{Class: ClassAuth, Message: msg, StatusCode: status, Retryable: true}
	case 403:
		return &Error{Class: quotaClass(ClassAuth), Message: msg, StatusCode: status, Retryable: true}
	case 402:
		return &Error{Class: quotaClass(ClassBilling), Message: msg, StatusCode: status, Retryable: true}
	case 404:
		return &Error{Class: ClassInvalidModel, Message: msg, StatusCode: status}
	case 429:
		return &Error{Class: quotaClass(ClassRateLimit), Message: msg, StatusCode: status, Retryable: true}
	}
	if status >= 500 {
		return &Error{Class: ClassUpstream, Message: msg, StatusCode: status, Retryable: true}
	}
	return &Error{Class: ClassUnsupported, Message: msg, StatusCode: status}
}

// FromNetwork classifies a PRE-first-byte network/timeout failure; always
// retryable (§7). Failures observed after the first upstream byte must use
// PostFirstByteNetwork instead.
//
// The transient_transport code keeps CPA from cooling the credential: a
// dial/TLS/DNS/reset failure says nothing about the account's health, so the
// request stays eligible for another attempt without benching the account.
func FromNetwork(err error) *Error {
	msg := ""
	if err != nil {
		msg = Redact(err.Error())
	}
	return &Error{
		Class:     ClassNetwork,
		Message:   msg,
		Retryable: true,
		WireCode:  pluginabi.ErrorCodeTransientTransport,
	}
}

// PostFirstByteNetwork classifies a network/timeout failure seen AFTER the
// first upstream byte was received: part of the response may already be
// delivered downstream, so a retry would risk duplicate delivery of a half
// stream — never retryable (§7). The stream_truncated wire code matches
// PostFirstByteTruncated's code so CPA applies the same no-cooldown, no-
// replay policy to both kinds of post-first-byte failure.
func PostFirstByteNetwork(msg string) *Error {
	return &Error{
		Class:     ClassNetwork,
		Message:   Redact(msg),
		Retryable: false,
		WireCode:  pluginabi.ErrorCodeStreamTruncated,
	}
}

// Translation reports an FR-005/FR-006 translation failure; never retryable.
func Translation(msg string) *Error {
	return &Error{Class: ClassTranslation, Message: Redact(msg)}
}

// UpstreamFallback builds a ClassUpstream error for the adapter fallback
// sites (chunk/failure errors) that have no upstream status code. Resolved
// §7 makes upstream server failures retryable; the message is redacted at
// construction.
func UpstreamFallback(msg string) *Error {
	return &Error{Class: ClassUpstream, Message: Redact(msg), Retryable: true}
}

// ToEnvelopeError converts to the shared SDK wire type with a redacted
// message. Re-redacting at the envelope edge is INTENTIONAL defense-in-depth:
// constructors already redact (so non-envelope consumers — logs, executor
// fail paths reading .Message directly) are safe, and Redact is idempotent.
//
// The cooldown fields are forwarded verbatim so CPA applies its configured
// cooling policy to this provider exactly as it does to built-in ones.
func ToEnvelopeError(e *Error) pluginabi.Error {
	return pluginabi.Error{
		Code:              e.EnvelopeCode(),
		Message:           Redact(e.Message),
		Retryable:         e.Retryable,
		HTTPStatus:        e.StatusCode,
		RetryAfterSeconds: e.RetryAfterSeconds,
		CredentialScoped:  e.CredentialScoped,
	}
}

// bearerRe matches a bearer token (case-insensitive scheme, optional
// space/tab separator, token terminated by whitespace, comma, or quote).
// The separator is optional so a bare "bearer" word is redacted exactly as
// the previous hand-rolled scanner did. Redact is idempotent — applying it
// twice yields the same output — which is what makes edge re-redaction safe.
var bearerRe = regexp.MustCompile(`(?i)bearer[ \t]*[^\s,"]*`)

// basicAuthRe matches an "Authorization: Basic <creds>" header echo so base64
// HTTP Basic credentials (user:pass) never reach logs. The value run stops at
// whitespace/quote/comma. Idempotent: "[redacted]" starts with '[' which is
// outside the value class.
var basicAuthRe = regexp.MustCompile(`(?i)(authorization["'\s:]*basic[ \t]+)([^\s,"']+)`)

// apiKeyRe finds x-api-key header echoes (Messages-route auth, FR-007):
// case-insensitive name, then a run of quote/colon/equals/space separators,
// then the value run. The value class spans hex/base62 plus the base64
// alphabet ("+", "/", "="), matching bearerRe/queryKeyRe sibling classes.
// The candidate is only replaced when the value looks
// like a key (contains a digit) so ordinary prose after the header name —
// "x-api-key authentication failed" — passes untouched; every real key
// alphabet (hex, base64, base62) carries digits. Idempotent: "[redacted]"
// starts with '[' which is outside the value class, so it never re-matches.
var apiKeyRe = regexp.MustCompile(`(?i)x-api-key["':= ]*[A-Za-z0-9_\-.+/=]+`)

func redactAPIKey(match string) string {
	value := strings.TrimLeft(match[len("x-api-key"):], "\"':= ")
	hasDigit := false
	for _, r := range value {
		if r >= '0' && r <= '9' {
			hasDigit = true
			break
		}
	}
	if !hasDigit || len(value) < 8 {
		return match
	}
	return match[:len(match)-len(value)] + "[redacted]"
}

// queryKeyRe redacts secrets carried in URL query strings (FR-011): transport
// failures conventionally echo the full URL, so base-url/catalog-url keys
// passed as ?api_key=… / ?token=… must not survive into envelopes or logs.
// The value run stops at whitespace, '&', and quotes so consecutive params
// survive; the replacement keeps the parameter name. Idempotent: the
// redacted value "[redacted]" still matches the value class, so re-running
// yields the same text.
var queryKeyRe = regexp.MustCompile(`(?i)([?&](?:api[_-]?key|key|access[_-]?token|token|secret|password|auth)=)[^\s&"']+`)

// cmdcodeKeyRe redacts bare CommandCode-style API keys embedded in prose,
// e.g. "user_a1b2c3d4e5f6g7h8" (the "user_" prefix followed by hex/base32
// material). The run stops at whitespace, punctuation, and quotes; the key
// must contain a digit (real keys always do; ordinary "user_" usernames like
// "user_admin" have no digits after the prefix). Idempotent: "[redacted]"
// does not start with "user_".
var cmdcodeKeyRe = regexp.MustCompile(`(?i)\buser_[A-Za-z0-9_\-.+/=]+`)

func redactCmdCodeKey(match string) string {
	value := strings.TrimPrefix(match, "user_")
	value = strings.TrimPrefix(value, "USER_")
	hasDigit := false
	for _, r := range value {
		if r >= '0' && r <= '9' {
			hasDigit = true
			break
		}
	}
	if !hasDigit || len(value) < 8 {
		return match
	}
	// Replace the entire key (prefix included): a leaked CommandCode key is
	// secret in full, and leaving "user_" behind still hints at the format.
	return "[redacted]"
}

// redactJSONKeyRe finds sensitive JSON values for keys api_key/apiKey/token/
// access_token/secret when their value is a JSON string. The pattern requires
// the key to be quoted (a JSON object key) so bare query-param names like
// "?api_key=..." in URLs are NOT matched — those are covered by queryKeyRe.
// Run AFTER the bearer and apiKey patterns. Idempotent: after replacement the
// value is "[redacted]" which starts with '[', outside the value class.
var redactJSONKeyRe = regexp.MustCompile(`(?i)"(?:api[_-]?key|apiKey|token|access[_-]?token|secret)"\s*:\s*"([A-Za-z0-9_\-.+/=]+)"`)

func redactJSONKey(match string) string {
	m := redactJSONKeyRe.FindStringSubmatch(match)
	if len(m) < 2 {
		return match
	}
	value := m[1]
	hasDigit := false
	for _, r := range value {
		if r >= '0' && r <= '9' {
			hasDigit = true
			break
		}
	}
	if !hasDigit || len(value) < 8 {
		return match
	}
	// Replace only the value inside the quotes; keep key and surrounding quotes.
	return strings.Replace(match, `"`+value+`"`, `"[redacted]"`, 1)
}

// Redact strips bearer tokens, x-api-key echoes, Authorization: Basic creds,
// bare CommandCode keys (user_<alnum>), JSON-object secret values, and query-
// string key parameters from a message so keys never reach logs or clients.
func Redact(msg string) string {
	msg = bearerRe.ReplaceAllString(msg, "Bearer [redacted]")
	// Keep the "Authorization: Basic " prefix (group 1) and redact only the
	// base64 credential run (group 2), so the header structure survives.
	msg = basicAuthRe.ReplaceAllString(msg, "${1}[redacted]")
	msg = apiKeyRe.ReplaceAllStringFunc(msg, redactAPIKey)
	// JSON-object secret values must be redacted BEFORE the bare CommandCode
	// key scanner: a JSON value like "user_a1b2…" should collapse to the whole
	// value "[redacted]" rather than "user_[redacted]", which the bare scanner
	// would otherwise leave behind (and which would then block the JSON regex
	// because "[" is outside its value class).
	msg = redactJSONKeyRe.ReplaceAllStringFunc(msg, redactJSONKey)
	msg = cmdcodeKeyRe.ReplaceAllStringFunc(msg, redactCmdCodeKey)
	return queryKeyRe.ReplaceAllString(msg, "${1}[redacted]")
}
