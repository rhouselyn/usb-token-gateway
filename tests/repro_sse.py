# -*- coding: utf-8 -*-
"""Minimal repro: raw SSE response from the Go gateway."""
import http.client
import json
import os
import shutil
import subprocess
import sys
import time
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
GODIR = os.path.join(ROOT, "go")
PORT = 8933

sys.path.insert(0, HERE)
from mock_upstream import start_mock  # noqa: E402

mock = start_mock(8491)
data_dir = os.path.join(GODIR, "bridge-repro")
shutil.rmtree(data_dir, ignore_errors=True)
gw = subprocess.Popen(
    [os.path.join(GODIR, "usbgw.exe"), "--hidden", "--data-dir", data_dir, "--port", str(PORT)],
    cwd=GODIR, stdout=subprocess.DEVNULL, stderr=subprocess.STDOUT,
)
time.sleep(1.2)


def post_json(path, body, headers=None):
    req = urllib.request.Request(f"http://127.0.0.1:{PORT}{path}",
                                 data=json.dumps(body).encode(), method="POST",
                                 headers=headers or {})
    with urllib.request.urlopen(req, timeout=20) as r:
        return json.loads(r.read())


u = post_json("/admin/unlock", {"master_password": "repro-pass-1"})
tok = u["admin_token"]
post_json("/admin/cmd", {"action": "add_channel", "proto": "openai",
                         "base_url": "http://127.0.0.1:8491/v1",
                         "api_key": "sk-mock", "models": "gpt-*"},
          {"X-Admin-Token": tok})
k = post_json("/admin/cmd", {"action": "add_dkey", "name": "x", "ttl": 0},
              {"X-Admin-Token": tok})["new_key"]

conn = http.client.HTTPConnection("127.0.0.1", PORT, timeout=20)
conn.request("POST", "/v1/chat/completions",
             json.dumps({"model": "gpt-mock", "stream": True, "max_tokens": 50,
                         "messages": [{"role": "user", "content": "raw"}]}),
             {"Authorization": "Bearer " + k, "Content-Type": "application/json"})
resp = conn.getresponse()
print("VERSION:", resp.version, "chunked_flag:", getattr(resp, "chunked", "?"))
print("STATUS:", resp.status)
print("HEADERS:", list(resp.getheaders()))
raw = resp.read(600)
print("RAW[:600]:", repr(raw))

# urllib path (same as e2e)
req = urllib.request.Request(
    f"http://127.0.0.1:{PORT_wrong}" if False else f"http://127.0.0.1:{PORT}/v1/chat/completions",
    data=json.dumps({"model": "gpt-mock", "stream": True, "max_tokens": 50,
                     "messages": [{"role": "user", "content": "raw"}]}).encode(),
    method="POST", headers={"Authorization": "Bearer " + k, "Content-Type": "application/json"})
try:
    with urllib.request.urlopen(req, timeout=20) as r2:
        print("URLOPEN HEADERS:", list(r2.getheaders())[:3], "...")
        body2 = r2.read()
        print("URLOPEN BEGIN:", repr(body2[:80]))
        print("URLOPEN COUNT data:", body2.count(b"data: "))
except Exception as e:
    print("URLOPEN ERR:", e)
gw.terminate()
shutil.rmtree(data_dir, ignore_errors=True)
