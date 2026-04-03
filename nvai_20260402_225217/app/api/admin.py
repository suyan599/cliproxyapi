from __future__ import annotations

from pydantic import BaseModel, Field
from fastapi import APIRouter, HTTPException

from app.config import settings
from app.deps import RequireAdmin
from app.key_health import probe_all_keys, probe_key_by_name, wake_key_probe_scheduler
from app.key_manager import key_manager
from app.model_manager import model_manager
from app.proxy import rebuild_client

router = APIRouter(prefix="/admin", dependencies=[RequireAdmin], tags=["admin"])


# ── Stats ────────────────────────────────────────────

@router.get("/stats")
async def get_stats():
    return {"keys": key_manager.get_stats()}


# ── Key CRUD ─────────────────────────────────────────

@router.get("/keys")
async def list_keys():
    return {"keys": key_manager.get_all_keys()}


class KeyCreateBody(BaseModel):
    key: str
    name: str
    enabled: bool = True


@router.post("/keys", status_code=201)
async def add_key(body: KeyCreateBody):
    try:
        result = key_manager.add_key(body.key, body.name, body.enabled)
        wake_key_probe_scheduler()
        return result
    except ValueError as e:
        raise HTTPException(status_code=409, detail=str(e))


@router.post("/keys/probe-all")
async def probe_all_configured_keys():
    return await probe_all_keys(trigger="manual", enabled_only=True)


class KeyUpdateBody(BaseModel):
    new_name: str | None = None
    new_key: str | None = None
    enabled: bool | None = None


@router.put("/keys/{name}")
async def update_key(name: str, body: KeyUpdateBody):
    try:
        result = key_manager.update_key(
            name, new_name=body.new_name, new_key=body.new_key, enabled=body.enabled
        )
        wake_key_probe_scheduler()
        return result
    except KeyError as e:
        raise HTTPException(status_code=404, detail=str(e))
    except ValueError as e:
        raise HTTPException(status_code=409, detail=str(e))


@router.post("/keys/{name}/probe")
async def probe_single_key(name: str):
    try:
        return await probe_key_by_name(name, trigger="manual")
    except KeyError as e:
        raise HTTPException(status_code=404, detail=str(e))
    except RuntimeError as e:
        raise HTTPException(status_code=409, detail=str(e))


@router.delete("/keys/{name}", status_code=204)
async def delete_key(name: str):
    try:
        key_manager.remove_key(name)
        wake_key_probe_scheduler()
    except KeyError as e:
        raise HTTPException(status_code=404, detail=str(e))


@router.post("/keys/{name}/reset-stats")
async def reset_key_stats(name: str):
    try:
        key_manager.reset_stats(name)
        return {"message": f"Status reset for '{name}'"}
    except KeyError as e:
        raise HTTPException(status_code=404, detail=str(e))


@router.post("/keys/reload")
async def reload_keys():
    key_manager.reload()
    wake_key_probe_scheduler()
    return {"message": "Keys reloaded", "count": len(key_manager.get_stats())}


# ── Config ───────────────────────────────────────────

@router.get("/config")
async def get_config():
    return {
        "upstream_base_url": settings.UPSTREAM_BASE_URL,
        "http_proxy": settings.HTTP_PROXY or "",
        "upstream_timeout": settings.UPSTREAM_TIMEOUT,
        "max_retries": settings.MAX_RETRIES,
        "key_cooldown_seconds": settings.KEY_COOLDOWN_SECONDS,
        "key_max_consecutive_failures": settings.KEY_MAX_CONSECUTIVE_FAILURES,
        "key_healthcheck_enabled": settings.KEY_HEALTHCHECK_ENABLED,
        "key_healthcheck_interval_seconds": settings.KEY_HEALTHCHECK_INTERVAL_SECONDS,
        "key_healthcheck_timeout": settings.KEY_HEALTHCHECK_TIMEOUT,
        "key_healthcheck_model": settings.KEY_HEALTHCHECK_MODEL,
    }


class ConfigUpdateBody(BaseModel):
    upstream_base_url: str | None = None
    http_proxy: str | None = None
    upstream_timeout: float | None = None
    max_retries: int | None = None
    key_cooldown_seconds: int | None = None
    key_max_consecutive_failures: int | None = None
    key_healthcheck_enabled: bool | None = None
    key_healthcheck_interval_seconds: int | None = Field(default=None, ge=30)
    key_healthcheck_timeout: float | None = Field(default=None, gt=0)
    key_healthcheck_model: str | None = None


@router.put("/config")
async def update_config(body: ConfigUpdateBody):
    need_rebuild = False
    wake_probe_scheduler = False

    if body.upstream_base_url is not None:
        settings.UPSTREAM_BASE_URL = body.upstream_base_url
    if body.http_proxy is not None:
        new_proxy = body.http_proxy.strip() or None
        if new_proxy != settings.HTTP_PROXY:
            settings.HTTP_PROXY = new_proxy
            need_rebuild = True
    if body.upstream_timeout is not None:
        if body.upstream_timeout != settings.UPSTREAM_TIMEOUT:
            settings.UPSTREAM_TIMEOUT = body.upstream_timeout
            need_rebuild = True
    if body.max_retries is not None:
        settings.MAX_RETRIES = body.max_retries
    if body.key_cooldown_seconds is not None:
        settings.KEY_COOLDOWN_SECONDS = body.key_cooldown_seconds
    if body.key_max_consecutive_failures is not None:
        settings.KEY_MAX_CONSECUTIVE_FAILURES = body.key_max_consecutive_failures
    if body.key_healthcheck_enabled is not None:
        settings.KEY_HEALTHCHECK_ENABLED = body.key_healthcheck_enabled
        wake_probe_scheduler = True
    if body.key_healthcheck_interval_seconds is not None:
        settings.KEY_HEALTHCHECK_INTERVAL_SECONDS = body.key_healthcheck_interval_seconds
        wake_probe_scheduler = True
    if body.key_healthcheck_timeout is not None:
        settings.KEY_HEALTHCHECK_TIMEOUT = body.key_healthcheck_timeout
        wake_probe_scheduler = True
    if body.key_healthcheck_model is not None:
        model = body.key_healthcheck_model.strip()
        if not model:
            raise HTTPException(status_code=400, detail="key_healthcheck_model cannot be empty")
        settings.KEY_HEALTHCHECK_MODEL = model
        wake_probe_scheduler = True

    if need_rebuild:
        await rebuild_client()
    if wake_probe_scheduler:
        wake_key_probe_scheduler()

    return {
        "message": "Config updated",
        "rebuild_client": need_rebuild,
        "wake_probe_scheduler": wake_probe_scheduler,
    }


# ── Model Aliases ────────────────────────────────────

@router.get("/models")
async def list_aliases():
    return {"aliases": model_manager.get_all()}


class AliasCreateBody(BaseModel):
    alias: str
    target: str


@router.post("/models", status_code=201)
async def add_alias(body: AliasCreateBody):
    try:
        return model_manager.add(body.alias, body.target)
    except ValueError as e:
        raise HTTPException(status_code=409, detail=str(e))


class AliasUpdateBody(BaseModel):
    new_alias: str | None = None
    new_target: str | None = None


@router.put("/models/{alias}")
async def update_alias(alias: str, body: AliasUpdateBody):
    try:
        return model_manager.update(alias, new_alias=body.new_alias, new_target=body.new_target)
    except KeyError as e:
        raise HTTPException(status_code=404, detail=str(e))
    except ValueError as e:
        raise HTTPException(status_code=409, detail=str(e))


@router.delete("/models/{alias}", status_code=204)
async def delete_alias(alias: str):
    try:
        model_manager.remove(alias)
    except KeyError as e:
        raise HTTPException(status_code=404, detail=str(e))
