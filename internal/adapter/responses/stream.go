package responses

import (
	"encoding/json"
	"fmt"
	"strings"

	"commandcode-or-or-cliproxyapi/internal/adapter/shared"
	"commandcode-or-or-cliproxyapi/internal/errclass"
)

// StreamConverter converts an upstream /v1/responses SSE stream into the
// client protocol incrementally (FR-006, AC §D; Luna REQUIRED v1 scope per
// 07-open-questions.md §5).
//
// Contract: instances are single-use and strictly sequential — Feed is
// called from one goroutine with consecutive network chunks; there is no
// internal locking. Partial SSE lines are buffered until a blank line
// completes the `event:`/`data:` pair.
type StreamConverter struct {
	framer *shared.SSEFramer
	source string

	// Response identity captured from response.created.
	id      string
	model   string
	created int64

	// openai (Chat Completions) target state.
	toolCallsSeen bool

	// claude (Messages) target state.
	textIndex  int
	textOpen   bool
	nextIndex  int
	openBlocks []int

	// Reasoning (thinking) state for cross-format conversion (fix #2).
	// Upstream Responses reasoning events are converted to reasoning_content
	// (OpenAI target) or thinking blocks (Claude target).
	thinkIndex int
	thinkOpen  bool
	thinkBuf   strings.Builder

	// Flush state: once flushed, subsequent Flush calls return nil (fix #7).
	// started records that response.created was observed, so Flush never
	// emits a terminal for a stream that never opened (which would be
	// malformed: message_delta/message_stop without a message_start).
	flushed bool
	started bool

	// Shared function_call announce-or-replay decision table for both
	// conversion targets; an instance converts to exactly one target,
	// so nextIndex doubles as the tool block index allocator.
	tracker *toolCallTracker
}

// NewStreamConverter builds a converter for sourceFormat ("openai",
// "claude", "openai-response"); unknown formats fail on first Feed with
// ClassUnsupported (same classes as BuildRequest).
func NewStreamConverter(sourceFormat string) *StreamConverter {
	sc := &StreamConverter{
		// wantRaw only for the openai-response passthrough, which
		// forwards verbatim blocks; a rebuilt single data line would
		// embed raw newlines from legally multi-data-line frames and
		// corrupt native framing. Conversion targets parse the joined
		// payload and never read raw.
		framer:    shared.NewSSEFramer(sourceFormat == "openai-response"),
		source:    sourceFormat,
		id:        "commandcode",
		textIndex: -1,
	}
	sc.tracker = newToolCallTracker(sc.allocIndex)
	return sc
}

// allocIndex hands out the next block/entry index, shared by text and
// tool blocks in Messages vocabulary and by tool entries alone in Chat
// Completions vocabulary (only one target runs per converter instance).
func (sc *StreamConverter) allocIndex() int {
	i := sc.nextIndex
	sc.nextIndex++
	return i
}

// toolCallTracker is the single announce-or-replay decision table for
// upstream function_call items, keyed by call_id and owned by both
// conversion targets so their delivery decisions cannot diverge:
// announce once per call, deliver complete arguments the moment they are
// observed (at output_item.added or .done alike), and replay at done
// only when nothing was delivered earlier (FR-006).
//
// OpenAI-conformant Responses streams identify one item in TWO distinct
// namespaces: output_item events carry call_id ("call_…") while
// argument-delta events reference the item's own id ("fc_…"). Every
// registration therefore binds BOTH identifiers to the same state and
// lookups resolve through either before creating new state — otherwise
// each delta fragment would fork a second tracker state and clients
// would see a ghost tool entry beside the announced call.
type toolCallTracker struct {
	calls map[string]*toolCallState
	alloc func() int
}

type toolCallState struct {
	index     int
	delivered bool
}

func newToolCallTracker(alloc func() int) *toolCallTracker {
	return &toolCallTracker{calls: map[string]*toolCallState{}, alloc: alloc}
}

// Observe records an output_item.added/.done sighting of a function_call
// (callID from item.call_id, itemID from the item's own "id" field) and
// reports whether this is the first sight (the caller must emit its
// announce frame), the call's stable index, and whether complete
// arguments must be delivered with this event.
func (t *toolCallTracker) Observe(callID, itemID string, argsComplete bool) (first bool, index int, deliverArgs bool) {
	first, st := t.state(callID, itemID)
	if argsComplete && !st.delivered {
		st.delivered = true
		deliverArgs = true
	}
	return first, st.index, deliverArgs
}

// StreamArgs records argument fragments streamed outside item events;
// any fragment counts as delivered, so a later done cannot replay
// duplicates. It reports whether the call was first seen here (the
// caller must open its entry/block defensively).
func (t *toolCallTracker) StreamArgs(itemID string) (first bool, index int) {
	first, st := t.state(itemID, "")
	st.delivered = true
	return first, st.index
}

// state resolves key, then alias, binding whichever identifiers are
// present to the shared state so both namespaces stay one call.
func (t *toolCallTracker) state(key, alias string) (bool, *toolCallState) {
	if st, ok := t.calls[key]; ok {
		t.bind(alias, st)
		return false, st
	}
	if st, ok := t.calls[alias]; ok {
		t.bind(key, st)
		return false, st
	}
	st := &toolCallState{index: t.alloc()}
	t.bind(key, st)
	t.bind(alias, st)
	return true, st
}

func (t *toolCallTracker) bind(key string, st *toolCallState) {
	if key != "" {
		t.calls[key] = st
	}
}

// ---- upstream Responses SSE payload shapes ----
//
// One typed struct per handled event type, keyed off the framer's event
// name; unknown keys are ignored and absent members stay zero-valued at
// no cost. Unknown EVENT types are JSON-validated without decoding and
// ignored.

type usageCounts struct {
	InputTokens  float64 `json:"input_tokens"`
	OutputTokens float64 `json:"output_tokens"`
	InputDetails *struct {
		CachedTokens     *float64 `json:"cached_tokens"`
		CacheWriteTokens *float64 `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
	OutputDetails *struct {
		ReasoningTokens *float64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

type errorMessage struct {
	Message string `json:"message"`
}

type responseMeta struct {
	ID         string       `json:"id"`
	Model      string       `json:"model"`
	CreatedAt  float64      `json:"created_at"`
	StatusCode float64      `json:"status_code"`
	Usage      usageCounts  `json:"usage"`
	Error      errorMessage `json:"error"`
}

type functionCallItem struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type createdEvent struct {
	Response responseMeta `json:"response"`
}

type textDeltaEvent struct {
	Delta string `json:"delta"`
}

type itemEvent struct {
	Item functionCallItem `json:"item"`
}

type argsDeltaEvent struct {
	ItemID string `json:"item_id"`
	Delta  string `json:"delta"`
}

type reasoningDeltaEvent struct {
	Delta string `json:"delta"`
}

type terminalEvent struct {
	Response responseMeta `json:"response"`
}

type failureEvent struct {
	StatusCode float64      `json:"status_code"`
	Message    string       `json:"message"`
	Response   responseMeta `json:"response"`
}

// decodeEvent parses one SSE data payload into its typed shape; malformed
// JSON yields a translation failure carrying only a short redacted
// snippet (FR-006, §5 security: no upstream body echo). An empty payload
// decodes to zero values, matching the legacy empty-map behavior.
func decodeEvent[T any](eventType, payload string, v *T) *errclass.Error {
	if payload == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(payload), v); err != nil {
		return errclass.Translation(fmt.Sprintf(
			"malformed %s event payload: %s", eventType, shared.RedactedSnippet(payload)))
	}
	return nil
}

// checkTarget rejects unknown conversion sources after payload decode, so
// malformed-payload translation failures keep their pre-existing
// precedence over ClassUnsupported.
func (sc *StreamConverter) checkTarget() *errclass.Error {
	if sc.source == "openai" || sc.source == "claude" {
		return nil
	}
	return shared.UnsupportedFormat(sc.source, EndpointPath)
}

// Feed consumes one network chunk and returns synthesized client events,
// whether the upstream stream reached a terminal state, and a classified
// error for failed/malformed streams (FR-006). Native passthrough frames
// are forwarded verbatim without JSON validation; conversion targets
// parse eagerly into typed per-event structs.
func (sc *StreamConverter) Feed(chunk []byte) (events [][]byte, done bool, eErr *errclass.Error) {
	sc.framer.Push(chunk)
	for {
		eventType, payload, raw, ok := sc.framer.Next()
		if !ok {
			return events, done, nil
		}
		if sc.source == "openai-response" {
			evs, d, dErr := sc.passthroughEvent(eventType, payload, raw)
			events = append(events, evs...)
			if dErr != nil {
				return events, false, dErr
			}
			done = done || d
			continue
		}
		evs, d, dErr := sc.convertEvent(eventType, payload)
		events = append(events, evs...)
		if dErr != nil {
			return events, false, dErr
		}
		done = done || d
	}
}

// convertEvent routes one complete SSE frame to a conversion target:
// handled event types decode into their typed structs and share the
// field extraction below; unknown types only validate their payload.
func (sc *StreamConverter) convertEvent(eventType, payload string) ([][]byte, bool, *errclass.Error) {
	switch eventType {
	case "response.created":
		var ev createdEvent
		if eErr := decodeEvent(eventType, payload, &ev); eErr != nil {
			return nil, false, eErr
		}
		if eErr := sc.checkTarget(); eErr != nil {
			return nil, false, eErr
		}
		sc.captureResponse(&ev.Response)
		if sc.source == "openai" {
			return [][]byte{sc.chatChunks().RoleChunk()}, false, nil
		}
		// Input token count only becomes known at completion; Claude
		// clients read authoritative usage from message_delta, so
		// starting at zero is lossless here.
		return [][]byte{sc.claudeChunks().MessageStart(0)}, false, nil
	case "response.output_text.delta":
		var ev textDeltaEvent
		if eErr := decodeEvent(eventType, payload, &ev); eErr != nil {
			return nil, false, eErr
		}
		if eErr := sc.checkTarget(); eErr != nil {
			return nil, false, eErr
		}
		return sc.textDelta(ev.Delta)
	case "response.output_item.added", "response.output_item.done":
		var ev itemEvent
		if eErr := decodeEvent(eventType, payload, &ev); eErr != nil {
			return nil, false, eErr
		}
		if eErr := sc.checkTarget(); eErr != nil {
			return nil, false, eErr
		}
		return sc.outputItem(&ev.Item)
	case "response.function_call_arguments.delta":
		var ev argsDeltaEvent
		if eErr := decodeEvent(eventType, payload, &ev); eErr != nil {
			return nil, false, eErr
		}
		if eErr := sc.checkTarget(); eErr != nil {
			return nil, false, eErr
		}
		return sc.argsFragment(ev.ItemID, ev.Delta)
	case "response.completed", "response.incomplete":
		var ev terminalEvent
		if eErr := decodeEvent(eventType, payload, &ev); eErr != nil {
			return nil, false, eErr
		}
		if eErr := sc.checkTarget(); eErr != nil {
			return nil, false, eErr
		}
		details := shared.UsageDetails{}
		if ev.Response.Usage.InputDetails != nil {
			if ev.Response.Usage.InputDetails.CachedTokens != nil {
				v := int64(*ev.Response.Usage.InputDetails.CachedTokens)
				details.CachedTokens = &v
			}
			if ev.Response.Usage.InputDetails.CacheWriteTokens != nil {
				v := int64(*ev.Response.Usage.InputDetails.CacheWriteTokens)
				details.CacheWriteTokens = &v
			}
		}
		if ev.Response.Usage.OutputDetails != nil && ev.Response.Usage.OutputDetails.ReasoningTokens != nil {
			v := int64(*ev.Response.Usage.OutputDetails.ReasoningTokens)
			details.ReasoningTokens = &v
		}
		return sc.terminal(eventType == "response.incomplete", int(ev.Response.Usage.InputTokens), int(ev.Response.Usage.OutputTokens), details)
	case "response.failed", "error":
		var ev failureEvent
		if eErr := decodeEvent(eventType, payload, &ev); eErr != nil {
			return nil, false, eErr
		}
		if eErr := sc.checkTarget(); eErr != nil {
			return nil, false, eErr
		}
		return nil, false, failureError(&ev, []byte(payload))
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		// Fix #2: Upstream reasoning is now replayed for cross-format clients.
		// OpenAI clients receive reasoning_content deltas, Claude clients
		// receive thinking blocks. Responses passthrough handled by earlier
		// passthroughEvent; this case only runs for conversion targets.
		var ev reasoningDeltaEvent
		if eErr := decodeEvent(eventType, payload, &ev); eErr != nil {
			return nil, false, eErr
		}
		if eErr := sc.checkTarget(); eErr != nil {
			return nil, false, eErr
		}
		return sc.reasoningDelta(ev.Delta)
	case "response.reasoning_text.done", "response.reasoning_summary_text.done",
		"response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		// Fix #2: Lifecycle events validated and ignored (delta carries content).
		// OpenAI/Claude don't have separate done events for reasoning.
		if eErr := sc.checkTarget(); eErr != nil {
			return nil, false, eErr
		}
		var ignored map[string]any
		if eErr := decodeEvent(eventType, payload, &ignored); eErr != nil {
			return nil, false, eErr
		}
		return nil, false, nil
	default:
		// Informational events (response.in_progress, annotations) have no
		// client equivalent and are omitted per the FR-005/FR-006
		// compatibility policy; the payload is validated without
		// materializing a discarded generic graph.
		if payload != "" && !json.Valid([]byte(payload)) {
			return nil, false, errclass.Translation(fmt.Sprintf(
				"malformed %s event payload: %s", eventType, shared.RedactedSnippet(payload)))
		}
		return nil, false, nil
	}
}

// passthroughEvent echoes native Responses frames verbatim (the framer's
// raw block, byte-identical to the upstream bytes); terminal events flip
// done. Failure/error payloads are the only ones parsed — best-effort, so
// an unparseable failure degrades to a retryable upstream error rather
// than failing the passthrough contract.
func (sc *StreamConverter) passthroughEvent(eventType, payload string, raw []byte) ([][]byte, bool, *errclass.Error) {
	switch eventType {
	case "response.completed", "response.incomplete":
		return [][]byte{raw}, true, nil
	case "response.failed", "error":
		var ev failureEvent
		json.Unmarshal([]byte(payload), &ev)
		return nil, false, failureError(&ev, []byte(payload))
	default:
		return [][]byte{raw}, false, nil
	}
}

// textDelta emits one output text delta: a plain Chat Completions content
// chunk, or a Messages content_block_delta that auto-opens (and may
// reopen) the text block.
func (sc *StreamConverter) textDelta(delta string) ([][]byte, bool, *errclass.Error) {
	if sc.source == "openai" {
		return [][]byte{sc.chatChunks().Delta(map[string]any{"content": delta})}, false, nil
	}
	var out [][]byte
	if !sc.textOpen {
		// A tool_use block start closed the previous text block; a
		// later text delta (legal Responses interleaving) reopens a
		// fresh text block at the next free index to keep the
		// mandated start/delta/stop pairing per block index (same
		// stop/reopen pattern as the Chat Completions route). Any
		// still-open block stops first: close-before-next-start.
		out = append(out, sc.closeOpenBlocks()...)
		sc.textIndex = sc.allocIndex()
		sc.textOpen = true
		sc.openBlocks = append(sc.openBlocks, sc.textIndex)
		out = append(out, sc.claudeChunks().ContentBlockStart(sc.textIndex, "text", map[string]any{"text": ""}))
	}
	out = append(out, sc.claudeChunks().ContentBlockDelta(sc.textIndex,
		map[string]any{"type": "text_delta", "text": delta}))
	return out, false, nil
}

// reasoningDelta converts upstream Responses reasoning events to the target
// format (fix #2): reasoning_content deltas for OpenAI Chat Completions
// targets, thinking blocks for Claude Messages targets. Responses passthrough
// is handled by passthroughEvent and never reaches here.
func (sc *StreamConverter) reasoningDelta(delta string) ([][]byte, bool, *errclass.Error) {
	sc.thinkBuf.WriteString(delta)
	if sc.source == "openai" {
		// OpenAI Chat Completions: emit reasoning_content in the delta.
		// The shared.ReasoningFields kernel handles both reasoning_content
		// and reasoning_details spellings.
		return [][]byte{sc.chatChunks().Delta(map[string]any{"reasoning_content": delta})}, false, nil
	}
	// Claude Messages: emit thinking blocks with auto-open on first delta.
	var out [][]byte
	if !sc.thinkOpen {
		sc.thinkIndex = sc.allocIndex()
		sc.thinkOpen = true
		sc.openBlocks = append(sc.openBlocks, sc.thinkIndex)
		out = append(out, sc.claudeChunks().ContentBlockStart(sc.thinkIndex, "thinking", map[string]any{"thinking": ""}))
	}
	out = append(out, sc.claudeChunks().ContentBlockDelta(sc.thinkIndex,
		map[string]any{"type": "thinking_delta", "thinking": delta}))
	return out, false, nil
}

// outputItem handles output_item.added/.done through the shared tracker:
// Chat Completions announces/replays tool_calls entries, Messages opens
// tool_use blocks with mandated pairing; non-function items are ignored.
func (sc *StreamConverter) outputItem(item *functionCallItem) ([][]byte, bool, *errclass.Error) {
	if item.Type != "function_call" {
		return nil, false, nil
	}
	sc.toolCallsSeen = true
	first, idx, deliver := sc.tracker.Observe(item.CallID, item.ID, item.Arguments != "")
	if sc.source == "openai" {
		switch {
		case first:
			// The announcement embeds complete arguments when the item
			// event already carries them.
			entryArgs := ""
			if deliver {
				entryArgs = item.Arguments
			}
			return [][]byte{sc.chatChunks().Delta(map[string]any{"tool_calls": []any{
				shared.CCToolCallOpeningEntry(idx, item.CallID, item.Name, entryArgs),
			}})}, false, nil
		case deliver:
			// Done for an already-announced call with no streamed arguments:
			// replay the complete arguments so none are lost (FR-006).
			return [][]byte{sc.chatChunks().Delta(map[string]any{"tool_calls": []any{map[string]any{
				"index": idx, "function": map[string]any{"arguments": item.Arguments},
			}}})}, false, nil
		}
		return nil, false, nil
	}
	var out [][]byte
	if first {
		out = append(out, sc.closeOpenBlocks()...)
		sc.openBlocks = append(sc.openBlocks, idx)
		out = append(out, sc.claudeChunks().ContentBlockStart(idx, "tool_use",
			map[string]any{"id": item.CallID, "name": item.Name, "input": map[string]any{}}))
	}
	if deliver {
		// Complete arguments are delivered the moment they are seen,
		// whether they arrive at added or at done (FR-006) — the
		// same decision table as the Chat Completions route.
		out = append(out, sc.argsDelta(idx, item.Arguments))
	}
	if len(out) == 0 {
		return nil, false, nil
	}
	return out, false, nil
}

// argsFragment streams one function_call_arguments.delta: Chat
// Completions feeds its accumulated tool_calls entry (defensively opening
// it when arguments precede the announcement), Messages mirrors that with
// input_json_delta blocks.
func (sc *StreamConverter) argsFragment(itemID, delta string) ([][]byte, bool, *errclass.Error) {
	first, idx := sc.tracker.StreamArgs(itemID)
	if sc.source == "openai" {
		sc.toolCallsSeen = true
		if first {
			// Arguments before the announcement: open the entry so no
			// partial JSON is lost.
			return [][]byte{sc.chatChunks().Delta(map[string]any{"tool_calls": []any{
				shared.CCToolCallOpeningEntry(idx, itemID, "", delta),
			}})}, false, nil
		}
		return [][]byte{sc.chatChunks().Delta(map[string]any{"tool_calls": []any{map[string]any{
			"index": idx, "function": map[string]any{"arguments": delta},
		}}})}, false, nil
	}
	var out [][]byte
	if first {
		// Arguments before the item announcement: open the block with
		// the identifiers available so no partial JSON is lost.
		out = append(out, sc.closeOpenBlocks()...)
		sc.openBlocks = append(sc.openBlocks, idx)
		out = append(out, sc.claudeChunks().ContentBlockStart(idx, "tool_use",
			map[string]any{"id": itemID, "name": "", "input": map[string]any{}}))
	}
	out = append(out, sc.argsDelta(idx, delta))
	return out, false, nil
}

// terminal renders the completed/incomplete tail: Chat Completions gets a
// finish_reason chunk plus [DONE]; Messages gets close-before-stop block
// pairing, message_delta (stop_sequence omitted entirely) and
// message_stop. Shared precedence: tool calls outrank the status-derived
// reason, so response.incomplete cannot downgrade them.
func (sc *StreamConverter) terminal(incomplete bool, in, out int, details shared.UsageDetails) ([][]byte, bool, *errclass.Error) {
	st := "completed"
	if incomplete {
		st = "incomplete"
	}
	if sc.source == "openai" {
		finish := shared.TerminalReason(sc.toolCallsSeen, "tool_calls", shared.CCFinishFromResponseStatus(st))
		// Always attached (F-R6 parity with the Messages route's terminal
		// chunk): typed clients prefer a stable schema, zero-valued fields
		// when upstream reported none. Shared kernel keeps total_tokens
		// consistent with the non-stream Chat Completions mapper.
		chunk := sc.chatChunks().Finish(finish, shared.CCUsageFrom(int64(in), int64(out), details))
		return [][]byte{chunk}, true, nil
	}
	statusStop := shared.ClaudeStopFromResponseStatus(st)
	stop := shared.TerminalReason(sc.toolCallsSeen, "tool_use", statusStop)
	cacheRead, cacheWrite := details.CachedTokens, details.CacheWriteTokens
	em := sc.claudeChunks()
	var events [][]byte
	for _, idx := range sc.openBlocks {
		events = append(events, em.ContentBlockStop(idx))
	}
	sc.openBlocks = nil
	events = append(events,
		em.MessageDelta(&stop, shared.ClaudeUsage(shared.ClampSubtract(int64(in), cacheRead, cacheWrite), int64(out), cacheRead, cacheWrite)),
		em.MessageStop(),
	)
	return events, true, nil
}

// chatChunks binds the shared chunk kernel to the captured upstream
// response identity so this route's frames cannot diverge from the
// sibling Messages-route synthesizer (FR-006); a zero Created defaults
// to render-time now inside the kernel.
func (sc *StreamConverter) chatChunks() shared.ChatChunkBuilder {
	return shared.ChatChunkBuilder{ID: sc.id, Model: sc.model, Created: sc.created}
}

// claudeChunks binds the shared Claude emitter kernel to the captured
// upstream response identity so this route's Messages frames cannot
// diverge from the sibling Chat Completions-route synthesizer (FR-006).
func (sc *StreamConverter) claudeChunks() shared.ClaudeEventEmitter {
	return shared.NewClaudeEventEmitter(sc.id, sc.model)
}

// closeOpenBlocks emits content_block_stop for every currently open
// content block and resets open-block tracking, so the next
// content_block_start obeys Anthropic's close-before-next-start rule;
// the text block is marked closed so a later text delta reopens a fresh
// block (stop/reopen parity with the Chat Completions route).
func (sc *StreamConverter) closeOpenBlocks() [][]byte {
	if len(sc.openBlocks) == 0 {
		return nil
	}
	em := sc.claudeChunks()
	var out [][]byte
	for _, idx := range sc.openBlocks {
		out = append(out, em.ContentBlockStop(idx))
	}
	sc.openBlocks = nil
	sc.textOpen = false
	// The thinking block is closed too: later reasoning must open a fresh
	// block rather than send a delta after its content_block_stop.
	sc.thinkOpen = false
	return out
}

// argsDelta renders one input_json_delta content block delta.
func (sc *StreamConverter) argsDelta(index int, partial string) []byte {
	return sc.claudeChunks().ContentBlockDelta(index,
		map[string]any{"type": "input_json_delta", "partial_json": partial})
}

// captureResponse records stream identity from response.created.
func (sc *StreamConverter) captureResponse(resp *responseMeta) {
	if resp.ID != "" {
		sc.id = resp.ID
	}
	if resp.Model != "" {
		sc.model = resp.Model
	}
	sc.created = int64(resp.CreatedAt)
	sc.started = true
}

// failureError maps response.failed / error events onto FR-009 classes,
// honoring an upstream status_code when present (§7 semantics); without
// one the failure is treated as a retryable upstream server failure.
// Fix #18: uses FromUpstream to preserve vendor error codes and rate-limit
// metadata for accurate cooldown scope.
func failureError(ev *failureEvent, rawJSON []byte) *errclass.Error {
	status := int(ev.StatusCode)
	if status == 0 {
		status = int(ev.Response.StatusCode)
	}
	if status > 0 {
		// Fix #18: hand the raw vendor error JSON to FromUpstream so a
		// structured code / rateLimit is parsed and preserved (cooldown scope
		// and retry timing), instead of classifying on the status alone.
		return errclass.FromUpstream(status, rawJSON)
	}
	// No status anywhere in the event: an opaque upstream failure, treated as
	// a retryable upstream server error (StatusCode 0) exactly as before.
	msg := ev.Message
	if msg == "" {
		msg = ev.Response.Error.Message
	}
	return errclass.UpstreamFallback(msg)
}

// Flush synthesizes terminal events when the upstream closed without
// response.completed (fix #7). OpenAI targets receive a finish chunk,
// Claude targets receive content_block_stop + message_delta + message_stop,
// Responses targets receive response.completed with the captured output.
// Returns nil if nothing was emitted (no terminal to defer) or if already
// flushed.
func (sc *StreamConverter) Flush() [][]byte {
	if sc.flushed {
		return nil
	}
	sc.flushed = true
	// A stream that never saw response.created never opened a client-facing
	// response; there is nothing to close and emitting a terminal would be
	// malformed (e.g. message_delta without message_start).
	if !sc.started {
		return nil
	}
	switch sc.source {
	case "openai", "claude":
		// Reuse the normal-path terminal so a deferred/absent upstream
		// response.completed closes the converted stream with exactly the
		// same frames the complete path would have emitted: a finish chunk
		// (Chat Completions) or close-before-stop blocks + message_delta +
		// message_stop (Messages). Usage is unknown here, so zero-valued
		// counts are reported (the truncation itself is surfaced separately
		// via the stream's error_detail, never replayed here).
		events, _, _ := sc.terminal(false, 0, 0, shared.UsageDetails{})
		return events
	case "openai-response":
		// Responses passthrough forwards every upstream frame verbatim and
		// buffers no content, so a synthesized completion can only carry an
		// empty output snapshot. Emit it anyway so the Responses client sees
		// a terminal and closes instead of hanging; the truncation is
		// reported separately via error_detail.
		return sc.synthesizeResponseCompleted()
	default:
		return nil
	}
}

// synthesizeResponseCompleted builds a minimal response.completed event for
// the passthrough target when the upstream closed without one (Flush path).
// The passthrough buffers no content, so the output snapshot is empty and
// usage is zero; its only job is to give the client a terminal to close on.
func (sc *StreamConverter) synthesizeResponseCompleted() [][]byte {
	ev := map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id":     sc.id,
			"model":  sc.model,
			"output": []any{},
			"status": "completed",
			"usage": map[string]any{
				"input_tokens":  0,
				"output_tokens": 0,
			},
		},
	}
	return [][]byte{shared.SSEEvent("response.completed", ev)}
}
