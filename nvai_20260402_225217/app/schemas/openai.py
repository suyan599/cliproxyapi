"""OpenAI-compatible request / response schemas."""

from __future__ import annotations

from typing import Any, Optional

from pydantic import BaseModel, Field


# ── Request ──────────────────────────────────────────

class ChatMessage(BaseModel):
    role: str
    content: Any = None
    tool_call_id: Optional[str] = None
    tool_calls: Optional[list[dict]] = None
    name: Optional[str] = None

    model_config = {"extra": "allow"}


class ChatCompletionRequest(BaseModel):
    model: str = ""
    messages: list[ChatMessage]
    temperature: Optional[float] = None
    top_p: Optional[float] = None
    max_tokens: Optional[int] = None
    stream: bool = False
    stop: Optional[Any] = None
    frequency_penalty: Optional[float] = None
    presence_penalty: Optional[float] = None
    tools: Optional[list[dict]] = None
    tool_choice: Optional[Any] = None
    response_format: Optional[dict] = None

    model_config = {"extra": "allow"}


# ── Response (non-stream) ────────────────────────────

class Usage(BaseModel):
    prompt_tokens: int = 0
    completion_tokens: int = 0
    total_tokens: int = 0


class ToolCallFunction(BaseModel):
    name: str = ""
    arguments: str = ""


class ToolCall(BaseModel):
    id: str = ""
    type: str = "function"
    function: ToolCallFunction


class AssistantMessage(BaseModel):
    role: str = "assistant"
    content: Optional[str] = ""
    reasoning: Optional[str] = None
    reasoning_content: Optional[str] = None
    tool_calls: Optional[list[ToolCall]] = None


class Choice(BaseModel):
    index: int = 0
    message: AssistantMessage
    finish_reason: str = "stop"


class ChatCompletionResponse(BaseModel):
    id: str
    object: str = "chat.completion"
    created: int
    model: str
    choices: list[Choice]
    usage: Usage = Field(default_factory=Usage)


# ── Error (OpenAI standard) ─────────────────────────

class ErrorDetail(BaseModel):
    message: str
    type: str = "invalid_request_error"
    code: Optional[str] = None


class ErrorResponse(BaseModel):
    error: ErrorDetail
