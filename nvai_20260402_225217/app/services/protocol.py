"""
Request/response cleaning for NVIDIA → OpenAI compatibility.

Ensures outgoing responses stay close to the OpenAI chat completion spec while
preserving reasoning fields that thinking-capable clients may depend on.
"""

from __future__ import annotations

import json
import time
import uuid
from typing import Any


# ── Request cleaning ─────────────────────────────────

def normalize_content(content: Any) -> str:
    """Flatten OpenAI multi-part content list into a plain string."""
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        parts: list[str] = []
        for part in content:
            if isinstance(part, dict):
                if part.get("type") == "text":
                    parts.append(part.get("text", ""))
            elif isinstance(part, str):
                parts.append(part)
        return "\n".join(p for p in parts if p)
    return str(content) if content is not None else ""


def clean_request_payload(
    messages: list[dict[str, Any]],
) -> list[dict[str, Any]]:
    """
    Clean inbound messages before forwarding to NVIDIA upstream:
    1. content list → string
    2. developer role → system
    3. strip None-valued fields
    """
    cleaned: list[dict[str, Any]] = []
    for msg in messages:
        role = msg.get("role", "user")
        if role == "developer":
            role = "system"

        new_msg: dict[str, Any] = {"role": role}

        content = msg.get("content")
        if content is not None:
            new_msg["content"] = normalize_content(content)
        else:
            new_msg["content"] = ""

        # pass through known optional fields if present
        for key in ("tool_call_id", "tool_calls", "name"):
            val = msg.get(key)
            if val is not None:
                new_msg[key] = val

        cleaned.append(new_msg)
    return cleaned


# ── Non-stream response cleaning ─────────────────────

def _clean_tool_calls(raw_tool_calls: list[Any] | None) -> list[dict] | None:
    """Normalize tool_calls to strict OpenAI format."""
    if not raw_tool_calls:
        return None
    result = []
    for tc in raw_tool_calls:
        if not isinstance(tc, dict):
            continue
        func = tc.get("function") or {}
        arguments = func.get("arguments", "{}")
        if isinstance(arguments, dict):
            arguments = json.dumps(arguments, ensure_ascii=False)
        elif not isinstance(arguments, str):
            arguments = str(arguments)

        result.append({
            "id": tc.get("id") or f"call_{uuid.uuid4().hex[:24]}",
            "type": tc.get("type", "function"),
            "function": {
                "name": func.get("name", ""),
                "arguments": arguments,
            },
        })
    return result or None


def _clean_usage(raw_usage: dict | None) -> dict:
    """Extract only standard OpenAI usage fields."""
    u = raw_usage or {}
    return {
        "prompt_tokens": int(u.get("prompt_tokens", 0) or 0),
        "completion_tokens": int(u.get("completion_tokens", 0) or 0),
        "total_tokens": int(u.get("total_tokens", 0) or 0),
    }


def clean_response(upstream: dict, model: str) -> dict:
    """
    Transform a NVIDIA upstream JSON response into a clean OpenAI response.
    Strips NVIDIA-only transport noise but keeps reasoning fields intact.
    """
    resp_id = upstream.get("id") or f"chatcmpl-{uuid.uuid4().hex[:32]}"
    created = upstream.get("created") or int(time.time())

    choices_out = []
    for choice in upstream.get("choices") or []:
        message = choice.get("message") or {}
        content = message.get("content") or ""
        tool_calls = _clean_tool_calls(message.get("tool_calls"))
        reasoning = message.get("reasoning")
        reasoning_content = message.get("reasoning_content")

        finish_reason = choice.get("finish_reason") or "stop"
        if tool_calls:
            finish_reason = "tool_calls"
            content = ""

        clean_msg: dict[str, Any] = {
            "role": "assistant",
            "content": content,
        }
        if tool_calls:
            clean_msg["tool_calls"] = tool_calls
        if reasoning is not None:
            clean_msg["reasoning"] = reasoning
        if reasoning_content is not None:
            clean_msg["reasoning_content"] = reasoning_content

        choices_out.append({
            "index": choice.get("index", 0),
            "message": clean_msg,
            "finish_reason": finish_reason,
        })

    return {
        "id": resp_id,
        "object": "chat.completion",
        "created": created,
        "model": model,
        "choices": choices_out,
        "usage": _clean_usage(upstream.get("usage")),
    }


# ── Stream chunk cleaning ────────────────────────────

def _clean_delta(delta: dict) -> dict:
    """Keep standard delta fields and reasoning deltas for thinking models."""
    out: dict[str, Any] = {}
    if "role" in delta:
        out["role"] = delta["role"]
    if "content" in delta and delta["content"] is not None:
        out["content"] = delta["content"]
    if "tool_calls" in delta and delta["tool_calls"] is not None:
        out["tool_calls"] = delta["tool_calls"]
    if "reasoning" in delta and delta["reasoning"] is not None:
        out["reasoning"] = delta["reasoning"]
    if "reasoning_content" in delta and delta["reasoning_content"] is not None:
        out["reasoning_content"] = delta["reasoning_content"]
    return out


def _clean_chunk_obj(chunk: dict, model_override: str | None = None) -> dict:
    """Strip NVIDIA-specific fields from a stream chunk object."""
    choices = []
    for c in chunk.get("choices") or []:
        delta = _clean_delta(c.get("delta") or {})
        clean_choice: dict[str, Any] = {
            "index": c.get("index", 0),
            "delta": delta,
            "finish_reason": c.get("finish_reason"),
        }
        choices.append(clean_choice)

    out = {
        "id": chunk.get("id", ""),
        "object": "chat.completion.chunk",
        "created": chunk.get("created", 0),
        "model": model_override or chunk.get("model", ""),
        "choices": choices,
    }
    # include usage on final chunk if present
    if chunk.get("usage") is not None:
        out["usage"] = _clean_usage(chunk["usage"])
    return out


def clean_stream_line(line: bytes, model_override: str | None = None) -> bytes | None:
    """
    Clean a single SSE line from the upstream stream.
    Returns cleaned line bytes, or None to skip the line.
    """
    text = line.decode("utf-8", errors="replace").rstrip("\r\n")

    if not text:
        return line  # blank line (SSE separator)

    if not text.startswith("data: "):
        return None  # skip non-data lines (comments, etc.)

    payload = text[6:]

    if payload.strip() == "[DONE]":
        return line  # pass through as-is

    try:
        chunk = json.loads(payload)
    except json.JSONDecodeError:
        return None  # malformed, skip

    cleaned = _clean_chunk_obj(chunk, model_override=model_override)
    return f"data: {json.dumps(cleaned, ensure_ascii=False)}\n\n".encode()
