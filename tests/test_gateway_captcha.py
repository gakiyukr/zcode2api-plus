from __future__ import annotations

import unittest
from unittest.mock import AsyncMock, patch

import httpx

from app.captcha import CaptchaToken
from app.models import Account
from app.routes import gateway
from app.routes.gateway import _is_captcha_error, _is_model_concurrency_limit


class GatewayCaptchaClassificationTests(unittest.TestCase):
    def test_f001_is_a_captcha_challenge_for_client_errors(self):
        self.assertTrue(_is_captcha_error("F001", 400, {}))
        self.assertTrue(_is_captcha_error('{"error":"F001"}', 403, {}))

    def test_f001_on_unrelated_server_error_is_not_reclassified(self):
        self.assertFalse(_is_captcha_error("F001", 500, {}))

    def test_non_captcha_auth_error_stays_auth_error(self):
        self.assertFalse(_is_captcha_error("invalid token", 403, {}))

    def test_model_concurrency_3010_is_not_account_rate_limit(self):
        self.assertTrue(_is_model_concurrency_limit(429, '{"code":3010,"msg":"model admission concurrency limit exceeded"}'))
        self.assertTrue(_is_model_concurrency_limit(429, "model admission concurrency limit exceeded"))
        self.assertFalse(_is_model_concurrency_limit(429, '{"code":3007,"msg":"captcha"}'))
        self.assertFalse(_is_model_concurrency_limit(500, "model admission concurrency limit exceeded"))


class _Response:
    status_code = 200
    headers = httpx.Headers({"content-type": "application/json"})

    async def aiter_bytes(self):
        yield b'{"ok":true}'


class _Context:
    def __init__(self):
        self.response = _Response()

    async def __aenter__(self):
        return self.response

    async def __aexit__(self, *args):
        return False


class _Client:
    def __init__(self, *args, **kwargs):
        pass

    def stream(self, *args, **kwargs):
        return _Context()

    async def aclose(self):
        pass


class _SequenceResponse:
    def __init__(self, status_code, body):
        self.status_code = status_code
        self.headers = httpx.Headers({"content-type": "application/json"})
        self._body = body.encode()

    async def aread(self):
        return self._body

    async def aiter_bytes(self):
        yield self._body


class _SequenceContext:
    def __init__(self, response):
        self.response = response

    async def __aenter__(self):
        return self.response

    async def __aexit__(self, *args):
        return False


class _SequenceClient:
    responses = []
    calls = 0

    def __init__(self, *args, **kwargs):
        pass

    def stream(self, *args, **kwargs):
        response = type(self).responses[type(self).calls]
        type(self).calls += 1
        return _SequenceContext(response)

    async def aclose(self):
        pass


class GatewayCaptchaRetryTests(unittest.IsolatedAsyncioTestCase):
    async def test_solver_failure_refreshes_token_before_upstream_request(self):
        account = Account.create("zai", "retry", "header.payload.signature")
        tokens = AsyncMock(side_effect=[RuntimeError("temporary browser failure"), CaptchaToken("fresh-token", "sgp")])

        with (
            patch.object(gateway.captcha_manager, "get_verify_param", tokens),
            patch.object(gateway.captcha_manager, "invalidate") as invalidate,
            patch.object(gateway.httpx, "AsyncClient", _Client),
            patch.object(gateway.store, "update_account"),
            patch.object(gateway.asyncio, "create_task", side_effect=lambda coroutine: coroutine.close()),
        ):
            response = await gateway._try_account("req", account, {}, b"{}", {}, None, True)
            chunks = [chunk async for chunk in response.body_iterator]

        self.assertEqual(response.status_code, 200)
        self.assertEqual(chunks, [b'{"ok":true}'])
        self.assertEqual(tokens.await_count, 2)
        invalidate.assert_called_once()

    async def test_3010_retries_without_cooling_account(self):
        account = Account.create("zai", "busy", "header.payload.signature")
        _SequenceClient.responses = [
            _SequenceResponse(429, '{"code":3010,"msg":"model admission concurrency limit exceeded"}'),
            _SequenceResponse(200, '{"ok":true}'),
        ]
        _SequenceClient.calls = 0

        with (
            patch.object(gateway.captcha_manager, "get_verify_param", AsyncMock(return_value=CaptchaToken("token", "sgp"))),
            patch.object(gateway.httpx, "AsyncClient", _SequenceClient),
            patch.object(gateway.store, "update_account"),
            patch.object(gateway.asyncio, "sleep", AsyncMock()),
            patch.object(gateway.asyncio, "create_task", side_effect=lambda coroutine: coroutine.close()),
        ):
            response = await gateway._try_account("req", account, {}, b"{}", {}, None, True)
            chunks = [chunk async for chunk in response.body_iterator]

        self.assertEqual(response.status_code, 200)
        self.assertEqual(chunks, [b'{"ok":true}'])
        self.assertEqual(_SequenceClient.calls, 2)
        self.assertEqual(account.status, "active")


if __name__ == "__main__":
    unittest.main()
