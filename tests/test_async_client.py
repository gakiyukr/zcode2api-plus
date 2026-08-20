"""测试 Async Off-Peak 客户端。"""

from __future__ import annotations

import unittest

from app.async_routes.client import create_off_peak_client
from app.async_routes.types import OffPeakServerError


class OffPeakClientTests(unittest.TestCase):
    def test_create_off_peak_client(self):
        client = create_off_peak_client(
            origin="https://zcode.z.ai",
            jwt="fake.jwt.token",
            coding_plan_api_key="fake_key",
        )
        self.assertEqual(client.origin, "https://zcode.z.ai")
        self.assertEqual(client.jwt, "fake.jwt.token")
        self.assertEqual(client.coding_plan_api_key, "fake_key")

    def test_off_peak_server_error(self):
        err = OffPeakServerError("Test error", 503, "test_code")
        self.assertEqual(str(err), "Test error")
        self.assertEqual(err.http_status, 503)
        self.assertEqual(err.biz_code, "test_code")

    def test_off_peak_client_headers(self):
        client = create_off_peak_client(
            origin="https://zcode.z.ai",
            jwt="test_jwt",
            coding_plan_api_key="test_key",
        )
        headers = client._build_headers(has_body=True)
        self.assertEqual(headers["authorization"], "Bearer test_jwt")
        self.assertEqual(headers["x-coding-plan-api-key"], "test_key")
        self.assertEqual(headers["content-type"], "application/json")
