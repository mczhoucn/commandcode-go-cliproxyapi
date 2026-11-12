package messages

import (
	"encoding/json"
	"testing"
)

// DeepSeek's Anthropic surface reads output_config.effort and ignores
// thinking.budget_tokens. A client that expresses strength through
// output_config must therefore have it forwarded, or its choice is silently
// dropped on that upstream.
func TestOutputConfigEffortForwarded(t *testing.T) {
	body := []byte(`{"model":"commandcode/glm-5.2","max_tokens":1024,
	  "thinking":{"type":"enabled","budget_tokens":8192},
	  "output_config":{"effort":"low"},
	  "messages":[{"role":"user","content":"hi"}]}`)
	out, eErr := BuildRequest("glm-5.2", "claude", body, nil)
	if eErr != nil {
		t.Fatalf("build: %v", eErr)
	}
	var req struct {
		OutputConfig *struct {
			Effort string `json:"effort"`
		} `json:"output_config"`
		Thinking *struct {
			Type string `json:"type"`
		} `json:"thinking"`
	}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.OutputConfig == nil || req.OutputConfig.Effort != "low" {
		t.Fatalf("output_config = %+v, want effort low", req.OutputConfig)
	}
	if req.Thinking == nil || req.Thinking.Type != "enabled" {
		t.Fatalf("thinking control lost: %+v", req.Thinking)
	}
}

// A Chat Completions client's reasoning_effort must also surface as
// output_config.effort on the Anthropic upstream, since that is the only
// effort spelling the Messages wire carries.
func TestChatReasoningEffortBecomesOutputConfig(t *testing.T) {
	body := []byte(`{"model":"commandcode/glm-5.2","max_tokens":1024,
	  "reasoning_effort":"high",
	  "messages":[{"role":"user","content":"hi"}]}`)
	out, eErr := BuildRequest("glm-5.2", "openai", body, nil)
	if eErr != nil {
		t.Fatalf("build: %v", eErr)
	}
	var req struct {
		OutputConfig *struct {
			Effort string `json:"effort"`
		} `json:"output_config"`
	}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.OutputConfig == nil || req.OutputConfig.Effort != "high" {
		t.Fatalf("output_config = %+v, want effort high", req.OutputConfig)
	}
}

// A Responses client's reasoning.effort reaches the same place.
func TestResponsesEffortBecomesOutputConfig(t *testing.T) {
	body := []byte(`{"model":"commandcode/glm-5.2","max_output_tokens":1024,
	  "reasoning":{"effort":"medium"},
	  "input":[{"role":"user","content":"hi"}]}`)
	out, eErr := BuildRequest("glm-5.2", "openai-response", body, nil)
	if eErr != nil {
		t.Fatalf("build: %v", eErr)
	}
	var req struct {
		OutputConfig *struct {
			Effort string `json:"effort"`
		} `json:"output_config"`
	}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.OutputConfig == nil || req.OutputConfig.Effort != "medium" {
		t.Fatalf("output_config = %+v, want effort medium", req.OutputConfig)
	}
}

// An effort of "none" is a switch, not a strength level the upstream ranks,
// so no output_config is emitted for it. It must, however, send
// thinking {"type":"disabled"} so a model that defaults to reasoning is
// explicitly turned off rather than left reasoning (fix #15).
func TestNoneEffortEmitsNoOutputConfig(t *testing.T) {
	body := []byte(`{"model":"commandcode/glm-5.2","max_output_tokens":256,
	  "reasoning":{"effort":"none"},
	  "input":[{"role":"user","content":"hi"}]}`)
	out, eErr := BuildRequest("glm-5.2", "openai-response", body, nil)
	if eErr != nil {
		t.Fatalf("build: %v", eErr)
	}
	var req struct {
		OutputConfig *struct {
			Effort string `json:"effort"`
		} `json:"output_config"`
		Thinking *struct {
			Type string `json:"type"`
		} `json:"thinking"`
	}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.OutputConfig != nil {
		t.Fatalf("output_config emitted for none: %+v", req.OutputConfig)
	}
	if req.Thinking == nil || req.Thinking.Type != "disabled" {
		t.Fatalf("effort none must send thinking disabled, got: %+v", req.Thinking)
	}
}
