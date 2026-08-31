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


if __name__ == "__main__":
    unittest.main()
