# -*- coding: utf-8 -*-
"""Mock upstream for e2e tests: openid-style OpenAI + Anthropic endpoints."""

from __future__ import annotations

import json
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

STATE: dict[str, int] = {"fail_openai_once": 0, "calls": 0}


def serve_openai_chat(handler):
    STATE["calls"] += 1
    length = int(handler.headers.get("Content-Length") or 0)
    body = json.loads(handler.rfile.read(length).decode()) if length else {}
    fail_once = handler.headers.get("X-Mock", "") == "fail-openai-once" and STATE["fail_openai_once"] == 0
    if fail_once:
        STATE["fail_openai_once"] = 1
    if fail_once or handler.headers.get("X-Mock", "") == "fail-always":
        payload = json.dumps({"error": {"message": "mock upstream explode", "type": "server_error"}}).encode()
        handler.send_response(500)
        handler.send_header("Content-Type", "application/json")
        handler.send_header("Content-Length", str(len(payload)))
        handler.end_headers()
        handler.wfile.write(payload)
        return
    STATE["fail_openai_once"] = 0
    model = body.get("model", "gpt-mock")
    prompt_text = "".join(
        (m.get("content") if isinstance(m.get("content"), str) else "")
        for m in body.get("messages", [])
    )
    answer = f"echo-openai[{model}]< {prompt_text[:40]} >"
    pt, ct = len(prompt_text), len(answer)
    if body.get("stream"):
        handler.send_response(200)
        handler.send_header("Content-Type", "text/event-stream; charset=utf-8")
        handler.send_header("Transfer-Encoding", "chunked")
        handler.end_headers()

        def chunk(ob):
            data = b"data: " + json.dumps(ob, ensure_ascii=False).encode() + b"\n\n"
            handler.wfile.write(f"{len(data):x}\r\n".encode() + data + b"\r\n")
            handler.wfile.flush()

        idx = 0
        i = 0
        while i < len(answer):
            seg = answer[i : i + 3]
            chunk(
                {
                    "id": "chatcmpl-mock",
                    "object": "chat.completion.chunk",
                    "created": int(time.time()),
                    "model": model,
                    "choices": [{"index": 0, "delta": {"content": seg}, "finish_reason": None}],
                }
            )
            i += 3
            idx += 1
        chunk(
            {
                "id": "chatcmpl-mock",
                "object": "chat.completion.chunk",
                "created": int(time.time()),
                "model": model,
                "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
            }
        )
        chunk({"id": "chatcmpl-mock", "object": "chat.completion.chunk", "choices": []})
        data = b"data: [DONE]\n\n"
        handler.wfile.write(f"{len(data):x}\r\n".encode() + data + b"\r\n")
        handler.wfile.write(b"0\r\n\r\n")
        return
    payload = json.dumps(
        {
            "id": "chatcmpl-mock",
            "object": "chat.completion",
            "created": int(time.time()),
            "model": model,
            "choices": [{"index": 0, "message": {"role": "assistant", "content": answer}, "finish_reason": "stop"}],
            "usage": {"prompt_tokens": pt, "completion_tokens": ct, "total_tokens": pt + ct},
        },
        ensure_ascii=False,
    ).encode()
    handler.send_response(200)
    handler.send_header("Content-Type", "application/json")
    handler.send_header("Content-Length", str(len(payload)))
    handler.end_headers()
    handler.wfile.write(payload)


def serve_anthropic_messages(handler):
    length = int(handler.headers.get("Content-Length") or 0)
    body = json.loads(handler.rfile.read(length).decode()) if length else {}
    if handler.headers.get("x-api-key") != "sk-ant-mock":
        payload = json.dumps({"error": {"type": "authentication_error", "message": "bad key"}}).encode()
        handler.send_response(401)
    elif not body.get("max_tokens"):
        payload = json.dumps({"error": {"type": "invalid_request_error", "message": "max_tokens required"}}).encode()
        handler.send_response(400)
    else:
        model = body.get("model", "claude-mock")
        sys_text = body.get("system", "")
        user_text = body["messages"][-1].get("content", "") if body.get("messages") else ""
        answer = f"echo-claude[{model}]{('sys:' + sys_text.strip()) if sys_text else ''}< {user_text[:40]} >"
        if body.get("stream"):
            handler.send_response(200)
            handler.send_header("Content-Type", "text/event-stream; charset=utf-8")
            handler.send_header("Transfer-Encoding", "chunked")
            handler.end_headers()

            def event(ob):
                data = b"data: " + json.dumps(ob, ensure_ascii=False).encode() + b"\n\n"
                handler.wfile.write(f"{len(data):x}\r\n".encode() + data + b"\r\n")
                handler.wfile.flush()

            event({"type": "message_start", "message": {"model": model, "usage": {"input_tokens": len(user_text)}}})
            for i in range(0, len(answer), 4):
                event({"type": "content_block_delta", "delta": {"type": "text_delta", "text": answer[i : i + 4]}})
            event({"type": "message_delta", "delta": {"stop_reason": "end_turn"}, "usage": {"output_tokens": len(answer)}})
            event({"type": "message_stop"})
            handler.wfile.write(b"0\r\n\r\n")
            return
        payload = json.dumps(
            {
                "id": "msg_mock",
                "type": "message",
                "model": model,
                "content": [{"type": "text", "text": answer}],
                "stop_reason": "end_turn",
                "usage": {"input_tokens": len(user_text), "output_tokens": len(answer)},
            },
            ensure_ascii=False,
        ).encode()
        handler.send_response(200)
    handler.send_header("Content-Type", "application/json")
    handler.send_header("Content-Length", str(len(payload)))
    handler.end_headers()
    handler.wfile.write(payload)


class MockUpstream(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_POST(self):
        p = self.path.split("?")[0]
        if p.endswith("/chat/completions"):
            serve_openai_chat(self)
        elif p.endswith("/v1/messages"):
            serve_anthropic_messages(self)
        elif p.endswith("/embeddings"):
            length = int(self.headers.get("Content-Length") or 0)
            body = json.loads(self.rfile.read(length).decode()) if length else {}
            payload = json.dumps(
                {
                    "object": "list",
                    "data": [{"object": "embedding", "index": 0, "embedding": [0.1, 0.2, 0.3]}],
                    "model": body.get("model", "embed-mock"),
                    "usage": {"prompt_tokens": 1, "total_tokens": 1},
                }
            ).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)
        else:
            payload = b"not found"
            self.send_response(404)
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)

    def do_GET(self):
        p = self.path.split("?")[0]
        if p.endswith("/models"):
            payload = json.dumps(
                {
                    "object": "list",
                    "data": [
                        {"id": m, "object": "model"}
                        for m in ("gpt-mock-discovered", "claude-3-5-mock", "not-allowed-model", "smart")
                    ],
                }
            ).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)
            return
        self.do_POST()

    def log_message(self, fmt, *args):
        pass


def start_mock(port: int) -> ThreadingHTTPServer:
    srv = ThreadingHTTPServer(("127.0.0.1", port), MockUpstream)
    srv.daemon_threads = True
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv
