#!/usr/bin/env python3
"""测试用假 worker：只走行协议，不启动浏览器，不产生真实 token。

行为完全由环境变量控制（经池的 worker_env 注入）：

    FAKE_DELAY_READY        float  打印 READY 前先 sleep 的秒数（模拟慢启动）。
    FAKE_EXIT_IMMEDIATELY   1      打印 READY 前立即以 FAKE_EXIT_CODE 退出。
    FAKE_EXIT_AFTER_READY   float  打印 READY 后 sleep 这么多秒再以 FAKE_EXIT_CODE 退出。
    FAKE_EXIT_ON_GET        1      收到第一条 GET_TOKEN 时以 FAKE_EXIT_CODE 退出。
    FAKE_EXIT_CODE          int    上述退出路径的退出码（默认 0）。
    FAKE_SPAM_TOKEN         1      打印 READY 后先发一条未经请求的 TOKEN 噪音行。
    FAKE_STDERR_NOISE       int    打印 READY 后向 stderr 写这么多字节噪音（默认 0）。
    FAKE_SOLVE_DELAY        float  每条 GET_TOKEN 的求解耗时（秒）。
    FAKE_FAIL_SOLVE         1      用 FAILED 应答，而不是 TOKEN。
    FAKE_IGNORE_FIRST_GET   1      吞掉第一条 GET_TOKEN（睡 0.3s 继续），模拟慢响应。
    FAKE_TOKEN_PREFIX       str    token 前缀（默认 fake-token，永远不要用真实格式）。

FAKE_EXIT_ON_GET / FAKE_IGNORE_FIRST_GET 是一次性行为：把一次性动作记录在
FAKE_MARKER_FILE 指向的文件里，替换后的新进程看到标记就转为正常求解，
从而能在"第一次失败、替换后成功"的测试里确定性地模拟。
"""

import os
import sys
import time
from pathlib import Path


def _env_int(name: str, default: int) -> int:
    try:
        return int(os.environ.get(name, str(default)))
    except ValueError:
        return default


def _env_float(name: str, default: float) -> float:
    try:
        return float(os.environ.get(name, str(default)))
    except ValueError:
        return default


def _take_once(marker: str) -> bool:
    """返回 True 表示本次是"一次性动作"（标记文件不存在），并创建标记。"""
    if not marker:
        return True
    path = Path(marker)
    if path.exists():
        return False
    try:
        path.write_text(str(os.getpid()))
    except OSError:
        return True
    return True


def main() -> int:
    delay_ready = _env_float("FAKE_DELAY_READY", 0.0)
    exit_immediately = os.environ.get("FAKE_EXIT_IMMEDIATELY") == "1"
    exit_after_ready = _env_float("FAKE_EXIT_AFTER_READY", -1.0)
    exit_on_get = os.environ.get("FAKE_EXIT_ON_GET") == "1"
    exit_code = _env_int("FAKE_EXIT_CODE", 0)
    spam_token = os.environ.get("FAKE_SPAM_TOKEN") == "1"
    stderr_noise = _env_int("FAKE_STDERR_NOISE", 0)
    solve_delay = _env_float("FAKE_SOLVE_DELAY", 0.0)
    fail_solve = os.environ.get("FAKE_FAIL_SOLVE") == "1"
    ignore_first_get = os.environ.get("FAKE_IGNORE_FIRST_GET") == "1"
    prefix = os.environ.get("FAKE_TOKEN_PREFIX", "fake-token")
    marker = os.environ.get("FAKE_MARKER_FILE", "")

    if exit_immediately:
        return exit_code
    if delay_ready:
        time.sleep(delay_ready)
    if spam_token:
        # 先于任何 GET_TOKEN 的噪音：池必须丢弃，不能投递给后续请求。
        print("TOKEN fake-noise-unsolicited", flush=True)
    print("READY", flush=True)
    if exit_after_ready >= 0 and _take_once(marker):
        if exit_after_ready:
            time.sleep(exit_after_ready)
        return exit_code
    if stderr_noise:
        line = b"stderr-noise-line-" + b"x" * 4000 + b"\n"
        sys.stderr.buffer.write(line * (stderr_noise // len(line)))
        sys.stderr.flush()

    seq = 0
    for raw in sys.stdin:
        line = raw.strip()
        if line == "SHUTDOWN":
            break
        if line != "GET_TOKEN":
            continue
        if exit_on_get and _take_once(marker):
            return exit_code
        if ignore_first_get and _take_once(marker):
            time.sleep(0.3)
            continue
        if solve_delay:
            time.sleep(solve_delay)
        if fail_solve:
            print("FAILED fake solver error (solver-level, worker healthy)", flush=True)
            continue
        seq += 1
        # 明确伪造的 token：永远不是真实格式，杜绝任何"真实 token"外泄。
        print(f"TOKEN {prefix}-{os.getpid()}-{seq}", flush=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
