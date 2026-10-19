# -*- coding: utf-8 -*-
"""USB Token Gateway -- bridge CLI entry.

    python bridge.py init                       # create vault (first run)
    python bridge.py run [--port 8989]          # unlock + serve
    python bridge.py passwd                     # change master password
    python bridge.py add-channel ...            # add upstream channel
    python bridge.py add-key [--name ...]       # generate downstream key
"""

from __future__ import annotations

import argparse
import getpass
import logging
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from bridgetools import crypto_backend  # noqa: E402
from bridgetools import server as srv  # noqa: E402
from bridgetools import vault as V  # noqa: E402

BANNER = r"""
+=====================================================+
| USB Bridge  --  private token gateway (prototype)    |
+====================================================="""


def default_data_dir() -> str:
    # everything lives next to the program: on the USB stick
    return os.path.join(os.path.dirname(os.path.abspath(__file__)), "bridge")


def ask_password_twice() -> str:
    while True:
        p1 = getpass.getpass("set master password: ")
        if len(p1) < 8:
            print("  x master password needs at least 8 chars")
            continue
        p2 = getpass.getpass("repeat: ")
        if p1 == p2:
            return p1
        print("  x passwords differ")


def ask_password() -> str:
    return getpass.getpass("master password: ")


# ------------------------------------------------------------------ init
def cmd_init(args: argparse.Namespace) -> int:
    data_dir = args.data_dir or default_data_dir()
    if V.exists(data_dir) and not args.force:
        print("config.vault already exists; use --force to overwrite")
        return 1
    seed = None
    if args.seed and os.path.isfile(args.seed):
        seed = V.import_seed(data_dir, args.seed)
        chans = seed.get("channels", [])
        print(f"seed file: importing {len(chans)} channel(s)")
    password = args.password or ask_password_twice()
    vault, dk = V.create(data_dir, password, seed)
    print()
    print("init done.")
    print(f"  data dir    : {os.path.abspath(data_dir)}")
    print(f"  crypto      : {crypto_backend.backend_name()}")
    print()
    print(f"  downstream key (save it!): {dk}")
    print(f"  admin token             : {vault.data.get('admin_token')}")
    print()
    print("next step: python bridge.py run")
    return 0


# ------------------------------------------------------------------ run
def cmd_run(args: argparse.Namespace) -> int:
    data_dir = args.data_dir or default_data_dir()
    bridge = srv.Bridge(data_dir=data_dir)
    admin_token = ""
    if args.password:
        try:
            admin_token, _fresh = bridge.unlock(args.password)
        except Exception as e:  # noqa: BLE001
            print(f"unlock failed: {e}")
            return 1
    elif not V.exists(data_dir):
        print("* 初始化提示: 现在没有加密配置，先启动也可以 --")
        print("  启动后打开控制面板, 按提示设置主密码即可创建配置和填入 key。")
        print()

    relocked = V.exists(data_dir) and bridge.locked
    port = args.port or (getattr(bridge, "_port_hint", 8989) if not bridge.locked else 8989)
    httpd = srv.serve(bridge, host=args.host or "127.0.0.1", port=port)

    print(BANNER)
    print(f"  listen  : http://{args.host or '127.0.0.1'}:{port}")
    print(f"  crypto  : {crypto_backend.backend_name()}")
    print()
    if bridge.locked:
        print("  [锁住状态] 下游调用会得到 503, 直到在控制面板解锁。")
        print(f"  控制面板 : http://{args.host or '127.0.0.1'}:{port}/admin")
        print("             (浏览器打开 -> 输入主密码解锁 / 首次也在这里创建配置、填 key)")
        if relocked:
            print("             (也可以重启动时带 --password <主密码> 直接解锁)")
    else:
        print(f"  api base: http://{args.host or '127.0.0.1'}:{port}/v1")
        print(f"  proxy   : http://{args.host or '127.0.0.1'}:{port}/proxy/<channel_id>/")
        print(f"  panels  : http://{args.host or '127.0.0.1'}:{port}/admin   admin token: {admin_token}")
    print()
    print("Ctrl+C for graceful shutdown (flushes usage db to the USB folder)")
    try:
        httpd.serve_forever(poll_interval=0.5)
    except KeyboardInterrupt:
        print()
        print("flushing ...")
        httpd.server_close()
        bridge.usage.close()
        if bridge.vault is not None:
            bridge.vault.save()
        print("stopped, nothing left on the system.")
    return 0


# ------------------------------------------------------------------ misc
def cmd_passwd(args: argparse.Namespace) -> int:
    data_dir = args.data_dir or default_data_dir()
    if not V.exists(data_dir):
        print("config.vault not found")
        return 1
    old = args.old_password or ask_password()
    vault = V.load(data_dir, old)
    new = args.password or ask_password_twice()
    V.change_password(vault, new)
    print("master password changed.")
    return 0


def cmd_add_channel(args: argparse.Namespace) -> int:
    data_dir = args.data_dir or default_data_dir()
    password = args.password or ask_password()
    vault = V.load(data_dir, password)
    ch = vault.add_channel(
        {
            "protocol": args.protocol,
            "base_url": args.base_url,
            "api_key": args.api_key,
            "models": args.models,
            "weight": args.weight,
            "priority": args.priority,
        }
    )
    print(f"channel added: {ch['id']} ({args.protocol} -> {args.base_url})")
    return 0


def cmd_add_key(args: argparse.Namespace) -> int:
    data_dir = args.data_dir or default_data_dir()
    password = args.password or ask_password()
    vault = V.load(data_dir, password)
    key = vault.add_downstream_key(args.name, args.ttl)
    print(f"new downstream key: {key['key']}")
    return 0


def main() -> int:
    common = argparse.ArgumentParser(add_help=False)
    common.add_argument("--data-dir", default=None, help="USB data dir (default: ./bridge next to program)")
    common.add_argument("--password", default=None, help="non-interactive password")
    ap = argparse.ArgumentParser(
        prog="bridge", description="USB token gateway", parents=[common]
    )
    sub = ap.add_subparsers(dest="cmd", required=True)

    sp = sub.add_parser("init", parents=[common], help="create encrypted config")
    sp.add_argument("--seed", default=None, help="optional plaintext seed.json (delete it after import)")
    sp.add_argument("--force", action="store_true")
    sp.set_defaults(fn=cmd_init)

    sp = sub.add_parser("run", parents=[common], help="unlock and serve")
    sp.add_argument("--port", type=int, default=None)
    sp.add_argument("--host", default="127.0.0.1")
    sp.set_defaults(fn=cmd_run)

    sp = sub.add_parser("passwd", parents=[common], help="change master password")
    sp.add_argument("--old-password", default=None)
    sp.set_defaults(fn=cmd_passwd)

    sp = sub.add_parser("add-channel", parents=[common], help="add upstream channel")
    sp.add_argument("--protocol", default="openai", choices=V.SUPPORTED_PROTOCOLS)
    sp.add_argument("--base-url", required=True)
    sp.add_argument("--api-key", required=True)
    sp.add_argument("--models", default="*")
    sp.add_argument("--weight", type=int, default=10)
    sp.add_argument("--priority", type=int, default=0)
    sp.set_defaults(fn=cmd_add_channel)

    sp = sub.add_parser("add-key", parents=[common], help="generate downstream key")
    sp.add_argument("--name", default="")
    sp.add_argument("--ttl", type=float, default=0.0)
    sp.set_defaults(fn=cmd_add_key)

    args = ap.parse_args()
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s %(levelname)-7s %(message)s",
        datefmt="%H:%M:%S",
    )
    return args.fn(args) or 0


if __name__ == "__main__":
    sys.exit(main())
