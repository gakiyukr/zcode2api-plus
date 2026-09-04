"""鉴权依赖：后台管理密钥 + 可选的网关 API Key。"""

from __future__ import annotations

import hmac

from fastapi import Header, HTTPException, status

from .store import store


def _extract_bearer(authorization: str | None) -> str | None:
    if not authorization:
        return None
    scheme, _, token = authorization.partition(" ")
    if scheme.lower() != "bearer" or not token:
        return None
    return token


async def verify_admin_key(
    authorization: str | None = Header(default=None),
) -> None:
    """校验後台管理密鑰，僅接受 `Authorization: Bearer <key>` 頭。

    舊版支援的 `?app_key=<key>` 查詢參數已移除：金鑰會落入反向代理與
    訪問日誌，且前端已無 EventSource 等無法自訂標頭的場景。
    """
    key = store.admin_key()
    if not key:
        raise HTTPException(status.HTTP_401_UNAUTHORIZED, "未配置后台密钥")

    token = _extract_bearer(authorization)
    if token is None:
        raise HTTPException(status.HTTP_401_UNAUTHORIZED, "缺少鉴权凭证")
    if not hmac.compare_digest(token, key):
        raise HTTPException(status.HTTP_401_UNAUTHORIZED, "鉴权凭证无效")


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
