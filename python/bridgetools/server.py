# -*- coding: utf-8 -*-
"""HTTP gateway core (DESIGN §4-§8).

Endpoints
---------
  POST /v1/chat/completions      OpenAI-compatible; multi-channel scheduler
  POST /v1/embeddings            pass-through (openai-protocol only in MVP)
  GET  /v1/models                aggregate capability list
  ANY  /proxy/{cid}/{path}...    raw reverse-proxy passthrough
  GET  /healthz                  liveness
  GET  /admin + JSON endpoints   admin panel (X-Admin-Token required)
"""

from __future__ import annotations

import http.client
import http.server
import json
import logging
import socket
import threading
import time
import urllib.parse
from typing import Any

from . import admin as adminui
from . import transforms as T
from . import vault as V
from .scheduler import Scheduler
from .usage import UsageDB
from .vault import Vault

log = logging.getLogger("bridge")

STREAM_CT = "text/event-stream"


def _split_url(url: str) -> tuple[str, str, int, str]:
    """-> (scheme, path_with_query, port, host)"""
    p = urllib.parse.urlparse(url)
    if p.scheme not in ("http", "https"):
        raise ValueError(f"unsupported scheme: {p.scheme!r}")
    host = p.hostname or ""
    port = p.port or (443 if p.scheme == "https" else 80)
    path = p.path or "/"
    if p.query:
        path += "?" + p.query
    return p.scheme, path, port, host


class Bridge:
    """Shared runtime state.  May start LOCKED (no vault material in memory)."""

    def __init__(self, data_dir: str = "."):
        self.data_dir = data_dir
        self.vault: Vault | None = None
        self.policy: dict[str, Any] | None = None
        self.scheduler: Scheduler | None = None
        self.usage = UsageDB(data_dir)
        self.admin_token = ""
        self.started = time.time()
        self.port = 8989

    # ------------------------------------------------------------ lifecycle
    @property
    def locked(self) -> bool:
        return self.vault is None

    def unlock(self, password: str) -> tuple[str, bool]:
        """Unlock the existing vault or create one on first run.

        Returns (admin_token, created_now).
        """
        created = not V.exists(self.data_dir)
        if created:
            vault, _dk = V.create(self.data_dir, password)
            log.info("created new vault at %s", vault.path)
        else:
            vault = V.load(self.data_dir, password)
            log.info("vault unlocked")
        self.vault = vault
        self.policy = vault.policy
        self.scheduler = Scheduler(vault.channels, self.policy)
        self.admin_token = vault.data.get("admin_token", "")
        self._port_hint = int(vault.data.get("port", 8989))
        return self.admin_token, created

    def lock(self) -> None:
        """Panel-initiated lock: downstream traffic stops until re-unlock.

        NOTE per DESIGN §5.3: fully wiping derived keys from process memory
        is a hardening item for the Go version.
        """
        self.vault = None
        self.policy = None
        self.scheduler = None
        self.admin_token = ""
        log.info("bridge locked; awaiting re-unlock")

    def state_payload(self) -> dict[str, Any]:
        return {
            "locked": self.locked,
            "has_vault": V.exists(self.data_dir),
            "port": self.port,
            "started": self.started,
        }

    # ------------------------------------------------------------------ auth
    def downstream_ok(self, headers: dict[str, str]) -> bool:
        if self.locked:
            return False
        auth = headers.get("Authorization", headers.get("authorization", ""))
        if not auth.lower().startswith("bearer "):
            return False
        return self.vault.check_downstream_key(auth[7:].strip()) is not None  # type: ignore[union-attr]

    def admin_ok(self, headers: dict[str, str]) -> bool:
        token = headers.get("X-Admin-Token", headers.get("x-admin-token", ""))
        return bool(token) and token == self.admin_token and bool(self.admin_token)

    def save(self) -> None:
        assert self.vault is not None
        self.vault.save()
        self.scheduler.refresh(self.vault.channels, self.vault.policy)


def upstream_call(
    url: str,
    method: str,
    headers: dict[str, str],
    body: bytes | None,
    timeout_first_byte: float,
) -> tuple[http.client.HTTPConnection, http.client.HTTPResponse]:
    """Issue one upstream request; return (connection, response).

    Raise on connection errors/timeouts. Caller is responsible for closing conn.
    """
    scheme, path, port, host = _split_url(url)
    if scheme == "https":
        conn: http.client.HTTPConnection = http.client.HTTPSConnection(host, port, timeout=timeout_first_byte)
    else:
        conn = http.client.HTTPConnection(host, port, timeout=timeout_first_byte)
    conn.request(method, path, body=body, headers=headers)
    resp = conn.getresponse()  # raises on timeout / refused
    # idle read timeout for the streaming phase
    if conn.sock:
        conn.sock.settimeout(getattr(_idle_timeout, "value", None) or 120.0)
    return conn, resp


_idle_timeout = threading.local()


class Handler(http.server.BaseHTTPRequestHandler):
    server_version = "USBBridge/0.1"
    protocol_version = "HTTP/1.1"

    # ------------------------------------------------------------- plumbing
    def _json_body(self) -> dict:
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length) if length else b""
        if not raw:
            return {}
        try:
            return json.loads(raw.decode("utf-8", "replace"))
        except json.JSONDecodeError:
            return {"__bad_json__": True}

    def _reply_json(self, obj: dict, status: int = 200, extra_headers: dict | None = None) -> None:
        payload = json.dumps(obj, ensure_ascii=False).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(payload)))
        self.send_header("Connection", "close")
        for k, v in (extra_headers or {}).items():
            self.send_header(k, v)
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, fmt, *args) -> None:  # console-only, never to disk
        if self.path.startswith("/admin"):
            log.info("admin %s %s", self.command, self.path)
        elif self.path in ("/healthz",):
            log.debug(fmt % args)
        else:
            log.info("%s %s", self.command, self.path.split("?")[0])

    # ------------------------------------------------------------- routing
    def do_GET(self) -> None:
        try:
            self._do_GET()
        except (BrokenPipeError, ConnectionResetError):
            log.debug("client closed connection")
        except Exception:  # noqa: BLE001
            log.exception("GET handler failed")
            try:
                self._reply_json(T.openai_error("internal error", "500"), 500)
            except Exception:
                pass

    def _do_GET(self) -> None:
        path = urllib.parse.urlparse(self.path).path
        if path == "/healthz":
            self._reply_json({"ok": True, "started": self.bridge.started})
        elif path == "/v1/models":
            self._models()
        elif path == "/admin":
            self._admin_page()
        elif path.startswith("/admin/state"):
            self._reply_json(self.bridge.state_payload())
        elif path.startswith("/admin/"):
            self._admin_get(path)
        else:
            self._reply_json(T.openai_error("not found", "404"), 404)

    def do_POST(self) -> None:
        try:
            self._do_POST()
        except (BrokenPipeError, ConnectionResetError):
            log.debug("client closed connection")
        except Exception:  # noqa: BLE001
            log.exception("POST handler failed")
            try:
                self._reply_json(T.openai_error("internal error", "500"), 500)
            except Exception:
                pass

    def _do_POST(self) -> None:
        path = urllib.parse.urlparse(self.path).path
        if path == "/v1/chat/completions":
            self._chat()
        elif path == "/v1/embeddings":
            self._embeddings()
        elif path.startswith("/proxy/"):
            self._proxy("POST")
        elif path == "/admin/cmd":
            self._admin_cmd()
        elif path == "/admin/unlock":
            self._admin_unlock()
        else:
            self._reply_json(T.openai_error("not found", "404"), 404)

    def do_PUT(self) -> None:
        self._proxy_guarded("PUT")

    def do_DELETE(self) -> None:
        self._proxy_guarded("DELETE")

    def do_PATCH(self) -> None:
        self._proxy_guarded("PATCH")

    def _proxy_guarded(self, method: str) -> None:
        try:
            self._proxy(method)
        except (BrokenPipeError, ConnectionResetError):
            log.debug("client closed connection")
        except Exception:  # noqa: BLE001
            log.exception("%s proxy handler failed", method)
            try:
                self._reply_json(T.openai_error("internal error", "500"), 500)
            except Exception:
                pass

    @property
    def bridge(self) -> Bridge:
        return self.server.bridge  # type: ignore[attr-defined]

    # ------------------------------------------------------------- /v1/*
    def _models(self) -> None:
        bridge = self.bridge
        if bridge.locked:
            self._reply_json(T.openai_error("bridge is locked; open /admin", "locked"), 503)
            return
        if not bridge.downstream_ok(self.headers):
            self._reply_json(T.openai_error("invalid downstream key", "401"), 401)
            return
        seen, out = set(), []
        for ch in self.bridge.vault.channels:
            if not ch.get("enabled", True):
                continue
            for m in ch.get("models", []):
                if m not in seen:
                    seen.add(m)
                    out.append({"id": m, "object": "model", "owned_by": ch["id"]})
        self._reply_json({"object": "list", "data": out})

    def _chat(self) -> None:
        bridge = self.bridge
        if bridge.locked:
            self._reply_json(T.openai_error("bridge is locked; open /admin", "locked"), 503)
            return
        if not bridge.downstream_ok(self.headers):
            self._reply_json(T.openai_error("invalid downstream key", "401"), 401)
            return
        body = self._json_body()
        if body.get("__bad_json__"):
            self._reply_json(T.openai_error("request body is not valid json", "400"), 400)
            return
        model = body.get("model", "")
        stream = bool(body.get("stream"))
        policy = bridge.policy
        _idle_timeout.value = float(policy.get("timeout_stream_idle_s", 120))
        t0 = time.time()

        candidates = bridge.scheduler.candidates(model)
        budget = max(0, int(bridge.scheduler.retry_budget)) + 1
        candidates = candidates[: budget] if budget else candidates[:1]

        if not candidates:
            bridge.usage.record(model, "", 503, 0, error="no healthy channel for model")
            self._reply_json(
                T.openai_error(f"no healthy channel serves model {model!r}", "503"),
                503,
            )
            return

        last_err = "no candidate attempted"
        for attempt, ch in enumerate(candidates, 1):
            try:
                method, url, payload, hdrs = T.build_upstream(ch, body)
            except NotImplementedError as e:
                bridge.scheduler.report(ch["id"], False, status=0)
                last_err = str(e)
                continue
            except KeyError as e:
                last_err = f"channel {ch.get('id')} misconfigured: missing {e}"
                continue
            conn = None
            try:
                conn, resp = upstream_call(
                    url, method, hdrs, payload,
                    float(policy.get("timeout_first_byte_s", 20)),
                )
                first_ms = (time.time() - t0) * 1000.0
                if resp.status >= 400:
                    err_body = resp.read(4096).decode("utf-8", "replace")
                    retry_after = resp.getheader("Retry-After")
                    bridge.scheduler.report(
                        ch["id"], False,
                        status=resp.status,
                        retry_after=float(retry_after) if retry_after and retry_after.isdigit() else 0.0,
                    )
                    bridge.usage.record(model, ch["id"], resp.status, first_ms, error=err_body[:200])
                    last_err = f"upstream {resp.status} from {ch['id']}"
                    try:
                        conn.close()
                    except Exception:
                        pass
                    # attempt next channel only if we have NOT started responding
                    if attempt < len(candidates):
                        continue
                    # final: surface upstream error, mapped per DESIGN §6.2
                    final_status = 502 if resp.status >= 500 else resp.status
                    if resp.status == 429:
                        final_status = 429
                    self._reply_json(
                        T.openai_error(_err_text(err_body, last_err), str(resp.status), final_status),
                        final_status,
                    )
                    return
                if stream:
                    self._stream_response(ch, body, resp, conn, attempt, t0, first_ms)
                    return
                raw_body = resp.read()
                try:
                    up_json = json.loads(raw_body.decode("utf-8", "replace"))
                except json.JSONDecodeError:
                    bridge.scheduler.report(ch["id"], False, status=resp.status)
                    bridge.usage.record(model, ch["id"], resp.status, first_ms, error="non-json upstream body")
                    last_err = f"non-json response from {ch['id']}"
                    try:
                        conn.close()
                    except Exception:
                        pass
                    if attempt < len(candidates):
                        continue
                    self._reply_json(T.openai_error(last_err, "502"), 502)
                    return
                out = T.upstream_to_openai_response(ch, body, up_json)
                bridge.scheduler.report(ch["id"], True, first_ms, resp.status)
                u = out.get("usage", {})
                bridge.usage.record(
                    model, ch["id"], 200, first_ms,
                    prompt_tokens=u.get("prompt_tokens", 0),
                    completion_tokens=u.get("completion_tokens", 0),
                )
                self._reply_json(
                    out, 200,
                    {"X-BRG-Channel": ch["id"], "X-BRG-Attempt": str(attempt),
                     "X-BRG-Latency-First": f"{first_ms:.0f}"},
                )
                return
            except NotImplementedError:
                raise
            except (OSError, socket.timeout, http.client.HTTPException, TimeoutError) as e:
                bridge.scheduler.report(ch["id"], False, status=0)
                bridge.usage.record(model, ch["id"], 0, (time.time() - t0) * 1000, error=str(e)[:200])
                last_err = f"upstream connect/timeout on {ch.get('id')}: {e}"
                try:
                    if conn:
                        conn.close()
                except Exception:
                    pass
                continue
            except (BrokenPipeError, ConnectionResetError):
                # client disconnected mid-stream -- cancel upstream, no retry
                try:
                    if conn:
                        conn.close()
                except Exception:
                    pass
                log.info("client disconnected, upstream cancelled")
                return
        self._reply_json(T.openai_error(last_err, "upstream_exhausted"), 502)

    def _stream_response(
        self,
        ch: dict,
        body: dict,
        resp: http.client.HTTPResponse,
        conn: http.client.HTTPConnection,
        attempt: int,
        t0: float,
        first_ms: float,
    ) -> None:
        bridge = self.bridge
        proto = ch.get("protocol", "openai")
        self.send_response(200)
        self.send_header("Content-Type", STREAM_CT + "; charset=utf-8")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Transfer-Encoding", "chunked")
        self.send_header("X-BRG-Channel", ch["id"])
        self.send_header("X-BRG-Attempt", str(attempt))
        self.send_header("X-BRG-Latency-First", f"{first_ms:.0f}")
        self.end_headers()
        bridge.scheduler.report(ch["id"], True, first_ms, resp.status)
        bridge.usage.record(body.get("model", ""), ch["id"], 200, first_ms)

        state = {
            "id": "chatcmpl-bridge-stream",
            "model": body.get("model", "stream"),
            "created": int(time.time()),
            "finish_reason": None,
        }

        def write_payload(data: bytes) -> None:
            self.wfile.write(f"{len(data):x}\r\n".encode() + data + b"\r\n")
            self.wfile.flush()

        def write_raw_event(event_line: bytes) -> None:
            # re-chunk a raw `data: ...` line for the client
            if not event_line.strip():
                return
            payload = event_line + b"\n\n"
            self.wfile.write(f"{len(payload):x}\r\n".encode() + payload + b"\r\n")
            self.wfile.flush()

        try:
            fp = resp
            while True:
                line = fp.readline()
                if not line:
                    break
                sline = line.strip()
                if not sline:
                    continue
                if proto == "anthropic":
                    for chunk in T.anthropic_event_to_chunks(sline, state):
                        write_payload(chunk)
                else:
                    write_raw_event(sline if sline.startswith(b"data:") else b"data: " + sline)
        except (BrokenPipeError, ConnectionResetError, socket.timeout) as e:
            log.info("stream ended abnormally: %s", e)
        finally:
            try:
                write_payload(b"data: [DONE]\n\n")
            except Exception:
                pass
            try:
                self.wfile.write(b"0\r\n\r\n")
                self.wfile.flush()
            except Exception:
                pass
            try:
                conn.close()
            except Exception:
                pass

    def _embeddings(self) -> None:
        bridge = self.bridge
        if bridge.locked:
            self._reply_json(T.openai_error("bridge is locked; open /admin", "locked"), 503)
            return
        if not bridge.downstream_ok(self.headers):
            self._reply_json(T.openai_error("invalid downstream key", "401"), 401)
            return
        body = self._json_body()
        model = body.get("model", "")
        ch = next(
            (c for c in bridge.scheduler.candidates(model) if c.get("protocol") == "openai"),
            None,
        )
        if ch is None:
            self._reply_json(
                T.openai_error("no openai-protocol channel available for embeddings", "501"), 501
            )
            return
        t0 = time.time()
        try:
            url = ch.get("base_url", "").rstrip("/")
            if url.endswith("/chat/completions"):
                url = url.rsplit("/chat/completions", 1)[0]
            if not url.endswith("/embeddings"):
                url += "/embeddings"
            hdrs = {"Content-Type": "application/json"}
            key = ch.get("api_key", "")
            if ch.get("auth_header", "bearer") == "bearer":
                hdrs["Authorization"] = f"Bearer {key}"
            else:
                hdrs["x-api-key"] = key
            conn, resp = upstream_call(
                url, "POST", hdrs, json.dumps(body).encode(),
                float(bridge.policy.get("timeout_first_byte_s", 20)),
            )
            first_ms = (time.time() - t0) * 1000
            payload = resp.read()
            conn.close()
            self._reply_json_json(payload, resp.status, ch["id"], attempt=1, first_ms=first_ms)
        except Exception as e:  # noqa: BLE001
            bridge.usage.record(model, ch["id"], 0, 0, error=str(e)[:200])
            self._reply_json(T.openai_error(str(e), "502"), 502)

    def _reply_json_json(self, payload: bytes, status: int, cid: str, attempt: int, first_ms: float) -> None:
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(payload)))
        self.send_header("X-BRG-Channel", cid)
        self.send_header("X-BRG-Attempt", str(attempt))
        self.send_header("X-BRG-Latency-First", f"{first_ms:.0f}")
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(payload)

    # ------------------------------------------------------------- /proxy
    def _proxy(self, method: str) -> None:
        bridge = self.bridge
        if bridge.locked:
            self._reply_json(T.openai_error("bridge is locked; open /admin", "locked"), 503)
            return
        if not bridge.downstream_ok(self.headers):
            self._reply_json(T.openai_error("invalid downstream key", "401"), 401)
            return
        parts = urllib.parse.urlparse(self.path).path.split("/")  # ['', 'proxy', cid, ...rest]
        if len(parts) < 3 or not parts[2]:
            self._reply_json(T.openai_error("usage: /proxy/{channel_id}/path", "404"), 404)
            return
        cid, rest = parts[2], "/".join(parts[3:])
        ch = bridge.scheduler.get(cid)
        if ch is None:
            self._reply_json(T.openai_error(f"unknown channel {cid}", "404"), 404)
            return
        base = (ch.get("base_url") or "").rstrip("/")
        target = base + ("/" + rest if rest else "/")
        length = int(self.headers.get("Content-Length") or 0)
        payload = self.rfile.read(length) if length else None
        fwd = {k: v for k, v in self.headers.items() if k.lower() not in {"authorization", "host"}}
        key = ch.get("api_key", "")
        if ch.get("auth_header", "bearer") == "bearer":
            fwd["Authorization"] = f"Bearer {key}"
        else:
            fwd["x-api-key"] = key
            fwd["anthropic-version"] = fwd.get("anthropic-version", "2023-06-01")
        t0 = time.time()
        try:
            conn, resp = upstream_call(
                target, method, fwd, payload,
                float(bridge.policy.get("timeout_first_byte_s", 20)),
            )
        except Exception as e:  # noqa: BLE001
            bridge.usage.record("proxy:" + method, cid, 0, 0, error=str(e)[:200])
            self._reply_json(T.openai_error(f"upstream unreachable: {e}", "502"), 502)
            return
        first_ms = (time.time() - t0) * 1000
        bridge.scheduler.report(cid, resp.status < 400, first_ms, resp.status)
        bridge.usage.record("proxy:" + method, cid, resp.status, first_ms)
        self.send_response(resp.status)
        hop = {"connection", "keep-alive", "transfer-encoding", "content-encoding"}
        for k, v in resp.getheaders():
            if k.lower() not in hop:
                self.send_header(k, v)
        self.send_header("X-BRG-Channel", cid)
        self.send_header("Connection", "close")
        if str(resp.getheader("Transfer-Encoding", "")).lower() == "chunked":
            self._proxy_stream(resp, conn)
            return
        body = resp.read()
        conn.close()
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        try:
            self.wfile.write(body)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def _proxy_stream(self, resp, conn) -> None:
        self.send_header("Transfer-Encoding", "chunked")
        self.end_headers()
        try:
            while True:
                data = resp.read(4096)
                if not data:
                    break
                self.wfile.write(f"{len(data):x}\r\n".encode() + data + b"\r\n")
                self.wfile.flush()
            self.wfile.write(b"0\r\n\r\n")
        except (BrokenPipeError, ConnectionResetError, socket.timeout):
            pass
        finally:
            try:
                conn.close()
            except Exception:
                pass

    # ------------------------------------------------------------- admin
    def _admin_page(self) -> None:
        # static HTML only -- all data endpoints below require the admin token
        body = adminui.render_page()
        self.send_response(200)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(body)

    def _admin_get(self, path: str) -> None:
        bridge = self.bridge
        if not bridge.admin_ok(self.headers):
            self._reply_json(T.openai_error("admin token required (X-Admin-Token)", "401"), 401)
            return
        sub = path[len("/admin/") :]
        if sub == "summary":
            data = bridge.usage.summary()
            data["strategy"] = bridge.policy.get("strategy")
            data["backend_protocols"] = sorted({c.get("protocol", "openai") for c in bridge.vault.channels})
            self._reply_json(data)
        elif sub == "channels":
            self._reply_json(
                {"channels": [adminui.sanitize_channel(c) for c in bridge.vault.channels]}
            )
        elif sub == "dkeys":
            self._reply_json(
                {"keys": [adminui.sanitize_dkey(k) for k in bridge.vault.downstream_keys]}
            )
        elif sub == "recent":
            self._reply_json({"recent": bridge.usage.list_recent()})
        elif sub == "page":
            body = adminui.render_page()
            self.send_response(200)
            self.send_header("Content-Type", "text/html; charset=utf-8")
            self.send_header("Content-Length", str(len(body)))
            self.send_header("Connection", "close")
            self.end_headers()
            self.wfile.write(body)
        else:
            self._reply_json(T.openai_error("unknown admin endpoint", "404"), 404)

    def _admin_unlock(self) -> None:
        """Web-side init/unlock.  Auth = knowing the master password."""
        body = self._json_body()
        pw = str(body.get("master_password") or "")
        if len(pw) < 8:
            self._reply_json(
                {"ok": False, "error": "master password must be at least 8 chars"}, 400
            )
            return
        try:
            token, created = self.bridge.unlock(pw)
        except Exception as e:  # noqa: BLE001 -- wrong password / bad file
            self._reply_json({"ok": False, "error": f"unlock failed: {e}"}, 200)
            return
        self._reply_json(
            {"ok": True, "admin_token": token, "fresh": created, "port": self.bridge.port}
        )

    def _admin_cmd(self) -> None:
        bridge = self.bridge
        if bridge.locked:
            self._reply_json({"ok": False, "error": "bridge locked"}, 401)
            return
        if not bridge.admin_ok(self.headers):
            self._reply_json(T.openai_error("admin token required (X-Admin-Token)", "401"), 401)
            return
        cmd = self._json_body()
        action = cmd.get("action", "")
        arg = cmd.get("arg")
        try:
            if action == "add_channel":
                if not cmd.get("base_url") or not cmd.get("api_key"):
                    raise ValueError("base_url 和 api_key 必填")
                ch = bridge.vault.add_channel(
                    {
                        "protocol": cmd.get("proto", "openai"),
                        "base_url": cmd["base_url"],
                        "api_key": cmd["api_key"],
                        "models": cmd.get("models", "*"),
                        "weight": cmd.get("weight", 10),
                        "priority": cmd.get("priority", 0),
                    }
                )
                bridge.save()
                self._reply_json({"ok": True, "channel_id": ch["id"]})
            elif action == "toggle_channel":
                for c in bridge.vault.channels:
                    if c["id"] == arg:
                        c["enabled"] = not c.get("enabled", True)
                bridge.save()
                self._reply_json({"ok": True})
            elif action == "del_channel":
                bridge.vault.channels[:] = [c for c in bridge.vault.channels if c["id"] != arg]
                bridge.save()
                self._reply_json({"ok": True})
            elif action == "add_dkey":
                dk = bridge.vault.add_downstream_key(
                    name=cmd.get("name", ""),
                    ttl_hours=float(cmd.get("ttl", 0) or 0),
                )
                self._reply_json({"ok": True, "new_key": dk["key"]})
            elif action == "toggle_dkey":
                for k in bridge.vault.downstream_keys:
                    if k.get("key") == arg:
                        k["enabled"] = not k.get("enabled", True)
                bridge.save()
                self._reply_json({"ok": True})
            elif action == "del_dkey":
                bridge.vault.downstream_keys[:] = [
                    k for k in bridge.vault.downstream_keys if k.get("key") != arg
                ]
                bridge.save()
                self._reply_json({"ok": True})
            elif action == "set_strategy":
                assert arg in ("failover", "round_robin", "weighted", "least_errors")
                bridge.policy["strategy"] = arg
                bridge.save()
                self._reply_json({"ok": True, "strategy": arg})
            elif action == "set_port":
                p = int(arg)
                if not (1 <= p <= 65535):
                    raise ValueError("port out of range")
                bridge.vault.data["port"] = p  # type: ignore[union-attr]
                bridge.save()
                self._reply_json(
                    {"ok": True, "note": "port takes effect after gateway restart"}
                )
            elif action == "test_channel":
                assert bridge.scheduler is not None
                ch = bridge.scheduler.get(arg or "")
                if ch is None:
                    raise ValueError(f"unknown channel {arg!r}")
                model = (ch.get("models") or ["*"])[0]
                probe = {
                    "model": model,
                    "max_tokens": 16,
                    "messages": [{"role": "user", "content": "bridge connectivity test"}],
                }
                method, url, payload, hdrs = T.build_upstream(ch, probe)
                t0 = time.time()
                try:
                    conn, resp = upstream_call(
                        url, method, hdrs, payload,
                        float(bridge.policy.get("timeout_first_byte_s", 20)),
                    )
                except Exception as e:  # noqa: BLE001
                    self._reply_json({"ok": False, "status": 0, "error": str(e)})
                    return
                first_ms = (time.time() - t0) * 1000.0
                raw = resp.read(2048)
                try:
                    conn.close()
                except Exception:
                    pass
                self._reply_json(
                    {
                        "ok": resp.status < 400,
                        "status": resp.status,
                        "first_byte_ms": round(first_ms),
                        "body": raw.decode("utf-8", "replace")[:400],
                    }
                )
            elif action == "lock":
                bridge.lock()
                self._reply_json({"ok": True, "locked": True})
            else:
                self._reply_json({"ok": False, "error": f"unknown action {action!r}"})
        except Exception as e:  # noqa: BLE001
            self._reply_json({"ok": False, "error": str(e)})


def _err_text(raw: str, fallback: str) -> str:
    try:
        obj = json.loads(raw)
        if isinstance(obj, dict) and obj.get("error"):
            err = obj["error"]
            return err.get("message") if isinstance(err, dict) else str(err)
    except Exception:
        pass
    return fallback


class BridgeServer(http.server.ThreadingHTTPServer):
    daemon_threads = True
    # NOTE: on Windows, SO_REUSEADDR (allow_reuse_address=True) allows TWO
    # processes to bind the same port and hijack each other's connections.
    # We want a loud "address already in use" instead.
    allow_reuse_address = False

    def __init__(self, addr: tuple[str, int], handler, bridge: Bridge):
        super().__init__(addr, handler)
        self.bridge = bridge

    def handle_error(self, request, client_address) -> None:
        import sys

        _type, value, _tb = sys.exc_info()
        if isinstance(value, (ConnectionResetError, ConnectionAbortedError, BrokenPipeError)):
            log.debug("client connection dropped: %s", value)
            return
        log.exception("handler error for %s", client_address)


def serve(
    bridge: Bridge,
    host: str = "127.0.0.1",
    port: int | None = None,
) -> BridgeServer:
    port = port or getattr(bridge, "_port_hint", 8989)
    bridge.port = port
    httpd = BridgeServer((host, port), Handler, bridge)
    return httpd
