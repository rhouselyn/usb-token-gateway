// admin.go: panel JSON API -- state/unlock/summary/channels/dkeys/recent/cmd.
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"usbridge/internal/codexgen"
	"usbridge/internal/probe"
	"usbridge/internal/remember"
	"usbridge/internal/vault"
	"usbridge/internal/xfmt"
)

func mask(k string) string {
	if len(k) > 8 {
		return k[:4] + "****" + k[len(k)-4:]
	}
	if len(k) > 2 {
		return k[:2] + "****"
	}
	return "****"
}

func (hb *httpBridge) adminAPI(w http.ResponseWriter, r *http.Request) {
	sub := strings.TrimPrefix(r.URL.Path, "/admin/")
	switch {
	case sub == "" || sub == "index.html":
		hb.adminPage(w, r)
	case sub == "state":
		st := hb.B.State()
		st["lan_ip"] = lanIP()
		okJSON(w, st)
	case sub == "unlock" && r.Method == "POST":
		hb.adminUnlock(w, r)
	case sub == "token" && r.Method == "GET":
		hb.adminSessionToken(w, r)
	case sub == "cmd" && r.Method == "POST":
		hb.adminCmd(w, r)
	default:
		if !hb.guardAdmin(w, r) {
			return
		}
		switch sub {
		case "summary":
			s := hb.B.Audit().Summary()
			s["strategy"] = hb.B.Policy().Strategy
			okJSON(w, s)
		case "channels":
			okJSON(w, map[string]any{"channels": sanitizeChannels(hb.B.Vault().Data.Channels)})
		case "dkeys":
			okJSON(w, map[string]any{
				"keys":        sanitizeKeys(hb.B.Vault().Data.DownstreamKeys),
				"codex_model": hb.B.Vault().Data.CodexModel,
				"smart_model": hb.B.Vault().Data.SmartModel,
			})
		case "recent":
			okJSON(w, map[string]any{"recent": recentList(hb)})
		case "models":
			// all model patterns served by enabled channels, for the panel
			// quick-connect card (chips are one-click copyable)
			type mrow struct {
				Model   string `json:"model"`
				Channel string `json:"channel"`
			}
			var out []mrow
			seen := map[string]bool{}
			if hb.B.SmartModel() != "" {
				seen[vault.SmartAlias] = true
				out = append(out, mrow{vault.SmartAlias, "smart"})
			}
			for _, ch := range hb.B.Vault().Data.Channels {
				if !ch.Enabled {
					continue
				}
				for _, m := range ch.Models {
					if !seen[m] {
						seen[m] = true
						out = append(out, mrow{m, ch.ID})
					}
				}
			}
			if out == nil {
				out = []mrow{}
			}
			okJSON(w, map[string]any{"models": out})
		default:
			writeJSON(w, 404, xfmt.OpenAIError("unknown admin endpoint", "404", 404))
		}
	}
}

func recentList(hb *httpBridge) []map[string]any {
	out := []map[string]any{}
	if a := hb.B.Audit(); a != nil {
		for _, r := range a.Recent() {
			out = append(out, map[string]any{
				"ts": r.TS, "model": r.Model, "channel_id": r.ChannelID,
				"status": r.Status, "first_byte_ms": r.FirstByteMS,
				"prompt_tokens": r.PromptToks, "completion_tokens": r.CompToks,
				"error": r.Error,
			})
		}
	}
	return out
}

func (hb *httpBridge) adminUnlock(w http.ResponseWriter, r *http.Request) {
	body, err := readJSON(r)
	if err != nil {
		okJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	pw := strV(body["master_password"])
	if len(pw) < 8 {
		okJSON(w, map[string]any{"ok": false, "error": "master password must be at least 8 chars"})
		return
	}
	token, created, err := hb.B.Unlock(pw)
	if err != nil {
		okJSON(w, map[string]any{"ok": false, "error": "unlock failed: " + err.Error()})
		return
	}
	// per-device remember: DPAPI-scoped to this Windows user, never plaintext.
	// Absent flag = keep current remembered state; only explicit false clears.
	if set, ok := body["remember_device"]; ok {
		if boolV(set) {
			remember.Save(hb.B.DataDir(), vault.Path(hb.B.DataDir()), pw)
		} else {
			remember.Clear(hb.B.DataDir())
		}
	}
	okJSON(w, map[string]any{"ok": true, "admin_token": token, "fresh": created, "port": hb.B.Port()})
}

func sanitizeChannels(chs []*vault.Channel) []map[string]any {
	out := make([]map[string]any, 0, len(chs))
	for _, c := range chs {
		out = append(out, map[string]any{
			"id": c.ID, "protocol": c.Protocol, "base_url": c.BaseURL,
			"models": c.Models, "enabled": c.Enabled,
			"weight": c.Weight, "priority": c.Priority,
			"key_masked": mask(c.APIKey),
		})
	}
	return out
}

// syncCodex rewrites the bundled Codex CLI home (best effort).  Uses the
// actually-bound port, matching what /v1/* is serving right now.
func (hb *httpBridge) syncCodex() {
	v := hb.B.Vault()
	if v == nil {
		return
	}
	if err := codexgen.Sync(hb.B.DataDir(), hb.B.Port(), v.Data.DownstreamKeys, v.Data.CodexModel); err != nil {
		fmt.Println("[bridge] codex sync:", err)
	}
}

func sanitizeKeys(keys []*vault.DownstreamKey) []map[string]any {
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, map[string]any{
			"name": k.Name, "key": k.Key,
			"key_masked": mask(k.Key), "enabled": k.Enabled,
			"ttl_hours": k.TTLHours, "created": k.Created,
			"codex_use": k.CodexUse,
		})
	}
	return out
}

func (hb *httpBridge) adminCmd(w http.ResponseWriter, r *http.Request) {
	if hb.B.Locked() {
		writeJSON(w, 401, xfmt.OpenAIError("bridge locked", "locked", 401))
		return
	}
	if !hb.guardAdmin(w, r) {
		return
	}
	body, err := readJSON(r)
	if err != nil {
		okJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	action := strV(body["action"])
	arg := strV(body["arg"])
	v := hb.B.Vault()
	fail := func(e string) { okJSON(w, map[string]any{"ok": false, "error": e}) }
	done := func(extra map[string]any) {
		if err := hb.B.Save(); err != nil {
			fail(err.Error())
			return
		}
		hb.syncCodex() // panel change -> bundled Codex CLI home stays in sync
		obj := map[string]any{"ok": true}
		for k, v2 := range extra {
			obj[k] = v2
		}
		okJSON(w, obj)
	}
	switch action {
	case "add_channel":
		ch := &vault.Channel{
			Protocol: strOr(body["proto"], "openai"),
			BaseURL:  strV(body["base_url"]),
			APIKey:   strV(body["api_key"]),
			Weight:   intF(body["weight"]),
			Priority: intF(body["priority"]),
		}
		if ch.BaseURL == "" || ch.APIKey == "" {
			fail("base_url and api_key required")
			return
		}
		if s := strV(body["models"]); s != "" {
			for _, m := range strings.Split(s, ",") {
				if m = strings.TrimSpace(m); m != "" {
					ch.Models = append(ch.Models, m)
				}
			}
		}
		if len(ch.Models) == 0 {
			ch.Models = []string{"*"}
		}
		v.AddChannel(ch)
		done(map[string]any{"channel_id": ch.ID})
	case "toggle_channel":
		for _, c := range v.Data.Channels {
			if c.ID == arg {
				c.Enabled = !c.Enabled
			}
		}
		done(nil)
	case "del_channel":
		keep := make([]*vault.Channel, 0, len(v.Data.Channels))
		for _, c := range v.Data.Channels {
			if c.ID != arg {
				keep = append(keep, c)
			}
		}
		v.Data.Channels = keep
		done(nil)
	case "add_dkey":
		dk := v.AddDownstreamKey(strV(body["name"]), float64(intF(body["ttl"])))
		done(map[string]any{"new_key": dk.Key})
	case "toggle_dkey":
		for _, k := range v.Data.DownstreamKeys {
			if k.Key == arg {
				k.Enabled = !k.Enabled
			}
		}
		done(nil)
	case "codex_dkey":
		// exclusive selection: which downstream key Codex CLI uses
		off := boolV(body["off"])
		for _, k := range v.Data.DownstreamKeys {
			if off {
				if k.Key == arg {
					k.CodexUse = false
				}
			} else {
				k.CodexUse = k.Key == arg
			}
		}
		done(nil)
	case "set_codex_model":
		m := strings.TrimSpace(arg)
		if strings.ContainsAny(m, " \t\r\n\"'#") {
			fail("invalid model name")
			return
		}
		v.Data.CodexModel = m
		done(nil)
	case "set_smart_model":
		// the model behind the "smart" alias; empty arg clears it
		m := strings.TrimSpace(arg)
		if strings.ContainsAny(m, " \t\r\n\"'#") {
			fail("invalid model name")
			return
		}
		v.Data.SmartModel = m
		done(map[string]any{"smart_model": m})
	case "del_dkey":
		keep := make([]*vault.DownstreamKey, 0, len(v.Data.DownstreamKeys))
		for _, k := range v.Data.DownstreamKeys {
			if k.Key != arg {
				keep = append(keep, k)
			}
		}
		v.Data.DownstreamKeys = keep
		done(nil)
	case "set_strategy":
		switch arg {
		case "failover", "round_robin", "weighted", "least_errors":
			v.Data.Policy.Strategy = arg
			done(map[string]any{"strategy": arg})
		default:
			fail("unknown strategy")
		}
	case "set_port":
		p := intF(body["arg"])
		if p < 1 || p > 65535 {
			fail("port out of range")
			return
		}
		v.Data.Port = p
		// bare port marker (not secret); the vault port hides in ciphertext
		// and cannot be peeked while locked, so the marker drives bind order
		hb.B.SavePortMarker(p)
		done(map[string]any{"note": "port takes effect after gateway restart"})
	case "set_bind_lan":
		on := arg == "on" || arg == "true" || arg == "1"
		v.Data.Policy.BindLan = on
		note := "only this PC can access it (restart to apply)"
		if on {
			note = "LAN devices can access it (restart to apply; allow firewall)"
		}
		done(map[string]any{"note": note, "bind_lan": on})
	case "change_password":
		oldP := strV(body["old_password"])
		newP := strV(body["new_password"])
		if len(newP) < 8 {
			fail("new password must be at least 8 chars")
			return
		}
		if oldP == "" {
			fail("old password required")
			return
		}
		if _, err := vault.Load(hb.B.DataDir(), oldP); err != nil {
			fail("old password is incorrect")
			return
		}
		if err := v.ChangePassword(newP); err != nil {
			fail("save failed: " + err.Error())
			return
		}
		// keep per-device remember in sync (new password => new salt => old blob void)
		if remember.Exists(hb.B.DataDir()) {
			remember.Save(hb.B.DataDir(), vault.Path(hb.B.DataDir()), newP)
		}
		done(map[string]any{"note": "master password changed; remembered devices updated"})
	case "test_channel":
		ch := hb.B.Sched().Get(arg)
		if ch == nil {
			fail("unknown channel " + arg)
			return
		}
		res := channelProbe(ch, hb.B.Policy().TimeoutFirstByteS)
		okJSON(w, map[string]any{
			"ok": res.ok, "status": res.status,
			"first_byte_ms": res.firstMS, "body": firstN(res.body, 400), "error": res.err,
		})
	case "lock":
		hb.B.Lock()
		okJSON(w, map[string]any{"ok": true, "locked": true})
	case "fetch_models":
		// best-effort upstream model discovery: each channel's key asks its own
		// /models; anything that fails simply contributes nothing (no error UI).
		okJSON(w, map[string]any{"ok": true, "models": fetchUpstreamModels(v.Data.Channels)})
	default:
		fail("unknown action " + action)
	}
}

/* ------------------------------------------------------------------ probe */

type probeResult struct {
	ok      bool
	status  int
	firstMS float64
	body    string
	err     string
}

// channelProbe delegates to the shared probe package.
func channelProbe(ch *vault.Channel, timeoutFirst float64) probeResult {
	r := probe.Channel(ch, timeoutFirst)
	return probeResult{
		ok: r.OK, status: r.Status, firstMS: r.FirstMS,
		body: r.Err, err: r.Err,
	}
}

/* -------------------------------------------------- upstream model discovery */

// modelsBase turns a channel base URL into the root the /models endpoint lives
// under, e.g. "https://api.openai.com/v1" -> same, ".../v1/chat/completions" ->
// ".../v1".
func modelsBase(raw string) string {
	base := strings.TrimRight(strings.TrimSpace(raw), "/")
	for _, suf := range []string{"/chat/completions", "/messages", "/embeddings", "/responses"} {
		base = strings.TrimRight(strings.TrimSuffix(base, suf), "/")
	}
	return base
}

// listChannelModels asks one channel which models its key can use. Any failure
// (network, non-2xx, unparsable body) yields nil.
func listChannelModels(ch *vault.Channel) []string {
	base := modelsBase(ch.BaseURL)
	if base == "" || ch.APIKey == "" {
		return nil
	}
	req, err := http.NewRequest("GET", base+"/models", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Accept", "application/json")
	if ch.AuthHeader == "bearer" {
		req.Header.Set("Authorization", "Bearer "+ch.APIKey)
	} else {
		req.Header.Set("x-api-key", ch.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	client := &http.Client{Timeout: 6 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&body); err != nil {
		return nil
	}
	out := make([]string, 0, len(body.Data))
	for _, m := range body.Data {
		if m.ID != "" {
			out = append(out, m.ID)
		}
	}
	return out
}

// fetchUpstreamModels queries every enabled channel in parallel and keeps only
// models that channel is actually allowed to route (wildcards respected).
func fetchUpstreamModels(chs []*vault.Channel) []string {
	var mu sync.Mutex
	var wg sync.WaitGroup
	seen := map[string]bool{}
	for _, ch := range chs {
		if ch == nil || !ch.Enabled || ch.APIKey == "" {
			continue
		}
		wg.Add(1)
		go func(ch *vault.Channel) {
			defer wg.Done()
			for _, m := range listChannelModels(ch) {
				if vault.IsSmartAlias(m) || !matchesModel(ch.Models, m) {
					continue
				}
				mu.Lock()
				seen[m] = true
				mu.Unlock()
			}
		}(ch)
	}
	wg.Wait()
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	sort.Strings(out)
	if len(out) > 200 {
		out = out[:200]
	}
	return out
}
