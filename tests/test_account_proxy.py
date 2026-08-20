"""账号独立出站代理：模型字段、URL 校验、httpx 客户端构造。"""

from __future__ import annotations

import asyncio
import unittest

from app.models import Account
from app.proxy import ALLOWED_SCHEMES, make_async_client, normalize_proxy_url


def _account(**extra) -> Account:
    data = {
        "id": "test-acc-0001",
        "name": "test-account",
        "provider": "zai",
        "mode": "apiKey",
        "api_key": "fake-api-key",
    }
    data.update(extra)
    return Account.from_dict(data)


def _stringify_proxy(obj) -> str:
    """把 httpx/httpcore 的 proxy 对象收成可断言的字符串。"""
    if obj is None:
        return ""
    host = getattr(obj, "host", None)
    scheme = getattr(obj, "scheme", None)
    if host is not None or scheme is not None:
        if isinstance(host, bytes):
            host = host.decode()
        if isinstance(scheme, bytes):
            scheme = scheme.decode()
        port = getattr(obj, "port", None)
        return f"{scheme}://{host}:{port}"
    return str(obj)


def _proxy_urls_from_client(client) -> list[str]:
    """按已安装 httpx 的实际属性抽出代理 URL，避免猜错字段名。"""
    found: list[str] = []
    for attr in ("_proxy", "proxy"):
        value = getattr(client, attr, None)
        if value is not None:
            found.append(_stringify_proxy(value))
    mounts = getattr(client, "_mounts", None)
    if mounts is None:
        mounts = getattr(client, "mounts", None) or {}
    items = mounts.items() if hasattr(mounts, "items") else []
    for _pattern, transport in items:
        pool = getattr(transport, "_pool", None)
        proxy_url = None
        if pool is not None:
            proxy_url = getattr(pool, "_proxy_url", None) or getattr(pool, "proxy_url", None)
        if proxy_url is None:
            proxy_url = getattr(transport, "_proxy", None) or getattr(transport, "proxy", None)
        if proxy_url is not None:
            found.append(_stringify_proxy(proxy_url))
    return [u for u in found if u]


class AccountProxyTests(unittest.TestCase):
    def test_from_dict_legacy_without_proxy_url(self):
        """旧数据没有 proxy_url 键时，缺省为 None。"""
        acc = Account.from_dict({
            "id": "legacy-acc-0001",
            "name": "legacy",
            "provider": "zai",
            "mode": "apiKey",
            "api_key": "fake-api-key",
        })
        self.assertIsNone(acc.proxy_url)

    def test_to_dict_from_dict_roundtrip_keeps_proxy_url(self):
        """to_dict/from_dict 往返保留 proxy_url。"""
        url = "socks5://127.0.0.1:1080"
        restored = Account.from_dict(_account(proxy_url=url).to_dict())
        self.assertEqual(restored.proxy_url, url)

    def test_public_view_includes_proxy_url(self):
        """public_view 必须带上 proxy_url，供前端编辑框回填。"""
        url = "http://127.0.0.1:8080"
        view = _account(proxy_url=url).public_view()
        self.assertIn("proxy_url", view)
        self.assertEqual(view["proxy_url"], url)

        empty = _account(proxy_url=None).public_view()
        self.assertIn("proxy_url", empty)
        self.assertIsNone(empty["proxy_url"])

    def test_normalize_proxy_url_blank_is_none(self):
        """None / 空 / 空白 -> None。"""
        self.assertIsNone(normalize_proxy_url(None))
        self.assertIsNone(normalize_proxy_url(""))
        self.assertIsNone(normalize_proxy_url("   "))
        self.assertIsNone(normalize_proxy_url("\t\n"))

    def test_normalize_proxy_url_allowed_schemes(self):
        """http/https/socks5/socks5h 合法，strip 后返回。"""
        urls = [
            "http://127.0.0.1:8080",
            "https://proxy.example.com:443",
            "socks5://127.0.0.1:1080",
            "socks5h://127.0.0.1:1080",
            "  http://127.0.0.1:8080  ",
        ]
        for url in urls:
            with self.subTest(url=url):
                got = normalize_proxy_url(url)
                self.assertEqual(got, url.strip())
                self.assertIn(got.split(":", 1)[0], ALLOWED_SCHEMES)

    def test_normalize_proxy_url_invalid_raises(self):
        """ftp 以及没有 host 的 URL 抛 ValueError（中文错误信息）。"""
        for url in ("ftp://127.0.0.1:21", "http://", "socks5://", "https://"):
            with self.subTest(url=url):
                with self.assertRaises(ValueError) as ctx:
                    normalize_proxy_url(url)
                msg = str(ctx.exception)
                self.assertTrue(msg)
                self.assertTrue(any("\u4e00" <= ch <= "\u9fff" for ch in msg))

    def test_make_async_client_without_proxy(self):
        """无代理时 mounts / proxy 相关属性为空或默认，且能成功构造、aclose。"""
        client = make_async_client(trust_env=False)
        try:
            mounts = getattr(client, "_mounts", None)
            if mounts is None:
                mounts = getattr(client, "mounts", None)
            self.assertIn(mounts, (None, {}))
            self.assertIsNone(getattr(client, "_proxy", None))
            self.assertIsNone(getattr(client, "proxy", None))
            self.assertEqual(_proxy_urls_from_client(client), [])
        finally:
            asyncio.run(client.aclose())

    def test_make_async_client_with_explicit_proxy(self):
        """显式 proxy_url 时 mounts/_proxy 带上该 URL，且能成功构造、aclose。"""
        url = "http://127.0.0.1:8080"
        client = make_async_client(proxy_url=url, trust_env=False)
        try:
            found = _proxy_urls_from_client(client)
            self.assertTrue(found, "构造了带代理的客户端，但 mounts/_proxy 未带上 URL")
            blob = " ".join(found)
            self.assertIn("127.0.0.1", blob)
            self.assertIn("8080", blob)
            self.assertIn("http", blob)
        finally:
            asyncio.run(client.aclose())

    def test_make_async_client_uses_account_proxy_url(self):
        """未传显式 proxy_url 时使用 account.proxy_url。"""
        url = "http://10.0.0.1:3128"
        client = make_async_client(_account(proxy_url=url), trust_env=False)
        try:
            blob = " ".join(_proxy_urls_from_client(client))
            self.assertIn("10.0.0.1", blob)
            self.assertIn("3128", blob)
        finally:
            asyncio.run(client.aclose())

    def test_make_async_client_explicit_proxy_overrides_account(self):
        """显式 proxy_url 优先于 account.proxy_url。"""
        client = make_async_client(
            _account(proxy_url="http://10.0.0.1:3128"),
            proxy_url="http://127.0.0.1:8080",
            trust_env=False,
        )
        try:
            blob = " ".join(_proxy_urls_from_client(client))
            self.assertIn("127.0.0.1", blob)
            self.assertIn("8080", blob)
            self.assertNotIn("10.0.0.1", blob)
        finally:
            asyncio.run(client.aclose())
