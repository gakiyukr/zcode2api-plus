"""测试模型列表。"""

from __future__ import annotations

import unittest

from app.routes.gateway import AVAILABLE_MODELS
from app.models import Account


class AvailableModelsTests(unittest.TestCase):
    def test_available_models_includes_new_models(self):
        expected = {
            "glm-4.5-air",
            "glm-4.6",
            "glm-4.6v",
            "glm-4.7",
            "glm-5",
            "glm-5-turbo",
            "glm-5v-turbo",
            "glm-5.1",
            "GLM-5.2",
            "GLM-5.3",
            "GLM-5-Turbo",
        }
        self.assertEqual(set(AVAILABLE_MODELS), expected)

    def test_available_models_count(self):
        self.assertEqual(len(AVAILABLE_MODELS), 11)


class AccountIdentityTests(unittest.TestCase):
    def test_public_view_exposes_email_and_trial_plan(self):
        """帳號視圖應提供郵箱與體驗套餐標記供管理介面辨識。"""
        account = Account.create("zai", "oauth-login", "header.payload.signature")
        account.email = "user@example.com"
        account.plan = {"plan_id": "free_trial", "name": "Coding Plan"}

        view = account.public_view()

        self.assertEqual(view["email"], "user@example.com")
        self.assertEqual(view["plan_name"], "Coding Plan")
        self.assertTrue(view["plan_is_trial"])

    def test_trial_detection_handles_nested_entitlements(self):
        """方案包含額度陣列時，體驗判斷不得遞迴失控。"""
        account = Account.create("zai", "nested", "header.payload.signature")
        account.plan = {"entitlements": [{"plan_type": "trial"}]}

        self.assertTrue(account.public_view()["plan_is_trial"])

    def test_plan_view_covers_all_subscriptions(self):
        """多訂閱帳號的方案名稱應串接，體驗判定須涵蓋每一筆訂閱。"""
        account = Account.create("zai", "multi-plan", "header.payload.signature")
        account.plans = [
            {"plan_id": "global-build", "name": "ZCode Global Build"},
            {"plan_id": "start-plan", "name": "ZCode Start Plan"},
        ]
        account.plan = account.plans[0]

        view = account.public_view()

        self.assertEqual(view["plan_name"], "ZCode Global Build / ZCode Start Plan")
        self.assertFalse(view["plan_is_trial"])

        account.plans.append({"plan_id": "trial", "name": "體驗套餐"})
        self.assertTrue(account.public_view()["plan_is_trial"])


class ModelAvailabilityTests(unittest.TestCase):
    def test_availability_spans_all_plan_rows(self):
        """同名模型存在多訂閱額度列時，任一列有餘額即視為可用。"""
        account = Account.create("zai", "rows", "header.payload.signature")
        account.quota = {
            "GLM-5.3-Flash · Global": {"model": "GLM-5.3-Flash", "remaining": 100},
            "GLM-5.3-Flash · Start": {"model": "GLM-5.3-Flash", "remaining": 0},
        }
        account.sync_exhausted_models()

        self.assertEqual(account.model_availability("GLM-5.3-Flash"), "available")
        self.assertEqual(account.exhausted_models, [])

        account.quota["GLM-5.3-Flash · Global"]["remaining"] = 0
        account.sync_exhausted_models()

        self.assertEqual(account.model_availability("GLM-5.3-Flash"), "exhausted")
        self.assertEqual(account.exhausted_models, ["glm-5.3-flash"])

    def test_legacy_quota_snapshot_without_model_field(self):
        """舊版快照（鍵即模型名、無 model 欄位）仍可正確比對額度。"""
        account = Account.create("zai", "legacy", "header.payload.signature")
        account.quota = {"GLM-5.3-Flash": {"remaining": 5}}

        self.assertEqual(account.quota_entries_for_model("glm-5.3-flash"), [{"remaining": 5}])
        self.assertEqual(account.model_availability("GLM-5.3-Flash"), "available")
