"""app/captcha_browser 池模块的独立单元测试。

全部测试使用 tests/fake_browser_worker.py 假 worker（行协议兼容、不启动浏览器），
不会产生真实 token：任何"token"都是 fake-token-<pid>-<seq> 的明确伪造格式。
"""

from __future__ import annotations

import asyncio
import importlib.util
import os
import re
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path

from app.captcha_browser import (
    BrowserWorkerPool,
    CaptchaBrowserError,
    SolverFailureError,
    SolverTimeoutError,
    StartupError,
    WorkerExitError,
    _build_captcha_html,
    _redact,
)

FAKE_WORKER = Path(__file__).resolve().parent / "fake_browser_worker.py"
FAKE_TOKEN_RE = re.compile(r"^fake-token-\d+-\d+$")
PROJECT_ROOT = Path(__file__).resolve().parents[1]

_HAS_CLOAKBROWSER = importlib.util.find_spec("cloakbrowser") is not None


def _pool(size: int = 1, worker_env: dict | None = None, **kwargs) -> BrowserWorkerPool:
    env = {"FAKE_TOKEN_PREFIX": "fake-token", **(worker_env or {})}
    defaults = {
        "startup_timeout": 5.0,
        "request_timeout": 5.0,
        "restart_delay": 0.05,
        "shutdown_timeout": 5.0,
    }
    defaults.update(kwargs)
    return BrowserWorkerPool(
        size=size,
        worker_cmd=[sys.executable, str(FAKE_WORKER)],
        worker_env=env,
        **defaults,
    )


def _marker_path() -> str:
    """一次性标记文件：只保留路径（不创建），由 worker 首次命中时创建。"""
    fd, path = tempfile.mkstemp(prefix="fake-worker-marker-")
    os.close(fd)
    os.unlink(path)
    return path


async def _wait_until(cond, timeout: float = 2.0) -> bool:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if cond():
            return True
        await asyncio.sleep(0.02)
    return False


class PoolStartupTests(unittest.IsolatedAsyncioTestCase):
    async def test_start_spawns_exact_pool_size_and_roundtrip(self):
        pool = _pool(size=2)
        async with pool:
            self.assertTrue(pool.is_started)
            self.assertEqual(pool.stats["started"], 2)
            self.assertEqual(len(pool._states), 2)
            token = await pool.get_token()
            self.assertRegex(token, FAKE_TOKEN_RE)
            self.assertEqual(pool.stats["requests"], 1)

    async def test_startup_total_timeout_tears_down_pool(self):
        pool = _pool(size=2, startup_timeout=0.3, worker_env={"FAKE_DELAY_READY": "0.8"})
        with self.assertRaises(StartupError):
            await pool.start()
        self.assertFalse(pool.is_started)
        self.assertEqual(pool._loops, [])
        self.assertTrue(all(s.proc is None for s in pool._states))
        with self.assertRaises(CaptchaBrowserError):
            await pool.get_token()

    async def test_worker_exit_before_ready_fails_startup(self):
        pool = _pool(size=1, worker_env={"FAKE_EXIT_IMMEDIATELY": "1", "FAKE_EXIT_CODE": "3"})
        with self.assertRaises(StartupError):
            await pool.start()
        self.assertFalse(pool.is_started)

    async def test_bad_worker_command_fails_startup(self):
        pool = BrowserWorkerPool(size=1, worker_cmd=["/nonexistent/binary-xyz"])
        with self.assertRaises(StartupError):
            await pool.start()
        self.assertFalse(pool.is_started)

    async def test_worker_exit_after_ready_is_replaced(self):
        marker = _marker_path()
        self.addCleanup(os.unlink, marker)
        pool = _pool(
            size=1,
            worker_env={"FAKE_EXIT_AFTER_READY": "0.3", "FAKE_MARKER_FILE": marker},
        )
        await pool.start()
        self.assertTrue(pool.is_started)
        replaced = await _wait_until(
            lambda: pool.stats["worker_restarts"] >= 1
            and all(s.state == "ready" for s in pool._states)
        )
        self.assertTrue(replaced, "worker 应在退出后被替换并重新 READY")
        token = await pool.get_token()
        self.assertRegex(token, FAKE_TOKEN_RE)
        await pool.stop()


class PoolRequestTests(unittest.IsolatedAsyncioTestCase):
    async def test_exit_on_get_fails_request_and_worker_replaced(self):
        marker = _marker_path()
        self.addCleanup(os.unlink, marker)
        pool = _pool(
            size=1,
            worker_env={
                "FAKE_EXIT_ON_GET": "1",
                "FAKE_EXIT_CODE": "7",
                "FAKE_MARKER_FILE": marker,
            },
        )
        await pool.start()
        with self.assertRaises(WorkerExitError) as ctx:
            await pool.get_token()
        self.assertEqual(ctx.exception.code, 7)
        self.assertEqual(pool.stats["worker_restarts"], 1)
        ready = await _wait_until(lambda: all(s.state == "ready" for s in pool._states))
        self.assertTrue(ready)
        token = await pool.get_token()
        self.assertRegex(token, FAKE_TOKEN_RE)
        await pool.stop()

    async def test_solver_failure_keeps_worker_healthy(self):
        pool = _pool(size=1, worker_env={"FAKE_FAIL_SOLVE": "1"})
        await pool.start()
        with self.assertRaises(SolverFailureError):
            await pool.get_token()
        with self.assertRaises(SolverFailureError):
            await pool.get_token()
        self.assertEqual(pool.stats["solver_failures"], 2)
        self.assertEqual(pool.stats["worker_restarts"], 0)
        await pool.stop()

    async def test_large_stderr_does_not_block_protocol(self):
        pool = _pool(size=1, worker_env={"FAKE_STDERR_NOISE": "2000000"})
        await pool.start()
        token = await pool.get_token()  # stderr 排空不及时会阻塞 worker，导致超时
        self.assertRegex(token, FAKE_TOKEN_RE)
        self.assertEqual(pool.stats["worker_restarts"], 0)
        await pool.stop()

    async def test_request_timeout_replaces_worker(self):
        marker = _marker_path()
        self.addCleanup(os.unlink, marker)
        pool = _pool(
            size=1,
            request_timeout=0.25,
            worker_env={"FAKE_IGNORE_FIRST_GET": "1", "FAKE_MARKER_FILE": marker},
        )
        await pool.start()
        with self.assertRaises(SolverTimeoutError):
            await pool.get_token()
        self.assertEqual(pool.stats["timeouts"], 1)
        self.assertEqual(pool.stats["worker_restarts"], 1)
        ready = await _wait_until(lambda: all(s.state == "ready" for s in pool._states))
        self.assertTrue(ready)
        token = await pool.get_token()
        self.assertRegex(token, FAKE_TOKEN_RE)
        await pool.stop()

    async def test_unsolicited_token_lines_are_discarded(self):
        pool = _pool(size=1, worker_env={"FAKE_SPAM_TOKEN": "1"})
        await pool.start()
        token = await pool.get_token()
        self.assertRegex(token, FAKE_TOKEN_RE)
        self.assertNotEqual(token, "fake-noise-unsolicited")
        self.assertGreaterEqual(pool.stats["discarded_lines"], 1)
        await pool.stop()

    async def test_bounded_concurrency_serializes_requests(self):
        pool = _pool(size=1, worker_env={"FAKE_SOLVE_DELAY": "0.2"})
        await pool.start()
        started = time.monotonic()
        tokens = await asyncio.gather(*(pool.get_token() for _ in range(3)))
        elapsed = time.monotonic() - started
        # size=1：3 个请求必须串行，至少 3×0.2s；并行则约 0.2s
        self.assertGreaterEqual(elapsed, 0.55)
        self.assertEqual(len(tokens), 3)
        for token in tokens:
            self.assertRegex(token, FAKE_TOKEN_RE)
        self.assertEqual(pool.stats["requests"], 3)
        await pool.stop()

    async def test_get_token_before_start_raises(self):
        pool = _pool()
        with self.assertRaises(CaptchaBrowserError):
            await pool.get_token()

    async def test_get_token_after_stop_raises(self):
        pool = _pool()
        await pool.start()
        await pool.stop()
        with self.assertRaises(CaptchaBrowserError):
            await pool.get_token()


class PoolShutdownTests(unittest.IsolatedAsyncioTestCase):
    async def test_stop_waits_for_inflight_request(self):
        pool = _pool(size=1, worker_env={"FAKE_SOLVE_DELAY": "1.0"})
        await pool.start()
        task = asyncio.create_task(pool.get_token())
        await asyncio.sleep(0.2)
        await pool.stop()  # 优雅关闭应等求解完成
        token = await task
        self.assertRegex(token, FAKE_TOKEN_RE)
        self.assertFalse(pool.is_started)

    async def test_stop_kills_stragglers_and_fails_inflight(self):
        pool = _pool(
            size=1,
            shutdown_timeout=0.3,
            worker_env={"FAKE_SOLVE_DELAY": "2.0"},
        )
        await pool.start()
        task = asyncio.create_task(pool.get_token())
        await asyncio.sleep(0.2)
        started = time.monotonic()
        await pool.stop()  # 求解时间远超 shutdown_timeout → 强杀
        self.assertLess(time.monotonic() - started, 1.5)
        with self.assertRaises(CaptchaBrowserError):
            await task

    async def test_stop_is_idempotent_and_context_manager_works(self):
        pool = _pool(size=2)
        async with pool:
            token = await pool.get_token()
            self.assertRegex(token, FAKE_TOKEN_RE)
        self.assertFalse(pool.is_started)
        await pool.stop()  # 再次 stop 应为无害空操作

    async def test_restart_after_stop(self):
        pool = _pool(size=1)
        await pool.start()
        self.assertRegex(await pool.get_token(), FAKE_TOKEN_RE)
        await pool.stop()
        await pool.start()
        self.assertRegex(await pool.get_token(), FAKE_TOKEN_RE)
        await pool.stop()


class WorkerProtocolTests(unittest.IsolatedAsyncioTestCase):
    """直接验证假 worker 与池之间的协议契约（READY/SHUTDOWN/GET_TOKEN）。"""

    async def test_fake_worker_ready_and_graceful_shutdown(self):
        proc = await asyncio.create_subprocess_exec(
            sys.executable,
            str(FAKE_WORKER),
            stdin=asyncio.subprocess.PIPE,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
        )
        try:
            line = await asyncio.wait_for(proc.stdout.readline(), timeout=5.0)
            self.assertEqual(line.decode().strip(), "READY")
            proc.stdin.write(b"SHUTDOWN\n")
            await proc.stdin.drain()
            rc = await asyncio.wait_for(proc.wait(), timeout=5.0)
            self.assertEqual(rc, 0)
        finally:
            if proc.returncode is None:
                proc.kill()
                await proc.wait()


class WorkerEntryTests(unittest.TestCase):
    def test_redact_hides_long_secrets_and_keeps_short_text(self):
        secret = "A" * 80
        self.assertEqual(_redact(secret), "[redacted]")
        self.assertNotIn(secret, _redact(f"msg {secret} tail"))
        self.assertEqual(_redact("a b\n\tc"), "a b c")
        self.assertEqual(_redact("普通短文本"), "普通短文本")

    def test_build_captcha_html_config_before_sdk(self):
        sdk = "window.SDK_BODY = 1;"
        html = _build_captcha_html(sdk, '{"region": "sgp", "prefix": "no8xfe"}')
        self.assertIn("window.AliyunCaptchaConfig =", html)
        config_at = html.index("AliyunCaptchaConfig")
        sdk_at = html.index(sdk)
        self.assertLess(config_at, sdk_at, "AliyunCaptchaConfig 必须先于 SDK 执行")

    def test_pool_default_worker_command(self):
        pool = BrowserWorkerPool.from_cloakbrowser("scene", "sgp", "no8xfe")
        self.assertEqual(
            pool._worker_cmd,
            [
                sys.executable,
                "-m",
                "app.captcha_browser",
                "worker",
                "--scene",
                "scene",
                "--region",
                "sgp",
                "--prefix",
                "no8xfe",
            ],
        )

    def test_worker_cli_help_exits_zero_without_cloakbrowser(self):
        result = subprocess.run(
            [sys.executable, "-m", "app.captcha_browser", "worker", "--help"],
            cwd=PROJECT_ROOT,
            capture_output=True,
            text=True,
            timeout=30,
        )
        self.assertEqual(result.returncode, 0)
        self.assertIn("usage:", result.stdout)

    def test_module_main_without_subcommand_exits_two(self):
        result = subprocess.run(
            [sys.executable, "-m", "app.captcha_browser"],
            cwd=PROJECT_ROOT,
            capture_output=True,
            text=True,
            timeout=30,
        )
        self.assertEqual(result.returncode, 2)

    @unittest.skipIf(_HAS_CLOAKBROWSER, "本环境已安装 cloakbrowser，无法复现缺失场景")
    def test_worker_main_without_cloakbrowser_fails_cleanly(self):
        result = subprocess.run(
            [
                sys.executable,
                "-m",
                "app.captcha_browser",
                "worker",
                "--scene",
                "scene",
                "--region",
                "sgp",
                "--prefix",
                "no8xfe",
            ],
            cwd=PROJECT_ROOT,
            capture_output=True,
            text=True,
            timeout=30,
        )
        self.assertEqual(result.returncode, 2)
        self.assertTrue(result.stdout.startswith("FAILED "))


if __name__ == "__main__":
    unittest.main()
