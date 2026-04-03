from __future__ import annotations

import json
from collections.abc import AsyncIterator

from fastapi import APIRouter, Request
from fastapi.responses import StreamingResponse

from app.deps import RequireAuth
from app.model_manager import model_manager
from app.proxy import forward_request, forward_stream
from app.schemas.openai import ChatCompletionRequest
from app.services.protocol import clean_request_payload, clean_response, clean_stream_line

router = APIRouter(prefix="/v1", dependencies=[RequireAuth])


def _build_upstream_body(req: ChatCompletionRequest, upstream_model: str) -> bytes:
    """Build cleaned JSON body for upstream NVIDIA API."""
    cleaned_messages = clean_request_payload(
        [m.model_dump(exclude_none=True) for m in req.messages]
    )

    # Preserve extra client-supplied fields such as reasoning controls.
    payload: dict = req.model_dump(exclude_none=True)
    payload["model"] = upstream_model
    payload["messages"] = cleaned_messages

    return json.dumps(payload, ensure_ascii=False).encode()


async def _cleaned_stream(
    raw_iter: AsyncIterator[bytes], display_model: str
) -> AsyncIterator[bytes]:
    """Filter each SSE line through protocol cleaning + model rewrite."""
    async for line in raw_iter:
        cleaned = clean_stream_line(line, model_override=display_model)
        if cleaned is not None:
            yield cleaned


@router.post("/chat/completions")
async def chat_completions(req: ChatCompletionRequest):
    upstream_model, alias = model_manager.resolve(req.model)
    display_model = alias or req.model

    body = _build_upstream_body(req, upstream_model)

    if req.stream:
        raw_iter = await forward_stream("POST", "/v1/chat/completions", body)
        return StreamingResponse(
            _cleaned_stream(raw_iter, display_model),
            media_type="text/event-stream",
            headers={
                "Cache-Control": "no-cache",
                "Connection": "keep-alive",
                "X-Accel-Buffering": "no",
            },
        )

    upstream = await forward_request("POST", "/v1/chat/completions", body)
    return clean_response(upstream, display_model)


@router.post("/completions")
async def completions(request: Request):
    body = await request.body()
    try:
        payload = json.loads(body)
    except json.JSONDecodeError:
        payload = {}

    original_model = payload.get("model", "")
    upstream_model, alias = model_manager.resolve(original_model)
    display_model = alias or original_model

    if upstream_model != original_model:
        payload["model"] = upstream_model
        body = json.dumps(payload, ensure_ascii=False).encode()

    is_stream = payload.get("stream", False)
    if is_stream:
        raw_iter = await forward_stream("POST", "/v1/completions", body)
        return StreamingResponse(
            _cleaned_stream(raw_iter, display_model),
            media_type="text/event-stream",
        )

    upstream = await forward_request("POST", "/v1/completions", body)
    if "model" in upstream:
        upstream["model"] = display_model
    return upstream


@router.post("/embeddings")
async def embeddings(request: Request):
    body = await request.body()
    try:
        payload = json.loads(body)
    except json.JSONDecodeError:
        payload = {}

    original_model = payload.get("model", "")
    upstream_model, _ = model_manager.resolve(original_model)
    if upstream_model != original_model:
        payload["model"] = upstream_model
        body = json.dumps(payload, ensure_ascii=False).encode()

    return await forward_request("POST", "/v1/embeddings", body)


@router.get("/models")
async def list_models():
    import time

    aliases = model_manager.get_all()
    now = int(time.time())
    models = [
        {
            "id": a["alias"],
            "object": "model",
            "created": now,
            "owned_by": "nvai",
        }
        for a in aliases
    ]
    return {"object": "list", "data": models}
