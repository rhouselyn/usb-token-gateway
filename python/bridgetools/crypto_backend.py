# -*- coding: utf-8 -*-
"""Crypto backends for the USB bridge vault.

Two backends, chosen automatically at import time:

1) argon2 backend (preferred):        Argon2id KDF  (cryptography >= 42) + AES-256-GCM
2) scrypt  backend (stdlib fallback): scrypt KDF (hashlib) + SHA256-CTR keystream
                                      + HMAC-SHA256 (encrypt-then-MAC)

File format (little endian ints where noted):

    magic   4B   b"BVLT"
    version 1B   0x01
    kdf_id  1B   0x01 argon2 / 0x02 scrypt
    salt    16B  random
    body    rest backend-specific:
        argon2: nonce 12B + AESGCM ciphertext(with tag)
        scrypt: nonce 16B + ct + mac 32B

The master password is never stored or logged anywhere.
"""

from __future__ import annotations

import hashlib
import hmac
import secrets
import struct

MAGIC = b"BVLT"
VERSION = 1
KDF_ARGON2 = 0x01
KDF_SCRYPT = 0x02

# ---------------------------------------------------------------- argon2 probe
try:  # pragma: no cover
    from cryptography.hazmat.primitives.kdf.argon2 import Argon2id

    # Probe the API shape with cheap parameters.
    Argon2id(
        salt=b"0123456789abcdef",
        length=32,
        iterations=1,
        lanes=1,
        memory_cost=1024,
    ).derive(b"probe")
    _HAS_ARGON2 = True
except Exception:  # noqa: BLE001 - any failure means "use scrypt"
    _HAS_ARGON2 = False
    Argon2id = None  # type: ignore[assignment]

try:  # pragma: no cover
    from cryptography.hazmat.primitives.ciphers.aead import AESGCM

    AESGCM(b"\0" * 32).encrypt(b"\x00" * 12, b"probe", None)
    _HAS_AESGCM = True
except Exception:  # noqa: BLE001
    _HAS_AESGCM = False
    AESGCM = None  # type: ignore[assignment]


def backend_name() -> str:
    if _HAS_ARGON2 and _HAS_AESGCM:
        return "argon2id+aesgcm"
    return "scrypt+hmac(sha256-ctr)"


# ---------------------------------------------------------------- KDF wrappers
def _derive_argon2(password: bytes, salt: bytes) -> bytes:
    kdf = Argon2id(
        salt=salt,
        length=32,
        iterations=3,
        lanes=1,
        memory_cost=64 * 1024,  # 64 MiB, per DESIGN §5.2
    )
    return kdf.derive(password)


def _derive_scrypt(password: bytes, salt: bytes) -> bytes:
    # n=32MiB state, memory-hard; stdlib only.
    return hashlib.scrypt(password, salt=salt, n=2**15, r=8, p=1, maxmem=256 * 1024 * 1024, dklen=64)


def derive_keys(password: bytes, salt: bytes, kdf_id: int) -> tuple[bytes, bytes]:
    """Return (enc_key, mac_key).  enc_key 32B, mac_key 32B."""
    if kdf_id == KDF_ARGON2:
        data = _derive_argon2(password, salt)
        return data[:32], hashlib.sha256(data + b"mac").digest()
    if kdf_id == KDF_SCRYPT:
        data = _derive_scrypt(password, salt)
        return data[:32], data[32:64]
    raise ValueError(f"unknown kdf_id {kdf_id}")


# ---------------------------------------------------------------- primitives
def _keystream_xor(key: bytes, nonce: bytes, data: bytes) -> bytes:
    """SHA256-based CTR-style keystream (stdlib fallback)."""
    out = bytearray(len(data))
    block = b""
    counter = 0
    idx = 0
    while idx < len(data):
        block = hashlib.sha256(key + nonce + counter.to_bytes(8, "little")).digest()
        chunk = data[idx : idx + 32]
        out[idx : idx + 32] = bytes(a ^ b for a, b in zip(chunk, block))
        idx += 32
        counter += 1
    return bytes(out)


def _encrypt_argon2(enc_key: bytes, mac_key: bytes, plaintext: bytes) -> bytes:
    nonce = secrets.token_bytes(12)
    ct = AESGCM(enc_key).encrypt(nonce, plaintext, b"BVLT")
    return nonce + ct


def _decrypt_argon2(enc_key: bytes, mac_key: bytes, body: bytes) -> bytes:
    nonce, ct = body[:12], body[12:]
    return AESGCM(enc_key).decrypt(nonce, ct, b"BVLT")


def _encrypt_scrypt(enc_key: bytes, mac_key: bytes, plaintext: bytes) -> bytes:
    nonce = secrets.token_bytes(16)
    ct = _keystream_xor(enc_key, nonce, plaintext)
    mac = hmac.new(mac_key, ct, hashlib.sha256).digest()
    return nonce + ct + mac


def _decrypt_scrypt(enc_key: bytes, mac_key: bytes, body: bytes) -> bytes:
    nonce, ct, mac = body[:16], body[16:-32], body[-32:]
    expect = hmac.new(mac_key, ct, hashlib.sha256).digest()
    if not hmac.compare_digest(mac, expect):
        raise ValueError("vault authentication failed (wrong password or corrupted file)")
    return _keystream_xor(enc_key, nonce, ct)


# ---------------------------------------------------------------- public API
def encrypt(password: bytes, plaintext: bytes, salt: bytes | None = None) -> bytes:
    salt = salt or secrets.token_bytes(16)
    kdf_id = KDF_ARGON2 if (_HAS_ARGON2 and _HAS_AESGCM) else KDF_SCRYPT
    enc_key, mac_key = derive_keys(password, salt, kdf_id)
    if kdf_id == KDF_ARGON2:
        body = _encrypt_argon2(enc_key, mac_key, plaintext)
    else:
        body = _encrypt_scrypt(enc_key, mac_key, plaintext)
    head = MAGIC + struct.pack("<B", VERSION) + struct.pack("<B", kdf_id) + salt
    return head + body


def decrypt(password: bytes, blob: bytes) -> bytes:
    if len(blob) < 4 + 1 + 1 + 16:
        raise ValueError("vault file too short")
    if blob[:4] != MAGIC:
        raise ValueError("not a bridge vault file")
    version = blob[4]
    if version != VERSION:
        raise ValueError(f"unsupported vault version {version}")
    kdf_id = blob[5]
    salt = blob[6:22]
    body = blob[22:]
    enc_key, mac_key = derive_keys(password, salt, kdf_id)
    if kdf_id == KDF_ARGON2:
        return _decrypt_argon2(enc_key, mac_key, body)
    return _decrypt_scrypt(enc_key, mac_key, body)
