# -*- coding: utf-8 -*-
"""End-to-end tests for the USB bridge prototype.

Stand-alone runner:  python tests/e2e_test.py
"""

from __future__ import annotations

import http.client
import json
import os
import shutil
import sys
import tempfile
import threading

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, os.path.join(ROOT, "python"))
sys.path.insert(0, ROOT)

from tests.mock_upstream import start_mock  # noqa: E402
from bridgetools import server as srv  # noqa: E402
from bridgetools import vault as V  # noqa: E402

PASS = []
FAIL = []


def check(name: str, cond: bool, detail: str = "") -> None:
    (PASS if cond else FAIL).append(name)
    print(("  ok  " if cond else "  FAIL") + f"  {name}" + (f"  -- {detail}" if detail and not cond else ""))


def http_req(port: int, method: str, path: str, body: dict | None = None, headers: dict | None = None):
    conn = http.client.HTTPConnection("127.0.0.1", port, timeout=15)
    payload = json.dumps(body).encode() if body is not None else None
    hdrs = {"Content-Type": "application/json", **(headers or {})}
    conn.request(method, path, body=payload, headers=hdrs)
    resp = conn.getresponse()
    raw = resp.read()
    status, headers_out = resp.status, dict(resp.getheaders())
    conn.close()
    try:
        return status, headers_out, json.loads(raw.decode())
    except json.JSONDecodeError:
        return status, headers_out, raw


def stream_req(port: int, path: str, body: dict, headers: dict):
    """Return (status, headers, list_of_decoded_data_events)."""
    conn = http.client.HTTPConnection("127.0.0.1", port, timeout=20)
    payload = json.dumps(body).encode()
    conn.request("POST", path, body=payload, headers={"Content-Type": "application/json", **headers})
    resp = conn.getresponse()
    status, hdrs = resp.status, dict(resp.getheaders())
    events = []
    if status != 200:
        raw = resp.read()
        conn.close()
        return status, hdrs, [raw.decode("utf-8", "replace")]
    while True:
        line = resp.readline()
        if not line:
            break
        line = line.strip()
        if not line.startswith(b"data:"):
            continue
        data = line[5:].strip()
        if data == b"[DONE]":
            events.append("[DONE]")
            break
        try:
            events.append(json.loads(data.decode()))
        except json.JSONDecodeError:
            events.append(data.decode())
    conn.close()
    return status, hdrs, events


def main() -> int:
    mock_a = start_mock(18080)  # openai-style
    mock_b = start_mock(18081)  # anthropic-style
    mock_c = start_mock(18082)  # anthropic-style with fixed good key (for proxy test)

    data_dir = os.path.join(tempfile.mkdtemp(prefix="usbbdg_e2e_"), "bridge")
    os.makedirs(data_dir, exist_ok=True)
    seed = {
        "policy": {
            "strategy": "failover",
            "retry_budget": 2,
            "timeout_first_byte_s": 10,
            "timeout_stream_idle_s": 30,
            "circuit": {"err_rate_threshold": 0.5, "min_samples": 8, "cooldown_s": 60},
        },
        "channels": [
            {
                "id": "ch-dead", "protocol": "openai",
                "base_url": "http://127.0.0.1:19999/v1",
                "api_key": "unused", "models": ["gpt-*"], "priority": -1,
            },
            {
                "id": "ch-openai", "protocol": "openai",
                "base_url": "http://127.0.0.1:18080/v1",
                "api_key": "sk-openai-mock",
                "models": ["gpt-*", "text-embedding-*"],
                "priority": 0, "auth_header": "bearer",
            },
            {
                "id": "ch-claude-bad", "protocol": "anthropic",
                "base_url": "http://127.0.0.1:18081",
                "api_key": "sk-ant-WRONG",
                "models": ["claude-error*"], "priority": 0, "auth_header": "x-api-key",
            },
            {
                "id": "ch-claude", "protocol": "anthropic",
                "base_url": "http://127.0.0.1:18081",
                "api_key": "sk-ant-mock",
                "models": ["claude-mock*"], "priority": 0, "auth_header": "x-api-key",
            },
            {
                "id": "ch-proxy", "protocol": "anthropic",
                "base_url": "http://127.0.0.1:18082",
                "api_key": "sk-ant-mock",
                "models": ["proxy-*"], "priority": 99, "auth_header": "x-api-key",
            },
        ],
    }
    password = "test-password-123"
    _vault, dk = V.create(data_dir, password, seed)
    print(f"downstream key: {dk}\n")

    bridge = srv.Bridge(data_dir=data_dir)
    bridge.unlock(password)
    port = 18989
    httpd = srv.serve(bridge, port=port)
    threading.Thread(target=httpd.serve_forever, daemon=True).start()
    AH = {"Authorization": f"Bearer {dk}"}

    # ---- 1. liveness
    st, _, body = http_req(port, "GET", "/healthz")
    check("healthz", st == 200 and body.get("ok") is True)

    # ---- 2. downstream key auth
    st, _, body = http_req(port, "POST", "/v1/chat/completions", {"model": "gpt-*", "messages": []})
    check("401 on wrong key", st == 401, str(body))
    st, _, body = http_req(port, "POST", "/v1/chat/completions",
                           {"model": "gpt-mock", "messages": [{"role": "user", "content": "hi"}]},
                           {"Authorization": "Bearer sk-usb-invalid"})
    check("401 on fake key", st == 401)

    # ---- 3. model list
    st, _, body = http_req(port, "GET", "/v1/models", headers=AH)
    ids = {m["id"] for m in body.get("data", [])}
    check("models capability patterns", st == 200 and "gpt-*" in ids and "claude-mock*" in ids, str(body)[:200])

    # ---- 4. non-stream openai + failover (dead channel first by priority)
    st, hd, body = http_req(
        port, "POST", "/v1/chat/completions",
        {"model": "gpt-mock", "messages": [{"role": "user", "content": "failover-test"}]},
        AH,
    )
    check("non-stream chat 200", st == 200, str(body)[:200])
    check("failover used ch-openai", hd.get("X-BRG-Channel") == "ch-openai", str(hd))
    check("content echo",
          "echo-openai[gpt-mock]" in body["choices"][0]["message"]["content"], str(body)[:200])
    check("usage tokens", body["usage"]["prompt_tokens"] > 0)

    # ---- 5. stream openai (pass-through SSE)
    st, hd, events = stream_req(port, "/v1/chat/completions",
                                {"model": "gpt-mock", "stream": True,
                                 "messages": [{"role": "user", "content": "stream-test"}]}, AH)
    content = "".join(
        e["choices"][0]["delta"].get("content", "")
        for e in events
        if isinstance(e, dict) and e.get("choices")
    )
    check("stream status 200", st == 200)
    check("stream content reassembled",
          "echo-openai[gpt-mock]< stream-test" in content, str(events)[:300])
    check("stream [DONE] present", events and events[-1] == "[DONE]")

    # ---- 6. anthropic transform, non-stream
    st, _, body = http_req(
        port, "POST", "/v1/chat/completions",
        {"model": "claude-mock", "max_tokens": 100,
         "messages": [{"role": "system", "content": "be nice"},
                      {"role": "user", "content": "anthropic-test"}]},
        AH,
    )
    check("anthropic non-stream 200", st == 200, str(body)[:200])
    check("anthropic text mapped",
          "echo-claude[claude-mock]sys:be nice" in body["choices"][0]["message"]["content"],
          str(body)[:200])
    check("anthropic finish_reason stop", body["choices"][0]["finish_reason"] == "stop")

    # ---- 7. anthropic SSE transform
    st, hd, events = stream_req(
        port, "/v1/chat/completions",
        {"model": "claude-mock", "stream": True, "max_tokens": 100,
         "messages": [{"role": "user", "content": "anthropic-stream"}]},
        AH,
    )
    content = "".join(
        e["choices"][0]["delta"].get("content", "")
        for e in events
        if isinstance(e, dict) and e.get("choices")
    )
    finishes = [e["choices"][0]["finish_reason"] for e in events
                if isinstance(e, dict) and e.get("choices")
                and e["choices"][0].get("finish_reason")]
    check("anthropic stream 200", st == 200)
    check("anthropic stream content",
          "echo-claude[claude-mock]< anthropic-stream" in content, str(events)[:300])
    check("anthropic stream finish stop", finishes and finishes[-1] == "stop", str(finishes))
    check("anthropic stream channel", hd.get("X-BRG-Channel") == "ch-claude")

    # ---- 8. upstream 401 mapped through
    st, _, body = http_req(
        port, "POST", "/v1/chat/completions",
        {"model": "claude-error1", "max_tokens": 10, "messages": [{"role": "user", "content": "x"}]},
        AH,
    )
    check("upstream 401 passthrough", st == 401 and "error" in body, str(body))

    # ---- 9. /proxy raw passthrough (body stays anthropic-format, key injected)
    st, _, body = http_req(
        port, "POST", "/proxy/ch-proxy/v1/messages",
        {"model": "claude-mock", "max_tokens": 50,
         "messages": [{"role": "user", "content": "proxy-test"}]},
        AH,
    )
    check("proxy 200", st == 200, str(body)[:200])
    check("proxy raw anthropic format", body.get("type") == "message", str(body)[:200])

    # ---- 10. embeddings passthrough
    st, _, body = http_req(
        port, "POST", "/v1/embeddings",
        {"model": "text-embedding-3-small", "input": "hello"},
        AH,
    )
    check("embeddings 200", st == 200, str(body)[:200])
    check("embeddings vector", body["data"][0]["embedding"] == [0.1, 0.2, 0.3], str(body)[:120])

    # ---- 11. admin auth
    st, _, _ = http_req(port, "GET", "/admin/summary", headers=None)
    check("admin rejected without token", st == 401)
    st, _, summary = http_req(port, "GET", "/admin/summary",
                              headers={"X-Admin-Token": bridge.admin_token})
    check("admin summary", st == 200 and "strategy" in summary, str(summary))

    # ---- 12. usage audit rows
    rows = bridge.usage.list_recent()
    check("usage rows recorded", any(r["channel_id"] in ("ch-openai", "ch-claude") for r in rows))
    summary = bridge.usage.summary()
    check("usage tokens aggregated", summary["requests_24h"] >= 5, str(summary))

    # ---- 13. admin command: add channel then use it
    st, _, r = http_req(port, "POST", "/admin/cmd",
                        {"action": "add_channel", "proto": "openai",
                         "base_url": "http://127.0.0.1:18080/v1",
                         "api_key": "sk-openai-mock", "models": "adm-test-*"},
                        {"X-Admin-Token": bridge.admin_token})
    check("admin add channel", st == 200 and r.get("ok"), str(r))
    st, _, body = http_req(
        port, "POST", "/v1/chat/completions",
        {"model": "adm-test-x", "messages": [{"role": "user", "content": "via-admin"}]},
        AH,
    )
    check("new channel serves model", st == 200, str(body)[:200])

    # ---- 14. circuit breaker: hammer a failing-only model -> 401s, then open -> 503
    for _ in range(10):
        st, _, body = http_req(
            port, "POST", "/v1/chat/completions",
            {"model": "claude-error2", "max_tokens": 10,
             "messages": [{"role": "user", "content": "x"}]},
            AH,
        )
        st_high = st
    # breaker should have opened after >= min_samples(8) consecutive failures
    st, _, body = http_req(
        port, "POST", "/v1/chat/completions",
        {"model": "claude-error2", "max_tokens": 10,
         "messages": [{"role": "user", "content": "x"}]},
        AH,
    )
    check("breaker opens after failures",
          st == 503 and "no healthy channel" in str(body), f"last->{st} {body}")

    # ---- 15. unknown route
    st, _, _ = http_req(port, "GET", "/v1/nope", headers=AH)
    check("unknown route 404", st == 404)

    httpd.shutdown()
    bridge.usage.close()
    shutil.rmtree(os.path.dirname(data_dir), ignore_errors=True)
    print()
    print(f"PASSED {len(PASS)}  FAILED {len(FAIL)}")
    if FAIL:
        print("failed checks:", ", ".join(FAIL))
    return 1 if FAIL else 0


if __name__ == "__main__":
    sys.exit(main())
