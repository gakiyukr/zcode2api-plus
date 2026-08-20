"""测试模型列表。"""

from __future__ import annotations

import unittest

from app.routes.gateway import AVAILABLE_MODELS


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
