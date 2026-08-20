"""验证码管理：真实 Chromium 自动求解优先，人工回填作为兜底。"""

from __future__ import annotations

import asyncio
import re
import time
from dataclasses import dataclass
from urllib.parse import urlencode

import httpx

from . import logs, settings
from .captcha_browser import BrowserWorkerPool, CaptchaBrowserError


_DEFAULT_CONFIG = {"enabled": True, "prefix": "no8xfe", "region": "sgp", "sceneId": "11xygtvd"}


@dataclass(frozen=True)
class CaptchaToken:
    verify_param: str
    region: str


class CaptchaManager:
    def __init__(self) -> None:
        self._cached: CaptchaToken | None = None
        self._cached_at: float = 0.0
        self._cached_ttl: float = 0.0
        self._lock = asyncio.Lock()
        self._config_cache: dict | None = None
        self._config_cache_at: float = 0.0
        self._browser_pool: BrowserWorkerPool | None = None
        self._browser_config_key: tuple[str, str, str] | None = None
        self._browser_failure_until: float = 0.0

    # ── 配置 ─────────────────────────────────────────────────────────────────
    async def fetch_config(self) -> dict:
        now = time.time() * 1000
        if self._config_cache and now - self._config_cache_at < settings.CAPTCHA_CONFIG_CACHE_TTL:
            return self._config_cache
        try:
            query = urlencode({
                "app_version": settings.ZCODE_CLIENT_VERSION,
                "platform": settings.ZCODE_CLIENT_PLATFORM,
            })
            async with httpx.AsyncClient(timeout=15) as client:
                res = await client.get(f"https://zcode.z.ai/api/v1/client/configs?{query}")
            res.raise_for_status()
            payload = res.json()
            captcha = ((payload.get("data") or {}).get("configs") or {}).get("captcha")
            if isinstance(captcha, dict):
                self._config_cache = captcha
                self._config_cache_at = now
                return captcha
            raise ValueError("上游配置缺少 captcha 对象")
        except (httpx.HTTPError, ValueError, TypeError) as err:
            logs.warn("captcha", f"获取配置失败，使用默认: {err}")
        return dict(_DEFAULT_CONFIG)

    # ── 求解 ─────────────────────────────────────────────────────────────────
    async def get_verify_param(self, port: int | None = None) -> CaptchaToken | None:
        """返回验证码令牌；真实浏览器令牌每次请求重新生成，人工令牌短期复用。"""
        del port  # 保留旧调用参数，验证码服务本身不依赖网关端口。
        now = time.time() * 1000
        if self._cached and now - self._cached_at < self._cached_ttl:
            return self._cached

        async with self._lock:
            # 二次检查：等锁期间可能已被其他请求或人工页面填充
            now = time.time() * 1000
            if self._cached and now - self._cached_at < self._cached_ttl:
                return self._cached

            config = await self.fetch_config()
            if config.get("enabled") is False:
                self.invalidate()
                await self._stop_browser_pool()
                return None

            # Docker 默认使用真实 Chromium；浏览器启动失败时回退旧 Node 求解器。
            browser_token = await self._solve_browser(config)
            if browser_token is not None:
                # 上游验证码参数可能是一次性的，不能像人工回填一样缓存 45 秒。
                return browser_token

            token = await self._solve(config)
            self._cached = token
            self._cached_at = time.time() * 1000
            self._cached_ttl = settings.CAPTCHA_CACHE_TTL
            return token

    async def set_manual_param(self, param: str, region: str | None = None) -> None:
        """缓存真实浏览器完成验证得到的短期结果，不写入磁盘。"""
        param = (param or "").strip()
        if not param:
            raise ValueError("verify_param 不能为空")
        async with self._lock:
            config_region = (self._config_cache or {}).get("region")
            token_region = (region or config_region or _DEFAULT_CONFIG["region"]).strip()
            if not token_region:
                raise ValueError("验证码 region 不能为空")
            self._cached = CaptchaToken(verify_param=param, region=token_region)
            self._cached_at = time.time() * 1000
            self._cached_ttl = settings.CAPTCHA_MANUAL_CACHE_TTL

    async def _solve_browser(self, config: dict) -> CaptchaToken | None:
        if not settings.CAPTCHA_BROWSER_ENABLED:
            return None

        scene = str(config.get("sceneId") or "").strip()
        region = str(config.get("region") or "").strip()
        prefix = str(config.get("prefix") or "").strip()
        if not scene or not region or not prefix:
            return None
        if time.monotonic() < self._browser_failure_until:
            return None

        pool = await self._ensure_browser_pool(scene, region, prefix)
        if pool is None:
            return None
        try:
            param = await pool.get_token(acquire_timeout=settings.CAPTCHA_BROWSER_QUEUE_TIMEOUT)
        except CaptchaBrowserError as err:
            # 池已启动但本次求解失败时交给网关有限重试，避免退回已被 F001
            # 拒绝的 jsdom 结果；池自身会负责替换超时或退出的 worker。
            logs.warn("captcha", f"真实浏览器验证码求解失败: {type(err).__name__}")
            raise RuntimeError("真实浏览器验证码求解失败") from err
        if not param:
            raise RuntimeError("真实浏览器验证码求解器返回空结果")
        return CaptchaToken(verify_param=param, region=region)

    async def _ensure_browser_pool(self, scene: str, region: str, prefix: str) -> BrowserWorkerPool | None:
        key = (scene, region, prefix)
        if self._browser_pool and self._browser_config_key == key and self._browser_pool.is_started:
            return self._browser_pool

        await self._stop_browser_pool()
        pool: BrowserWorkerPool | None = None
        try:
            pool = BrowserWorkerPool.from_cloakbrowser(
                scene,
                region,
                prefix,
                size=settings.CAPTCHA_BROWSER_WORKERS,
                startup_timeout=float(settings.CAPTCHA_BROWSER_STARTUP_TIMEOUT),
                request_timeout=float(settings.CAPTCHA_BROWSER_REQUEST_TIMEOUT),
                queue_timeout=float(settings.CAPTCHA_BROWSER_QUEUE_TIMEOUT),
                shutdown_timeout=float(settings.CAPTCHA_BROWSER_SHUTDOWN_TIMEOUT),
            )
            await pool.start()
        except Exception as err:  # noqa: BLE001
            self._browser_failure_until = time.monotonic() + settings.CAPTCHA_BROWSER_FAILURE_COOLDOWN
            logs.warn(
                "captcha",
                f"真实浏览器池不可用，{settings.CAPTCHA_BROWSER_FAILURE_COOLDOWN}s 内回退 Node: {type(err).__name__}",
            )
            if pool is not None:
                try:
                    await pool.stop()
                except Exception:  # noqa: BLE001
                    pass
            return None

        self._browser_pool = pool
        self._browser_config_key = key
        self._browser_failure_until = 0.0
        logs.ok("captcha", f"真实浏览器验证码池已就绪（{settings.CAPTCHA_BROWSER_WORKERS} 个 worker）")
        return pool

    async def _stop_browser_pool(self) -> None:
        pool = self._browser_pool
        self._browser_pool = None
        self._browser_config_key = None
        if pool is not None:
            try:
                await pool.stop()
            except Exception as err:  # noqa: BLE001
                logs.warn("captcha", f"关闭真实浏览器验证码池失败: {type(err).__name__}")

    async def _solve(self, config: dict) -> CaptchaToken:
        scene = str(config.get("sceneId") or "").strip()
        region = str(config.get("region") or "").strip()
        prefix = str(config.get("prefix") or "").strip()
        if not scene or not region or not prefix:
            raise RuntimeError("验证码配置缺少 sceneId、region 或 prefix")

        last_err: str | None = None
        for attempt in range(1, settings.CAPTCHA_SOLVE_RETRIES + 1):
            try:
                param = await self._run_solver(scene, region, prefix)
            except Exception as err:  # noqa: BLE001
                last_err = str(err)
                param = None
                if "F001" in last_err:
                    logs.warn("captcha", "Node 无浏览器求解被上游风控拒绝（F001），请依赖真实浏览器自动池或后台验证")
                    break
            if param:
                if attempt > 1:
                    logs.ok("captcha", f"求解成功（第 {attempt} 次尝试）")
                return CaptchaToken(verify_param=param, region=region)
            if last_err and "F001" in last_err:
                break
            logs.warn("captcha", f"第 {attempt}/{settings.CAPTCHA_SOLVE_RETRIES} 次求解未果，重试…")

        raise RuntimeError(f"验证码求解失败: {last_err or '多次重试无结果'}")

    async def _run_solver(self, scene: str, region: str, prefix: str) -> str | None:
        if not settings.CAPTCHA_SOLVER_JS.exists():
            raise RuntimeError(
                f"未找到求解器 {settings.CAPTCHA_SOLVER_JS}，请先在 captcha_node 下执行 npm install"
            )
        try:
            proc = await asyncio.create_subprocess_exec(
                settings.NODE_PATH, str(settings.CAPTCHA_SOLVER_JS), scene, region, prefix,
                cwd=str(settings.CAPTCHA_SOLVER_DIR),
                stdout=asyncio.subprocess.PIPE,
                stderr=asyncio.subprocess.PIPE,
            )
        except FileNotFoundError as err:
            raise RuntimeError(f"无法启动 Node（{settings.NODE_PATH}）: {err}") from err

        try:
            stdout, stderr = await asyncio.wait_for(proc.communicate(), timeout=settings.CAPTCHA_SOLVE_TIMEOUT)
        except asyncio.TimeoutError as err:
            try:
                proc.kill()
            except ProcessLookupError:
                pass
            await proc.communicate()
            raise RuntimeError("验证码求解器超时") from err

        if proc.returncode:
            detail = _compact_solver_error(stderr)
            raise RuntimeError(f"求解器退出码 {proc.returncode}{f': {detail}' if detail else ''}")

        for line in stdout.decode("utf-8", "ignore").splitlines():
            if line.startswith("VERIFY_PARAM="):
                param = line[len("VERIFY_PARAM="):].strip()
                if param:
                    return param
        detail = _compact_solver_error(stderr)
        raise RuntimeError(f"求解器未返回验证码结果{f': {detail}' if detail else ''}")

    def invalidate(self) -> None:
        self._cached = None
        self._cached_at = 0.0
        self._cached_ttl = 0.0

    async def close(self) -> None:
        async with self._lock:
            await self._stop_browser_pool()


def _compact_solver_error(stderr: bytes) -> str:
    """只保留脱敏错误摘要，避免把验证码参数写入日志。"""
    text = stderr.decode("utf-8", "ignore").strip()
    if not text:
        return ""
    text = re.sub(r"VERIFY_PARAM=.*", "VERIFY_PARAM=[redacted]", text)
    text = re.sub(r"[A-Za-z0-9+/=_-]{64,}", "[redacted]", text)
    return text[-500:]


captcha_manager = CaptchaManager()
