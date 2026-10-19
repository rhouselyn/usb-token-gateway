// Package xfmt: protocol transforms (DESIGN §7.5), OpenAI compatible surface,
// upstream openai/anthropic, SSE event mapping.  Gemini = M2 (501 for now).
package xfmt

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"usbridge/internal/vault"
)

const anthropicVersion = "2023-06-01"

var finishMap = map[string]string{
	"end_turn":      "stop",
	"stop_sequence": "stop",
	"max_tokens":    "length",
	"tool_use":      "tool_calls",
}

// OpenAIError renders the standard {"error": {...}} shape.
func OpenAIError(message, code string, status int) map[string]any {
	t := fmt.Sprintf("upstream_%d", status)
	if status == 0 {
		t = "bridge_error"
	}
	if code == "" {
		code = fmt.Sprintf("%d", status)
	}
	return map[string]any{
		"error": map[string]any{"message": message, "type": t, "code": code},
	}
}

func toJson(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}

/* ------------------------------------------------------------ request side */

// Build builds the upstream request (method, URL, body, headers).
type Req struct {
	Method, URL string
	Body        []byte
	Headers     map[string]string
}

func Build(ch *vault.Channel, body map[string]any) (*Req, error) {
	switch ch.Protocol {
	case "gemini":
		return nil, fmt.Errorf("gemini transform ships with M2; use /proxy passthrough meanwhile")
	case "anthropic":
		return buildAnthropic(ch, body), nil
	default:
		return buildOpenAI(ch, body), nil
	}
}

func authHeaders(ch *vault.Channel, headers map[string]string) map[string]string {
	if headers == nil {
		headers = map[string]string{}
	}
	if ch.AuthHeader == "bearer" {
		headers["Authorization"] = "Bearer " + ch.APIKey
	} else {
		headers["x-api-key"] = ch.APIKey
		headers["anthropic-version"] = anthropicVersion
	}
	return headers
}

func buildOpenAI(ch *vault.Channel, body map[string]any) *Req {
	base := strings.TrimRight(ch.BaseURL, "/")
	var url string
	switch {
	case strings.HasSuffix(base, "/chat/completions"):
		url = base
	default:
		url = base + "/chat/completions"
	}
	b := map[string]any{}
	for k, v := range body {
		b[k] = v
	}
	if m, ok := b["model"]; !ok || m == "" {
		if len(ch.Models) > 0 {
			b["model"] = ch.Models[0]
		}
	}
	return &Req{
		Method: "POST", URL: url,
		Body:    jsonMarshal(b),
		Headers: authHeaders(ch, map[string]string{
			"Content-Type": "application/json", "Accept": "application/json",
		}),
	}
}

func buildAnthropic(ch *vault.Channel, body map[string]any) *Req {
	base := strings.TrimRight(ch.BaseURL, "/")
	url := base + "/v1/messages"
	var systemParts []string
	type msg struct {
		Role    string      `json:"role"`
		Content interface{} `json:"content"`
	}
	var convo []msg
	raws, _ := body["messages"].([]any)
	for _, raw := range raws {
		mm, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := mm["role"].(string)
		content := mm["content"]
		switch role {
		case "system":
			switch c := content.(type) {
			case string:
				systemParts = append(systemParts, c)
			case []any:
				for _, seg := range c {
					if m, ok := seg.(map[string]any); ok {
						if s, ok := m["text"].(string); ok {
							systemParts = append(systemParts, s)
						}
					}
				}
			}
		case "assistant":
			convo = append(convo, msg{"assistant", content})
		default:
			convo = append(convo, msg{"user", content})
		}
	}
	model := str(body["model"])
	if model == "" && len(ch.Models) > 0 {
		model = ch.Models[0]
	}
	maxTok := float64p(body["max_tokens"])
	if maxTok <= 0 {
		maxTok = 1024
	}
	payload := map[string]any{
		"model":    model,
		"max_tokens": maxTok,
		"messages": convo,
		"stream":   truthy(body["stream"]),
	}
	if len(systemParts) > 0 {
		payload["system"] = strings.Join(systemParts, "\n")
	}
	if v, ok := body["temperature"]; ok {
		payload["temperature"] = v
	}
	if v, ok := body["top_p"]; ok {
		payload["top_p"] = v
	}
	if v, ok := body["stop"]; ok {
		payload["stop_sequences"] = v
	}
	headers := map[string]string{"Content-Type": "application/json", "Accept": "application/json"}
	return &Req{Method: "POST", URL: url, Body: jsonMarshal(payload), Headers: authHeaders(ch, headers)}
}

func str(v any) string { s, _ := v.(string); return s }
func truthy(v any) bool { b, _ := v.(bool); return b }
func float64p(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	}
	return 0
}

func jsonMarshal(v any) []byte { b, _ := json.Marshal(v); return b }

/* ----------------------------------------------------------- response side */

// ToOpenAI converts an upstream (non-stream) JSON response to OpenAI format.
func ToOpenAI(ch *vault.Channel, body map[string]any, upstream map[string]any) map[string]any {
	if ch.Protocol == "anthropic" {
		return anthropicToOpenAI(body, upstream)
	}
	return upstream
}

func anthropicToOpenAI(body map[string]any, aj map[string]any) map[string]any {
	var text strings.Builder
	if blocks, ok := aj["content"].([]any); ok {
		for _, b := range blocks {
			m, ok := b.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := m["type"].(string); ok && t == "text" {
				text.WriteString(str_1(m["text"]))
			}
		}
	}
	stop := "stop"
	if sr, ok := aj["stop_reason"].(string); ok {
		if m, ok2 := finishMap[sr]; ok2 {
			stop = m
		}
	}
	usage, _ := aj["usage"].(map[string]any)
	in, out := 0, 0
	if usage != nil {
		in = intF(usage["input_tokens"])
		out = intF(usage["output_tokens"])
	}
	return map[string]any{
		"id":      "chatcmpl-bridge-" + str_1(aj["id"]),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   str_1(body["model"]),
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": text.String()},
			"finish_reason": stop,
		}},
		"usage": map[string]any{"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out},
		"bridge": map[string]any{"upstream": "anthropic"},
	}
}

func str_1(v any) string { s, _ := v.(string); return s }
func intF(v any) int { f, _ := v.(float64); return int(f) }

/* --------------------------------------------------------------- SSE utils */

// SSEState carries context across events of one stream.
type SSEState struct {
	ID             string
	Model          string
	Created        int64
	Finish         string
	CompletionToks int
}

func (s *SSEState) mk(role, content, finish *string) []byte {
	delta := map[string]any{}
	if role != nil {
		delta["role"] = *role
	}
	if content != nil {
		delta["content"] = *content
	}
	payload := map[string]any{
		"id": s.ID, "object": "chat.completion.chunk",
		"created": s.Created, "model": s.Model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nilIf(finish)}},
	}
	b := noEscape(payload)
	return append(append([]byte("data: "), b...), '\n', '\n')
}

// noEscape marshals without HTML escaping so "<" stays "<".
func noEscape(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
	return bytes.TrimRight(buf.Bytes(), "\n")
}

func nilIf(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// AnthropicEventToChunks maps one anthropic SSE data-line to OpenAI chunks.
func AnthropicEventToChunks(line []byte, s *SSEState) [][]byte {
	raw := strings.TrimSpace(string(line))
	if raw == "" || strings.HasPrefix(raw, ":") ||
		strings.HasPrefix(raw, "event:") {
		return nil
	}
	if strings.HasPrefix(raw, "data:") {
		data := strings.TrimSpace(raw[5:])
		if data == "[DONE]" {
			return [][]byte{[]byte("data: [DONE]")}
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(data), &obj); err != nil {
			return nil
		}
		switch string_(obj["type"]) {
		case "message_start":
			role := "assistant"
			return [][]byte{s.mk(&role, nil, nil)}
		case "content_block_delta":
			delta, _ := obj["delta"].(map[string]any)
			switch string_(delta["type"]) {
			case "text_delta":
				if t := str_1(delta["text"]); t != "" {
					return [][]byte{s.mk(nil, &t, nil)}
				}
			case "input_json_delta":
				if t := str_1(delta["partial_json"]); t != "" {
					return [][]byte{s.mk(nil, &t, nil)}
				}
			}
		case "message_delta":
			d, _ := obj["delta"].(map[string]any)
			if m, ok := finishMap[string_(d["stop_reason"])]; ok {
				s.Finish = m
			}
			if u, ok := obj["usage"].(map[string]any); ok {
				s.CompletionToks = intF(u["output_tokens"])
			}
		case "message_stop":
			f := s.Finish
			if f == "" {
				f = "stop"
			}
			return [][]byte{s.mk(nil, nil, &f), []byte("data: [DONE]")}
		}
	}
	return nil
}

func string_(v any) string { s, _ := v.(string); return s }
