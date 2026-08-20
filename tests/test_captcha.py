from __future__ import annotations

import unittest
from unittest.mock import AsyncMock, patch

import httpx

from app.agent import build_request
from app.captcha import CaptchaManager, CaptchaToken, _compact_solver_error
from app.models import Account
from app.routes import gateway
from app.routes.gateway import _is_captcha_error


class CaptchaProtocolTests(unittest.TestCase):
    def test_captcha_headers_include_region_and_client_cannot_override(self):
        account = Account.create("zai", "test", "header.payload.signature")
        _, headers = build_request(
            account,
            {},
            "server-token",
            "sgp",
            {
                "x-aliyun-captcha-verify-param": "client-token",
                "x-aliyun-captcha-verify-region": "client-region",
            },
        )

        self.assertEqual(headers["X-Aliyun-Captcha-Verify-Param"], "server-token")
        self.assertEqual(headers["X-Aliyun-Captcha-Verify-Region"], "sgp")
        self.assertNotIn("x-aliyun-captcha-verify-param", headers)
        self.assertNotIn("x-aliyun-captcha-verify-region", headers)

    def test_captcha_challenge_detection_supports_header_and_code_3007(self):
        self.assertTrue(_is_captcha_error('{"code":3007,"msg":"captcha verify failed"}', 400, {}))
        self.assertTrue(_is_captcha_error("ordinary response", 403, {"x-aliyun-captcha-verify-param": "challenge"}))
        self.assertFalse(_is_captcha_error("invalid token", 403, {}))

    def test_solver_error_summary_redacts_long_values(self):
        secret = "A" * 80
        summary = _compact_solver_error(f"SDK_FAIL={secret}".encode())
        self.assertNotIn(secret, summary)
        self.assertIn("[redacted]", summary)


class _FakeResponse:
    def __init__(self, status_code, body, headers=None):
        self.status_code = status_code
        self.headers = httpx.Headers(headers or {})
        self._body = body.encode("utf-8")

    async def aread(self):
        return self._body

    async def aiter_bytes(self):
        yield self._body


class _FakeContext:
    def __init__(self, response):
        self.response = response

    async def __aenter__(self):
        return self.response

    async def __aexit__(self, *args):
        return False


class _FakeClient:
    responses = []
    calls = []

    def __init__(self, *args, **kwargs):
        pass

    def stream(self, method, url, headers, content):
        self.calls.append({"method": method, "url": url, "headers": headers})
        return _FakeContext(self.responses[len(self.calls) - 1])

    async def aclose(self):
        pass


class GatewayCaptchaRetryTests(unittest.IsolatedAsyncioTestCase):
    async def test_code_3007_refreshes_token_without_invalidating_account(self):
        account = Account.create("zai", "retry", "header.payload.signature")
        _FakeClient.responses = [
            _FakeResponse(400, '{"code":3007,"msg":"captcha verify failed"}'),
            _FakeResponse(200, '{"id":"ok"}', {"content-type": "application/json"}),
        ]
        _FakeClient.calls = []
        tokens = AsyncMock(side_effect=[CaptchaToken("first-token", "sgp"), CaptchaToken("fresh-token", "sgp")])

        with (
            patch.object(gateway.httpx, "AsyncClient", _FakeClient),
            patch.object(gateway.captcha_manager, "get_verify_param", tokens),
            patch.object(gateway.store, "update_account"),
            patch.object(gateway.asyncio, "create_task", side_effect=lambda coroutine: coroutine.close()),
        ):
            response = await gateway._try_account(
                "req",
                account,
                {},
                b"{}",
                {"x-aliyun-captcha-verify-param": "client-token"},
                None,
                True,
            )
            chunks = [chunk async for chunk in response.body_iterator]

        self.assertEqual(response.status_code, 200)
        self.assertEqual(chunks, [b'{"id":"ok"}'])
        self.assertEqual(len(_FakeClient.calls), 2)
        self.assertEqual(_FakeClient.calls[0]["headers"]["X-Aliyun-Captcha-Verify-Param"], "first-token")
        self.assertEqual(_FakeClient.calls[1]["headers"]["X-Aliyun-Captcha-Verify-Param"], "fresh-token")
        self.assertEqual(account.status, "active")
        self.assertEqual(tokens.await_count, 2)


class CaptchaManagerTests(unittest.IsolatedAsyncioTestCase):
    async def test_disabled_config_does_not_start_solver(self):
        manager = CaptchaManager()
        manager.fetch_config = AsyncMock(return_value={"enabled": False})
        manager._solve = AsyncMock()

        self.assertIsNone(await manager.get_verify_param())
        manager._solve.assert_not_awaited()

    async def test_cached_token_contains_region_and_deduplicates_solver(self):
        manager = CaptchaManager()
        manager.fetch_config = AsyncMock(return_value={
            "enabled": True,
            "sceneId": "scene",
            "region": "sgp",
            "prefix": "prefix",
        })
        manager._solve = AsyncMock(return_value=CaptchaToken("automatic-token", "sgp"))

        with patch("app.captcha.settings.CAPTCHA_BROWSER_ENABLED", False):
            first = await manager.get_verify_param()
            second = await manager.get_verify_param()

        self.assertEqual(first, CaptchaToken("automatic-token", "sgp"))
        self.assertEqual(second, first)
        manager._solve.assert_awaited_once()

    async def test_manual_token_uses_configured_region(self):
        manager = CaptchaManager()
        manager.fetch_config = AsyncMock(return_value={"enabled": True, "region": "sgp"})
        await manager.fetch_config()
        manager._config_cache = {"enabled": True, "region": "sgp"}

        await manager.set_manual_param("manual-token")
        token = await manager.get_verify_param()

        self.assertEqual(token, CaptchaToken("manual-token", "sgp"))


if __name__ == "__main__":
    unittest.main()
