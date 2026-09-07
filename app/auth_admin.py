"""鉴权依赖：后台管理密钥 + 网关 API Key。"""

from __future__ import annotations

import hmac
import time
from collections import deque

from fastapi import Header, HTTPException, Request, status

from .store import store

# 後台密鑰失敗嘗試的速率限制：單一來源 IP 在滑動窗口內失敗過多後一律 429，
# 防止 /admin/api/* 被用來暴力猜測密鑰；成功校驗會清空該來源的失敗記錄。
_FAILED_ATTEMPTS: dict[str, deque[float]] = {}
_FAILURE_WINDOW_SECONDS = 300.0
_MAX_FAILURES_PER_WINDOW = 10


def _client_host(request: Request) -> str:
    return request.client.host if request.client else "?"


def _prune_attempts(host: str, now: float) -> deque[float]:
    attempts = _FAILED_ATTEMPTS.setdefault(host, deque())
    while attempts and now - attempts[0] > _FAILURE_WINDOW_SECONDS:
        attempts.popleft()
    return attempts


def _extract_bearer(authorization: str | None) -> str | None:
    if not authorization:
        return None
    scheme, _, token = authorization.partition(" ")
    if scheme.lower() != "bearer" or not token:
        return None
    return token


async def verify_admin_key(
    request: Request,
    authorization: str | None = Header(default=None),
) -> None:
    """校验後台管理密鑰，僅接受 `Authorization: Bearer <key>` 頭。

    對單一來源 IP 的連續失敗做速率限制：窗口內失敗達上限後一律 429
    （正確密鑰也會被暫拒，寧可誤傷也不放行暴力嘗試），成功校驗清空記錄。
    舊版支援的 `?app_key=<key>` 查詢參數已移除：金鑰會落入反向代理與
    訪問日誌，且前端已無 EventSource 等無法自訂標頭的場景。
    """
    host = _client_host(request)
    now = time.monotonic()
    attempts = _prune_attempts(host, now)
    if len(attempts) >= _MAX_FAILURES_PER_WINDOW:
        raise HTTPException(status.HTTP_429_TOO_MANY_REQUESTS, "失败次数过多，请稍后再试")

    key = store.admin_key()
    if not key:
        raise HTTPException(status.HTTP_401_UNAUTHORIZED, "未配置后台密钥")

    token = _extract_bearer(authorization)
    if token is None:
        attempts.append(now)
        raise HTTPException(status.HTTP_401_UNAUTHORIZED, "缺少鉴权凭证")
    if not hmac.compare_digest(token, key):
        attempts.append(now)
        raise HTTPException(status.HTTP_401_UNAUTHORIZED, "鉴权凭证无效")
    attempts.clear()


async def verify_gateway_key(
    authorization: str | None = Header(default=None),
    x_api_key: str | None = Header(default=None, alias="x-api-key"),
) -> None:
    """校验 /v1/messages 网关访问密钥；密钥一律必填，未配置即拒绝（fail closed）。"""
    key = store.gateway_key()
    if not key:
        # 僅在 meta 表被手動清空時觸達；寧可拒絕服務也不放行未鑑權流量
        raise HTTPException(status.HTTP_503_SERVICE_UNAVAILABLE, "网关未配置 API Key，请在后台设置")
    token = _extract_bearer(authorization) or x_api_key
    if token is None:
        raise HTTPException(status.HTTP_401_UNAUTHORIZED, "缺少 API Key")
    if not hmac.compare_digest(token, key):
        raise HTTPException(status.HTTP_403_FORBIDDEN, "API Key 无效")
