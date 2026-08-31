from __future__ import annotations

import unittest
from unittest.mock import patch

from app import quota, settings
from app.models import Account, Status


class _QuotaResponse:
    status_code = 200

    def __init__(self, payload: dict):
        self._payload = payload

    def json(self):
        return self._payload


class _QuotaClient:
    calls: list[dict] = []
    payload: dict = {}

    def __init__(self, *args, **kwargs):
        pass

    async def __aenter__(self):
        return self

    async def __aexit__(self, *args):
        return False

    async def get(self, url, *, headers, params):
        type(self).calls.append({"url": url, "headers": headers, "params": params})
        return _QuotaResponse(type(self).payload)


def _balance_payload(*, flash_remaining: int = 2_500_000) -> dict:
    return {
        "code": 0,
        "data": {
            "plans": [
                {
                    "plan_id": "start-plan",
                    "entitlements": [
                        {
                            "entitlement_id": "ent-flash",
                            "show_name": "GLM-5.3-Flash",
                            "period": "daily",
                            "grant_units": 5_000_000,
                        }
                    ],
                }
            ],
            "balances": [
                {
                    "entitlement_id": "ent-flash",
                    "show_name": "GLM-5.3-Flash",
                    "total_units": 5_000_000,
                    "used_units": 5_000_000 - flash_remaining,
                    "remaining_units": flash_remaining,
                    "available_units": flash_remaining,
                    "period_start": 1_788_105_600,
                    "period_end": 1_788_191_999,
                    "expires_at": 1_788_191_999,
                }
            ],
        },
    }


class QuotaQueryTests(unittest.IsolatedAsyncioTestCase):
    async def test_daily_quota_uses_device_identity_and_preserves_period(self):
        """額度端點必須帶完整裝置資訊，並保存每日配額與重置週期。"""
        account = Account.create("zai", "daily", "header.payload.signature")
        _QuotaClient.calls = []
        _QuotaClient.payload = _balance_payload()

        with (
            patch.object(quota, "make_async_client", _QuotaClient),
            patch.object(quota, "get_device_mid", return_value="device-mid"),
            patch.object(quota.store, "update_account"),
        ):
            result = await quota._fetch_quota_once(account)

        self.assertNotIn("error", result)
        call = _QuotaClient.calls[0]
        self.assertEqual(call["headers"]["X-Device-Mid"], "device-mid")
        self.assertEqual(call["headers"]["X-Platform"], settings.ZCODE_CLIENT_PLATFORM)
        self.assertEqual(call["params"]["platform"], settings.ZCODE_CLIENT_PLATFORM)
        daily = account.quota["GLM-5.3-Flash"]
        self.assertEqual(daily["total"], 5_000_000)
        self.assertEqual(daily["remaining"], 2_500_000)
        self.assertEqual(daily["period"], "daily")
        self.assertEqual(daily["period_end"], 1_788_191_999)

    async def test_zero_daily_balance_marks_account_exhausted(self):
        """所有官方餘額均為零時，帳號不可再參與輪詢。"""
        account = Account.create("zai", "empty", "header.payload.signature")
        _QuotaClient.calls = []
        _QuotaClient.payload = _balance_payload(flash_remaining=0)

        with (
            patch.object(quota, "make_async_client", _QuotaClient),
            patch.object(quota, "get_device_mid", return_value="device-mid"),
            patch.object(quota.store, "update_account"),
        ):
            await quota._fetch_quota_once(account)

        self.assertEqual(account.status, Status.EXHAUSTED)
        self.assertEqual(account.last_error, "額度已用完")


if __name__ == "__main__":
    unittest.main()
