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
