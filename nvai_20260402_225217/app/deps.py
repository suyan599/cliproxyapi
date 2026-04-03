from __future__ import annotations

from typing import Annotated

from fastapi import Depends, HTTPException, Security, status
from fastapi.security import HTTPAuthorizationCredentials, HTTPBearer

from app.config import settings

_bearer = HTTPBearer(auto_error=False)


async def verify_api_key(
    cred: Annotated[HTTPAuthorizationCredentials | None, Security(_bearer)],
) -> str:
    if cred is None or cred.credentials != settings.API_KEY:
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="Invalid or missing API key",
        )
    return cred.credentials


async def verify_admin_key(
    cred: Annotated[HTTPAuthorizationCredentials | None, Security(_bearer)],
) -> str:
    if cred is None or cred.credentials != settings.ADMIN_KEY:
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="Invalid or missing admin key",
        )
    return cred.credentials


RequireAuth = Depends(verify_api_key)
RequireAdmin = Depends(verify_admin_key)
