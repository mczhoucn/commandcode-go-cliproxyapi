// Package catalog fetches the live CommandCode /v1/models catalog,
// normalizes metadata, assigns routes, and serves the last good snapshot
// (FR-002, FR-003, FR-004, FR-010).
//
// Route resolution priority follows arch spec §5: user route override >
// built-in compatibility table > unsupported (excluded from the routable set,
// kept in diagnostics).
//
// No scheduling lives here; M3 wires the refresh cadence.
package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"commandcode-or-or-cliproxyapi/internal/config"
)

// catalogBudgetFloor keeps an undersized max-response-bytes knob from
// freezing stale serving forever: catalogs grow larger than typical
// completion bodies, so the fetch budget never drops below 16 MiB while
// max-response-bytes stays meaningful for completions elsewhere.
const catalogBudgetFloor = 16 << 20

// Route is the upstream protocol a model is served through (FR-004).
type Route string

const (
	RouteChatCompletions Route = "chat-completions"
	RouteMessages        Route = "messages"
	RouteResponses       Route = "responses"
)

// EndpointPath returns the upstream endpoint path for the route (FR-004).
// routeEndpoints maps each route to its upstream endpoint path.
var routeEndpoints = map[Route]string{
	RouteChatCompletions: "/v1/chat/completions",
	RouteMessages:        "/v1/messages",
	RouteResponses:       "/v1/responses",
}

// EndpointPath returns the route's upstream endpoint path, or "" for
// unknown routes.
func (r Route) EndpointPath() string {
	return routeEndpoints[r]
}

func routeFromString(s string) (Route, bool) {
	if _, ok := routeEndpoints[Route(s)]; ok {
		return Route(s), true
	}
	return "", false
}

// ModelRecord is one routable normalized catalog entry (arch §7, FR-003).
type ModelRecord struct {
	PublicID     string
	UpstreamID   string
	DisplayName  string
	Protocol     Route
	EndpointPath string
	ContextLimit int64
	OutputLimit  int64
	InputModes   []string
	OutputModes  []string
	Thinking     *pluginapi.ThinkingSupport
}

// UnsupportedModel is a discovered model excluded from the routable set,
// retained for diagnostics (FR-004, open questions §10).
type UnsupportedModel struct {
	UpstreamID string
	Reason     string
}

// HostClient issues HTTP requests through the host (pluginapi.HTTPDo
// semantics); M3's host callback satisfies it directly.
type HostClient interface {
	Do(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)
}

// rawModel tolerantly decodes one catalog entry; unknown fields —
// including protocol/endpoint fields the catalog does not provide and
// reasoning/tool_calling flags the plugin does not act on — are ignored via
// json tags. Modality metadata accepts both spellings.
type rawModel struct {
	ID               string       `json:"id"`
	DisplayName      string       `json:"display_name"`
	ContextLength    int64        `json:"context_length"`
	MaxOutputTokens  int64        `json:"max_output_tokens"`
	InputModes       []string     `json:"input_modes"`
	OutputModes      []string     `json:"output_modes"`
	InputModalities  []string     `json:"input_modalities"`
	OutputModalities []string     `json:"output_modalities"`
	Thinking         *rawThinking `json:"thinking"`
}

// rawThinking tolerantly decodes the optional per-model thinking object;
// pluginapi.ThinkingSupport itself is untagged, so snake_case upstream keys
// are decoded here and mapped field-by-field (partial objects decode to a
// partial struct).
type rawThinking struct {
	Min            *int     `json:"min"`
	Max            *int     `json:"max"`
	ZeroAllowed    *bool    `json:"zero_allowed"`
	DynamicAllowed *bool    `json:"dynamic_allowed"`
	Levels         []string `json:"levels"`
}

// DefaultRoute is the route every catalog model takes when no
// route-override claims it (arch §5 amended for the Chat-Completions-only
// upstream): CommandCode serves OpenAI/OSS models on /provider/v1/
// chat/completions only — its /messages endpoint accepts Claude models and
// rejects OSS ids with "Model \"<id>\" is not supported on this endpoint",
// and /provider/v1/responses is not a registered route at all. Publishing
// models on those routes would advertise ids that can only fail, so the
// family-prefix compatibility table is replaced by a single default; a
// deployment whose upstream really does serve another protocol can pin
// individual models (or a family) back with route-overrides.
const DefaultRoute = RouteChatCompletions

// Manager owns the catalog snapshot. All accessors are safe for
// concurrent use alongside Refresh.
type Manager struct {
	cfg    config.Config
	client HostClient

	mu sync.Mutex
	// raw retains the decoded upstream entries behind the current snapshot
	// so SeedFrom can rebuild records against a NEW config (route overrides,
	// protocol flags, and prefix settings may all have changed since the
	// snapshot was built).
	raw    []rawModel
	models []ModelRecord
	// index maps both PublicID and UpstreamID to their record so ID
	// resolution is O(1); rebuilt atomically with models on every swap.
	index map[string]ModelRecord
	unsup []UnsupportedModel
	warns []string
}

// New returns a Manager serving cfg through the host client.
func New(cfg config.Config, client HostClient) *Manager {
	return &Manager{cfg: cfg, client: client}
}

// SeedFrom republishes prev's last-good snapshot into m by REBUILDING every
// record from prev's raw upstream entries through the same
// resolution/validation path a refresh applies, but against m's OWN cfg:
// routes re-resolve (a removed override drops its model), the protocolEnabled
// gate re-applies, PublicIDs recompute under m's prefix, endpoints re-check
// against m's base-url, and collisions/diagnostics regenerate (arch §5).
//
// m keeps its OWN cfg: subsequent Refresh calls use m's base-url/catalog-url/
// protocols/prefix (F5: a failed-refresh reconfigure must adopt the new
// manager rather than keep ticking the old URL). Because seeding rebuilds
// instead of copying, a config change between snapshots takes effect at seed
// time — e.g. a protocol kill-switch flipped during a catalog outage demotes
// its routes immediately instead of serving stale routable records until the
// outage ends.
func (m *Manager) SeedFrom(prev *Manager) {
	prev.mu.Lock()
	raw := prev.raw
	prev.mu.Unlock()
	m.swap(raw)
}

// Refresh fetches and swaps the catalog snapshot. On failure it returns a
// classified, key-free error; the previous snapshot keeps serving while
// catalog.stale-while-unavailable is enabled, otherwise the routable set
// is cleared until the next success (FR-002).
func (m *Manager) Refresh(ctx context.Context, apiKey string) error {
	if strings.TrimSpace(apiKey) == "" {
		return m.fail("no enabled CommandCode auth files")
	}
	if m.client == nil {
		// Only direct construction can produce a nil host client
		// (production always wires the bridge); fail loudly through the
		// classified path instead of panicking inside Do.
		return m.fail("host client unavailable")
	}
	req := pluginapi.HTTPRequest{
		Method: http.MethodGet,
		URL:    m.cfg.CatalogURL,
		Headers: http.Header{
			"Authorization": []string{"Bearer " + apiKey},
			"Accept":        []string{"application/json"},
		},
	}
	resp, err := m.client.Do(ctx, req)
	if err != nil {
		return m.fail("network error")
	}
	budget := max(m.cfg.MaxResponseBytes, catalogBudgetFloor)
	if int64(len(resp.Body)) > budget {
		return m.fail("response exceeds max-response-bytes")
	}
	if resp.StatusCode != http.StatusOK {
		return m.fail(fmt.Sprintf("http %d", resp.StatusCode))
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		return m.fail("invalid json")
	}
	var entries []rawModel
	var warns []string
	if env.Data == nil {
		// Key ABSENCE is upstream shape drift, never an intended clear:
		// clear like an empty plan (swap(nil)) but leave a diagnostic.
		// A present-but-empty array stays silent (tested-intended state).
		warns = append(warns, `upstream catalog response missing "data" field`)
	} else if err := json.Unmarshal(env.Data, &entries); err != nil {
		return m.fail("invalid json")
	}
	m.swap(entries, warns...)
	return nil
}

// fail applies the stale policy and returns the classified error.
func (m *Manager) fail(category string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.cfg.Catalog.StaleWhileUnavailable {
		// Clear the index too — Lookup must stop resolving IDs whose
		// records are gone (FR-002). Raw entries go with them: a cleared
		// snapshot has nothing to seed.
		m.raw, m.models, m.index, m.unsup, m.warns = nil, nil, nil, nil, nil
	}
	return fmt.Errorf("catalog refresh failed: %s", category)
}

// swap builds the new snapshot from decoded entries and installs it
// atomically under the mutex (FR-010 dedup, arch §5 route priority).
// extraWarns are caller-supplied snapshot diagnostics (e.g. decode-level
// shape-drift notices) recorded alongside the per-entry ones.
func (m *Manager) swap(entries []rawModel, extraWarns ...string) {
	models := make([]ModelRecord, 0, len(entries))
	index := make(map[string]ModelRecord, len(entries)*2)
	var unsup []UnsupportedModel
	var warns []string
	warns = append(warns, extraWarns...)
	seen := make(map[string]bool, len(entries))

	// Track which route-override keys were matched so we can warn about
	// keys that resolve no catalog entry (typos, stale IDs). An override key
	// may match via the bare upstream ID OR the prefixed public ID; we record
	// each key that fires in resolve().
	matchedOverrideKeys := make(map[string]bool, len(m.cfg.RouteOverrides))
	for _, e := range entries {
		if e.ID == "" {
			unsup = append(unsup, UnsupportedModel{Reason: "missing id"})
			continue
		}
		if seen[e.ID] {
			warns = append(warns, fmt.Sprintf("duplicate model %q ignored; keeping first occurrence", e.ID))
			continue
		}
		seen[e.ID] = true
		route, endpoint, ok, warn, matchedKey := m.resolve(e)
		if matchedKey != "" {
			matchedOverrideKeys[matchedKey] = true
		}
		if warn != "" {
			warns = append(warns, warn)
		}
		if !ok {
			unsup = append(unsup, UnsupportedModel{UpstreamID: e.ID, Reason: "no route determined"})
			continue
		}
		if !m.protocolEnabled(route) {
			// Flags apply at snapshot build; a stale snapshot kept per
			// FR-002 reflects the flags it was built with until the next
			// successful refresh.
			unsup = append(unsup, UnsupportedModel{
				UpstreamID: e.ID,
				Reason:     fmt.Sprintf("protocol %s disabled by config", route),
			})
			continue
		}
		if endpoint != "" && endpointEscapesBase(m.cfg.BaseURL, endpoint) {
			// The joined URL must stay on the configured upstream
			// authority. Override endpoints flow through here, so they
			// cannot rewrite the URL host — e.g. "/v1@evil.com/x"
			// against a bare-host base-url turns the base into userinfo
			// and would send credentials/prompts to an attacker host
			// (spec 05 §2 trust boundary).
			unsup = append(unsup, UnsupportedModel{
				UpstreamID: e.ID,
				Reason:     "resolved endpoint escapes the configured upstream host",
			})
			continue
		}
		if endpoint == "" {
			endpoint = route.EndpointPath()
		}
		inputModes := firstNonEmpty(e.InputModes, e.InputModalities)
		outputModes := firstNonEmpty(e.OutputModes, e.OutputModalities)
		display := e.DisplayName
		if display == "" {
			display = e.ID
		}
		rec := ModelRecord{
			PublicID:     config.PublicID(m.cfg, e.ID),
			UpstreamID:   e.ID,
			DisplayName:  display,
			Protocol:     route,
			EndpointPath: endpoint,
			ContextLimit: e.ContextLength,
			OutputLimit:  e.MaxOutputTokens,
			InputModes:   inputModes,
			OutputModes:  outputModes,
			Thinking:     normalizeThinking(e.Thinking),
		}
		// With a prefix enabled, one record's PublicID can equal another
		// record's UpstreamID (upstream "foo" and "commandcode/foo" both
		// claim index key "<prefix>/foo"); last-write-wins would silently
		// misroute. First in catalog order wins (same dedup rule as
		// duplicate IDs); the later record is excluded. Same-role clashes
		// are impossible — distinct upstream IDs and an injective public
		// mapping — so the slot owner always holds the key via its other
		// role, and a record's own public==upstream rewrite is not a clash.
		if other, ok := index[rec.PublicID]; ok {
			reason := fmt.Sprintf("public id %q collides with upstream id %q", rec.PublicID, other.UpstreamID)
			warns = append(warns, reason)
			unsup = append(unsup, UnsupportedModel{UpstreamID: e.ID, Reason: reason})
			continue
		}
		if other, ok := index[rec.UpstreamID]; ok {
			reason := fmt.Sprintf("public id %q collides with upstream id %q", other.PublicID, rec.UpstreamID)
			warns = append(warns, reason)
			unsup = append(unsup, UnsupportedModel{UpstreamID: e.ID, Reason: reason})
			continue
		}
		models = append(models, rec)
		index[rec.PublicID] = rec
		index[rec.UpstreamID] = rec
	}
	// Warn about route-override keys that never matched any catalog entry.
	// This catches typos and stale IDs written as either form (bare upstream
	// ID or prefixed public ID).
	for k := range m.cfg.RouteOverrides {
		if !matchedOverrideKeys[k] {
			warns = append(warns, fmt.Sprintf("route-override key %q did not match any catalog model", k))
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.raw, m.models, m.index, m.unsup, m.warns = entries, models, index, unsup, warns
}

// protocolEnabled reports whether the resolved route's protocol flag is on
// (spec 04 §4); flags default to true.
func (m *Manager) protocolEnabled(r Route) bool {
	switch r {
	case RouteChatCompletions:
		return m.cfg.Protocols.ChatCompletions
	case RouteMessages:
		return m.cfg.Protocols.Messages
	case RouteResponses:
		return m.cfg.Protocols.Responses
	}
	return true
}

// resolve applies route priority strictly: user override, then the
// Chat-Completions-only default (arch §5 as amended for CommandCode). A
// catalog entry is therefore always routable; the protocol gates and the
// endpoint-escape check still filter what reaches the model list.
//
// Override keys may be written as either the bare upstream ID or the
// prefixed public ID (e.g. "glm-5.2" or "commandcode/glm-5.2"). The bare
// upstream ID takes precedence when both keys are present (they refer to the
// same model, so the user probably wrote one form without knowing about the
// other). An override key that matches neither form warns at snapshot build.
func (m *Manager) resolve(e rawModel) (Route, string, bool, string, string) {
	publicID := config.PublicID(m.cfg, e.ID)
	// Prefer the bare upstream ID key; fall back to the prefixed public ID.
	if o, ok := m.cfg.RouteOverrides[e.ID]; ok {
		// Spec 04 §5: an override MUST be visible in diagnostics. Load
		// validation (spec 04 §6) guarantees a known protocol, so the
		// map lookup above is the only decision point here.
		return Route(o.Protocol), o.Endpoint, true, fmt.Sprintf(
			"model %q routed via user override (%s %s)", e.ID, o.Protocol, o.Endpoint), e.ID
	}
	if publicID != e.ID {
		if o, ok := m.cfg.RouteOverrides[publicID]; ok {
			return Route(o.Protocol), o.Endpoint, true, fmt.Sprintf(
				"model %q routed via user override key %q (%s %s)", e.ID, publicID, o.Protocol, o.Endpoint), publicID
		}
	}
	return DefaultRoute, "", true, "", ""
}

// JoinUpstreamURL joins the configured base-url onto an endpoint path and
// is the single source of truth for that join (spec 05 §2): base-url
// already carries the version segment (default .../provider/v1, catalog at
// {base}/models), so a leading "/v1" PATH SEGMENT on the endpoint is dropped
// instead of doubled. Only an exact "/v1" segment is trimmed: "/v1beta/x"
// must NOT become "/beta/x", so we strip the prefix only when it is followed
// by "/" or end-of-string. The authority-escape check (endpointEscapesBase)
// must parse exactly the URL this produces — two independent join formulas
// would be free to drift apart and validate one URL while sending another.
func JoinUpstreamURL(baseURL, endpoint string) string {
	trimmed := endpoint
	if strings.HasPrefix(endpoint, "/v1/") {
		trimmed = endpoint[len("/v1"):]
	} else if endpoint == "/v1" {
		trimmed = ""
	}
	return strings.TrimSuffix(baseURL, "/") + trimmed
}

// endpointEscapesBase reports whether joining baseURL with endpoint via
// JoinUpstreamURL would send the request to a different authority, scheme, or
// base path than the configured base-url, or would use userinfo credentials.
// Any parse failure counts as escaping (fail closed). Path-traversal sequences
// (".."), userinfo in the endpoint, and absolute URLs to other hosts are all
// rejected.
func endpointEscapesBase(baseURL, endpoint string) bool {
	base, err := url.Parse(baseURL)
	if err != nil {
		return true
	}
	// An "@" in the endpoint combined with a bare-host base-url (no path) can
	// cause the URL parser to interpret everything before the "@" as userinfo
	// and everything after as the new host. Example:
	//   base  = "https://api.example.com"
	//   ep    = "/v1@evil.com/x"
	//   old join: ".../v1" + "@evil.com/x" => userinfo rewrite
	//   new join: "...commandcode.ai" + "/v1@evil.com/x" => "/v1@evil.com/x" is a safe path
	//
	// Even with the new join (which does NOT strip /v1 unless it's an exact
	// segment), a bare-host base + endpoint containing "@" can still create
	// an authority rewrite when the "@" lands between the scheme+host and the
	// path. The safe approach: if the base has no path (just scheme+host), any
	// "@" in the endpoint that would appear in the join result before the first
	// "/" is a potential authority rewrite, and we reject it.
	if base.Path == "" || base.Path == "/" {
		// Endpoint starts at the authority boundary; an "@" anywhere before
		// the first "/" in the endpoint could make the parser reinterpret the
		// base hostname as userinfo.
		epForCheck := endpoint
		if strings.HasPrefix(epForCheck, "/v1/") {
			epForCheck = epForCheck[len("/v1"):]
		} else if epForCheck == "/v1" {
			epForCheck = ""
		}
		if idx := strings.Index(epForCheck, "/"); idx < 0 {
			// No path separator — treat as escaping if "@" present
			if strings.Contains(epForCheck, "@") {
				return true
			}
		} else {
			// Check only the part before the first "/"
			if strings.Contains(epForCheck[:idx], "@") {
				return true
			}
		}
	}

	joined := JoinUpstreamURL(baseURL, endpoint)
	got, err := url.Parse(joined)
	if err != nil {
		return true
	}
	// Different scheme or host.
	if !strings.EqualFold(got.Host, base.Host) || got.Scheme != base.Scheme {
		return true
	}
	// Userinfo in the joined URL means the endpoint contained an "@" that the
	// URL parser treated as userinfo, sending credentials to an attacker host.
	if got.User != nil {
		return true
	}
	// Path traversal: a ".." anywhere in the cleaned path means the endpoint
	// left the configured base path prefix.
	cleanedPath := got.EscapedPath()
	if strings.Contains(cleanedPath, "..") || strings.Contains(cleanedPath, "%2e%2e") ||
		strings.Contains(strings.ToLower(cleanedPath), "%2e%2e") {
		return true
	}
	// The joined path must stay on or under the base path prefix. This prevents
	// an endpoint "/../admin" from reaching an entirely different subtree.
	basePath := strings.TrimSuffix(base.Path, "/")
	gotPath := got.Path
	if basePath != "" && !strings.HasPrefix(gotPath, basePath) {
		return true
	}
	return false
}

func firstNonEmpty(a, b []string) []string {
	if len(a) > 0 {
		return a
	}
	return b
}

// normalizeThinking converts the decoded raw thinking object into its
// pluginapi form; absent objects yield nil, partial ones a partial struct.
func normalizeThinking(rt *rawThinking) *pluginapi.ThinkingSupport {
	if rt == nil {
		return nil
	}
	t := &pluginapi.ThinkingSupport{}
	if rt.Min != nil {
		t.Min = *rt.Min
	}
	if rt.Max != nil {
		t.Max = *rt.Max
	}
	if rt.ZeroAllowed != nil {
		t.ZeroAllowed = *rt.ZeroAllowed
	}
	if rt.DynamicAllowed != nil {
		t.DynamicAllowed = *rt.DynamicAllowed
	}
	if len(rt.Levels) > 0 {
		t.Levels = append([]string(nil), rt.Levels...)
	}
	return t
}

// Models returns the routable snapshot (FR-003 identity fields). The slice
// is a shallow copy; records are rebuilt fresh on every swap and consumers
// treat them as read-only, so nested slices/thinking are shared safely.
func (m *Manager) Models() []ModelRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ModelRecord, len(m.models))
	copy(out, m.models)
	return out
}

// Lookup resolves an ID to its routable record, accepting the public ID
// (with or without prefix) or the bare upstream ID (FR-003): the PublicID
// key wins over UpstreamID when both exist.
func (m *Manager) Lookup(id string) (ModelRecord, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.index[id]
	return rec, ok
}

// Unsupported returns models excluded from the routable set with reasons.
func (m *Manager) Unsupported() []UnsupportedModel {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]UnsupportedModel, len(m.unsup))
	copy(out, m.unsup)
	return out
}

// Warnings returns dedup and override diagnostics.
func (m *Manager) Warnings() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.warns))
	copy(out, m.warns)
	return out
}
