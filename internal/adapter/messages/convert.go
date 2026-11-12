// Non-stream response conversion for the Messages adapter (FR-006,
// AC §C): an upstream Anthropic Messages HTTP response is translated back
// into the client protocol — validated passthrough for claude clients,
// Chat Completions / Responses synthesis otherwise. Thinking text becomes
// reasoning_content (Chat) or one reasoning item (Responses), matching the
// stream path; redacted_thinking has no plaintext and is omitted.
package messages

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"commandcode-or-or-cliproxyapi/internal/adapter/shared"
	"commandcode-or-or-cliproxyapi/internal/errclass"
)

// ---- upstream Anthropic Messages response shapes (decode-only) ----

type claudeBlockIn struct {
	Type string `json:"type"` // text | thinking | tool_use | ...
	Text string `json:"text"`
	// Thinking carries a thinking block's text: Anthropic puts it under
	// "thinking", never "text".
	Thinking string          `json:"thinking"`
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Input    json.RawMessage `json:"input"`
}

type claudeUsageIn struct {
	InputTokens   int64  `json:"input_tokens"`
	OutputTokens  int64  `json:"output_tokens"`
	CacheRead     *int64 `json:"cache_read_input_tokens"`
	CacheCreation *int64 `json:"cache_creation_input_tokens"`
}

type claudeResponseIn struct {
	ID         string          `json:"id"`
	Model      string          `json:"model"`
	Content    []claudeBlockIn `json:"content"`
	StopReason string          `json:"stop_reason"`
	Usage      claudeUsageIn   `json:"usage"`
}

// ConvertNonStreamResponse translates an upstream Anthropic Messages HTTP
// response back into the client protocol sourceFormat (FR-006, AC §C).
// Statuses >= 400 become classified redacted errors (§7); "claude"
// passes through; "openai" and "openai-response" are converted, preserving
// text, thinking, tool_use, stop reason, and usage (redacted_thinking is
// omitted). Unknown formats are ClassUnsupported;
// malformed upstream bodies are ClassTranslation.
func ConvertNonStreamResponse(sourceFormat string, status int, upstreamBody []byte) ([]byte, *errclass.Error) {
	if status >= 400 {
		return nil, shared.UpstreamStatusError(status, upstreamBody)
	}
	switch sourceFormat {
	case "claude":
		if !json.Valid(upstreamBody) {
			return nil, errclass.Translation("malformed Messages response JSON")
		}
		return upstreamBody, nil
	case "openai":
		return claudeToChat(upstreamBody)
	case "openai-response":
		return claudeToResponses(upstreamBody)
	default:
		return nil, shared.UnsupportedFormat(sourceFormat, EndpointPath)
	}
}

// decodeClaude validates and decodes an upstream non-stream Messages body;
// a missing content array is a translation failure, never silently empty
// (FR-006).
func decodeClaude(body []byte) (*claudeResponseIn, *errclass.Error) {
	var resp claudeResponseIn
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, errclass.Translation("malformed Messages response JSON: " + err.Error())
	}
	if resp.Content == nil {
		return nil, errclass.Translation("Messages response carries no content blocks")
	}
	return &resp, nil
}

// claudeToChat converts a Messages response into a Chat Completions
// response: text blocks concatenate into message.content, tool_use blocks
// become indexed tool_calls with JSON-encoded arguments, stop_reason maps
// to finish_reason, and input/output tokens map to prompt/completion
// usage (FR-006, AC §C). Thinking blocks concatenate into
// message.reasoning_content; redacted_thinking blocks are omitted.
func claudeToChat(body []byte) ([]byte, *errclass.Error) {
	resp, eErr := decodeClaude(body)
	if eErr != nil {
		return nil, eErr
	}
	var text, reasoning strings.Builder
	msg := shared.CCMessage{Role: "assistant"}
	for _, blk := range resp.Content {
		switch blk.Type {
		case "text":
			text.WriteString(blk.Text)
		case "tool_use":
			args, _ := json.Marshal(decodeBlockInput(blk.Input))
			tc := shared.CCToolCall{ID: blk.ID, Type: "function"}
			tc.Function.Name = blk.Name
			tc.Function.Arguments = string(args)
			msg.ToolCalls = append(msg.ToolCalls, tc)
		case "thinking":
			// Fix #2: thinking text ships as reasoning_content, the same
			// field the stream path emits, so the client can replay it.
			reasoning.WriteString(blk.Thinking)
		case "redacted_thinking":
			// Pure encrypted reasoning metadata with no plaintext to
			// replay; omitted like the stream path instead of failing.
		default:
			return nil, errclass.Translation(fmt.Sprintf(
				"%s output cannot represent %q content blocks", EndpointPath, blk.Type))
		}
	}
	// Aggregated text ships verbatim (repo-wide verbatim policy, matching
	// OutputAssembler.Render and the streaming legs): no trimming here.
	if text.Len() > 0 {
		msg.Content, _ = json.Marshal(text.String())
	}
	msg.ReasoningContent = reasoning.String()
	// Observed tool calls outrank the status-derived reason: a terminal
	// max_tokens cannot downgrade tool_calls to length (FR-006).
	finish := shared.TerminalReason(len(msg.ToolCalls) > 0, "tool_calls",
		shared.ClaudeStopToFinish(resp.StopReason))
	out := shared.CompletionEnvelope(resp.ID, resp.Model, time.Now().Unix(),
		[]map[string]any{{
			"index":         0,
			"message":       msg,
			"finish_reason": finish,
		}},
		shared.CCUsageFrom(resp.Usage.InputTokens+valueOrZero(resp.Usage.CacheRead)+valueOrZero(resp.Usage.CacheCreation), resp.Usage.OutputTokens, shared.UsageDetails{CachedTokens: resp.Usage.CacheRead}))
	b, _ := json.Marshal(out) // composed marshallable types only; cannot fail
	return b, nil
}

// decodeBlockInput normalizes a tool_use input payload: absent/empty
// decodes to an empty object so every tool_call carries valid arguments.
// The raw bytes come from an already-validated outer document, so they are
// always valid JSON.
func decodeBlockInput(raw json.RawMessage) any {
	if !shared.HasContent(raw) {
		return map[string]any{}
	}
	var v any
	json.Unmarshal(raw, &v)
	return v
}

// ---- Messages -> openai-response (Responses) (FR-006) ----

// claudeToResponses converts a Messages response into a Responses result:
// text blocks aggregate into an assistant output_text message item,
// tool_use blocks become function_call items, stop_reason end_turn maps to
// status completed, and input/output tokens map to input/output usage
// (FR-006). Thinking blocks fold into one reasoning item; redacted_thinking
// blocks are omitted.
func claudeToResponses(body []byte) ([]byte, *errclass.Error) {
	resp, eErr := decodeClaude(body)
	if eErr != nil {
		return nil, eErr
	}
	// Stop reason maps straight onto the Responses status via the shared
	// table (max_tokens -> incomplete, end_turn -> completed); function_call
	// items already carry the tool signal (FR-006).
	status := shared.ResponseStatusFromClaudeStop(resp.StopReason)
	out := shared.ResponsesResult{
		ID:     resp.ID,
		Object: "response",
		Model:  resp.Model,
		Status: status,
		Output: []any{},
		Usage:  shared.NewResponsesUsageFrom(resp.Usage.InputTokens+valueOrZero(resp.Usage.CacheRead)+valueOrZero(resp.Usage.CacheCreation), resp.Usage.OutputTokens, shared.UsageDetails{CachedTokens: resp.Usage.CacheRead, CacheWriteTokens: resp.Usage.CacheCreation}),
	}
	oa := shared.NewOutputAssembler(resp.ID)
	// Fix #2: all thinking blocks fold into ONE reasoning item placed at the
	// first thinking block's position, the same shape the stream
	// synthesizer renders (stream.go outputItems), so modes cannot diverge.
	// A pre-pass gathers the full text so the item can be appended in place.
	var thinking strings.Builder
	for _, blk := range resp.Content {
		if blk.Type == "thinking" {
			thinking.WriteString(blk.Thinking)
		}
	}
	reasoningPlaced := false
	for _, blk := range resp.Content {
		switch blk.Type {
		case "text":
			// The shared assembler reserves the first text block's slot so
			// the output array matches the streaming synthesizer's
			// upstream-order emission (stream.go outputItems): identical
			// input must not reorder by mode (FR-006).
			oa.ReserveTextSlot()
			oa.AddText(blk.Text)
		case "tool_use":
			args, _ := json.Marshal(decodeBlockInput(blk.Input))
			oa.AppendFunctionCall(blk.ID, blk.Name, string(args))
		case "thinking":
			if !reasoningPlaced {
				reasoningPlaced = true
				oa.AppendReasoning(shared.NewRespReasoningItem(shared.ReasoningItemID(resp.ID, 0), thinking.String()))
			}
		case "redacted_thinking":
			// No plaintext to expose (see claudeToChat).
		default:
			return nil, errclass.Translation(fmt.Sprintf(
				"%s output cannot represent %q content blocks", EndpointPath, blk.Type))
		}
	}
	out.Output = oa.Render()
	b, _ := json.Marshal(out) // only marshallable composed types; cannot fail
	return b, nil
}

func valueOrZero(v *int64) int64 {
	if v != nil {
		return *v
	}
	return 0
}
