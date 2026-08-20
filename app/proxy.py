"""账号级出站代理：URL 校验与 httpx 客户端构造。"""

from __future__ import annotations

from urllib.parse import urlparse

import httpx

from .models import Account

ALLOWED_SCHEMES = ("http", "https", "socks4", "socks5", "socks5h")


def normalize_proxy_url(raw: str | None) -> str | None:
    """空白视为未配置；非法 scheme 或缺少主机则抛 ValueError。"""
    if raw is None:
        return None
    url = raw.strip()
    if not url:
        return None
    parsed = urlparse(url)
    scheme = (parsed.scheme or "").lower()
    if scheme not in ALLOWED_SCHEMES:
        raise ValueError(
            f"不支持的代理协议: {scheme or '(空)'}，允许: {', '.join(ALLOWED_SCHEMES)}"
        )
    if not parsed.netloc:
        raise ValueError("代理 URL 缺少主机地址")
    return url


def make_async_client(
    account: Account | None = None,
    *,
    proxy_url: str | None = None,
    **kwargs,
) -> httpx.AsyncClient:
    """优先用显式 proxy_url，否则用账号上的 proxy_url；空则直连。"""
    if proxy_url is not None:
        url = proxy_url
    elif account is not None:
        url = account.proxy_url
    else:
        url = None
    if url:
        kwargs["proxy"] = url
    return httpx.AsyncClient(**kwargs)
