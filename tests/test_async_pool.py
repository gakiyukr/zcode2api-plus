"""async_pool 必须带上验证码，3007 时刷新令牌重试。"""

from __future__ import annotations

import asyncio
import json
import unittest
from unittest.mock import AsyncMock, Mock, patch

import httpx
from fastapi.responses import StreamingResponse

from app.captcha import CaptchaToken
from app.models import Account
from app.routes import async_pool


class _FakeResponse:
    def __init__(self, status_code, body, headers=None, lines=None):
        self.status_code = status_code
        self.headers = httpx.Headers(headers or {"content-type": "text/event-stream"})
        self._body = body.encode("utf-8")
        self._lines = lines or []

    async def aread(self):
        return self._body

    async def aiter_lines(self):
        for line in self._lines:
            yield line


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
        self.calls.append({"method": method, "url": url, "headers": dict(headers), "content": content})
        return _FakeContext(self.responses[len(self.calls) - 1])

    async def aclose(self):
        pass

    async def __aenter__(self):
        return self

    async def __aexit__(self, *args):
        return False


class AsyncPoolCaptchaTests(unittest.IsolatedAsyncioTestCase):
    async def test_code_3007_refreshes_token_and_forwards_sse(self):
        account = Account.create("zai", "async-acc", "header.payload.signature")
        _FakeClient.responses = [
            _FakeResponse(400, '{"code":3007,"msg":"captcha verify failed"}'),
            _FakeResponse(
                200,
                "",
                lines=['data: {"id":"ok"}', "data: [DONE]"],
            ),
        ]
        _FakeClient.calls = []
        tokens = AsyncMock(side_effect=[
            CaptchaToken("first-token", "sgp"),
            CaptchaToken("fresh-token", "sgp"),
        ])
        ticket_id = "ticket-captcha"
        queue: asyncio.Queue = asyncio.Queue()
        async_pool._tickets[ticket_id] = {
            "status": "pending",
            "body": {"model": "GLM-5.3", "max_tokens": 8, "messages": [{"role": "user", "content": "ping"}]},
            "queue": queue,
            "created_at": 0,
        }

        with (
            patch.object(async_pool.store, "select", return_value=account),
            patch.object(async_pool.captcha_manager, "get_verify_param", tokens),
            patch.object(async_pool.captcha_manager, "invalidate") as invalidate,
            patch.object(async_pool, "make_async_client", _FakeClient),
        ):
            await async_pool._process_ticket(ticket_id)

        events = []
        while not queue.empty():
            events.append(queue.get_nowait())
        async_pool._tickets.pop(ticket_id, None)

        self.assertEqual([e["type"] for e in events], ["ready", "chunk", "done"])
        self.assertEqual(events[1]["data"], {"id": "ok"})
        self.assertEqual(len(_FakeClient.calls), 2)
        self.assertEqual(_FakeClient.calls[0]["headers"]["X-Aliyun-Captcha-Verify-Param"], "first-token")
        self.assertEqual(_FakeClient.calls[1]["headers"]["X-Aliyun-Captcha-Verify-Param"], "fresh-token")
        self.assertEqual(_FakeClient.calls[0]["headers"]["X-Aliyun-Captcha-Verify-Region"], "sgp")
        self.assertEqual(tokens.await_count, 2)
        invalidate.assert_called_once()

    async def test_missing_captcha_token_is_not_sent_bare(self):
        """求解失败时不得裸打上游；耗尽后返回 captcha_required。"""
        account = Account.create("zai", "async-acc", "header.payload.signature")
        _FakeClient.calls = []
        tokens = AsyncMock(side_effect=RuntimeError("browser down"))
        ticket_id = "ticket-no-token"
        queue: asyncio.Queue = asyncio.Queue()
        async_pool._tickets[ticket_id] = {
            "status": "pending",
            "body": {"model": "GLM-5.3", "messages": []},
            "queue": queue,
            "created_at": 0,
        }

        with (
            patch.object(async_pool.store, "select", return_value=account),
            patch.object(async_pool.captcha_manager, "get_verify_param", tokens),
            patch.object(async_pool.captcha_manager, "invalidate"),
            patch.object(async_pool, "make_async_client", _FakeClient),
        ):
            await async_pool._process_ticket(ticket_id)

        events = []
        while not queue.empty():
            events.append(queue.get_nowait())
        async_pool._tickets.pop(ticket_id, None)

        self.assertEqual(len(_FakeClient.calls), 0)
        self.assertEqual(events[-1]["type"], "error")
        self.assertEqual(events[-1]["data"]["error"]["type"], "captcha_required")
        self.assertEqual(tokens.await_count, async_pool.MAX_CAPTCHA_RETRIES)


def _fake_request(body):
    """構造僅需實作 json() 的假 Request，供入口端點單元測試使用。"""
    request = Mock()
    request.json = AsyncMock(return_value=body)
    return request


class AsyncModelWhitelistTests(unittest.IsolatedAsyncioTestCase):
    """入口端點必須在建票前擋下白名單外的模型，不得轉發上游。"""

    def setUp(self):
        async_pool._tickets.clear()

    def tearDown(self):
        async_pool._tickets.clear()

    async def test_messages_rejects_model_outside_whitelist(self):
        body = {"model": "GLM-5-Turbo", "max_tokens": 8, "messages": []}
        with patch.object(async_pool, "_process_ticket", AsyncMock()):
            resp = await async_pool.async_messages(_fake_request(body))

        self.assertEqual(resp.status_code, 400)
        payload = json.loads(resp.body)
        self.assertEqual(payload["error"]["type"], "model_not_allowed")
        self.assertEqual(async_pool._tickets, {})

    async def test_messages_accepts_whitelisted_model(self):
        body = {"model": "glm-5.3-flash", "max_tokens": 8, "messages": []}
        with patch.object(async_pool, "_process_ticket", AsyncMock()):
            resp = await async_pool.async_messages(_fake_request(body))

        self.assertIsInstance(resp, StreamingResponse)
        self.assertEqual(len(async_pool._tickets), 1)
        self.assertEqual(async_pool._tickets[next(iter(async_pool._tickets))]["body"]["model"], "glm-5.3-flash")

    async def test_chat_completions_rejects_model_outside_whitelist(self):
        body = {"model": "glm-4.7", "messages": []}
        with patch.object(async_pool, "_process_ticket", AsyncMock()):
            resp = await async_pool.async_chat_completions(_fake_request(body))

        self.assertEqual(resp.status_code, 400)
        payload = json.loads(resp.body)
        self.assertEqual(payload["error"]["type"], "model_not_allowed")
        self.assertEqual(async_pool._tickets, {})

    async def test_chat_completions_defaults_to_whitelisted_model(self):
        """未帶 model 的舊客戶端應落到白名單內的預設模型，而非被拒。"""
        body = {"messages": []}
        with patch.object(async_pool, "_process_ticket", AsyncMock()):
            resp = await async_pool.async_chat_completions(_fake_request(body))

        self.assertIsInstance(resp, StreamingResponse)
        self.assertEqual(async_pool._tickets[next(iter(async_pool._tickets))]["body"]["model"], "GLM-5.3")
