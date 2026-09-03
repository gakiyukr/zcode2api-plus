from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

from app import settings
from app.models import Account, Status
from app.store import Store


class ModelAwareSelectionTests(unittest.TestCase):
    def setUp(self):
        self._temp = tempfile.TemporaryDirectory()
        self._old_data_dir = settings.DATA_DIR
        self._old_db_path = settings.DB_PATH
        settings.DATA_DIR = Path(self._temp.name)
        settings.DB_PATH = settings.DATA_DIR / "accounts.db"
        self.store = Store()

    def tearDown(self):
        settings.DATA_DIR = self._old_data_dir
        settings.DB_PATH = self._old_db_path
        self._temp.cleanup()

    def test_selection_prefers_positive_quota_over_unknown_and_zero(self):
        """請求指定模型時，應優先選擇仍有餘額的帳號。"""
        zero = self.store.add_account("zai", "zero", "zero-key")
        unknown = self.store.add_account("zai", "unknown", "unknown-key")
        positive = self.store.add_account("zai", "positive", "positive-key")
        zero.quota = {"GLM-5.3": {"remaining": 0}}
        positive.quota = {"GLM-5.3": {"remaining": 100}}

        self.assertIs(self.store.select("zai", model="glm-5.3"), positive)
        self.assertIs(self.store.select("zai", skip_ids={positive.id}, model="glm-5.3"), unknown)

    def test_selection_skips_all_accounts_when_model_is_exhausted(self):
        """所有帳號的指定模型均耗盡時，不應回退調用該模型。"""
        first = self.store.add_account("zai", "first", "first-key")
        second = self.store.add_account("zai", "second", "second-key")
        for account in (first, second):
            account.quota = {"GLM-5.3": {"remaining": 0}}
            account.sync_exhausted_models()

        self.assertIsNone(self.store.select("zai", model="GLM-5.3"))
        self.assertEqual(first.status, Status.ACTIVE)

    def test_model_exhaustion_is_persisted(self):
        """單模型耗盡標記應隨帳號資料重新載入。"""
        account = self.store.add_account("zai", "persist", "persist-key")
        account.mark_model_exhausted("GLM-5.3-Flash")
        self.store.update_account(account)

        restored = Store().find_any(account.id)

        self.assertIsNotNone(restored)
        self.assertEqual(restored.exhausted_models, ["glm-5.3-flash"])
        self.assertEqual(restored.model_availability("GLM-5.3-Flash"), "exhausted")

    def test_selection_excludes_accounts_without_model_entitlement(self):
        """快照中未提供此模型的帳號，不得再收到該模型的請求。"""
        has_flash = self.store.add_account("zai", "has-flash", "has-flash-key")
        only_53 = self.store.add_account("zai", "only-53", "only-53-key")
        has_flash.quota = {
            "GLM-5.3": {"model": "GLM-5.3", "remaining": 100},
            "GLM-5.3-Flash": {"model": "GLM-5.3-Flash", "remaining": 50},
        }
        only_53.quota = {"GLM-5.3": {"model": "GLM-5.3", "remaining": 100}}

        self.assertEqual(only_53.model_availability("GLM-5.3-Flash"), "absent")
        self.assertEqual(only_53.model_availability("GLM-5.3"), "available")
        # 有 5.3-Flash 額度的帳號優先承接 5.3-Flash 請求
        self.assertIs(self.store.select("zai", model="GLM-5.3-Flash"), has_flash)
        # 僅有 5.3 額度的帳號仍可服務 5.3 請求
        self.assertIs(self.store.select("zai", skip_ids={has_flash.id}, model="GLM-5.3"), only_53)
        # 所有帳號都不提供此模型時直接拒絕，不得回退亂派
        self.assertIsNone(self.store.select("zai", skip_ids={has_flash.id}, model="GLM-5.3-Flash"))

    def test_manual_model_disable_overrides_positive_quota(self):
        """手動停用模型後，即使仍有餘額也不得選中該帳號。"""
        disabled = self.store.add_account("zai", "disabled-model", "disabled-model-key")
        fallback = self.store.add_account("zai", "fallback", "fallback-key")
        disabled.quota = {"GLM-5.3": {"remaining": 1_000}}
        fallback.quota = {"GLM-5.3": {"remaining": 500}}
        disabled.set_disabled_models(["GLM_5.3", "glm-5.3"])

        self.assertEqual(disabled.disabled_models, ["glm-5.3"])
        self.assertEqual(disabled.model_availability("GLM-5.3"), "disabled")
        self.assertIs(self.store.select("zai", model="GLM-5.3"), fallback)

    def test_manual_model_disable_survives_export_and_import(self):
        """帳號匯出再匯入後，手動停用模型設定必須保留。"""
        account = self.store.add_account("zai", "portable", "portable-key")
        account.set_disabled_models(["GLM-5.3", "GLM-5-Turbo"])
        self.store.update_account(account)
        payload = self.store.export()

        imported_temp = tempfile.TemporaryDirectory()
        try:
            settings.DATA_DIR = Path(imported_temp.name)
            settings.DB_PATH = settings.DATA_DIR / "accounts.db"
            imported_store = Store()
            imported_store.import_accounts(payload)
            restored = imported_store.list_accounts("zai")[0]
            self.assertEqual(restored.disabled_models, ["glm-5.3", "glm-5-turbo"])
        finally:
            imported_temp.cleanup()


if __name__ == "__main__":
    unittest.main()
