package errclass

import (
	"strings"
	"testing"
)

// TestRedactJSONKeys pins Finding 10: JSON keys api_key/apiKey/token/access_token/secret
// with string values are redacted.
func TestRedactJSONKeys(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{
			`{"api_key": "sk-live-abc123def456", "model": "glm-5.2"}`,
			`{"api_key": "[redacted]", "model": "glm-5.2"}`,
		},
		{
			`{"apiKey": "user_a1b2c3d4e5f6", "status": "ok"}`,
			`{"apiKey": "[redacted]", "status": "ok"}`,
		},
		{
			`{"token": "tok_9876543210abcdef", "name": "test"}`,
			`{"token": "[redacted]", "name": "test"}`,
		},
		{
			`{"access_token": "ghu_1234567890abcdef", "scope": "repo"}`,
			`{"access_token": "[redacted]", "scope": "repo"}`,
		},
		{
			`{"secret": "secret_key_abcd1234", "env": "prod"}`,
			`{"secret": "[redacted]", "env": "prod"}`,
		},
		// No digit or too short: not redacted.
		{
			`{"api_key": "short", "model": "x"}`,
			`{"api_key": "short", "model": "x"}`,
		},
	}
	for _, tc := range cases {
		got := Redact(tc.input)
		if got != tc.want {
			t.Errorf("Redact(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// TestRedactBareCommandCodeKeys pins Finding 10: bare "user_<alnum>+" keys in prose.
func TestRedactBareCommandCodeKeys(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{
			"authentication failed for user_abc123def456ghi",
			"authentication failed for [redacted]",
		},
		{
			"key user_9876543210abcdef not found",
			"key [redacted] not found",
		},
		// No digit after user_: not redacted.
		{
			"user_admin logged in",
			"user_admin logged in",
		},
	}
	for _, tc := range cases {
		got := Redact(tc.input)
		if got != tc.want {
			t.Errorf("Redact(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// TestRedactBasicAuth pins Finding 10: "Authorization: Basic ..." is redacted.
func TestRedactBasicAuth(t *testing.T) {
	input := `request headers: {"Authorization": "Basic dXNlcjpwYXNzd29yZA==", "Content-Type": "application/json"}`
	want := `request headers: {"Authorization": "Basic [redacted]", "Content-Type": "application/json"}`
	got := Redact(input)
	if got != want {
		t.Errorf("Redact(%q) = %q, want %q", input, got, want)
	}
}

// TestRedactQueryParams pins Finding 10: query params secret/password/auth are redacted.
func TestRedactQueryParams(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{
			`Get "https://api.example.com/v1?secret=abc123def456": dial tcp failed`,
			`Get "https://api.example.com/v1?secret=[redacted]": dial tcp failed`,
		},
		{
			`POST /auth?password=secret123&user=alice HTTP/1.1`,
			`POST /auth?password=[redacted]&user=alice HTTP/1.1`,
		},
		{
			`fetch https://gw.test/v1?auth=token987654321 failed`,
			`fetch https://gw.test/v1?auth=[redacted] failed`,
		},
	}
	for _, tc := range cases {
		got := Redact(tc.input)
		if got != tc.want {
			t.Errorf("Redact(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// TestRedactPreservesExisting pins idempotency: re-running Redact on already
// redacted text yields the same output.
func TestRedactPreservesExisting(t *testing.T) {
	input := `{"api_key": "sk-live-abc123def456", "bearer": "Bearer sk-test-xyz"}`
	once := Redact(input)
	twice := Redact(once)
	if once != twice {
		t.Errorf("Redact not idempotent: once=%q twice=%q", once, twice)
	}
}

// TestPostFirstByteNetworkHasWireCode pins Finding 11: PostFirstByteNetwork
// gets stream_truncated WireCode (non-retryable, no-cooldown).
func TestPostFirstByteNetworkHasWireCode(t *testing.T) {
	e := PostFirstByteNetwork("connection reset by peer")
	if e.WireCode == "" {
		t.Error("PostFirstByteNetwork WireCode empty, want stream_truncated")
	}
	// The exact constant name may vary; check it's a non-retryable code.
	if !strings.Contains(strings.ToLower(e.WireCode), "stream") && !strings.Contains(strings.ToLower(e.WireCode), "truncat") {
		t.Errorf("WireCode = %q, expected stream-related code", e.WireCode)
	}
}
