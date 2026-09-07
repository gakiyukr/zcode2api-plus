"""金鑰引導與網關鑑權測試：後台密碼／網關 API Key 永不為空、永不為固定預設值。"""

from __future__ import annotations

import asyncio
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from fastapi import HTTPException, Request

from app import settings
from app.auth_admin import _FAILED_ATTEMPTS, verify_admin_key, verify_gateway_key
from app.routes import admin_api
from app.store import Store


def _admin_request(host: str = "127.0.0.1") -> Request:
    """構造僅含 client 位址的假 Request，供直接呼叫後台鑑權依賴使用。"""
    return Request({"type": "http", "client": (host, 12345), "headers": []})


class AuthBootstrapTestBase(unittest.TestCase):
    """以臨時資料庫與隔離的環境變數設定建立 Store，避免汙染實際資料。"""

    def setUp(self):
        self._temp = tempfile.TemporaryDirectory()
        self._old_data_dir = settings.DATA_DIR
        self._old_db_path = settings.DB_PATH
        self._old_admin_env = settings.ADMIN_KEY_ENV
        self._old_gateway_env = settings.GATEWAY_KEY_ENV
        settings.DATA_DIR = Path(self._temp.name)
        settings.DB_PATH = settings.DATA_DIR / "accounts.db"
        settings.ADMIN_KEY_ENV = ""
        settings.GATEWAY_KEY_ENV = ""
        self._stores: list[Store] = []

    def make_store(self) -> Store:
        """建立登錄於本測試案例的 Store，tearDown 時統一關閉以釋放檔案鎖。"""
        store = Store()
        self._stores.append(store)
        return store

    def tearDown(self):
        settings.DATA_DIR = self._old_data_dir
        settings.DB_PATH = self._old_db_path
        settings.ADMIN_KEY_ENV = self._old_admin_env
        settings.GATEWAY_KEY_ENV = self._old_gateway_env
        for store in self._stores:
            store.close()
        self._temp.cleanup()


class BootstrapGenerationTests(AuthBootstrapTestBase):
    def test_fresh_store_generates_random_admin_key(self):
        """全新資料庫應隨機生成後台密碼，而非眾所周知的固定預設值。"""
        store = self.make_store()
        key = store.admin_key()
        self.assertTrue(key)
        self.assertNotEqual(key, "zcode")
        self.assertEqual(store.generated_admin_key, key)

    def test_fresh_store_generates_gateway_key(self):
        """全新資料庫應隨機生成網關 API Key，空值不再代表免鑑權。"""
        store = self.make_store()
        key = store.gateway_key()
        self.assertTrue(key.startswith("sk-"))
        self.assertEqual(store.generated_gateway_key, key)

    def test_legacy_default_admin_key_is_rotated_on_upgrade(self):
        """存量部署遺留的預設密碼 zcode 與空网关密鑰應在啟動時強制輪換。"""
        old = self.make_store()
        old.set_setting("admin_key", "zcode")
        old.set_setting("gateway_key", "")

        store = self.make_store()
        self.assertTrue(store.admin_key())
        self.assertNotEqual(store.admin_key(), "zcode")
        self.assertTrue(store.gateway_key())
        self.assertEqual(store.generated_admin_key, store.admin_key())
        self.assertEqual(store.generated_gateway_key, store.gateway_key())

    def test_env_var_is_respected_for_missing_keys(self):
        """環境變數顯式配置時應作為初始值寫入，且不標記為隨機生成。"""
        settings.ADMIN_KEY_ENV = "env-admin-key"
        settings.GATEWAY_KEY_ENV = "env-gateway-key"

        store = self.make_store()
        self.assertEqual(store.admin_key(), "env-admin-key")
        self.assertEqual(store.gateway_key(), "env-gateway-key")
        self.assertIsNone(store.generated_admin_key)
        self.assertIsNone(store.generated_gateway_key)

    def test_custom_existing_keys_are_preserved(self):
        """管理者已自訂的金鑰不受升級影響，亦不觸發輪換。"""
        old = self.make_store()
        old.set_setting("admin_key", "my-secret")
        old.set_setting("gateway_key", "sk-my-gateway")

        store = self.make_store()
        self.assertEqual(store.admin_key(), "my-secret")
        self.assertEqual(store.gateway_key(), "sk-my-gateway")
        self.assertIsNone(store.generated_admin_key)
        self.assertIsNone(store.generated_gateway_key)

    def test_bootstrap_is_idempotent(self):
        """重複開啟同一資料庫不應重新生成或輪換金鑰。"""
        first = self.make_store()
        admin, gateway = first.admin_key(), first.gateway_key()

        second = self.make_store()
        self.assertEqual(second.admin_key(), admin)
        self.assertEqual(second.gateway_key(), gateway)
        self.assertIsNone(second.generated_admin_key)
        self.assertIsNone(second.generated_gateway_key)


class GatewayKeyVerificationTests(AuthBootstrapTestBase):
    def setUp(self):
        super().setUp()
        self.store = self.make_store()
        _FAILED_ATTEMPTS.clear()

    def test_missing_key_header_is_rejected(self):
        """未攜帶 API Key 的請求必須被拒絕，不得放行。"""
        with patch("app.auth_admin.store", self.store):
            with self.assertRaises(HTTPException) as ctx:
                asyncio.run(verify_gateway_key(authorization=None, x_api_key=None))
        self.assertEqual(ctx.exception.status_code, 401)

    def test_wrong_key_is_rejected(self):
        with patch("app.auth_admin.store", self.store):
            with self.assertRaises(HTTPException) as ctx:
                asyncio.run(verify_gateway_key(authorization="Bearer wrong", x_api_key=None))
        self.assertEqual(ctx.exception.status_code, 403)

    def test_correct_key_passes(self):
        key = self.store.gateway_key()
        with patch("app.auth_admin.store", self.store):
            asyncio.run(verify_gateway_key(authorization=f"Bearer {key}", x_api_key=None))
            asyncio.run(verify_gateway_key(authorization=None, x_api_key=key))

    def test_empty_stored_key_fails_closed(self):
        """資料庫金鑰被手動清空時應拒絕服務，而非回到免鑑權狀態。"""
        self.store.set_setting("gateway_key", "")
        with patch("app.auth_admin.store", self.store):
            with self.assertRaises(HTTPException) as ctx:
                asyncio.run(verify_gateway_key(authorization=None, x_api_key=None))
        self.assertEqual(ctx.exception.status_code, 503)

    def test_admin_verification_still_fails_closed_without_key(self):
        """後台密碼缺失時維持 401，不放行管理介面。"""
        self.store.set_setting("admin_key", "")
        with patch("app.auth_admin.store", self.store):
            with self.assertRaises(HTTPException) as ctx:
                asyncio.run(verify_admin_key(_admin_request(), authorization=None))
        self.assertEqual(ctx.exception.status_code, 401)

    def test_app_key_query_param_is_removed(self):
        """`?app_key=` URL 傳參鑑權已移除，防止金鑰落入代理與訪問日誌。"""
        with self.assertRaises(TypeError):
            asyncio.run(verify_admin_key(authorization=None, app_key="zcode"))


class SettingsEndpointTests(AuthBootstrapTestBase):
    def setUp(self):
        super().setUp()
        self.store = self.make_store()

    def test_update_settings_rejects_empty_gateway_key(self):
        """設定端點必須拒絕空網關密鑰，避免意外清空後回到裸奔狀態。"""
        with patch("app.routes.admin_api.store", self.store):
            with self.assertRaises(HTTPException) as ctx:
                asyncio.run(admin_api.update_settings({"gateway_key": "  "}))
        self.assertEqual(ctx.exception.status_code, 400)
        self.assertTrue(self.store.gateway_key())

    def test_update_settings_accepts_valid_gateway_key(self):
        with patch("app.routes.admin_api.store", self.store):
            result = asyncio.run(admin_api.update_settings({"gateway_key": "sk-new"}))
        self.assertEqual(result, {"ok": True})
        self.assertEqual(self.store.gateway_key(), "sk-new")


class AdminRateLimitTests(AuthBootstrapTestBase):
    """後台密鑰連續失敗應觸發 429 限速；成功校驗清空失敗記錄。"""

    def setUp(self):
        super().setUp()
        self.store = self.make_store()
        _FAILED_ATTEMPTS.clear()

    def tearDown(self):
        _FAILED_ATTEMPTS.clear()
        super().tearDown()

    def test_repeated_failures_are_rate_limited(self):
        """窗口內失敗達上限後，即使換上正確密鑰也一律 429。"""
        key = self.store.admin_key()
        with patch("app.auth_admin.store", self.store):
            for _ in range(10):
                with self.assertRaises(HTTPException) as ctx:
                    asyncio.run(verify_admin_key(_admin_request(), authorization="Bearer wrong"))
                self.assertEqual(ctx.exception.status_code, 401)
            with self.assertRaises(HTTPException) as ctx:
                asyncio.run(verify_admin_key(_admin_request(), authorization=f"Bearer {key}"))
            self.assertEqual(ctx.exception.status_code, 429)

    def test_success_clears_failure_record(self):
        """成功校驗清空失敗記錄，不誤傷正常使用者。"""
        with patch("app.auth_admin.store", self.store):
            for _ in range(3):
                with self.assertRaises(HTTPException):
                    asyncio.run(verify_admin_key(_admin_request(), authorization="Bearer wrong"))
            key = self.store.admin_key()
            asyncio.run(verify_admin_key(_admin_request(), authorization=f"Bearer {key}"))
            asyncio.run(verify_admin_key(_admin_request(), authorization=f"Bearer {key}"))


if __name__ == "__main__":
    unittest.main()
