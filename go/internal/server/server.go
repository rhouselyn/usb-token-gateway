// Package server: gateway core (net/http, stdlib only) -- /v1/* + /proxy + SSE.
// Admin routes live in admin.go.
package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"usbridge/internal/audit"
	"usbridge/internal/bridge"
	"usbridge/internal/sched"
	"usbridge/internal/vault"
	"usbridge/internal/webui"
	"usbridge/internal/xfmt"
)

type Options struct {
	DataDir      string
	Host         string
	PortOverride int
}

func Run(b *bridge.Bridge, opts Options) (*http.Server, int) {
	port := opts.PortOverride
	if port == 0 {
		port = b.Port()
	}
	if port == 0 {
		port = 8989
	}
	host := opts.Host
	if host == "" {
		if b.Policy().BindLan {
			host = "0.0.0.0" // LAN / phone access (DESIGN §4)
		} else {
			host = "127.0.0.1"
		}
	}
	hb := &httpBridge{B: b}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		st := b.State()
		st["lan_ip"] = lanIP()
		writeJSON(w, 200, st)
	})
	mux.HandleFunc("/v1/models", hb.models)
	mux.HandleFunc("/v1/chat/completions", hb.chat)
	mux.HandleFunc("/v1/messages", hb.messages)   // anthropic-native inbound
	mux.HandleFunc("/v1/responses", hb.responses) // OpenAI Responses API (Codex CLI)
	mux.HandleFunc("/v1/embeddings", hb.embeddings)
	mux.HandleFunc("/proxy/", hb.proxy)
	mux.HandleFunc("/admin", hb.adminPage)
	mux.HandleFunc("/admin/", hb.adminAPI)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 404, xfmt.OpenAIError("not found", "404", 404))
	})
	srv := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", host, port),
		Handler:           http.HandlerFunc(withRecover(mux.ServeHTTP)),
		ReadHeaderTimeout: 30 * time.Second,
	}
	ln, lerr := net.Listen("tcp", srv.Addr)
	if lerr != nil {
		// almost always: another instance of the bridge already owns the port
		fmt.Printf("[bridge] listen %s failed: %v\n", srv.Addr, lerr)
		return srv, 0 // caller must handle 0 (port taken)
	}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			fmt.Printf("[bridge] serve %s: %v\n", srv.Addr, err)
		}
	}()
	return srv, port
}

// withRecover turns a panicking handler into a 502 instead of a dropped socket.
func withRecover(next func(http.ResponseWriter, *http.Request)) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				fmt.Printf("[bridge] panic in %s %s: %v\n", r.Method, r.URL.Path, rec)
				writeJSON(w, 502, xfmt.OpenAIError("internal bridge panic", "502", 502))
			}
		}()
		next(w, r)
	}
}

/* ---------------------------------------------------------------- plumbing */

type httpBridge struct{ B *bridge.Bridge }

func writeJSON(w http.ResponseWriter, status int, obj any) {
	payload, _ := json.Marshal(obj)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
	w.Header().Set("X-BRG-Server", "USBBridge/0.2-go")
	w.WriteHeader(status)
	w.Write(payload)
}

func okJSON(w http.ResponseWriter, obj map[string]any) { writeJSON(w, 200, obj) }

func readJSON(r *http.Request) (map[string]any, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("request body is not valid json")
	}
	return m, nil
}

func bearerKey(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return ""
	}
	return strings.TrimSpace(auth[7:])
}

func (hb *httpBridge) guardLocked(w http.ResponseWriter) bool {
	if hb.B.Locked() {
		writeJSON(w, 503, xfmt.OpenAIError("bridge is locked; open /admin", "locked", 503))
		return true
	}
	return false
}

func (hb *httpBridge) guardDownstream(w http.ResponseWriter, r *http.Request) bool {
	key := bearerKey(r)
	if key == "" || hb.B.CheckDownstream(key) == nil {
		writeJSON(w, 401, xfmt.OpenAIError("invalid downstream key", "401", 401))
		return false
	}
	return true
}

func (hb *httpBridge) guardAdmin(w http.ResponseWriter, r *http.Request) bool {
	if hb.B.AdminOK(r.Header.Get("X-Admin-Token")) {
		return true
	}
	writeJSON(w, 401, xfmt.OpenAIError("admin token required (X-Admin-Token)", "401", 401))
	return false
}

// isLoopbackRemote reports whether the request came from this machine.
func isLoopbackRemote(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// adminSessionToken hands the CURRENT admin session token to a loopback caller
// while the bridge is already unlocked.  This makes "silent start on a
// remembered device -> tray -> open panel" seamless (no password re-entry).
//
//   - locked             -> 403: unlock with the master password instead
//   - non-loopback peer  -> 403: LAN/phone clients never get a free token
//
// Security note: the vault is already unlocked in this state and any local
// process could use the gateway anyway; 锁屏 / quitting drops the token.
func (hb *httpBridge) adminSessionToken(w http.ResponseWriter, r *http.Request) {
	if !isLoopbackRemote(r.RemoteAddr) {
		writeJSON(w, 403, xfmt.OpenAIError("admin token endpoint is loopback-only", "403", 403))
		return
	}
	tok := hb.B.AdminToken()
	if tok == "" {
		writeJSON(w, 403, xfmt.OpenAIError("bridge is locked -- unlock with master password", "403", 403))
		return
	}
	okJSON(w, map[string]any{"ok": true, "admin_token": tok})
}

func (hb *httpBridge) record(model, channel string, status int, firstMS float64, pin, pout int, errS string) {
	if a := hb.B.Audit(); a != nil {
		a.Record(audit.Record{
			Model: model, ChannelID: channel, Status: status,
			FirstByteMS: firstMS, PromptToks: pin, CompToks: pout, Error: errS,
		})
	}
}

func newUpstreamClient(timeoutFirst float64) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			ResponseHeaderTimeout: time.Duration(timeoutFirst * float64(time.Second)),
			MaxIdleConns:          64,
		},
	}
}

func boolV(v any) bool  { x, _ := v.(bool); return x }
func intF(v any) int    { f, _ := v.(float64); return int(f) }
func strV(v any) string { s, _ := v.(string); return s }
func strOr(v any, def string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return def
}
func jsonStr(v any) string { b, _ := json.Marshal(v); return string(b) }
func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func retryAfterSeconds(h string) float64 {
	var f float64
	fmt.Sscanf(h, "%f", &f)
	return f
}

func errText(raw []byte, fallback string) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) == nil {
		if e, ok := m["error"]; ok {
			switch ev := e.(type) {
			case string:
				return ev
			case map[string]any:
				if s, ok := ev["message"].(string); ok {
					return s
				}
			}
		}
	}
	return fallback
}

// splitModels: comma patterns -> trimmed unique list ("*"-aware passthrough).
func splitModels(spec string) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range strings.Split(spec, ",") {
		p = strings.TrimSpace(p)
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

func matchesModel(patterns []string, model string) bool {
	for _, pat := range patterns {
		if pat == model {
			return true
		}
		if strings.HasSuffix(pat, "*") && strings.HasPrefix(model, strings.TrimSuffix(pat, "*")) {
			return true
		}
		if strings.HasPrefix(pat, "*") && strings.HasSuffix(model, strings.TrimPrefix(pat, "*")) {
			return true
		}
	}
	return false
}

// candidatesFor mirrors DESIGN §7.2-7.3: health/ability filtered, strategy ordered.
// When nothing healthy matches but channels exist, fall back to "all matching,
// cooldown ignored" so single-channel users still get best-effort service.
func candidatesFor(sch *sched.Scheduler, model string) []*vault.Channel {
	schAll := sch.All()
	var fits, all []*vault.Channel
	for _, ch := range schAll {
		if !ch.Enabled || !matchesModel(ch.Models, model) {
			continue
		}
		if !sch.IsOpen(ch.ID) {
			fits = append(fits, ch)
		}
		all = append(all, ch)
	}
	if len(fits) == 0 {
		return sch.Order(all)
	}
	return sch.Order(fits)
}

/* --------------------------------------------------------------- /v1/models */

func (hb *httpBridge) models(w http.ResponseWriter, r *http.Request) {
	if hb.guardLocked(w) || !hb.guardDownstream(w, r) {
		return
	}
	seen := map[string]bool{}
	data := []any{}
	for _, ch := range hb.B.Vault().Data.Channels {
		if !ch.Enabled {
			continue
		}
		for _, m := range ch.Models {
			if m == "" || seen[m] {
				continue
			}
			seen[m] = true
			data = append(data, map[string]any{"id": m, "object": "model", "owned_by": ch.ID})
		}
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

/* --------------------------------------------------------------------- chat */

func (hb *httpBridge) chat(w http.ResponseWriter, r *http.Request) {
	if hb.guardLocked(w) || !hb.guardDownstream(w, r) {
		return
	}
	body, err := readJSON(r)
	if err != nil {
		writeJSON(w, 400, xfmt.OpenAIError(err.Error(), "400", 400))
		return
	}
	hb.chatCore(w, r, body, "openai")
}

// chatCore carries the whole request pipeline; outFmt selects the downstream
// wire protocol: "openai" (native) or "anthropic" (/v1/messages inbound).
func (hb *httpBridge) chatCore(w http.ResponseWriter, r *http.Request, body map[string]any, outFmt string) {
	model := strV(body["model"])
	stream := boolV(body["stream"])
	pol := hb.B.Policy()
	sch := hb.B.Sched()
	if sch == nil {
		writeJSON(w, 503, outErr(outFmt, "bridge is locked; open /admin", "locked", 503))
		return
	}

	all := sch.All()
	if len(all) == 0 {
		hb.record(model, "", 503, 0, 0, 0, "no channels configured")
		writeJSON(w, 503, outErr(outFmt, "no channels configured; open /admin", "503", 503))
		return
	}
	anyMatches := false
	for _, ch := range all {
		if ch.Enabled && matchesModel(ch.Models, model) {
			anyMatches = true
			break
		}
	}
	if model != "" && !anyMatches && model != "*" {
		hb.record(model, "", 503, 0, 0, 0, "no channel serves model")
		writeJSON(w, 503, outErr(outFmt, fmt.Sprintf("no channel serves model %q", model), "503", 503))
		return
	}

	candidates := candidatesFor(sch, model)
	if budget := pol.RetryBudget; budget >= 0 && len(candidates) > budget+1 {
		candidates = candidates[:budget+1]
	}

	t0 := time.Now()
	var lastErr string
	for attempt, ch := range candidates {
		attempt++
		reqOut, xerr := xfmt.Build(ch, body)
		if xerr != nil {
			sch.Report(ch.ID, false, 0, 0, 0)
			hb.record(model, ch.ID, 0, 0, 0, 0, xerr.Error())
			lastErr = xerr.Error()
			continue
		}
		hreq, err := http.NewRequest(r.Method, reqOut.URL, strings.NewReader(string(reqOut.Body)))
		if err != nil {
			lastErr = err.Error()
			continue
		}
		hreq = hreq.WithContext(r.Context()) // client disconnect => upstream cancel
		for k, v := range reqOut.Headers {
			hreq.Header.Set(k, v)
		}
		client := newUpstreamClient(pol.TimeoutFirstByteS)
		resp, err := client.Do(hreq)
		firstMS := msSince(t0)
		if err != nil {
			sch.Report(ch.ID, false, 0, 0, 0)
			hb.record(model, ch.ID, 0, firstMS, 0, 0, err.Error())
			lastErr = fmt.Sprintf("upstream connect/timeout on %s: %v", ch.ID, err)
			continue
		}
		if resp.StatusCode >= 400 {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			sch.Report(ch.ID, false, firstMS, resp.StatusCode, retryAfterSeconds(resp.Header.Get("Retry-After")))
			hb.record(model, ch.ID, resp.StatusCode, firstMS, 0, 0, firstN(string(raw), 200))
			lastErr = fmt.Sprintf("upstream %d from %s", resp.StatusCode, ch.ID)
			if attempt < len(candidates) {
				continue
			}
			final := resp.StatusCode
			if final >= 500 {
				final = 502 // per DESIGN §6.2
			}
			writeJSON(w, final, outErr(outFmt, errText(raw, lastErr), fmt.Sprintf("%d", resp.StatusCode), final))
			return
		}
		if stream {
			hb.sse(w, ch, body, resp, attempt, firstMS, pol.TimeoutStreamIdle, outFmt)
			return
		}
		raw, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if rerr != nil {
			sch.Report(ch.ID, false, firstMS, 0, 0)
			hb.record(model, ch.ID, 0, firstMS, 0, 0, rerr.Error())
			lastErr = fmt.Sprintf("upstream read error from %s: %v", ch.ID, rerr)
			if attempt < len(candidates) {
				continue
			}
			writeJSON(w, 502, xfmt.OpenAIError(lastErr, "502", 502))
			return
		}
		var upJSON map[string]any
		if json.Unmarshal(raw, &upJSON) != nil {
			sch.Report(ch.ID, false, firstMS, resp.StatusCode, 0)
			hb.record(model, ch.ID, resp.StatusCode, firstMS, 0, 0, "non-json upstream body")
			lastErr = fmt.Sprintf("non-json response from %s", ch.ID)
			if attempt < len(candidates) {
				continue
			}
			writeJSON(w, 502, xfmt.OpenAIError(lastErr, "502", 502))
			return
		}
		out := xfmt.ToOpenAI(ch, body, upJSON)
		sch.Report(ch.ID, true, firstMS, resp.StatusCode, 0)
		u, _ := out["usage"].(map[string]any)
		pu, cu := intF(u["prompt_tokens"]), intF(u["completion_tokens"])
		switch outFmt {
		case "anthropic":
			out = openAIOutToAnthropic(out, strV(body["model"]))
		case "responses":
			out = xfmt.ChatToResponses(out)
		}
		hb.record(model, ch.ID, 200, firstMS, pu, cu, "")
		payload, _ := json.Marshal(out)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
		w.Header().Set("X-BRG-Channel", ch.ID)
		w.Header().Set("X-BRG-Attempt", fmt.Sprintf("%d", attempt))
		w.Header().Set("X-BRG-Latency-First", fmt.Sprintf("%.0f", firstMS))
		w.WriteHeader(200)
		w.Write(payload)
		return
	}
	writeJSON(w, 502, xfmt.OpenAIError(lastErr, "upstream_exhausted", 502))
}

func msSince(t time.Time) float64 { return float64(time.Since(t).Microseconds()) / 1000.0 }

/* ---------------------------------------------------------------------- SSE */

func (hb *httpBridge) sse(w http.ResponseWriter, ch *vault.Channel, body map[string]any, resp *http.Response, attempt int, firstMS float64, idleS float64, outFmt string) {
	sch := hb.B.Sched()
	sch.Report(ch.ID, true, firstMS, resp.StatusCode, 0)
	hb.record(strV(body["model"]), ch.ID, 200, firstMS, 0, 0, "")

	upModel := strOr(body["model"], "stream")
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-BRG-Channel", ch.ID)
	w.Header().Set("X-BRG-Attempt", fmt.Sprintf("%d", attempt))
	w.Header().Set("X-BRG-Latency-First", fmt.Sprintf("%.0f", firstMS))
	w.WriteHeader(200)
	fl, _ := w.(http.Flusher)

	state := &xfmt.SSEState{
		ID:      "chatcmpl-bridge-stream",
		Model:   strOr(body["model"], "stream"),
		Created: time.Now().Unix(),
	}
	// NOTE: no manual chunk framing here -- net/http auto-chunks every write
	// and Flusher releases each one; writing our own "%x\r\n" framing would be
	// wrapped AGAIN by net/http and corrupt the stream.
	sawDONE := false
	writePayload := func(data []byte) error {
		if bytesContains(data, []byte("[DONE]")) {
			sawDONE = true
		}
		_, err := w.Write(data)
		if err == nil && fl != nil {
			fl.Flush()
		}
		return err
	}
	proto := ch.Protocol
	anthClient := outFmt == "anthropic"
	antOut := &anthropicSSEState{Model: upModel} // used only when anthClient && proto != "anthropic"
	var rs *xfmt.RespSSE                         // used only when outFmt == "responses"
	if outFmt == "responses" {
		rs = xfmt.NewRespSSE(upModel)
	}

	// idle watchdog: cancel the upstream if nothing arrives in idleS.
	idle := time.Duration(idleS * float64(time.Second))
	if idle <= 0 {
		idle = 120 * time.Second
	}
	timer := time.AfterFunc(idle, func() {
		resp.Body.Close()
	})
	defer timer.Stop()
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			timer.Reset(idle)
			line = trimEOL(line)
			if len(line) > 0 {
				var chunks [][]byte
				switch {
				case outFmt == "responses": // openai or anthropic upstream -> Responses events
					chunks = xfmt.RespFromChatLine(line, proto, rs, state)
				case proto == "anthropic" && !anthClient:
					chunks = xfmt.AnthropicEventToChunks(line, state)
				case proto != "anthropic" && anthClient:
					chunks = anthropicFromOpenAIFn(line, antOut)
				default:
					chunks = passEvent(line) // native fmt matches upstream fmt
				}
				if anthClient {
					s := string(line)
					if strings.Contains(s, "message_stop") || strings.Contains(s, "message_delta") {
						antOut.Stopped = true // real anthropic upstream already closed the message
					}
				}
				for _, c := range chunks {
					if writePayload(c) != nil {
						return // downstream gone
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	if outFmt == "responses" {
		if rs != nil && !rs.Done { // upstream died mid-stream without [DONE]
			for _, f := range rs.Finalize() {
				if writePayload(f) != nil {
					return
				}
			}
		}
		return
	}
	if !sawDONE && !anthClient { // upstream died mid-stream without [DONE]
		writePayload([]byte("data: [DONE]\n\n"))
	}
	if anthClient && !antOut.Stopped {
		writePayload(anthropicFinalize(antOut)) // message_delta + message_stop
	}
}

// outErr writes the failure in the client's protocol shape.
func outErr(outFmt, msg, code string, status int) map[string]any {
	if outFmt == "anthropic" {
		return map[string]any{
			"type":  "error",
			"error": map[string]any{"type": code, "message": msg},
		}
	}
	return xfmt.OpenAIError(msg, code, status)
}

func bytesContains(b, sub []byte) bool {
	return strings.Contains(string(b), string(sub))
}

func trimEOL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	return b
}

func passEvent(line []byte) [][]byte {
	if len(line) == 0 {
		return nil
	}
	out := line
	if !strings.HasPrefix(string(out), "data:") {
		out = append([]byte("data: "), out...)
	}
	out = append(out, '\n', '\n')
	return [][]byte{out}
}

// bytesTrim strips the trailing newline json.Encoder appends.
func bytesTrim(s string) []byte {
	return bytes.TrimRight([]byte(s), "\n")
}

// lanIP best-effort picks this PC's LAN address (for the panel phone hint).
func lanIP() string {
	if c, err := net.Dial("udp", "8.8.8.8:53"); err == nil {
		defer c.Close()
		if ip, ok := c.LocalAddr().(*net.UDPAddr); ok && !ip.IP.IsLoopback() && ip.IP != nil {
			return ip.IP.String()
		}
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				v4 := ipn.IP.To4()
				if v4 != nil && v4[0] != 127 {
					return v4.String()
				}
			}
		}
	}
	return ""
}

/* --------------------------------------------------------------- embeddings */

func (hb *httpBridge) embeddings(w http.ResponseWriter, r *http.Request) {
	if hb.guardLocked(w) || !hb.guardDownstream(w, r) {
		return
	}
	body, rerr := readJSON(r)
	if rerr != nil {
		writeJSON(w, 400, xfmt.OpenAIError(rerr.Error(), "400", 400))
		return
	}
	model := strV(body["model"])
	sch := hb.B.Sched()
	if sch == nil {
		writeJSON(w, 503, xfmt.OpenAIError("bridge is locked", "locked", 503))
		return
	}
	var ch *vault.Channel
	for _, c := range candidatesFor(sch, model) {
		if c.Protocol == "openai" {
			ch = c
			break
		}
	}
	if ch == nil {
		writeJSON(w, 501, xfmt.OpenAIError("no openai-protocol channel available for embeddings", "501", 501))
		return
	}
	base := strings.TrimRight(ch.BaseURL, "/")
	base = strings.TrimSuffix(base, "/chat/completions")
	target := base + "/embeddings"
	t0 := time.Now()
	client := newUpstreamClient(hb.B.Policy().TimeoutFirstByteS)
	req0, _ := http.NewRequest("POST", target, strings.NewReader(jsonStr(body)))
	req0.Header.Set("Content-Type", "application/json")
	if ch.AuthHeader == "bearer" {
		req0.Header.Set("Authorization", "Bearer "+ch.APIKey)
	} else {
		req0.Header.Set("x-api-key", ch.APIKey)
	}
	resp, err := client.Do(req0)
	if err != nil {
		hb.record(model, ch.ID, 0, 0, 0, 0, err.Error())
		writeJSON(w, 502, xfmt.OpenAIError(err.Error(), "502", 502))
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	firstMS := msSince(t0)
	sch.Report(ch.ID, resp.StatusCode < 400, firstMS, resp.StatusCode, 0)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-BRG-Channel", ch.ID)
	w.WriteHeader(resp.StatusCode)
	w.Write(raw)
}

/* -------------------------------------------------------------------- proxy */

func (hb *httpBridge) proxy(w http.ResponseWriter, r *http.Request) {
	if hb.guardLocked(w) || !hb.guardDownstream(w, r) {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/proxy/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		writeJSON(w, 404, xfmt.OpenAIError("usage: /proxy/{channel_id}/path", "404", 404))
		return
	}
	ch := hb.B.Sched().Get(parts[0])
	if ch == nil {
		writeJSON(w, 404, xfmt.OpenAIError("unknown channel "+parts[0], "404", 404))
		return
	}
	rest2 := ""
	if len(parts) > 1 {
		rest2 = parts[1]
	}
	target := strings.TrimRight(ch.BaseURL, "/") + "/" + rest2
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	t0 := time.Now()
	body, _ := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	req0, err := http.NewRequestWithContext(r.Context(), r.Method, target, strings.NewReader(string(body)))
	if err != nil {
		writeJSON(w, 502, xfmt.OpenAIError(err.Error(), "502", 502))
		return
	}
	for k, v := range r.Header {
		lk := strings.ToLower(k)
		if lk == "authorization" || lk == "host" || lk == "connection" {
			continue
		}
		for _, vv := range v {
			req0.Header.Add(k, vv)
		}
	}
	if ch.AuthHeader == "bearer" {
		req0.Header.Set("Authorization", "Bearer "+ch.APIKey)
	} else {
		req0.Header.Set("x-api-key", ch.APIKey)
		if req0.Header.Get("anthropic-version") == "" {
			req0.Header.Set("anthropic-version", "2023-06-01")
		}
	}
	client := newUpstreamClient(hb.B.Policy().TimeoutFirstByteS)
	resp, err := client.Do(req0)
	if err != nil {
		hb.record("proxy:"+r.Method, ch.ID, 0, 0, 0, 0, err.Error())
		writeJSON(w, 502, xfmt.OpenAIError("upstream unreachable: "+err.Error(), "502", 502))
		return
	}
	defer resp.Body.Close()
	firstMS := msSince(t0)
	hb.B.Sched().Report(ch.ID, resp.StatusCode < 400, firstMS, resp.StatusCode, 0)
	hb.record("proxy:"+r.Method, ch.ID, resp.StatusCode, firstMS, 0, 0, "")
	for k, v := range resp.Header {
		lk := strings.ToLower(k)
		if lk == "transfer-encoding" || lk == "connection" {
			continue
		}
		for _, vv := range v {
			w.Header().Add(k, vv)
		}
	}
	w.Header().Set("X-BRG-Channel", ch.ID)
	w.WriteHeader(resp.StatusCode)
	if _, enc := w.Header()["Content-Length"]; !enc && resp.ContentLength > 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", resp.ContentLength))
	}
	io.Copy(w, resp.Body)
}

/* ----------------------------------------------------------------- admin UI */

func (hb *httpBridge) adminPage(w http.ResponseWriter, r *http.Request) {
	page := webui.Page()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(page)))
	// never let a browser cache the panel: any exe update must appear on refresh
	w.Header().Set("Cache-Control", "no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(200)
	w.Write([]byte(page))
}
