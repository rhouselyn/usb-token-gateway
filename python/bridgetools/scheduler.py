# -*- coding: utf-8 -*-
"""Channel scheduler (DESIGN §7): strategies, health tracking, circuit breaker."""

from __future__ import annotations

import itertools
import threading
import time
from collections import defaultdict, deque
from typing import Any

import fnmatch


class ChannelHealth:
    """Sliding-window statistics for one channel."""

    def __init__(self, min_samples: int = 10, err_threshold: float = 0.5, cooldown_s: float = 300.0):
        self.window: deque[tuple[float, bool, float]] = deque(maxlen=50)  # (ts, ok, first_byte_ms)
        self.min_samples = min_samples
        self.err_threshold = err_threshold
        self.cooldown_s = cooldown_s
        self.cooldown_until = 0.0
        self.half_open = False
        self.retry_earliest: dict[str, float] = defaultdict(float)  # scope: 429 recovery
        self.lock = threading.Lock()

    # ------------------------------------------------------------------ record
    def record(self, ok: bool, first_byte_ms: float = 0.0, status: int = 0) -> None:
        now = time.time()
        with self.lock:
            self.window.append((now, ok, first_byte_ms))
            if ok:
                self.cooldown_until = 0.0
                self.half_open = False
                return
            # circuit breaker check over window
            recent = [w for w in self.window if now - w[0] <= 300]
            if len(recent) >= self.min_samples and recent:
                rate = sum(1 for _, is_ok, _ in recent if not is_ok) / len(recent)
                if rate >= self.err_threshold and not self.half_open:
                    self.cooldown_until = now + self.cooldown_s
                    self.half_open = True

    def record_429(self, retry_after_s: float) -> None:
        now = time.time()
        with self.lock:
            self.window.append((now, False, 0.0))
            self.retry_earliest["*"] = max(self.retry_earliest.get("*", 0.0), now + retry_after_s)
            self.cooldown_until = max(self.cooldown_until, now + retry_after_s)

    # ------------------------------------------------------------------ queries
    def is_open(self) -> bool:
        now = time.time()
        with self.lock:
            if self.cooldown_until > now:
                return True
            self.cooldown_until = 0.0
            return False

    def err_rate(self) -> float:
        now = time.time()
        with self.lock:
            recent = [w for w in self.window if now - w[0] <= 300]
            if not recent:
                return 0.0
            return sum(1 for _, ok, _ in recent if not ok) / len(recent)


class Scheduler:
    """Chooses a channel for a model request according to the configured policy."""

    def __init__(self, channels: list[dict[str, Any]], policy: dict[str, Any]):
        self._channels = channels
        self._rr = itertools.count()
        self.lock = threading.Lock()
        circ = policy.get("circuit", {})
        self.health: dict[str, ChannelHealth] = {}
        self.strategy = policy.get("strategy", "failover")
        self.retry_budget = int(policy.get("retry_budget", 2))
        for ch in channels:
            self.health[ch["id"]] = ChannelHealth(
                min_samples=int(circ.get("min_samples", 10)),
                err_threshold=float(circ.get("err_rate_threshold", 0.5)),
                cooldown_s=float(circ.get("cooldown_s", 300)),
            )

    # ------------------------------------------------------------------ helpers
    def _enabled(self, ch: dict[str, Any]) -> bool:
        return ch.get("enabled", True) and not self.health[ch["id"]].is_open()

    def candidates(self, model: str) -> list[dict[str, Any]]:
        """Ordered healthy candidate list for this model."""
        fits = [c for c in self._channels if self._matches(c, model) and self._enabled(c)]
        if self.strategy == "round_robin":
            n = next(self._rr)
            return fits[n % len(fits) :] + fits[: n % len(fits)] if fits else []
        if self.strategy == "weighted":
            pool: list[dict[str, Any]] = []
            for c in fits:
                pool += [c] * max(1, int(c.get("weight", 10)))
            # dedupe but keep weighted order via stable random start
            if not pool:
                return []
            import random

            start = random.randrange(len(pool))
            seen, out = set(), []
            for c in pool[start:] + pool[:start]:
                if id(c) not in seen:
                    seen.add(id(c))
                    out.append(c)
            return out
        if self.strategy == "least_errors":
            return sorted(
                fits,
                key=lambda c: (
                    self.health[c["id"]].err_rate(),
                    c.get("priority", 0),
                ),
            )
        # failover (default): priority order
        return sorted(fits, key=lambda c: c.get("priority", 0))

    def all_channels(self) -> list[dict[str, Any]]:
        return list(self._channels)

    def get(self, channel_id: str) -> dict[str, Any] | None:
        for ch in self._channels:
            if ch["id"] == channel_id:
                return ch
        return None

    def refresh(self, channels: list[dict[str, Any]], policy: dict[str, Any]) -> None:
        with self.lock:
            self._channels = channels
            self.strategy = policy.get("strategy", "failover")
            self.retry_budget = int(policy.get("retry_budget", 2))
            ids = {c["id"] for c in channels}
            for cid in list(self.health):
                if cid not in ids:
                    self.health.pop(cid, None)
            for c in channels:
                if c["id"] not in self.health:
                    self.health[c["id"]] = ChannelHealth()

    # ------------------------------------------------------------------ reports
    def report(self, channel_id: str, ok: bool, first_byte_ms: float = 0.0, status: int = 0, retry_after: float = 0.0) -> None:
        if channel_id not in self.health:
            return
        if status == 429:
            self.health[channel_id].record_429(retry_after or 30.0)
        else:
            self.health[channel_id].record(ok, first_byte_ms, status)

    # ------------------------------------------------------------------ static
    @staticmethod
    def _matches(channel: dict[str, Any], model: str) -> bool:
        pats = channel.get("models") or ["*"]
        return any(fnmatch.fnmatch(model, pat) or pat == model for pat in pats)
