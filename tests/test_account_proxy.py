"""账号独立出站代理：模型字段、URL 校验、httpx 客户端构造。"""

from __future__ import annotations

import asyncio
import tempfile
import unittest
from pathlib import Path

from app import settings
from app.models import Account
from app.proxy import ALLOWED_SCHEMES, make_async_client, normalize_proxy_url
from app.routes.admin_api import _parse_as_lookup, _parse_ip_lookup
from app.store import Store


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


class ProxyProfileStoreTests(unittest.TestCase):
    """命名代理出口必須持久化，並與帳號指派保持一致。"""

    def setUp(self):
        self._temp = tempfile.TemporaryDirectory()
        self._old_data_dir = settings.DATA_DIR
        self._old_db_path = settings.DB_PATH
        settings.DATA_DIR = Path(self._temp.name)
        settings.DB_PATH = settings.DATA_DIR / "accounts.db"
        self.store = Store()

    def tearDown(self):
        settings.DATA_DIR = self._old_data_dir
        settings.DB_PATH = self._old_db_path
        self._temp.cleanup()

    def test_profile_assignment_update_and_delete(self):
        """出口的新增、指派、修改與刪除會同步反映到帳號。"""
        account = self.store.add_account("zai", "account-1", "fake-api-key")
        profile = self.store.add_proxy_profile("香港出口", "socks5://127.0.0.1:1080")

        self.assertTrue(self.store.assign_proxy_profile(account.id, profile["id"]))
        self.assertEqual(account.proxy_id, profile["id"])
        self.assertEqual(account.proxy_url, "socks5://127.0.0.1:1080")

        updated = self.store.update_proxy_profile(
            profile["id"], "香港出口", "http://127.0.0.1:8080"
        )
        self.assertIsNotNone(updated)
        self.assertEqual(account.proxy_url, "http://127.0.0.1:8080")

        reloaded = Store()
        restored = reloaded.find_any(account.id)
        self.assertIsNotNone(restored)
        self.assertEqual(restored.proxy_id, profile["id"])
        self.assertEqual(restored.proxy_url, "http://127.0.0.1:8080")

        self.assertTrue(reloaded.delete_proxy_profile(profile["id"]))
        self.assertIsNone(restored.proxy_id)
        self.assertIsNone(restored.proxy_url)

    def test_duplicate_name_and_missing_profile_are_rejected(self):
        """代理名稱不可重複，帳號不可指派到不存在的出口。"""
        account = self.store.add_account("zai", "account-1", "fake-api-key")
        self.store.add_proxy_profile("固定出口", "http://127.0.0.1:8080")
        with self.assertRaisesRegex(ValueError, "代理名稱已存在"):
            self.store.add_proxy_profile("固定出口", "http://127.0.0.1:8081")
        with self.assertRaisesRegex(ValueError, "代理配置不存在"):
            self.store.assign_proxy_profile(account.id, "proxy-missing")


class IpLookupParsingTests(unittest.TestCase):
    """出口探測必須統一整理不同查詢服務的 IP 與 ASN 欄位。"""

    def test_parse_ip_sb_payload(self):
        result = _parse_ip_lookup({
            "ip": "203.0.113.10",
            "asn": 64500,
            "asn_organization": "Example Network",
            "country": "Singapore",
            "country_code": "SG",
        })
        self.assertEqual(result["ip"], "203.0.113.10")
        self.assertEqual(result["asn"], "AS64500")
        self.assertEqual(result["operator"], "Example Network")
        self.assertEqual(result["country_code"], "SG")

    def test_parse_ipwhois_payload(self):
        result = _parse_ip_lookup({
            "ip": "2001:db8::1",
            "country": "Japan",
            "country_code": "jp",
            "connection": {"asn": 64501, "org": "Backup Network"},
        })
        self.assertEqual(result["ip"], "2001:db8::1")
        self.assertEqual(result["asn"], "AS64501")
        self.assertEqual(result["operator"], "Backup Network")
        self.assertEqual(result["country_code"], "JP")

    def test_missing_ip_is_rejected(self):
        with self.assertRaisesRegex(ValueError, "未回傳 IP"):
            _parse_ip_lookup({"asn": 64500})

    def test_parse_ipapi_payload(self):
        result = _parse_ip_lookup({
            "ip": "203.0.113.20",
            "asn_num": 64502,
            "asn_org": "Third Network",
            "company_name": "Fallback Company",
            "cc": "DE",
        })
        self.assertEqual(result["asn"], "AS64502")
        self.assertEqual(result["operator"], "Third Network")
        self.assertEqual(result["country_code"], "DE")

    def test_parse_as_lookup_csv(self):
        asn, operator = _parse_as_lookup(
            '"203.0.113.20","64503","203.0.113.0/24","Example ASN, TW"'
        )
        self.assertEqual(asn, "AS64503")
        self.assertEqual(operator, "Example ASN, TW")
