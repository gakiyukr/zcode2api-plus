"""验证码浏览器 worker 池：封装 cloakbrowser 真实 Chromium。

替代 captcha_node/solver.js（jsdom 仿真被上游风控识别为 F001）的方案：
池管理一批真实 Chromium（cloakbrowser）求解进程，token 只存在于内存，
不落盘、不写入日志。

行协议（UTF-8、\n 结尾、逐行）：
    worker → 池:  READY / TOKEN <param> / FAILED <reason>
    池 → worker:  GET_TOKEN / SHUTDOWN

池的职责边界：
    - 生命周期：有界 worker 数、启动总超时、优雅关闭、异常 worker 自动替换。
    - 调度：一次 get_token = 一次 GET_TOKEN，TOKEN 返回、FAILED 抛出
      SolverFailureError（worker 仍健康）、超时/退出则替换 worker 后抛错。
    - 超时后响应错位防护：请求超时即标记该 worker broken、杀掉进程，
      迟到的 TOKEN 行只会在旧进程管道上被丢弃，永远不会投递到后续请求。
    - 重试与缓存属于调用方（captcha.py 的 get_verify_param 等）职责，池不代劳。

真实求解由 worker 子进程完成（python -m app.captcha_browser worker），
cloakbrowser 采用惰性导入，池模块本身不依赖它。
"""

from __future__ import annotations

import argparse
import asyncio
import json
import os
import re
import sys
import time
from collections import deque
from dataclasses import dataclass, field
from pathlib import Path

from . import logs

_LOG = "captcha_browser"

# ── 行协议 ──────────────────────────────────────────────────────────────────
READY = "READY"
GET_TOKEN = "GET_TOKEN"
TOKEN = "TOKEN"
FAILED = "FAILED"
SHUTDOWN = "SHUTDOWN"

# 浏览器启动参数（与隔离审计 smoke test 一致）
_BROWSER_ARGS = [
    "--disable-dev-shm-usage",
    "--disable-background-networking",
    "--disable-component-update",
    "--disable-sync",
    "--no-first-run",
]


class CaptchaBrowserError(RuntimeError):
    """池相关错误基类。"""


class StartupError(CaptchaBrowserError):
    """池启动失败（启动总超时或 worker 未在 READY 阶段存活）。"""


class SolverTimeoutError(CaptchaBrowserError):
    """单次求解超时；对应 worker 已被替换。"""


class SolverFailureError(CaptchaBrowserError):
    """worker 明确回报 FAILED（求解被上游拒绝等），worker 本身仍健康。"""


class WorkerExitError(CaptchaBrowserError):
    """worker 提前退出（EOF / 非零退出码）。"""

    def __init__(self, message: str, code: int | None = None) -> None:
        super().__init__(message)
        self.code = code


def _redact(text: str) -> str:
    """脱敏：抹掉疑似 token/密钥的长串（≥64 字符），并折叠空白。"""
    text = re.sub(r"[A-Za-z0-9+/=_-]{64,}", "[redacted]", text or "")
    return re.sub(r"\s+", " ", text).strip()


def _tail_of(buf: deque[str], n: int = 3) -> str:
    return " | ".join(list(buf)[-n:])


@dataclass
class _WorkerSlot:
    """单个 worker 槽位状态机：starting → ready → busy → ready …；异常 → broken。"""

    wid: int
    proc: asyncio.subprocess.Process | None = None
    stdin: asyncio.StreamWriter | None = None
    state: str = "starting"  # starting / ready / busy / broken / stopped
    request: asyncio.Future | None = field(default=None, repr=False)


class BrowserWorkerPool:
    """有界 worker/token 池：管理 N 个行协议 worker 子进程。

    参数：
        size:           worker 进程数上限（同时也是并发求解上限）。
        worker_cmd:     子进程 argv；测试注入假 worker 脚本即可 mock。
        startup_timeout: 启动总超时（秒），全部 worker READY 才算成功。
        request_timeout: 单次 GET_TOKEN 等待上限（秒），超时替换 worker。
        queue_timeout:   等待空闲 worker 的最长时间（秒），避免全池宕机时挂死。
        shutdown_timeout: 优雅关闭时等待 worker 自行退出的时间（秒）。
        restart_delay:   worker 异常替换前的间隔（秒）。
        worker_env:      注入给子进程的额外环境变量（覆盖继承的环境）。
    """

    def __init__(
        self,
        *,
        size: int,
        worker_cmd: list[str],
        startup_timeout: float = 90.0,
        request_timeout: float = 75.0,
        queue_timeout: float = 60.0,
        shutdown_timeout: float = 10.0,
        restart_delay: float = 1.0,
        stderr_tail_lines: int = 30,
        worker_env: dict[str, str] | None = None,
    ) -> None:
        if size < 1:
            raise ValueError("size 必须 >= 1")
        if not worker_cmd:
            raise ValueError("worker_cmd 不能为空")
        self._size = size
        self._worker_cmd = list(worker_cmd)
        self._worker_env = dict(worker_env) if worker_env else {}
        self._startup_timeout = startup_timeout
        self._request_timeout = request_timeout
        self._queue_timeout = queue_timeout
        self._shutdown_timeout = shutdown_timeout
        self._restart_delay = restart_delay
        self._stderr_tail_lines = stderr_tail_lines

        self._started = False
        self._stopping = False
        self._startup_failed: list[Exception] = []
        self._states: list[_WorkerSlot] = [_WorkerSlot(wid=i) for i in range(size)]
        self._idle: deque[int] = deque()
        self._loops: list[asyncio.Task] = []
        self._cond = asyncio.Condition()
        self._lifecycle_lock = asyncio.Lock()
        self._slots = asyncio.Semaphore(size)
        self.stats = {
            "started": 0,
            "requests": 0,
            "timeouts": 0,
            "solver_failures": 0,
            "worker_restarts": 0,
            "discarded_lines": 0,
        }

    # ── 构建真实 worker 命令 ─────────────────────────────────────────────────
    @classmethod
    def from_cloakbrowser(
        cls,
        scene: str,
        region: str,
        prefix: str,
        *,
        size: int = 1,
        **kwargs,
    ) -> "BrowserWorkerPool":
        """构造跑 cloakbrowser 的默认池。

        默认 size=1：cloakbrowser 免费档只允许 1 个并发会话，需要更多
        并发时按授权档位显式传入 size。
        """
        worker_cmd = [
            sys.executable,
            "-m",
            "app.captcha_browser",
            "worker",
            "--scene",
            scene,
            "--region",
            region,
            "--prefix",
            prefix,
        ]
        return cls(size=size, worker_cmd=worker_cmd, **kwargs)

    # ── 生命周期 ─────────────────────────────────────────────────────────────
    @property
    def is_started(self) -> bool:
        return self._started

    async def __aenter__(self) -> "BrowserWorkerPool":
        await self.start()
        return self

    async def __aexit__(self, *exc_info) -> None:
        await self.stop()

    async def start(self) -> None:
        """启动全部 worker，并在 startup_timeout 内等待全部 READY。"""
        async with self._lifecycle_lock:
            if self._started or self._loops:
                return
            self._stopping = False
            self._startup_failed: list[Exception] = []
            self._states = [_WorkerSlot(wid=i) for i in range(self._size)]
            self._idle.clear()
            self._loops = [asyncio.create_task(self._worker_loop(i)) for i in range(self._size)]
            try:
                await asyncio.wait_for(self._wait_all_ready(), timeout=self._startup_timeout)
            except asyncio.TimeoutError as exc:
                await self._teardown(send_shutdown=False)
                raise StartupError(
                    f"启动总超时：{self._size} 个 worker 未在 {self._startup_timeout:g}s 内全部 READY"
                ) from exc
            except BaseException:
                await self._teardown(send_shutdown=False)
                raise
            self._started = True
            self.stats["started"] = self._size
            logs.ok(_LOG, f"池已启动：{self._size} 个 worker 全部 READY")

    async def _wait_all_ready(self) -> None:
        while True:
            if self._startup_failed:
                raise self._startup_failed[0]
            if all(slot.state == "ready" for slot in self._states):
                return
            await asyncio.sleep(0.05)

    async def stop(self) -> None:
        """优雅关闭：向全部 worker 发送 SHUTDOWN，等待其自行退出。"""
        async with self._lifecycle_lock:
            if not self._started and not self._loops:
                return
            await self._teardown(send_shutdown=True)
            logs.ok(_LOG, "池已关闭")

    async def _teardown(self, send_shutdown: bool) -> None:
        self._stopping = True
        async with self._cond:
            self._cond.notify_all()
        if send_shutdown:
            for slot in self._states:
                if slot.proc is not None and slot.stdin is not None:
                    try:
                        slot.stdin.write(f"{SHUTDOWN}\n".encode("utf-8"))
                        await slot.stdin.drain()
                    except (BrokenPipeError, ConnectionResetError, OSError):
                        pass
        if self._loops:
            _, pending = await asyncio.wait(self._loops, timeout=self._shutdown_timeout)
            for task in pending:
                task.cancel()
            await asyncio.gather(*self._loops, return_exceptions=True)
            self._loops = []
        # 快速失败在途请求：避免池已关闭仍在等待响应
        for slot in self._states:
            fut = slot.request
            if fut is not None and not fut.done():
                slot.request = None
                fut.set_exception(CaptchaBrowserError("池已关闭"))
        await self._kill_all()
        self._idle.clear()
        self._states = [_WorkerSlot(wid=i) for i in range(self._size)]
        self._started = False
        self._stopping = False

    async def _kill_all(self) -> None:
        for slot in self._states:
            if slot.proc is not None:
                await self._kill(slot)

    @staticmethod
    async def _kill(slot: _WorkerSlot) -> None:
        proc = slot.proc
        if proc is None or proc.returncode is not None:
            return
        try:
            proc.kill()
        except ProcessLookupError:
            return
        try:
            await asyncio.wait_for(proc.wait(), timeout=2.0)
        except asyncio.TimeoutError:
            pass

    # ── 求解入口 ─────────────────────────────────────────────────────────────
    async def get_token(self, *, acquire_timeout: float | None = None) -> str:
        """向池取一个验证码 token（内存，不落盘）。

        返回 worker 求解出的 verify_param；失败抛 CaptchaBrowserError
        子类。并发上限 = size。
        """
        if not self._started:
            raise CaptchaBrowserError("池未启动：请先 await pool.start()")
        timeout = self._queue_timeout if acquire_timeout is None else acquire_timeout
        async with self._slots:
            wid = await self._acquire_worker(timeout)
            try:
                return await self._request(wid)
            finally:
                await self._release_worker(wid)

    async def _acquire_worker(self, timeout: float) -> int:
        loop = asyncio.get_running_loop()
        deadline = loop.time() + timeout
        while True:
            async with self._cond:
                if self._stopping:
                    raise CaptchaBrowserError("池已关闭")
                while self._idle:
                    wid = self._idle.popleft()
                    slot = self._states[wid]
                    if slot.state == "ready":
                        slot.state = "busy"
                        return wid
                    # 槽位已失效（broken/stopped）——丢弃
                remaining = deadline - loop.time()
                if remaining <= 0:
                    raise CaptchaBrowserError(f"等待空闲 worker 超时（{timeout:g}s）")
                try:
                    await asyncio.wait_for(self._cond.wait(), timeout=remaining)
                except asyncio.TimeoutError:
                    continue  # 回到循环顶部重新判断

    async def _release_worker(self, wid: int) -> None:
        slot = self._states[wid]
        if slot.state != "busy":
            return
        slot.state = "ready"
        slot.request = None
        async with self._cond:
            self._idle.append(wid)
            self._cond.notify()

    async def _request(self, wid: int) -> str:
        slot = self._states[wid]
        fut = asyncio.get_running_loop().create_future()
        slot.request = fut
        try:
            slot.stdin.write(f"{GET_TOKEN}\n".encode("utf-8"))
            await slot.stdin.drain()
        except (BrokenPipeError, ConnectionResetError, OSError) as exc:
            slot.request = None
            fut.cancel()
            await self._mark_broken(wid, f"stdin 写入失败: {_redact(str(exc))}")
            raise WorkerExitError(f"worker {wid} 通信失败") from exc
        self.stats["requests"] += 1
        done, _ = await asyncio.wait({fut}, timeout=self._request_timeout)
        if not done:
            self.stats["timeouts"] += 1
            # 置空槽位请求 + 取消 future：迟到的 TOKEN/FAILED/EOF 无法再投递，
            # 旧进程将被杀掉，新进程使用全新管道——杜绝响应错位。
            slot.request = None
            fut.cancel()
            await self._mark_broken(wid, "求解超时")
            raise SolverTimeoutError(f"worker {wid} 求解超时（>{self._request_timeout:g}s），已替换")
        return fut.result()

    # ── worker 循环（每槽一个任务）───────────────────────────────────────────
    async def _worker_loop(self, wid: int) -> None:
        while not self._stopping:
            slot = self._states[wid]
            try:
                proc = await self._spawn(wid)
            except OSError as exc:
                if not self._started:
                    self._startup_failed.append(
                        StartupError(f"无法启动 worker {wid}（{self._worker_cmd[0]}）: {_redact(str(exc))}")
                    )
                    return
                logs.warn(_LOG, f"worker {wid} 启动失败，稍后重试: {_redact(str(exc))}")
                await asyncio.sleep(self._restart_delay)
                continue
            slot.proc = proc
            slot.stdin = proc.stdin
            try:
                outcome = await self._drive_worker(wid, proc)
            except Exception as exc:  # noqa: BLE001
                if not self._started:
                    self._startup_failed.append(StartupError(f"worker {wid} 驱动异常: {_redact(str(exc))}"))
                    return
                logs.warn(_LOG, f"worker {wid} 驱动异常: {_redact(str(exc))}")
                await self._kill(slot)
                await asyncio.sleep(self._restart_delay)
                continue
            if outcome == "shutdown":
                break
            if not self._started:
                # 启动阶段（start() 尚未完成）不允许替换，经信号上报失败
                self._startup_failed.append(StartupError(f"worker {wid} 未在 READY 阶段存活"))
                return
            await asyncio.sleep(self._restart_delay)
        self._states[wid] = _WorkerSlot(wid=wid)

    async def _spawn(self, wid: int):
        env = None
        if self._worker_env:
            env = dict(os.environ)
            env.update(self._worker_env)
        return await asyncio.create_subprocess_exec(
            *self._worker_cmd,
            stdin=asyncio.subprocess.PIPE,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
            env=env,
        )

    async def _drive_worker(self, wid: int, proc) -> str:
        """驱动单个进程直到 EOF / 退出；stderr 由独立任务持续排空，不阻塞。"""
        stderr_buf: deque[str] = deque(maxlen=self._stderr_tail_lines)
        drainer = asyncio.create_task(self._drain_stderr(proc, stderr_buf))
        try:
            while True:
                line = await proc.stdout.readline()
                if not line:  # EOF
                    return await self._on_eof(wid, proc, stderr_buf)
                await self._handle_line(wid, line)
        finally:
            drainer.cancel()
            await asyncio.gather(drainer, return_exceptions=True)

    async def _on_eof(self, wid: int, proc, stderr_buf: deque[str]) -> str:
        code = proc.returncode
        if code is None:
            code = await proc.wait()
        tail = _tail_of(stderr_buf)
        detail = f"（code={code}）{f': {tail}' if tail else ''}"
        # 无论是否正在关闭：先快速失败在途请求，避免挂到请求超时
        self._fail_pending(wid, WorkerExitError(f"worker {wid} 提前退出{detail}", code=code))
        if self._stopping:
            return "shutdown"
        await self._mark_broken(wid, f"提前退出{detail}")
        return "dead"

    async def _drain_stderr(self, proc, buf: deque[str]) -> None:
        while True:
            line = await proc.stderr.readline()
            if not line:
                return
            text = _redact(line.decode("utf-8", "ignore"))
            if text:
                buf.append(text)

    # ── 协议处理（读循环调用）───────────────────────────────────────────────
    async def _handle_line(self, wid: int, line: bytes) -> None:
        text = line.decode("utf-8", "ignore").strip()
        if text == READY:
            await self._on_ready(wid)
            return
        if text.startswith(f"{TOKEN} "):
            param = text[len(f"{TOKEN} "):].strip()
            if param:
                self._resolve_token(wid, param)
            else:
                self._resolve_fail(wid, "空 token")
            return
        if text.startswith(f"{FAILED} "):
            self._resolve_fail(wid, text[len(f"{FAILED} "):].strip() or "未知错误")
            return
        self.stats["discarded_lines"] += 1  # 协议噪音（含超时后迟到的消息）

    async def _on_ready(self, wid: int) -> None:
        slot = self._states[wid]
        if slot.state == "ready" or slot.state == "busy" or slot.state == "stopped":
            return
        slot.state = "ready"
        async with self._cond:
            self._idle.append(wid)
            self._cond.notify_all()

    def _resolve_token(self, wid: int, param: str) -> None:
        slot = self._states[wid]
        fut = slot.request
        if fut is None or fut.done():
            self.stats["discarded_lines"] += 1  # 迟到/多余响应，丢弃
            return
        slot.request = None
        fut.set_result(param)

    def _resolve_fail(self, wid: int, reason: str) -> None:
        slot = self._states[wid]
        fut = slot.request
        if fut is None or fut.done():
            self.stats["discarded_lines"] += 1
            return
        slot.request = None
        self.stats["solver_failures"] += 1
        fut.set_exception(SolverFailureError(f"worker {wid} 求解失败: {_redact(reason)}"))

    def _fail_pending(self, wid: int, exc: Exception) -> None:
        """worker 死亡时快速失败正在等待的请求（未放弃的）。"""
        slot = self._states[wid]
        fut = slot.request
        if fut is None or fut.done():
            return
        slot.request = None
        fut.set_exception(exc)

    async def _mark_broken(self, wid: int, reason: str) -> None:
        slot = self._states[wid]
        if slot.state in ("broken", "stopped"):
            return
        slot.state = "broken"
        self.stats["worker_restarts"] += 1
        logs.warn(_LOG, f"worker {wid} 异常: {_redact(reason)}")
        await self._kill(slot)
        async with self._cond:
            self._cond.notify_all()

# ═════════════════════════════════════════════════════════════════════════════
# 真实 worker 入口：python -m app.captcha_browser worker --scene … --region … --prefix …
# ═════════════════════════════════════════════════════════════════════════════

def _build_worker_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="python -m app.captcha_browser worker",
        description="cloakbrowser 验证码求解 worker：与池通过 READY/GET_TOKEN/TOKEN/FAILED/SHUTDOWN 行协议通信",
    )
    parser.add_argument("--scene", required=True, help="阿里云验证码 SceneId")
    parser.add_argument("--region", required=True, help="验证码 region，如 sgp")
    parser.add_argument("--prefix", required=True, help="验证码 prefix，如 no8xfe")
    parser.add_argument("--sdk", default=None, help="本地 AliyunCaptcha SDK 文件路径（默认 captcha_node/AliyunCaptcha.js.txt）")
    parser.add_argument("--sdk-load-timeout", type=float, default=20.0, help="SDK 加载超时（秒）")
    parser.add_argument("--solve-timeout", type=float, default=40.0, help="单次求解超时（秒）")
    return parser


def _default_sdk_path() -> Path:
    return Path(__file__).resolve().parents[1] / "captcha_node" / "AliyunCaptcha.js.txt"


def _read_sdk(path: str | None) -> str:
    sdk_path = Path(path) if path else _default_sdk_path()
    try:
        sdk = sdk_path.read_text(encoding="utf-8")
    except OSError as exc:
        raise RuntimeError(f"SDK 文件不可用: {sdk_path}") from exc
    return sdk.replace("</script>", "<\\/script>")


def _build_captcha_html(sdk: str, config_json: str) -> str:
    """构造加载阿里云 SDK 的最小页面；AliyunCaptchaConfig 先于 SDK 执行。"""
    return (
        "<!DOCTYPE html><html><head><meta charset=\"utf-8\">"
        "<style>html,body{margin:0;padding:0}button{display:none}</style>"
        "</head><body><div id=\"cap\"></div><button id=\"btn\"></button>"
        f"<script>window.AliyunCaptchaConfig = {config_json};</script>"
        f"<script>{sdk}</script></body></html>"
    )


def _load_page(browser, args):
    """加载一次 SDK 页面；后续 token 请求复用该页面和浏览器会话。"""
    sdk = _read_sdk(args.sdk)
    config_json = json.dumps({"region": args.region, "prefix": args.prefix})
    page = browser.new_page()
    try:
        try:
            page.set_content(
                _build_captcha_html(sdk, config_json),
                wait_until="domcontentloaded",
                # 内联 SDK 会让 DOMContentLoaded 等待超时，但页面已经可用；
                # 用短超时快速进入下面的 initAliyunCaptcha 就绪检查。
                timeout=3_000,
            )
        except Exception as exc:  # noqa: BLE001
            if type(exc).__name__ != "TimeoutError":
                raise
        page.wait_for_function(
            "typeof window.initAliyunCaptcha === 'function'",
            timeout=args.sdk_load_timeout * 1000,
        )
        return page
    except Exception:
        try:
            page.close()
        except Exception:
            pass
        raise


def _solve_once(page, args) -> str:
    """在已加载的真实 Chromium 页面中生成一次 verify_param。"""
    page.evaluate(
        "() => {\n"
        "  window.__zcodeOutcome = null;\n"
        "  window.__zcodeError = null;\n"
        "  window.initAliyunCaptcha({\n"
        f"    SceneId: {json.dumps(args.scene)},\n"
        "    mode: 'popup',\n"
        f"    region: {json.dumps(args.region)},\n"
        f"    prefix: {json.dumps(args.prefix)},\n"
        "    language: 'en',\n"
        "    element: '#cap',\n"
        "    button: '#btn',\n"
        "    captchaLogoImg: '',\n"
        "    showErrorTip: false,\n"
        "    getInstance(instance) {\n"
        "      const start = instance && (instance.startTracelessVerification || instance.show);\n"
        "      if (typeof start !== 'function') { window.__zcodeError = 'SDK instance unavailable'; return; }\n"
        "      try { start.call(instance); } catch (e) { window.__zcodeError = String(e && e.message || e); }\n"
        "    },\n"
        "    success(param) { window.__zcodeOutcome = param; },\n"
        "    fail(e) { window.__zcodeError = 'SDK fail: ' + String(e && e.message || e); },\n"
        "    onError(e) { window.__zcodeError = 'SDK error: ' + String(e && e.message || e); }\n"
        "  });\n"
        "}"
    )
    deadline = time.monotonic() + args.solve_timeout
    while time.monotonic() < deadline:
        state = page.evaluate(
            "() => ({ outcome: window.__zcodeOutcome, error: window.__zcodeError })"
        )
        if state.get("error"):
            raise RuntimeError(f"captcha {_redact(str(state.get('error')))}")
        outcome = state.get("outcome")
        if isinstance(outcome, str) and outcome.strip():
            return outcome.strip()
        time.sleep(0.25)
    raise RuntimeError(f"captcha solve timeout after {args.solve_timeout:g}s")


def worker_main(argv: list[str]) -> int:
    args = _build_worker_parser().parse_args(argv)
    try:
        from cloakbrowser import launch
    except Exception as exc:  # noqa: BLE001
        print(f"{FAILED} cloakbrowser 不可用: {_redact(str(exc))}", flush=True)
        return 2
    browser = None
    try:
        browser = launch(headless=True, args=list(_BROWSER_ARGS))
    except Exception as exc:  # noqa: BLE001
        print(f"{FAILED} 浏览器启动失败: {_redact(str(exc))}", flush=True)
        return 1
    page = None
    try:
        try:
            page = _load_page(browser, args)
        except Exception as exc:  # noqa: BLE001
            print(f"{FAILED} 页面初始化失败: {_redact(str(exc))}", flush=True)
            return 1
        print(READY, flush=True)
        for raw in sys.stdin:
            line = raw.strip()
            if line == SHUTDOWN:
                break
            if line != GET_TOKEN:
                continue
            try:
                param = _solve_once(page, args)
                print(f"{TOKEN} {param}", flush=True)
            except Exception as exc:  # noqa: BLE001
                print(f"{FAILED} {_redact(str(exc))}", flush=True)
                # 每次求解失败都重载页面，清掉 SDK 内部状态；成功路径
                # 则继续复用同一页面，避免每次重新加载 224KB SDK。
                try:
                    page.close()
                except Exception:
                    pass
                page = None
                try:
                    page = _load_page(browser, args)
                except Exception as reload_exc:  # noqa: BLE001
                    print(f"{FAILED} 页面重载失败: {_redact(str(reload_exc))}", flush=True)
    finally:
        if page is not None:
            try:
                page.close()
            except Exception:
                pass
        try:
            browser.close()
        except Exception:
            pass
    return 0


def main(argv: list[str] | None = None) -> int:
    argv = list(sys.argv[1:] if argv is None else argv)
    if argv and argv[0] == "worker":
        return worker_main(argv[1:])
    _build_worker_parser().print_help()
    return 2


if __name__ == "__main__":
    sys.exit(main())
