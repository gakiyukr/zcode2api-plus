"""后台管理 API：/admin/api/*（账号池、设置、用量监控）。"""

from __future__ import annotations

import csv
import time
import os

from fastapi import APIRouter, Body, Depends, HTTPException
from fastapi.responses import JSONResponse

from .. import settings
from ..auth_admin import verify_admin_key
from ..captcha import captcha_manager
from ..models import PROVIDERS, Status
from ..oauth import ZaiAuthFlow, extract_user_email
from ..proxy import make_async_client, normalize_proxy_url
from ..quota import fetch_quota, refresh_accounts
from ..store import store

router = APIRouter(prefix="/admin/api", dependencies=[Depends(verify_admin_key)])

_STARTED_AT = time.time()

# 以不可预测的 flow_id/state 关联后台发起的 OAuth 会话。
_login_flows: dict[str, ZaiAuthFlow] = {}
_login_results: dict[str, dict] = {}
_LOGIN_TTL_SECONDS = 600


def _cleanup_login_flows() -> None:
    now = time.monotonic()
    expired = [flow_id for flow_id, flow in _login_flows.items()
               if now - flow.created_at > _LOGIN_TTL_SECONDS]
    for flow_id in expired:
        _login_flows.pop(flow_id, None)
        _login_results.pop(flow_id, None)


# ── 鉴权探针 ─────────────────────────────────────────────────────────────────
@router.get("/verify")
async def verify():
    return {"status": "ok"}


# ── 账号列表 + 概览统计 ──────────────────────────────────────────────────────
def _account_snapshot() -> tuple[list[dict], dict]:
    accounts = [a.public_view() for a in store.list_accounts()]
    stats = {"total": len(accounts), "active": 0, "exhausted": 0,
             "cooling": 0, "invalid": 0, "disabled": 0,
             "calls": 0, "fail": 0,
             "tokens_in": 0, "tokens_out": 0, "tokens_cache": 0}
    for a in accounts:
        st = a["status"]
        if st in stats:
            stats[st] += 1
        stats["calls"] += a["use_count"]
        stats["fail"] += a["fail_count"]
        total_tokens = a.get("total_tokens") or {}
        stats["tokens_in"] += total_tokens.get("input") or 0
        stats["tokens_out"] += total_tokens.get("output") or 0
        stats["tokens_cache"] += (total_tokens.get("cache_creation") or 0) + (total_tokens.get("cache_read") or 0)
    return accounts, stats


@router.get("/accounts")
async def list_accounts():
    accounts, stats = _account_snapshot()
    return {
        "accounts": accounts,
        "stats": stats,
        "providers": list(PROVIDERS),
        "proxies": store.list_proxy_profiles(),
        "ts": time.time(),
    }


@router.get("/status")
async def status_info():
    return {
        "providers": list(PROVIDERS),
        "gateway_key_set": bool(store.gateway_key()),
        "quota_pool": {
            p: sum(1 for a in store.list_accounts(p) if a.is_selectable())
            for p in PROVIDERS
        },
    }


@router.get("/proxies")
async def list_proxies():
    """列出可供帳號選用的命名代理線路。"""
    return {"profiles": store.list_proxy_profiles()}


@router.post("/proxies")
async def add_proxy(payload: dict = Body(...)):
    try:
        profile = store.add_proxy_profile(
            payload.get("name", ""), payload.get("url", ""), payload.get("enabled", True)
        )
    except ValueError as err:
        raise HTTPException(400, str(err)) from err
    return profile


@router.put("/proxies/{profile_id}")
async def update_proxy(profile_id: str, payload: dict = Body(...)):
    try:
        profile = store.update_proxy_profile(
            profile_id,
            payload.get("name", ""),
            payload.get("url", ""),
            payload.get("enabled", True),
        )
    except ValueError as err:
        raise HTTPException(400, str(err)) from err
    if profile is None:
        raise HTTPException(404, "代理配置不存在")
    return profile


@router.delete("/proxies/{profile_id}")
async def delete_proxy(profile_id: str):
    if not store.delete_proxy_profile(profile_id):
        raise HTTPException(404, "代理配置不存在")
    return {"ok": True}


def _parse_ip_lookup(payload: dict) -> dict:
    """將不同 IP 查詢服務的欄位整理成穩定的管理 API 格式。"""
    connection = payload.get("connection") or {}
    ip = str(payload.get("ip") or "").strip()
    if not ip:
        raise ValueError("查詢服務未回傳 IP")
    asn = payload.get("asn") or payload.get("asn_num") or connection.get("asn")
    asn_text = str(asn or "").strip().upper()
    if asn_text and not asn_text.startswith("AS"):
        asn_text = f"AS{asn_text}"
    operator = str(
        payload.get("asn_organization")
        or payload.get("asn_org")
        or payload.get("company_name")
        or payload.get("organization")
        or payload.get("isp")
        or connection.get("org")
        or connection.get("isp")
        or ""
    ).strip()
    return {
        "ip": ip,
        "asn": asn_text,
        "operator": operator,
        "country": str(payload.get("country") or "").strip(),
        "country_code": str(payload.get("country_code") or payload.get("cc") or "").strip().upper(),
    }


def _parse_as_lookup(raw: str) -> tuple[str, str]:
    """解析備援 ASN 查詢的 CSV 回應。"""
    rows = list(csv.reader([raw.strip()]))
    if not rows or len(rows[0]) < 2:
        raise ValueError("ASN 查詢服務回應格式無效")
    row = rows[0]
    asn = str(row[1] or "").strip().upper()
    if not asn:
        raise ValueError("ASN 查詢服務未回傳 ASN")
    if not asn.startswith("AS"):
        asn = f"AS{asn}"
    operator = str(row[3] if len(row) > 3 else "").strip()
    return asn, operator


async def _probe_egress(proxy_url: str | None = None) -> dict:
    """經指定出口查詢公網資訊；主服務失敗時自動切換備援。"""
    providers = (
        ("ip.sb", "https://api.ip.sb/geoip"),
        ("ipwho.is", "https://ipwho.is/"),
        ("ipapi.is", "https://api.ipapi.is/"),
    )
    started = time.monotonic()
    last_error: Exception | None = None
    async with make_async_client(
        proxy_url=proxy_url,
        timeout=12.0,
        follow_redirects=True,
        headers={"User-Agent": "zcode2api-plus/2.0"},
    ) as client:
        for source, url in providers:
            try:
                response = await client.get(url)
                response.raise_for_status()
                result = _parse_ip_lookup(response.json() or {})
                if not result["asn"]:
                    try:
                        as_response = await client.get(
                            "https://api.hackertarget.com/aslookup/",
                            params={"q": result["ip"]},
                        )
                        as_response.raise_for_status()
                        result["asn"], as_operator = _parse_as_lookup(as_response.text)
                        if not result["operator"]:
                            result["operator"] = as_operator
                    except Exception:  # noqa: BLE001 - ASN 補查失敗時仍應保留 IP 結果
                        pass
                result.update({
                    "ok": True,
                    "source": source,
                    "latency_ms": round((time.monotonic() - started) * 1000),
                })
                return result
            except Exception as err:  # noqa: BLE001 - 備援服務需涵蓋連線與格式錯誤
                last_error = err
    error_name = type(last_error).__name__ if last_error else "UnknownError"
    raise HTTPException(502, f"出口查詢失敗（所有 IP 查詢服務均無回應，{error_name}）")


@router.post("/proxies/test-current")
async def test_current_proxy():
    return await _probe_egress()


@router.post("/proxies/{profile_id}/test")
async def test_proxy(profile_id: str):
    profile = next(
        (p for p in store.list_proxy_profiles() if p.get("id") == profile_id), None
    )
    if profile is None:
        raise HTTPException(404, "代理配置不存在")
    return await _probe_egress(profile["url"])


@router.post("/proxies/assign")
async def assign_proxy(payload: dict = Body(...)):
    account_id = str(payload.get("account_id") or "").strip()
    profile_id = payload.get("proxy_id") or None
    if not account_id:
        raise HTTPException(400, "缺少帳號 ID")
    try:
        if not store.assign_proxy_profile(account_id, profile_id):
            raise HTTPException(404, "帳號不存在")
    except ValueError as err:
        raise HTTPException(400, str(err)) from err
    return {"ok": True}


def _memory_snapshot() -> dict:
    """讀取主機記憶體；非 Linux 環境回傳空值以保持 API 可用。"""
    meminfo = {}
    try:
        for line in open("/proc/meminfo", encoding="utf-8"):
            key, value = line.split(":", 1)
            meminfo[key] = int(value.strip().split()[0])
    except (OSError, ValueError):
        return {"total_mb": None, "available_mb": None, "used_percent": None}
    total = meminfo.get("MemTotal", 0)
    available = meminfo.get("MemAvailable", meminfo.get("MemFree", 0))
    used_percent = round((total - available) / total * 100, 1) if total else None
    return {
        "total_mb": round(total / 1024, 1) if total else None,
        "available_mb": round(available / 1024, 1) if available else None,
        "used_percent": used_percent,
    }


@router.get("/monitor")
async def monitor_info():
    """提供控制台使用的服務、主機與帳號池健康資料。"""
    _accounts, stats = _account_snapshot()
    uptime = max(0, time.time() - _STARTED_AT)
    try:
        load_1m = round(os.getloadavg()[0], 2)
    except (AttributeError, OSError):
        load_1m = None
    calls = stats["calls"]
    fail = stats["fail"]
    return {
        "ts": time.time(),
        "uptime_sec": round(uptime),
        "system": {
            "cpu_count": os.cpu_count() or 1,
            "load_1m": load_1m,
            "memory": _memory_snapshot(),
        },
        "requests": {
            "total": calls,
            "errors": fail,
            "success_rate": round((calls - fail) / calls * 100, 2) if calls else None,
            "average_qps": round(calls / uptime, 3) if uptime else 0,
        },
        "accounts": {
            "total": stats["total"],
            "active": stats["active"],
            "cooling": stats["cooling"],
            "exhausted": stats["exhausted"],
            "invalid": stats["invalid"],
            "disabled": stats["disabled"],
        },
        "services": [
            {"name": "API Gateway", "status": "online", "detail": "Anthropic Messages"},
            {"name": "額度監控", "status": "online", "detail": "背景輪詢"},
            {
                "name": "驗證瀏覽器",
                "status": "online" if settings.CAPTCHA_BROWSER_ENABLED else "standby",
                "detail": "Chromium" if settings.CAPTCHA_BROWSER_ENABLED else "按需啟用",
            },
        ],
    }


@router.get("/usage")
async def usage_info():
    """提供累計用量與帳號排行；歷史明細不足時不虛構時間序列。"""
    accounts, stats = _account_snapshot()
    ranking = sorted(
        (
            {
                "name": a["name"],
                "provider": a["provider"],
                "requests": a["use_count"],
                "errors": a["fail_count"],
                "tokens": sum((a.get("total_tokens") or {}).values()),
            }
            for a in accounts
        ),
        key=lambda item: (item["requests"], item["tokens"]),
        reverse=True,
    )
    return {
        "ts": time.time(),
        "window": "累計",
        "summary": stats,
        "ranking": ranking[:12],
    }


# ── 新增账号 ─────────────────────────────────────────────────────────────────
@router.post("/accounts")
async def add_accounts(payload: dict = Body(...)):
    provider = payload.get("provider", "zai")
    if provider not in PROVIDERS:
        raise HTTPException(400, "不支持的 provider")
    tokens = payload.get("tokens") or []
    if isinstance(tokens, str):
        tokens = [t.strip() for t in tokens.splitlines() if t.strip()]
    tokens = [t.strip() for t in tokens if t and t.strip()]
    if not tokens:
        raise HTTPException(400, "请输入至少一个 Token / API Key")

    has_profile = "proxy_id" in payload
    profile_id = payload.get("proxy_id") or None
    if profile_id and not any(
        p.get("id") == profile_id for p in store.list_proxy_profiles()
    ):
        raise HTTPException(400, "代理配置不存在")

    has_proxy = "proxy_url" in payload and not has_profile
    proxy_url = None
    if has_proxy:
        try:
            proxy_url = normalize_proxy_url(payload.get("proxy_url"))
        except ValueError as err:
            raise HTTPException(400, str(err)) from err

    added = []
    for tok in dict.fromkeys(tokens):  # 去重保序
        name = payload.get("name") or f"{provider}-{len(store.list_accounts(provider)) + 1}"
        acc = store.add_account(provider, name, tok)
        if has_profile:
            store.assign_proxy_profile(acc.id, profile_id)
        elif has_proxy:
            acc.proxy_url = proxy_url
            store.update_account(acc)
        added.append(acc.id)
    # 立即刷新一次额度（仅 zai jwt）
    fresh = [a for a in store.list_accounts(provider) if a.id in added and a.mode == "jwt"]
    if fresh:
        await refresh_accounts(fresh)
    return {"count": len(added), "ids": added}


# ── 删除账号 ─────────────────────────────────────────────────────────────────
@router.delete("/accounts")
async def delete_accounts(ids: list[str] = Body(...)):
    deleted = 0
    for aid in ids:
        acc = store.find_any(aid)
        if acc and store.remove_account(acc.provider, aid):
            deleted += 1
    return {"deleted": deleted}


# ── 编辑账号 ─────────────────────────────────────────────────────────────────
@router.put("/accounts/{account_id}")
async def edit_account(account_id: str, payload: dict = Body(...)):
    acc = store.find_any(account_id)
    if not acc:
        raise HTTPException(404, "账号不存在")
    has_profile = "proxy_id" in payload
    profile_id = payload.get("proxy_id") or None
    if profile_id and not any(
        p.get("id") == profile_id for p in store.list_proxy_profiles()
    ):
        raise HTTPException(400, "代理配置不存在")
    if "name" in payload and payload["name"]:
        acc.name = payload["name"].strip()
    secret = payload.get("token") or payload.get("secret")
    if secret:
        secret = secret.strip()
        acc.mode = "jwt" if (secret.count(".") == 2 and acc.provider == "zai") else "apiKey"
        acc.jwt_token = secret if acc.mode == "jwt" else None
        acc.api_key = None if acc.mode == "jwt" else secret
        acc.status = Status.ACTIVE
        acc.last_error = None
    if "proxy_url" in payload and not has_profile:
        try:
            proxy_url = normalize_proxy_url(payload.get("proxy_url"))
        except ValueError as err:
            raise HTTPException(400, str(err)) from err
        if proxy_url != acc.proxy_url:
            acc.proxy_id = None
        acc.proxy_url = proxy_url
    store.update_account(acc)
    if has_profile:
        store.assign_proxy_profile(acc.id, profile_id)
    return {"ok": True}


# ── 启用 / 禁用 ──────────────────────────────────────────────────────────────
@router.post("/accounts/{account_id}/enabled")
async def set_enabled(account_id: str, payload: dict = Body(...)):
    acc = store.find_any(account_id)
    if not acc:
        raise HTTPException(404, "账号不存在")
    enabled = bool(payload.get("enabled", True))
    store.set_enabled(acc.provider, account_id, enabled)
    return {"ok": True}


# ── 刷新额度（实时用量监控）─────────────────────────────────────────────────
@router.post("/accounts/refresh")
async def refresh(payload: dict = Body(default=None)):
    payload = payload or {}
    if payload.get("all"):
        targets = [a for a in store.list_accounts("zai") if a.mode == "jwt"]
    else:
        ids = set(payload.get("ids") or [])
        targets = [a for a in store.list_accounts() if a.id in ids and a.mode == "jwt"]
    summary = await refresh_accounts(targets)
    return {"summary": summary, "count": len(targets)}


@router.post("/accounts/{account_id}/refresh")
async def refresh_one(account_id: str):
    acc = store.find_any(account_id)
    if not acc:
        raise HTTPException(404, "账号不存在")
    if acc.mode != "jwt":
        return {"ok": False, "message": "仅 Coding Plan (JWT) 账号支持额度查询"}
    res = await fetch_quota(acc)
    updated = store.find_any(account_id) or acc
    return {"ok": "error" not in res, "result": res, "account": updated.public_view()}


# ── 重置調度統計 ─────────────────────────────────────────────────────────────
@router.post("/accounts/{account_id}/reset-stats")
async def reset_account_stats(account_id: str):
    acc = store.find_any(account_id)
    if not acc:
        raise HTTPException(404, "账号不存在")
    acc.reset_token_stats()
    store.update_account(acc)
    return {"ok": True}


# ── 手动人机验证 ─────────────────────────────────────────────────────────────
@router.get("/captcha/config")
async def captcha_config():
    return await captcha_manager.fetch_config()


@router.post("/captcha/submit")
async def captcha_submit(payload: dict = Body(...)):
    param = (payload.get("verify_param") or "").strip()
    if not param:
        raise HTTPException(400, "verify_param 不能为空")
    config = await captcha_manager.fetch_config()
    try:
        await captcha_manager.set_manual_param(param, config.get("region"))
    except ValueError as err:
        raise HTTPException(400, str(err)) from err
    return {"ok": True}


# ── OAuth 登录（Z.AI）────────────────────────────────────────────────────────
def _find_login_flow(state: str) -> tuple[str | None, ZaiAuthFlow | None]:
    _cleanup_login_flows()
    for flow_id, flow in _login_flows.items():
        if flow.matches_state(state):
            return flow_id, flow
    return None, None


async def _save_oauth_account(flow: ZaiAuthFlow, data: dict):
    zcode_jwt = data.get("token")
    access_token = (data.get("zai") or {}).get("access_token")
    user = data.get("user") or {}
    email = (data.get("email") or extract_user_email(user) or "").strip() or None
    account_name = email or "oauth-login"
    account = store.add_account("zai", account_name, zcode_jwt) if zcode_jwt else None
    if account is not None and email:
        account.email = email
        if account.name == "oauth-login":
            account.name = email
        store.update_account(account)
    if access_token:
        try:
            api_key = await flow.exchange_api_key(access_token)
            if account is not None:
                account.api_key = api_key
                store.update_account(account)
            else:
                account = store.add_account("zai", "oauth-login", api_key)
                if email:
                    account.email = email
                    account.name = email
                    store.update_account(account)
        except Exception:  # noqa: BLE001 - 兑换失败不影响 JWT 已入池
            pass
    if account is None:
        raise RuntimeError("未能从授权结果中获取凭证")
    if account.mode == "jwt":
        try:
            await refresh_accounts([account])
        except Exception:  # noqa: BLE001 - 额度刷新失败不影响账号入池
            pass
    return account


async def _complete_login_callback(code: str, state: str, error: str) -> tuple[bool, str]:
    flow_id, flow = _find_login_flow(state)
    if flow_id is None or flow is None:
        raise RuntimeError("登录会话不存在、已过期或 state 无效")
    if error:
        message = f"Z.AI 拒绝授权: {error}"
        _login_results[flow_id] = {"status": "failed", "message": message}
        return False, message
    try:
        data = await flow.exchange_code(code, state)
        account = await _save_oauth_account(flow, data)
    except Exception as err:  # noqa: BLE001
        message = str(err) or "OAuth 授权失败"
        _login_results[flow_id] = {"status": "failed", "message": message}
        return False, message
    _login_results[flow_id] = {"status": "ready", "account": account.public_view()}
    return True, "授权成功，账号已加入账号池"


@router.post("/login/start")
async def login_start():
    """发起 Z.AI 浏览器 OAuth，返回使用官方已注册回调的授权链接。"""
    _cleanup_login_flows()
    flow = ZaiAuthFlow()
    try:
        flow_id, authorize_url = await flow.init()
    except Exception as err:  # noqa: BLE001
        raise HTTPException(502, f"登录初始化失败: {err}")
    _login_flows[flow_id] = flow
    _login_results[flow_id] = {"status": "pending"}
    return {"flow_id": flow_id, "authorize_url": authorize_url}


@router.post("/login/complete/{flow_id}")
async def login_complete(flow_id: str, payload: dict = Body(...)):
    """校验用户从官方登录完成页复制的回调地址并导入凭证。"""
    _cleanup_login_flows()
    flow = _login_flows.get(flow_id)
    if flow is None:
        raise HTTPException(404, "登录会话不存在或已过期")
    callback_url = (payload.get("callback_url") or "").strip()
    try:
        code, state, error = flow.parse_callback_url(callback_url)
    except Exception as err:  # noqa: BLE001
        raise HTTPException(400, str(err) or "回调地址无效")
    if not flow.matches_state(state):
        raise HTTPException(400, "回调地址与当前登录会话不匹配")

    ok, message = await _complete_login_callback(code, state, error)
    result = _login_results.get(flow_id, {"status": "failed", "message": message})
    if not ok:
        _login_results[flow_id] = {"status": "pending"}
        raise HTTPException(400, message)
    _login_flows.pop(flow_id, None)
    _login_results.pop(flow_id, None)
    return result


# ── 设置 ─────────────────────────────────────────────────────────────────────
@router.get("/settings")
async def get_settings():
    return {
        "admin_key": store.admin_key(),
        "gateway_key": store.gateway_key(),
        "quota_refresh_interval": store.quota_refresh_interval(),
    }


@router.put("/settings")
async def update_settings(payload: dict = Body(...)):
    if "admin_key" in payload:
        key = (payload["admin_key"] or "").strip()
        if not key:
            raise HTTPException(400, "后台密钥不能为空")
        store.set_setting("admin_key", key)
    if "gateway_key" in payload:
        store.set_setting("gateway_key", (payload["gateway_key"] or "").strip())
    if "quota_refresh_interval" in payload:
        try:
            interval = max(0, int(payload["quota_refresh_interval"]))
        except (TypeError, ValueError):
            raise HTTPException(400, "刷新间隔必须是非负整数")
        store.set_setting("quota_refresh_interval", str(interval))
    return {"ok": True}


# ── 导入 / 导出 ─────────────────────────────────────────────────────────────
@router.get("/export")
async def export_accounts():
    return store.export()


@router.post("/import")
async def import_accounts(payload: dict = Body(...)):
    count = store.import_accounts(payload)
    return {"count": count}
