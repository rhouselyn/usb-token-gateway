package server

// ------------------------------------------------------------------ /v1/messages
// Anthropic-native INBOUND endpoint (M2.5): Claude-syntax clients talk to the
// gateway exactly like they would to api.anthropic.com, while the routing /
// failover / usage machinery stays the OpenAI-shaped core.
//
//   client (anthropic syntax) -> /v1/messages -> openai-ish core -> channel
//   response: converted back to anthropic message shape (stream & non-stream)

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var bubbleID = regexp.MustCompile(`[^a-zA-Z0-9]`)

// messages is the POST /v1/messages handler.
func (hb *httpBridge) messages(w http.ResponseWriter, r *http.Request) {
	if hb.guardLocked(w) || !hb.guardDownstream(w, r) {
		return
	}
	body, err := readJSON(r)
	if err != nil {
		writeJSON(w, 400, outErr("anthropic", err.Error(), "invalid_request_error", 400))
		return
	}
	if strV(body["model"]) == "" {
		writeJSON(w, 400, outErr("anthropic", "model is required", "invalid_request_error", 400))
		return
	}
	// translate to the internal openai-ish shape and run the shared pipeline
	inner := anthropicReqToOpenAI(body)
	hb.chatCore(w, r, inner, "anthropic")
}

// anthropicReqToOpenAI translates a Claude-syntax body into the internal form.
func anthropicReqToOpenAI(body map[string]any) map[string]any {
	ob := map[string]any{
		"model":  strV(body["model"]),
		"stream": boolV(body["stream"]),
	}
	if mt, ok := body["max_tokens"].(float64); ok {
		ob["max_tokens"] = mt
	}
	if t, ok := body["temperature"].(float64); ok {
		ob["temperature"] = t
	}
	if tp, ok := body["top_p"].(float64); ok {
		ob["top_p"] = tp
	}
	if ss, ok := body["stop_sequences"].([]any); ok && len(ss) > 0 {
		ob["stop"] = ss
	}
	msgs := []any{}
	if s := blocksText(body["system"]); s != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": s})
	}
	if raw, ok := body["messages"].([]any); ok {
		for _, v := range raw {
			m, _ := v.(map[string]any)
			role := strV(m["role"])
			if role != "assistant" {
				role = "user" // anthropic allows only user/assistant anyway
			}
			if txt := blocksText(m["content"]); txt != "" {
				msgs = append(msgs, map[string]any{"role": role, "content": txt})
			}
		}
	}
	ob["messages"] = msgs
	return ob
}

// blocksText flattens anthropic content (string or [{type:"text",text:...}])
// into a single plain string.
func blocksText(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, it := range c {
			m, _ := it.(map[string]any)
			if strV(m["type"]) == "text" {
				b.WriteString(strV(m["text"]))
			}
		}
		return b.String()
	}
	return ""
}

// openAIOutToAnthropic converts a normalized OpenAI response into an
// anthropic "message" object.  usage stays prompt/completion-shaped upstream;
// here it maps to input/output tokens.
func openAIOutToAnthropic(out map[string]any, model string) map[string]any {
	text := ""
	if ch, ok := out["choices"].([]any); ok && len(ch) > 0 {
		c0, _ := ch[0].(map[string]any)
		if msg, ok := c0["message"].(map[string]any); ok {
			text = strV(msg["content"])
		}
	}
	stop := "end_turn"
	switch f := out["finish_reason"]; f {
	case "length":
		stop = "max_tokens"
	case "tool_calls":
		stop = "tool_use"
	}
	u, _ := out["usage"].(map[string]any)
	return map[string]any{
		"id":            "msg_" + bubbleID.ReplaceAllString(fmt.Sprintf("%d", time.Now().UnixNano()), ""),
		"type":          "message",
		"role":          "assistant",
		"content":       []any{map[string]any{"type": "text", "text": text}},
		"model":         model,
		"stop_reason":   stop,
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  intF(u["prompt_tokens"]),
			"output_tokens": intF(u["completion_tokens"]),
		},
	}
}

/* ------------------------------------------------------------- SSE mapping */

// anthropicSSEState turns OpenAI chat chunks into anthropic SSE events.
type anthropicSSEState struct {
	Model   string
	Started bool   // message_start emitted
	Block   bool   // content_block open
	Finish  string // openai finish_reason seen
	OutTok  int    // rough output tokens (text pieces)
	Stopped bool
}

var finishToStop = map[string]string{
	"stop":       "end_turn",
	"length":     "max_tokens",
	"tool_calls": "tool_use",
	"eos_token":  "end_turn",
}

// anthropicFromOpenAIFn maps one raw upstream openai SSE line to 0..n
// anthropic events, each already re-framed as `data: {...}\n\n`.
func anthropicFromOpenAIFn(line []byte, st *anthropicSSEState) [][]byte {
	s := string(line)
	payload := s
	if i := strings.Index(s, "data:"); i >= 0 {
		payload = strings.TrimSpace(s[i+5:])
	}
	if payload == "" {
		return nil
	}
	if payload == "[DONE]" {
		events := anthropicFinalize(st)
		st.Stopped = true
		return [][]byte{events}
	}
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) != nil {
		return nil
	}

	// error chunk shaped {"error": {...}} -> anthropic error event
	if e, ok := obj["error"].(map[string]any); ok {
		return [][]byte{anthropicFrame(mustJSON(map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    strOr(e["type"], "upstream_error"),
				"message": strOr(e["message"], "upstream error"),
			},
		}))}
	}

	var out [][]byte
	if !st.Started {
		st.Started = true
		out = append(out, anthropicFrame(mustJSON(map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":      "msg_" + bubbleID.ReplaceAllString(fmt.Sprintf("%d", time.Now().UnixNano()), ""),
				"type":    "message",
				"role":    "assistant",
				"content": []any{},
				"model":   st.Model,
				"usage":   map[string]any{"input_tokens": 0, "output_tokens": 0},
			},
		})))
	}
	if chs, ok := obj["choices"].([]any); ok && len(chs) > 0 {
		c0, _ := chs[0].(map[string]any)
		if d, ok := c0["delta"].(map[string]any); ok {
			if piece := strV(d["content"]); piece != "" {
				st.OutTok += len([]rune(piece))/4 + 1
				if !st.Block {
					st.Block = true
					out = append(out, anthropicFrame(mustJSON(map[string]any{
						"type":          "content_block_start",
						"index":         0,
						"content_block": map[string]any{"type": "text", "text": ""},
					})))
				}
				out = append(out, anthropicFrame(mustJSON(map[string]any{
					"type":  "content_block_delta",
					"index": 0,
					"delta": map[string]any{"type": "text_delta", "text": piece},
				})))
			}
		}
		if fr := strV(c0["finish_reason"]); fr != "" {
			st.Finish = fr
		}
	}
	return out
}

// anthropicFinalize builds the closing events (block stop, message_delta,
// message_stop), each already framed as `data: {...}\n\n`, concatenated.
func anthropicFinalize(st *anthropicSSEState) []byte {
	stop := "end_turn"
	if m, ok := finishToStop[st.Finish]; ok {
		stop = m
	}
	var b strings.Builder
	if st.Block {
		b.Write(anthropicFrame(mustJSON(map[string]any{
			"type": "content_block_stop", "index": 0})))
	}
	b.Write(anthropicFrame(mustJSON(map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": st.OutTok},
	})))
	b.Write(anthropicFrame(mustJSON(map[string]any{"type": "message_stop"})))
	return []byte(b.String())
}

// anthropicFrame prefixes event payload with `data: ` and suffixes \n\n.
func anthropicFrame(payload []byte) []byte {
	out := append([]byte("data: "), payload...)
	return append(out, '\n', '\n')
}

// mustJSON marshals without HTML escaping (same rule as xfmt's SSE writer:
// "<" must not become "\u003c" inside streamed text).
func mustJSON(v any) []byte {
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return []byte("{}")
	}
	return bytesTrim(buf.String())
}
