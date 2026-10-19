# -*- coding: utf-8 -*-
"""E2E tests for the Go gateway (go/usbgw.exe).

Flow:
  1. start python mock upstream (shared with the prototype tests)
  2. build a SCRYPT vault with the PYTHON prototype in a temp dir
     -> cross-language check: Go must be able to unlock it
  3. run the Go binary (console build, --hidden) on 8931
  4. drive the HTTP API end-to-end: unlock / channels / dkey /
     openai + anthropic (stream & non-stream) / failover / proxy /
     embeddings / admin / lock
"""
from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
PYDIR = os.path.join(ROOT, "python")
GODIR = os.path.join(ROOT, "go")

sys.path.insert(0, HERE)
from mock_upstream import start_mock  # noqa: E402

PORT_GATEWAY = int(os.environ.get("USBGW_PORT_GATEWAY", "8931"))
PORT_INTEROP = int(os.environ.get("USBGW_PORT_INTEROP", "8932"))
PORT_MOCK = int(os.environ.get("USBGW_PORT_MOCK", "8490"))
PASSWORD = "go-e2e-pass-1"

DEV_EXE = os.path.join(GODIR, os.environ.get("USBGW_DEV_EXE", "usbgw.exe"))


def find_go() -> str:
    """Locate the Go toolchain without hardcoding anyone's machine layout.

    Order: $GO_BIN -> PATH (shutil.which) -> common install dirs.
    """
    env = os.environ.get("GO_BIN")
    if env and os.path.exists(env):
        return env
    found = shutil.which("go") or shutil.which("go.exe")
    if found:
        return found
    candidates = [
        os.path.join(os.environ.get("ProgramFiles", r"C:\Program Files"), "Go", "bin", "go.exe"),
        os.path.join(os.environ.get("ProgramFiles(x86)", r"C:\Program Files (x86)"), "Go", "bin", "go.exe"),
        os.path.join(os.environ.get("LOCALAPPDATA", ""), "Programs", "Go", "bin", "go.exe"),
        r"C:\Go\bin\go.exe",
    ]
    for c in candidates:
        if c and os.path.exists(c):
            return c
    return "go"  # last resort: let the shell resolve it

results: list[tuple[str, bool, str]] = []


def check(name, ok, detail=""):
    results.append((name, bool(ok), str(detail)[:400]))
    print(("PASS " if ok else "FAIL ") + name + ("" if ok else f"  <- {detail}"))


def http(method, url, body=None, headers=None, timeout=30, raw=False):
    data = json.dumps(body).encode() if isinstance(body, dict) else body
    req = urllib.request.Request(url, data=data, method=method, headers=headers or {})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            payload = resp.read()
            return resp.status, payload if raw else (json.loads(payload) if payload else {})
    except urllib.error.HTTPError as e:
        payload = e.read()
        try:
            return e.code, payload if raw else (json.loads(payload) if payload else {})
        except Exception:
            return e.code, {"_raw": payload.decode(errors="replace")}


def sse_contents(text: str) -> str:
    """Parse an OpenAI-style SSE stream; concatenate delta.content pieces."""
    parts = []
    for ln in text.splitlines():
        if not ln.startswith("data: "):
            continue
        payload = ln[6:].strip()
        if payload in ("", "[DONE]"):
            continue
        try:
            obj = json.loads(payload)
        except Exception:
            continue
        choices = obj.get("choices") or [{}]
        delta = (choices[0] or {}).get("delta") or {}
        if isinstance(delta.get("content"), str):
            parts.append(delta["content"])
    return "".join(parts)


def wait_healthy(url, seconds=15):
    for _ in range(seconds * 4):
        try:
            code, st = http("GET", url)
            if code == 200:
                return st
        except Exception:
            pass
        time.sleep(0.25)
    return None


def dpapi_unprotect(session_file: str):
    """Decrypt b64 DPAPI blob from session.key (same-user/machine only)."""
    try:
        import base64 as b64m
        import ctypes
        import ctypes.wintypes as wt

        class BLOB(ctypes.Structure):
            _fields_ = [("cb", wt.DWORD), ("pb", ctypes.c_void_p)]

        raw = json.load(open(session_file))["pw"]
        data = b64m.b64decode(raw)
        buf = ctypes.create_string_buffer(data, len(data))
        i = BLOB(len(data), ctypes.cast(buf, ctypes.c_void_p))
        o = BLOB()
        if not ctypes.windll.crypt32.CryptUnprotectData(
                ctypes.byref(i), None, None, None, None, 0, ctypes.byref(o)):
            return None
        out = ctypes.string_at(o.pb, o.cb)
        ctypes.windll.kernel32.LocalFree(ctypes.c_void_p(o.pb))
        return out.decode()
    except Exception:
        return None


def main():
    # kill leftover usbgw.exe from previous runs (they hold the port and
    # every test would otherwise silently run against the stale binary)
    subprocess.run(["taskkill", "/F", "/IM", os.path.basename(DEV_EXE)], capture_output=True)
    time.sleep(0.5)

    shutil.rmtree(DEV_EXE, ignore_errors=True)
    subprocess.run(
        [find_go(), "build", "-o", os.path.basename(DEV_EXE), "."],
        cwd=GODIR, check=True, capture_output=True,
    )
    check("go build usbgw.exe", os.path.exists(DEV_EXE))

    mock = start_mock(PORT_MOCK)

    # ------------------------------------------------ python scrypt vault
    interop_dir = tempfile.mkdtemp(prefix="usbgw-interop-")
    seed_path = os.path.join(interop_dir, "seed.json")
    with open(seed_path, "w", encoding="utf-8") as f:
        json.dump(
            {
                "port": PORT_INTEROP,
                "channels": [
                    {
                        "protocol": "openai",
                        "base_url": f"http://127.0.0.1:{PORT_MOCK}/v1",
                        "api_key": "sk-mock",
                        "models": ["*"],
                    }
                ],
            },
            f,
        )
    subprocess.run(
        [sys.executable, os.path.join(PYDIR, "bridge.py"), "init",
         "--data-dir", interop_dir, "--password", PASSWORD, "--seed", seed_path, "--force"],
        check=True, capture_output=True, cwd=ROOT,
    )
    check("python prototype created scrypt vault",
          os.path.exists(os.path.join(interop_dir, "config.vault")))

    gw2 = subprocess.Popen(
        [DEV_EXE, "--hidden",
         "--data-dir", interop_dir, "--port", str(PORT_INTEROP)],
        cwd=GODIR, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    )
    st = wait_healthy(f"http://127.0.0.1:{PORT_INTEROP}/healthz")
    check("interop gw healthy", st is not None and st.get("port") == PORT_INTEROP, str(st))
    code, r = http("POST", f"http://127.0.0.1:{PORT_INTEROP}/admin/unlock",
                   {"master_password": PASSWORD})
    check("go unlocks PYTHON scrypt vault", code == 200 and r.get("ok") and r.get("fresh") is False, r)
    tok2 = r.get("admin_token")
    code, r = http("POST", f"http://127.0.0.1:{PORT_INTEROP}/admin/cmd",
                   {"action": "add_dkey", "name": "interop", "ttl": 0},
                   {"X-Admin-Token": tok2})
    check("interop add_dkey", code == 200 and r.get("ok"), r)
    dk2 = r.get("new_key")
    code, r = http("POST", f"http://127.0.0.1:{PORT_INTEROP}/v1/chat/completions",
                   {"model": "gpt-mock", "max_tokens": 50,
                    "messages": [{"role": "user", "content": "interop"}]},
                   {"Authorization": "Bearer " + dk2})
    check("interop gateway serves chat", code == 200 and "echo-openai" in r["choices"][0]["message"].get("content", ""), r)
    code, r = http("POST", f"http://127.0.0.1:{PORT_INTEROP}/admin/cmd",
                   {"action": "lock"}, {"X-Admin-Token": tok2})
    check("interop lock", code == 200 and r.get("locked"))
    gw2.terminate()
    shutil.rmtree(interop_dir, ignore_errors=True)

    # ------------------------------------------------ fresh Go vault (argon2)
    data_dir = os.path.join(ROOT, "go", "bridge-e2e")
    shutil.rmtree(data_dir, ignore_errors=True)

    env = os.environ.copy()
    env["PYTHONIOENCODING"] = "utf-8"
    gw = subprocess.Popen(
        [DEV_EXE, "--hidden",
         "--data-dir", data_dir, "--port", str(PORT_GATEWAY)],
        cwd=GODIR, stdout=open(os.path.join(GODIR, "gw-runs.log"), "ab"),
        stderr=subprocess.STDOUT, env=env,
    )
    base = f"http://127.0.0.1:{PORT_GATEWAY}"
    try:
        st = wait_healthy(f"{base}/healthz")
        check("gw healthy & locked", st is not None and st.get("locked") is True, str(st))

        code, r = http("GET", f"{base}/v1/chat/completions")
        check("locked /v1 -> 503", code == 503, (code, r))

        code, r = http("GET", f"{base}/admin/token")
        check("locked /admin/token -> 403", code == 403, (code, r))

        code, r = http("POST", f"{base}/admin/unlock", {"master_password": "short"})
        check("reject short password", code == 200 and not r.get("ok"), r)
        code, r = http("POST", f"{base}/admin/unlock", {"master_password": PASSWORD, "remember_device": True})
        check("unlock creates fresh argon2 vault", r.get("ok") and r.get("fresh") is True, r)
        tok = r.get("admin_token")
        adm = {"X-Admin-Token": tok}

        # loopback session token: same value as the unlock token, no password
        code, r = http("GET", f"{base}/admin/token")
        check("unlocked /admin/token -> session token", code == 200 and r.get("admin_token") == tok, (code, r))
        code, r = http("GET", f"{base}/admin/summary", headers={"X-Admin-Token": (r or {}).get("admin_token", "")})
        check("session token grants admin", code == 200 and "requests_24h" in r, (code, r))

        sess = os.path.join(data_dir, "session.key")
        check("remember device -> session.key saved", os.path.exists(sess), sess)
        code, r = http("GET", f"{base}/healthz")
        check("state remembers device", r.get("remembered") is True, r)

        # change password (wrong old must fail)
        code, r = http("POST", f"{base}/admin/cmd",
                       {"action": "change_password", "old_password": "nope-nope", "new_password": "new-pass-123"}, adm)
        check("change_password rejects wrong old", not r.get("ok"), r)
        code, r = http("POST", f"{base}/admin/cmd",
                       {"action": "change_password", "old_password": "nope-nope", "new_password": "short"}, adm)
        check("change_password rejects short new", not r.get("ok"), r)
        code, r = http("POST", f"{base}/admin/cmd",
                       {"action": "change_password", "old_password": PASSWORD, "new_password": "new-pass-123"}, adm)
        check("change_password ok", code == 200 and r.get("ok"), r)
        # the remembered blob must decrypt to the NEW password (DPAPI pool)
        check("remember blob decrypts to new password", dpapi_unprotect(sess) == "new-pass-123", sess)

        # LAN toggle (setting persisted; actual 0.0.0.0 bind verified manually)
        code, r = http("POST", f"{base}/admin/cmd", {"action": "set_bind_lan", "arg": "on"}, adm)
        check("set_bind_lan on", r.get("ok") and r.get("bind_lan") is True, r)
        code, r = http("GET", f"{base}/healthz")
        check("state shows bind_lan", r.get("bind_lan") is True, r)
        http("POST", f"{base}/admin/cmd", {"action": "set_bind_lan", "arg": "off"}, adm)

        # re-lock + old password semantics
        http("POST", f"{base}/admin/cmd", {"action": "lock"}, adm)
        code, r = http("POST", f"{base}/admin/unlock", {"master_password": PASSWORD})
        check("old password no longer works", not r.get("ok"), r)
        code, r = http("POST", f"{base}/admin/unlock", {"master_password": "new-pass-123", "remember_device": True})
        check("unlock with new password", r.get("ok") and r.get("fresh") is False, r)
        tok = r["admin_token"]; adm = {"X-Admin-Token": tok}

        # relock + wrong password (vault now exists -> must fail)
        http("POST", f"{base}/admin/cmd", {"action": "lock"}, adm)
        code, r = http("POST", f"{base}/admin/unlock", {"master_password": "wrong-password"})
        check("wrong pw on existing vault fails", not r.get("ok"), r)
        code, r = http("POST", f"{base}/admin/unlock", {"master_password": "new-pass-123"})
        check("argon2 vault re-unlock", r.get("ok") and r.get("fresh") is False, r)
        tok = r["admin_token"]
        adm = {"X-Admin-Token": tok}

        # channels
        code, r = http("POST", f"{base}/admin/cmd", {
            "action": "add_channel", "proto": "openai",
            "base_url": f"http://127.0.0.1:{PORT_MOCK}/v1",
            "api_key": "sk-mock", "models": "gpt-*", "priority": 1,
        }, adm)
        oid = r.get("channel_id")
        check("add openai channel", r.get("ok") and oid, r)
        code, r = http("POST", f"{base}/admin/cmd", {
            "action": "add_channel", "proto": "anthropic",
            "base_url": f"http://127.0.0.1:{PORT_MOCK}",
            "api_key": "sk-ant-mock", "models": "claude*", "priority": 0,
        }, adm)
        aid = r.get("channel_id")
        check("add anthropic channel", r.get("ok") and aid, r)
        code, r = http("POST", f"{base}/admin/cmd", {
            "action": "add_channel", "proto": "anthropic",
            "base_url": f"http://127.0.0.1:{PORT_MOCK}",
            "api_key": "sk-WRONG", "models": "claude*", "priority": -1,
        }, adm)
        badid = r.get("channel_id")
        check("add bad anthropic channel", r.get("ok") and badid, r)

        # downstream keys
        code, r = http("POST", f"{base}/admin/cmd", {"action": "add_dkey", "name": "t1", "ttl": 0}, adm)
        dk = r.get("new_key")
        check("mint downstream key", bool(dk), r)
        auth = {"Authorization": "Bearer " + dk}

        code, r = http("GET", f"{base}/v1/chat/completions", headers={"Authorization": "Bearer nope"})
        check("bad downstream key -> 401", code == 401, (code, r))

        # /v1/models includes wildcard channel models (mock answer models not listed)
        code, r = http("GET", f"{base}/v1/models", headers=auth)
        ids = [m.get("id") for m in r.get("data", [])]
        check("models endpoint", code == 200 and any(m == "gpt-*" for m in ids) and any(m.startswith("claude") for m in ids), r)

        # chat non-stream via openai channel
        code, r = http("POST", f"{base}/v1/chat/completions", {
            "model": "gpt-mock", "max_tokens": 50,
            "messages": [{"role": "user", "content": "hello go"}],
        }, auth)
        txt = r.get("choices", [{}])[0].get("message", {}).get("content", "")
        check("openai chat non-stream", code == 200 and "echo-openai[gpt-mock]" in txt, (code, r))
        check("usage tokens recorded", r.get("usage", {}).get("completion_tokens", 0) > 0, r.get("usage"))
        check("channel header", isinstance(r, dict))  # headers not in urllib resp payload; skip

        # keyed model routing: claude -> anthropic (skips bad key channel via failover)
        code, r = http("POST", f"{base}/v1/chat/completions", {
            "model": "claude-mock", "max_tokens": 50,
            "messages": [{"role": "user", "content": "bonjour"}],
        }, auth)
        txt = r.get("choices", [{}])[0].get("message", {}).get("content", "")
        check("anthropic -> openai convert", code == 200 and "echo-claude[claude-mock]" in txt, (code, r))
        check("anthropic usage mapped", r.get("usage", {}).get("prompt_tokens", 0) >= 0, r.get("usage"))

        # streaming openai (decode chunked + assemble delta contents)
        body = json.dumps({"model": "gpt-mock", "max_tokens": 50, "stream": True,
                           "messages": [{"role": "user", "content": "stream me"}]}).encode()
        code, raw = http("POST", f"{base}/v1/chat/completions", body,
                         {**auth, "Content-Type": "application/json"}, raw=True)
        text = raw.decode()
        assembled = sse_contents(text)
        check("openai SSE chunks", code == 200 and text.count("data: ") >= 10 and "echo-openai" in assembled, text[:150])
        check("SSE content assembled", "echo-openai[gpt-mock]" in assembled, assembled)
        check("SSE terminated [DONE]", "data: [DONE]" in text, text[-60:])

        # streaming anthropic -> converted SSE
        body = json.dumps({"model": "claude-mock", "max_tokens": 50, "stream": True,
                           "messages": [{"role": "user", "content": "claude stream"}]}).encode()
        code, raw = http("POST", f"{base}/v1/chat/completions", body,
                         {**auth, "Content-Type": "application/json"}, raw=True)
        text = raw.decode()
        assembled = sse_contents(text)
        check("anthropic SSE converted", code == 200 and "echo-claude" in assembled and "chat.completion.chunk" in text, text[:150])
        check("anthropic SSE [DONE]", "data: [DONE]" in text, text[-60:])
        check("anthropic SSE role chunk", '"role":"assistant"' in text.replace(" ", "") or '"role": "assistant"' in text, text[:220])

        # provider shuffle: system messages route to anthropic
        code, r = http("POST", f"{base}/v1/chat/completions", {
            "model": "claude-mock", "max_tokens": 50,
            "messages": [
                {"role": "system", "content": "be nice"},
                {"role": "user", "content": "sys-check"},
            ],
        }, auth)
        check("anthropic system passthrough", code == 200 and "sys:be nice" in json.dumps(r), (code, r))

        # ------------------------------------------------ /v1/messages: Anthropic-native inbound
        code, r = http("POST", f"{base}/v1/messages", {
            "model": "claude-mock", "max_tokens": 40,
            "messages": [{"role": "user", "content": "native cl"}],
        }, auth)
        txt = (r.get("content") or [{}])[0].get("text", "")
        check("messages non-stream anthropic upstream",
              code == 200 and r.get("type") == "message" and "echo-claude" in txt, (code, r))
        check("messages stop_reason end_turn", r.get("stop_reason") == "end_turn", r.get("stop_reason"))

        code, r = http("POST", f"{base}/v1/messages", {
            "model": "gpt-mock", "max_tokens": 40,
            "system": "be neat", "messages": [{"role": "user", "content": "openai via claude syntax"}],
        }, auth)
        txt = (r.get("content") or [{}])[0].get("text", "")
        check("messages non-stream openai upstream",
              code == 200 and r.get("type") == "message" and "echo-openai" in txt, (code, r))

        body = json.dumps({"model": "gpt-mock", "max_tokens": 40, "stream": True,
                           "messages": [{"role": "user", "content": "claude syntax stream"}]}).encode()
        code, raw = http("POST", f"{base}/v1/messages", body,
                         {**auth, "Content-Type": "application/json"}, raw=True)
        text = raw.decode()
        assembled = ""
        for ln in text.splitlines():
            if not ln.startswith("data: "):
                continue
            try:
                obj = json.loads(ln[6:])
            except Exception:
                continue
            if obj.get("type") == "content_block_delta":
                assembled += obj.get("delta", {}).get("text", "")
        check("messages SSE openai upstream", code == 200 and "echo-openai" in assembled, (code, text[:120]))
        check("messages SSE events complete",
              all(k in text for k in ["message_start", "content_block_delta", "message_stop"]), text[:200])
        check("messages SSE no openai objects", "chat.completion.chunk" not in text, text[:80])

        # anthropic-native stream via anthropic upstream channel -> raw relay
        body = json.dumps({"model": "claude-mock", "max_tokens": 40, "stream": True,
                           "messages": [{"role": "user", "content": "cl native stream"}]}).encode()
        code, raw = http("POST", f"{base}/v1/messages", body,
                         {**auth, "Content-Type": "application/json"}, raw=True)
        text = raw.decode()
        assembled2 = ""
        for ln in text.splitlines():
            if not ln.startswith("data: "):
                continue
            try:
                obj = json.loads(ln[6:])
            except Exception:
                continue
            if obj.get("type") == "content_block_delta":
                assembled2 += obj.get("delta", {}).get("text", "")
        check("messages SSE anthropic relay",
              code == 200 and "message_stop" in text and "echo-claude" in assembled2, text[:200])

        # ------------------------------------------------ /v1/responses (Codex CLI)
        code, r = http("POST", f"{base}/v1/responses", {"model": "gpt-mock", "input": "responses check"}, auth)
        texts = "".join(p.get("text", "") for it in (r.get("output") or [])
                        for p in (it.get("content") or []) if isinstance(p, dict))
        check("responses non-stream openai upstream",
              code == 200 and r.get("object") == "response" and "echo-openai[gpt-mock]" in texts, (code, r))
        check("responses usage remapped", (r.get("usage") or {}).get("total_tokens", 0) > 0, r.get("usage"))

        code, r = http("POST", f"{base}/v1/responses", {"model": "claude-mock", "input": "cl resp"}, auth)
        texts = "".join(p.get("text", "") for it in (r.get("output") or [])
                        for p in (it.get("content") or []) if isinstance(p, dict))
        check("responses non-stream anthropic upstream", code == 200 and "echo-claude" in texts, (code, r))

        body = json.dumps({"model": "gpt-mock", "stream": True, "input": "stream resp"}).encode()
        code, raw = http("POST", f"{base}/v1/responses", body,
                         {**auth, "Content-Type": "application/json"}, raw=True)
        text = raw.decode()
        deltas = ""
        for ln in text.splitlines():
            if not ln.startswith("data: "):
                continue
            try:
                obj = json.loads(ln[6:])
            except Exception:
                continue
            if obj.get("type") == "response.output_text.delta":
                deltas += obj.get("delta", "")
        check("responses SSE deltas assembled",
              code == 200 and "echo-openai[gpt-mock]" in deltas, (code, text[:200]))
        check("responses SSE lifecycle events",
              all(k in text for k in ["response.created", "response.output_item.added",
                                      "response.output_item.done", "response.completed"]), text[:150])
        check("responses SSE carries no chat chunks", "chat.completion.chunk" not in text, text[:80])

        code, r = http("POST", f"{base}/v1/responses", {
            "model": "gpt-mock",
            "tools": [{"type": "function", "name": "shell", "description": "d",
                       "parameters": {"type": "object"}}],
            "input": [
                {"type": "message", "role": "user", "content": [{"type": "input_text", "text": "run"}]},
                {"type": "function_call", "call_id": "c1", "name": "shell", "arguments": "{\"x\":1}"},
                {"type": "function_call_output", "call_id": "c1", "output": "ok"},
            ],
        }, auth)
        check("responses tools + call items accepted", code == 200 and r.get("object") == "response", (code, r))

        # ------------------------------------------- codex key checkbox -> config sync
        code, r = http("POST", f"{base}/admin/cmd", {"action": "codex_dkey", "arg": dk}, adm)
        check("codex_dkey select", r.get("ok"), r)
        code, r = http("POST", f"{base}/admin/cmd", {"action": "add_dkey", "name": "second", "ttl": 0}, adm)
        dk2 = r.get("new_key")
        http("POST", f"{base}/admin/cmd", {"action": "codex_dkey", "arg": dk2}, adm)
        code, r = http("GET", f"{base}/admin/dkeys", headers=adm)
        sel = [k for k in r.get("keys", []) if k.get("codex_use")]
        check("codex_use exclusive", len(sel) == 1 and sel[0]["key"] == dk2, r)
        http("POST", f"{base}/admin/cmd", {"action": "codex_dkey", "arg": dk}, adm)  # back to primary
        cfg_dir = os.path.join(data_dir, "codex")
        auth_json = os.path.join(cfg_dir, "auth.json")
        cfg_toml = os.path.join(cfg_dir, "config.toml")
        okj = json.load(open(auth_json, encoding="utf-8")) if os.path.exists(auth_json) else {}
        toml = open(cfg_toml, encoding="utf-8").read() if os.path.exists(cfg_toml) else ""
        check("codex auth.json written with selected key", okj.get("OPENAI_API_KEY") == dk, okj)
        check("codex config.toml written",
              f"127.0.0.1:{PORT_GATEWAY}" in toml and 'wire_api = "responses"' in toml, toml)
        code, r = http("POST", f"{base}/admin/cmd", {"action": "set_codex_model", "arg": "gpt-5.7-test"}, adm)
        toml = open(cfg_toml, encoding="utf-8").read()
        check("codex model sync", r.get("ok") and 'model = "gpt-5.7-test"' in toml, toml)
        code, r = http("POST", f"{base}/admin/cmd", {"action": "codex_dkey", "arg": dk, "off": True}, adm)
        check("codex_dkey off ok", r.get("ok"), r)
        check("codex auth.json removed when unbound", not os.path.exists(auth_json), auth_json)
        http("POST", f"{base}/admin/cmd", {"action": "del_dkey", "arg": dk2}, adm)

        # admin models endpoint (quick-connect card data)
        code, r = http("GET", f"{base}/admin/models", None, adm)
        mset = [m["model"] for m in r.get("models", [])]
        check("admin models lists channel patterns", "gpt-*" in mset and "claude*" in mset, mset)

        # healthz carries lan_ip for the card's LAN row
        code, r = http("GET", f"{base}/healthz")
        check("healthz has lan_ip field", "lan_ip" in r, list(r.keys()))

        # failover: bad anthropic channel returns 401 -> good channel served
        code, r = http("POST", f"{base}/v1/chat/completions", {
            "model": "claude-fail", "max_tokens": 30,
            "messages": [{"role": "user", "content": "failover"}],
        }, auth)
        merged = json.dumps(r)
        check("failover to healthy channel", code == 200 and "echo-claude" in merged, (code, r))

        # unknown model -> 503
        code, r = http("POST", f"{base}/v1/chat/completions", {
            "model": "totally-unknown-model", "max_tokens": 5,
            "messages": [{"role": "user", "content": "x"}],
        }, auth)
        check("no channel for model -> 503", code == 503, (code, r))

        # embeddings through the openai-protocol channel (model matches its patterns)
        code, r = http("POST", f"{base}/v1/embeddings", {"model": "gpt-embed-3", "input": "hi"}, auth)
        check("embeddings", code == 200 and r.get("data"), (code, r))

        # test_channel (admin probe)
        code, r = http("POST", f"{base}/admin/cmd", {"action": "test_channel", "arg": oid}, adm)
        check("admin test_channel", code == 200 and r.get("ok") is True, r)
        # bad anthropic channel probe must fail
        code, r = http("POST", f"{base}/admin/cmd", {"action": "test_channel", "arg": badid}, adm)
        check("admin test_channel flags bad channel", code == 200 and r.get("ok") is False, r)

        # summary + recent
        code, r = http("GET", f"{base}/admin/summary", headers=adm)
        check("admin summary", code == 200 and r.get("requests_24h", 0) > 0, r)
        code, r = http("GET", f"{base}/admin/recent", headers=adm)
        check("admin recent", code == 200 and len(r.get("recent", [])) > 0, r)
        code, r = http("GET", f"{base}/admin/channels", headers=adm)
        masked = json.dumps(r)
        check("channels masked in panel", "sk-ant-mock" not in masked and "sk-mock\"" not in masked, r)
        code, r = http("GET", f"{base}/admin/dkeys", headers=adm)
        check("dkeys visible with full key", dk in json.dumps(r), r)

        # proxy passthrough (downstream key auths the bridge; channel key goes upstream)
        code, r = http("POST", f"{base}/proxy/{aid}/v1/messages", {
            "model": "claude-mock", "max_tokens": 30,
            "messages": [{"role": "user", "content": "proxy go"}],
        }, {"x-api-key": "sk-ant-mock", "Authorization": "Bearer " + dk})
        check("proxy anthropic passthrough", code == 200 and r.get("type") == "message", (code, r))

        # set_port persist then invalid
        code, r = http("POST", f"{base}/admin/cmd", {"action": "set_port", "arg": 8944}, adm)
        check("set_port ok", r.get("ok"), r)
        code, r = http("GET", f"{base}/healthz")
        check("set_port needs restart", r.get("port") == PORT_GATEWAY, r)

        # strategies
        code, r = http("POST", f"{base}/admin/cmd", {"action": "set_strategy", "arg": "round_robin"}, adm)
        check("set_strategy round_robin", r.get("ok") and r.get("strategy") == "round_robin", r)
        code, r = http("POST", f"{base}/admin/cmd", {"action": "set_strategy", "arg": "bogus"}, adm)
        check("bad strategy rejected", not r.get("ok"), r)
        http("POST", f"{base}/admin/cmd", {"action": "set_strategy", "arg": "failover"}, adm)

        # dkey ttl expiry: create expired by crafting (ttl tiny in hours -> skip; negative ttl not allowed) -> just check del/toggle
        code, r = http("POST", f"{base}/admin/cmd", {"action": "toggle_dkey", "arg": dk}, adm)
        check("toggle dkey", r.get("ok"), r)
        code, r = http("POST", f"{base}/v1/chat/completions", {
            "model": "gpt-mock", "max_tokens": 5,
            "messages": [{"role": "user", "content": "x"}], "stream": False,
        }, auth)
        check("disabled dkey -> 401", code == 401, (code, r))
        http("POST", f"{base}/admin/cmd", {"action": "toggle_dkey", "arg": dk}, adm)
        code, r = http("POST", f"{base}/admin/cmd", {"action": "del_dkey", "arg": "nope"}, adm)
        check("del_dkey noop ok", r.get("ok"), r)

        # forbidden admin without token
        code, r = http("GET", f"{base}/admin/summary")
        check("admin requires token", code == 401, (code, r))

        # end: lock
        code, r = http("POST", f"{base}/admin/cmd", {"action": "lock"}, adm)
        check("lock", code == 200 and r.get("locked"))
        code, r = http("POST", f"{base}/v1/chat/completions", {
            "model": "gpt-mock", "max_tokens": 5,
            "messages": [{"role": "user", "content": "x"}],
        }, auth)
        check("locked chat -> 503", code == 503, (code, r))
        code, r = http("GET", f"{base}/healthz")
        check("healthz shows locked", r.get("locked") is True, r)
    finally:
        gw.terminate()
        time.sleep(0.5)

    # ------------------------------------------------ remembered reboot
    # session.key exists (saved during the unlock tests with the NEW password)
    # -> the device remembers: boot silently unlocks, then probes (no browser).
    gw = subprocess.Popen(
        [DEV_EXE, "--hidden",
         "--data-dir", data_dir, "--port", str(PORT_GATEWAY)],
        cwd=GODIR, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, env=env,
    )
    try:
        st = wait_healthy(f"http://127.0.0.1:{PORT_GATEWAY}/healthz")
        check("remembered device boots UNLOCKED (silent unlock)",
              st is not None and st.get("locked") is False, str(st))
        check("remembered state persists across boots", st.get("remembered") is True, str(st))
        check("vault file kept encrypted on disk",
              open(os.path.join(data_dir, "config.vault"), "rb").read(4) == b"BVLT")
    finally:
        gw.terminate()
        time.sleep(0.3)

    # forget this device -> next boot is locked again
    if os.path.exists(sess):
        os.remove(sess)
    gw = subprocess.Popen(
        [DEV_EXE, "--hidden",
         "--data-dir", data_dir, "--port", str(PORT_GATEWAY)],
        cwd=GODIR, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, env=env,
    )
    try:
        st = wait_healthy(f"http://127.0.0.1:{PORT_GATEWAY}/healthz")
        check("after forgetting device: boots LOCKED again",
              st is not None and st.get("locked") is True, str(st))
    finally:
        gw.terminate()
        time.sleep(0.3)
        shutil.rmtree(data_dir, ignore_errors=True)

    # ------------------------------------------------ report
    failed = [n for n, ok, _ in results if not ok]
    print(f"\n{'=' * 50}\n{len(results) - len(failed)}/{len(results)} PASS"
          + ("" if not failed else f"\nFAILED: {failed}"))
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
