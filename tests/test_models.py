"""閘門模型白名單測試：僅清單內模型可轉發上游。"""

from __future__ import annotations

import unittest

from app.routes.gateway import AVAILABLE_MODELS, MODEL_NAME_MAP, _model_allowed
from app.models import Account


class AvailableModelsTests(unittest.TestCase):
    def test_available_models_only_keeps_53_family(self):
        """模型清單僅保留 5.3 系列，其餘模型不再對外公布。"""
        self.assertEqual(AVAILABLE_MODELS, ["glm-5.3-flash", "GLM-5.3"])

    def test_available_models_count(self):
        self.assertEqual(len(AVAILABLE_MODELS), 2)

    def test_model_whitelist_accepts_listed_models(self):
        """清單內模型（含大小寫/底線變體）應允許調用。"""
        self.assertTrue(_model_allowed("GLM-5.3"))
        self.assertTrue(_model_allowed("glm-5.3"))
        self.assertTrue(_model_allowed("glm-5.3-flash"))
        self.assertTrue(_model_allowed("GLM-5.3-Flash"))
        self.assertTrue(_model_allowed("glm_5.3_flash"))

    def test_model_whitelist_rejects_unlisted_models(self):
        """清單外模型一律拒絕，不得轉發上游。"""
        self.assertFalse(_model_allowed("glm-4.7"))
        self.assertFalse(_model_allowed("glm-5"))
        self.assertFalse(_model_allowed("GLM-5.2"))
        self.assertFalse(_model_allowed("glm-5-turbo"))
        self.assertFalse(_model_allowed(""))
        self.assertFalse(_model_allowed(None))

    def test_normalize_maps_alias_before_check(self):
        """別名映射後的模型仍應通過白名單檢查（glm-5.3 → GLM-5.3）。"""
        self.assertEqual(MODEL_NAME_MAP.get("glm-5.3"), "GLM-5.3")
        self.assertTrue(_model_allowed(MODEL_NAME_MAP.get("glm-5.3")))


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

    def test_missing_model_in_snapshot_is_absent_not_unknown(self):
        """已有額度快照但無此模型列時，應回傳 absent 而非 unknown。"""
        account = Account.create("zai", "absent", "header.payload.signature")
        account.quota = {"GLM-5.3": {"model": "GLM-5.3", "remaining": 10}}
        self.assertEqual(account.model_availability("GLM-5.3-Flash"), "absent")

        account.quota = {}
        self.assertEqual(account.model_availability("GLM-5.3-Flash"), "unknown")


if __name__ == "__main__":
    unittest.main()
