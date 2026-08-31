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
