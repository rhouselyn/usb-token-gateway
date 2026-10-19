# -*- coding: utf-8 -*-
"""Protocol transforms (DESIGN §7.5).

MVP scope:
  * ``openai``    -- pass-through (or OpenAI-compatible services)
  * ``anthropic`` -- request/response transform incl. SSE event mapping
  * ``gemini``    -- stub (501) in the MVP, on the M2 roadmap
"""

from __future__ import annotations

import json
import typing as t

ANTHROPIC_VERSION = "2023-06-01"

FINISH_MAP = {
    "end_turn": "stop",
    "stop_sequence": "stop",
    "max_tokens": "length",
    "tool_use": "tool_calls",
}


# ---------------------------------------------------------------- request side
def build_upstream(
    channel: dict[str, t.Any],
    openai_body: dict[str, t.Any],
) -> tuple[str, str, bytes, dict[str, str]]:
    """Build (METHOD, path, body_bytes, headers) for the upstream call.

    ``path`` is relative to the channel ``base_url``.
    """
    proto = channel.get("protocol", "openai")
    if proto == "gemini":  # M2 roadmap item
        raise NotImplementedError("gemini transform ships with M2; use /proxy passthrough meanwhile")
    if proto == "anthropic":
        return _build_anthropic(channel, openai_body)
    return _build_openai(channel, openai_body)


def _upstream_url(channel: dict[str, t.Any], path: str) -> str:
    base = (channel.get("base_url") or "").rstrip("/")
    return base + path


def _apply_auth(channel: dict[str, t.Any], headers: dict[str, str] | None = None) -> dict[str, str]:
    headers = headers or {}
    key = channel.get("api_key") or ""
    if channel.get("auth_header", "x-api-key") == "bearer":
        headers["Authorization"] = f"Bearer {key}"
    else:
        headers["x-api-key"] = key
        headers["Authorization"] = f"Bearer {key}"  # some gateways accept both
    return headers


def _build_openai(channel, body) -> tuple[str, str, bytes, dict[str, str]]:
    base = (channel.get("base_url") or "").rstrip("/")
    if base.endswith("/chat/completions"):
        path = ""
    elif base.endswith("/v1") or base.count("/") <= 2:
        path = "/chat/completions"
    else:
        path = "/chat/completions"
    payload = dict(body)
    payload.setdefault("model", channel.get("models", ["gpt-4o"])[0] if channel.get("models") else "gpt-4o")
    headers = {"Content-Type": "application/json", "Accept": "application/json"}
    hdrs = _apply_auth(channel, headers)
    return "POST", _upstream_url(channel, path), json.dumps(payload, ensure_ascii=False).encode(), hdrs


def _build_anthropic(channel, body) -> tuple[str, str, bytes, dict[str, str]]:
    messages = body.get("messages", [])
    system_parts = []
    convo = []
    for m in messages:
        role, content = m.get("role"), m.get("content")
        if role == "system":
            if isinstance(content, list):
                system_parts += [seg.get("text", "") for seg in content if isinstance(seg, dict)]
            else:
                system_parts.append(content or "")
        else:
            convo.append({"role": "assistant" if role == "assistant" else "user", "content": content})
    payload: dict[str, t.Any] = {
        "model": body.get("model") or (channel.get("models") or ["claude-3-5-sonnet-latest"])[0],
        "max_tokens": body.get("max_tokens") or 1024,  # Anthropic requires it
        "messages": convo,
        "stream": bool(body.get("stream")),
    }
    if system_parts:
        payload["system"] = "\n".join(x for x in system_parts if x)
    for src, dst in (("temperature", "temperature"), ("top_p", "top_p"), ("stop", "stop_sequences")):
        if src in body and body[src] is not None:
            payload[dst] = body[src]
    headers = {
        "Content-Type": "application/json",
        "Accept": "application/json",
        "anthropic-version": ANTHROPIC_VERSION,
    }
    hdrs = _apply_auth(channel, headers)
    url = _upstream_url(channel, "/v1/messages")
    return "POST", url, json.dumps(payload, ensure_ascii=False).encode(), hdrs


# --------------------------------------------------------------- response side
def upstream_to_openai_response(
    channel: dict[str, t.Any],
    openai_body: dict[str, t.Any],
    upstream_json: dict[str, t.Any],
) -> dict[str, t.Any]:
    proto = channel.get("protocol", "openai")
    if proto == "anthropic":
        return anthropic_to_openai_response(openai_body, upstream_json)
    return upstream_json  # openai: unchanged


def anthropic_to_openai_response(openai_body: dict, aj: dict) -> dict:
    text = "".join(
        b.get("text", "")
        for b in aj.get("content", [])
        if isinstance(b, dict) and b.get("type") == "text"
    )
    stop = FINISH_MAP.get(aj.get("stop_reason"), "stop")
    usage_in = aj.get("usage", {}).get("input_tokens", 0)
    usage_out = aj.get("usage", {}).get("output_tokens", 0)
    return {
        "id": "chatcmpl-bridge-" + aj.get("id", "x")[:16],
        "object": "chat.completion",
        "created": int(__import__("time").time()),
        "model": openai_body.get("model", "claude"),
        "choices": [
            {
                "index": 0,
                "message": {"role": "assistant", "content": text},
                "finish_reason": stop,
            }
        ],
        "usage": {
            "prompt_tokens": usage_in,
            "completion_tokens": usage_out,
            "total_tokens": usage_in + usage_out,
        },
        # bridge extras for debugging
        "bridge": {"upstream": "anthropic", "channel_id": aj.get("id")},
    }


# ------------------------------------------------------------------ SSE utils
def upsert_openai_chunk(
    chunk_id: str,
    model: str,
    created: int,
    *,
    role: str | None = None,
    content: str | None = None,
    finish_reason: str | None = None,
) -> bytes:
    delta: dict[str, t.Any] = {}
    if role is not None:
        delta["role"] = role
    if content is not None:
        delta["content"] = content
    payload = {
        "id": chunk_id,
        "object": "chat.completion.chunk",
        "created": created,
        "model": model,
        "choices": [{"index": 0, "delta": delta, "finish_reason": finish_reason}],
    }
    return b"data: " + json.dumps(payload, ensure_ascii=False).encode() + b"\n\n"


ANTHROPIC_SSE_HANDLERS = """
Maps raw anthropic SSE events to OpenAI-format data: lines.
Used by server.py during streaming.
"""


def anthropic_event_to_chunks(
    event: bytes,
    state: dict[str, t.Any],
) -> list[bytes]:
    """Convert one anthropic SSE event into zero or more OpenAI chunks.

    ``state`` carries per-request context (model, chunk id, created, stop_reason).
    """
    raw = event.strip()
    if not raw or raw.startswith(b":") or raw.startswith(b"event:") or raw.startswith(b"event="):
        return []
    if raw == b"data: [DONE]" or raw == b"data:[DONE]":
        return [b"data: [DONE]\n\n"]
    if not raw.startswith(b"data:"):
        return []
    data = raw[5:].strip()
    try:
        obj = json.loads(data.decode("utf-8", "replace"))
    except json.JSONDecodeError:
        return []

    out: list[bytes] = []
    etype = obj.get("type", "")

    def mk(**kw: t.Any) -> bytes:
        return upsert_openai_chunk(
            state["id"], state["model"], state["created"], **kw
        )

    if etype == "message_start":
        out.append(mk(role="assistant"))
    elif etype == "content_block_start":
        pass
    elif etype == "content_block_delta":
        delta = obj.get("delta", {})
        if delta.get("type") in ("text_delta", "input_json_delta"):
            text = delta.get("text", delta.get("partial_json", ""))
            if text:
                out.append(mk(content=text))
    elif etype == "content_block_stop":
        pass
    elif etype == "message_delta":
        reason = FINISH_MAP.get(obj.get("delta", {}).get("stop_reason"), None)
        usage = obj.get("usage", {})
        if reason:
            state["finish_reason"] = reason
            if usage.get("output_tokens"):
                state["completion_tokens"] = usage["output_tokens"]
    elif etype == "message_stop":
        out.append(mk(finish_reason=state.get("finish_reason", "stop")))
        out.append(b"data: [DONE]\n\n")
    elif etype == "ping":
        pass
    return out


def openai_error(message: str, code: str = "", status: int = 0) -> dict[str, t.Any]:
    return {
        "error": {
            "message": message,
            "type": "bridge_error" if not status else f"upstream_{status}",
            "code": code or str(status),
        }
    }
