package chatcompletions

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"

	"commandcode-or-or-cliproxyapi/internal/adapter/shared"
	"commandcode-or-or-cliproxyapi/internal/errclass"
)

// StreamConverter incrementally converts an upstream Chat Completions
// SSE stream into the client protocol's stream (FR-006, AC §B): "openai"
// frames pass through verbatim (with reasoning_content backfilled from
// vendor reasoning spellings), "claude" synthesizes Anthropic Messages
// events, "openai-response" synthesizes Responses events. Partial SSE
// lines are buffered across Feed calls; tool-call argument fragments are
// accumulated per tool_calls index until the stream terminates.
//
// A converter is safe for SEQUENTIAL Feed calls only — it is not
// goroutine-safe; use one converter per upstream stream. Synthesis
// follows choices[0] (upstream serves n=1 routes).
type StreamConverter struct {
	sourceFormat string
	lineBuf      []byte // partial SSE line carried across Feed calls
	done         bool

	started      bool // message_start / first chunk seen
	id           string
	model        string
	claudeEm     shared.ClaudeEventEmitter // canonical Messages frames bound to first-chunk identity
	textOpen     bool                      // claude text content_block open
	textIndex    int                       // claude index of the currently-open text block
	thinkOpen    bool                      // claude thinking content_block open
	thinkIndex   int                       // claude index of the currently-open thinking block
	nextIndex    int                       // next output/block index
	msgIndex     int                       // announced assistant message item index (-1 until text)
	reasoning    []*reasoningSegment       // every announced Responses reasoning item, in arrival order
	curReasoning *reasoningSegment         // open reasoning item receiving deltas (nil once closed)
	tools        map[int64]*streamTool
	liveTool     *streamTool      // claude target: the one tool_use block streamed live (nil until the first tool)
	deferred     []*claudeSegment // claude target: blocks started after liveTool, rendered at the terminal
	toolOrder    []int64          // upstream tool_call indices in first-arrival order
	toolsSeen    bool             // any tool_calls entry observed (terminal-reason precedence)
	usage        *ccUsage
	finished     bool            // finish_reason processed
	heldFinish   string          // finish_reason awaiting terminal emission (flushed on the next data line or [DONE], so the standard include_usage trailer lands in the terminal event; Flush covers close-without-[DONE])
	terminalSent bool            // terminal (message_delta / response.completed) already emitted; claude Flush must still close with message_stop
	flushed      bool            // Flush already ran (one-shot guard)
	respText     strings.Builder // openai-response accumulated output_text
}

// reasoningSegment is one announced Responses reasoning item. Reasoning
// that resumes after the previous item closed (text or a tool call came in
// between) opens a NEW item instead of sending deltas to a done item, so
// no thinking text is lost and no event references a closed item (fix #16).
type reasoningSegment struct {
	itemID string
	outIdx int
	text   strings.Builder
}

// streamTool accumulates one upstream tool_calls index; args collects
// argument fragments so the terminal response.completed output carries the
// complete call (FR-006).
type streamTool struct {
	blockIndex int
	id         string
	itemID     string // fc_-prefixed Responses item id shared by deltas and done events
	name       string
	args       strings.Builder
	stopped    bool // claude target: content_block_stop emitted
}

// claudeSegment is one Messages content block held back while the live
// tool_use block is open (fix #9). Chat Completions addresses tool argument
// fragments by tool index, so a fragment for an earlier tool can arrive
// after a later block started. Anthropic allows one open block at a time
// and no deltas after content_block_stop, so the first tool_use streams
// live and stays open; every block that starts after it is buffered here
// and rendered complete (start, one delta, stop) at the terminal, in
// arrival order. No fragment is dropped and none lands after its stop.
type claudeSegment struct {
	kind string          // "text" | "thinking" | "tool_use"
	text strings.Builder // text or thinking content
	tool *streamTool     // tool_use: arguments accumulate in tool.args
}

// NewStreamConverter returns a converter translating Chat Completions
// SSE into sourceFormat's stream shape.
func NewStreamConverter(sourceFormat string) *StreamConverter {
	return &StreamConverter{
		sourceFormat: sourceFormat,
		msgIndex:     -1,
		tools:        map[int64]*streamTool{},
	}
}

// Feed consumes one upstream chunk (any split of the byte stream),
// returning the client-protocol events completed by this chunk, whether
// the stream is done ([DONE] seen), and a classified error for
// malformed chunks (FR-006). Errors carry at most an 80-character
// redacted snippet — never the full upstream body.
func (sc *StreamConverter) Feed(chunk []byte) (events [][]byte, done bool, eErr *errclass.Error) {
	sc.lineBuf = append(sc.lineBuf, chunk...)
	for {
		i := bytes.IndexByte(sc.lineBuf, '\n')
		if i < 0 {
			break // keep trailing partial line buffered
		}
		line := strings.TrimSuffix(string(sc.lineBuf[:i]), "\r")
		sc.lineBuf = sc.lineBuf[i+1:]
		ev, eErr := sc.handleLine(line)
		if eErr != nil {
			return events, false, eErr
		}
		events = append(events, ev...)
		if sc.done {
			break
		}
	}
	return events, sc.done, nil
}

func (sc *StreamConverter) handleLine(line string) ([][]byte, *errclass.Error) {
	switch sc.sourceFormat {
	case "openai":
		return sc.passthroughLine(line), nil
	case "claude":
		return sc.claudeLine(line)
	case "openai-response":
		return sc.responsesLine(line)
	default:
		return nil, shared.UnsupportedFormat(sc.sourceFormat, EndpointPath)
	}
}

// Flush terminates a stream whose upstream closed without [DONE]: the
// finish_reason chunk may legally be the last data line before close, and
// without this the client never sees message_delta/message_stop or
// response.completed — nor usage stranded behind the deferred terminal
// (FR-006). Claude synthesis appends message_stop after the held
// message_delta; Responses synthesis emits response.completed. One-shot:
// later calls return nothing, as does any call with no pending terminal.
func (sc *StreamConverter) Flush() [][]byte {
	if sc.flushed {
		return nil
	}
	sc.flushed = true
	if sc.done || !sc.started {
		// [DONE] already delivered the terminal, or nothing was emitted.
		return nil
	}
	switch sc.sourceFormat {
	case "claude":
		// Close live blocks and render deferred ones first: a stream cut
		// before finish_reason still owes the client every block (fix #9)
		// and a synthesized message_delta (fix #6).
		events := sc.finalizeBlocks()
		events = append(events, sc.claudeTerminal(true)...)
		// When the post-finish include_usage trailer already delivered the
		// terminal, close-without-[DONE] must still emit message_stop or
		// Anthropic clients hang until timeout.
		return append(events, sc.claudeEm.MessageStop())
	case "openai-response":
		events := sc.stopReasoning()
		return append(events, sc.responsesTerminal(true)...)
	default:
		return nil
	}
}

// sseData extracts a data-line payload; ok is false for non-data lines
// (comments, event:, retry:, empty lines), which converters ignore.
//
// KNOWN LIMITATION (audited, accepted): this line-oriented parser treats
// each data: line as one complete payload, so a legal multi-data-line SSE
// frame whose lines form a single JSON document decodes as separate
// fragments and fails unmarshal with a spurious translation error. Real
// Chat Completions upstreams emit single-data-line frames, and the shared
// SSEFramer migration was declined because its frame-granular batching
// conflicts with passthroughLine's line-immediate [DONE] semantics.
func sseData(line string) (payload string, ok bool) {
	rest, ok := strings.CutPrefix(line, "data:")
	if !ok {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

// passthroughLine extracts the bare data payload for openai-target emissions;
// [DONE] terminates the stream without emitting a chunk (CPA's WriteDone
// automatically writes the trailing data: [DONE]). Reasoning text arriving
// under a vendor spelling is mirrored onto the standard reasoning_content
// member so clients that only read that member still see thinking; every
// other payload forwards byte-identical.
func (sc *StreamConverter) passthroughLine(line string) [][]byte {
	data, ok := sseData(line)
	if !ok {
		return nil
	}
	if shared.IsSSEDone(data) {
		sc.done = true
		return nil
	}
	if data == "" {
		return nil
	}
	if fixed := shared.BackfillReasoningContent([]byte(data), "delta"); fixed != nil {
		return [][]byte{fixed}
	}
	return [][]byte{[]byte(data)}
}

// ---- upstream Chat Completions chunk shape ----

type ccToolCallDelta struct {
	Index    int64  `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type ccDelta struct {
	Content   string            `json:"content"`
	ToolCalls []ccToolCallDelta `json:"tool_calls"`
	// Reasoning carries the vendor thinking spellings (reasoning /
	// reasoning_details[].text / reasoning_content); embedded untagged so
	// the shared kernel decodes all three at once.
	shared.ReasoningFields
}

type ccChunkChoice struct {
	Delta        ccDelta `json:"delta"`
	FinishReason string  `json:"finish_reason"`
}

type ccChunk struct {
	ID      string          `json:"id"`
	Model   string          `json:"model"`
	Choices []ccChunkChoice `json:"choices"`
	Usage   *ccUsage        `json:"usage"`
	Error   *ccChunkError   `json:"error"`
}

// ccChunkError is an in-stream upstream error object; choice-less chunks
// carrying one are failures, not empty deltas.
type ccChunkError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
	// RateLimit is CommandCode's window-exhaustion detail ({window, reset,
	// model}); forwarded to errclass so the cooldown lasts until the reset.
	RateLimit json.RawMessage `json:"rateLimit,omitempty"`
}

// decodeChunk parses one data payload; malformed JSON yields a
// translation failure carrying only a short redacted snippet (FR-006,
// §5 security: no upstream body echo). A non-empty upstream error object
// fails the conversion with a classified error so the fail path closes
// both streams instead of the chunk being silently dropped as a
// zero-choices delta.
//
// Usage is captured here — before any empty-choices early return in the
// format converters: OpenAI's stream_options.include_usage terminal
// chunk is exactly choice-less-with-usage, and dropping it would lose
// the final counts.
func (sc *StreamConverter) decodeChunk(data string) (*ccChunk, *errclass.Error) {
	var chunk ccChunk
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return nil, errclass.Translation("malformed Chat Completions stream chunk: " + shared.RedactedSnippet(data))
	}
	if chunk.Error != nil {
		return nil, chunkError(chunk.Error)
	}
	if chunk.Usage != nil {
		sc.usage = chunk.Usage
	}
	return &chunk, nil
}

// chunkError classifies an in-stream Chat Completions error object onto
// FR-009 classes. Fix #18: pass the raw error JSON to FromUpstream so vendor
// code/rateLimit fields are parsed and preserved (for cooldown scope + retry
// timing). The status is derived from the in-stream code if numeric, else 429
// when the error signals a quota/rate condition, else a generic upstream failure.
func chunkError(e *ccChunkError) *errclass.Error {
	status := 0
	switch c := e.Code.(type) {
	case float64:
		status = int(c)
	case string:
		if n, err := strconv.Atoi(c); err == nil {
			status = n
		}
	}
	// If no numeric code, infer 429 from quota/rate markers in the string
	// code, type, or message (a non-numeric code like "rate_limit_exceeded"
	// is the usual carrier, so it must be part of the scanned text).
	if status == 0 {
		codeStr, _ := e.Code.(string)
		lower := strings.ToLower(codeStr + " " + e.Type + " " + e.Message)
		if strings.Contains(lower, "rate_limit") || strings.Contains(lower, "usage_exceeded") ||
			strings.Contains(lower, "credits") || strings.Contains(lower, "usage-limit") {
			status = 429
		}
	}
	if status == 0 {
		// No status signal at all: an opaque upstream failure, retryable
		// with StatusCode 0 exactly as before.
		return errclass.UpstreamFallback(e.Message)
	}
	// Rebuild a well-formed vendor envelope rather than re-marshalling e:
	// FromUpstream decodes code as a string, so a numeric code would fail the
	// whole decode; rateLimit rides along for credential-scoped cooldown.
	codeStr, _ := e.Code.(string)
	inner := map[string]any{"code": codeStr, "message": e.Message}
	if len(e.RateLimit) > 0 {
		inner["rateLimit"] = e.RateLimit
	}
	raw, _ := json.Marshal(map[string]any{"error": inner})
	return errclass.FromUpstream(status, raw)
}

// ---- Chat Completions -> claude (Anthropic Messages) stream (FR-006) ----

// claudeLine synthesizes Messages events from one chunk: the first
// chunk opens message_start, delta.content opens the text content block
// lazily on first non-empty delta (tool-only streams therefore carry
// tool_use from index 0 like non-stream chatToClaude) and becomes
// text_delta (reopening a new text block if tool_use starts
// closed it), tool_calls argument fragments open tool_use
// blocks and stream input_json_delta, finish_reason closes blocks and
// holds message_delta with the mapped stop reason until the next data
// line or [DONE] (OpenAI's standard include_usage trailer follows the
// finish_reason chunk and must land in the terminal event), and [DONE]
// emits message_stop. Vendor thinking text opens the leading thinking
// block, which every later block start closes — Anthropic's block order is
// thinking, text, tool_use, one block open at a time.
func (sc *StreamConverter) claudeLine(line string) ([][]byte, *errclass.Error) {
	data, ok := sseData(line)
	if !ok || data == "" {
		return nil, nil
	}
	if shared.IsSSEDone(data) {
		sc.done = true
		events := sc.finalizeBlocks()
		events = append(events, sc.claudeTerminal(true)...)
		return append(events, sc.claudeEm.MessageStop()), nil
	}
	chunk, eErr := sc.decodeChunk(data)
	if eErr != nil {
		return nil, eErr
	}
	var events [][]byte
	if sc.heldFinish != "" {
		// A post-finish chunk (typically the include_usage trailer)
		// flushes the deferred terminal so it carries the final counts.
		events = append(events, sc.claudeTerminal(false)...)
	}
	if !sc.started && len(chunk.Choices) > 0 {
		sc.started = true
		input := int64(0)
		if chunk.Usage != nil {
			input = chunk.Usage.PromptTokens
		}
		sc.claudeEm = shared.NewClaudeEventEmitter(chunk.ID, chunk.Model)
		events = append(events, sc.claudeEm.MessageStart(input))
	}
	if len(chunk.Choices) == 0 {
		return events, nil
	}
	choice := chunk.Choices[0]
	if thinking, ok := choice.Delta.ReasoningText(); ok {
		if sc.liveTool != nil {
			// A block that starts after the live tool_use is held back
			// and rendered complete at the terminal (fix #9).
			sc.deferText("thinking", thinking)
		} else {
			// Vendor thinking spellings become the leading thinking block. A
			// reopen (thinking after text) closes every open block first, the
			// same one-open-block rule the text path follows.
			if !sc.thinkOpen {
				events = append(events, sc.stopBlocks()...)
				sc.thinkIndex = sc.nextIndex
				sc.nextIndex++
				sc.thinkOpen = true
				events = append(events, sc.claudeEm.ThinkingBlockStart(sc.thinkIndex))
			}
			events = append(events, sc.claudeEm.ThinkingDelta(sc.thinkIndex, thinking))
		}
	}
	if choice.Delta.Content != "" && sc.liveTool != nil {
		// Text after the live tool_use is held back too (fix #9).
		sc.deferText("text", choice.Delta.Content)
	} else if choice.Delta.Content != "" {
		// The text block opens lazily on first non-empty content so pure
		// tool-call streams carry no phantom empty text block, matching
		// non-stream chatToClaude's block shape. Every content_block_start
		// (this reopen and a first-of-a-new-index tool_use start below)
		// closes ALL currently-open blocks first — open text via stopText
		// and sibling tool_use blocks ascending — mirroring responses'
		// closeOpenBlocks discipline, because Anthropic forbids two
		// concurrently-open blocks and legal Chat Completions interleaving
		// would otherwise hold both open.
		if !sc.textOpen {
			events = append(events, sc.stopBlocks()...)
			sc.textIndex = sc.nextIndex
			sc.nextIndex++
			sc.textOpen = true
			events = append(events, sc.claudeEm.ContentBlockStart(sc.textIndex, "text", map[string]any{"text": ""}))
		}
		events = append(events, sc.claudeEm.ContentBlockDelta(sc.textIndex,
			map[string]any{"type": "text_delta", "text": choice.Delta.Content}))
	}
	for _, tc := range choice.Delta.ToolCalls {
		t := sc.tools[tc.Index]
		if t == nil {
			t = &streamTool{id: tc.ID, name: tc.Function.Name}
			sc.tools[tc.Index] = t
			sc.toolsSeen = true
			sc.toolOrder = append(sc.toolOrder, tc.Index)
			if sc.liveTool == nil {
				// The first tool streams live and stays open until the
				// terminal, so a late fragment for it is never orphaned.
				events = append(events, sc.stopBlocks()...)
				t.blockIndex = sc.nextIndex
				sc.nextIndex++
				sc.liveTool = t
				events = append(events, sc.claudeEm.ContentBlockStart(t.blockIndex, "tool_use",
					map[string]any{"id": t.id, "name": t.name, "input": map[string]any{}}))
			} else {
				sc.deferred = append(sc.deferred, &claudeSegment{kind: "tool_use", tool: t})
			}
		}
		if tc.Function.Arguments == "" {
			continue
		}
		if t == sc.liveTool && !t.stopped {
			events = append(events, sc.claudeEm.ContentBlockDelta(t.blockIndex,
				map[string]any{"type": "input_json_delta", "partial_json": tc.Function.Arguments}))
			continue
		}
		// A deferred tool accumulates so its complete arguments render
		// once at the terminal (fix #9).
		t.args.WriteString(tc.Function.Arguments)
	}
	if choice.FinishReason != "" && !sc.finished {
		sc.finished = true
		sc.heldFinish = choice.FinishReason
		events = append(events, sc.finalizeBlocks()...)
	}
	return events, nil
}

// deferText appends text or thinking to the trailing deferred segment of
// the same kind, or starts a new one, keeping arrival order (fix #9).
func (sc *StreamConverter) deferText(kind, text string) {
	if n := len(sc.deferred); n > 0 && sc.deferred[n-1].kind == kind {
		sc.deferred[n-1].text.WriteString(text)
		return
	}
	seg := &claudeSegment{kind: kind}
	seg.text.WriteString(text)
	sc.deferred = append(sc.deferred, seg)
}

// finalizeBlocks closes every live block and then renders each deferred
// block complete (start, its whole content as one delta, stop) at fresh
// sequential indexes. Idempotent: the deferred queue drains once, so the
// finish_reason, [DONE] and Flush paths can all call it.
func (sc *StreamConverter) finalizeBlocks() [][]byte {
	events := sc.stopBlocks()
	for _, seg := range sc.deferred {
		idx := sc.nextIndex
		sc.nextIndex++
		switch seg.kind {
		case "thinking":
			events = append(events,
				sc.claudeEm.ThinkingBlockStart(idx),
				sc.claudeEm.ThinkingDelta(idx, seg.text.String()))
		case "text":
			events = append(events,
				sc.claudeEm.ContentBlockStart(idx, "text", map[string]any{"text": ""}),
				sc.claudeEm.ContentBlockDelta(idx, map[string]any{"type": "text_delta", "text": seg.text.String()}))
		case "tool_use":
			t := seg.tool
			t.blockIndex = idx
			t.stopped = true
			events = append(events, sc.claudeEm.ContentBlockStart(idx, "tool_use",
				map[string]any{"id": t.id, "name": t.name, "input": map[string]any{}}))
			if args := t.args.String(); args != "" {
				events = append(events, sc.claudeEm.ContentBlockDelta(idx,
					map[string]any{"type": "input_json_delta", "partial_json": args}))
			}
		}
		events = append(events, sc.claudeEm.ContentBlockStop(idx))
	}
	sc.deferred = nil
	return events
}

// claudeTerminal renders the held message_delta exactly once, carrying
// the captured usage when any arrived before emission. force marks the
// end of the stream ([DONE] or Flush): if the upstream never sent a
// finish_reason but the message did start, a message_delta is synthesized
// with the stop-derived reason so the client is not left with an
// unterminated message (fix #6). A post-finish data line passes false.
func (sc *StreamConverter) claudeTerminal(force bool) [][]byte {
	finish := sc.heldFinish
	sc.heldFinish = ""
	if finish == "" {
		if !force || !sc.started || sc.terminalSent {
			return nil
		}
		finish = "stop"
	}
	sc.terminalSent = true
	// Observed tool calls outrank the status-derived reason, mirroring
	// non-stream chatToClaude (FR-006).
	reason := shared.TerminalReason(sc.toolsSeen, "tool_use", shared.FinishToClaudeStop(finish))
	// Always attach: sibling terminals never omit usage; fields zero when
	// upstream reported none (F-R6).
	input, output := int64(0), int64(0)
	var cacheRead *int64
	if sc.usage != nil {
		input = sc.usage.PromptTokens
		output = sc.usage.CompletionTokens
		if sc.usage.PromptDetails != nil {
			cacheRead = sc.usage.PromptDetails.CachedTokens
		}
	}
	return [][]byte{sc.claudeEm.MessageDelta(&reason,
		shared.ClaudeUsage(shared.ClampSubtract(input, cacheRead), output, cacheRead, nil))}
}

// stopThinking closes the open thinking content block, if any.
func (sc *StreamConverter) stopThinking() [][]byte {
	if !sc.thinkOpen {
		return nil
	}
	sc.thinkOpen = false
	return [][]byte{sc.claudeEm.ContentBlockStop(sc.thinkIndex)}
}

// stopText closes the open text content block, if any.
func (sc *StreamConverter) stopText() [][]byte {
	if !sc.textOpen {
		return nil
	}
	sc.textOpen = false
	return [][]byte{sc.claudeEm.ContentBlockStop(sc.textIndex)}
}

// stopBlocks closes the thinking, text, and every open tool_use block
// exactly once in Anthropic block order; called before every
// content_block_start (so blocks never overlap) and again (as a no-op) on
// finish_reason and [DONE].
func (sc *StreamConverter) stopBlocks() [][]byte {
	var events [][]byte
	events = append(events, sc.stopThinking()...)
	events = append(events, sc.stopText()...)
	if t := sc.liveTool; t != nil && !t.stopped {
		t.stopped = true
		events = append(events, sc.claudeEm.ContentBlockStop(t.blockIndex))
	}
	return events
}

// ---- Chat Completions -> openai-response (Responses) stream (FR-006) ----

// responsesLine synthesizes Responses events: delta.content becomes
// response.output_text.delta, tool_calls argument fragments become
// response.function_call_arguments.delta keyed by item_id, and
// finish_reason holds response.completed — emitted at the next data
// line or [DONE] with prompt/completion tokens mapped to input/output
// usage (the standard include_usage trailer follows the finish_reason
// chunk); [DONE] terminates the stream. Vendor thinking text becomes the
// leading reasoning item with its summary events (host-translator parity),
// closed before the first text or function_call item is announced.
func (sc *StreamConverter) responsesLine(line string) ([][]byte, *errclass.Error) {
	data, ok := sseData(line)
	if !ok || data == "" {
		return nil, nil
	}
	if shared.IsSSEDone(data) {
		sc.done = true
		events := sc.stopReasoning()
		return append(events, sc.responsesTerminal(true)...), nil
	}
	chunk, eErr := sc.decodeChunk(data)
	if eErr != nil {
		return nil, eErr
	}
	var events [][]byte
	if sc.heldFinish != "" {
		// A post-finish chunk (typically the include_usage trailer)
		// flushes the deferred terminal so it carries the final counts.
		events = append(events, sc.responsesTerminal(false)...)
	}
	if !sc.started && len(chunk.Choices) > 0 {
		sc.started = true
		sc.id = chunk.ID
		sc.model = chunk.Model
		// Lifecycle parity with the Responses protocol: announce the
		// response, then mark it in progress, before any deltas reference
		// it (F18, fix #8).
		em := sc.responsesEm()
		events = append(events, em.Created(), em.InProgress())
	}
	if len(chunk.Choices) == 0 {
		return events, nil
	}
	choice := chunk.Choices[0]
	if thinking, ok := choice.Delta.ReasoningText(); ok {
		if sc.curReasoning == nil {
			// First reasoning, or reasoning that resumes after text or a
			// tool call closed the previous item: open a fresh item at the
			// next output position rather than streaming into a done item
			// (fix #16). The first keeps the rs_<id>_0 identity.
			seg := &reasoningSegment{
				itemID: shared.ReasoningItemID(sc.id, len(sc.reasoning)),
				outIdx: sc.nextIndex,
			}
			sc.nextIndex++
			sc.reasoning = append(sc.reasoning, seg)
			sc.curReasoning = seg
			em := sc.responsesEm()
			events = append(events, em.ReasoningItemAdded(seg.itemID, seg.outIdx), em.ReasoningPartAdded(seg.itemID, seg.outIdx))
		}
		seg := sc.curReasoning
		seg.text.WriteString(thinking)
		events = append(events, sc.responsesEm().ReasoningSummaryDelta(seg.itemID, seg.outIdx, thinking)...)
	}
	if choice.Delta.Content != "" {
		events = append(events, sc.stopReasoning()...)
		if sc.msgIndex < 0 {
			sc.msgIndex = sc.nextIndex
			sc.nextIndex++
			em := sc.responsesEm()
			events = append(events, em.ItemAdded(sc.msgIndex, map[string]any{
				"type": "message", "role": "assistant", "id": sc.id, "content": []any{},
			}))
			// The output_text part opens before its first delta and closes
			// at the terminal (fix #8 message item lifecycle).
			events = append(events, em.ContentPartAdded(sc.id, sc.msgIndex))
		}
		sc.respText.WriteString(choice.Delta.Content)
		events = append(events, sc.responsesEm().TextDelta(sc.id, sc.msgIndex, choice.Delta.Content))
	}
	for _, tc := range choice.Delta.ToolCalls {
		t := sc.tools[tc.Index]
		if t == nil {
			events = append(events, sc.stopReasoning()...)
			t = &streamTool{blockIndex: sc.nextIndex, id: tc.ID, itemID: shared.FunctionCallItemID(tc.ID), name: tc.Function.Name}
			sc.nextIndex++
			sc.tools[tc.Index] = t
			sc.toolsSeen = true
			sc.toolOrder = append(sc.toolOrder, tc.Index)
			// Announce the item before any arguments delta references it.
			// The fc_ item id is what clients use to bind the later done event.
			events = append(events, sc.responsesEm().ItemAdded(t.blockIndex, map[string]any{
				"type": "function_call", "id": t.itemID, "call_id": t.id,
				"name": t.name, "arguments": "",
			}))
		}
		if tc.Function.Arguments != "" {
			t.args.WriteString(tc.Function.Arguments)
			events = append(events, sc.responsesEm().ArgsDelta(t.itemID, t.blockIndex, tc.Function.Arguments))
		}
	}
	if choice.FinishReason != "" && !sc.finished {
		sc.finished = true
		sc.heldFinish = choice.FinishReason
		events = append(events, sc.stopReasoning()...)
	}
	return events, nil
}

// stopReasoning emits the reasoning item's closing transitions exactly
// once, after the summary text is complete: text done, part done, item
// done. Text, tool calls, finish_reason, and [DONE] all trigger it, so a
// thinking run always closes before the next item is announced
// (host-translator parity).
func (sc *StreamConverter) stopReasoning() [][]byte {
	seg := sc.curReasoning
	if seg == nil {
		return nil
	}
	sc.curReasoning = nil // later reasoning opens a new item (fix #16)
	text := seg.text.String()
	em := sc.responsesEm()
	events := em.ReasoningSummaryDone(seg.itemID, seg.outIdx, text)
	return append(events,
		em.ReasoningPartDone(seg.itemID, seg.outIdx, text),
		em.ReasoningItemDone(seg.itemID, seg.outIdx, text),
	)
}

// responsesEm binds the shared Responses emitter kernel to the captured
// upstream chunk identity so this route's frames cannot diverge from the
// sibling Messages-route synthesizer (FR-006).
func (sc *StreamConverter) responsesEm() shared.ResponsesEventEmitter {
	return shared.ResponsesEventEmitter{ID: sc.id, Model: sc.model}
}

// responsesTerminal renders the held response.completed exactly once,
// carrying the captured usage when any arrived before emission. force
// marks the end of the stream ([DONE] or Flush): a started response that
// never received a finish_reason still gets its closing events and a
// completed terminal instead of hanging the client (fix #6). A post-finish
// data line passes false.
func (sc *StreamConverter) responsesTerminal(force bool) [][]byte {
	finish := sc.heldFinish
	sc.heldFinish = ""
	if finish == "" {
		if !force || !sc.started || sc.terminalSent {
			return nil
		}
		finish = "stop"
	}
	sc.terminalSent = true
	// Status derives positionally from finish alone: length→incomplete,
	// else completed. Tool calls are represented by output items, not
	// status vocabulary (F-R2, Messages-route parity).
	status := shared.ResponseStatusFromCCFinish(finish)
	// Shared assembler (FR-006 sibling parity): the terminal payload
	// carries the output items fed in ARRIVAL order so Render() reproduces
	// the streamed output_item.added indexes exactly — the message slot is
	// pinned at sc.msgIndex (assigned at the first text delta, after any
	// tools-first announcements), never displacing already-announced
	// function_call items; this mirrors the Messages-route invariant that
	// terminal output order equals announcement order (W4 pin).
	oa := shared.NewOutputAssembler(sc.id)
	// Every item renders at the output_index it was announced with:
	// reasoning items (possibly several, fix #16), the one message slot,
	// and function calls are placed by walking the announced positions in
	// order, so Render() reproduces the streamed indexes exactly.
	byPos := make(map[int]func(), sc.nextIndex)
	for _, seg := range sc.reasoning {
		seg := seg
		byPos[seg.outIdx] = func() { oa.AppendReasoning(shared.NewRespReasoningItem(seg.itemID, seg.text.String())) }
	}
	if sc.msgIndex >= 0 {
		byPos[sc.msgIndex] = oa.ReserveTextSlot
	}
	for _, idx := range sc.toolOrder {
		t := sc.tools[idx]
		byPos[t.blockIndex] = func() { oa.AppendFunctionCall(t.id, t.name, shared.DefaultArgs(t.args.String())) }
	}
	for pos := 0; pos < sc.nextIndex; pos++ {
		if place := byPos[pos]; place != nil {
			place()
		}
	}
	oa.AddText(sc.respText.String())
	// Always attach (F-R6): zero-valued fields when upstream sent none.
	input, outputTokens := int64(0), int64(0)
	if sc.usage != nil {
		input = sc.usage.PromptTokens
		outputTokens = sc.usage.CompletionTokens
	}
	var details shared.UsageDetails
	if sc.usage != nil {
		if sc.usage.PromptDetails != nil {
			details.CachedTokens = sc.usage.PromptDetails.CachedTokens
		}
		if sc.usage.CompletionDetails != nil {
			details.ReasoningTokens = sc.usage.CompletionDetails.ReasoningTokens
		}
	}
	usage := shared.NewResponsesUsageFrom(input, outputTokens, details)
	em := sc.responsesEm()
	events := make([][]byte, 0, len(sc.toolOrder)*2+4)
	// Close the message item before the function calls, the same order the
	// Messages-route synthesizer uses (fix #8 lifecycle, FR-006 parity).
	if sc.msgIndex >= 0 {
		text := sc.respText.String()
		events = append(events,
			em.OutputTextDone(sc.id, sc.msgIndex, text),
			em.ContentPartDone(sc.id, sc.msgIndex, text),
			em.MessageItemDone(sc.msgIndex, sc.id, text),
		)
	}
	for _, idx := range sc.toolOrder {
		t := sc.tools[idx]
		if t == nil {
			continue
		}
		args := shared.DefaultArgs(t.args.String())
		events = append(events, em.CloseFunctionCall(t.blockIndex, shared.RespItem{
			Type: "function_call", ID: t.itemID, CallID: t.id, Name: t.name, Arguments: args,
		})...)
	}
	return append(events, em.Completed(status, usage, oa.Render()))
}
