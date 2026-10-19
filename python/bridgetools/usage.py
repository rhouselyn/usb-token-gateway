# -*- coding: utf-8 -*-
"""Usage audit DB (SQLite) stored on the USB stick (DESIGN §4.3/§10)."""

from __future__ import annotations

import os
import sqlite3
import threading
import time

SCHEMA = """
CREATE TABLE IF NOT EXISTS requests (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    ts REAL NOT NULL,
    model TEXT,
    channel_id TEXT,
    status INTEGER,
    first_byte_ms REAL,
    prompt_tokens INTEGER DEFAULT 0,
    completion_tokens INTEGER DEFAULT 0,
    error TEXT DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_requests_ts ON requests(ts);
"""


class UsageDB:
    def __init__(self, data_dir: str):
        os.makedirs(data_dir, exist_ok=True)
        self.path = os.path.join(data_dir, "usage.db")
        self.lock = threading.Lock()
        self.conn = sqlite3.connect(self.path, check_same_thread=False)
        with self.lock, self.conn:
            self.conn.executescript(SCHEMA)

    def record(
        self,
        model: str,
        channel_id: str,
        status: int,
        first_byte_ms: float,
        prompt_tokens: int = 0,
        completion_tokens: int = 0,
        error: str = "",
    ) -> None:
        with self.lock, self.conn:
            self.conn.execute(
                "INSERT INTO requests (ts, model, channel_id, status, first_byte_ms,"
                " prompt_tokens, completion_tokens, error) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
                (time.time(), model, channel_id, status, first_byte_ms, prompt_tokens, completion_tokens, error[:200]),
            )

    def list_recent(self, limit: int = 200) -> list[dict]:
        with self.lock:
            cur = self.conn.execute(
                "SELECT ts, model, channel_id, status, first_byte_ms,"
                " prompt_tokens, completion_tokens, error FROM requests ORDER BY id DESC LIMIT ?",
                (limit,),
            )
            cols = [d[0] for d in cur.description]
            return [dict(zip(cols, row)) for row in cur.fetchall()]

    def summary(self) -> dict:
        day_start = time.time() - 86400
        with self.lock:
            cur = self.conn.execute(
                "SELECT COUNT(*),"
                " COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0),"
                " COALESCE(SUM(CASE WHEN status >= 400 OR status = 0 THEN 1 ELSE 0 END),0)"
                " FROM requests WHERE ts >= ?",
                (day_start,),
            )
            total, pin, pout, errs = cur.fetchone()
            by_channel = self.conn.execute(
                "SELECT channel_id, COUNT(*), COALESCE(SUM(prompt_tokens+completion_tokens),0)"
                " FROM requests WHERE ts >= ? GROUP BY channel_id ORDER BY COUNT(*) DESC",
                (day_start,),
            ).fetchall()
        return {
            "requests_24h": total,
            "tokens_24h": {"prompt": pin, "completion": pout},
            "errors_24h": errs,
            "by_channel": [{"channel": c or "-", "requests": n, "tokens": t} for c, n, t in by_channel],
        }

    def close(self) -> None:
        try:
            with self.lock:
                self.conn.close()
        except Exception:
            pass
