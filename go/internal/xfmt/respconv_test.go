package xfmt

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	return m
}

func TestResponsesToChatBasics(t *testing.T) {
	in := mustJSON(t, `{
		"model": "gpt-5.1-codex", "stream": true, "store": false,
		"instructions": "be brief",
		"max_output_tokens": 512,
		"reasoning": {"effort": "high"},
		"tools": [
			{"type": "function", "name": "shell", "description": "run", "parameters": {"type": "object"}},
			{"type": "web_search"}
		],
		"input": [
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "hi"}]},
			{"type": "function_call", "call_id": "c1", "name": "shell", "arguments": "{\"cmd\":\"ls\"}"},
			{"type": "function_call_output", "call_id": "c1", "output": "out"},
			{"type": "reasoning", "summary": [{"type": "summary_text", "text": "hmm"}]},
			{"type": "message", "role": "user", "content": "plain"}
		]
	}`)
	out := ResponsesToChat(in)
	if out["model"] != "gpt-5.1-codex" || out["stream"] != true || out["max_tokens"] != float64(512) {
		t.Fatalf("bad scalars: %v", out)
	}
	if out["reasoning_effort"] != "high" {
		t.Fatalf("reasoning effort lost: %v", out)
	}
	msgs := out["messages"].([]any)
	if len(msgs) != 5 {
		t.Fatalf("want 5 messages, got %d: %v", len(msgs), msgs)
	}
	if m0 := msgs[0].(map[string]any); m0["role"] != "system" || m0["content"] != "be brief" {
		t.Fatalf("instructions not mapped: %v", m0)
	}
	if m1 := msgs[1].(map[string]any); m1["role"] != "user" || m1["content"] != "hi" {
		t.Fatalf("parts text not flattened: %v", m1)
	}
	if m2 := msgs[2].(map[string]any); m2["role"] != "assistant" {
		t.Fatalf("function_call not assistant: %v", m2)
	} else if tc := m2["tool_calls"].([]any)[0].(map[string]any); tc["id"] != "c1" {
		t.Fatalf("call_id lost: %v", tc)
	}
	if m3 := msgs[3].(map[string]any); m3["role"] != "tool" || m3["tool_call_id"] != "c1" || m3["content"] != "out" {
		t.Fatalf("function_call_output not mapped: %v", m3)
	}
	tools := out["tools"].([]any)
	if len(tools) != 1 { // web_search dropped
		t.Fatalf("want 1 tool, got %v", tools)
	}
	if _, ok := out["stream_options"].(map[string]any); !ok {
		t.Fatalf("stream_options missing for stream request")
	}
}

func TestChatToResponsesNonStream(t *testing.T) {
	chat := mustJSON(t, `{
		"id": "chatcmpl-1", "model": "m1",
		"choices": [{"index": 0, "finish_reason": "tool_calls", "message": {
			"role": "assistant", "content": "let me check",
			"tool_calls": [{"id": "c9", "type": "function", "function": {"name": "shell", "arguments": "{\"x\":1}"}}]}}],
		"usage": {"prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18}
	}`)
	r := ChatToResponses(chat)
	if r["object"] != "response" || r["status"] != "completed" {
		t.Fatalf("bad envelope: %v", r)
	}
	out := r["output"].([]any)
	if len(out) != 2 {
		t.Fatalf("want message+function_call, got %v", out)
	}
	u := r["usage"].(map[string]any)
	if u["input_tokens"] != 11 || u["output_tokens"] != 7 {
		t.Fatalf("usage not remapped: %v", u)
	}
}

func TestRespSSETextStream(t *testing.T) {
	rs := NewRespSSE("m1")
	var buf strings.Builder
	feed := func(s string) {
		for _, f := range RespFromChatLine([]byte(s), "openai", rs, nil) {
			buf.Write(f)
		}
	}
	feed(`data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"he"}}]}`)
	feed(`data: {"choices":[{"index":0,"delta":{"content":"llo"}}]}`)
	feed(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	feed(`data: [DONE]`)

	s := buf.String()
	for _, want := range []string{
		`"response.created"`,
		`"response.output_item.added"`,
		`"response.output_text.delta"`, `"he"`, `"llo"`,
		`"response.output_text.done"`, `"hello"`,
		`"response.output_item.done"`,
		`"response.completed"`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("stream missing %s:\n%s", want, s)
		}
	}
	if !rs.Done {
		t.Fatal("Done not set after [DONE]")
	}
	// completed event carries usage + full output
	if !strings.Contains(s, `"input_tokens":3`) || !strings.Contains(s, `"output_tokens":2`) {
		t.Fatalf("usage missing in completed:\n%s", s)
	}
}

func TestRespSSEToolCallStream(t *testing.T) {
	rs := NewRespSSE("m1")
	var frames [][]byte
	feed := func(s string) { frames = append(frames, RespFromChatLine([]byte(s), "openai", rs, nil)...) }
	feed(`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"shell","arguments":"{\"c"}}]}}]}`)
	feed(`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\":\"ls\"}"}}]}}]}`)
	feed(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)
	feed(`data: [DONE]`)
	s := ""
	for _, f := range frames {
		s += string(f)
	}
	for _, want := range []string{
		`"response.output_item.added"`, `"function_call"`, `"shell"`,
		`"response.function_call_arguments.delta"`, `{"c`, `\"ls\"`,
		`"response.function_call_arguments.done"`, `{\"c\":\"ls\"}`,
		`"response.output_item.done"`, `"call_id":"c1"`, `"response.completed"`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("tool stream missing %s:\n%s", want, s)
		}
	}
}

func TestRespSSEFinalizeOnEOF(t *testing.T) {
	rs := NewRespSSE("m1")
	RespFromChatLine([]byte(`data: {"choices":[{"index":0,"delta":{"content":"abc"}}]}`), "openai", rs, nil)
	var out []byte
	for _, f := range rs.Finalize() {
		out = append(out, f...)
	}
	s := string(out)
	if !strings.Contains(s, `"response.output_text.done"`) || !strings.Contains(s, `"response.completed"`) {
		t.Fatalf("EOF finalize incomplete:\n%s", s)
	}
}

func TestRespFromAnthropicLine(t *testing.T) {
	rs := NewRespSSE("m1")
	cs := &SSEState{ID: "x", Model: "m1", Created: 1}
	var buf strings.Builder
	feed := func(s string) {
		for _, f := range RespFromChatLine([]byte(s), "anthropic", rs, cs) {
			buf.Write(f)
		}
	}
	feed(`data: {"type":"message_start","message":{"role":"assistant"}}`)
	feed(`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hey"}}`)
	feed(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`)
	feed(`data: {"type":"message_stop"}`)
	s := buf.String()
	if !strings.Contains(s, `"response.output_text.delta"`) || !strings.Contains(s, "hey") ||
		!strings.Contains(s, `"response.completed"`) {
		t.Fatalf("anthropic->responses missing events:\n%s", s)
	}
}
