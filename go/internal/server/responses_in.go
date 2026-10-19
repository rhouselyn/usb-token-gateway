// responses_in.go: /v1/responses inbound (OpenAI Responses API) -- the wire
// protocol Codex CLI speaks natively.  Requests are converted to the
// chat-completions pipeline (openai OR anthropic upstreams), replies are
// converted back; see internal/xfmt/respconv.go.
package server

import (
	"net/http"

	"usbridge/internal/xfmt"
)

func (hb *httpBridge) responses(w http.ResponseWriter, r *http.Request) {
	if hb.guardLocked(w) || !hb.guardDownstream(w, r) {
		return
	}
	body, err := readJSON(r)
	if err != nil {
		writeJSON(w, 400, xfmt.OpenAIError(err.Error(), "400", 400))
		return
	}
	hb.chatCore(w, r, xfmt.ResponsesToChat(body), "responses")
}
