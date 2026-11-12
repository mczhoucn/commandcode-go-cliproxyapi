// Standard CPA quota-provider surface (QuotaProvider capability).
//
// The plugin's own page (quota-usage) and this provider share one upstream
// reader (fetchQuota); this file only maps the account snapshot onto the host's
// normalized QuotaFetchResponse so Management Center's auth-file cards can
// render it alongside built-in providers.

package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"commandcode-or-or-cliproxyapi/internal/config"
	"commandcode-or-or-cliproxyapi/internal/errclass"
)

// quotaProviderIdentifier is the provider key this quota provider answers for.
// It must match the auth-file provider ("commandcode"); the host matches it
// against QuotaProvider.Identifier() and the credential's provider key.
func quotaProviderIdentifier() string { return ProviderID }

// quotaProvider describes this provider to the host: which provider keys it
// serves and whether reset is possible. CommandCode exposes no reset endpoint,
// so SupportsReset stays false and quota.reset is never advertised.
func quotaProvider() pluginapi.QuotaDescribeResponse {
	return pluginapi.QuotaDescribeResponse{
		SupportedProviders: []string{ProviderID},
		DisplayName:        "CommandCode",
		SupportsReset:      false,
	}
}

// quotaFetch resolves and reads one credential's quota for the host's standard
// surface. The key comes from the request's own attributes/metadata (the host
// already selected this credential), falling back to the physical auth file by
// index so a credential delivered without attributes still resolves.
func (m *Manager) quotaFetch(ctx context.Context, req pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
	if req.Provider != "" && req.Provider != ProviderID {
		return pluginapi.QuotaFetchResponse{}, fmt.Errorf("provider %q is not served by this quota provider", req.Provider)
	}
	key := selectedAuthKey(req.Attributes, req.Metadata)
	if key == "" && req.AuthIndex != "" {
		// The host fills attributes for Credential fetch requests; this branch
		// covers direct plugin-owned calls that carry only the index.
		if entry, ok := m.authEntryByIndex(ctx, req.AuthIndex); ok {
			if resolved, err := authFileKey(ctx, m.bridge, entry); err == nil {
				key = resolved
			}
		}
	}
	if key == "" {
		return pluginapi.QuotaFetchResponse{}, fmt.Errorf("credential has no CommandCode API key")
	}
	m.mu.RLock()
	baseURL, timeout := m.cfg.BaseURL, m.cfg.RequestTimeout
	m.mu.RUnlock()
	if baseURL == "" {
		defaults, _ := config.Load(nil)
		baseURL = defaults.BaseURL
	}
	usage, _, _, err := fetchQuota(ctx, m.bridge, baseURL, timeout, key)
	if err != nil {
		return pluginapi.QuotaFetchResponse{}, err
	}
	// An exhausted window is NOT a fetch failure. The host renders any error as
	// "failed to fetch quota" and drops the whole card, hiding the window data
	// that explains the situation. Return the snapshot instead: the windows are
	// already folded to 100% by applyWindowHierarchy, and the exhausted window
	// is named in the summary so the card says which limit applies.
	return quotaResponseFromUsage(usage), nil
}

// quotaExhaustedLabel names the window that has no allowance left, for the
// summary row on the host's auth-file card. Empty when quota remains.
func quotaExhaustedLabel(window string) string {
	switch window {
	case "month":
		return "Monthly plan credits exhausted"
	case "weekly":
		return "Weekly usage window exhausted"
	case "five_hour":
		return "5-hour usage window exhausted"
	}
	return ""
}

// authEntryByIndex finds one plugin-owned auth file entry by runtime index.
func (m *Manager) authEntryByIndex(ctx context.Context, authIndex string) (pluginapi.HostAuthFileEntry, bool) {
	files, err := commandcodeAuthFiles(ctx, m.bridge)
	if err != nil {
		return pluginapi.HostAuthFileEntry{}, false
	}
	for _, entry := range files {
		if entry.AuthIndex == authIndex {
			return entry, true
		}
	}
	return pluginapi.HostAuthFileEntry{}, false
}

// quotaResponseFromUsage maps the account snapshot onto the host's normalized
// quota shape. One group carries the three windows as buckets; the bucket
// "window" spellings match the host's known order/period table (five_hour,
// weekly) so the management UI orders and labels them like built-in providers.
//
// CommandCode reports only REMAINING monthly credits, so the month bucket is
// omitted when the plan is unknown (no denominator) — same policy as the
// plugin's own page. Finding 10: the bucket window is "monthly", the spelling
// the host's own quota fixtures use (plugin_quota_test.go, types_test.go). The
// Management Center's order/period table knows only five_hour and weekly
// (utils/quota/builders.ts), so neither "month" nor "monthly" gets a period
// label there; the row label comes from the description instead.
func quotaResponseFromUsage(usage quotaUsage) pluginapi.QuotaFetchResponse {
	buckets := make([]pluginapi.QuotaBucket, 0, 3)
	if usage.Month != nil {
		buckets = append(buckets, quotaBucket("monthly", "Monthly credits", *usage.Month))
	}
	buckets = append(buckets,
		quotaBucket("five_hour", "5-hour window", usage.FiveHour),
		quotaBucket("weekly", "Weekly window", usage.Weekly),
	)
	resp := pluginapi.QuotaFetchResponse{
		Subscription: quotaSubscription(usage),
		Groups:       []pluginapi.QuotaGroup{{DisplayName: "CommandCode", Buckets: buckets}},
	}
	// Surface exhaustion as a summary metric rather than an error: the card
	// stays readable, the named limit appears beside the plan, and the buckets
	// below already read 100% used.
	//
	// Finding 10: the Management Center renders a metric as "<label> <value>
	// <unit>" (PluginQuotaBody.tsx formatMetric), so a bare 0 reads like a count
	// ("... exhausted 0"). The value is the remaining percentage (0) with an
	// explicit unit, giving "... exhausted 0 % left". A reset epoch was rejected:
	// the UI formats it as a plain number, not a date. The host keeps any metric
	// with a string key/label and a finite numeric value
	// (plugin_quota.go filterUsableQuotaSummary).
	if window := usage.exhaustedWindow(); window != "" {
		resp.Summary = append(resp.Summary, pluginapi.QuotaMetric{
			Key:    "quota_status",
			Label:  quotaExhaustedLabel(window),
			Value:  0,
			Unit:   "% left",
			Format: "number",
		})
	}
	return resp
}

// quotaSubscription reports the plan name and the plan id where known. The host
// renders plan/tierName as the card's plan label.
func quotaSubscription(usage quotaUsage) *pluginapi.QuotaSubscription {
	plan := strings.TrimSpace(usage.Plan)
	if plan == "" && usage.PlanCredits <= 0 && usage.CreditsLeft <= 0 {
		return nil
	}
	return &pluginapi.QuotaSubscription{Plan: plan, TierName: plan}
}

// quotaBucket converts one internal window into a host bucket. RemainingFraction
// is the REMAINING ratio (host contract), clamped to [0,1]. The state flags win
// over the arithmetic: an unlimited window reports a full meter even when its
// cap is stale, and an exceeded one reports empty.
//
// The host derives a bucket's row label from displayName, then description,
// then window — QuotaBucket has no display-name member — so label carries the
// human name and description carries the used/cap detail.
func quotaBucket(window, label string, w quotaWindow) pluginapi.QuotaBucket {
	remaining := 1.0
	switch {
	case w.Status == "unlimited":
		remaining = 1
	case w.Status == "exceeded":
		remaining = 0
	case w.Cap > 0:
		remaining = 1 - w.Used/w.Cap
	}
	if remaining < 0 {
		remaining = 0
	}
	if remaining > 1 {
		remaining = 1
	}
	return pluginapi.QuotaBucket{
		Window:            window,
		RemainingFraction: remaining,
		ResetTime:         w.ResetsAt,
		Description:       label + " · " + quotaWindowDetail(w),
	}
}

// quotaWindowDetail renders the human-readable "used / cap" detail shared by the
// built-in quota cards. Finding 4: state flags beat arithmetic, so "exceeded"
// reads as exhausted regardless of Cap.
func quotaWindowDetail(w quotaWindow) string {
	if w.Status == "exceeded" {
		return "exhausted"
	}
	if w.Status == "unlimited" || w.Cap <= 0 {
		return "unlimited"
	}
	return fmt.Sprintf("%.2f of %.2f credits used", w.Used, w.Cap)
}

// quotaStatusError maps a quota failure onto the classified envelope the host
// surfaces as a fetch error. Messages are already redacted by fetchQuota.
func quotaStatusError(err error) *errclass.Error {
	if err == nil {
		return nil
	}
	return &errclass.Error{Class: errclass.ClassUpstream, Message: errclass.Redact(err.Error())}
}

// handleQuotaMethod dispatches the four quota.* RPC methods. A nil error with
// handled=true means the response belongs to this provider.
func (m *Manager) handleQuotaMethod(method string, request []byte) ([]byte, bool, error) {
	switch method {
	case pluginabi.MethodQuotaIdentifier:
		return okEnvelope(map[string]string{"identifier": quotaProviderIdentifier()}), true, nil
	case pluginabi.MethodQuotaDescribe:
		return okEnvelope(quotaProvider()), true, nil
	case pluginabi.MethodQuotaFetch:
		var req pluginapi.QuotaFetchRequest
		if json.Unmarshal(request, &req) != nil {
			return ErrEnvelope("invalid_request", "malformed quota fetch request body"), true, nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), quotaMaxTimeout)
		defer cancel()
		resp, err := m.quotaFetch(ctx, req)
		if err != nil {
			return classEnvelope(quotaStatusError(err)), true, nil
		}
		return okEnvelope(resp), true, nil
	case pluginabi.MethodQuotaReset:
		return classEnvelope(&errclass.Error{
			Class:   errclass.ClassUnsupported,
			Message: "CommandCode exposes no quota reset endpoint",
		}), true, nil
	}
	return nil, false, nil
}
