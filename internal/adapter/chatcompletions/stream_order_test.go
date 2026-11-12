package chatcompletions

import (
	"fmt"
	"strings"
	"testing"
)

// Fix #9 pin: Chat Completions addresses argument fragments by tool index,
// so a fragment for tool 0 may arrive after tool 1 started. The Claude
// target must never emit a delta after its block's content_block_stop and
// must not lose any argument text.
func TestStreamConverterClaudeLateToolFragment(t *testing.T) {
	sc := NewStreamConverter("claude")
	evs := feedAll(t, sc,
		`data: {"id":"r","model":"m","choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"f","arguments":"{\"a\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"c2","function":{"name":"g","arguments":"{\"b\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"2}"}}]}}]}`,
		`data: {"choices":[{"delta":{"content":"after"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`)
	assertCloseBeforeStart(t, evs)
	args := map[string]string{}
	idByIndex := map[float64]string{}
	var text string
	for _, e := range evs {
		idx, _ := e.Data["index"].(float64)
		switch e.Name {
		case "content_block_start":
			cb := e.Data["content_block"].(map[string]any)
			if cb["type"] == "tool_use" {
				idByIndex[idx] = cb["id"].(string)
			}
		case "content_block_delta":
			d := e.Data["delta"].(map[string]any)
			switch d["type"] {
			case "input_json_delta":
				args[idByIndex[idx]] += d["partial_json"].(string)
			case "text_delta":
				text += d["text"].(string)
			}
		}
	}
	if args["c1"] != `{"a":1}` || args["c2"] != `{"b":2}` {
		t.Fatalf("tool arguments lost or misrouted: %v", args)
	}
	if text != "after" {
		t.Fatalf("deferred text lost: %q", text)
	}
}

// Fix #16 pin: reasoning that resumes after text closed the first
// reasoning item opens a second item instead of streaming deltas into a
// done item, and the terminal output lists every item at its announced
// output_index.
func TestStreamResponsesReasoningAfterText(t *testing.T) {
	sc := NewStreamConverter("openai-response")
	evs := feedAll(t, sc,
		`data: {"id":"r","choices":[{"delta":{"reasoning_content":"one"}}]}`,
		`data: {"choices":[{"delta":{"content":"hi"}}]}`,
		`data: {"choices":[{"delta":{"reasoning_content":"two"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`)
	closed := map[string]bool{}
	addedAt := map[string]float64{}
	var output []any
	for _, e := range evs {
		switch e.Name {
		case "response.output_item.added":
			item := e.Data["item"].(map[string]any)
			addedAt[item["id"].(string)] = e.Data["output_index"].(float64)
		case "response.output_item.done":
			closed[e.Data["item"].(map[string]any)["id"].(string)] = true
		case "response.reasoning_summary_text.delta":
			if id := e.Data["item_id"].(string); closed[id] {
				t.Fatalf("reasoning delta sent to closed item %s: %v", id, e)
			}
		case "response.completed":
			output = e.Data["response"].(map[string]any)["output"].([]any)
		}
	}
	if addedAt["rs_r_0"] != 0 || addedAt["r"] != 1 || addedAt["rs_r_1"] != 2 {
		t.Fatalf("announced positions = %v", addedAt)
	}
	if !closed["rs_r_0"] || !closed["rs_r_1"] || !closed["r"] {
		t.Fatalf("every item must close: %v", closed)
	}
	if len(output) != 3 {
		t.Fatalf("terminal output = %v", output)
	}
	for pos, id := range []string{"rs_r_0", "r", "rs_r_1"} {
		if got := output[pos].(map[string]any)["id"]; got != id {
			t.Fatalf("output[%d] id = %v, want %s", pos, got, id)
		}
	}
	if !strings.Contains(fmt.Sprint(output[2]), "two") {
		t.Fatalf("second reasoning text lost: %v", output[2])
	}
}
