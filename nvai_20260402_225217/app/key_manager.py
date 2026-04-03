from __future__ import annotations

import json
import threading
import time
from dataclasses import dataclass
from pathlib import Path

from app.config import settings


def _mask_key(key: str) -> str:
    if len(key) <= 12:
        return key[:4] + "****"
    return key[:8] + "****" + key[-4:]


@dataclass
class KeyState:
    key: str
    name: str
    enabled: bool = True
    success_count: int = 0
    fail_count: int = 0
    consecutive_failures: int = 0
    last_fail_time: float = 0.0
    is_healthy: bool = True
    is_invalid: bool = False
    probe_status: str = "never_checked"
    probe_message: str = ""
    last_probe_time: float = 0.0
    last_probe_status_code: int | None = None
    last_probe_latency_ms: int | None = None
    last_probe_trigger: str | None = None
    is_checking: bool = False


class KeyManager:
    def __init__(self) -> None:
        self._keys: list[KeyState] = []
        self._index: int = 0
        self._lock = threading.Lock()

    # ── load / save ──────────────────────────────────

    def load(self, path: str | Path | None = None) -> None:
        path = Path(path or settings.KEYS_FILE)
        with open(path) as f:
            data = json.load(f)
        with self._lock:
            existing = {k.key: k for k in self._keys}
            new_keys: list[KeyState] = []
            for item in data["keys"]:
                if item["key"] in existing:
                    state = existing[item["key"]]
                    state.name = item.get("name", state.name)
                    state.enabled = item.get("enabled", True)
                    new_keys.append(state)
                else:
                    new_keys.append(
                        KeyState(
                            key=item["key"],
                            name=item.get("name", item["key"][:12]),
                            enabled=item.get("enabled", True),
                        )
                    )
            self._keys = new_keys
            self._index = 0

    def _save(self) -> None:
        data = {
            "keys": [
                {"key": ks.key, "name": ks.name, "enabled": ks.enabled}
                for ks in self._keys
            ]
        }
        path = Path(settings.KEYS_FILE)
        with open(path, "w") as f:
            json.dump(data, f, indent=2, ensure_ascii=False)
            f.write("\n")

    def reload(self) -> None:
        self.load()

    # ── round-robin / health ─────────────────────────

    def _is_available(self, ks: KeyState) -> bool:
        if not ks.enabled:
            return False
        if ks.is_invalid:
            return False
        if not ks.is_healthy:
            if time.time() - ks.last_fail_time > settings.KEY_COOLDOWN_SECONDS:
                ks.is_healthy = True
                ks.consecutive_failures = 0
                return True
            return False
        return True

    def _find_key_locked(self, name: str) -> KeyState | None:
        for ks in self._keys:
            if ks.name == name:
                return ks
        return None

    def _reset_runtime_state(self, ks: KeyState, *, clear_probe: bool) -> None:
        ks.success_count = 0
        ks.fail_count = 0
        ks.consecutive_failures = 0
        ks.last_fail_time = 0.0
        ks.is_healthy = True
        if clear_probe:
            ks.is_invalid = False
            ks.probe_status = "never_checked"
            ks.probe_message = ""
            ks.last_probe_time = 0.0
            ks.last_probe_status_code = None
            ks.last_probe_latency_ms = None
            ks.last_probe_trigger = None
            ks.is_checking = False

    def _serialize_key(self, ks: KeyState) -> dict:
        self._is_available(ks)
        effective_probe_status = (
            "checking"
            if ks.is_checking
            else "invalid"
            if ks.is_invalid
            else ks.probe_status
        )
        return {
            "name": ks.name,
            "key_masked": _mask_key(ks.key),
            "enabled": ks.enabled,
            "is_healthy": ks.is_healthy,
            "is_invalid": ks.is_invalid,
            "success_count": ks.success_count,
            "fail_count": ks.fail_count,
            "consecutive_failures": ks.consecutive_failures,
            "probe_status": ks.probe_status,
            "effective_probe_status": effective_probe_status,
            "probe_message": ks.probe_message,
            "last_probe_time": ks.last_probe_time,
            "last_probe_status_code": ks.last_probe_status_code,
            "last_probe_latency_ms": ks.last_probe_latency_ms,
            "last_probe_trigger": ks.last_probe_trigger,
            "is_checking": ks.is_checking,
        }

    def next_key(self) -> KeyState:
        with self._lock:
            n = len(self._keys)
            if n == 0:
                raise RuntimeError("No API keys configured — check keys.json")
            for _ in range(n):
                ks = self._keys[self._index % n]
                self._index += 1
                if self._is_available(ks):
                    return ks
            raise RuntimeError("All API keys are unhealthy or disabled")

    def report_success(self, ks: KeyState) -> None:
        with self._lock:
            ks.success_count += 1
            ks.consecutive_failures = 0

    def report_failure(self, ks: KeyState) -> None:
        with self._lock:
            ks.fail_count += 1
            ks.consecutive_failures += 1
            ks.last_fail_time = time.time()
            if ks.consecutive_failures >= settings.KEY_MAX_CONSECUTIVE_FAILURES:
                ks.is_healthy = False

    # ── stats ────────────────────────────────────────

    def get_stats(self) -> list[dict]:
        with self._lock:
            return [
                {
                    "name": item["name"],
                    "enabled": item["enabled"],
                    "is_healthy": item["is_healthy"],
                    "is_invalid": item["is_invalid"],
                    "success_count": item["success_count"],
                    "fail_count": item["fail_count"],
                    "consecutive_failures": item["consecutive_failures"],
                    "probe_status": item["probe_status"],
                    "effective_probe_status": item["effective_probe_status"],
                    "probe_message": item["probe_message"],
                    "last_probe_time": item["last_probe_time"],
                    "last_probe_status_code": item["last_probe_status_code"],
                    "last_probe_latency_ms": item["last_probe_latency_ms"],
                    "last_probe_trigger": item["last_probe_trigger"],
                    "is_checking": item["is_checking"],
                }
                for item in (self._serialize_key(ks) for ks in self._keys)
            ]

    # ── CRUD ─────────────────────────────────────────

    def get_all_keys(self) -> list[dict]:
        with self._lock:
            return [self._serialize_key(ks) for ks in self._keys]

    def get_key(self, name: str) -> dict:
        with self._lock:
            target = self._find_key_locked(name)
            if target is None:
                raise KeyError(f"Key '{name}' not found")
            return self._serialize_key(target)

    def list_probe_targets(self, *, enabled_only: bool = False) -> list[dict]:
        with self._lock:
            targets = []
            for ks in self._keys:
                if enabled_only and not ks.enabled:
                    continue
                targets.append({"name": ks.name, "key": ks.key, "enabled": ks.enabled})
            return targets

    def start_probe(self, name: str) -> dict:
        with self._lock:
            target = self._find_key_locked(name)
            if target is None:
                raise KeyError(f"Key '{name}' not found")
            if target.is_checking:
                raise RuntimeError(f"Key '{name}' is already being checked")
            target.is_checking = True
            return {"name": target.name, "key": target.key, "enabled": target.enabled}

    def finish_probe(
        self,
        name: str,
        expected_key: str,
        *,
        status: str,
        message: str,
        status_code: int | None,
        latency_ms: int | None,
        trigger: str,
    ) -> dict:
        with self._lock:
            target = self._find_key_locked(name)
            if target is None:
                raise KeyError(f"Key '{name}' not found")

            target.is_checking = False
            if target.key != expected_key:
                return self._serialize_key(target)

            now = time.time()
            target.probe_status = status
            target.probe_message = message
            target.last_probe_time = now
            target.last_probe_status_code = status_code
            target.last_probe_latency_ms = latency_ms
            target.last_probe_trigger = trigger

            if status == "healthy":
                target.is_invalid = False
                target.is_healthy = True
                target.consecutive_failures = 0
                target.last_fail_time = 0.0
            elif status == "invalid":
                target.is_invalid = True
                target.is_healthy = False
                target.last_fail_time = now

            return self._serialize_key(target)

    def add_key(self, key: str, name: str, enabled: bool = True) -> dict:
        with self._lock:
            for ks in self._keys:
                if ks.name == name:
                    raise ValueError(f"Key name '{name}' already exists")
                if ks.key == key:
                    raise ValueError("This API key already exists")
            ks = KeyState(key=key, name=name, enabled=enabled)
            self._keys.append(ks)
            self._save()
            return self._serialize_key(ks)

    def update_key(
        self,
        name: str,
        *,
        new_name: str | None = None,
        new_key: str | None = None,
        enabled: bool | None = None,
    ) -> dict:
        with self._lock:
            target = None
            for ks in self._keys:
                if ks.name == name:
                    target = ks
                    break
            if target is None:
                raise KeyError(f"Key '{name}' not found")
            if new_name is not None and new_name != name:
                for ks in self._keys:
                    if ks.name == new_name:
                        raise ValueError(f"Key name '{new_name}' already exists")
                target.name = new_name
            if new_key is not None:
                for ks in self._keys:
                    if ks is not target and ks.key == new_key:
                        raise ValueError("This API key already exists")
                if new_key != target.key:
                    target.key = new_key
                    self._reset_runtime_state(target, clear_probe=True)
            if enabled is not None:
                target.enabled = enabled
            self._save()
            return self._serialize_key(target)

    def remove_key(self, name: str) -> None:
        with self._lock:
            before = len(self._keys)
            self._keys = [ks for ks in self._keys if ks.name != name]
            if len(self._keys) == before:
                raise KeyError(f"Key '{name}' not found")
            self._index = 0
            self._save()

    def reset_stats(self, name: str) -> None:
        with self._lock:
            target = self._find_key_locked(name)
            if target is None:
                raise KeyError(f"Key '{name}' not found")
            self._reset_runtime_state(target, clear_probe=True)


key_manager = KeyManager()
