"""激活遙測上報 —— 官方 event/report 端點的單一事實源。

活動套餐投放疑似以「官方客戶端當日活躍」為資格信號，preview 前模擬
app_launch / app_daily_active 兩個事件（與 zcode-switch claim_refresh 同形）。
事件體欄位集固定為官方 sendReport 的 16 欄；端點不校驗登入態，
因此請求不帶 Authorization。

本專案不引入 dengyie 版的完整設備指紋池：档案欄位直接採宿主機真實
平台資料（真機事實即合規形態），device_mid 沿用 device_identity 的
本機持久化標識。
"""

from __future__ import annotations

import os
import platform
import sys
import uuid

import httpx

from .device_identity import get_device_mid
from . import settings

EVENT_REPORT_URL = os.getenv(
    "ZCODE_EVENT_REPORT_URL", "https://zcode.z.ai/api/v1/event/report"
)
ACTIVATION_ELEMENTS = ("app_launch", "app_daily_active")
# 桌面端常見解析度；上游僅做形態校驗，固定值即可
ACTIVATION_SCREEN = "2560x1440"

_TIMEOUT = 10


def _os_version() -> str:
    """os.release() 語義的版本字串：windows 取 platform.version()（10.0.build）。"""
    if sys.platform == "win32":
        return platform.version()
    return platform.release()


def _timezone() -> str:
    """本機 IANA 時區名；取不到時回退 UTC（上報失敗不阻斷 preview）。"""
    tz = os.getenv("TZ", "").strip()
    if tz:
        return tz
    tzfile = "/etc/timezone"
    try:
        with open(tzfile, "r", encoding="utf-8") as fh:
            name = fh.read().strip()
            if name:
                return name
    except OSError:
        pass
    return "UTC"


def _language() -> str:
    """本機 locale 語言標籤（LANG / LC_ALL / Windows 用戶預設）。"""
    for var in ("LC_ALL", "LC_MESSAGES", "LANG"):
        raw = os.getenv(var, "").strip()
        if raw:
            return raw.split(".")[0].replace("_", "-")
    if sys.platform == "win32":
        try:
            import ctypes

            windll = ctypes.windll.kernel32  # type: ignore[attr-defined]
            lang = windll.GetUserDefaultUILanguage()
            if lang:
                return platform.windows_locale.get(lang, "en-US")  # type: ignore[attr-defined]
        except Exception:  # noqa: BLE001 - 僅影響事件欄位品質
            pass
    return "en-US"


def build_activation_event_body(element: str, user_id: str) -> dict:
    """激活事件體（官方 sendReport 欄位集固定為這 16 個）。"""
    return {
        "event_id": str(uuid.uuid4()),
        "client_timezone": _timezone(),
        "client_language": _language(),
        "element_name": element,
        "event_region": "app",
        "event_type": "view",
        "event_text": "",
        "event_extra_detail": {},
        "user_id": user_id,
        "screen_resolution": ACTIVATION_SCREEN,
        "app_version": settings.ZCODE_CLIENT_VERSION,
        "device_os_category": (
            "windows" if sys.platform == "win32"
            else "macos" if sys.platform == "darwin" else "linux"
        ),
        "device_os_version": _os_version(),
        "device_mid": get_device_mid(),
        "mac_id": "",
        "marketing_params": "{}",
    }


def business_code(body: object) -> int:
    """上游業務碼；非物件 JSON / 缺 code / 非數字 → -1（視為失敗）。"""
    if not isinstance(body, dict):
        return -1
    try:
        return int(body.get("code", -1))
    except (TypeError, ValueError):
        return -1


async def post_activation_event(user_id: str, element: str,
                                timeout: float = _TIMEOUT) -> None:
    """單條激活事件上報（無 Authorization，官方端點不校驗登入態）。

    HTTP >= 400 或業務碼非 0 拋 RuntimeError，文案含定位資訊；
    httpx.HTTPError 原樣上拋 —— 容錯策略（中止/繼續）由呼叫方定。
    """
    async with httpx.AsyncClient(timeout=timeout) as client:
        res = await client.post(
            EVENT_REPORT_URL,
            headers={"Content-Type": "application/json"},
            json=build_activation_event_body(element, user_id),
        )
    if res.status_code >= 400:
        raise RuntimeError(f"event/report {element} HTTP {res.status_code}: {res.text[:120]}")
    try:
        body = res.json()
    except ValueError:
        body = None
    code = business_code(body)
    if code != 0:
        raise RuntimeError(f"event/report {element} 業務碼異常({code}): {res.text[:120]}")
