"""套餐領取（Z.AI billing/preview + billing/claim）。

鏈路與 zcode-switch claim.rs 同形：
  1. GET  {BILLING_BASE}/billing/preview?app_version=&platform= → data.plans[]
  2. 領取需阿里雲無痕驗證碼：CaptchaManager 求解 → X-Aliyun-Captcha-Verify-Param
  3. POST {BILLING_BASE}/billing/claim  body {"plan_id":...}（+ 可選 Verify-Region 頭）

上游業務碼語義（沿用 zcode-switch 映射）：1001 套餐不存在 / 1002 活動結束 /
1003 已領取過 / 1004 不符合條件 / 1005 今日名額用完 / 3001 參數錯誤 /
3007 驗證碼失敗（換驗證碼重試一次）/ 401 未登入。

請求一律走 make_async_client，使每賬號代理設定對領取流量同樣生效。
"""

from __future__ import annotations

import base64
import json

import httpx

from . import logs, settings
from .captcha import captcha_manager
from .models import Account
from .proxy import make_async_client
from .quota import _auth_headers


class ClaimError(Exception):
    """業務失敗（含上游 code 語義），message 面向使用者。"""


_CLAIM_FAIL = {
    1001: "套餐不存在",
    1002: "活動已結束或套餐暫不可領取",
    1003: "該套餐已經領取過",
    1004: "不符合領取條件",
    1005: "今日領取名額已用完",
    3001: "領取參數錯誤，請重新整理後重試",
    3007: "驗證碼校驗失敗，請重試",
    401: "請先登入後再領取",
}

_CAPTCHA_HEADER = "X-Aliyun-Captcha-Verify-Param"
_CAPTCHA_REGION_HEADER = "X-Aliyun-Captcha-Verify-Region"


def _fail_message(code: int, body: dict) -> str:
    base = _CLAIM_FAIL.get(code, "領取失敗")
    server = body.get("msg") or body.get("message") or ""
    return f"{base}（{server}）" if server else base


def _business_code(body: dict) -> int:
    code = body.get("code")
    try:
        return int(code) if code is not None else -1
    except (TypeError, ValueError):
        return -1


def parse_plan(raw: dict) -> dict | None:
    """提取可領取套餐（plan_id/name/描述/優先級 + model_usage token 授權項）。"""
    plan_id = str(raw.get("plan_id") or raw.get("planId") or "").strip()
    if not plan_id:
        return None
    grants = []
    for ent in raw.get("entitlements") or []:
        if ent.get("meter") != "model_usage" or ent.get("unit_type") != "token":
            continue
        name = str(ent.get("show_name") or ent.get("showName") or "").strip()
        if not name:
            continue
        units = ent.get("grant_units", ent.get("grantUnits")) or 0
        grants.append({
            "name": name,
            "units": float(units),
            "period": ent.get("period") or "one_time",
        })
    return {
        "plan_id": plan_id,
        "name": str(raw.get("name") or "").strip(),
        "description": str(raw.get("description") or "").strip(),
        "priority": raw.get("priority") or 0,
        "grants": grants,
    }


async def _billing_request(account: Account, method: str, path: str, **kwargs) -> dict:
    headers = dict(kwargs.pop("headers"))
    try:
        async with make_async_client(account, timeout=25) as client:
            res = await client.request(
                method, f"{settings.ZCODE_BILLING_BASE}{path}",
                headers=headers, **kwargs,
            )
    except httpx.HTTPError as err:
        # 連線/逾時等網路故障統一轉業務錯誤：路由層只需面對 ClaimError 一種失敗
        raise ClaimError(f"上游網路錯誤: {err}") from err
    if res.status_code in (401, 403):
        text = (res.text or "").lower()
        if "captcha" not in text and "verify" not in text:
            raise ClaimError(f"鑑權失敗 HTTP {res.status_code}")
    try:
        body = res.json()
    except ValueError:
        raise ClaimError(f"上游回應非 JSON HTTP {res.status_code}") from None
    return body


def jwt_user_id(account: Account) -> str | None:
    """JWT payload 的 user_id（官方客戶端事件上報以 user_id 標識使用者）。

    user_id 優先，sub 兜底（兩者同為 36 位 uuid）；解析失敗回 None。
    """
    token = (account.jwt_token or "").strip()
    if not token:
        return None
    try:
        seg = token.split(".")[1]
        payload = json.loads(base64.urlsafe_b64decode(seg + "=" * (-len(seg) % 4)))
    except (IndexError, ValueError):
        return None
    uid = payload.get("user_id") or payload.get("sub")
    if not isinstance(uid, str) or not uid.strip():
        return None
    return uid.strip()


async def report_activation_events(account: Account) -> str | None:
    """上報官方客戶端激活事件（app_launch + app_daily_active），回傳錯誤或 None。

    preview 前模擬桌面端當日活躍（疑似活動套餐投放資格信號）。任何失敗僅
    回傳文案，不阻斷 preview；首個失敗即中止（日活鍵在上游按
    device_mid+日期去重，重試無意義）。
    """
    from .telemetry import ACTIVATION_ELEMENTS, post_activation_event

    user_id = jwt_user_id(account)
    if not user_id:
        return "JWT 無 user_id，跳過激活上報"
    for element in ACTIVATION_ELEMENTS:
        try:
            await post_activation_event(user_id, element)
        except (httpx.HTTPError, RuntimeError) as err:
            return f"激活事件 {element} 上報失敗: {err}"
    return None


async def preview_plans(account: Account) -> list[dict]:
    """拉取賬號當前可領取套餐，按優先級降序。"""
    body = await _billing_request(
        account, "GET", "/billing/preview",
        headers=_auth_headers(account),
        # 官方客戶端 preview 對 platform 參數寬容；client/configs 才拒收。
        params={"app_version": settings.ZCODE_CLIENT_VERSION,
                "platform": settings.ZCODE_CLIENT_PLATFORM},
    )
    code = _business_code(body)
    if code != 0:
        raise ClaimError(_fail_message(code, body))
    raw_plans = (body.get("data") or {}).get("plans") or []
    plans = [parsed for parsed in (parse_plan(p) for p in raw_plans) if parsed]
    plans.sort(key=lambda p: (-p["priority"], p["plan_id"]))
    return plans


async def _auto_pick_plan(account: Account, plan_id: str | None) -> tuple[str, str, list]:
    """plan_id 為空時 preview 自動選優先級最高套餐。回傳 (plan_id, plan_name, grants)。"""
    if plan_id:
        return plan_id, "", []
    plans = await preview_plans(account)
    if not plans:
        raise ClaimError("沒有待領取的套餐")
    best = plans[0]
    return best["plan_id"], best["name"] or best["plan_id"], best["grants"]


def _claim_headers(account: Account, verify_param: str, region: str | None) -> dict:
    """billing/claim 客戶端請求頭形態（asar claimManualPlan）。

    實測缺版本/平台頭時即使驗證碼有效也 3007；X-Device-Mid 由 _auth_headers
    提供，此處顯式兜底防止基座頭漂移。
    """
    headers = _auth_headers(account)
    headers[_CAPTCHA_HEADER] = verify_param
    if region and region.strip():
        headers[_CAPTCHA_REGION_HEADER] = region.strip()
    headers["X-ZCode-App-Version"] = settings.ZCODE_CLIENT_VERSION
    headers["X-Platform"] = settings.ZCODE_CLIENT_PLATFORM
    return headers


async def claim(account: Account, plan_id: str | None = None) -> dict:
    """領取套餐。plan_id 缺省時自動選優先級最高的可領套餐。

    回傳 {"plan_id", "plan_name", "grants"}；3007（驗證碼失敗）自動換碼重試一次。
    """
    if not (account.mode == "jwt" and account.jwt_token):
        raise ClaimError("僅 Coding Plan (JWT) 賬號支持領取")

    plan_id, plan_name, grants = await _auto_pick_plan(account, plan_id)
    last_err: ClaimError | None = None
    for attempt in (1, 2):
        token = await captcha_manager.get_verify_param()
        if token is None:
            raise ClaimError("驗證碼求解失敗或已停用，請到後台驗證碼頁面回填參數")
        headers = _claim_headers(account, token.verify_param, token.region)

        body = await _billing_request(
            account, "POST", "/billing/claim",
            headers=headers, json={"plan_id": plan_id},
        )
        code = _business_code(body)
        if code == 0:
            return {"plan_id": plan_id, "plan_name": plan_name, "grants": grants}
        if code == 3007 and attempt == 1:
            logs.warn("claim", f"賬號 {account.name} 驗證碼被拒，換碼重試")
            captcha_manager.invalidate()
            last_err = ClaimError(_fail_message(code, body))
            continue
        raise ClaimError(_fail_message(code, body))
    raise last_err or ClaimError("領取失敗")


async def auto_claim_all_plans(account: Account) -> list[dict]:
    """新賬號入池自動領取：激活上報 + 逐個領取全部可領套餐。

    入池鏈路的 fire-and-forget 收尾：任何失敗只記日誌/回傳 outcome，絕不拋出
    （入池流程不受影響）。重複執行安全（上游 1003 已領取過冪等）。
    """
    if not (account.mode == "jwt" and account.jwt_token):
        return []
    outcomes: list[dict] = []

    try:
        err = await report_activation_events(account)
        if err:
            logs.warn("claim", f"賬號 {account.name} 激活上報失敗: {err}")
    except Exception as err:  # noqa: BLE001 - 激活失敗不阻斷領取
        logs.warn("claim", f"賬號 {account.name} 激活上報異常: {err}")

    try:
        plans = await preview_plans(account)
    except ClaimError as err:
        logs.info("claim", f"賬號 {account.name} 無可領套餐（{err}）")
        return outcomes
    except Exception as err:  # noqa: BLE001
        logs.warn("claim", f"賬號 {account.name} preview 異常: {err}")
        return outcomes

    if not plans:
        logs.info("claim", f"賬號 {account.name} 上游無投放套餐，跳過領取")
        return outcomes

    for plan in plans:
        try:
            result = await claim(account, plan["plan_id"])
            outcomes.append({"account_id": account.id, "account_name": account.name,
                             "ok": True, **result})
            logs.ok("claim", f"賬號 {account.name} 自動領取成功: "
                             f"{result.get('plan_name') or plan['plan_id']}")
        except ClaimError as err:
            outcomes.append({"account_id": account.id, "account_name": account.name,
                             "ok": False, "plan_id": plan["plan_id"], "message": str(err)})
            logs.warn("claim", f"賬號 {account.name} 自動領取 {plan['plan_id']} 失敗: {err}")
        except Exception as err:  # noqa: BLE001
            logs.warn("claim", f"賬號 {account.name} 自動領取異常: {err}")
    return outcomes
