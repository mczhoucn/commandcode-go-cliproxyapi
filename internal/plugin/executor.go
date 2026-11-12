// Non-stream and stream execution paths (FR-005/FR-006/FR-007, arch §4/§5
// steps 5-8): resolve the public model ID against the catalog snapshot,
// translate the inbound payload to the record's upstream protocol, call
// upstream through the host HTTP callbacks, and translate back to the
// client protocol. Request authentication is selected by CPA.

package plugin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/sjson"

	"commandcode-or-or-cliproxyapi/internal/adapter/chatcompletions"
	"commandcode-or-or-cliproxyapi/internal/adapter/messages"
	"commandcode-or-or-cliproxyapi/internal/adapter/responses"
	"commandcode-or-or-cliproxyapi/internal/adapter/shared"
	"commandcode-or-or-cliproxyapi/internal/catalog"
	"commandcode-or-or-cliproxyapi/internal/config"
	"commandcode-or-or-cliproxyapi/internal/errclass"
)

// executorRequest mirrors rpcExecutorRequest: the SDK embeds
// pluginapi.ExecutorRequest untagged, so its fields marshal under Go field
// names ("Model", "SourceFormat", "OriginalRequest", "Stream"). StreamID is
// the DOWNSTREAM host-allocated id for executor.execute_stream emissions;
// ids returned by DoStream are UPSTREAM and never interchangeable (§4).
type executorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// resolvedExecution carries everything both execution paths need after
// model/key resolution succeeded.
type resolvedExecution struct {
	cfg config.Config
	rec catalog.ModelRecord
	key string
}

// resolveExecution resolves the requested model against the snapshot and
// extracts the key selected by CPA. A non-nil second return is a ready-made
// failure envelope.
func (m *Manager) resolveExecution(req executorRequest) (*resolvedExecution, []byte) {
	m.mu.RLock()
	cfg, mgr := m.cfg, m.mgr
	m.mu.RUnlock()
	if req.AuthProvider != ProviderID {
		return nil, classEnvelope(&errclass.Error{Class: errclass.ClassAuth, Message: "selected auth provider is not commandcode"})
	}
	key := selectedAuthKey(req.AuthAttributes, req.AuthMetadata)
	debugTrace("executor auth model=%s auth_id=%s provider=%s attr_api_key_present=%t attr_count=%d storage_json_bytes=%d", req.Model, req.AuthID, req.AuthProvider, key != "", len(req.AuthAttributes), len(req.StorageJSON))
	if key == "" {
		return nil, classEnvelope(&errclass.Error{Class: errclass.ClassAuth, Message: "selected auth has no api key"})
	}
	var rec catalog.ModelRecord
	var found bool
	if mgr != nil && req.Model != "" {
		rec, found = mgr.Lookup(req.Model)
	}
	if !found {
		return nil, classEnvelope(&errclass.Error{
			Class:      errclass.ClassInvalidModel,
			Message:    "model not in routable catalog",
			StatusCode: http.StatusNotFound,
		})
	}
	return &resolvedExecution{cfg: cfg, rec: rec, key: key}, nil
}

// handleExecute implements executor.execute (non-stream). Stream-flagged
// requests are routed to the stream path instead of rejected.
func (m *Manager) handleExecute(request []byte) ([]byte, error) {
	var req executorRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed executor request body"), nil
	}
	debugTrace("executor invoked model=%s source_format=%s stream=%t original_body_%s payload_%s", req.Model, req.SourceFormat, req.Stream, debugBodyMeta(req.OriginalRequest), debugBodyMeta(req.Payload))
	if req.Stream {
		// executor.execute is the NON-stream RPC: the host allocated no
		// downstream stream id, so the async pump (executeStream) would emit
		// into a stream that does not exist. A stream-flagged body reaching
		// this method must instead be collapsed to a single non-stream
		// round-trip: drop the client's top-level "stream" so upstream
		// answers with a unary body, and clear the flag so the rest of this
		// path treats it as non-stream.
		if stripped, err := sjson.DeleteBytes(req.OriginalRequest, "stream"); err == nil {
			req.OriginalRequest = stripped
		}
		req.Stream = false
	}
	res, failEnv := m.resolveExecution(req)
	if res == nil {
		return failEnv, nil
	}
	sessionID, eErr := resolveCommandCodeSessionID(req)
	if eErr != nil {
		return classEnvelope(eErr), nil
	}
	debugTrace("executor session mode=%s source_format=%s x_commandcode_session=%s fallback=%t", "non-stream", req.SourceFormat, sessionID, sessionID == emptyCommandCodeSessionID)
	upstreamBody, eErr := buildUpstreamRequest(res.rec.Protocol, res.rec.UpstreamID, req.SourceFormat, req.OriginalRequest, res.rec.Thinking)
	if eErr != nil {
		return classEnvelope(eErr), nil
	}

	url := catalog.JoinUpstreamURL(res.cfg.BaseURL, res.rec.EndpointPath)
	debugTrace("executor resolved public_model=%s upstream_model=%s route=%s url=%s", req.Model, res.rec.UpstreamID, res.rec.Protocol, url)
	debugTrace("executor sending non-stream url=%s body_len=%d", url, len(upstreamBody))
	ctx, cancel := context.WithTimeout(context.Background(), res.cfg.RequestTimeout)
	defer cancel()
	resp, err := m.bridge.doWithCallbackID(ctx, pluginapi.HTTPRequest{
		Method:  http.MethodPost,
		URL:     url,
		Headers: upstreamAuthHeaders(res.rec.Protocol, res.key, sessionID),
		Body:    upstreamBody,
	}, req.HostCallbackID)
	if err != nil {
		debugTrace("executor non-stream network error: %v", err)
		return classEnvelope(errclass.FromNetwork(err)), nil
	}
	debugTrace("executor received non-stream status=%d body_len=%d", resp.StatusCode, len(resp.Body))
	if resp.StatusCode >= 400 {
		return classEnvelope(shared.UpstreamStatusErrorWithHeaders(resp.StatusCode, resp.Body, resp.Headers)), nil
	}
	// Parse/envelope guard only; true OOM prevention belongs to the host transport's byte cap.
	if int64(len(resp.Body)) > res.cfg.MaxResponseBytes {
		return classEnvelope(errclass.Translation("response exceeds max-response-bytes")), nil
	}
	converted, eErr := convertNonStream(res.rec.Protocol, req.SourceFormat, resp.StatusCode, resp.Body)
	if eErr != nil {
		return classEnvelope(eErr), nil
	}
	return okEnvelope(pluginapi.ExecutorResponse{Payload: converted, Headers: resp.Headers}), nil
}

func buildUpstreamRequest(route catalog.Route, upstreamModel, sourceFormat string, sourceBody []byte, ts *pluginapi.ThinkingSupport) ([]byte, *errclass.Error) {
	switch route {
	case catalog.RouteChatCompletions:
		return chatcompletions.BuildRequest(upstreamModel, sourceFormat, sourceBody, ts)
	case catalog.RouteMessages:
		return messages.BuildRequest(upstreamModel, sourceFormat, sourceBody, ts)
	case catalog.RouteResponses:
		return responses.BuildRequest(upstreamModel, sourceFormat, sourceBody, ts)
	}
	return nil, errclass.Translation("unsupported route")
}

func upstreamAuthHeaders(route catalog.Route, key, sessionID string) http.Header {
	var h http.Header
	if route == catalog.RouteMessages {
		h = messages.AuthHeaders(key)
	} else {
		// Chat Completions and Responses endpoints are OpenAI-style bearer.
		h = chatcompletions.AuthHeaders(key)
	}
	h.Set("x-commandcode-session", sessionID)
	return h
}

const emptyCommandCodeSessionID = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// resolveCommandCodeSessionID applies the FR-012/AC-H precedence: CPA's canonical
// identity, an explicit inbound session header, then the existing content hash.
func resolveCommandCodeSessionID(req executorRequest) (string, *errclass.Error) {
	if sid, ok := req.Metadata["canonical_session_id"].(string); ok && sid != "" {
		return sid, nil
	}
	for _, name := range []string{
		"X-Session-Affinity",
		"X-Commandcode-Session",
		"X-Session-Id",
		"X-Claude-Code-Session-Id",
		"Session-Id",
	} {
		if sid := req.Headers.Get(name); sid != "" {
			return sid, nil
		}
	}
	return deriveCommandCodeSessionID(req.SourceFormat, req.OriginalRequest)
}

// deriveCommandCodeSessionID hashes the model-visible content of the initial user
// turn before translation (FR-012/AC-H). Metadata is excluded;
// valid requests without a user turn use the fixed empty-input digest.
func deriveCommandCodeSessionID(sourceFormat string, originalRequest []byte) (string, *errclass.Error) {
	var content strings.Builder
	appendParts := func(raw json.RawMessage, target string) *errclass.Error {
		parts, eErr := shared.DecodeStringOrParts(raw, target)
		if eErr != nil {
			return eErr
		}
		for _, part := range parts {
			if part.ImageURL != "" {
				content.WriteString(part.ImageURL)
			} else {
				content.WriteString(part.Text)
			}
		}
		return nil
	}

	switch sourceFormat {
	case "openai":
		var req shared.ChatCompletionsRequest
		if err := json.Unmarshal(originalRequest, &req); err != nil {
			return "", errclass.Translation("malformed openai request JSON: " + err.Error())
		}
		started := false
		for _, msg := range req.Messages {
			if !started {
				if msg.Role == "system" || msg.Role == "developer" {
					continue
				}
				if msg.Role != "user" {
					break
				}
				started = true
			} else if msg.Role != "user" {
				break
			}
			if eErr := appendParts(msg.Content, "/v1/chat/completions"); eErr != nil {
				return "", eErr
			}
		}
	case "claude":
		req, eErr := shared.DecodeClaudeMessages(originalRequest)
		if eErr != nil {
			return "", eErr
		}
		started := false
		for _, msg := range req.Messages {
			if !started {
				if msg.Role != "user" {
					continue
				}
				started = true
			} else if msg.Role != "user" {
				break
			}
			if msg.Content != "" {
				content.WriteString(msg.Content)
			}
			for _, block := range msg.Blocks {
				switch block.Kind {
				case "text":
					content.WriteString(block.Text)
				case "image":
					content.WriteString(block.URL)
				case "tool_result":
					text, eErr := shared.ToolResultText(block.Result, "tool messages carry text only")
					if eErr != nil {
						return "", eErr
					}
					content.WriteString(text)
				}
			}
		}
	case "openai-response":
		var req shared.ResponsesRequest
		if err := json.Unmarshal(originalRequest, &req); err != nil {
			return "", errclass.Translation("malformed openai-response request JSON: " + err.Error())
		}
		items, eErr := req.DecodeInputItems()
		if eErr != nil {
			return "", eErr
		}
		started := false
		for _, item := range items {
			isUserMessage := item.Role == "user" && (item.Type == "message" || item.Type == "")
			if !started {
				if !isUserMessage {
					continue
				}
				started = true
			} else if !isUserMessage {
				break
			}
			if eErr := appendParts(item.Content, "/v1/responses"); eErr != nil {
				return "", eErr
			}
		}
	default:
		return "", shared.UnsupportedFormat(sourceFormat, "CommandCode session derivation")
	}

	digest := sha256.Sum256([]byte(content.String()))
	return hex.EncodeToString(digest[:]), nil
}

// convertNonStream routes one upstream response to its adapter's uniform
// translator: every adapter owns status classification (>=400 → §7
// classified errors), native passthrough, and cross-format conversion for
// all client formats.
func convertNonStream(route catalog.Route, sourceFormat string, status int, body []byte) ([]byte, *errclass.Error) {
	switch route {
	case catalog.RouteChatCompletions:
		return chatcompletions.ConvertNonStreamResponse(sourceFormat, status, body)
	case catalog.RouteMessages:
		return messages.ConvertNonStreamResponse(sourceFormat, status, body)
	case catalog.RouteResponses:
		return responses.ConvertNonStreamResponse(sourceFormat, status, body)
	}
	return nil, errclass.Translation("unsupported route")
}

// classEnvelope renders a classified failure as the wire error envelope;
// ToEnvelopeError redacts and propagates Retryable/HTTPStatus host-side.
func classEnvelope(e *errclass.Error) []byte {
	wire := errclass.ToEnvelopeError(e)
	out, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: &wire})
	return out
}

// streamConverter is the common shape of the three adapters' stream
// converters: feed one upstream SSE chunk, get translated client events.
type streamConverter interface {
	Feed(chunk []byte) (events [][]byte, done bool, eErr *errclass.Error)
}

// Compile-time proof the close-without-terminal Flush seam (F5) picks up
// both converters that can hold a deferred terminal.
var (
	_ interface{ Flush() [][]byte } = (*chatcompletions.StreamConverter)(nil)
	_ interface{ Flush() [][]byte } = (*messages.StreamConverter)(nil)
)

func newStreamConverter(route catalog.Route, sourceFormat string) streamConverter {
	switch route {
	case catalog.RouteMessages:
		return messages.NewStreamConverter(sourceFormat)
	case catalog.RouteResponses:
		return responses.NewStreamConverter(sourceFormat)
	}
	return chatcompletions.NewStreamConverter(sourceFormat)
}

// handleExecuteStream implements executor.execute_stream (FR-006, §7).
// It decodes once and delegates to executeStream so a stream-flagged
// request arriving via executor.execute is never parsed twice.
func (m *Manager) handleExecuteStream(request []byte) ([]byte, error) {
	var req executorRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed executor request body"), nil
	}
	debugTrace("executor stream invoked model=%s source_format=%s stream=%t original_body_%s payload_%s", req.Model, req.SourceFormat, req.Stream, debugBodyMeta(req.OriginalRequest), debugBodyMeta(req.Payload))
	return m.executeStream(req)
}

// executeStream runs the already-decoded stream execution: pre-first-byte errors
// (invalid request, unroutable model, upstream HTTP >=400) return immediately as
// error envelopes so CPA can failover pre-emission.
// On success (upstream HTTP 200 OK), the reading and emitting pump loop runs in
// a background goroutine and executeStream returns okEnvelope immediately so the
// host can start draining chunks to the downstream client without buffer deadlocks.
func (m *Manager) executeStream(req executorRequest) ([]byte, error) {
	res, failEnv := m.resolveExecution(req)
	if res == nil {
		return failEnv, nil
	}
	sessionID, eErr := resolveCommandCodeSessionID(req)
	if eErr != nil {
		return classEnvelope(eErr), nil
	}
	debugTrace("executor session mode=%s source_format=%s x_commandcode_session=%s fallback=%t", "stream", req.SourceFormat, sessionID, sessionID == emptyCommandCodeSessionID)
	upstreamBody, eErr := buildUpstreamRequest(res.rec.Protocol, res.rec.UpstreamID, req.SourceFormat, req.OriginalRequest, res.rec.Thinking)
	if eErr != nil {
		return classEnvelope(eErr), nil
	}

	url := catalog.JoinUpstreamURL(res.cfg.BaseURL, res.rec.EndpointPath)
	debugTrace("executor sending stream url=%s body_len=%d", url, len(upstreamBody))
	ctx, cancel := context.WithTimeout(context.Background(), res.cfg.RequestTimeout)
	defer cancel()
	st, respHeaders, id, err := m.bridge.doStreamWithCallbackID(ctx, pluginapi.HTTPRequest{
		Method:  http.MethodPost,
		URL:     url,
		Headers: upstreamAuthHeaders(res.rec.Protocol, res.key, sessionID),
		Body:    upstreamBody,
	}, req.HostCallbackID)
	debugTrace("executor stream DoStream status=%d upstreamID=%s err=%v", st, id, err)
	if err != nil {
		return classEnvelope(errclass.FromNetwork(err)), nil
	}
	if st >= 400 {
		var body []byte
		if id != "" {
			body = m.readErrorBody(ctx, id)
			_ = m.bridge.StreamClose(id)
		}
		// Headers carry the upstream Retry-After hint, which CPA turns into the
		// cooldown deadline for this credential or model.
		return classEnvelope(shared.UpstreamStatusErrorWithHeaders(st, body, respHeaders)), nil
	}

	downID := req.StreamID
	if m.bridge != nil {
		m.bridge.inFlight.Add(1)
	}
	go func() {
		defer func() {
			if r := recover(); r != nil && m.bridge != nil {
				// A panic in the pump goroutine would kill the host process:
				// recover, log the failure (secret-free), and ensure both
				// streams are closed with an error. The deferred inFlight.Done
				// below still runs after this recover.
				_ = m.bridge.Log("error", "stream pump panicked", nil)
				_ = m.bridge.StreamClose(id)
				_ = m.bridge.StreamCloseDownstream(downID, "internal error")
			}
		}()
		if m.bridge != nil {
			defer m.bridge.inFlight.Done()
		}
		m.pumpStream(downID, id, res, req.SourceFormat)
	}()
	return okEnvelope(struct{}{}), nil
}

// errorBodyReadTimeout bounds the pre-first-byte error-body drain. A failing
// upstream has already sent its status line; its error JSON is small and
// should arrive promptly, so a stalled body must not park the executor —
// classification proceeds with whatever arrived before the deadline.
const errorBodyReadTimeout = 3 * time.Second

// errorBodyMaxBytes caps the drained error body. Upstream error envelopes are
// a few hundred bytes; the cap stops a hostile or buggy upstream from
// streaming an unbounded body into the classifier's memory. The first chunk
// that crosses the cap is kept whole (so a complete small JSON is never split)
// and the drain then stops.
const errorBodyMaxBytes = 64 << 10

// readErrorBody drains an UPSTREAM error stream (HTTP >=400) across however
// many chunks the host hands back, bounded by errorBodyReadTimeout (shared
// with, but never exceeding, the request deadline carried by ctx) and
// errorBodyMaxBytes. It returns the concatenated payload, or the first
// upstream-reported errMsg when no payload bytes arrive. A read error or a
// clean done both stop the drain; the caller still issues StreamClose.
func (m *Manager) readErrorBody(ctx context.Context, upstreamID string) []byte {
	if m.bridge == nil {
		return nil
	}
	readCtx, cancel := context.WithTimeout(ctx, errorBodyReadTimeout)
	defer cancel()
	var body []byte
	var firstErrMsg string
	for {
		payload, errMsg, done, readErr := m.bridge.StreamReadContext(readCtx, upstreamID)
		if readErr != nil {
			// A deadline hit or transport failure stops the drain; whatever
			// arrived so far is still worth classifying.
			break
		}
		if len(payload) > 0 {
			body = append(body, payload...)
		}
		if firstErrMsg == "" && errMsg != "" {
			firstErrMsg = errMsg
		}
		if len(body) >= errorBodyMaxBytes || done {
			break
		}
	}
	if len(body) > 0 {
		return body
	}
	if firstErrMsg != "" {
		return []byte(firstErrMsg)
	}
	return nil
}

func (m *Manager) pumpStream(downID, upstreamID string, res *resolvedExecution, sourceFormat string) {
	var closeOnce sync.Once
	// closeStreams ends both streams. A structured detail is passed through so
	// CPA can apply its configured cooldown policy to a post-first-byte failure
	// instead of falling back to host defaults; nil keeps the plain-message path.
	closeStreams := func(downErrMsg string, eErr *errclass.Error) {
		closeOnce.Do(func() {
			_ = m.bridge.StreamClose(upstreamID)
			if eErr != nil {
				wire := errclass.ToEnvelopeError(eErr)
				label := downErrMsg
				if label == "" {
					label = wire.Message
				}
				_ = m.bridge.StreamCloseDownstreamError(downID, label, &wire)
				return
			}
			_ = m.bridge.StreamCloseDownstream(downID, downErrMsg)
		})
	}
	defer closeStreams("", nil)

	// The watchdog and the pump goroutine both want to close the upstream
	// stream, and exactly one must win: a CAS on pumpState decides. The
	// watchdog closes (to unblock a StreamRead parked past the deadline) only
	// if it flips pumpRunning->pumpAborted first; a pump that already reached
	// pumpCompleted makes the watchdog a no-op. watchdogDone lets the pump
	// wait out a watchdog callback that is still running inside the host, so
	// shutdown's inFlight drain cannot race a StreamClose mid-flight.
	var pumpState atomic.Int32 // pumpRunning
	watchdogDone := make(chan struct{})
	watchdog := time.AfterFunc(res.cfg.RequestTimeout, func() {
		defer close(watchdogDone)
		defer func() {
			if r := recover(); r != nil && m.bridge != nil {
				_ = m.bridge.Log("error", "stream watchdog panicked", nil)
			}
		}()
		if pumpState.CompareAndSwap(pumpRunning, pumpAborted) {
			_ = m.bridge.StreamClose(upstreamID)
		}
	})
	defer func() {
		// Stop()==true: the timer never fired, so the callback never ran and
		// watchdogDone will never close — do not wait on it. Stop()==false:
		// the callback fired (or is running); block until it returns before
		// the goroutine's inFlight.Done lets shutdown proceed.
		if !watchdog.Stop() {
			<-watchdogDone
		}
	}()

	conv := newStreamConverter(res.rec.Protocol, sourceFormat)
	var terminal terminalScanner
	var (
		total          int64
		upstreamClosed bool
		convDone       bool
		emittedAny     bool
	)
	for {
		payload, readErrMsg, closed, err := m.bridge.StreamRead(upstreamID)
		debugTrace("executor stream read chunk_len=%d closed=%t readErrMsg=%q err=%v", len(payload), closed, readErrMsg, err)
		upstreamClosed = closed
		if pumpState.Load() == pumpAborted {
			closeStreams(errclass.Redact("stream exceeded request-timeout"), errclass.PostFirstByteTruncated("stream truncated after first byte"))
			return
		}
		if err != nil {
			closeStreams(errclass.Redact(err.Error()), errclass.PostFirstByteTruncated("stream truncated after first byte"))
			return
		}
		if readErrMsg != "" {
			closeStreams(errclass.Redact(readErrMsg), errclass.PostFirstByteTruncated("stream truncated after first byte"))
			return
		}
		total += int64(len(payload))
		if total > res.cfg.MaxResponseBytes {
			closeStreams(errclass.Redact("stream exceeded max-response-bytes"), errclass.PostFirstByteTruncated("stream truncated after first byte"))
			return
		}
		// Record whether a logical terminal marker ever crossed the wire, so a
		// clean upstream close can be told apart from a mid-response cutoff
		// (the converter only sets done on the [DONE] sentinel).
		terminal.feed(payload)
		events, done, convErr := conv.Feed(payload)
		if convErr != nil {
			// CommandCode can embed an error object inside the SSE stream (the
			// converter classifies it through FromStatus/UpstreamFallback).
			// When that classification is account- or model-level — the
			// credential is exhausted, the model is gone, the request is
			// defective — forward it verbatim so CPA's cooldown pipeline sees
			// the real reason instead of a generic truncation. Everything else
			// (a malformed chunk, a plain upstream hiccup) stays a
			// post-first-byte truncation: no cooldown, no replay.
			if significantStreamConvError(convErr) {
				fwd := *convErr
				if emittedAny {
					// Bytes already reached the client; a replay would deliver
					// them twice, so a forwarded classification must never
					// re-enable retry past the first downstream byte.
					fwd.Retryable = false
				}
				closeStreams(errclass.Redact(fwd.Message), &fwd)
				return
			}
			closeStreams(errclass.Redact(convErr.Message), errclass.PostFirstByteTruncated("stream truncated after first byte"))
			return
		}
		if emitErr := m.emitAll(downID, events); emitErr != nil {
			// A downstream write failure is the client going away, not an
			// upstream health signal: the connection-lifecycle code keeps CPA
			// from cooling a perfectly good credential for it.
			closeStreams(errclass.Redact(emitErr.Error()), errclass.PostFirstByteConnectionLifecycle("downstream consumer closed"))
			return
		}
		if len(events) > 0 {
			emittedAny = true
		}
		convDone = done
		if convDone || upstreamClosed {
			// Claim completion so a watchdog that has not yet fired becomes a
			// no-op; if it already won the CAS the top-of-loop check caught it.
			pumpState.CompareAndSwap(pumpRunning, pumpCompleted)
			break
		}
	}
	debugTrace("executor stream loop end convDone=%t upstreamClosed=%t terminalSeen=%t", convDone, upstreamClosed, terminal.seen)
	if !convDone && upstreamClosed {
		if !terminal.seen {
			// Upstream closed without the [DONE] sentinel AND without any
			// logical terminal (finish_reason / message_stop /
			// response.completed): the connection was cut mid-response.
			// Synthesizing a clean end here would misreport a truncated answer
			// as complete, so report a post-first-byte truncation instead. Like
			// every post-first-byte failure it is non-retryable and carries no
			// cooldown — the bytes already emitted stay with the client.
			closeStreams(errclass.Redact("upstream closed before stream terminal"), errclass.PostFirstByteTruncated("stream truncated before terminal event"))
			return
		}
		// A terminal arrived but [DONE] did not: the converter may be holding a
		// deferred terminal (message_stop / response.completed) that must still
		// reach the client before the clean close.
		if flusher, ok := conv.(interface{ Flush() [][]byte }); ok {
			flushed := flusher.Flush()
			if emitErr := m.emitAll(downID, flushed); emitErr != nil {
				closeStreams(errclass.Redact(emitErr.Error()), errclass.PostFirstByteConnectionLifecycle("downstream consumer closed"))
				return
			}
		}
	}
}

// significantStreamConvError reports whether a mid-stream converter error
// carries account- or model-level meaning CPA must act on (cool the
// credential, retire the model, stop rotation) rather than a generic stream
// fault. For these the executor forwards the converter's own classification so
// the cooldown pipeline sees the real reason; everything else collapses to a
// post-first-byte truncation.
func significantStreamConvError(e *errclass.Error) bool {
	if e == nil {
		return false
	}
	if e.CredentialScoped {
		return true
	}
	switch e.WireCode {
	case pluginabi.ErrorCodeCredentialQuota,
		pluginabi.ErrorCodeModelNotFound,
		pluginabi.ErrorCodeRequestScoped,
		pluginabi.ErrorCodeForceCooldown:
		return true
	}
	switch e.Class {
	case errclass.ClassAuth, errclass.ClassBilling, errclass.ClassQuota, errclass.ClassInvalidModel:
		return true
	}
	return false
}

// pump goroutine lifecycle states for the watchdog CAS. Exactly one of the
// watchdog (pumpAborted) and the pump loop (pumpCompleted) flips pumpRunning,
// deciding who owns the upstream StreamClose.
const (
	pumpRunning int32 = iota
	pumpCompleted
	pumpAborted
)

// terminalScanner watches the raw UPSTREAM SSE bytes for any marker that ends
// a response — the [DONE] sentinel, a non-null finish_reason, Anthropic's
// message_stop, or Responses' response.completed. It exists so the pump can
// distinguish a clean close that merely omitted [DONE] (terminal seen, flush
// and close cleanly) from a connection cut mid-response (nothing seen, report
// a truncation). A short tail is carried between chunks so a marker split
// across two host reads is still matched.
type terminalScanner struct {
	seen bool
	tail []byte
}

var terminalMarkers = [][]byte{
	[]byte("[DONE]"),
	[]byte(`"finish_reason":"`), // a quoted value is non-null; ":null" never matches
	[]byte("message_stop"),
	[]byte("response.completed"),
}

// terminalScanCarry is one byte shorter than the longest marker, enough to
// re-join a marker straddling a chunk boundary without unbounded buffering.
const terminalScanCarry = len("response.completed") - 1

func (ts *terminalScanner) feed(chunk []byte) {
	if ts.seen || len(chunk) == 0 {
		return
	}
	buf := make([]byte, 0, len(ts.tail)+len(chunk))
	buf = append(buf, ts.tail...)
	buf = append(buf, chunk...)
	for _, mk := range terminalMarkers {
		if bytes.Contains(buf, mk) {
			ts.seen = true
			ts.tail = nil
			return
		}
	}
	if len(buf) > terminalScanCarry {
		buf = buf[len(buf)-terminalScanCarry:]
	}
	ts.tail = append(ts.tail[:0], buf...)
}

// emitAll feeds converted events downstream in order. StreamEmit is
// deadline-bounded (emitTimeout), so a host that stops draining the
// downstream stream surfaces here as an error; the pump loop treats that
// as a post-first-byte stream-fatal failure and runs fail(), whose
// Once-closer releases both streams.
func (m *Manager) emitAll(downStreamID string, events [][]byte) error {
	for _, evt := range events {
		if err := m.bridge.StreamEmit(downStreamID, evt); err != nil {
			return err
		}
	}
	return nil
}
