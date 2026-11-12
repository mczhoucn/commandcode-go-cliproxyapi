package messages

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"commandcode-or-or-cliproxyapi/internal/adapter/shared"
	"commandcode-or-or-cliproxyapi/internal/errclass"
)

// StreamConverter converts an upstream Anthropic Messages SSE stream
// (typed `event:`/`data:` pairs) into the client protocol incrementally
// (FR-006, AC §C streaming). It is stateful and sequential-use only:
// one instance per upstream response, Feed called with consecutive
// network chunks in order.
type StreamConverter struct {
	framer           *shared.SSEFramer
	sourceFormat     string
	created          int64
	blocks           map[int]*blockState
	toolOrdinal      int
	msgID            string
	model            string
	promptTokens     int64
	completionTokens int64
	cacheRead        *int64
	cacheCreation    *int64
	stopReason       string // last observed message_delta stop_reason (responses terminal status)
	toolsSeen        bool   // any tool_use block observed (openai finish-reason precedence)
	roleSent         bool
	msgIdx           int  // responses: compacted output_index of the ONE announced message item (-1 until then)
	outCount         int  // responses: next compacted output position; thinking blocks consume none (FR-005 omission)
	emitted          bool // any client event emitted (Flush eligibility)
	flushed          bool // one-shot guard for Flush
	// Reasoning (thinking) tracking for the Responses target. Anthropic
	// sends thinking as a content_block_start(thinking) followed by
	// thinking_delta events; we synthesize the Responses reasoning item
	// lifecycle (item added, summary deltas, done) so cross-format clients
	// receive the thinking text the chat route already emits (Fix #2,
	// CLAUDE.md parity invariant).
	thinkSeen    bool
	thinkStopped bool
	thinkItemID  string
	thinkOutIdx  int
	thinkBuf     strings.Builder
	// respTextBuf aggregates the Responses message item's output_text so
	// the closing output_text.done / content_part.done / output_item.done
	// events carry the full text (Fix #8: message item lifecycle events).
	respTextBuf strings.Builder
}

// blockState tracks one open upstream content block by index.
type blockState struct {
	kind    string
	id      string
	itemID  string // fc_-prefixed Responses item id shared by deltas and done events
	name    string
	ordinal int             // compact tool_calls index for openai chunks
	outIdx  int             // responses: compacted output_index assigned at block start
	emitted bool            // openai: id/name attached to the first arguments fragment
	text    strings.Builder // accumulated text_delta content (responses target)
	args    strings.Builder // accumulated input_json_delta content (responses target)
}

// NewStreamConverter prepares stream conversion for sourceFormat
// ("openai" Chat Completions chunks, "openai-response" Responses events,
// "claude" verbatim passthrough).
func NewStreamConverter(sourceFormat string) *StreamConverter {
	return &StreamConverter{
		framer:       shared.NewSSEFramer(sourceFormat == "claude"),
		sourceFormat: sourceFormat,
		created:      time.Now().Unix(),
		blocks:       map[int]*blockState{},
		msgIdx:       -1,
	}
}

// Feed consumes one network chunk and returns fully framed client events,
// whether the upstream stream is finished (message_stop seen), and a
// classified error. Partial lines are buffered until complete.
func (sc *StreamConverter) Feed(chunk []byte) (events [][]byte, done bool, eErr *errclass.Error) {
	sc.framer.Push(chunk)
	for {
		etype, data, rawBlock, ok := sc.framer.Next()
		if !ok {
			return events, false, nil
		}
		d, eErr := sc.dispatch(etype, data, rawBlock, &events)
		if len(events) > 0 {
			sc.emitted = true
		}
		if eErr != nil {
			return events, false, eErr
		}
		if d {
			return events, true, nil
		}
	}
}

// dispatch routes one complete SSE frame. Conversion targets parse the
// data payload eagerly into typed per-event structs (with the
// type-from-payload fallback when the event: line was absent); claude
// passthrough forwards the verbatim raw block and parses only what error
// handling and done-detection require.
func (sc *StreamConverter) dispatch(etype, data string, rawBlock []byte, events *[][]byte) (bool, *errclass.Error) {
	if sc.sourceFormat == "claude" {
		return sc.dispatchClaude(etype, data, rawBlock, events)
	}
	var ev sseEvent
	if data != "" {
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return false, errclass.Translation(fmt.Sprintf("malformed SSE data JSON: %s", shared.RedactedSnippet(data)))
		}
		if etype == "" {
			etype = ev.Type // tolerate missing event: line
		}
	}
	switch sc.sourceFormat {
	case "openai":
		return sc.dispatchOpenAI(etype, &ev, events)
	case "openai-response":
		return sc.dispatchResponses(etype, &ev, events)
	default:
		return false, shared.UnsupportedFormat(sc.sourceFormat, EndpointPath)
	}
}

// sseEvent is the typed union of Anthropic Messages SSE payload shapes on
// conversion targets. One struct serves every event type because the
// type may arrive only inside the payload (missing event: line), so the
// decode must precede routing; absent members stay zero-valued at no
// cost, and unknown keys are ignored. Error stays a raw map because the
// classifier reads an open-ended upstream object.
type sseEvent struct {
	Type         string         `json:"type"`
	Index        float64        `json:"index"`
	Message      startMessage   `json:"message"`
	ContentBlock contentBlock   `json:"content_block"`
	Delta        deltaBody      `json:"delta"`
	Usage        usageCounts    `json:"usage"`
	Error        map[string]any `json:"error"`
}

type startMessage struct {
	ID    string      `json:"id"`
	Model string      `json:"model"`
	Usage usageCounts `json:"usage"`
}

type contentBlock struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

type deltaBody struct {
	Type string `json:"type"`
	Text string `json:"text"`
	// Thinking carries thinking_delta text: Anthropic puts it under
	// "thinking", never "text", so reading only Text empties every
	// reasoning fragment.
	Thinking    string `json:"thinking"`
	PartialJSON string `json:"partial_json"`
	StopReason  string `json:"stop_reason"`
}

type usageCounts struct {
	InputTokens   float64  `json:"input_tokens"`
	OutputTokens  float64  `json:"output_tokens"`
	CacheRead     *float64 `json:"cache_read_input_tokens"`
	CacheCreation *float64 `json:"cache_creation_input_tokens"`
}

// dispatchClaude passes event blocks through verbatim; an in-stream error
// event becomes a classified error instead of reaching the client raw.
// The payload is parsed lazily: only when the typed fallback must resolve
// a missing event: line or when the frame is an error event.
func (sc *StreamConverter) dispatchClaude(etype, data string, raw []byte, events *[][]byte) (bool, *errclass.Error) {
	var payload map[string]any
	if etype == "" && data != "" {
		json.Unmarshal([]byte(data), &payload) // best-effort type probe
		etype, _ = payload["type"].(string)
	}
	if etype == "error" {
		if payload == nil && data != "" {
			json.Unmarshal([]byte(data), &payload)
		}
		return false, sseError(mget(payload, "error"))
	}
	if len(raw) > 0 {
		*events = append(*events, raw)
	}
	return etype == "message_stop", nil
}

// dispatchOpenAI synthesizes chat.completion.chunk frames (FR-006).
func (sc *StreamConverter) dispatchOpenAI(etype string, ev *sseEvent, events *[][]byte) (bool, *errclass.Error) {
	switch etype {
	case "message_start":
		sc.msgID = ev.Message.ID
		sc.model = ev.Message.Model
		sc.promptTokens = int64(ev.Message.Usage.InputTokens)
		sc.captureCache(ev.Message.Usage)
		sc.emitRole(events)
	case "content_block_start":
		bs := &blockState{kind: ev.ContentBlock.Type, id: ev.ContentBlock.ID, name: ev.ContentBlock.Name}
		if bs.kind == "tool_use" {
			bs.ordinal = sc.toolOrdinal
			sc.toolOrdinal++
			sc.toolsSeen = true
			// Announce the tool_call immediately on block start so a no-arg
			// tool (start then stop, no deltas) still produces a tool_calls
			// entry with finish_reason=tool_calls (Fix #10).
			bs.emitted = true
			sc.emitRole(events)
			sc.emitChunk(events, map[string]any{"tool_calls": []any{
				shared.CCToolCallOpeningEntry(bs.ordinal, bs.id, bs.name, ""),
			}}, "", false)
		}
		sc.blocks[int(ev.Index)] = bs
	case "content_block_delta":
		bs := sc.blocks[int(ev.Index)]
		switch ev.Delta.Type {
		case "text_delta":
			sc.emitRole(events)
			sc.emitChunk(events, map[string]any{"content": ev.Delta.Text}, "", false)
		case "input_json_delta":
			if bs == nil || bs.kind != "tool_use" {
				return false, errclass.Translation("tool arguments fragment without a tool_use block start")
			}
			var tc map[string]any
			if !bs.emitted {
				// Defensive: bs.emitted is set at content_block_start, so
				// this branch is only reached if content_block_start was
				// missed; fall back to opening entry with this fragment.
				tc = shared.CCToolCallOpeningEntry(bs.ordinal, bs.id, bs.name, ev.Delta.PartialJSON)
				bs.emitted = true
			} else {
				tc = map[string]any{
					"index":    bs.ordinal,
					"function": map[string]any{"arguments": ev.Delta.PartialJSON},
				}
			}
			sc.emitRole(events)
			sc.emitChunk(events, map[string]any{"tool_calls": []any{tc}}, "", false)
		case "thinking_delta":
			// Replay thinking as reasoning_content so OpenAI clients that read
			// the standard spelling receive the thinking text (Fix #2, CLAUDE.md
			// parity invariant: "Historical thinking must be replayed"). A
			// thinking_delta addressed to a non-thinking block is a protocol
			// defect and is dropped, matching the Responses dispatcher.
			if bs == nil || bs.kind != "thinking" {
				break
			}
			sc.emitRole(events)
			sc.emitChunk(events, map[string]any{"reasoning_content": ev.Delta.Thinking}, "", false)
		}
	case "message_delta":
		if ev.Usage.InputTokens != 0 {
			sc.promptTokens = int64(ev.Usage.InputTokens)
		}
		sc.completionTokens = int64(ev.Usage.OutputTokens)
		sc.captureCache(ev.Usage)
		// Observed tool calls outrank the status-derived reason: a
		// terminal max_tokens cannot downgrade tool_calls to length.
		sc.emitChunk(events, map[string]any{},
			shared.TerminalReason(sc.toolsSeen, "tool_calls",
				shared.ClaudeStopToFinish(ev.Delta.StopReason)), true)
	case "message_stop":
		return true, nil
	case "error":
		return false, sseError(ev.Error)
	default: // ping and unknown event types are ignored (forward compat)
	}
	return false, nil
}

// dispatchResponses synthesizes Responses SSE events (FR-006).
func (sc *StreamConverter) dispatchResponses(etype string, ev *sseEvent, events *[][]byte) (bool, *errclass.Error) {
	switch etype {
	case "message_start":
		sc.msgID = ev.Message.ID
		sc.model = ev.Message.Model
		sc.promptTokens = int64(ev.Message.Usage.InputTokens)
		sc.captureCache(ev.Message.Usage)
		// Lifecycle parity with the Chat Completions route's synthesizer:
		// announce the response before any deltas reference it (F18).
		// in_progress follows created before any item event (fix #8).
		*events = append(*events, sc.responsesEm().Created(), sc.responsesEm().InProgress())
	case "content_block_start":
		bs := &blockState{kind: ev.ContentBlock.Type, id: ev.ContentBlock.ID, name: ev.ContentBlock.Name}
		sc.blocks[int(ev.Index)] = bs
		// Announce each translatable item before its first delta (F18
		// lifecycle parity). Indexes come from the SAME compacted space
		// outputItems() renders, so streamed output_index values always match
		// terminal response.completed.output positions.
		switch bs.kind {
		case "text":
			if sc.msgIdx >= 0 {
				break // one message item aggregates every text block (canonical shape)
			}
			sc.msgIdx = sc.outCount
			sc.outCount++
			// Native-compatible shape: the announced item carries "id" so
			// downstream output_text.delta item_ids resolve (F-R3 parity).
			*events = append(*events, sc.responsesEm().ItemAdded(sc.msgIdx, map[string]any{
				"type": "message", "id": sc.msgID, "role": "assistant", "content": []any{},
			}))
			// The text part is opened before the first delta and closed at
			// completion (fix #8: message item lifecycle).
			*events = append(*events, sc.responsesEm().ContentPartAdded(sc.msgID, sc.msgIdx))
		case "tool_use":
			sc.toolsSeen = true
			bs.outIdx = sc.outCount
			sc.outCount++
			bs.itemID = shared.FunctionCallItemID(bs.id)
			*events = append(*events, sc.responsesEm().ItemAdded(bs.outIdx, map[string]any{
				"type": "function_call", "id": bs.itemID, "call_id": bs.id, "name": bs.name, "arguments": "",
			}))
		case "thinking":
			// Fix #2: emit reasoning item lifecycle (added, summary_text deltas,
			// summary_text.done, done) so Responses clients receive the thinking
			// text the chat route already emits (CLAUDE.md parity invariant:
			// "Historical thinking must be replayed").
			// Thinking blocks carry no id, so the item identity follows the
			// host convention rs_<response>_<choice>. Only the first thinking
			// block announces: later (interleaved) thinking blocks extend the
			// same reasoning item, so outCount stays aligned with the single
			// item outputItems renders at this block's position.
			if sc.thinkSeen {
				break
			}
			sc.thinkSeen = true
			sc.thinkItemID = shared.ReasoningItemID(sc.msgID, 0)
			sc.thinkOutIdx = sc.outCount
			sc.outCount++
			em := sc.responsesEm()
			*events = append(*events, em.ReasoningItemAdded(sc.thinkItemID, sc.thinkOutIdx))
			*events = append(*events, em.ReasoningPartAdded(sc.thinkItemID, sc.thinkOutIdx))
		}
	case "content_block_delta":
		idx := int(ev.Index)
		bs := sc.blocks[idx]
		switch ev.Delta.Type {
		case "text_delta":
			if bs != nil {
				bs.text.WriteString(ev.Delta.Text)
			}
			sc.respTextBuf.WriteString(ev.Delta.Text)
			// Deltas target the single aggregated message item, so later
			// text blocks reference its announced position, never their own.
			dIdx := idx
			if sc.msgIdx >= 0 {
				dIdx = sc.msgIdx
			}
			*events = append(*events, sc.responsesEm().TextDelta(sc.msgID, dIdx, ev.Delta.Text))
		case "input_json_delta":
			if bs == nil || bs.kind != "tool_use" {
				return false, errclass.Translation("tool arguments fragment without a tool_use block start")
			}
			bs.args.WriteString(ev.Delta.PartialJSON)
			*events = append(*events, sc.responsesEm().ArgsDelta(bs.itemID, bs.outIdx, ev.Delta.PartialJSON))
		case "thinking_delta":
			// Fix #2: emit reasoning_text events so Responses clients receive the
			// thinking text the chat route already emits (CLAUDE.md parity invariant:
			// "Historical thinking must be replayed"). DeepSeek docs state both
			// response.reasoning_text.* and response.reasoning_summary_text.* are
			// distinct event types, so emit both.
			// A thinking fragment only belongs to a thinking block. One that
			// targets any other block (or an unannounced index) is dropped:
			// opening a reasoning item mid-stream would claim an output_index
			// after text/tool items and desynchronize the terminal output.
			if bs == nil || bs.kind != "thinking" || !sc.thinkSeen {
				break
			}
			sc.thinkBuf.WriteString(ev.Delta.Thinking)
			*events = append(*events, sc.responsesEm().ReasoningSummaryDelta(sc.thinkItemID, sc.thinkOutIdx, ev.Delta.Thinking)...)
		}
	case "message_delta":
		if ev.Usage.InputTokens != 0 {
			sc.promptTokens = int64(ev.Usage.InputTokens)
		}
		sc.completionTokens = int64(ev.Usage.OutputTokens) // accumulated, reported on completion
		sc.captureCache(ev.Usage)
		sc.stopReason = ev.Delta.StopReason
	case "message_stop":
		*events = append(*events, sc.responsesCompleted()...)
		return true, nil
	case "error":
		return false, sseError(ev.Error)
	default: // ping and unknown event types are ignored (forward compat)
	}
	return false, nil
}

// responsesCompleted builds the terminal response.completed event: shape
// parity with the non-stream claudeToResponses converter — status comes
// from the Responses vocabulary {completed, incomplete} via the observed
// stop_reason (max_tokens truncation → incomplete); tool calls are
// represented by their function_call output items and never override the
// status (FR-006). Each function_call is closed at its announced
// output_index before completed, because clients finish a call only after
// the matching done event. Reasoning items are closed similarly (Fix #2:
// emit reasoning_summary_text.done and response.output_item.done before
// response.completed). Shared by message_stop and Flush so an early
// upstream close cannot diverge from the normal-path shape.
func (sc *StreamConverter) responsesCompleted() [][]byte {
	status := shared.ResponseStatusFromClaudeStop(sc.stopReason)
	usage := shared.NewResponsesUsageFrom(sc.promptTokens+valueOrZero(sc.cacheRead)+valueOrZero(sc.cacheCreation), sc.completionTokens,
		shared.UsageDetails{CachedTokens: sc.cacheRead, CacheWriteTokens: sc.cacheCreation})
	em := sc.responsesEm()
	indexes := make([]int, 0, len(sc.blocks))
	for i := range sc.blocks {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)
	events := make([][]byte, 0, len(indexes)*2+1)
	// Close reasoning item if seen (Fix #2: emit reasoning_summary_text.done
	// and output_item.done before response.completed).
	if sc.thinkSeen && !sc.thinkStopped {
		sc.thinkStopped = true
		text := sc.thinkBuf.String()
		events = append(events, em.ReasoningSummaryDone(sc.thinkItemID, sc.thinkOutIdx, text)...)
		events = append(events, em.ReasoningPartDone(sc.thinkItemID, sc.thinkOutIdx, text))
		events = append(events, em.ReasoningItemDone(sc.thinkItemID, sc.thinkOutIdx, text))
	}
	// Close the message item (fix #8): output_text.done, content_part.done,
	// then output_item.done, each carrying the full aggregated text.
	if sc.msgIdx >= 0 {
		text := sc.respTextBuf.String()
		events = append(events,
			em.OutputTextDone(sc.msgID, sc.msgIdx, text),
			em.ContentPartDone(sc.msgID, sc.msgIdx, text),
			em.MessageItemDone(sc.msgIdx, sc.msgID, text),
		)
		sc.msgIdx = -1 // one-shot: a later Flush must not close it twice
	}
	for _, i := range indexes {
		bs := sc.blocks[i]
		if bs == nil || bs.kind != "tool_use" {
			continue
		}
		args := shared.DefaultArgs(bs.args.String())
		events = append(events, em.CloseFunctionCall(bs.outIdx, shared.RespItem{
			Type: "function_call", ID: bs.itemID, CallID: bs.id, Name: bs.name, Arguments: args,
		})...)
	}
	return append(events, em.Completed(status, usage, sc.outputItems()))
}

// responsesEm binds the shared Responses emitter kernel to the captured
// upstream message identity so this route's frames cannot diverge from the
// sibling Chat-Completions-route synthesizer (FR-006).
func (sc *StreamConverter) responsesEm() shared.ResponsesEventEmitter {
	return shared.ResponsesEventEmitter{ID: sc.msgID, Model: sc.model}
}

// Flush terminates a stream whose upstream closed before message_stop:
// without this the client never sees response.completed nor [DONE] (F5,
// FR-006). Responses synthesis emits response.completed from captured
// identity, usage, and accumulated output items; openai targets get the
// missing [DONE]; claude passthrough holds nothing back. One-shot: later
// calls return nothing, as does any call with nothing emitted.
func (sc *StreamConverter) Flush() [][]byte {
	if sc.flushed {
		return nil
	}
	sc.flushed = true
	if !sc.emitted {
		return nil
	}
	switch sc.sourceFormat {
	case "openai-response":
		return sc.responsesCompleted()
	case "openai":
		return nil
	default: // claude passthrough forwards verbatim; nothing deferred
		return nil
	}
}

// outputItems materializes the Responses output array from observed
// content blocks in upstream index order (FR-006) through the shared
// OutputAssembler: ALL text blocks aggregate into ONE message item
// positioned at the first text block's slot while tool_use blocks become
// separate function_call items — the canonical native shape, equal to the
// non-stream claudeToResponses converter (FR-006 mode parity). Thinking
// blocks become reasoning items (Fix #2: emit reasoning so Responses clients
// receive thinking text the chat route already emits).
func (sc *StreamConverter) outputItems() []any {
	indexes := make([]int, 0, len(sc.blocks))
	for i := range sc.blocks {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)
	oa := shared.NewOutputAssembler(sc.msgID)
	// Fix #2: the single reasoning item renders at the FIRST thinking block's
	// position, the same compacted slot its announcement claimed, so the
	// streamed output_index values keep matching the terminal array.
	reasoningPlaced := false
	for _, i := range indexes {
		bs := sc.blocks[i]
		switch bs.kind {
		case "thinking":
			if sc.thinkSeen && !reasoningPlaced {
				reasoningPlaced = true
				oa.AppendReasoning(shared.NewRespReasoningItem(sc.thinkItemID, sc.thinkBuf.String()))
			}
		case "text":
			oa.ReserveTextSlot()
			oa.AddText(bs.text.String())
		case "tool_use":
			oa.AppendFunctionCall(bs.id, bs.name, shared.DefaultArgs(bs.args.String()))
		}
	}
	return oa.Render()
}

// chatChunks binds the shared chunk kernel to the captured upstream
// message identity so this route's frames cannot diverge from the
// sibling Responses-route synthesizer (FR-006).
func (sc *StreamConverter) chatChunks() shared.ChatChunkBuilder {
	return shared.ChatChunkBuilder{ID: sc.msgID, Model: sc.model, Created: sc.created}
}

// emitRole guarantees the leading assistant-role chunk openai clients expect.
func (sc *StreamConverter) emitRole(events *[][]byte) {
	if sc.roleSent {
		return
	}
	sc.roleSent = true
	*events = append(*events, sc.chatChunks().RoleChunk())
}

// emitChunk appends one synthesized chat.completion.chunk frame via the
// shared kernel; an empty finish renders null (intermediate chunks) and
// withUsage attaches the accumulated token usage to the terminal frame.
func (sc *StreamConverter) emitChunk(events *[][]byte, delta map[string]any, finish string, withUsage bool) {
	b := sc.chatChunks()
	if finish == "" {
		*events = append(*events, b.Delta(delta))
		return
	}
	var usage map[string]any
	if withUsage {
		usage = shared.CCUsageFrom(sc.promptTokens+valueOrZero(sc.cacheRead)+valueOrZero(sc.cacheCreation), sc.completionTokens,
			shared.UsageDetails{CachedTokens: sc.cacheRead})
	}
	*events = append(*events, b.Finish(finish, usage))
}

func (sc *StreamConverter) captureCache(u usageCounts) {
	if u.CacheRead != nil {
		v := int64(*u.CacheRead)
		sc.cacheRead = &v
	}
	if u.CacheCreation != nil {
		v := int64(*u.CacheCreation)
		sc.cacheCreation = &v
	}
}

// sseError maps an Anthropic in-stream error event onto the shared
// classification. Fix #18: pass the raw error JSON to FromUpstream so vendor
// code/rateLimit fields are parsed and preserved (for cooldown scope + retry
// timing). The status is derived from the documented Anthropic error type.
func sseError(e map[string]any) *errclass.Error {
	errorType := sget(e, "type")
	message := sget(e, "message")
	if strings.Contains(strings.ToLower(errorType), "gousagelimiterror") || strings.Contains(strings.ToLower(errorType), "freeusagelimiterror") {
		message = errorType + ": " + message
	}
	status := anthropicErrorStatus(errorType)
	// Fix #18: the vendor's code / rateLimit must reach FromUpstream so a
	// window-exhausted error cools the whole credential until upstream reset
	// instead of degrading to a status-only classification. The envelope is
	// rebuilt rather than re-marshalled verbatim: FromUpstream decodes code as
	// a string (a numeric code would fail the whole decode and leak raw JSON
	// into the message), and the usage-limit prefix above must survive.
	code := sget(e, "code")
	if code == "" && message == "" {
		return errclass.FromStatus(status, message)
	}
	inner := map[string]any{"code": code, "message": message}
	if rl, ok := e["rateLimit"]; ok {
		inner["rateLimit"] = rl
	}
	raw, _ := json.Marshal(map[string]any{"error": inner})
	return errclass.FromUpstream(status, raw)
}

func anthropicErrorStatus(t string) int {
	switch t {
	case "invalid_request_error":
		return 400
	case "authentication_error":
		return 401
	case "permission_error":
		return 403
	case "not_found_error":
		return 404
	case "request_too_large":
		return 413
	case "rate_limit_error":
		return 429
	case "overloaded_error":
		return 503
	default:
		return 500
	}
}

func mget(m map[string]any, k string) map[string]any {
	v, _ := m[k].(map[string]any)
	return v
}

func sget(m map[string]any, k string) string {
	v, _ := m[k].(string)
	return v
}
