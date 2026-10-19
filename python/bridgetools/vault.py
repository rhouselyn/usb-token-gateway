# -*- coding: utf-8 -*-
"""Encrypted configuration vault for the USB bridge (DESIGN §5).

Everything sensitive (upstream api keys, downstream keys, channel list)
lives in a single encrypted file ``config.vault`` next to the program -- on
the USB stick.  It is only ever decrypted into memory at runtime.
"""

from __future__ import annotations

import json
import os
import secrets
from dataclasses import dataclass, field
from typing import Any

from . import crypto_backend

VAULT_FILENAME = "config.vault"
SUPPORTED_PROTOCOLS = ("openai", "anthropic", "gemini", "generic")


def new_downstream_key() -> str:
    return "sk-usb-" + secrets.token_urlsafe(24).replace("-", "").replace("_", "")[:32]


def new_admin_token() -> str:
    return "brg-" + secrets.token_urlsafe(16)


def default_policy() -> dict[str, Any]:
    return {
        "strategy": "failover",  # round_robin | weighted | failover | least_errors
        "retry_budget": 2,
        "timeout_first_byte_s": 20.0,
        "timeout_stream_idle_s": 120.0,
        "circuit": {
            "err_rate_threshold": 0.5,
            "min_samples": 10,
            "cooldown_s": 300,
        },
    }


@dataclass
class Vault:
    """In-memory representation of the encrypted config."""

    path: str
    data: dict[str, Any]
    master_password: bytes  # held in memory only while running

    # ------------------------------------------------------------ persistence
    def save(self) -> None:
        blob = crypto_backend.encrypt(self.master_password, json.dumps(self.data).encode())
        tmp = self.path + ".tmp"
        with open(tmp, "wb") as fh:
            fh.write(blob)
        os.replace(tmp, self.path)  # atomic on same volume

    # ------------------------------------------------------------ convenience
    @property
    def policy(self) -> dict[str, Any]:
        return self.data.setdefault("policy", default_policy())

    @property
    def channels(self) -> list[dict[str, Any]]:
        return self.data.setdefault("channels", [])

    @property
    def downstream_keys(self) -> list[dict[str, Any]]:
        return self.data.setdefault("downstream_keys", [])

    def add_channel(self, ch: dict[str, Any]) -> dict[str, Any]:
        ch = dict(ch)
        ch.setdefault("id", "ch-" + secrets.token_hex(4))
        ch.setdefault("enabled", True)
        ch.setdefault("weight", 10)
        ch.setdefault("priority", 0)
        ch.setdefault("max_concurrency", 8)
        ch.setdefault("auth_header", "bearer" if ch.get("protocol") == "openai" else "x-api-key")
        models = ch.get("models")
        if isinstance(models, str):
            ch["models"] = [m.strip() for m in models.split(",") if m.strip()]
        self.channels.append(ch)
        self.save()
        return ch

    def add_downstream_key(self, name: str = "", ttl_hours: float = 0.0) -> dict[str, Any]:
        dk = {
            "key": new_downstream_key(),
            "name": name or "key-" + secrets.token_hex(2),
            "enabled": True,
            "rate_limit_rpm": 0,  # 0 = unlimited (MVP)
            "ttl_hours": ttl_hours,  # 0 = never expires
            "created": float(__import__("time").time()),
        }
        self.downstream_keys.append(dk)
        self.save()
        return dk

    def check_downstream_key(self, key: str) -> dict[str, Any] | None:
        import time

        for dk in self.downstream_keys:
            if not dk.get("enabled") or dk.get("key") != key:
                continue
            ttl = dk.get("ttl_hours") or 0
            if ttl and (time.time() - dk.get("created", 0)) > ttl * 3600:
                return None
            return dk
        return None


# ---------------------------------------------------------------- file helpers
def vault_path(data_dir: str) -> str:
    return os.path.join(data_dir, VAULT_FILENAME)


def exists(data_dir: str) -> bool:
    return os.path.isfile(vault_path(data_dir))


def default_payload() -> dict[str, Any]:
    return {
        "port": 8989,
        "policy": default_policy(),
        "channels": [],
        "downstream_keys": [],
        "admin_token": new_admin_token(),
    }


def create(
    data_dir: str,
    master_password: str,
    seed_payload: dict[str, Any] | None = None,
) -> tuple[Vault, str]:
    """Create a brand-new vault.  Returns (vault, downstream_key_plaintext)."""
    os.makedirs(data_dir, exist_ok=True)
    payload = default_payload()
    if seed_payload:
        for key, value in seed_payload.items():
            if key in ("channels", "downstream_keys", "policy", "port"):
                payload[key] = value
    dk = {
        "key": new_downstream_key(),
        "name": "primary",
        "enabled": True,
        "rate_limit_rpm": 0,
        "ttl_hours": 0,
        "created": float(__import__("time").time()),
    }
    payload["downstream_keys"] = [
        *payload.get("downstream_keys", []),
        dk,
    ]
    vault = Vault(
        path=vault_path(data_dir),
        data=payload,
        master_password=master_password.encode(),
    )
    vault.save()
    return vault, dk["key"]


def load(data_dir: str, master_password: str) -> Vault:
    with open(vault_path(data_dir), "rb") as fh:
        blob = fh.read()
    raw = crypto_backend.decrypt(master_password.encode(), blob)
    payload = json.loads(raw.decode())
    payload.setdefault("policy", default_policy())
    return Vault(
        path=vault_path(data_dir),
        data=payload,
        master_password=master_password.encode(),
    )


def change_password(vault: Vault, new_password: str) -> None:
    vault.master_password = new_password.encode()
    vault.save()


def import_seed(data_dir: str, seed_file: str) -> dict[str, Any]:
    """Read an unencrypted seed json (dev convenience) and return its payload."""
    with open(seed_file, encoding="utf-8") as fh:
        return json.load(fh)
