// Responses API <-> Chat Completions conversion (inbound /v1/responses).
//
// Serves OpenAI Codex CLI (model_providers.*.wire_api = "responses"): the
// request is reshaped into the chat-completions body the channel pipeline
// already speaks, and replies -- non-stream JSON and SSE -- are reshaped back
// into Responses objects / events.  Codex always sends the full conversation
// in `input` (stateless, store:false), so no server-side state is needed.
package xfmt

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

/* --------------------------------------------------------------- request */

// ResponsesToChat converts a /v1/responses request body into a
// /v1/chat/completions body.  Unknown input item types (reasoning, images)
// are dropped; tool types other than "function" (local_shell, web_search)
// are dropped because chat-completions upstreams only know functions.
func ResponsesToChat(body map[string]any) map[string]any {
	out := map[string]any{
		"model":  str(body["model"]),
		"stream": truthy(body["stream"]),
	}
	if v, ok := body["temperature"]; ok {
		out["temperature"] = v
	}
	if v, ok := body["top_p"]; ok {
		out["top_p"] = v
	}
	if mt := float64p(body["max_output_tokens"]); mt > 0 {
		out["max_tokens"] = mt
	}
	if v, ok := body["parallel_tool_calls"]; ok {
		out["parallel_tool_calls"] = v
	}
	if r, ok := body["reasoning"].(map[string]any); ok {
		if e := str(r["effort"]); e != "" {
			out["reasoning_effort"] = e
		}
	}

	var msgs []any
	if ins := str(body["instructions"]); ins != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": ins})
	}

	var tools []any
	if tl, ok := body["tools"].([]any); ok {
		for _, t := range tl {
			m, ok := t.(map[string]any)
			if !ok || str(m["type"]) != "function" {
				continue
			}
			tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
				"name":        str(m["name"]),
				"description": str(m["description"]),
				"parameters":  m["parameters"],
			}})
		}
	}
	if len(tools) > 0 {
		out["tools"] = tools
		switch tc := body["tool_choice"].(type) {
		case string:
			out["tool_choice"] = tc
		case map[string]any:
			if str(tc["type"]) == "function" {
				out["tool_choice"] = map[string]any{"type": "function",
					"function": map[string]any{"name": str(tc["name"])}}
			}
		}
	}

	switch in := body["input"].(type) {
	case string:
		msgs = append(msgs, map[string]any{"role": "user", "content": in})
	case []any:
		for _, it := range in {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			switch str(m["type"]) {
			case "function_call":
				callID := str(m["call_id"])
				if callID == "" {
					callID = str(m["id"])
				}
				args := str(m["arguments"])
				if args == "" {
					args = "{}"
				}
				msgs = append(msgs, map[string]any{"role": "assistant", "content": "",
					"tool_calls": []any{map[string]any{
						"id": callID, "type": "function",
						"function": map[string]any{"name": str(m["name"]), "arguments": args},
					}}})
			case "function_call_output":
				msgs = append(msgs, map[string]any{"role": "tool",
					"tool_call_id": str(m["call_id"]), "content": str(m["output"])})
			case "reasoning":
				// no chat-completions equivalent; upstream regenerates its own
			default: // "message" or a bare {role, content}
				role := str(m["role"])
				if role == "" || role == "developer" {
					role = "system"
				}
				if role != "system" && role != "assistant" {
					role = "user"
				}
				msgs = append(msgs, map[string]any{"role": role, "content": respTextContent(m["content"])})
			}
		}
	}
	out["messages"] = msgs
	if truthy(body["stream"]) {
		out["stream_options"] = map[string]any{"include_usage": true}
	}
	return out
}

// respTextContent flattens Responses content (string | parts[]) to chat text.
func respTextContent(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	parts, ok := v.([]any)
	if !ok {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if m, ok := p.(map[string]any); ok {
			b.WriteString(str(m["text"]))
		}
	}
	return b.String()
}

/* ------------------------------------------------------ non-stream reply */

// ChatToResponses converts a chat-completion (non-stream) JSON into a
// Responses response object.
func ChatToResponses(c map[string]any) map[string]any {
	id := "resp_" + strOr2(c["id"], "bridge")
	var output []any
	if choices, ok := c["choices"].([]any); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if msg, ok := choice["message"].(map[string]any); ok {
				if txt := str(msg["content"]); txt != "" {
					output = append(output, map[string]any{
						"id": id + "_msg", "type": "message", "role": "assistant",
						"status": "completed",
						"content": []any{map[string]any{
							"type": "output_text", "text": txt, "annotations": []any{}}},
					})
				}
				if tcs, ok := msg["tool_calls"].([]any); ok {
					for i, t := range tcs {
						tm, ok := t.(map[string]any)
						if !ok {
							continue
						}
						fn, _ := tm["function"].(map[string]any)
						output = append(output, map[string]any{
							"id": fmt.Sprintf("%s_fc_%d", id, i), "type": "function_call",
							"status": "completed", "call_id": str(tm["id"]),
							"name": str(fn["name"]), "arguments": strOr2(fn["arguments"], "{}"),
						})
					}
				}
			}
		}
	}
	out := map[string]any{
		"id": id, "object": "response", "created_at": time.Now().Unix(),
		"status": "completed", "model": str(c["model"]),
		"output": output, "store": false, "parallel_tool_calls": true,
	}
	if u, ok := c["usage"].(map[string]any); ok {
		inT, outT := intF(u["prompt_tokens"]), intF(u["completion_tokens"])
		out["usage"] = map[string]any{
			"input_tokens": inT, "output_tokens": outT, "total_tokens": inT + outT,
			"input_tokens_details":  map[string]any{"cached_tokens": 0},
			"output_tokens_details": map[string]any{"reasoning_tokens": 0},
		}
	}
	return out
}

/* -------------------------------------------------------------- SSE side */

type respItem struct {
	id     string
	typ    string // "message" | "function_call"
	chatIx int    // upstream tool_calls[] index (function_call only)
	callID string
	name   string
	args   strings.Builder
	text   strings.Builder
	closed bool
}

// RespSSE accumulates one streamed Responses reply from chat-completion
// chunks (openai upstream) or from anthropic events already converted to
// chat chunks (via AnthropicEventToChunks).
type RespSSE struct {
	ID      string
	Model   string
	Created int64
	Usage   map[string]any // raw openai usage of the LAST chat chunk
	Finish  string
	Done    bool

	started bool
	items   []*respItem
}

func NewRespSSE(model string) *RespSSE {
	return &RespSSE{
		ID:      "resp_bridge_" + fmt.Sprintf("%d", time.Now().UnixNano()),
		Model:   model,
		Created: time.Now().Unix(),
	}
}

// RespFromChatLine converts one upstream SSE line into Responses SSE frames.
//   - proto "openai":    line is a raw chat-completion chunk ("data: {...}")
//   - proto "anthropic": line is an anthropic event; cs converts it to chat
//     chunks first (cs may be shared with the anthropic path)
//
// The [DONE] sentinel (or the caller after stream EOF) triggers Finalize.
func RespFromChatLine(line []byte, proto string, rs *RespSSE, cs *SSEState) [][]byte {
	var chunks []map[string]any
	done := false
	grab := func(s string) {
		s = strings.TrimSpace(s)
		s = strings.TrimPrefix(s, "data:")
		s = strings.TrimSpace(s)
		if s == "" || strings.HasPrefix(s, ":") || strings.HasPrefix(s, "event:") {
			return
		}
		if s == "[DONE]" {
			done = true
			return
		}
		var m map[string]any
		if json.Unmarshal([]byte(s), &m) == nil && m != nil {
			chunks = append(chunks, m)
		}
	}
	if proto == "anthropic" {
		for _, c := range AnthropicEventToChunks(line, cs) {
			grab(string(c))
		}
	} else {
		grab(string(line))
	}

	var out [][]byte
	for _, c := range chunks {
		out = append(out, rs.FromChatChunk(c)...)
	}
	if done && !rs.Done {
		out = append(out, rs.Finalize()...)
	}
	return out
}

// FromChatChunk maps one chat-completion chunk to zero or more Responses
// events (item add, text/arguments deltas, item close).
func (rs *RespSSE) FromChatChunk(chunk map[string]any) [][]byte {
	var out [][]byte
	if !rs.started {
		rs.started = true
		out = append(out, rs.event("response.created",
			map[string]any{"response": rs.snapshot("in_progress")}))
	}
	if u, ok := chunk["usage"].(map[string]any); ok && u != nil {
		rs.Usage = u
	}
	choices, _ := chunk["choices"].([]any)
	for _, rc := range choices {
		ch, ok := rc.(map[string]any)
		if !ok {
			continue
		}
		if fr, ok := ch["finish_reason"].(string); ok && fr != "" {
			rs.Finish = fr
		}
		delta, _ := ch["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		if t := str(delta["content"]); t != "" {
			ix := len(rs.items)
			var item *respItem
			if rs.textOpen() {
				item = rs.items[ix-1]
			} else {
				item = &respItem{id: fmt.Sprintf("%s_msg_%d", rs.ID, ix), typ: "message"}
				rs.items = append(rs.items, item)
				out = append(out,
					rs.event("response.output_item.added", map[string]any{
						"output_index": ix, "item": itemView(item, "in_progress")}),
					rs.event("response.content_part.added", map[string]any{
						"item_id": item.id, "output_index": ix, "content_index": 0,
						"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}}))
			}
			item.text.WriteString(t)
			out = append(out, rs.event("response.output_text.delta", map[string]any{
				"item_id": item.id, "output_index": rs.indexOf(item), "content_index": 0,
				"delta": t, "logprobs": []any{}}))
		}
		if tcs, ok := delta["tool_calls"].([]any); ok {
			for _, t := range tcs {
				tm, ok := t.(map[string]any)
				if !ok {
					continue
				}
				ix := intF(tm["index"])
				item := rs.findCall(ix)
				if item == nil {
					fn, _ := tm["function"].(map[string]any)
					item = &respItem{
						id:  fmt.Sprintf("%s_fc_%d", rs.ID, len(rs.items)),
						typ: "function_call", chatIx: ix,
						callID: str(tm["id"]), name: str(fn["name"]),
					}
					rs.items = append(rs.items, item)
					out = append(out, rs.event("response.output_item.added", map[string]any{
						"output_index": rs.indexOf(item), "item": itemView(item, "in_progress")}))
				}
				if fn, ok := tm["function"].(map[string]any); ok {
					if n := str(fn["name"]); n != "" && item.name == "" {
						item.name = n
					}
					if a := str(fn["arguments"]); a != "" {
						item.args.WriteString(a)
						out = append(out, rs.event("response.function_call_arguments.delta", map[string]any{
							"item_id": item.id, "output_index": rs.indexOf(item), "delta": a}))
					}
				}
				if id := str(tm["id"]); id != "" && item.callID == "" {
					item.callID = id
				}
			}
		}
	}
	if rs.Finish != "" {
		out = append(out, rs.closeOpen()...)
	}
	return out
}

// Finalize closes every open item and emits response.completed.  Called on
// the upstream [DONE] sentinel, or by the SSE pump on premature EOF.
func (rs *RespSSE) Finalize() [][]byte {
	out := rs.closeOpen()
	rs.Done = true
	completed := rs.snapshot("completed")
	if u := rs.usageMap(); u != nil {
		completed["usage"] = u
	}
	return append(out, rs.event("response.completed", map[string]any{"response": completed}))
}

func (rs *RespSSE) textOpen() bool {
	n := len(rs.items)
	return n > 0 && rs.items[n-1].typ == "message" && !rs.items[n-1].closed
}

func (rs *RespSSE) findCall(ix int) *respItem {
	for _, it := range rs.items {
		if it.typ == "function_call" && it.chatIx == ix {
			return it
		}
	}
	return nil
}

func (rs *RespSSE) indexOf(it *respItem) int {
	for i, x := range rs.items {
		if x == it {
			return i
		}
	}
	return 0
}

// closeOpen emits the *.done events for every still-open item.
func (rs *RespSSE) closeOpen() [][]byte {
	var out [][]byte
	for i, it := range rs.items {
		if it.closed {
			continue
		}
		it.closed = true
		if it.typ == "message" {
			out = append(out,
				rs.event("response.output_text.done", map[string]any{
					"item_id": it.id, "output_index": i, "content_index": 0, "text": it.text.String()}),
				rs.event("response.content_part.done", map[string]any{
					"item_id": it.id, "output_index": i, "content_index": 0,
					"part": map[string]any{"type": "output_text", "text": it.text.String(), "annotations": []any{}}}))
		} else {
			out = append(out, rs.event("response.function_call_arguments.done", map[string]any{
				"item_id": it.id, "output_index": i, "arguments": it.args.String()}))
		}
		out = append(out, rs.event("response.output_item.done", map[string]any{
			"output_index": i, "item": itemView(it, "completed")}))
	}
	return out
}

func (rs *RespSSE) event(typ string, payload map[string]any) []byte {
	m := map[string]any{"type": typ}
	for k, v := range payload {
		m[k] = v
	}
	b := noEscape(m)
	return append(append([]byte("data: "), b...), '\n', '\n')
}

// snapshot builds the `response` object embedded in created/completed events.
func (rs *RespSSE) snapshot(status string) map[string]any {
	output := make([]any, 0, len(rs.items))
	for _, it := range rs.items {
		output = append(output, itemView(it, "completed"))
	}
	return map[string]any{
		"id": rs.ID, "object": "response", "created_at": rs.Created,
		"status": status, "model": rs.Model, "output": output,
		"store": false, "parallel_tool_calls": true,
	}
}

func (rs *RespSSE) usageMap() map[string]any {
	if rs.Usage == nil {
		return nil
	}
	inT, outT := intF(rs.Usage["prompt_tokens"]), intF(rs.Usage["completion_tokens"])
	return map[string]any{
		"input_tokens": inT, "output_tokens": outT, "total_tokens": inT + outT,
		"input_tokens_details":  map[string]any{"cached_tokens": 0},
		"output_tokens_details": map[string]any{"reasoning_tokens": 0},
	}
}

func itemView(it *respItem, status string) map[string]any {
	if it.typ == "function_call" {
		return map[string]any{
			"id": it.id, "type": "function_call", "status": status,
			"call_id": it.callID, "name": it.name, "arguments": it.args.String(),
		}
	}
	return map[string]any{
		"id": it.id, "type": "message", "status": status, "role": "assistant",
		"content": []any{map[string]any{
			"type": "output_text", "text": it.text.String(), "annotations": []any{}}},
	}
}

// strOr2 returns a if a non-empty string, else b.
func strOr2(a, b any) string {
	if s := str(a); s != "" {
		return s
	}
	return str(b)
}
