"""Z.AI OAuth 登录流程。

使用 ZCode 官网当前的浏览器授权流程获取 Coding Plan 凭证。
"""

from __future__ import annotations

import base64
import json
import secrets
import time
from urllib.parse import parse_qs, urlencode, urlparse

import httpx


_AUTHORIZE_URL = "https://chat.z.ai/api/oauth/authorize"
_TOKEN_URL = "https://zcode.z.ai/api/v1/oauth/token"
_CLIENT_ID = "client_P8X5CMWmlaRO9gyO-KSqtg"
_REGISTERED_REDIRECT_URI = (
    "https://zcode.z.ai/app/oauth/login?redirect=zcode%3A%2F%2Foauth%2Fcallback"
)


def extract_user_email(user: object) -> str | None:
    """從 OAuth 使用者資料遞迴取出郵箱，兼容不同版本的欄位命名。"""
    keys = {"email", "emailaddress", "mail", "useremail"}

    def walk(value: object, depth: int = 0) -> str | None:
        if depth > 3:
            return None
        if isinstance(value, dict):
            for key, item in value.items():
                normalized = str(key).replace("_", "").replace("-", "").lower()
                if normalized in keys and isinstance(item, str) and "@" in item:
                    candidate = item.strip()
                    if candidate:
                        return candidate
            for item in value.values():
                found = walk(item, depth + 1)
                if found:
                    return found
        elif isinstance(value, list):
            for item in value:
                found = walk(item, depth + 1)
                if found:
                    return found
        return None

    return walk(user)


def extract_jwt_email(token: str) -> str | None:
    """在 OAuth 使用者欄位缺失時，從 JWT 非驗證解析郵箱聲明作為備援。"""
    try:
        parts = token.split(".")
        if len(parts) != 3:
            return None
        padding = "=" * (-len(parts[1]) % 4)
        claims = json.loads(base64.urlsafe_b64decode(parts[1] + padding).decode("utf-8"))
    except (ValueError, UnicodeDecodeError, json.JSONDecodeError):
        return None
    return extract_user_email(claims)


class ZaiAuthFlow:
    def __init__(self) -> None:
        self.redirect_uri = _REGISTERED_REDIRECT_URI
        self.flow_id = secrets.token_urlsafe(24)
        self.nonce = secrets.token_urlsafe(24)
        self.state = ""
        self.created_at = time.monotonic()

    async def init(self) -> tuple[str, str]:
        state_data = {
            "nonce": self.nonce,
            "app_return_to": self.redirect_uri,
            "redirect_uri": self.redirect_uri,
        }
        raw_state = json.dumps(state_data, ensure_ascii=False, separators=(",", ":")).encode()
        self.state = base64.urlsafe_b64encode(raw_state).decode().rstrip("=")
        query = urlencode({
            "redirect_uri": self.redirect_uri,
            "response_type": "code",
            "client_id": _CLIENT_ID,
            "state": self.state,
        })
        return self.flow_id, f"{_AUTHORIZE_URL}?{query}"

    def matches_state(self, state: str) -> bool:
        return bool(self.state and state and secrets.compare_digest(self.state, state))

    @staticmethod
    def parse_callback_url(callback_url: str) -> tuple[str, str, str]:
        callback_url = callback_url.strip()
        if not callback_url or len(callback_url) > 8192:
            raise RuntimeError("回调地址为空或过长")
        parsed = urlparse(callback_url)
        query = parse_qs(parsed.query, keep_blank_values=True)
        if parsed.scheme == "https":
            if parsed.netloc != "zcode.z.ai" or parsed.path.rstrip("/") != "/app/oauth/login":
                raise RuntimeError("只接受 ZCode 官方登录完成页地址")
            redirect = (query.get("redirect") or [""])[0]
            if redirect.rstrip("/") != "zcode://oauth/callback":
                raise RuntimeError("ZCode 官方回调目标无效")
        elif parsed.scheme == "zcode":
            if parsed.netloc != "oauth" or parsed.path.rstrip("/") != "/callback":
                raise RuntimeError("ZCode 回调地址无效")
        else:
            raise RuntimeError("只接受 ZCode 官方 HTTPS 或 zcode:// 回调地址")

        code = (query.get("code") or query.get("authCode") or [""])[0].strip()
        state = (query.get("state") or [""])[0].strip()
        error = (query.get("error") or [""])[0].strip()
        if not error and (not code or not state):
            raise RuntimeError("回调地址缺少 code/authCode 或 state")
        return code, state, error

    async def exchange_code(self, code: str, state: str) -> dict:
        if not code:
            raise RuntimeError("OAuth 回调缺少授权码")
        if not self.matches_state(state):
            raise RuntimeError("OAuth state 校验失败")

        async with httpx.AsyncClient(timeout=30) as client:
            res = await client.post(
                _TOKEN_URL,
                headers={"Content-Type": "application/json"},
                json={"code": code, "redirect_uri": self.redirect_uri, "state": state},
            )
        if not res.is_success:
            detail = res.text.strip()[:200]
            raise RuntimeError(f"Z.AI 凭证交换失败 ({res.status_code}): {detail}")
        try:
            payload = res.json()
        except ValueError as err:
            raise RuntimeError("Z.AI 凭证交换返回了无效 JSON") from err
        if payload.get("code") != 0:
            raise RuntimeError((payload.get("msg") or "Z.AI 凭证交换失败").strip())

        data = payload.get("data") or {}
        zcode_jwt = (data.get("token") or "").strip()
        access_token = ((data.get("zai") or {}).get("access_token") or "").strip()
        if not zcode_jwt:
            raise RuntimeError("Z.AI 凭证响应中不含 Coding Plan Token")
        return {
            "status": "ready",
            "token": zcode_jwt,
            "zai": {"access_token": access_token} if access_token else {},
            "user": data.get("user") or {},
            "email": extract_user_email(data.get("user") or {}) or extract_jwt_email(zcode_jwt),
        }

    async def exchange_api_key(self, access_token: str) -> str:
        """OAuth access_token → 业务 token → 机构/项目 → API Key。"""
        async with httpx.AsyncClient(timeout=30) as client:
            login = await client.post(
                "https://api.z.ai/api/auth/z/login",
                headers={"Content-Type": "application/json"},
                json={"token": access_token},
            )
            login.raise_for_status()
            biz = (login.json().get("data") or {})
            biz_token = biz.get("access_token") or biz.get("accessToken")
            if not biz_token:
                raise RuntimeError("返回数据中不含业务凭证")

            info = await client.get(
                "https://api.z.ai/api/biz/customer/getCustomerInfo",
                headers={"Authorization": f"Bearer {biz_token}"},
            )
            info.raise_for_status()
            orgs = (info.json().get("data") or {}).get("organizations") or []
            org = next((o for o in orgs if "默认机构" in (o.get("organizationName") or "")), None) or (orgs[0] if orgs else None)
            if not org:
                raise RuntimeError("找不到可用的机构")
            projects = org.get("projects") or []
            proj = next((p for p in projects if "默认项目" in (p.get("projectName") or "")), None) or (projects[0] if projects else None)
            if not proj:
                raise RuntimeError("找不到可用的项目")

            org_id, proj_id = org["organizationId"], proj["projectId"]
            key_url = f"https://api.z.ai/api/biz/v1/organization/{org_id}/projects/{proj_id}/api_keys"

            keys_res = await client.get(key_url, headers={"Authorization": f"Bearer {biz_token}"})
            keys_res.raise_for_status()
            keys = keys_res.json().get("data") or []
            key_obj = next((k for k in keys if k.get("name") == "zcode-api-key"), None)
            if not key_obj:
                create = await client.post(
                    key_url,
                    headers={"Authorization": f"Bearer {biz_token}", "Content-Type": "application/json"},
                    json={"name": "zcode-api-key"},
                )
                create.raise_for_status()
                key_obj = create.json().get("data")

            api_key = (key_obj or {}).get("apiKey")
            if not api_key:
                raise RuntimeError("获取 API Key 失败")

            copy = await client.get(
                f"{key_url}/copy/{api_key}",
                headers={"Authorization": f"Bearer {biz_token}"},
            )
            copy.raise_for_status()
            secret_key = (copy.json().get("data") or {}).get("secretKey")
            if not secret_key:
                raise RuntimeError("未能解密 Secret Key")
        return f"{api_key}.{secret_key}"
