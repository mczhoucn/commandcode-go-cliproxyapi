package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"commandcode-go-cliproxyapi/internal/config"
	"commandcode-go-cliproxyapi/resources"
)

func TestManagementRegistration(t *testing.T) {
	m := NewManager(nil)
	var got struct {
		Routes    []struct{ Method, Path string }            `json:"routes"`
		Resources []struct{ Path, Menu, Description string } `json:"resources"`
	}
	decodeResult(t, mustHandle(t, m, pluginabi.MethodManagementRegister, []byte(`{}`)), &got)
	if len(got.Routes) != 5 || got.Routes[0].Method != http.MethodPost || got.Routes[0].Path != "/plugins/"+pluginName+"/quota-usage" || got.Routes[1].Path != "/plugins/"+pluginName+"/accounts" {
		t.Fatalf("routes = %+v", got.Routes)
	}
	if len(got.Resources) != 2 || got.Resources[0].Path != "/quota" || got.Resources[0].Menu != "CommandCode" || got.Resources[1].Path != "/oauth" {
		t.Fatalf("resources = %+v", got.Resources)
	}
	var registration registrationResult
	decodeResult(t, mustHandle(t, m, pluginabi.MethodPluginRegister, lifecycleRequestBody(testValidYAML)), &registration)
	if !registration.Capabilities.ManagementAPI {
		t.Fatal("registration did not advertise management_api")
	}
}

func TestQuotaListDoesNotCallHost(t *testing.T) {
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	f.setAuthFiles("quota-key-a", "quota-key-b")
	var got quotaList
	resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/" + pluginName + "/quota-usage", Body: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(resp.Body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Cards) != 2 || got.Cards[0].FileName == "" || got.Cards[1].FileName == "" {
		t.Fatalf("cards = %+v", got.Cards)
	}
	if strings.Contains(string(resp.Body), "quota-key-") || len(f.callsOf(pluginabi.MethodHostHTTPDo)) != 0 {
		t.Fatalf("list leaked key or called upstream: %s calls=%v", resp.Body, f.callsOf(pluginabi.MethodHostHTTPDo))
	}
	if got.Cards[0].Email != "" || got.Cards[1].Email != "" {
		t.Fatalf("unsynced list cards must not carry an email: %+v", got.Cards)
	}
}

func TestQuotaListSurfacesStoredEmail(t *testing.T) {
	const key = "quota-list-email-secret"
	id := authFileNameFromHash(authKeyHash(key))
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		if method == pluginabi.MethodHostHTTPDo {
			t.Fatal("list must not call upstream")
		}
		if method == pluginabi.MethodHostAuthList {
			return hostOK(hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{{
				ID: id, AuthIndex: id, Provider: ProviderID, Name: id, Email: "dev@example.test", Source: "file",
			}}}), nil
		}
		return hostOK(map[string]any{}), nil
	}}
	m := NewManager(NewHostBridge(f.call))
	resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/" + pluginName + "/quota-usage", Body: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	var got quotaList
	if err := json.Unmarshal(resp.Body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Cards) != 1 || got.Cards[0].KeyID != id || got.Cards[0].Email != "dev@example.test" || got.Cards[0].Label != "dev@example.test" {
		t.Fatalf("cards = %+v", got.Cards)
	}
	if got.Cards[0].Usage != nil {
		t.Fatal("list must not fetch usage for an already-synced account")
	}
}

func TestQuotaRefresh(t *testing.T) {
	const key = "quota-refresh-secret"
	seen := map[string]int{}
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(map[string]any{}), nil
		}
		var wire struct {
			Method  string      `json:"method"`
			URL     string      `json:"url"`
			Headers http.Header `json:"headers"`
		}
		if err := json.Unmarshal(payload, &wire); err != nil || wire.Method != http.MethodGet || wire.Headers.Get("Authorization") != "Bearer "+key || wire.Headers.Get("Accept") != "application/json" {
			t.Fatalf("bad quota request: %s", payload)
		}
		seen[wire.URL]++
		switch wire.URL {
		case "https://quota.test/alpha/billing/credits":
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(
				`{"credits":{"monthlyCredits":51.5,"purchasedCredits":2},"windowLimits":{"limited":true,` +
					`"fiveHour":{"used":0.5,"cap":14,"exceeded":false,"resetAt":1789657174807},` +
					`"weekly":{"used":18.5,"cap":35,"exceeded":false,"resetAt":1790136440498}}}`)}), nil
		case "https://quota.test/alpha/billing/subscriptions":
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(
				`{"success":true,"data":{"planId":"individual-goat","status":"active","currentPeriodEnd":"2026-10-16T04:03:37.000Z"}}`)}), nil
		case "https://quota.test/alpha/whoami?limits=1":
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(
				`{"success":true,"user":{"email":"dev@example.test"},"org":null}`)}), nil
		}
		t.Fatalf("unexpected quota URL %q", wire.URL)
		return nil, nil
	}}
	m := NewManager(NewHostBridge(f.call))
	// base-url carries the OpenAI-compatible surface; the account calls must
	// land on the same authority at /alpha/*.
	m.cfg = config.Config{BaseURL: "https://quota.test/provider/v1", RequestTimeout: config.DefaultRequestTimeout}
	f.setAuthFiles(key)
	id := authFileNameFromHash(authKeyHash(key))
	resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/" + pluginName + "/quota-usage", Body: []byte(`{"key_id":"` + id + `"}`)})
	if err != nil {
		t.Fatal(err)
	}
	var got quotaCard
	if err := json.Unmarshal(resp.Body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Error != "" {
		t.Fatalf("unexpected card error: %q", got.Error)
	}
	if got.Label != "dev@example.test" {
		t.Errorf("label = %q, want the account email", got.Label)
	}
	if got.Usage == nil {
		t.Fatal("usage missing")
	}
	if got.Usage.Plan != "GOAT" || got.Usage.PlanCredits != 70 || got.Usage.CreditsLeft != 51.5 || got.Usage.PurchasedCredits != 2 {
		t.Errorf("plan/credits = %+v", got.Usage)
	}
	if got.Usage.FiveHour.Used != 0.5 || got.Usage.FiveHour.Cap != 14 || got.Usage.FiveHour.Percent != 3 || got.Usage.FiveHour.Status != "ok" {
		t.Errorf("five-hour window = %+v", got.Usage.FiveHour)
	}
	if want := time.UnixMilli(1789657174807).UTC().Format(time.RFC3339); got.Usage.FiveHour.ResetsAt != want {
		t.Errorf("five-hour reset = %q, want %q", got.Usage.FiveHour.ResetsAt, want)
	}
	if got.Usage.Weekly.Percent != 52 || got.Usage.Weekly.Cap != 35 {
		t.Errorf("weekly window = %+v", got.Usage.Weekly)
	}
	if got.Usage.RefreshedAt == "" {
		t.Error("refreshed_at missing")
	}
	// The monthly allowance is derived from the plan table: pool = max(70, 51.5)
	// + 2 purchased = 72, consumed = 72 - (51.5 + 2) = 18.5, i.e. 25%.
	if got.Usage.Month == nil {
		t.Fatal("monthly window missing for a known plan")
	}
	if got.Usage.Month.Cap != 72 || got.Usage.Month.Used != 18.5 || got.Usage.Month.Percent != 25 || got.Usage.Month.Status != "ok" {
		t.Errorf("monthly window = %+v", *got.Usage.Month)
	}
	if got.Usage.Month.ResetsAt != "2026-10-16T04:03:37Z" {
		t.Errorf("monthly reset = %q, want the subscription period end", got.Usage.Month.ResetsAt)
	}
	for url, calls := range seen {
		if calls != 1 {
			t.Errorf("%s called %d times, want 1", url, calls)
		}
	}
	if len(seen) != 3 {
		t.Errorf("endpoints called = %v", seen)
	}
}

// CommandCode switches windowLimits.exceeded from null to the NAME of the
// over-cap window ("weekly") once a plan is exhausted. Decoding it as a
// boolean dropped exactly those accounts with a misleading "response
// invalid", so the exhausted shape is pinned here.
func TestQuotaRefreshExhaustedAccount(t *testing.T) {
	const key = "quota-exhausted-secret"
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(map[string]any{}), nil
		}
		var wire struct {
			Method string `json:"method"`
			URL    string `json:"url"`
		}
		if err := json.Unmarshal(payload, &wire); err != nil {
			t.Fatalf("bad quota request: %s", payload)
		}
		switch wire.URL {
		case "https://quota.test/alpha/billing/credits":
			// Byte-for-byte the live shape of an exhausted plan: top-level
			// exceeded is the string "weekly", fiveHour is drained to zero
			// with no reset time, and the weekly window sits over its cap.
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(
				`{"credits":{"belowThreshold":false,"creditThreshold":0,"monthlyCredits":34.9998441415,"purchasedCredits":0,"freeCredits":0},` +
					`"windowLimits":{"limited":true,"exceeded":"weekly",` +
					`"fiveHour":{"used":0,"cap":14,"exceeded":false,"resetAt":0},` +
					`"weekly":{"used":35.0001558585,"cap":35,"exceeded":true,"resetAt":1789721062334}}}`)}), nil
		case "https://quota.test/alpha/billing/subscriptions":
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(
				`{"success":true,"data":{"planId":"individual-goat","status":"active","currentPeriodEnd":"2026-10-16T04:03:37.000Z"}}`)}), nil
		case "https://quota.test/alpha/whoami?limits=1":
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(
				`{"success":true,"user":{"email":"exhausted@example.test"},"org":null}`)}), nil
		}
		t.Fatalf("unexpected quota URL %q", wire.URL)
		return nil, nil
	}}
	m := NewManager(NewHostBridge(f.call))
	m.cfg = config.Config{BaseURL: "https://quota.test/provider/v1", RequestTimeout: config.DefaultRequestTimeout}
	f.setAuthFiles(key)
	id := authFileNameFromHash(authKeyHash(key))
	resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/" + pluginName + "/quota-usage", Body: []byte(`{"key_id":"` + id + `"}`)})
	if err != nil {
		t.Fatal(err)
	}
	var got quotaCard
	if err := json.Unmarshal(resp.Body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Error != "" {
		t.Fatalf("exhausted account returned error %q", got.Error)
	}
	if got.Usage == nil {
		t.Fatal("usage missing for an exhausted account")
	}
	if got.Label != "exhausted@example.test" {
		t.Errorf("label = %q", got.Label)
	}
	if got.Usage.Weekly.Status != "exceeded" || got.Usage.Weekly.Percent != 100 {
		t.Errorf("weekly window = %+v, want exceeded at 100%%", got.Usage.Weekly)
	}
	if got.Usage.FiveHour.Status != "ok" || got.Usage.FiveHour.Percent != 0 {
		t.Errorf("five-hour window = %+v, want an idle ok window", got.Usage.FiveHour)
	}
	if got.Usage.CreditsLeft != 34.9998441415 || got.Usage.PlanCredits != 70 {
		t.Errorf("credits = %v/%v", got.Usage.CreditsLeft, got.Usage.PlanCredits)
	}
	if want := time.UnixMilli(1789721062334).UTC().Format(time.RFC3339); got.Usage.Weekly.ResetsAt != want {
		t.Errorf("weekly reset = %q, want %q", got.Usage.Weekly.ResetsAt, want)
	}
	// A zero resetAt means "no reset" and must stay empty rather than
	// rendering 1970-01-01 in the page.
	if got.Usage.FiveHour.ResetsAt != "" {
		t.Errorf("five-hour reset = %q, want empty", got.Usage.FiveHour.ResetsAt)
	}
	if got.Usage.Month == nil || got.Usage.Month.ResetsAt != "2026-10-16T04:03:37Z" {
		t.Errorf("monthly reset = %+v, want the subscription period end", got.Usage.Month)
	}
}

// The monthly row divides the plan allowance by what is left of it. CommandCode
// reports only the REMAINING monthly credits, so the pool and the consumed
// amount are derived the way the vendor CLI derives its own usage bar, and the
// window is omitted entirely when the plan (and therefore the denominator) is
// unknown.
func TestPeriodEndRFC3339(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{raw: "2026-10-16T04:03:37.000Z", want: "2026-10-16T04:03:37Z"},
		{raw: "2026-10-16T04:03:37Z", want: "2026-10-16T04:03:37Z"},
		{raw: " 2026-10-16T12:03:37+08:00 ", want: "2026-10-16T04:03:37Z"},
		{raw: "2026-10-16", want: "2026-10-16T00:00:00Z"},
		{raw: "", want: ""},
		{raw: "not-a-date", want: ""},
	}
	for _, tc := range cases {
		if got := periodEndRFC3339(tc.raw); got != tc.want {
			t.Errorf("periodEndRFC3339(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestMonthWindow(t *testing.T) {
	cases := []struct {
		name       string
		plan       float64
		left       float64
		purchased  float64
		wantNil    bool
		wantUsed   float64
		wantCap    float64
		wantPct    int
		wantStatus string
	}{
		{name: "half spent on GOAT", plan: 70, left: 35, wantUsed: 35, wantCap: 70, wantPct: 50, wantStatus: "ok"},
		// weekly/monthly top-ups enlarge the pool without shrinking the plan.
		{name: "purchased credits extend the pool", plan: 70, left: 51.5, purchased: 2, wantUsed: 18.5, wantCap: 72, wantPct: 25, wantStatus: "ok"},
		{name: "fully spent plan", plan: 70, left: 0, wantUsed: 70, wantCap: 70, wantPct: 100, wantStatus: "exceeded"},
		// A plan whose remaining credits exceed the table value (upgraded plan,
		// stale table) must not produce a negative or >100% bar.
		{name: "remaining above the table value", plan: 10, left: 70, wantUsed: 0, wantCap: 70, wantPct: 0, wantStatus: "ok"},
		{name: "unknown plan has no denominator", plan: 0, left: 34.99, wantNil: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := monthWindow(tc.plan, tc.left, tc.purchased)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("monthWindow(%v,%v,%v) = %+v, want nil", tc.plan, tc.left, tc.purchased, *got)
				}
				return
			}
			if got == nil {
				t.Fatalf("monthWindow(%v,%v,%v) = nil", tc.plan, tc.left, tc.purchased)
			}
			if got.Used != tc.wantUsed || got.Cap != tc.wantCap || got.Percent != tc.wantPct || got.Status != tc.wantStatus {
				t.Errorf("monthWindow(%v,%v,%v) = %+v, want used=%v cap=%v percent=%d status=%q",
					tc.plan, tc.left, tc.purchased, *got, tc.wantUsed, tc.wantCap, tc.wantPct, tc.wantStatus)
			}
			if got.Percent < 0 || got.Percent > 100 || got.Used < 0 {
				t.Errorf("monthWindow out of range: %+v", *got)
			}
		})
	}
}

func TestQuotaWindowStatuses(t *testing.T) {
	cases := []struct {
		name string
		w    accountWindow
		lim  bool
		want string
	}{
		{"within window", accountWindow{Used: 1, Cap: 10}, true, "ok"},
		{"exceeded window", accountWindow{Used: 11, Cap: 10, Exceeded: true}, true, "exceeded"},
		{"unlimited upstream", accountWindow{Used: 1, Cap: 0}, false, "unlimited"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := windowFromAccount(tc.w, tc.lim)
			if got.Status != tc.want {
				t.Errorf("status = %q, want %q", got.Status, tc.want)
			}
			if tc.w.Cap == 0 && got.Percent != 0 {
				t.Errorf("uncapped percent = %d", got.Percent)
			}
		})
	}
	// An over-cap window clamps the bar at 100%.
	if got := windowFromAccount(accountWindow{Used: 30, Cap: 10, Exceeded: true}, true); got.Percent != 100 {
		t.Errorf("over-cap percent = %d, want 100", got.Percent)
	}
}

func TestAccountAPIBase(t *testing.T) {
	cases := map[string]string{
		"https://api.commandcode.ai/provider/v1":         "https://api.commandcode.ai",
		"https://api.commandcode.ai/provider/v1/":        "https://api.commandcode.ai",
		"https://staging-api.commandcode.ai/provider/v1": "https://staging-api.commandcode.ai",
		"https://api.commandcode.ai/provider":            "https://api.commandcode.ai",
		"https://gateway.test/code/v1":                   "https://gateway.test/code/v1",
	}
	for in, want := range cases {
		got, err := AccountAPIBase(in)
		if err != nil || got != want {
			t.Errorf("AccountAPIBase(%q) = (%q,%v), want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "not a url", "/provider/v1", "ftp://api.commandcode.ai/provider/v1"} {
		if got, err := AccountAPIBase(bad); err == nil {
			t.Errorf("AccountAPIBase(%q) = %q, want an error", bad, got)
		}
	}
}

func TestPlanForUsesLongestPrefix(t *testing.T) {
	cases := map[string]struct {
		name      string
		allowance float64
	}{
		"individual-goat":   {"GOAT", 70},
		"individual-go":     {"", 0},
		"individual_max":    {"Max", 150},
		"INDIVIDUAL-PRO-V1": {"Pro", 80},
		"teams-pro":         {"Teams Pro", 40},
		"future-plan":       {"", 0},
		"":                  {"", 0},
	}
	for in, want := range cases {
		name, allowance := planFor(in)
		if name != want.name || allowance != want.allowance {
			t.Errorf("planFor(%q) = (%q,%v), want (%q,%v)", in, name, allowance, want.name, want.allowance)
		}
	}
}

func TestQuotaUnknownKeyAndResource(t *testing.T) {
	const key = "quota-known-secret"
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	f.setAuthFiles(key)
	unknown := "commandcode-key-unknown"
	resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/" + pluginName + "/quota-usage", Body: []byte(`{"key_id":"` + unknown + `"}`)})
	if err != nil || resp.StatusCode != http.StatusNotFound || strings.Contains(string(resp.Body), key) || strings.Contains(string(resp.Body), unknown) || len(f.callsOf(pluginabi.MethodHostHTTPDo)) != 0 {
		t.Fatalf("unknown response = %+v err=%v calls=%v", resp, err, f.callsOf(pluginabi.MethodHostHTTPDo))
	}
	resource, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/resource/plugins/" + pluginName + "/quota"})
	if err != nil || resource.StatusCode != 0 || resource.Headers.Get("Content-Type") != "text/html; charset=utf-8" || !strings.Contains(string(resource.Body), "CommandCode") || strings.Contains(string(resource.Body), key) {
		t.Fatalf("resource response = %+v err=%v", resource, err)
	}
}

func TestQuotaPageReadsRememberedManagementKey(t *testing.T) {
	if !strings.Contains(resources.QuotaPage, "parsed.state && parsed.state.managementKey") {
		t.Fatal("quota page does not read persisted state.managementKey")
	}
}

func TestQuotaPageDecodesCurrentCPAAuthStorage(t *testing.T) {
	for _, marker := range []string{"enc::v1::", "cli-proxy-api-webui::secure-storage", "TextDecoder"} {
		if !strings.Contains(resources.QuotaPage, marker) {
			t.Fatalf("quota page does not decode current CPA auth storage: missing %q", marker)
		}
	}
}

// Quota failures surface on the card with a redacted reason: one unreachable
// account must not blank the page, and no key material may appear in the
// response.
func TestQuotaErrorsAreRedacted(t *testing.T) {
	const key = "quota-error-secret"
	id := authFileNameFromHash(authKeyHash(key))
	for name, responder := range map[string]func(string, []byte) ([]byte, error){
		"bridge": func(string, []byte) ([]byte, error) { return nil, context.Canceled },
		"status": func(string, []byte) ([]byte, error) {
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusUnauthorized, Body: []byte(key + " upstream body")}), nil
		},
		"json": func(string, []byte) ([]byte, error) {
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(key + " not json")}), nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
				if method == pluginabi.MethodHostHTTPDo {
					return responder(method, payload)
				}
				return hostOK(map[string]any{}), nil
			}}
			m := NewManager(NewHostBridge(f.call))
			m.cfg = config.Config{BaseURL: "https://quota.test/provider/v1", RequestTimeout: config.DefaultRequestTimeout}
			f.setAuthFiles(key)
			resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/" + pluginName + "/quota-usage", Body: []byte(`{"key_id":"` + id + `"}`)})
			if err != nil || resp.StatusCode != 0 || strings.Contains(string(resp.Body), key) || strings.Contains(string(resp.Body), "upstream body") {
				t.Fatalf("response = %+v err=%v", resp, err)
			}
			var got quotaCard
			if err := json.Unmarshal(resp.Body, &got); err != nil {
				t.Fatal(err)
			}
			if got.Error == "" || got.Usage != nil {
				t.Fatalf("card = %+v, want an error and no usage", got)
			}
			if got.Label != authFileNameFromHash(authKeyHash(key)) {
				t.Errorf("label = %q, want the credential fallback label", got.Label)
			}
		})
	}
}

func TestQuotaPageIsStaticAndSecretFree(t *testing.T) {
	if resources.QuotaPage == "" || strings.Contains(resources.QuotaPage, testKey) || strings.Contains(resources.QuotaPage, "setInterval") || strings.Contains(resources.QuotaPage, "reset button") {
		t.Fatal("quota page contains a secret, polling, or reset action")
	}
	if !strings.Contains(resources.QuotaPage, "textContent") || !strings.Contains(resources.QuotaPage, "Remember password") {
		t.Fatal("quota page missing safe rendering or login guidance")
	}
}

func TestQuotaPageUsesManualSessionCache(t *testing.T) {
	page := resources.QuotaPage
	for _, marker := range []string{
		`const storageKey = "commandcode-go-cliproxyapi:quota"`,
		"sessionStorage.getItem(storageKey)",
		"sessionStorage.setItem(storageKey, JSON.stringify(cache))",
		"JSON.parse(stored)",
		"typeof parsed === \"object\"",
		"Array.isArray(parsed)",
		"catch (_) {}",
		"Object.entries(cache)",
		"values.set(keyID, entry.usage)",
		"cache[keyID] = {usage, fetched_at: timestamp, label: labels.get(keyID) || \"\"}",
		"delete cache[keyID]",
		"new Set(cards.map(card => card.key_id))",
		"cards.filter(card => !card.email)",
		"toLocaleString",
	} {
		if !strings.Contains(page, marker) {
			t.Fatalf("quota page missing session cache marker %q", marker)
		}
	}
	for _, marker := range []string{
		"setTimeout",
		"visibilitychange",
		"document.hidden",
		"window.onfocus",
		"window.addEventListener(\"focus\"",
		"TTL",
		"expiry",
		"expiration",
		"bulk refresh",
	} {
		if strings.Contains(page, marker) {
			t.Fatalf("quota page contains prohibited automatic or expiry behavior %q", marker)
		}
	}
}

func TestQuotaPageUsesNativeQuotaStylesAndThemeBridge(t *testing.T) {
	for _, marker := range []string{
		"quota-page",
		"quota-grid",
		"quota-card",
		"quota-track",
		"quota-fill",
		".quota-fill.is-exceeded { background: var(--error-color); }",
		`fill.className = usage.status === "exceeded" ? "quota-fill is-exceeded" : "quota-fill"`,
		`meta.className = usage.status === "exceeded" ? "quota-meta is-exceeded" : "quota-meta"`,
		// The monthly allowance must stay metered like the two rate-limit
		// windows instead of degrading back to a bare text row.
		`label.textContent = "Monthly credits"`,
		`if (month) { renderUsage(row, month, [left.toFixed(2) + " credits left"], true); }`,
		`[["five_hour", "5-hour window"], ["weekly", "Weekly window"]]`,
		`if (usage) renderUsage(row, usage, null, name === "weekly");`,
		`text(meta, left.toFixed(2) + " credits left (plan unknown)")`,
		`function renderUsage(row, usage, extraParts, withDaysLeft) {`,
		`(extraParts || []).forEach(part => parts.push(part));`,
		`const days = Math.max(0, Math.ceil((reset.getTime() - Date.now()) / 864e5));`,
		`parts.push(days === 1 ? "1 day left" : days + " days left");`,
		"repeat(auto-fill, minmax(380px, 1fr))",
		"@media (max-width: 768px)",
		`[data-theme="white"]`,
		`[data-theme="dark"]`,
		`window.parent.document`,
		`data-theme`,
		`--bg-secondary`,
		`frameElement.style.backgroundColor`,
		`frameElement.parentElement.style.backgroundColor`,
		".quota-refresh",
		"--bg-secondary: #faf9f5",
		"--bg-primary: #f0eee8",
		"--bg-tertiary: #e9e6df",
		"--text-primary: #2d2a26",
		"--text-secondary: #6d6760",
		"--text-tertiary: #a29c95",
		"--border-color: #e3e1db",
		"--primary-color: #8b8680",
		"--primary-hover: #7f7a74",
		"border-radius: 8px; padding: 8px 10px",
		`MutationObserver`,
		`catch (_) {}`,
	} {
		if !strings.Contains(resources.QuotaPage, marker) {
			t.Fatalf("quota page missing styling marker %q", marker)
		}
	}
	page := resources.QuotaPage
	five := strings.Index(page, `["five_hour", "5-hour window"]`)
	weekly := strings.Index(page, `["weekly", "Weekly window"]`)
	month := strings.Index(page, `label.textContent = "Monthly credits"`)
	if five < 0 || weekly < five || month < weekly {
		t.Fatalf("quota rows out of order: five=%d weekly=%d month=%d", five, weekly, month)
	}
	if strings.Count(resources.QuotaPage, `document.createElement("button")`) != 1 || strings.Contains(resources.QuotaPage, `textContent = "Refresh card"`) || strings.Contains(resources.QuotaPage, "quota-button") || strings.Contains(resources.QuotaPage, "quota-refresh-small") {
		t.Fatal("quota page does not have exactly one secondary refresh button path")
	}
	for _, marker := range []string{"querySelectorAll('head link[rel=\"stylesheet\"], head style')", "cloneNode(true)", "dataset.cpaStyle"} {
		if strings.Contains(resources.QuotaPage, marker) {
			t.Fatalf("quota page still clones parent styles: %q", marker)
		}
	}
}
