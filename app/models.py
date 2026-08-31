"""账号数据模型与状态枚举。"""

from __future__ import annotations

import secrets
import time
from dataclasses import asdict, dataclass, field

PROVIDERS = ("zai",)


class Status:
    """账号运行状态。"""

    ACTIVE = "active"        # 正常，可参与轮询
    EXHAUSTED = "exhausted"  # 额度用完
    COOLING = "cooling"      # 临时限流（冷却中）
    INVALID = "invalid"      # 凭证失效 / 鉴权失败
    DISABLED = "disabled"    # 手动禁用

    MANAGEABLE = (ACTIVE, COOLING, EXHAUSTED)


def _account_id(name: str) -> str:
    safe = "".join(c if c.isalnum() else "-" for c in (name or "account").lower())
    safe = safe.strip("-")[:32] or "account"
    return f"{safe}-{secrets.token_hex(4)}"


def normalize_model_name(model: object) -> str:
    """統一模型名稱格式，供額度快照與請求模型穩定比對。"""
    value = str(model or "").strip().lower().replace("_", "-").replace(" ", "-")
    while "--" in value:
        value = value.replace("--", "-")
    return value


def _plan_text(plan: dict) -> str:
    """從官方方案資料取出可供辨識的名稱。"""
    if not isinstance(plan, dict):
        return ""
    for key in ("plan_name", "display_name", "show_name", "name", "title", "plan_type", "plan_id"):
        value = plan.get(key)
        if value:
            return str(value).strip()
    return ""


def _is_trial_plan(plan: object, depth: int = 0) -> bool:
    """辨識官方回傳的體驗、試用或免費方案，缺少標記時以方案名稱補判。"""
    if depth > 6:
        return False
    if isinstance(plan, list):
        return any(_is_trial_plan(item, depth + 1) for item in plan)
    if not isinstance(plan, dict):
        return False
    markers = ("trial", "free", "experience", "體驗", "试用", "試用")
    for key, value in plan.items():
        normalized = str(key).replace("_", "").replace("-", "").lower()
        if normalized in {"istrial", "trial", "isfree", "free", "isexperience", "trialplan"} and value is True:
            return True
        if isinstance(value, (dict, list)) and _is_trial_plan(value, depth + 1):
            return True
        if any(marker in str(value or "").lower() for marker in markers):
            return True
    return False


@dataclass
class Account:
    """单个可轮询的账号凭证 + 运行时状态。"""

    id: str
    name: str
    provider: str
    mode: str  # "jwt" | "apiKey"
    email: str | None = None
    jwt_token: str | None = None
    api_key: str | None = None
    enabled: bool = True
    status: str = Status.ACTIVE

    # 额度快照：{ model_show_name: {total, used, remaining, expires_at} }
    quota: dict = field(default_factory=dict)
    exhausted_models: list[str] = field(default_factory=list)
    disabled_models: list[str] = field(default_factory=list)
    plan: dict = field(default_factory=dict)        # 当前激活方案
    usage: dict = field(default_factory=dict)       # 近期用量原始数据

    use_count: int = 0
    fail_count: int = 0
    # 累計調度 token 用量（僅計成功回應，由 UsageCollector 餵入）
    total_input_tokens: int = 0
    total_output_tokens: int = 0
    total_cache_creation_tokens: int = 0
    total_cache_read_tokens: int = 0
    last_used_at: float | None = None
    last_checked_at: float | None = None
    cooling_until: float | None = None
    last_error: str | None = None
    proxy_url: str | None = None  # 账号独立出站代理，空则直连
    proxy_id: str | None = None   # 代理設定頁中的命名出口
    created_at: float = field(default_factory=time.time)

    @staticmethod
    def create(provider: str, name: str, secret: str) -> "Account":
        secret = (secret or "").strip()
        is_jwt = secret.count(".") == 2 and provider == "zai"
        return Account(
            id=_account_id(name),
            name=name or f"{provider}-account",
            provider=provider,
            mode="jwt" if is_jwt else "apiKey",
            jwt_token=secret if is_jwt else None,
            api_key=None if is_jwt else secret,
        )

    @property
    def secret(self) -> str | None:
        return self.jwt_token if self.mode == "jwt" else self.api_key

    def is_selectable(self, now: float | None = None) -> bool:
        """是否可被轮询选中。"""
        if not self.enabled or self.status in (Status.DISABLED, Status.INVALID):
            return False
        if self.status == Status.EXHAUSTED:
            return False
        if self.status == Status.COOLING:
            now = now or time.time()
            return bool(self.cooling_until and now >= self.cooling_until)
        return True

    def quota_for_model(self, model: object) -> dict | None:
        """取得請求模型對應的額度；無法精確比對時回傳未知。"""
        target = normalize_model_name(model)
        if not target:
            return None
        for name, quota in self.quota.items():
            if normalize_model_name(name) == target and isinstance(quota, dict):
                return quota
        return None

    def model_availability(self, model: object) -> str:
        """回傳模型可用狀態，手動停用優先於官方額度快照。"""
        target = normalize_model_name(model)
        if not target:
            return "unknown"
        if target in {normalize_model_name(name) for name in self.disabled_models}:
            return "disabled"
        if target in {normalize_model_name(name) for name in self.exhausted_models}:
            return "exhausted"
        quota = self.quota_for_model(target)
        if quota is None or quota.get("remaining") is None:
            return "unknown"
        try:
            return "available" if float(quota["remaining"]) > 0 else "exhausted"
        except (TypeError, ValueError):
            return "unknown"

    def is_model_selectable(self, model: object, now: float | None = None) -> bool:
        """帳號全局可用且指定模型未耗盡時才允許調度。"""
        return self.is_selectable(now) and self.model_availability(model) in ("available", "unknown")

    def set_disabled_models(self, models: list[object]) -> None:
        """保存手動停用模型，正規化並去除空值與重複項。"""
        normalized = [normalize_model_name(model) for model in models]
        self.disabled_models = list(dict.fromkeys(model for model in normalized if model))

    def mark_model_exhausted(self, model: object) -> bool:
        """記錄單一模型已耗盡；無模型名稱時無法安全建立標記。"""
        target = normalize_model_name(model)
        if not target:
            return False
        known = {normalize_model_name(name) for name in self.exhausted_models}
        if target not in known:
            self.exhausted_models.append(target)
        return True

    def sync_exhausted_models(self) -> None:
        """以最新官方額度快照同步已耗盡模型，正額度模型會自動恢復。"""
        exhausted = []
        for model, quota in self.quota.items():
            if not isinstance(quota, dict) or quota.get("remaining") is None:
                continue
            try:
                if float(quota["remaining"]) <= 0:
                    exhausted.append(normalize_model_name(model))
            except (TypeError, ValueError):
                continue
        self.exhausted_models = list(dict.fromkeys(exhausted))

    def accumulate_tokens(self, usage: dict) -> None:
        """累加一次成功回應的 token 用量（鍵與 UsageCollector.as_dict 一致）。"""
        self.total_input_tokens += int(usage.get("input") or 0)
        self.total_output_tokens += int(usage.get("output") or 0)
        self.total_cache_creation_tokens += int(usage.get("cache_creation") or 0)
        self.total_cache_read_tokens += int(usage.get("cache_read") or 0)

    def reset_token_stats(self) -> None:
        """清零累計 token 用量。"""
        self.total_input_tokens = 0
        self.total_output_tokens = 0
        self.total_cache_creation_tokens = 0
        self.total_cache_read_tokens = 0

    def to_dict(self) -> dict:
        return asdict(self)

    @staticmethod
    def from_dict(data: dict) -> "Account":
        known = {f for f in Account.__dataclass_fields__}  # type: ignore[attr-defined]
        return Account(**{k: v for k, v in data.items() if k in known})

    def public_view(self) -> dict:
        """返回给前端的视图（脱敏 token）。"""
        secret = self.secret or ""
        masked = secret if len(secret) <= 16 else f"{secret[:8]}…{secret[-6:]}"
        return {
            "id": self.id,
            "name": self.name,
            "email": self.email,
            "provider": self.provider,
            "mode": self.mode,
            "token_masked": masked,
            "enabled": self.enabled,
            "status": self.effective_status(),
            "quota": self.quota,
            "exhausted_models": self.exhausted_models,
            "disabled_models": self.disabled_models,
            "plan": self.plan,
            "plan_name": _plan_text(self.plan),
            "plan_is_trial": _is_trial_plan(self.plan),
            "use_count": self.use_count,
            "fail_count": self.fail_count,
            "total_tokens": {
                "input": self.total_input_tokens,
                "output": self.total_output_tokens,
                "cache_creation": self.total_cache_creation_tokens,
                "cache_read": self.total_cache_read_tokens,
            },
            "last_used_at": self.last_used_at,
            "last_checked_at": self.last_checked_at,
            "cooling_until": self.cooling_until,
            "last_error": self.last_error,
            "proxy_url": self.proxy_url,
            "proxy_id": self.proxy_id,
            "created_at": self.created_at,
        }

    def effective_status(self, now: float | None = None) -> str:
        """考虑冷却到期后的实时状态。"""
        if self.status == Status.COOLING:
            now = now or time.time()
            if self.cooling_until and now >= self.cooling_until:
                return Status.ACTIVE
        return self.status
