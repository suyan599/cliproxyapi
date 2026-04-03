from __future__ import annotations

import asyncio
import logging
from collections.abc import AsyncIterator

import httpx
from fastapi import HTTPException

from app.config import settings
from app.key_manager import KeyState, key_manager

logger = logging.getLogger(__name__)

_client: httpx.AsyncClient | None = None

# 指数退避基础延迟（秒）
_BACKOFF_BASE = 0.5
_BACKOFF_MAX = 8.0

# 上游状态码 → 对外展示的标准错误消息
_ERROR_MAP: dict[int, tuple[int, str]] = {
    400: (400, "Invalid request parameters"),
    401: (500, "Upstream authentication failed — please contact admin"),
    403: (403, "Access denied for the requested model or resource"),
    404: (404, "The requested model or endpoint does not exist"),
    409: (409, "Request conflict — please retry"),
    413: (413, "Request payload too large"),
    422: (422, "Unprocessable request — check your input format"),
    429: (429, "Rate limit exceeded — please slow down or retry later"),
}


def _backoff_delay(attempt: int) -> float:
    """指数退避: 0.5s, 1s, 2s, 4s ... 最大 8s"""
    return min(_BACKOFF_BASE * (2 ** attempt), _BACKOFF_MAX)


def _sanitized_error(status_code: int, upstream_body: str = "") -> HTTPException:
    """将上游错误转为标准化错误，不泄露上游信息。原始错误仅记录日志。"""
    if upstream_body:
        logger.warning("Upstream %d: %s", status_code, upstream_body[:500])

    if status_code in _ERROR_MAP:
        code, msg = _ERROR_MAP[status_code]
        return HTTPException(status_code=code, detail=msg)

    if status_code >= 500:
        return HTTPException(
            status_code=502,
            detail="Upstream service temporarily unavailable — please retry later",
        )

    return HTTPException(
        status_code=status_code,
        detail="Request failed — please check your input and retry",
    )


def get_client() -> httpx.AsyncClient:
    global _client
    if _client is None or _client.is_closed:
        _client = httpx.AsyncClient(
            proxy=settings.HTTP_PROXY,
            timeout=httpx.Timeout(settings.UPSTREAM_TIMEOUT, connect=10.0),
        )
    return _client


async def close_client() -> None:
    global _client
    if _client and not _client.is_closed:
        await _client.close()
        _client = None


async def rebuild_client() -> None:
    """Close and recreate the httpx client (e.g. after proxy change)."""
    await close_client()
    get_client()


async def forward_request(
    method: str,
    path: str,
    body: bytes,
) -> dict:
    """Non-streaming request with exponential backoff retry & key failover."""
    url = f"{settings.UPSTREAM_BASE_URL}{path}"
    last_error: Exception | None = None

    for attempt in range(settings.MAX_RETRIES):
        if attempt > 0:
            await asyncio.sleep(_backoff_delay(attempt - 1))

        try:
            ks = key_manager.next_key()
        except RuntimeError as e:
            raise HTTPException(status_code=503, detail=str(e)) from e

        headers = {
            "Authorization": f"Bearer {ks.key}",
            "Content-Type": "application/json",
            "Accept": "application/json",
        }

        client = get_client()
        try:
            resp = await client.request(method, url, headers=headers, content=body)
        except httpx.HTTPError as e:
            key_manager.report_failure(ks)
            last_error = e
            continue

        if resp.status_code >= 500:
            key_manager.report_failure(ks)
            last_error = _sanitized_error(resp.status_code, resp.text)
            continue

        if resp.status_code == 429:
            key_manager.report_failure(ks)
            last_error = _sanitized_error(429, resp.text)
            continue

        if resp.status_code >= 400:
            key_manager.report_failure(ks)
            raise _sanitized_error(resp.status_code, resp.text)

        key_manager.report_success(ks)
        return resp.json()

    if isinstance(last_error, HTTPException):
        raise last_error
    raise HTTPException(status_code=502, detail="Service temporarily unavailable — please retry later")


async def forward_stream(
    method: str,
    path: str,
    body: bytes,
) -> AsyncIterator[bytes]:
    """Streaming request with exponential backoff retry & key failover.

    在连接建立阶段进行重试，一旦开始接收数据则不再重试。
    """
    url = f"{settings.UPSTREAM_BASE_URL}{path}"
    last_error: Exception | None = None

    for attempt in range(settings.MAX_RETRIES):
        if attempt > 0:
            await asyncio.sleep(_backoff_delay(attempt - 1))

        try:
            ks = key_manager.next_key()
        except RuntimeError as e:
            raise HTTPException(status_code=503, detail=str(e)) from e

        headers = {
            "Authorization": f"Bearer {ks.key}",
            "Content-Type": "application/json",
            "Accept": "text/event-stream",
        }

        client = get_client()
        try:
            resp = await client.send(
                client.build_request(method, url, headers=headers, content=body),
                stream=True,
            )
        except httpx.HTTPError as e:
            key_manager.report_failure(ks)
            last_error = e
            continue

        if resp.status_code == 429 or resp.status_code >= 500:
            await resp.aclose()
            key_manager.report_failure(ks)
            last_error = _sanitized_error(resp.status_code)
            continue

        if resp.status_code >= 400:
            body_text = (await resp.aread()).decode()
            await resp.aclose()
            key_manager.report_failure(ks)
            raise _sanitized_error(resp.status_code, body_text)

        # 连接成功，开始流式传输（不再重试）
        key_manager.report_success(ks)

        async def _yield_lines(r: httpx.Response, k: KeyState) -> AsyncIterator[bytes]:
            try:
                async for line in r.aiter_lines():
                    yield (line + "\n").encode()
            except httpx.HTTPError:
                key_manager.report_failure(k)
                raise
            finally:
                await r.aclose()

        return _yield_lines(resp, ks)

    if isinstance(last_error, HTTPException):
        raise last_error
    raise HTTPException(status_code=502, detail="Service temporarily unavailable — please retry later")
