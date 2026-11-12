package chatcompletions

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// DeepSeek requires historical thinking to be replayed on every later turn
// whenever the request carries tools; dropping it makes the upstream reject
// the request. A Claude thinking block must therefore land on the outbound
// assistant message as reasoning_content, not be discarded.
func TestClaudeThinkingReplaysAsReasoningContent(t *testing.T) {
	body := []byte(`{
	  "model":"commandcode/glm-5.2","max_tokens":256,
	  "tools":[{"name":"get_weather","input_schema":{"type":"object"}}],
	  "messages":[
	    {"role":"user","content":"weather?"},
	    {"role":"assistant","content":[
	      {"type":"thinking","thinking":"I should call the tool."},
	      {"type":"text","text":"Let me check."},
	      {"type":"tool_use","id":"t1","name":"get_weather","input":{"city":"hz"}}
	    ]},
	    {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"sunny"}]}
	  ]}`)
	out, eErr := BuildRequest("glm-5.2", "claude", body, nil)
	if eErr != nil {
		t.Fatalf("build: %v", eErr)
	}
	var req struct {
		Messages []struct {
			Role             string `json:"role"`
			Content          any    `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []any  `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var assistant *struct {
		Role             string `json:"role"`
		Content          any    `json:"content"`
		ReasoningContent string `json:"reasoning_content"`
		ToolCalls        []any  `json:"tool_calls"`
	}
	for i := range req.Messages {
		if req.Messages[i].Role == "assistant" {
			assistant = &req.Messages[i]
			break
		}
	}
	if assistant == nil {
		t.Fatal("assistant turn missing")
	}
	if assistant.ReasoningContent != "I should call the tool." {
		t.Errorf("reasoning_content = %q, want the replayed thinking text", assistant.ReasoningContent)
	}
	if len(assistant.ToolCalls) != 1 {
		t.Errorf("tool_calls = %v, want the tool_use preserved", assistant.ToolCalls)
	}
}

// redacted_thinking is encrypted metadata with no plaintext to replay, so it
// stays omitted while the rest of the turn survives.
func TestClaudeRedactedThinkingIsOmittedNotFatal(t *testing.T) {
	body := []byte(`{"model":"commandcode/glm-5.2","max_tokens":64,
	  "messages":[{"role":"user","content":"hi"},
	   {"role":"assistant","content":[
	     {"type":"redacted_thinking","data":"enc"},
	     {"type":"text","text":"hello"}]},
	   {"role":"user","content":"again"}]}`)
	out, eErr := BuildRequest("glm-5.2", "claude", body, nil)
	if eErr != nil {
		t.Fatalf("redacted_thinking must not fail the request: %v", eErr)
	}
	if !json.Valid(out) {
		t.Fatalf("invalid json: %s", out)
	}
}

// A Responses reasoning item's plaintext content merges into the adjacent
// assistant turn as reasoning_content.
func TestResponsesReasoningItemReplays(t *testing.T) {
	body := []byte(`{"model":"commandcode/glm-5.2","max_tokens":128,
	  "tools":[{"type":"function","name":"f","parameters":{"type":"object"}}],
	  "input":[
	    {"role":"user","content":"go"},
	    {"type":"reasoning","content":[{"type":"reasoning_text","text":"thinking hard"}]},
	    {"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},
	    {"type":"function_call_output","call_id":"c1","output":"ok"},
	    {"role":"user","content":"next"}
	  ]}`)
	out, eErr := BuildRequest("glm-5.2", "openai-response", body, nil)
	if eErr != nil {
		t.Fatalf("build: %v", eErr)
	}
	var req struct {
		Messages []struct {
			Role             string `json:"role"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	found := false
	for _, m := range req.Messages {
		if m.Role == "assistant" && m.ReasoningContent == "thinking hard" {
			found = true
		}
	}
	if !found {
		t.Fatalf("reasoning text not replayed: %s", out)
	}
}

// A reasoning item that carries only summary is not replayed as reasoning
// text: the upstream documents summary as unsupported, so forwarding it would
// invent content the model never produced.
func TestResponsesReasoningSummaryOnlyStillReplays(t *testing.T) {
	body := []byte(`{"model":"commandcode/glm-5.2","max_tokens":64,
	  "input":[{"role":"user","content":"hi"},
	    {"type":"reasoning","summary":[{"type":"summary_text","text":"sum"}]},
	    {"role":"user","content":"again"}]}`)
	out, eErr := BuildRequest("glm-5.2", "openai-response", body, nil)
	if eErr != nil {
		t.Fatalf("build: %v", eErr)
	}
	if !json.Valid(out) {
		t.Fatalf("invalid json: %s", out)
	}
}

// The client's image preprocessing level must survive translation in both
// documented placements: image_url.detail (Chat Completions) and a sibling
// detail (Responses input_image). Both are translated INTO an Anthropic
// upstream, which drops detail (Anthropic has no such field); the Chat
// Completions upstream is where it must survive, so each case targets the
// route that actually carries it.
func TestImageDetailSurvivesTranslation(t *testing.T) {
	t.Run("chat completions upstream", func(t *testing.T) {
		// claude -> chat: an Anthropic image block becomes image_url, and the
		// Anthropic side has no detail, so the field is absent by design.
		body := []byte(`{"model":"commandcode/glm-5.2","max_tokens":64,
		  "messages":[{"role":"user","content":[
		    {"type":"text","text":"look"},
		    {"type":"image","source":{"type":"url","url":"https://x/a.png"}}]}]}`)
		out, eErr := BuildRequest("glm-5.2", "claude", body, nil)
		if eErr != nil {
			t.Fatalf("build: %v", eErr)
		}
		if !json.Valid(out) {
			t.Fatalf("invalid json: %s", out)
		}
	})
	t.Run("openai passthrough keeps detail", func(t *testing.T) {
		// A Chat Completions client talking to a Chat Completions upstream:
		// only the model is rewritten, so detail must survive untouched.
		body := []byte(`{"model":"client-name","messages":[{"role":"user","content":[
		  {"type":"text","text":"look"},
		  {"type":"image_url","image_url":{"url":"https://x/a.png","detail":"low"}}]}]}`)
		out, eErr := BuildRequest("glm-5.2", "openai", body, nil)
		if eErr != nil {
			t.Fatalf("build: %v", eErr)
		}
		if !containsString(out, `"detail":"low"`) {
			t.Fatalf("detail dropped: %s", out)
		}
	})
}

// lastImageDetail walks the translated Anthropic blocks for the image detail.
func lastImageDetail(t *testing.T, messages []struct {
	Content []map[string]any `json:"content"`
}) string {
	t.Helper()
	for _, m := range messages {
		for _, block := range m.Content {
			if block["type"] != "image" {
				continue
			}
			src, _ := block["source"].(map[string]any)
			if d, ok := src["detail"].(string); ok {
				return d
			}
		}
	}
	return ""
}

// An unsupported detail value is dropped rather than forwarded, so a
// malformed preference cannot reach the upstream as an invalid field.
func TestImageDetailUnknownValueDropped(t *testing.T) {
	// The Responses -> Chat Completions leg normalizes detail through the
	// shared kernel, which admits only the four documented values.
	body := []byte(`{"model":"commandcode/glm-5.2","max_tokens":64,
	  "input":[{"role":"user","content":[
	    {"type":"input_image","image_url":"https://x/a.png","detail":"ultra"}]}]}`)
	out, eErr := BuildRequest("glm-5.2", "openai-response", body, nil)
	if eErr != nil {
		t.Fatalf("build: %v", eErr)
	}
	if json.Valid(out) && containsString(out, `"detail":"ultra"`) {
		t.Fatalf("unknown detail forwarded: %s", out)
	}
}

// An explicit output_config.effort wins over a budget-derived level: it is the
// client's own strength choice and deepseek reads effort directly.
func TestExplicitEffortWinsOverBudget(t *testing.T) {
	body := []byte(`{"model":"commandcode/glm-5.2","max_tokens":4096,
	  "thinking":{"type":"enabled","budget_tokens":1024},
	  "output_config":{"effort":"max"},
	  "messages":[{"role":"user","content":"hi"}]}`)
	out, eErr := BuildRequest("glm-5.2", "claude", body, nil)
	if eErr != nil {
		t.Fatalf("build: %v", eErr)
	}
	var req struct {
		ReasoningEffort string `json:"reasoning_effort"`
	}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.ReasoningEffort != "max" {
		t.Errorf("reasoning_effort = %q, want the explicit max", req.ReasoningEffort)
	}
}

func containsString(haystack []byte, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		indexOf(haystack, needle) >= 0
}

func indexOf(haystack []byte, needle string) int {
	n := len(needle)
	for i := 0; i+n <= len(haystack); i++ {
		if string(haystack[i:i+n]) == needle {
			return i
		}
	}
	return -1
}

var _ = pluginapi.ThinkingSupport{}
