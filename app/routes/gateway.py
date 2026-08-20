"""核心网关：兼容 Anthropic Messages 协议的 /v1/messages。

实现多账号轮询 + 额度用完自动换号 + 阿里无痕验证自动续期。
"""

from __future__ import annotations

import asyncio
import json
import secrets
import time
from pathlib import Path

import httpx
from fastapi import APIRouter, Depends, Request
from fastapi.responses import JSONResponse, StreamingResponse

from .. import logs, settings
from ..agent import build_request
from ..auth_admin import verify_gateway_key
from ..captcha import captcha_manager
from ..models import Account, Status
from ..proxy import make_async_client
from ..quota import fetch_quota
from ..store import store

router = APIRouter()

MAX_CAPTCHA_RETRIES = 3
MAX_ACCOUNT_ATTEMPTS = 5
_START_PLAN_BUSY_RETRY_DELAYS = (1.0, 2.0)

# Z.AI 上游模型名大小写敏感
MODEL_NAME_MAP = {
    "glm-5.2": "GLM-5.2",
    "glm-5-turbo": "GLM-5-Turbo",
    "glm-turbo": "GLM-5-Turbo",
    "glm-5.3": "GLM-5.3",
    "glm-5.1": "GLM-5.1",
    "glm-4.7": "GLM-4.7",
}

# /v1/models 对外公布的可用模型（扩展自 TriDefender）
AVAILABLE_MODELS = [
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
]

# 命中以下信号则认为账号额度用完
_EXHAUST_KEYWORDS = ("quota", "insufficient", "balance", "exhaust", "额度", "余额不足")
_CAPTCHA_HEADERS = (
    "x-aliyun-captcha-verify-param",
    "x-aliyun-captcha-verify-region",
    "x-aliyun-captcha-challenge",
)

# ZCode 系统提示词（用于 JWT 请求）
_ZCODE_SYSTEM_BLOCKS = None


def _load_zcode_system():
    global _ZCODE_SYSTEM_BLOCKS
    if _ZCODE_SYSTEM_BLOCKS is None:
        path = Path(__file__).parent.parent / "zcode_system.json"
        try:
            _ZCODE_SYSTEM_BLOCKS = json.loads(path.read_text(encoding="utf-8"))
        except Exception:  # noqa: BLE001
            _ZCODE_SYSTEM_BLOCKS = []
    return _ZCODE_SYSTEM_BLOCKS


def _detect_provider(body: dict, headers) -> str:
    model = body.get("model") or ""
    if model.startswith("bigmodel/") or headers.get("x-provider") == "bigmodel":
        return "bigmodel"
    return "zai"


def _normalize_body(body: dict, needs_zcode_system: bool = False) -> dict:
    model = body.get("model")
    if isinstance(model, str) and "/" in model:
        model = "/".join(model.split("/")[1:])
    if isinstance(model, str):
        model = MODEL_NAME_MAP.get(model.lower(), model)
        body["model"] = model

    messages = body.get("messages")
    if isinstance(messages, list):
        bridged = []
        for msg in messages:
            if isinstance(msg, dict) and isinstance(msg.get("content"), str):
                bridged.append({**msg, "content": [{"type": "text", "text": msg["content"]}]})
            else:
                bridged.append(msg)
        body["messages"] = bridged

    # JWT 账号需要注入 ZCode 系统提示词，否则上游返回 405
    if needs_zcode_system:
        zcode_blocks = _load_zcode_system()
        existing_system = body.get("system")
        if existing_system is None:
            body["system"] = zcode_blocks
        elif isinstance(existing_system, str):
            body["system"] = zcode_blocks + [{"type": "text", "text": existing_system}]
        elif isinstance(existing_system, list):
            body["system"] = zcode_blocks + existing_system
    return body


def _is_captcha_error(text: str, status_code: int | None = None, headers=None) -> bool:
    """识别上游验证码挑战，兼容响应头、400/code 3007 和旧文本。"""
    if headers:
        for name in _CAPTCHA_HEADERS:
            if headers.get(name):
                return True
    try:
        payload = json.loads(text)
    except (json.JSONDecodeError, TypeError, ValueError):
        payload = None
    if isinstance(payload, dict):
        codes = [payload.get("code"), (payload.get("error") or {}).get("code") if isinstance(payload.get("error"), dict) else None]
        if any(str(code) == "3007" for code in codes):
            return True
    low = text.lower()
    if "f001" in low:
        return status_code is None or status_code in (400, 403)
    if "captcha" in low or "verify token" in low or "verify failed" in low:
        return status_code is None or status_code in (400, 403)
    return False


def _captcha_required(req_id: str, detail: str) -> JSONResponse:
    logs.req_err(req_id, f"验证码不可用: {detail}")
    return JSONResponse(
        {
            "error": {
                "message": "自动验证码暂时失败，请稍后重试；如仍失败再打开后台 /admin/captcha",
                "type": "captcha_required",
            }
        },
        status_code=503,
    )


def _is_exhausted(status_code: int, text: str) -> bool:
    if status_code in (402,):
        return True
    low = text.lower()
    return any(k in low for k in _EXHAUST_KEYWORDS)


def _is_model_concurrency_limit(status_code: int, text: str) -> bool:
    """3010 是模型并发准入限制，不代表账号被限流或失效。"""
    if status_code != 429:
        return False
    try:
        payload = json.loads(text)
    except (json.JSONDecodeError, TypeError, ValueError):
        payload = None
    if isinstance(payload, dict) and str(payload.get("code")) == "3010":
        return True
    return "model admission concurrency limit" in text.lower()


def _mark(account: Account, status_value: str, error: str | None = None) -> None:
    account.status = status_value
    account.last_error = error
    if status_value == Status.COOLING:
        account.cooling_until = time.time() + settings.COOLING_SECONDS
    store.update_account(account)


def _last_user_text(body: dict) -> str:
    for msg in reversed(body.get("messages") or []):
        if not isinstance(msg, dict) or msg.get("role") != "user":
            continue
        content = msg.get("content")
        if isinstance(content, str):
            return content
        if isinstance(content, list):
            for part in content:
                if isinstance(part, dict) and part.get("type") == "text":
                    return part.get("text", "")
    return ""


@router.get("/v1/models", dependencies=[Depends(verify_gateway_key)])
async def list_models():
    """列出可用模型（Anthropic /v1/models 风格）。"""
    return {
        "object": "list",
        "data": [
            {"id": i, "type": "model", "display_name": i, "created_at": "2025-01-01T00:00:00Z"}
            for i in AVAILABLE_MODELS
        ],
    }


@router.post("/v1/messages", dependencies=[Depends(verify_gateway_key)])
async def messages(request: Request):
    try:
        body = await request.json()
    except (json.JSONDecodeError, ValueError):
        return JSONResponse({"error": {"message": "请求体不是合法 JSON", "type": "invalid_request"}}, status_code=400)

    incoming_headers = dict(request.headers)
    provider = _detect_provider(body, request.headers)
    body = _normalize_body(body)
    # 验证码页面由本服务托管，端口取实际请求端口（兼容任意启动端口）
    port = request.url.port or settings.PORT
    payload = json.dumps(body).encode("utf-8")

    req_id = secrets.token_hex(3)
    logs.req(req_id, str(body.get("model") or "-"), bool(body.get("stream")), _last_user_text(body))

    tried: set[str] = set()

    for _ in range(MAX_ACCOUNT_ATTEMPTS):
        account = store.select(provider, skip_ids=tried)
        if account is None:
            break
        tried.add(account.id)
        needs_captcha = provider == "zai" and account.mode == "jwt"

        result = await _try_account(req_id, account, body, payload, incoming_headers, port, needs_captcha)
        if result is _NEXT_ACCOUNT:
            continue
        return result

    logs.req_err(req_id, "无可用账号 / 额度均已耗尽")
    return JSONResponse(
        {"error": {"message": "所有账号均不可用或额度已用完，请在后台检查账号状态", "type": "no_available_account"}},
        status_code=503,
    )


_NEXT_ACCOUNT = object()


async def _try_account(req_id, account, body, payload, incoming_headers, port, needs_captcha):
    """尝试用单个账号转发，含验证码续期。返回 Response 或 _NEXT_ACCOUNT。"""
    # JWT 账号需要注入 ZCode 系统提示词
    needs_zcode_system = account.mode == "jwt"
    actual_body = _normalize_body(body.copy(), needs_zcode_system=needs_zcode_system)
    actual_payload = json.dumps(actual_body).encode("utf-8")

    for attempt in range(MAX_CAPTCHA_RETRIES):
        captcha_token = None
        if needs_captcha:
            try:
                captcha_token = await captcha_manager.get_verify_param(port)
            except Exception as err:  # noqa: BLE001
                captcha_manager.invalidate()
                if attempt + 1 < MAX_CAPTCHA_RETRIES:
                    logs.warn(req_id, f"验证码自动求解失败，刷新令牌重试（第 {attempt + 1} 次）")
                    continue
                return _captcha_required(req_id, str(err))

        verify_param = captcha_token.verify_param if captcha_token else None
        verify_region = captcha_token.region if captcha_token else None
        try:
            url, headers = build_request(account, actual_body, verify_param, verify_region, incoming_headers)
        except RuntimeError as err:
            _mark(account, Status.INVALID, str(err))
            logs.warn(req_id, f"账号 {account.name} 凭证无效，切换下一个")
            return _NEXT_ACCOUNT

        client = make_async_client(account, timeout=httpx.Timeout(connect=30.0, read=None, write=120.0, pool=30.0))
        cm = client.stream("POST", url, headers=headers, content=actual_payload)
        try:
            resp = await cm.__aenter__()
        except httpx.HTTPError as err:
            await client.aclose()
            _mark(account, Status.COOLING, f"连接失败: {err}")
            logs.warn(req_id, f"账号 {account.name} 连接失败，切换下一个")
            return _NEXT_ACCOUNT

        status_code = resp.status_code

        if status_code >= 400:
            captcha_challenge = any(resp.headers.get(name) for name in _CAPTCHA_HEADERS)
            text = (await resp.aread()).decode("utf-8", "ignore")
            await cm.__aexit__(None, None, None)
            await client.aclose()

            if needs_captcha and _is_captcha_error(text, status_code, resp.headers):
                captcha_manager.invalidate()
                captcha_token = None
                logs.warn(req_id, f"账号 {account.name} 验证码失效，刷新重试（第 {attempt + 1} 次）")
                if attempt + 1 >= MAX_CAPTCHA_RETRIES:
                    detail = "上游连续拒绝验证码"
                    if captcha_challenge:
                        detail = "上游持续返回验证码挑战"
                    return _captcha_required(req_id, detail)
                continue  # 同账号重试验证码

            if _is_exhausted(status_code, text):
                _mark(account, Status.EXHAUSTED, "额度已用完")
                logs.warn(req_id, f"账号 {account.name} 额度用完，切换下一个")
                asyncio.create_task(_safe_refresh(account))
                return _NEXT_ACCOUNT

            if status_code in (401, 403):
                _mark(account, Status.INVALID, f"鉴权失败 HTTP {status_code}")
                logs.warn(req_id, f"账号 {account.name} 鉴权失败 {status_code}，切换下一个")
                return _NEXT_ACCOUNT

            if _is_model_concurrency_limit(status_code, text):
                # 原版客户端对 Start Plan 的 3010 等待 1s、2s 重试；账号
                # 仍然可用，不能把模型准入限制写成账号 cooling。
                if attempt < len(_START_PLAN_BUSY_RETRY_DELAYS):
                    delay = _START_PLAN_BUSY_RETRY_DELAYS[attempt]
                    logs.warn(req_id, f"模型并发准入受限，{delay:g}s 后重试（账号仍可用）")
                    await asyncio.sleep(delay)
                    continue
                logs.warn(req_id, f"模型并发准入持续受限（账号 {account.name}），保留账号状态")
                return JSONResponse(
                    _safe_json(text) or {"error": {"message": text[:500], "type": "upstream_rate_limit"}},
                    status_code=status_code,
                )

            if status_code == 429:
                _mark(account, Status.COOLING, "上游限流 429")
                logs.warn(req_id, f"账号 {account.name} 被限流 429，切换下一个")
                return _NEXT_ACCOUNT

            # 其它错误：直接回传客户端
            account.fail_count += 1
            store.update_account(account)
            logs.req_err(req_id, f"上游错误 HTTP {status_code}（账号 {account.name}）")
            return JSONResponse(
                _safe_json(text) or {"error": {"message": text[:500], "type": "upstream_error"}},
                status_code=status_code,
            )

        # 成功：记录用量并流式透传
        account.use_count += 1
        account.last_used_at = time.time()
        if account.status in (Status.COOLING, Status.EXHAUSTED):
            account.status = Status.ACTIVE
        store.update_account(account)
        asyncio.create_task(_safe_refresh(account))

        content_type = resp.headers.get("content-type", "application/json")

        async def _body_iter():
            try:
                async for chunk in resp.aiter_bytes():
                    yield chunk
                logs.req_ok(req_id)
            except Exception as err:  # noqa: BLE001
                logs.req_err(req_id, f"流传输中断: {err}")
            finally:
                await cm.__aexit__(None, None, None)
                await client.aclose()

        out_headers = {"Cache-Control": "no-cache"}
        return StreamingResponse(_body_iter(), status_code=status_code,
                                 media_type=content_type, headers=out_headers)

    if needs_captcha:
        return _captcha_required(req_id, "验证码重试次数已耗尽")
    return _NEXT_ACCOUNT


def _safe_json(text: str):
    try:
        return json.loads(text)
    except (json.JSONDecodeError, ValueError):
        return None


async def _safe_refresh(account: Account) -> None:
    try:
        if account.provider == "zai" and account.mode == "jwt":
            await fetch_quota(account)
    except Exception:  # noqa: BLE001
        pass
