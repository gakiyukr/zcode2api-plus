from __future__ import annotations

import json
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
    body = b'{"ok":true}'

    async def aread(self):
        return self.body

    async def aiter_bytes(self):
        yield self.body


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
            response = await gateway._try_account("req", account, {}, {}, None, True)
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
            response = await gateway._try_account("req", account, {}, {}, None, True)
            chunks = [chunk async for chunk in response.body_iterator]

        self.assertEqual(response.status_code, 200)
        self.assertEqual(chunks, [b'{"ok":true}'])
        self.assertEqual(_SequenceClient.calls, 2)
        self.assertEqual(account.status, "active")

    async def test_http_200_quota_error_marks_only_requested_model(self):
        """上游以 HTTP 200 回傳 code 1005 時只耗盡請求模型。"""
        account = Account.create("zai", "quota", "header.payload.signature")
        account.quota = {
            "GLM-5.3": {"remaining": 1_000},
            "GLM-5.3-Flash": {"remaining": 0},
        }
        _SequenceClient.responses = [
            _SequenceResponse(200, '{"code":1005,"msg":"exceed quota limit"}'),
        ]
        _SequenceClient.calls = 0

        with (
            patch.object(gateway.httpx, "AsyncClient", _SequenceClient),
            patch.object(gateway.store, "update_account"),
            patch.object(gateway.asyncio, "create_task", side_effect=lambda coroutine: coroutine.close()),
        ):
            response = await gateway._try_account(
                "req", account, {"stream": True, "model": "GLM-5.3-Flash"}, {}, None, False
            )

        self.assertIs(response, gateway._NEXT_ACCOUNT)
        self.assertEqual(account.status, "active")
        self.assertEqual(account.last_error, "GLM-5.3-Flash 每日額度已用完")
        self.assertEqual(account.exhausted_models, ["glm-5.3-flash"])

    async def test_http_503_cools_account_and_switches(self):
        """上游 503 時帳號應進入冷卻，避免重複調用同一出口。"""
        account = Account.create("zai", "unavailable", "header.payload.signature")
        _SequenceClient.responses = [_SequenceResponse(503, '{"message":"upstream unavailable"}')]
        _SequenceClient.calls = 0

        with (
            patch.object(gateway.httpx, "AsyncClient", _SequenceClient),
            patch.object(gateway.store, "update_account"),
        ):
            response = await gateway._try_account(
                "req", account, {"stream": True, "model": "GLM-5.3"}, {}, None, False
            )

        self.assertIs(response, gateway._NEXT_ACCOUNT)
        self.assertEqual(account.status, "cooling")
        self.assertGreater(account.cooling_until or 0, 0)


class _UsageResponse:
    status_code = 200
    headers = httpx.Headers({"content-type": "application/json"})
    body = b'{"id":"msg_1","usage":{"input_tokens":11,"output_tokens":22}}'

    async def aread(self):
        return self.body

    async def aiter_bytes(self):
        yield self.body


class _UsageContext:
    def __init__(self):
        self.response = _UsageResponse()

    async def __aenter__(self):
        return self.response

    async def __aexit__(self, *args):
        return False


class _UsageClient:
    def __init__(self, *args, **kwargs):
        pass

    def stream(self, *args, **kwargs):
        return _UsageContext()

    async def aclose(self):
        pass


class GatewayTokenStatsTests(unittest.IsolatedAsyncioTestCase):
    async def test_success_stream_accumulates_token_usage(self):
        account = Account.create("zai", "tokens", "header.payload.signature")
        with (
            patch.object(gateway.captcha_manager, "get_verify_param", AsyncMock(return_value=CaptchaToken("token", "sgp"))),
            patch.object(gateway.httpx, "AsyncClient", _UsageClient),
            patch.object(gateway.store, "update_account") as update,
            patch.object(gateway.asyncio, "create_task", side_effect=lambda coroutine: coroutine.close()),
        ):
            response = await gateway._try_account("req", account, {}, {}, None, True)
            chunks = [chunk async for chunk in response.body_iterator]

        self.assertEqual(response.status_code, 200)
        self.assertEqual(chunks, [_UsageResponse.body])
        usage = account.public_view()["total_tokens"]
        self.assertEqual(usage["input"], 11)
        self.assertEqual(usage["output"], 22)
        update.assert_called()


class GatewayErrorClassificationTests(unittest.IsolatedAsyncioTestCase):
    """上游错误按「状态码 + 官方业务码」分类；未识别信号原样透传，不做关键词猜测。"""

    @staticmethod
    def _prepare(responses):
        _SequenceClient.responses = responses
        _SequenceClient.calls = 0
        return _SequenceClient

    async def test_business_code_extraction(self):
        self.assertEqual(gateway._upstream_business_code('{"code":1310,"msg":"cap"}'), "1310")
        self.assertEqual(
            gateway._upstream_business_code('{"error":{"message":"Authentication Failed","type":"1000"}}'),
            "1000",
        )
        self.assertIsNone(gateway._upstream_business_code("not json"))
        self.assertIsNone(gateway._upstream_business_code('{"error":"flat"}'))

    async def test_401_with_quota_words_marks_invalid(self):
        """鉴权失败必须先于额度判定：401 body 即使含 quota 字样也判 invalid 而非耗尽。"""
        account = Account.create("zai", "auth", "header.payload.signature")
        client = self._prepare(
            [_SequenceResponse(401, '{"error":{"message":"insufficient quota","type":"1000"}}')]
        )

        with (
            patch.object(gateway.httpx, "AsyncClient", client),
            patch.object(gateway.store, "update_account"),
        ):
            response = await gateway._try_account(
                "req", account, {"stream": True, "model": "GLM-5.3"}, {}, None, False
            )

        self.assertIs(response, gateway._NEXT_ACCOUNT)
        self.assertEqual(account.status, "invalid")
        self.assertEqual(account.exhausted_models, [])

    async def test_401_empty_body_marks_invalid(self):
        """实测 JWT 上游的 401 为空 body（Content-Length: 0），仍应判 invalid。"""
        account = Account.create("zai", "auth-empty", "header.payload.signature")
        client = self._prepare([_SequenceResponse(401, "")])

        with (
            patch.object(gateway.httpx, "AsyncClient", client),
            patch.object(gateway.store, "update_account"),
        ):
            response = await gateway._try_account(
                "req", account, {"stream": True, "model": "GLM-5.3"}, {}, None, False
            )

        self.assertIs(response, gateway._NEXT_ACCOUNT)
        self.assertEqual(account.status, "invalid")

    async def test_402_exhausts_requested_model(self):
        account = Account.create("zai", "payment", "header.payload.signature")
        account.quota = {
            "GLM-5.3": {"remaining": 1_000},
            "GLM-5.3-Flash": {"remaining": 1_000},
        }
        client = self._prepare([_SequenceResponse(402, '{"error":{"message":"payment required"}}')])

        with (
            patch.object(gateway.httpx, "AsyncClient", client),
            patch.object(gateway.store, "update_account"),
            patch.object(gateway.asyncio, "create_task", side_effect=lambda coroutine: coroutine.close()),
        ):
            response = await gateway._try_account(
                "req", account, {"stream": True, "model": "GLM-5.3"}, {}, None, False
            )

        self.assertIs(response, gateway._NEXT_ACCOUNT)
        self.assertEqual(account.status, "active")
        self.assertEqual(account.exhausted_models, ["glm-5.3"])

    async def test_429_official_quota_code_exhausts_model(self):
        """429 + 官方用量上限码族（如 1310）按模型耗尽处理，账号保持可用。"""
        account = Account.create("zai", "cap", "header.payload.signature")
        account.quota = {
            "GLM-5.3": {"remaining": 1_000},
            "GLM-5.3-Flash": {"remaining": 1_000},
        }
        client = self._prepare([_SequenceResponse(429, '{"code":1310,"msg":"usage cap reached"}')])

        with (
            patch.object(gateway.httpx, "AsyncClient", client),
            patch.object(gateway.store, "update_account"),
            patch.object(gateway.asyncio, "create_task", side_effect=lambda coroutine: coroutine.close()),
        ):
            response = await gateway._try_account(
                "req", account, {"stream": True, "model": "GLM-5.3"}, {}, None, False
            )

        self.assertIs(response, gateway._NEXT_ACCOUNT)
        self.assertEqual(account.status, "active")
        self.assertEqual(account.exhausted_models, ["glm-5.3"])

    async def test_429_rate_limit_cools_account(self):
        """429 + 瞬时限流码（1302）走冷却换号，不误标耗尽。"""
        account = Account.create("zai", "ratelimit", "header.payload.signature")
        client = self._prepare([_SequenceResponse(429, '{"code":1302,"msg":"rate limited"}')])

        with (
            patch.object(gateway.httpx, "AsyncClient", client),
            patch.object(gateway.store, "update_account"),
        ):
            response = await gateway._try_account(
                "req", account, {"stream": True, "model": "GLM-5.3"}, {}, None, False
            )

        self.assertIs(response, gateway._NEXT_ACCOUNT)
        self.assertEqual(account.status, "cooling")
        self.assertEqual(account.exhausted_models, [])

    async def test_unknown_upstream_error_is_passed_through_verbatim(self):
        """未识别的上游错误（如 500 且 body 含 quota 字样）不再误标账号，原样透传。"""
        account = Account.create("zai", "boom", "header.payload.signature")
        upstream_body = '{"error":{"message":"internal boom","quota_hint":"yes"}}'
        client = self._prepare([_SequenceResponse(500, upstream_body)])

        with (
            patch.object(gateway.httpx, "AsyncClient", client),
            patch.object(gateway.store, "update_account"),
        ):
            response = await gateway._try_account(
                "req", account, {"stream": True, "model": "GLM-5.3"}, {}, None, False
            )

        self.assertEqual(response.status_code, 500)
        self.assertEqual(json.loads(response.body), json.loads(upstream_body))
        self.assertEqual(account.status, "active")
        self.assertEqual(account.exhausted_models, [])


if __name__ == "__main__":
    unittest.main()
