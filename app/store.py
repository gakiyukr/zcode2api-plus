"""账号与设置的持久化存储（SQLite）。

数据保存在项目本目录下的 data/accounts.db，采用 WAL 模式，
与 grok2api 的本地 (local) 账号后端保持一致。

运行期账号对象常驻内存（保证轮询游标与状态实时性），
每次变更同步落库；进程启动时从 SQLite 读取快照。
"""

from __future__ import annotations

import json
import secrets
import sqlite3
import threading
import time
from contextlib import closing

from . import settings
from .models import PROVIDERS, Account, Status

_TBL = "accounts"
_META = "meta"


class Store:
    """线程安全的账号 / 设置存储，含轮询游标。"""

    def __init__(self) -> None:
        self._lock = threading.RLock()
        self._accounts: dict[str, list[Account]] = {p: [] for p in PROVIDERS}
        self._settings: dict = {}
        self._rotation: dict[str, int] = {p: 0 for p in PROVIDERS}
        self._init_db()
        self._load()

    # ── SQLite 基础 ──────────────────────────────────────────────────────────
    def _connect(self) -> sqlite3.Connection:
        conn = sqlite3.connect(settings.DB_PATH, check_same_thread=False)
        conn.row_factory = sqlite3.Row
        conn.execute("PRAGMA journal_mode=WAL")
        conn.execute("PRAGMA synchronous=NORMAL")
        conn.execute("PRAGMA busy_timeout=5000")
        return conn

    def _init_db(self) -> None:
        settings.DATA_DIR.mkdir(parents=True, exist_ok=True)
        with closing(self._connect()) as conn:
            conn.executescript(
                f"""
                CREATE TABLE IF NOT EXISTS {_META} (
                    key   TEXT PRIMARY KEY,
                    value TEXT NOT NULL
                );
                CREATE TABLE IF NOT EXISTS {_TBL} (
                    id          TEXT PRIMARY KEY,
                    provider    TEXT NOT NULL,
                    name        TEXT,
                    mode        TEXT,
                    status      TEXT,
                    enabled     INTEGER NOT NULL DEFAULT 1,
                    created_at  REAL,
                    data        TEXT NOT NULL
                );
                CREATE INDEX IF NOT EXISTS idx_acc_provider ON {_TBL} (provider);
                CREATE INDEX IF NOT EXISTS idx_acc_status   ON {_TBL} (status);
                """
            )
            conn.execute(
                f"INSERT OR IGNORE INTO {_META} (key, value) VALUES ('admin_key', ?)",
                (settings.DEFAULT_ADMIN_KEY,),
            )
            conn.execute(
                f"INSERT OR IGNORE INTO {_META} (key, value) VALUES ('gateway_key', '')"
            )
            conn.execute(
                f"INSERT OR IGNORE INTO {_META} (key, value) VALUES ('quota_refresh_interval', ?)",
                (str(settings.QUOTA_REFRESH_INTERVAL),),
            )
            conn.commit()

    def _load(self) -> None:
        with closing(self._connect()) as conn:
            meta_rows = conn.execute(f"SELECT key, value FROM {_META}").fetchall()
            self._settings = {r["key"]: r["value"] for r in meta_rows}
            self._settings.setdefault("admin_key", settings.DEFAULT_ADMIN_KEY)
            self._settings.setdefault("gateway_key", "")
            self._settings.setdefault("quota_refresh_interval", str(settings.QUOTA_REFRESH_INTERVAL))

            self._accounts = {p: [] for p in PROVIDERS}
            rows = conn.execute(
                f"SELECT data FROM {_TBL} ORDER BY created_at ASC"
            ).fetchall()
            for row in rows:
                try:
                    account = Account.from_dict(json.loads(row["data"]))
                except (json.JSONDecodeError, TypeError):
                    continue
                if account.provider in self._accounts:
                    self._accounts[account.provider].append(account)

    def _persist_account(self, account: Account) -> None:
        with closing(self._connect()) as conn:
            conn.execute(
                f"""INSERT OR REPLACE INTO {_TBL}
                    (id, provider, name, mode, status, enabled, created_at, data)
                    VALUES (?,?,?,?,?,?,?,?)""",
                (
                    account.id, account.provider, account.name, account.mode,
                    account.status, 1 if account.enabled else 0, account.created_at,
                    json.dumps(account.to_dict(), ensure_ascii=False),
                ),
            )
            conn.commit()

    def _delete_account(self, account_id: str) -> None:
        with closing(self._connect()) as conn:
            conn.execute(f"DELETE FROM {_TBL} WHERE id = ?", (account_id,))
            conn.commit()

    def _set_meta(self, key: str, value: str) -> None:
        with closing(self._connect()) as conn:
            conn.execute(
                f"INSERT OR REPLACE INTO {_META} (key, value) VALUES (?, ?)",
                (key, value),
            )
            conn.commit()

    def save(self) -> None:
        """全量落库（兜底接口）。"""
        with self._lock:
            for accounts in self._accounts.values():
                for account in accounts:
                    self._persist_account(account)

    # ── 设置 ─────────────────────────────────────────────────────────────────
    def get_setting(self, key: str, default=None):
        with self._lock:
            return self._settings.get(key, default)

    def set_setting(self, key: str, value) -> None:
        with self._lock:
            self._settings[key] = str(value)
            self._set_meta(key, str(value))

    def admin_key(self) -> str:
        return str(self.get_setting("admin_key", settings.DEFAULT_ADMIN_KEY) or "")

    def gateway_key(self) -> str:
        return str(self.get_setting("gateway_key", "") or "")

    def quota_refresh_interval(self) -> int:
        try:
            return max(0, int(self.get_setting("quota_refresh_interval", settings.QUOTA_REFRESH_INTERVAL)))
        except (TypeError, ValueError):
            return settings.QUOTA_REFRESH_INTERVAL

    # ── 代理設定 ────────────────────────────────────────────────────────────
    def list_proxy_profiles(self) -> list[dict]:
        """讀取命名代理出口；設定以 JSON 儲存在 meta 表中。"""
        raw = self.get_setting("proxy_profiles", "[]")
        try:
            profiles = json.loads(raw)
        except (TypeError, json.JSONDecodeError):
            return []
        if not isinstance(profiles, list):
            return []
        return [p for p in profiles if isinstance(p, dict) and p.get("id") and p.get("url")]

    def save_proxy_profiles(self, profiles: list[dict]) -> None:
        self.set_setting("proxy_profiles", json.dumps(profiles, ensure_ascii=False))

    def add_proxy_profile(self, name: str, url: str, enabled: bool = True) -> dict:
        from .proxy import normalize_proxy_url

        normalized = normalize_proxy_url(url)
        if not normalized:
            raise ValueError("代理 URL 不能為空")
        name = (name or "").strip() or f"代理-{len(self.list_proxy_profiles()) + 1}"
        profiles = self.list_proxy_profiles()
        if any(p.get("name") == name for p in profiles):
            raise ValueError("代理名稱已存在")
        profile = {"id": f"proxy-{secrets.token_hex(4)}", "name": name,
                   "url": normalized, "enabled": bool(enabled)}
        profiles.append(profile)
        self.save_proxy_profiles(profiles)
        return profile

    def update_proxy_profile(self, profile_id: str, name: str, url: str, enabled: bool = True) -> dict | None:
        from .proxy import normalize_proxy_url

        normalized = normalize_proxy_url(url)
        if not normalized:
            raise ValueError("代理 URL 不能為空")
        profiles = self.list_proxy_profiles()
        target = next((p for p in profiles if p["id"] == profile_id), None)
        if target is None:
            return None
        name = (name or "").strip() or target["name"]
        if any(p["id"] != profile_id and p.get("name") == name for p in profiles):
            raise ValueError("代理名稱已存在")
        target.update({"name": name, "url": normalized, "enabled": bool(enabled)})
        self.save_proxy_profiles(profiles)
        for account in self.list_accounts():
            if account.proxy_id == profile_id:
                account.proxy_url = normalized
                self.update_account(account)
        return target

    def delete_proxy_profile(self, profile_id: str) -> bool:
        profiles = self.list_proxy_profiles()
        remaining = [p for p in profiles if p.get("id") != profile_id]
        if len(remaining) == len(profiles):
            return False
        self.save_proxy_profiles(remaining)
        for account in self.list_accounts():
            if account.proxy_id == profile_id:
                account.proxy_id = None
                account.proxy_url = None
                self.update_account(account)
        return True

    def assign_proxy_profile(self, account_id: str, profile_id: str | None) -> bool:
        account = self.find_any(account_id)
        if account is None:
            return False
        profile = next((p for p in self.list_proxy_profiles() if p.get("id") == profile_id), None) if profile_id else None
        if profile_id and profile is None:
            raise ValueError("代理配置不存在")
        account.proxy_id = profile_id
        account.proxy_url = profile["url"] if profile else None
        self.update_account(account)
        return True

    # ── 账号读取 ─────────────────────────────────────────────────────────────
    def list_accounts(self, provider: str | None = None) -> list[Account]:
        with self._lock:
            if provider:
                return list(self._accounts.get(provider, []))
            return [a for p in PROVIDERS for a in self._accounts[p]]

    def find(self, provider: str, id_or_name: str) -> Account | None:
        with self._lock:
            return self._find_locked(provider, id_or_name)

    def find_any(self, id_or_name: str) -> Account | None:
        with self._lock:
            for p in PROVIDERS:
                for a in self._accounts[p]:
                    if a.id == id_or_name:
                        return a
        return None

    def _find_locked(self, provider: str, id_or_name: str) -> Account | None:
        for a in self._accounts.get(provider, []):
            if a.id == id_or_name or a.name == id_or_name:
                return a
        return None

    # ── 账号增删改 ───────────────────────────────────────────────────────────
    def add_account(self, provider: str, name: str, secret: str) -> Account:
        if provider not in PROVIDERS:
            raise ValueError(f"不支持的 provider: {provider}")
        account = Account.create(provider, name, secret)
        with self._lock:
            for a in self._accounts[provider]:
                if a.secret and a.secret == account.secret:
                    return a  # 跳过重复 token
            self._accounts[provider].append(account)
            self._persist_account(account)
        return account

    def remove_account(self, provider: str, id_or_name: str) -> bool:
        with self._lock:
            items = self._accounts.get(provider, [])
            target = next((a for a in items if a.id == id_or_name or a.name == id_or_name), None)
            if not target:
                return False
            self._accounts[provider] = [a for a in items if a.id != target.id]
            self._delete_account(target.id)
            return True

    def update_account(self, account: Account) -> None:
        """持久化某个账号的当前状态。"""
        with self._lock:
            self._persist_account(account)

    def set_enabled(self, provider: str, id_or_name: str, enabled: bool) -> bool:
        with self._lock:
            account = self._find_locked(provider, id_or_name)
            if not account:
                return False
            account.enabled = enabled
            if not enabled:
                account.status = Status.DISABLED
            elif account.status == Status.DISABLED:
                account.status = Status.ACTIVE
            self._persist_account(account)
            return True

    # ── 轮询选择 ─────────────────────────────────────────────────────────────
    def select(
        self,
        provider: str,
        skip_ids: set[str] | None = None,
        model: str | None = None,
    ) -> Account | None:
        """按模型額度與 round-robin 選擇帳號，未知額度僅作後備。"""
        skip_ids = skip_ids or set()
        now = time.time()
        with self._lock:
            base_pool = [
                a for a in self._accounts.get(provider, [])
                if a.is_selectable(now) and a.id not in skip_ids
            ]
            pool = base_pool
            if model:
                available = [a for a in base_pool if a.model_availability(model) == "available"]
                unknown = [a for a in base_pool if a.model_availability(model) == "unknown"]
                pool = available or unknown
            if not pool:
                return None
            rotation_key = f"{provider}:{model or '*'}"
            idx = self._rotation.get(rotation_key, 0) % len(pool)
            account = pool[idx]
            self._rotation[rotation_key] = (idx + 1) % len(pool)
            return account

    # ── 导入 / 导出 ─────────────────────────────────────────────────────────
    def export(self) -> dict:
        with self._lock:
            return {
                "version": 1,
                "exported_at": time.time(),
                "providers": {
                    p: [
                        {"name": a.name, "mode": a.mode, "secret": a.secret}
                        for a in self._accounts[p]
                    ]
                    for p in PROVIDERS
                },
            }

    def import_accounts(self, payload: dict) -> int:
        providers = payload.get("providers", {})
        count = 0
        for provider, items in providers.items():
            if provider not in PROVIDERS or not isinstance(items, list):
                continue
            for it in items:
                secret = it.get("secret") or it.get("token") or it.get("jwtToken") or it.get("apiKey")
                if not secret:
                    continue
                self.add_account(provider, it.get("name", provider), secret)
                count += 1
        return count


# 单例
store = Store()
