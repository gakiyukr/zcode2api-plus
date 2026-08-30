"""UsageCollector 與賬號 token 調度統計測試。"""

from __future__ import annotations

import unittest

from app.models import Account
from app.usage import UsageCollector


def _sse(*payloads: str) -> bytes:
    return "".join(f"data: {p}\n\n" for p in payloads).encode()


class UsageCollectorSseTests(unittest.TestCase):
    def test_message_start_and_delta(self):
        collector = UsageCollector(is_sse=True)
        payload = _sse(
            '{"type":"message_start","message":{"usage":{"input_tokens":120,"output_tokens":1,'
            '"cache_read_input_tokens":30,"cache_creation_input_tokens":5}}}',
            '{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}',
            '{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":456}}',
        )
        # 模擬任意分塊到達：跨塊切開的行也不能丟失事件
        collector.feed(payload[:40])
        collector.feed(payload[40:120])
        collector.feed(payload[120:])
        collector.finish()
        self.assertEqual(collector.input_tokens, 120)
        self.assertEqual(collector.output_tokens, 456)
        self.assertEqual(collector.cache_read_tokens, 30)
        self.assertEqual(collector.cache_creation_tokens, 5)

    def test_delta_output_is_cumulative_not_additive(self):
        collector = UsageCollector(is_sse=True)
        collector.feed(_sse(
            '{"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1}}}',
            '{"type":"message_delta","usage":{"output_tokens":50}}',
            '{"type":"message_delta","usage":{"output_tokens":80}}',
        ))
        self.assertEqual(collector.output_tokens, 80)

    def test_ignores_noise_and_done(self):
        collector = UsageCollector(is_sse=True)
        collector.feed(b"event: ping\n\n: keepalive\n\ndata: [DONE]\n\ndata: not-json\n\n")
        self.assertEqual(
            collector.as_dict(),
            {"input": 0, "output": 0, "cache_creation": 0, "cache_read": 0},
        )


class UsageCollectorJsonTests(unittest.TestCase):
    def test_json_usage(self):
        collector = UsageCollector(is_sse=False)
        collector.feed(b'{"id":"msg_1","usage":{"input_tokens":7,"output_tokens":9}}')
        collector.finish()
        self.assertEqual(collector.input_tokens, 7)
        self.assertEqual(collector.output_tokens, 9)

    def test_json_cache_fields(self):
        collector = UsageCollector(is_sse=False)
        collector.feed(b'{"usage":{"input_tokens":7,"output_tokens":9,'
                      b'"cache_read_input_tokens":11,"cache_creation_input_tokens":13}}')
        collector.finish()
        self.assertEqual(collector.cache_read_tokens, 11)
        self.assertEqual(collector.cache_creation_tokens, 13)

    def test_json_invalid_body_is_zero(self):
        collector = UsageCollector(is_sse=False)
        collector.feed(b"<html>gateway error</html>")
        collector.finish()
        self.assertEqual(collector.output_tokens, 0)


class AccountTokenStatsTests(unittest.TestCase):
    def test_accumulate_and_reset(self):
        account = Account.create("zai", "stats", "a.b.c")
        account.accumulate_tokens({"input": 10, "output": 3, "cache_read": 2, "cache_creation": 1})
        account.accumulate_tokens({"input": 5, "output": 0})
        view = account.public_view()["total_tokens"]
        self.assertEqual(view["input"], 15)
        self.assertEqual(view["output"], 3)
        self.assertEqual(view["cache_read"], 2)
        self.assertEqual(view["cache_creation"], 1)
        account.reset_token_stats()
        self.assertEqual(account.public_view()["total_tokens"]["input"], 0)

    def test_roundtrip_persists_counters(self):
        account = Account.create("zai", "persist", "a.b.c")
        account.accumulate_tokens({"input": 42, "output": 7})
        restored = Account.from_dict(account.to_dict())
        self.assertEqual(restored.total_input_tokens, 42)
        self.assertEqual(restored.total_output_tokens, 7)

    def test_legacy_dict_without_counters_defaults_zero(self):
        # 舊版落庫資料不含新欄位，載入後應以 0 為預設值
        account = Account.create("zai", "legacy", "a.b.c")
        data = account.to_dict()
        for key in ("total_input_tokens", "total_output_tokens",
                    "total_cache_creation_tokens", "total_cache_read_tokens"):
            data.pop(key)
        restored = Account.from_dict(data)
        self.assertEqual(restored.total_input_tokens, 0)
        self.assertEqual(restored.total_output_tokens, 0)


if __name__ == "__main__":
    unittest.main()
