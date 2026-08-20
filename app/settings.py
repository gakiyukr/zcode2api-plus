"""运行期配置：环境变量 + 默认值。

所有可调参数集中在此。账号与凭证不在此处，而是持久化到 data/ 目录（见 store.py）。
"""

from __future__ import annotations

import os
from pathlib import Path

from dotenv import load_dotenv

load_dotenv()

# 项目根目录
ROOT_DIR = Path(__file__).resolve().parents[1]


def _resolve_path(env_name: str, default: str) -> Path:
    raw = (os.getenv(env_name, default) or default).strip()
    path = Path(raw)
    if not path.is_absolute():
        path = ROOT_DIR / path
    return path


def _int(env_name: str, default: int) -> int:
    try:
        return int(os.getenv(env_name, str(default)))
    except (TypeError, ValueError):
        return default


# ── 目录 ─────────────────────────────────────────────────────────────────────
DATA_DIR = _resolve_path("ZCODE_DATA_DIR", "data")
# 账号与设置持久化到本地 SQLite（与 grok2api 的 local 后端一致）
DB_PATH = DATA_DIR / "accounts.db"
STATIC_DIR = Path(__file__).resolve().parent / "statics"

# ── 服务 ─────────────────────────────────────────────────────────────────────
PORT = _int("ZCODE_PORT", 3000)
HOST = os.getenv("ZCODE_HOST", "0.0.0.0")

# ── 鉴权 ─────────────────────────────────────────────────────────────────────
# 后台管理密码默认值，首次启动写入 data/accounts.db，之后以数据库（meta 表）为准。
DEFAULT_ADMIN_KEY = os.getenv("ZCODE_ADMIN_KEY", "zcode")

# ── 验证码缓存 ───────────────────────────────────────────────────────────────
CAPTCHA_CACHE_TTL = _int("CAPTCHA_CACHE_TTL", 45_000)          # ms
CAPTCHA_CONFIG_CACHE_TTL = _int("CAPTCHA_CONFIG_CACHE_TTL", 600_000)  # ms
CAPTCHA_MANUAL_CACHE_TTL = _int("CAPTCHA_MANUAL_CACHE_TTL", 45_000)  # ms，与上游短期 token 生命周期一致

# 验证码求解（无浏览器：Node + jsdom 模拟浏览器环境，运行阿里云无痕 SDK）
NODE_PATH = os.getenv("ZCODE_NODE_PATH", "node")
CAPTCHA_SOLVER_DIR = ROOT_DIR / "captcha_node"
CAPTCHA_SOLVER_JS = CAPTCHA_SOLVER_DIR / "solver.js"
CAPTCHA_SOLVE_RETRIES = _int("ZCODE_CAPTCHA_RETRIES", 4)
CAPTCHA_SOLVE_TIMEOUT = _int("ZCODE_CAPTCHA_TIMEOUT", 40)  # 每次求解超时（秒）

# 验证码求解（真实 Chromium：Docker 镜像默认启用；源码运行可显式打开）
CAPTCHA_BROWSER_ENABLED = (os.getenv("ZCODE_CAPTCHA_BROWSER", "false") or "false").strip().lower() not in {
    "0", "false", "no", "off",
}
CAPTCHA_BROWSER_WORKERS = max(1, _int("ZCODE_CAPTCHA_BROWSER_WORKERS", 1))
CAPTCHA_BROWSER_STARTUP_TIMEOUT = max(5, _int("ZCODE_CAPTCHA_BROWSER_STARTUP_TIMEOUT", 90))
CAPTCHA_BROWSER_REQUEST_TIMEOUT = max(5, _int("ZCODE_CAPTCHA_BROWSER_REQUEST_TIMEOUT", 45))
CAPTCHA_BROWSER_QUEUE_TIMEOUT = max(1, _int("ZCODE_CAPTCHA_BROWSER_QUEUE_TIMEOUT", 60))
CAPTCHA_BROWSER_SHUTDOWN_TIMEOUT = max(1, _int("ZCODE_CAPTCHA_BROWSER_SHUTDOWN_TIMEOUT", 10))
CAPTCHA_BROWSER_FAILURE_COOLDOWN = max(1, _int("ZCODE_CAPTCHA_BROWSER_FAILURE_COOLDOWN", 60))

# ── 设备身份 ─────────────────────────────────────────────────────────────────
# 设备唯一标识（UUIDv4），首次启动生成并持久化到 data/device_mid.txt
def _load_or_generate_device_mid() -> str:
    """首次启动生成 UUIDv4，后续从文件读取。"""
    import uuid
    DATA_DIR.mkdir(parents=True, exist_ok=True)
    device_mid_file = DATA_DIR / "device_mid.txt"
    if device_mid_file.exists():
        mid = device_mid_file.read_text(encoding="utf-8").strip()
        if mid:
            return mid
    mid = str(uuid.uuid4())
    device_mid_file.write_text(mid, encoding="utf-8")
    return mid

DEVICE_MID = _load_or_generate_device_mid()

# ── Async 空闲池 ─────────────────────────────────────────────────────────────
# 是否启用异步空闲池路由（/async/v1/messages 和 /async/v1/chat/completions）
ASYNC_ENABLED = (os.getenv("ZCODE_ASYNC_ENABLED", "true") or "true").strip().lower() not in {
    "0", "false", "no", "off",
}
ASYNC_TICKET_TIMEOUT = max(30, _int("ZCODE_ASYNC_TICKET_TIMEOUT", 300))
ASYNC_MAX_RETRIES = max(0, _int("ZCODE_ASYNC_MAX_RETRIES", 3))

# ── 用量监控 ─────────────────────────────────────────────────────────────────
# 后台自动刷新账号额度的间隔（秒）。0 表示关闭后台轮询，仅按需刷新。
QUOTA_REFRESH_INTERVAL = _int("ZCODE_QUOTA_REFRESH_INTERVAL", 60)
# 限流（cooling）冷却时长（秒）
COOLING_SECONDS = _int("ZCODE_COOLING_SECONDS", 300)

# ── 上游端点 ─────────────────────────────────────────────────────────────────
UPSTREAM = {
    "zai": os.getenv(
        "ZAI_UPSTREAM_URL",
        "https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages",
    ),
    "zai_fallback": os.getenv(
        "ZAI_FALLBACK_URL",
        "https://api.z.ai/api/anthropic/v1/messages",
    ),
    "bigmodel": os.getenv(
        "BIGMODEL_UPSTREAM_URL",
        "https://open.bigmodel.cn/api/anthropic/v1/messages",
    ),
}

# ZCode 计费 / 额度查询端点
ZCODE_BILLING_BASE = "https://zcode.z.ai/api/v1/zcode-plan"
ZCODE_CLIENT_VERSION = os.getenv("ZCODE_CLIENT_VERSION", "3.7.7")
# 与 solver.js 中的 Windows 浏览器指纹保持一致；旧的 win32 参数已失效。
ZCODE_CLIENT_PLATFORM = os.getenv("ZCODE_CLIENT_PLATFORM", "win32-x64")

USER_AGENT = os.getenv("UPSTREAM_USER_AGENT", f"ZCode/{ZCODE_CLIENT_VERSION}")
APP_VERSION = "2.0.0"
