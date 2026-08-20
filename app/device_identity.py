"""设备身份管理（固定 Device-Mid，避免频繁更换被风控）。

从 TriDefender/zcode-api 移植：
- 首次启动生成 UUIDv4
- 持久化到 data/device_mid.txt
- 后续启动复用同一 UUID
"""

from __future__ import annotations

from . import settings


def get_device_mid() -> str:
    """返回固定的设备 UUID（已在 settings 初始化时生成/加载）。"""
    return settings.DEVICE_MID
