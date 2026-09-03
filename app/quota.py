"""ZCode 额度 / 余额 / 用量查询，以及账号状态判定。

在查询基础上提供「额度用完自动标记 exhausted」的监控能力。
"""

from __future__ import annotations

import asyncio
import time

import httpx

from . import logs, settings
from .device_identity import get_device_mid
from .models import Account, Status, is_trial_plan, plan_text
from .proxy import make_async_client
from .store import store


def _auth_headers(account: Account) -> dict:
    headers = {
        "Content-Type": "application/json",
        "User-Agent": settings.USER_AGENT,
        "X-ZCode-App-Version": settings.ZCODE_CLIENT_VERSION,
        "X-Platform": settings.ZCODE_CLIENT_PLATFORM,
        "X-Device-Mid": get_device_mid(),
        "HTTP-Referer": "https://zcode.z.ai/",
    }
    if account.mode == "jwt" and account.jwt_token:
        headers["Authorization"] = f"Bearer {account.jwt_token}"
    elif account.api_key:
        headers["x-api-key"] = account.api_key
    return headers


_QUOTA_CACHE_TTL_SECONDS = 15.0
_quota_inflight: dict[str, asyncio.Task[dict]] = {}
_quota_cache: dict[str, tuple[float, dict]] = {}

_SUM_FIELDS = ("total", "used", "remaining", "available")
_EARLIEST_FIELDS = ("period_start", "period_end", "expires_at")


def _sum_units(current: object, incoming: object):
    """數值額度合併：任一端缺值時保留另一端，避免 None 參與加總。"""
    if current is None:
        return incoming
    if incoming is None:
        return current
    return current + incoming


def _earliest_ts(current: object, incoming: object):
    """週期時間合併取最早者，作為額度恢復或到期的保守估計。"""
    if current is None:
        return incoming
    if incoming is None:
        return current
    try:
        return min(current, incoming)
    except TypeError:
        return current


def _merge_quota_entry(current: dict | None, incoming: dict) -> dict:
    """同名模型出現在多個訂閱時，合併為單一加總快照而非互相覆蓋。"""
    if current is None:
        return incoming
    for key in _SUM_FIELDS:
        current[key] = _sum_units(current.get(key), incoming.get(key))
    for key in _EARLIEST_FIELDS:
        current[key] = _earliest_ts(current.get(key), incoming.get(key))
    periods = {period for period in (current.get("period"), incoming.get("period")) if period}
    current["period"] = "+".join(sorted(periods)) or None
    return current


async def _fetch_quota_once(account: Account) -> dict:
    """拉取官方客户端使用的套餐与模型余额，写回账号状态并持久化。"""
    headers = _auth_headers(account)
    url = f"{settings.ZCODE_BILLING_BASE}/billing/balance"
    account.last_checked_at = time.time()

    try:
        async with make_async_client(account, timeout=20) as client:
            response = await client.get(
                url,
                headers=headers,
                params={
                    "app_version": settings.ZCODE_CLIENT_VERSION,
                    "platform": settings.ZCODE_CLIENT_PLATFORM,
                },
            )
    except httpx.HTTPError as err:
        account.last_error = f"额度查询网络错误: {err}"
        store.update_account(account)
        return {"error": account.last_error}

    if response.status_code in (401, 403):
        account.status = Status.INVALID
        account.last_error = f"鉴权失败 HTTP {response.status_code}"
        store.update_account(account)
        return {"error": account.last_error}
    if response.status_code != 200:
        if response.status_code == 405 and account.quota:
            account.last_error = None
            store.update_account(account)
            return {"cached": True, "reason": "上游额度接口拒绝了重复查询（HTTP 405）"}
        account.last_error = f"额度查询失败 HTTP {response.status_code}"
        store.update_account(account)
        return {"error": account.last_error}

    try:
        payload = response.json()
    except ValueError:
        account.last_error = "额度查询返回了无效 JSON"
        store.update_account(account)
        return {"error": account.last_error}
    if payload.get("code") not in (None, 0):
        account.last_error = (payload.get("msg") or f"额度查询失败 code={payload.get('code')}").strip()
        store.update_account(account)
        return {"balance": payload, "error": account.last_error}

    data = payload.get("data") or {}
    plans = data.get("plans") or []
    balances = data.get("balances") or []
    account.plans = [plan for plan in plans if isinstance(plan, dict)]
    account.plan = account.plans[0] if account.plans else {}

    # balance 僅提供當期數值；週期與所屬方案需由 entitlement 對應回來。
    entitlements: dict[str, dict] = {}
    entitlement_plans: dict[str, dict] = {}
    for plan in account.plans:
        for entitlement in (plan.get("entitlements") or []):
            if isinstance(entitlement, dict) and entitlement.get("entitlement_id"):
                entitlements[entitlement["entitlement_id"]] = entitlement
                entitlement_plans[entitlement["entitlement_id"]] = plan

    # 同名模型在多個訂閱各自獨立一列（每日刷新的體驗套餐與限時活動套餐不得混合）；
    # 僅同一訂閱內的重複項目才合併加總。
    multi_plan = len(account.plans) > 1
    quota_map: dict = {}
    for balance in balances:
        name = balance.get("show_name") or balance.get("model") or "model"
        entitlement_id = balance.get("entitlement_id")
        entitlement = entitlements.get(entitlement_id, {})
        plan = entitlement_plans.get(entitlement_id)
        plan_name = plan_text(plan) if (multi_plan and plan) else ""
        key = f"{name} · {plan_name}" if plan_name else name
        quota_map[key] = _merge_quota_entry(quota_map.get(key), {
            "total": balance.get("total_units"),
            "used": balance.get("used_units"),
            "remaining": balance.get("remaining_units"),
            "available": balance.get("available_units"),
            "period": entitlement.get("period"),
            "period_start": balance.get("period_start"),
            "period_end": balance.get("period_end"),
            "expires_at": balance.get("expires_at"),
            "model": name,
            "plan_name": plan_name,
            "plan_is_trial": is_trial_plan(plan) if plan else False,
        })

    if not quota_map:
        account.quota = {}
        account.last_error = "账号未返回可用套餐额度"
        store.update_account(account)
        return {"balance": payload, "error": account.last_error}

    account.quota = quota_map
    account.sync_exhausted_models()
    remainings = [
        quota.get("remaining") for quota in quota_map.values()
        if quota.get("remaining") is not None
    ]
    if remainings and all((remaining or 0) <= 0 for remaining in remainings):
        account.status = Status.EXHAUSTED
        account.last_error = "額度已用完"
    else:
        if account.status in (Status.EXHAUSTED, Status.INVALID):
            account.status = Status.ACTIVE
            account.cooling_until = None
        elif account.status == Status.COOLING and account.cooling_until and account.cooling_until <= time.time():
            account.status = Status.ACTIVE
            account.cooling_until = None
        if account.status != Status.COOLING:
            account.last_error = None

    store.update_account(account)
    return {"balance": payload}


async def _fetch_and_cache(account: Account) -> dict:
    result = await _fetch_quota_once(account)
    if "error" not in result:
        _quota_cache[account.id] = (time.monotonic(), result)
    return result


async def fetch_quota(account: Account) -> dict:
    """合并同账号并发查询，并短暂复用成功结果以避免触发上游限流。"""
    inflight = _quota_inflight.get(account.id)
    if inflight is not None:
        return await asyncio.shield(inflight)

    cached = _quota_cache.get(account.id)
    if cached is not None:
        cached_at, result = cached
        if time.monotonic() - cached_at < _QUOTA_CACHE_TTL_SECONDS:
            return {**result, "cached": True}
        _quota_cache.pop(account.id, None)

    task = asyncio.create_task(_fetch_and_cache(account))
    _quota_inflight[account.id] = task

    def _clear_inflight(done: asyncio.Task[dict]) -> None:
        if _quota_inflight.get(account.id) is done:
            _quota_inflight.pop(account.id, None)

    task.add_done_callback(_clear_inflight)
    return await asyncio.shield(task)


async def refresh_accounts(accounts: list[Account]) -> dict:
    """并发刷新一批账号，返回汇总。"""
    if not accounts:
        return {"ok": 0, "fail": 0}
    sem = asyncio.Semaphore(8)

    async def _one(acc: Account) -> bool:
        async with sem:
            res = await fetch_quota(acc)
            return "error" not in res

    results = await asyncio.gather(*[_one(a) for a in accounts], return_exceptions=True)
    ok = sum(1 for r in results if r is True)
    return {"ok": ok, "fail": len(accounts) - ok}


class QuotaMonitor:
    """后台周期性刷新可管理账号的额度，实现实时用量监控。"""

    def __init__(self) -> None:
        self._task: asyncio.Task | None = None
        self._stop = asyncio.Event()

    async def _loop(self) -> None:
        # 启动后先等几秒，避免与服务启动争抢
        try:
            await asyncio.wait_for(self._stop.wait(), timeout=5)
            return
        except asyncio.TimeoutError:
            pass

        while not self._stop.is_set():
            interval = store.quota_refresh_interval()  # 实时读取设置，改后即生效
            if interval > 0:
                try:
                    accounts = [
                        a for a in store.list_accounts("zai")
                        if a.mode == "jwt" and a.status != Status.DISABLED
                    ]
                    if accounts:
                        await refresh_accounts(accounts)
                except Exception as err:  # noqa: BLE001 - 后台任务需吞掉异常继续运行
                    logs.err("quota", f"后台刷新出错: {err}")
            # interval<=0 视为关闭：仍周期性回看设置，便于随时启用
            wait = interval if interval > 0 else 30
            try:
                await asyncio.wait_for(self._stop.wait(), timeout=wait)
            except asyncio.TimeoutError:
                continue

    def start(self) -> None:
        if self._task is None:
            self._stop.clear()
            self._task = asyncio.create_task(self._loop())

    async def stop(self) -> None:
        self._stop.set()
        if self._task:
            await asyncio.gather(self._task, return_exceptions=True)
            self._task = None


monitor = QuotaMonitor()
