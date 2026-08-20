from __future__ import annotations

import unittest
from unittest.mock import AsyncMock, patch

from app.captcha import CaptchaManager, CaptchaToken


class _FakePool:
    def __init__(self, tokens=None, start_error: Exception | None = None):
        self.is_started = False
        self.tokens = list(tokens or [])
        self.start_error = start_error
        self.start = AsyncMock(side_effect=self._start)
        self.get_token = AsyncMock(side_effect=self._get_token)
        self.stop = AsyncMock()

    async def _start(self):
        if self.start_error is not None:
            raise self.start_error
        self.is_started = True

    async def _get_token(self, **kwargs):
        return self.tokens.pop(0)


class BrowserCaptchaManagerTests(unittest.IsolatedAsyncioTestCase):
    async def test_browser_tokens_are_fresh_and_pool_is_reused(self):
        manager = CaptchaManager()
        manager.fetch_config = AsyncMock(return_value={
            "enabled": True,
            "sceneId": "scene",
            "region": "sgp",
            "prefix": "prefix",
        })
        manager._solve = AsyncMock()
        pool = _FakePool(["browser-token-a", "browser-token-b"])

        with patch.multiple(
            "app.captcha.settings",
            CAPTCHA_BROWSER_ENABLED=True,
            CAPTCHA_BROWSER_WORKERS=1,
            CAPTCHA_BROWSER_STARTUP_TIMEOUT=5,
            CAPTCHA_BROWSER_REQUEST_TIMEOUT=5,
            CAPTCHA_BROWSER_QUEUE_TIMEOUT=5,
            CAPTCHA_BROWSER_SHUTDOWN_TIMEOUT=1,
        ), patch("app.captcha.BrowserWorkerPool.from_cloakbrowser", return_value=pool) as factory:
            first = await manager.get_verify_param()
            second = await manager.get_verify_param()

        self.assertEqual(first, CaptchaToken("browser-token-a", "sgp"))
        self.assertEqual(second, CaptchaToken("browser-token-b", "sgp"))
        self.assertEqual(factory.call_count, 1)
        self.assertEqual(pool.get_token.await_count, 2)
        manager._solve.assert_not_awaited()

        await manager.close()
        pool.stop.assert_awaited_once()

    async def test_browser_start_failure_falls_back_to_node_solver(self):
        manager = CaptchaManager()
        manager.fetch_config = AsyncMock(return_value={
            "enabled": True,
            "sceneId": "scene",
            "region": "sgp",
            "prefix": "prefix",
        })
        manager._solve = AsyncMock(return_value=CaptchaToken("node-token", "sgp"))
        pool = _FakePool(start_error=RuntimeError("browser unavailable"))

        with patch.multiple(
            "app.captcha.settings",
            CAPTCHA_BROWSER_ENABLED=True,
            CAPTCHA_BROWSER_FAILURE_COOLDOWN=60,
            CAPTCHA_BROWSER_WORKERS=1,
            CAPTCHA_BROWSER_STARTUP_TIMEOUT=5,
            CAPTCHA_BROWSER_REQUEST_TIMEOUT=5,
            CAPTCHA_BROWSER_QUEUE_TIMEOUT=5,
            CAPTCHA_BROWSER_SHUTDOWN_TIMEOUT=1,
        ), patch("app.captcha.BrowserWorkerPool.from_cloakbrowser", return_value=pool):
            token = await manager.get_verify_param()

        self.assertEqual(token, CaptchaToken("node-token", "sgp"))
        manager._solve.assert_awaited_once()
        pool.stop.assert_awaited_once()

    async def test_disabled_browser_keeps_existing_solver_path(self):
        manager = CaptchaManager()
        manager.fetch_config = AsyncMock(return_value={
            "enabled": True,
            "sceneId": "scene",
            "region": "sgp",
            "prefix": "prefix",
        })
        manager._solve = AsyncMock(return_value=CaptchaToken("node-token", "sgp"))

        with patch("app.captcha.settings.CAPTCHA_BROWSER_ENABLED", False):
            token = await manager.get_verify_param()

        self.assertEqual(token, CaptchaToken("node-token", "sgp"))
        manager._solve.assert_awaited_once()


if __name__ == "__main__":
    unittest.main()
