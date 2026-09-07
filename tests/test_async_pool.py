"""async_pool 必须带上验证码，3007 时刷新令牌重试。"""

from __future__ import annotations

import asyncio
import json
import time
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


def _make_ticket(body):
    """建票但以 AsyncMock 頂替後台任務，避免測試真正打上游。"""
    with patch.object(async_pool, "_process_ticket", AsyncMock()):
        return async_pool._new_ticket(body)


class TicketLifecycleTests(unittest.IsolatedAsyncioTestCase):
    """ticket 防泄漏：客戶端斷開/超時後必須釋放 ticket 並中止後台任務。"""

    def setUp(self):
        async_pool._tickets.clear()

    def tearDown(self):
        async_pool._tickets.clear()

    async def test_client_disconnect_releases_ticket_and_cancels_task(self):
        """模擬客戶端中途斷開：生成器被關閉時 finally 應清 ticket 並取消後台任務。"""
        ticket_id = _make_ticket({"model": "GLM-5.3", "messages": []})
        fake_task = asyncio.create_task(asyncio.sleep(3600))
        async_pool._tickets[ticket_id]["task"] = fake_task

        gen = async_pool._ticket_sse(ticket_id)
        first = await gen.__anext__()
        self.assertIn("event: ticket", first)
        await gen.aclose()

        self.assertIsNone(async_pool._tickets.get(ticket_id))
        with self.assertRaises(asyncio.CancelledError):
            await fake_task

    async def test_sse_done_releases_ticket(self):
        """正常 done 事件後同樣要釋放 ticket，不得殘留。"""
        ticket_id = _make_ticket({"model": "GLM-5.3", "messages": []})
        queue = async_pool._tickets[ticket_id]["queue"]
        await queue.put({"type": "done"})

        events = [event async for event in async_pool._ticket_sse(ticket_id)]

        self.assertEqual(events[-1], "event: done\ndata: {}\n\n")
        self.assertIsNone(async_pool._tickets.get(ticket_id))

    async def test_release_ticket_ignores_unknown_id_and_finished_task(self):
        """未知 id 與已結束任務都應安全跳過。"""
        async_pool._release_ticket("no-such-ticket")  # 不應拋錯

        ticket_id = _make_ticket({"messages": []})
        done_task = asyncio.create_task(asyncio.sleep(0))
        await done_task
        async_pool._tickets[ticket_id]["task"] = done_task

        async_pool._release_ticket(ticket_id)
        self.assertIsNone(async_pool._tickets.get(ticket_id))
        self.assertTrue(done_task.done())

    async def test_sweep_removes_expired_orphans_and_keeps_fresh(self):
        """超過生命周期 + 寬限的孤兒 ticket 應被清掃，新建的不受影響。"""
        stale_id = "ticket-stale"
        async_pool._tickets[stale_id] = {
            "status": "pending",
            "body": {},
            "queue": asyncio.Queue(),
            "created_at": time.monotonic() - async_pool.settings.ASYNC_TICKET_TIMEOUT - 120.0,
        }
        fresh_id = _make_ticket({"messages": []})

        async_pool._sweep_expired_tickets()

        self.assertNotIn(stale_id, async_pool._tickets)
        self.assertIn(fresh_id, async_pool._tickets)


class AsyncPoolNetworkErrorTests(unittest.IsolatedAsyncioTestCase):
    """上游連線異常必須走到報錯路徑，不得因日誌呼叫炸掉 ticket。"""

    def setUp(self):
        async_pool._tickets.clear()

    def tearDown(self):
        async_pool._tickets.clear()

    async def test_network_error_delivers_error_event(self):
        account = Account.create("zai", "async-acc", "header.payload.signature")

        class _ExplodingClient:
            def __init__(self, *args, **kwargs):
                pass

            async def __aenter__(self):
                raise httpx.ConnectError("boom")

            async def __aexit__(self, *args):
                return False

        ticket_id = "ticket-neterr"
        queue: asyncio.Queue = asyncio.Queue()
        async_pool._tickets[ticket_id] = {
            "status": "pending",
            "body": {"model": "GLM-5.3", "messages": []},
            "queue": queue,
            "created_at": 0,
        }

        with (
            patch.object(async_pool.store, "select", return_value=account),
            patch.object(
                async_pool.captcha_manager,
                "get_verify_param",
                AsyncMock(return_value=CaptchaToken("t", "sgp")),
            ),
            patch.object(async_pool, "make_async_client", _ExplodingClient),
            patch.object(async_pool.settings, "ASYNC_MAX_RETRIES", 0),
        ):
            await async_pool._process_ticket(ticket_id)

        events = []
        while not queue.empty():
            events.append(queue.get_nowait())
        async_pool._tickets.pop(ticket_id, None)

        self.assertEqual(events[-1]["type"], "error")
        self.assertEqual(events[-1]["data"]["error"]["type"], "max_retries")


class _BreakMidStreamResponse:
    """按序 yield 假 SSE 行，遍歷結束後（或立即）令流中斷的假響應。"""

    status_code = 200
    headers = httpx.Headers({"content-type": "text/event-stream"})

    def __init__(self, lines, fail_immediately=False):
        self._lines = lines
        self._fail_immediately = fail_immediately

    async def aread(self):
        return b""

    async def aiter_lines(self):
        if self._fail_immediately:
            raise httpx.RemoteProtocolError("peer closed connection without sending complete message body")
        for line in self._lines:
            yield line
        raise httpx.RemoteProtocolError("peer closed connection without sending complete message body")


class MidStreamInterruptTests(unittest.IsolatedAsyncioTestCase):
    """流式轉發中斷的語義：發過 chunk 後不得換號重發（會產生重複事件）。"""

    def setUp(self):
        async_pool._tickets.clear()

    def tearDown(self):
        async_pool._tickets.clear()

    @staticmethod
    def _prepare_ticket():
        ticket_id = "ticket-midstream"
        queue: asyncio.Queue = asyncio.Queue()
        async_pool._tickets[ticket_id] = {
            "status": "pending",
            "body": {"model": "GLM-5.3", "messages": []},
            "queue": queue,
            "created_at": 0,
        }
        return ticket_id, queue

    async def test_mid_stream_failure_terminates_without_retry(self):
        """已發出 chunk 後流中斷：終止票務並回 error 事件，不得重發。"""
        account = Account.create("zai", "midstream", "header.payload.signature")
        _FakeClient.responses = [_BreakMidStreamResponse(['data: {"id":"msg1"}'])]
        _FakeClient.calls = []
        ticket_id, queue = self._prepare_ticket()

        with (
            patch.object(async_pool.store, "select", return_value=account),
            patch.object(
                async_pool.captcha_manager,
                "get_verify_param",
                AsyncMock(return_value=CaptchaToken("t", "sgp")),
            ),
            patch.object(async_pool, "make_async_client", _FakeClient),
        ):
            await async_pool._process_ticket(ticket_id)

        events = []
        while not queue.empty():
            events.append(queue.get_nowait())
        async_pool._tickets.pop(ticket_id, None)

        self.assertEqual([e["type"] for e in events], ["ready", "chunk", "error"])
        self.assertEqual(events[1]["data"], {"id": "msg1"})
        self.assertEqual(events[2]["data"]["error"]["type"], "upstream_stream_interrupted")
        self.assertEqual(len(_FakeClient.calls), 1)

    async def test_failure_before_first_chunk_still_retries(self):
        """一個 chunk 都沒發出時中斷：仍按網絡錯誤走換號重試路徑。"""
        account = Account.create("zai", "prestream", "header.payload.signature")
        _FakeClient.responses = [_BreakMidStreamResponse([], fail_immediately=True)]
        _FakeClient.calls = []
        ticket_id, queue = self._prepare_ticket()

        with (
            patch.object(async_pool.store, "select", return_value=account),
            patch.object(
                async_pool.captcha_manager,
                "get_verify_param",
                AsyncMock(return_value=CaptchaToken("t", "sgp")),
            ),
            patch.object(async_pool, "make_async_client", _FakeClient),
            patch.object(async_pool.settings, "ASYNC_MAX_RETRIES", 0),
        ):
            await async_pool._process_ticket(ticket_id)

        events = []
        while not queue.empty():
            events.append(queue.get_nowait())
        async_pool._tickets.pop(ticket_id, None)

        self.assertEqual(events[-1]["type"], "error")
        self.assertEqual(events[-1]["data"]["error"]["type"], "max_retries")
        self.assertEqual(len(_FakeClient.calls), 1)
