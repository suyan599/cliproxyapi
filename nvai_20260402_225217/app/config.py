from __future__ import annotations

from pydantic_settings import BaseSettings


class Settings(BaseSettings):
    APP_NAME: str = "nvai"
    DEBUG: bool = False

    # 对外鉴权 — 客户端请求需要携带此 key
    API_KEY: str = "change-me"

    # 管理后台鉴权 — 登录 admin 面板用
    ADMIN_KEY: str = "change-me-admin"

    # 上游 NVIDIA API
    UPSTREAM_BASE_URL: str = "https://integrate.api.nvidia.com"
    KEYS_FILE: str = "keys.json"

    # 可选 HTTP 代理 (例如 http://127.0.0.1:7890)
    HTTP_PROXY: str | None = None

    # 故障转移
    MAX_RETRIES: int = 3
    KEY_COOLDOWN_SECONDS: int = 60
    KEY_MAX_CONSECUTIVE_FAILURES: int = 3

    # 上游请求超时 (秒)
    UPSTREAM_TIMEOUT: float = 120.0

    # API Key 主动测活
    KEY_HEALTHCHECK_ENABLED: bool = False
    KEY_HEALTHCHECK_INTERVAL_SECONDS: int = 300
    KEY_HEALTHCHECK_TIMEOUT: float = 15.0
    KEY_HEALTHCHECK_MODEL: str = "meta/llama-3.1-8b-instruct"

    CORS_ORIGINS: list[str] = ["*"]

    model_config = {"env_file": ".env", "env_file_encoding": "utf-8"}


settings = Settings()
