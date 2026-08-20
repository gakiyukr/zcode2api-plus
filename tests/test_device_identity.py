"""测试设备身份管理。"""

from __future__ import annotations

import unittest
import uuid

from app.device_identity import get_device_mid
from app import settings


class DeviceIdentityTests(unittest.TestCase):
    def test_get_device_mid_is_settings_uuid(self):
        mid = get_device_mid()
        self.assertEqual(mid, settings.DEVICE_MID)
        uuid.UUID(mid)
