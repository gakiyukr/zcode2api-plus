"""claim 模組測試：preview 解析、領取業務碼翻譯與 3007 換碼重試。"""

from __future__ import annotations

import unittest
from unittest.mock import patch

from app import claim as claim_mod
from app.models import Account


class _Response:
    def __init__(self, status_code=200, payload=None, text=""):
        self.status_code = status_code
        self._payload = payload or {}
        self.text = text

    def json(self):
        return self._payload


class _Client:
    """billing 請求樁：按序回放 _queue 中的回應。"""

    calls: list[dict] = []
    queue: list[_Response] = []

    def __init__(self, *args, **kwargs):
        pass

    async def __aenter__(self):
        return self

    async def __aexit__(self, *args):
        return False

    async def request(self, method, url, **kwargs):
        type(self).calls.append({"method": method, "url": url, **kwargs})
        return type(self).queue.pop(0)


def _jwt_account(name: str = "a1") -> Account:
    return Account.create(
        "zai", name,
        "eyJhbGciOiJIUzI1NiJ9."
        "eyJ1c2VyX2lkIjoiMTIzNDU2NzgtMTIzNC0xMjM0LTEyMzQtMTIzNDU2Nzg5MDEyIn0.sig",
    )


def _token():
    from app.captcha import CaptchaToken

    return CaptchaToken(verify_param="vp", region="cn")


class TestParsePlan(unittest.TestCase):
    def test_extracts_token_grants(self):
        plan = claim_mod.parse_plan({
            "plan_id": "p1", "name": "限时体验", "priority": 10,
            "entitlements": [
                {"meter": "model_usage", "unit_type": "token",
                 "show_name": "GLM-4.6", "grant_units": 300000},
                {"meter": "request", "unit_type": "count", "show_name": "忽略"},
            ],
        })
        assert plan is not None
        self.assertEqual(plan["plan_id"], "p1")
        self.assertEqual(plan["grants"], [
            {"name": "GLM-4.6", "units": 300000.0, "period": "one_time"},
        ])

    def test_returns_none_without_plan_id(self):
        self.assertIsNone(claim_mod.parse_plan({"name": "x"}))


class TestClaim(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        _Client.calls = []
        _Client.queue = []

    async def test_claim_success_with_retry_on_3007(self):
        acc = _jwt_account()
        _Client.queue = [
            _Response(payload={"code": 3007, "msg": "captcha"}),
            _Response(payload={"code": 0, "data": {}}),
        ]
        with patch.object(claim_mod, "make_async_client", _Client), \
                patch.object(claim_mod.captcha_manager, "get_verify_param",
                             side_effect=[_token(), _token()]):
            result = await claim_mod.claim(acc, "plan-x")
        self.assertEqual(result["plan_id"], "plan-x")
        self.assertEqual(len(_Client.calls), 2)
        second = _Client.calls[1]
        self.assertEqual(
            second["headers"]["X-Aliyun-Captcha-Verify-Param"], "vp")
        self.assertEqual(second["json"], {"plan_id": "plan-x"})

    async def test_claim_translates_business_code(self):
        acc = _jwt_account()
        _Client.queue = [_Response(payload={"code": 1003, "msg": "dup"})]
        with patch.object(claim_mod, "make_async_client", _Client), \
                patch.object(claim_mod.captcha_manager, "get_verify_param",
                             return_value=_token()):
            with self.assertRaises(claim_mod.ClaimError) as ctx:
                await claim_mod.claim(acc, "plan-x")
        self.assertIn("領取過", str(ctx.exception))
        self.assertIn("dup", str(ctx.exception))

    async def test_claim_rejects_api_key_account(self):
        acc = Account.create("zai", "k1", "sk-xxx")
        with self.assertRaises(claim_mod.ClaimError):
            await claim_mod.claim(acc)

    async def test_auto_claim_reports_activation_and_claims_all(self):
        acc = _jwt_account()
        _Client.queue = [
            _Response(payload={"code": 0, "data": {"plans": [
                {"plan_id": "p2", "name": "B", "priority": 1},
                {"plan_id": "p1", "name": "A", "priority": 5},
            ]}}),
            _Response(payload={"code": 0}),
            _Response(payload={"code": 0}),
        ]
        with patch.object(claim_mod, "make_async_client", _Client), \
                patch.object(claim_mod.captcha_manager, "get_verify_param",
                             side_effect=[_token(), _token()]), \
                patch.object(claim_mod, "report_activation_events",
                             return_value=None):
            outcomes = await claim_mod.auto_claim_all_plans(acc)
        self.assertTrue(all(o["ok"] for o in outcomes))
        # 按優先級降序：先領 p1 再領 p2
        self.assertEqual([o["plan_id"] for o in outcomes], ["p1", "p2"])

    async def test_auto_claim_skips_api_key_account(self):
        acc = Account.create("zai", "k1", "sk-xxx")
        self.assertEqual(await claim_mod.auto_claim_all_plans(acc), [])


if __name__ == "__main__":
    unittest.main()
