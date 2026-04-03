from __future__ import annotations

import asyncio
import time

import httpx

from app.config import settings
from app.key_manager import key_manager
from app.proxy import get_client

_scheduler_task: asyncio.Task | None = None
_scheduler_stop: asyncio.Event | None = None
_scheduler_wake: asyncio.Event | None = None


def _truncate_message(message: str, *, limit: int = 240) -> str:
    message = " ".join(message.split())
    if len(message) <= limit:
        return message
    return message[: limit - 3] + "..."


def _extract_response_message(resp: httpx.Response) -> str:
    try:
        payload = resp.json()
    except ValueError:
        payload = None

    if isinstance(payload, dict):
        for key in ("detail", "title", "error", "message"):
            value = payload.get(key)
            if isinstance(value, str) and value.strip():
                return _truncate_message(value)
        return _truncate_message(str(payload))

    text = resp.text.strip()
    if text:
        return _truncate_message(text)
    return f"HTTP {resp.status_code}"


def _is_auth_failure(status_code: int, message: str) -> bool:
    if status_code == 401:
        return True
    if status_code != 403:
        return False

    lowered = message.lower()
    return any(token in lowered for token in ("authorization", "authentication", "api key", "invalid key"))


async def _run_probe_request(raw_key: str) -> tuple[str, str, int | None, int]:
    started = time.perf_counter()
    url = f"{settings.UPSTREAM_BASE_URL}/v1/chat/completions"
    headers = {
        "Authorization": f"Bearer {raw_key}",
        "Content-Type": "application/json",
        "Accept": "application/json",
    }
    payload = {
        "model": settings.KEY_HEALTHCHECK_MODEL,
        "messages": [{"role": "user", "content": "ping"}],
        "max_tokens": 1,
        "stream": False,
        "temperature": 0,
    }

    try:
        resp = await get_client().post(
            url,
            headers=headers,
            json=payload,
            timeout=settings.KEY_HEALTHCHECK_TIMEOUT,
        )
    except httpx.HTTPError as exc:
        latency_ms = int((time.perf_counter() - started) * 1000)
        return "error", _truncate_message(str(exc)), None, latency_ms

    latency_ms = int((time.perf_counter() - started) * 1000)
    if resp.status_code == 200:
        return "healthy", "Probe succeeded", resp.status_code, latency_ms
    message = _extract_response_message(resp)
    if _is_auth_failure(resp.status_code, message):
        return "invalid", message, resp.status_code, latency_ms
    return "error", message, resp.status_code, latency_ms


async def probe_key_by_name(
    name: str,
    *,
    trigger: str = "manual",
    allow_skip_busy: bool = False,
) -> dict:
    try:
        target = key_manager.start_probe(name)
    except RuntimeError:
        if not allow_skip_busy:
            raise
        current = key_manager.get_key(name)
        return {
            "name": name,
            "ok": False,
            "status": "skipped",
            "message": f"Key '{name}' is already being checked",
            "key": current,
        }

    status, message, status_code, latency_ms = await _run_probe_request(target["key"])
    try:
        key_state = key_manager.finish_probe(
            name,
            target["key"],
            status=status,
            message=message,
            status_code=status_code,
            latency_ms=latency_ms,
            trigger=trigger,
        )
    except KeyError:
        return {
            "name": name,
            "ok": False,
            "status": "skipped",
            "status_code": status_code,
            "message": f"Key '{name}' was removed during probe",
            "latency_ms": latency_ms,
            "key": None,
        }
    return {
        "name": name,
        "ok": status == "healthy",
        "status": status,
        "status_code": status_code,
        "message": message,
        "latency_ms": latency_ms,
        "key": key_state,
    }


async def probe_all_keys(
    *,
    trigger: str = "manual",
    enabled_only: bool = True,
) -> dict:
    targets = key_manager.list_probe_targets(enabled_only=enabled_only)
    results: list[dict] = []
    healthy_count = 0
    invalid_count = 0
    error_count = 0
    skipped_count = 0

    for target in targets:
        try:
            result = await probe_key_by_name(
                target["name"],
                trigger=trigger,
                allow_skip_busy=True,
            )
        except KeyError:
            continue
        results.append(result)

        if result["status"] == "healthy":
            healthy_count += 1
        elif result["status"] == "invalid":
            invalid_count += 1
        elif result["status"] == "error":
            error_count += 1
        elif result["status"] == "skipped":
            skipped_count += 1

    return {
        "message": "Key probe completed",
        "healthy_count": healthy_count,
        "invalid_count": invalid_count,
        "error_count": error_count,
        "skipped_count": skipped_count,
        "results": results,
    }


async def _scheduler_loop() -> None:
    while True:
        if _scheduler_stop is not None and _scheduler_stop.is_set():
            return

        if settings.KEY_HEALTHCHECK_ENABLED:
            try:
                await probe_all_keys(trigger="auto", enabled_only=True)
            except Exception as exc:
                print(f"[KeyHealth] scheduled probe failed: {exc}")
            wait_seconds = max(30, settings.KEY_HEALTHCHECK_INTERVAL_SECONDS)
        else:
            wait_seconds = 5

        if _scheduler_wake is None:
            return

        try:
            await asyncio.wait_for(_scheduler_wake.wait(), timeout=wait_seconds)
        except asyncio.TimeoutError:
            continue
        _scheduler_wake.clear()


async def start_key_probe_scheduler() -> None:
    global _scheduler_task, _scheduler_stop, _scheduler_wake
    if _scheduler_task is not None and not _scheduler_task.done():
        return
    _scheduler_stop = asyncio.Event()
    _scheduler_wake = asyncio.Event()
    _scheduler_task = asyncio.create_task(_scheduler_loop())


async def stop_key_probe_scheduler() -> None:
    global _scheduler_task, _scheduler_stop, _scheduler_wake
    if _scheduler_stop is not None:
        _scheduler_stop.set()
    if _scheduler_wake is not None:
        _scheduler_wake.set()
    if _scheduler_task is not None:
        await _scheduler_task
    _scheduler_task = None
    _scheduler_stop = None
    _scheduler_wake = None


def wake_key_probe_scheduler() -> None:
    if _scheduler_wake is not None:
        _scheduler_wake.set()
