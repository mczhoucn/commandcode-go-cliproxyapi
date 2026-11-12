package plugin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"commandcode-or-or-cliproxyapi/internal/config"
)

// quotaProviderResponder serves the three account endpoints for one key and
// fails the test on any other URL.
func quotaProviderResponder(t *testing.T, base string) func(string, []byte) ([]byte, error) {
	t.Helper()
	return func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(map[string]any{}), nil
		}
		var wire struct {
			Method  string      `json:"method"`
			URL     string      `json:"url"`
			Headers http.Header `json:"headers"`
		}
		if err := json.Unmarshal(payload, &wire); err != nil {
			t.Fatalf("bad quota request: %s", payload)
		}
		switch wire.URL {
		case base + "/alpha/billing/credits":
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(
				`{"credits":{"monthlyCredits":51.5,"purchasedCredits":2},"windowLimits":{"limited":true,` +
					`"fiveHour":{"used":0.5,"cap":14,"exceeded":false,"resetAt":1789657174807},` +
					`"weekly":{"used":18.5,"cap":35,"exceeded":false,"resetAt":1790136440498}}}`)}), nil
		case base + "/alpha/billing/subscriptions":
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(
				`{"success":true,"data":{"planId":"individual-goat","status":"active","currentPeriodEnd":"2026-10-16T04:03:37.000Z"}}`)}), nil
		case base + "/alpha/whoami?limits=1":
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(
				`{"success":true,"user":{"email":"dev@example.test"},"org":null}`)}), nil
		}
		t.Fatalf("unexpected quota URL %q", wire.URL)
		return nil, nil
	}
}

// Registration must advertise the standard quota capability; without it the
// host never calls quota.* and auth-file cards show no usage.
func TestRegistrationAdvertisesQuotaProvider(t *testing.T) {
	m, _ := newTestManager(catalogResponder(true, testCatalogJSON))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	var reg registrationResult
	decodeResult(t, mustHandle(t, m, pluginabi.MethodPluginRegister, lifecycleRequestBody(testValidYAML)), &reg)
	if !reg.Capabilities.QuotaProvider {
		t.Fatal("registration did not advertise quota_provider")
	}
	var wire struct {
		Capabilities map[string]any `json:"capabilities"`
	}
	decodeResult(t, mustHandle(t, m, pluginabi.MethodPluginRegister, lifecycleRequestBody(testValidYAML)), &wire)
	if wire.Capabilities["quota_provider"] != true {
		t.Fatalf("quota_provider wire field = %v, want true", wire.Capabilities["quota_provider"])
	}
}

func TestQuotaIdentifierAndDescribe(t *testing.T) {
	m := NewManager(nil)
	var identifier struct {
		Identifier string `json:"identifier"`
	}
	decodeResult(t, mustHandle(t, m, pluginabi.MethodQuotaIdentifier, nil), &identifier)
	if identifier.Identifier != ProviderID {
		t.Fatalf("identifier = %q, want %q", identifier.Identifier, ProviderID)
	}
	var described pluginapi.QuotaDescribeResponse
	decodeResult(t, mustHandle(t, m, pluginabi.MethodQuotaDescribe, []byte(`{}`)), &described)
	if len(described.SupportedProviders) != 1 || described.SupportedProviders[0] != ProviderID {
		t.Fatalf("supported providers = %v", described.SupportedProviders)
	}
	if described.DisplayName == "" || described.SupportsReset {
		t.Fatalf("describe = %+v; want a label and no reset support", described)
	}
}

// The host contract is remainingFraction in [0,1] — the inverse of the plugin
// page's used/cap percentage. A regression to used-percent would render every
// meter full when the account is fresh.
func TestQuotaFetchMapsRemainingFraction(t *testing.T) {
	const key = "quota-provider-secret"
	f := &fakeCaller{responder: quotaProviderResponder(t, "https://quota.test")}
	m := NewManager(NewHostBridge(f.call))
	m.cfg = config.Config{BaseURL: "https://quota.test/provider/v1", RequestTimeout: config.DefaultRequestTimeout}

	raw := mustHandle(t, m, pluginabi.MethodQuotaFetch, mustJSON(pluginapi.QuotaFetchRequest{
		AuthIndex:  "idx",
		Provider:   ProviderID,
		Attributes: map[string]string{"api_key": key},
	}))
	var got pluginapi.QuotaFetchResponse
	decodeResult(t, raw, &got)

	if got.Subscription == nil || got.Subscription.Plan != "GOAT" {
		t.Fatalf("subscription = %+v, want the GOAT plan", got.Subscription)
	}
	if len(got.Groups) != 1 || len(got.Groups[0].Buckets) != 3 {
		t.Fatalf("groups = %+v, want one group with month/five-hour/weekly", got.Groups)
	}
	byWindow := map[string]pluginapi.QuotaBucket{}
	for _, bucket := range got.Groups[0].Buckets {
		byWindow[bucket.Window] = bucket
	}
	// fiveHour used 0.5 of 14 → remaining 96.43%.
	if frac := byWindow["five_hour"].RemainingFraction; frac < 0.96 || frac > 0.965 {
		t.Errorf("five-hour remaining = %v, want ~0.964", frac)
	}
	// weekly used 18.5 of 35 → remaining 47.14%.
	if frac := byWindow["weekly"].RemainingFraction; frac < 0.47 || frac > 0.473 {
		t.Errorf("weekly remaining = %v, want ~0.471", frac)
	}
	// month pool = max(70,51.5)+2 = 72; remaining (51.5+2)/72 = 74.3%.
	if frac := byWindow["monthly"].RemainingFraction; frac < 0.742 || frac > 0.745 {
		t.Errorf("month remaining = %v, want ~0.743", frac)
	}
	if byWindow["five_hour"].ResetTime != time.UnixMilli(1789657174807).UTC().Format(time.RFC3339) {
		t.Errorf("five-hour reset = %q", byWindow["five_hour"].ResetTime)
	}
}

func TestQuotaFetchRejectsForeignProvider(t *testing.T) {
	m := NewManager(nil)
	env := decodeEnv(t, mustHandle(t, m, pluginabi.MethodQuotaFetch, mustJSON(pluginapi.QuotaFetchRequest{Provider: "openai"})))
	if env.OK {
		t.Fatal("a foreign provider must not be served")
	}
	if env.Error == nil || env.Error.Code != "upstream_server_failure" {
		t.Fatalf("error = %+v", env.Error)
	}
}

func TestQuotaFetchWithoutKeyFails(t *testing.T) {
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	env := decodeEnv(t, mustHandle(t, m, pluginabi.MethodQuotaFetch, mustJSON(pluginapi.QuotaFetchRequest{Provider: ProviderID})))
	if env.OK || env.Error == nil {
		t.Fatalf("missing key must fail: %+v", env)
	}
	if env.Error.Retryable {
		t.Error("a missing credential must not be retryable")
	}
}

// The host renders left-to-right by window order and floors a bucket with no
// finite fraction, so the state flags must win over the used/cap arithmetic:
// an "unlimited" window stays full even with a stale cap, and an exceeded one
// reads empty even if used is somehow below cap.
func TestQuotaBucketStatePrecedence(t *testing.T) {
	unlimited := quotaBucket("weekly", "Weekly window", quotaWindow{Status: "unlimited", Used: 5, Cap: 10})
	if unlimited.RemainingFraction != 1 {
		t.Errorf("unlimited remaining = %v, want 1", unlimited.RemainingFraction)
	}
	exceeded := quotaBucket("weekly", "Weekly window", quotaWindow{Status: "exceeded", Used: 1, Cap: 10})
	if exceeded.RemainingFraction != 0 {
		t.Errorf("exceeded remaining = %v, want 0", exceeded.RemainingFraction)
	}
	unknownCap := quotaBucket("weekly", "Weekly window", quotaWindow{Status: "ok", Used: 3, Cap: 0})
	if unknownCap.RemainingFraction != 1 {
		t.Errorf("unknown cap remaining = %v, want 1", unknownCap.RemainingFraction)
	}
	if !strings.Contains(exceeded.Description, "Weekly window") {
		t.Errorf("description = %q, want the human label", exceeded.Description)
	}
}

func TestQuotaResetIsUnsupported(t *testing.T) {
	m := NewManager(nil)
	env := decodeEnv(t, mustHandle(t, m, pluginabi.MethodQuotaReset, mustJSON(pluginapi.QuotaResetRequest{Provider: ProviderID})))
	if env.OK || env.Error == nil || env.Error.Code != string("unsupported_protocol_or_parameter") {
		t.Fatalf("reset envelope = %+v", env.Error)
	}
}

// One quota failure must not leak the key into the envelope the host surfaces.
func TestQuotaFetchErrorIsRedacted(t *testing.T) {
	const key = "sk-live-quota-redaction-secret"
	f := &fakeCaller{responder: func(method string, _ []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(map[string]any{}), nil
		}
		return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusUnauthorized,
			Body: []byte("invalid key: Bearer " + key)}), nil
	}}
	m := NewManager(NewHostBridge(f.call))
	m.cfg = config.Config{BaseURL: "https://quota.test/provider/v1", RequestTimeout: config.DefaultRequestTimeout}
	raw := mustHandle(t, m, pluginabi.MethodQuotaFetch, mustJSON(pluginapi.QuotaFetchRequest{
		Provider: ProviderID, Attributes: map[string]string{"api_key": key},
	}))
	if bytes.Contains(raw, []byte(key)) {
		t.Fatalf("envelope leaked the API key: %s", raw)
	}
}

// Direct calls that carry only auth_index still resolve the key from the host.
func TestQuotaFetchResolvesKeyFromAuthIndex(t *testing.T) {
	const key = "quota-provider-index-secret"
	f := &fakeCaller{responder: quotaProviderResponder(t, "https://quota.test")}
	f.setAuthFiles(key)
	m := NewManager(NewHostBridge(f.call))
	m.cfg = config.Config{BaseURL: "https://quota.test/provider/v1", RequestTimeout: config.DefaultRequestTimeout}
	id := authFileNameFromHash(authKeyHash(key))
	var got pluginapi.QuotaFetchResponse
	decodeResult(t, mustHandle(t, m, pluginabi.MethodQuotaFetch, mustJSON(pluginapi.QuotaFetchRequest{
		AuthIndex: id, Provider: ProviderID,
	})), &got)
	if got.Subscription == nil || got.Subscription.Plan != "GOAT" {
		t.Fatalf("quota by index = %+v", got)
	}
}
