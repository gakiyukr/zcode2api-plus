"""Token 調度統計：從上游 Anthropic Messages 回應中提取 usage。

上游回應有兩種形態：
- SSE（text/event-stream）：`message_start` 攜帶輸入側（input / cache），
  `message_delta` 的 `usage.output_tokens` 為累計值（取最大，不可累加）。
- JSON：頂層 `usage` 一次帶齊全部欄位。

收集器以「塊」為單位餵入，內部緩衝不完整行，因此對上游分塊邊界不敏感；
僅在回應完整結束時計入賬號統計，串流中斷時 usage 不完整、不計入。
"""

from __future__ import annotations

import json


class UsageCollector:
    """收集單次成功回應的 token 用量。"""

    def __init__(self, is_sse: bool) -> None:
        self._is_sse = is_sse
        self._buf = b""
        self.input_tokens = 0
        self.output_tokens = 0
        self.cache_creation_tokens = 0
        self.cache_read_tokens = 0

    def feed(self, chunk: bytes) -> None:
        """餵入一段回應位元組。SSE 逐行解析；JSON 緩衝到 finish() 一次解析。"""
        if self._is_sse:
            self._buf += chunk
            while b"\n" in self._buf:
                line, self._buf = self._buf.split(b"\n", 1)
                self.feed_line(line.decode("utf-8", "ignore"))
        else:
            self._buf += chunk

    def feed_line(self, line: str) -> None:
        """解析一行 SSE data；供串流分塊解析與 async 池逐行解析共用。"""
        line = line.strip()
        if not line.startswith("data:"):
            return
        payload_text = line[len("data:"):].strip()
        if not payload_text or payload_text == "[DONE]":
            return
        try:
            payload = json.loads(payload_text)
        except json.JSONDecodeError:
            return
        if not isinstance(payload, dict):
            return

        event = payload.get("type")
        if event == "message_start":
            usage = (payload.get("message") or {}).get("usage") or {}
            self.input_tokens = max(self.input_tokens, _to_int(usage.get("input_tokens")))
            self.output_tokens = max(self.output_tokens, _to_int(usage.get("output_tokens")))
            self.cache_creation_tokens = max(
                self.cache_creation_tokens, _to_int(usage.get("cache_creation_input_tokens"))
            )
            self.cache_read_tokens = max(
                self.cache_read_tokens, _to_int(usage.get("cache_read_input_tokens"))
            )
        elif event == "message_delta":
            # output_tokens 為累計值，取最大避免重複累加
            usage = payload.get("usage") or {}
            self.output_tokens = max(self.output_tokens, _to_int(usage.get("output_tokens")))

    def finish(self) -> None:
        """回應結束後收尾：非 SSE 模式在此解析緩衝的完整 JSON。"""
        if self._is_sse:
            self._buf = b""
            return
        payload = None
        try:
            payload = json.loads(self._buf.decode("utf-8", "ignore"))
        except (json.JSONDecodeError, ValueError):
            payload = None
        self._buf = b""
        if not isinstance(payload, dict):
            return
        usage = payload.get("usage") or {}
        self.input_tokens = max(self.input_tokens, _to_int(usage.get("input_tokens")))
        self.output_tokens = max(self.output_tokens, _to_int(usage.get("output_tokens")))
        self.cache_creation_tokens = max(
            self.cache_creation_tokens, _to_int(usage.get("cache_creation_input_tokens"))
        )
        self.cache_read_tokens = max(
            self.cache_read_tokens, _to_int(usage.get("cache_read_input_tokens"))
        )

    def as_dict(self) -> dict:
        """返回累計結果，鍵與 Account.accumulate_tokens 約定一致。"""
        return {
            "input": self.input_tokens,
            "output": self.output_tokens,
            "cache_creation": self.cache_creation_tokens,
            "cache_read": self.cache_read_tokens,
        }


def _to_int(value) -> int:
    try:
        return max(0, int(value or 0))
    except (TypeError, ValueError):
        return 0
