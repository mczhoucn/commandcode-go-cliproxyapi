package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"commandcode-go-cliproxyapi/internal/catalog"
	"commandcode-go-cliproxyapi/internal/config"
	"commandcode-go-cliproxyapi/internal/errclass"
)

// ProviderID is the single provider key served by this plugin (FR-001).
const ProviderID = "commandcode"

// pluginName / pluginVersion are reported in registration metadata.
const (
	pluginName    = "commandcode-go-cliproxyapi"
	pluginVersion = "0.1.3"
)

// githubRepoURL satisfies the host's validPlugin gate (host.go
// validPlugin rejects empty Metadata.GitHubRepository) and is the address a
// plugin store polls for updates, so it must be the repository this build is
// published from. The project it was derived from is
// https://github.com/massiveits/opencode-go-cliproxyapi.
const githubRepoURL = "https://github.com/mczhoucn/commandcode-go-cliproxyapi"

// registerRefreshTimeout bounds ONLY the synchronous initial/reconfigure
// refreshOnce so a slow catalog cannot block host startup/reconfigure for a
// full request-timeout (default 15m). On expiry registration proceeds per
// FR-002 empty/stale semantics; the ticker retries at refresh-interval with
// the full request-timeout.
const registerRefreshTimeout = 10 * time.Second

// Manager owns dispatcher state (config snapshot, catalog manager, refresh
// loop) and routes every RPC method. Safe for concurrent HandleCall use.
type Manager struct {
	bridge *HostBridge // immutable after NewManager

	// lifeMu serializes whole register/reconfigure/shutdown sequences so
	// their stop-wait-install steps cannot interleave into orphaned tickers.
	lifeMu sync.Mutex

	// Login sessions and unresolved saves share lifeMu with account admission.
	logins           map[string]*loginSession
	credentialClaims map[string]bool
	mu               sync.RWMutex
	cfg              config.Config
	mgr              *catalog.Manager
	// stop/done manage the one background refresh goroutine; both nil
	// when no loop is running.
	stop chan struct{}
	done chan struct{}
}

// NewManager returns a dispatcher whose outbound traffic flows through bridge.
func NewManager(bridge *HostBridge) *Manager {
	return &Manager{bridge: bridge}
}

// HandleCall dispatches one RPC method and returns envelope bytes. Handler
// failures travel inside the envelope; a recovered panic becomes a
// "plugin_error" envelope so the host process never dies with us.
func (m *Manager) HandleCall(method string, request []byte) (resp []byte, err error) {
	debugTrace("handle method=%s request_bytes=%d", method, len(request))
	defer func() {
		// Defensive: host-boundary panics are goroutine-contained (see
		// HostBridge.callWithTimeout); this guards future handler bugs.
		if r := recover(); r != nil {
			resp = ErrEnvelope("plugin_error", fmt.Sprintf("internal error handling %s", method))
			err = nil
		}
	}()
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return m.handleLifecycle(request)
	case pluginabi.MethodModelStatic:
		return m.handleModels()
	case pluginabi.MethodModelForAuth:
		return m.handleModelsForAuth(request)
	case pluginabi.MethodPluginShutdown:
		return m.handleShutdown()
	case pluginabi.MethodExecutorExecute:
		return m.handleExecute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return m.handleExecuteStream(request)
	case pluginabi.MethodAuthIdentifier:
		return okEnvelope(map[string]string{"identifier": ProviderID}), nil
	case pluginabi.MethodAuthParse:
		var req pluginapi.AuthParseRequest
		if json.Unmarshal(request, &req) != nil {
			return ErrEnvelope("invalid_request", "malformed auth parse request body"), nil
		}
		resp, err := (authProvider{}).ParseAuth(context.Background(), req)
		if err != nil {
			return ErrEnvelope("auth_failure", err.Error()), nil
		}
		debugTrace("auth parse response provider=%s id=%s attr_api_key_present=%t storage_json_bytes=%d", resp.Auth.Provider, resp.Auth.ID, strings.TrimSpace(resp.Auth.Attributes["api_key"]) != "", len(resp.Auth.StorageJSON))
		return okEnvelope(resp), nil
	case pluginabi.MethodAuthLoginStart:
		var req struct {
			pluginapi.AuthLoginStartRequest
		}
		if json.Unmarshal(request, &req) != nil {
			return ErrEnvelope("invalid_request", "malformed auth login request body"), nil
		}
		resp, err := m.startLogin(req.AuthLoginStartRequest)
		if err != nil {
			return ErrEnvelope("auth_failure", err.Error()), nil
		}
		return okEnvelope(resp), nil
	case pluginabi.MethodAuthLoginPoll:
		var req struct{ pluginapi.AuthLoginPollRequest }
		if json.Unmarshal(request, &req) != nil {
			return ErrEnvelope("invalid_request", "malformed auth poll request body"), nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), quotaMaxTimeout)
		defer cancel()
		return okEnvelope(m.pollLogin(ctx, req.AuthLoginPollRequest)), nil
	case pluginabi.MethodAuthRefresh:
		var req struct{ pluginapi.AuthRefreshRequest }
		if json.Unmarshal(request, &req) != nil {
			return ErrEnvelope("invalid_request", "malformed auth refresh request body"), nil
		}
		resp, err := (authProvider{}).RefreshAuth(context.Background(), req.AuthRefreshRequest)
		if err != nil {
			return ErrEnvelope("auth_failure", err.Error()), nil
		}
		return okEnvelope(resp), nil
	case pluginabi.MethodManagementRegister:
		return m.registerManagement(request)
	case pluginabi.MethodManagementHandle:
		return m.handleManagement(request)
	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(map[string]string{"identifier": ProviderID}), nil
	case pluginabi.MethodExecutorCountTokens:
		return classEnvelope(&errclass.Error{
			Class:   errclass.ClassUnsupported,
			Message: "executor.count_tokens has no CommandCode equivalent",
		}), nil
	case pluginabi.MethodExecutorHTTPRequest:
		return classEnvelope(&errclass.Error{
			Class:   errclass.ClassUnsupported,
			Message: fmt.Sprintf("%s endpoint is not supported by commandcode", pluginabi.MethodExecutorHTTPRequest),
		}), nil
	default:
		return ErrEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// lifecycleRequest mirrors rpcLifecycleRequest: config_yaml is base64.
// The request's schema_version is decoded-and-ignored; registration echoes
// pluginabi.SchemaVersion.
type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type capabilities struct {
	ModelProvider         bool                         `json:"model_provider"`
	AuthProvider          bool                         `json:"auth_provider"`
	Executor              bool                         `json:"executor"`
	ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope,omitempty"`
	ExecutorInputFormats  []string                     `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats []string                     `json:"executor_output_formats,omitempty"`
	ManagementAPI         bool                         `json:"management_api"`
}

type registrationResult struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  capabilities       `json:"capabilities"`
}

func registrationEnvelope() []byte {
	formats := []string{"openai", "claude", "openai-response"}
	return okEnvelope(registrationResult{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           pluginName,
			GitHubRepository: githubRepoURL,
			ConfigFields:     []pluginapi.ConfigField{},
		},
		Capabilities: capabilities{
			ModelProvider:         true,
			AuthProvider:          true,
			Executor:              true,
			ExecutorModelScope:    pluginapi.ExecutorModelScopeOAuth,
			ExecutorInputFormats:  formats,
			ExecutorOutputFormats: formats,
			ManagementAPI:         true,
		},
	})
}

func (m *Manager) registerManagement(request []byte) ([]byte, error) {
	var req struct {
		Plugin           pluginapi.Metadata `json:"Plugin"`
		BasePath         string             `json:"BasePath"`
		ResourceBasePath string             `json:"ResourceBasePath"`
	}
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed management registration request body"), nil
	}
	return okEnvelope(struct {
		Routes []struct {
			Method string `json:"method"`
			Path   string `json:"path"`
		} `json:"routes"`
		Resources []struct {
			Path        string `json:"path"`
			Menu        string `json:"menu"`
			Description string `json:"description"`
		} `json:"resources"`
	}{
		Routes: []struct {
			Method string `json:"method"`
			Path   string `json:"path"`
		}{{Method: "POST", Path: "/plugins/" + pluginName + "/quota-usage"},
			{Method: "POST", Path: "/plugins/" + pluginName + "/accounts"},
			{Method: "POST", Path: "/plugins/" + pluginName + "/oauth/complete"},
			{Method: "POST", Path: "/plugins/" + pluginName + "/oauth/session"},
			{Method: "POST", Path: "/plugins/" + pluginName + "/oauth/cancel"}},
		Resources: []struct {
			Path        string `json:"path"`
			Menu        string `json:"menu"`
			Description string `json:"description"`
		}{{Path: "/quota", Menu: "CommandCode", Description: "Manage CommandCode accounts and view quota."},
			{Path: "/oauth"}},
	}), nil
}

func (m *Manager) handleManagement(request []byte) ([]byte, error) {
	var req struct {
		pluginapi.ManagementRequest
		HostCallbackID string `json:"host_callback_id,omitempty"`
	}
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed management request body"), nil
	}
	resp, err := m.HandleManagement(context.Background(), req.ManagementRequest)
	if err != nil {
		return ErrEnvelope("management_failure", err.Error()), nil
	}
	return okEnvelope(resp), nil
}

// handleLifecycle implements plugin.register / plugin.reconfigure: load
// config, refresh the catalog once, publish state, restart the refresh
// loop. Registration succeeds even when the initial refresh fails (FR-002).
func (m *Manager) handleLifecycle(request []byte) ([]byte, error) {
	var req lifecycleRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed lifecycle request body"), nil
	}
	cfg, err := config.Load(req.ConfigYAML)
	if err != nil {
		debugTrace("lifecycle config_error=%s", err.Error())
		return ErrEnvelope("invalid_config", err.Error()), nil
	}
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	m.mu.RLock()
	configChanged := !reflect.DeepEqual(m.cfg, cfg)
	m.mu.RUnlock()
	// Older hosts resend identical plugin config after auth-file mutations.
	// Only a real config change invalidates credentials not yet delivered.
	if configChanged {
		m.clearPendingLoginsLocked()
	}
	debugTrace("lifecycle config_loaded prefix_enabled=%t prefix=%s", cfg.ModelPrefix.Enabled, cfg.ModelPrefix.Value)
	// A nil *HostBridge must not enter the interface as a typed nil, or
	// catalog's nil-client guard never fires and Refresh panics inside Do.
	var client catalog.HostClient
	if m.bridge != nil {
		client = m.bridge
	}
	mgr := catalog.New(cfg, client)
	refreshErr := refreshOnce(context.Background(), mgr, m.bridge, registerRefreshTimeout)
	debugTrace("lifecycle refresh_complete model_count=%d refresh_error=%t", len(mgr.Models()), refreshErr != nil)

	// Retire any running loop and wait for its exit outside m.mu: a mid-refresh
	// tick must never stall readers holding RLock (F4). lifeMu keeps the
	// stop-wait-install sequence atomic against other lifecycles.
	if oldDone := m.closeStop(); oldDone != nil {
		<-oldDone
	}

	m.mu.Lock()
	m.cfg = cfg
	// FR-002 stale-while-unavailable: a failed refresh must neither wipe a
	// non-empty previous snapshot nor keep the OLD manager, whose stored cfg
	// would pin the old base-url/catalog-url/protocols/prefix so ticks
	// fetched the old URL with the new config's keys forever (F5). The NEW
	// manager is adopted unconditionally; when a previous snapshot exists
	// AND the NEW config keeps stale-while-unavailable enabled, it is
	// seeded into the new one, so serving is unchanged until the next
	// successful tick refreshes against the NEW config. Under fail-closed
	// (stale-while-unavailable:false) no seed happens — same as the ticker
	// failure path — so a reconfigure during an outage serves nothing until
	// a refresh succeeds, honoring the operator's policy on BOTH paths.
	if refreshErr != nil && cfg.Catalog.StaleWhileUnavailable && m.mgr != nil && len(m.mgr.Models()) > 0 {
		mgr.SeedFrom(m.mgr)
	}
	m.mgr = mgr
	stop, done := make(chan struct{}), make(chan struct{})
	m.stop, m.done = stop, done
	interval := cfg.Catalog.RefreshInterval
	m.mu.Unlock()

	m.startRefreshLoop(cfg, mgr, interval, stop, done)
	return registrationEnvelope(), nil
}

// authKeyHash is the non-secret digest used to name a newly added key's file.
// Existing file names and runtime IDs remain owned by the host.
func authKeyHash(key string) string {
	digest := sha256.Sum256([]byte(key))
	return hex.EncodeToString(digest[:])
}

// authRecordIDFromHash and authFileNameFromHash are the runtime auth ID and the
// auth file name for one key digest. Both derive from the digest alone so every
// producer agrees without recomputing it.
func authRecordIDFromHash(hash string) string { return ProviderID + "-key-" + hash }
func authFileNameFromHash(hash string) string { return authRecordIDFromHash(hash) + ".json" }

// handleModels implements model.static / model.for_auth (FR-003): the last
// good catalog snapshot mapped to wire ModelInfos; empty catalog yields an
// empty slice, not an error.
func (m *Manager) handleModels() ([]byte, error) {
	m.mu.RLock()
	mgr := m.mgr
	m.mu.RUnlock()
	models := make([]pluginapi.ModelInfo, 0)
	if mgr != nil {
		for _, rec := range mgr.Models() {
			models = append(models, pluginapi.ModelInfo{
				ID:                        rec.PublicID,
				Object:                    "model",
				OwnedBy:                   ProviderID,
				DisplayName:               rec.DisplayName,
				ContextLength:             rec.ContextLimit,
				MaxCompletionTokens:       rec.OutputLimit,
				SupportedInputModalities:  rec.InputModes,
				SupportedOutputModalities: rec.OutputModes,
				Thinking:                  rec.Thinking,
			})
		}
	}
	return okEnvelope(pluginapi.ModelResponse{Provider: ProviderID, Models: models}), nil
}

// On a fresh installation the catalog is empty until the first account is
// added. The host requests models while registering that selected credential;
// discover them here so adding a key never requires a plugin restart.
func (m *Manager) handleModelsForAuth(request []byte) ([]byte, error) {
	var req pluginapi.AuthModelRequest
	if json.Unmarshal(request, &req) != nil {
		return ErrEnvelope("invalid_request", "malformed model request"), nil
	}
	m.mu.RLock()
	mgr := m.mgr
	m.mu.RUnlock()
	if mgr != nil && len(mgr.Models()) == 0 && req.AuthProvider == ProviderID {
		if key := selectedAuthKey(req.Attributes, req.Metadata); key != "" {
			ctx, cancel := context.WithTimeout(context.Background(), registerRefreshTimeout)
			defer cancel()
			_ = mgr.Refresh(ctx, key)
		}
	}
	return m.handleModels()
}

// shutdownDrainTimeout bounds how long handleShutdown waits for orphaned
// host-callback goroutines (timed-out callbacks and their abandon/drain
// cleanup) to finish before returning. Package var so tests can shrink it.
var shutdownDrainTimeout = 15 * time.Second

// handleShutdown stops the refresh loop, drains orphaned host callbacks,
// and clears state. The drain matters on Unix: the SDK loader frees host_api
// and dlclose's the plugin immediately after this export returns
// (loader_unix.go), so a goroutine still calling into the host would crash
// the process — see COMPATIBILITY.md limitations.
func (m *Manager) handleShutdown() ([]byte, error) {
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	m.clearPendingLoginsLocked()
	oldDone := m.closeStop()
	if oldDone != nil {
		<-oldDone
	}
	if m.bridge != nil && !m.bridge.WaitForInFlight(shutdownDrainTimeout) {
		// Non-Windows unload during this residual window can crash the
		// host; the warn makes the stall visible without blocking forever.
		_ = m.bridge.Log("warn", "shutdown proceeding with host callbacks still in flight", nil)
	}
	m.mu.Lock()
	m.cfg, m.mgr = config.Config{}, nil
	m.mu.Unlock()
	return okEnvelope(struct{}{}), nil
}

// startRefreshLoop spawns the single background ticker goroutine over the
// caller-supplied stop/done pair (already stored under m.mu) and the served
// catalog manager. The loop captures its own cfg/mgr/bridge snapshot so
// it never contends on m.mu; reconfigure swaps state and restarts the loop.
func (m *Manager) startRefreshLoop(cfg config.Config, mgr *catalog.Manager, interval time.Duration, stop, done chan struct{}) {
	// F4: tick refresh contexts derive from stopCtx so close(stop) aborts
	// an in-flight Refresh immediately instead of leaving lifeMu held until
	// the old config's request-timeout expires. The watcher goroutine is
	// required because the loop body blocks inside refreshOnce while a tick
	// runs and cannot select on stop itself.
	stopCtx, cancel := context.WithCancel(context.Background())
	go func() {
		<-stop
		cancel()
	}()
	go func() {
		ticker := time.NewTicker(interval)
		cleanupTicker := time.NewTicker(time.Minute)
		defer cleanupTicker.Stop()
		defer close(done)
		defer ticker.Stop()
		defer cancel()
		for {
			select {
			case <-stop:
				return
			case <-cleanupTicker.C:
			case <-ticker.C:
				func() {
					defer func() {
						// CGO bridge calls can panic; a tick must never
						// take the host process down.
						if r := recover(); r != nil && m.bridge != nil {
							_ = m.bridge.Log("error", "catalog refresh panicked", nil)
						}
					}()
					refreshOnce(stopCtx, mgr, m.bridge, cfg.RequestTimeout)
				}()
			}
			// Lifecycle changes hold lifeMu while waiting for this loop to exit.
			// Never wait for that lock here; a busy lifecycle will clean or retry.
			if m.lifeMu.TryLock() {
				m.pruneLoginsLocked()
				m.lifeMu.Unlock()
			}
		}
	}()
}

// closeStop signals a running loop to exit and clears the stop/done pair.
// It returns the loop's done channel — nil when no loop was running — and
// the CALLER waits on it after this returns, never while holding m.mu (F4).
func (m *Manager) closeStop() chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stop == nil {
		return nil
	}
	done := m.done
	close(m.stop)
	m.stop, m.done = nil, nil
	return done
}

// refreshOnce discovers the catalog using the first readable, enabled host
// credential, never a configured key. The bounded context also covers listing
// and reading auth files. Upstream failure does not rotate client credentials.
func refreshOnce(parent context.Context, mgr *catalog.Manager, bridge *HostBridge, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	key := ""
	files, _ := commandcodeAuthFiles(ctx, bridge)
	for _, entry := range files {
		if entry.Disabled || entry.Status == "disabled" {
			continue
		}
		if candidate, err := authFileKey(ctx, bridge, entry); err == nil {
			key = candidate
			break
		}
	}
	err := mgr.Refresh(ctx, key)
	if err != nil {
		if bridge != nil {
			_ = bridge.Log("warn", "catalog refresh failed", map[string]any{"error": err.Error()})
		}
		return err
	}
	// FR-010: surface normalization diagnostics with the post-refresh log.
	warnings := mgr.Warnings()
	if uns := mgr.Unsupported(); len(uns) > 0 {
		parts := make([]string, len(uns))
		for i, u := range uns {
			parts[i] = fmt.Sprintf("%q: %s", u.UpstreamID, u.Reason)
		}
		fields := map[string]any{"models": strings.Join(parts, ", ")}
		if len(warnings) > 0 {
			fields["warnings"] = strings.Join(warnings, "; ")
		}
		_ = bridge.Log("warn", "unsupported models excluded from routable catalog", fields)
	} else if len(warnings) > 0 {
		_ = bridge.Log("warn", "catalog normalization warnings",
			map[string]any{"warnings": strings.Join(warnings, "; ")})
	}
	return nil
}

func okEnvelope(result any) []byte {
	raw, _ := json.Marshal(result)
	out, _ := json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
	return out
}

func ErrEnvelope(code, message string) []byte {
	out, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: &pluginabi.Error{Code: code, Message: message}})
	return out
}
