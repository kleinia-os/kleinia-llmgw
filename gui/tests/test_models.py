#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""数据模型解析测试：Source/Key/UsageRow/Summary/KeyWindows。"""

import os
import sys
import unittest

sys.path.insert(0, os.path.join(
    os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "src"))

from backend import (parse_source, parse_key, parse_usage_row,  # noqa: E402
                     parse_summary, parse_key_windows, SourceHealth)

FULL_SOURCE = {
    "id": 7, "name": "primary", "type": "openai",
    "base_url": "https://upstream.example.test/v1", "has_key": True,
    "priority": 10, "weight": 100, "enabled": True,
    "models": [{"alias": "model-a", "upstream": "model-a"}],
    "health": {"state": "cooling", "fails": 3,
               "cooling_until": "2026-10-28T23:00:00Z"},
    "unknown_field": "ignored",
}


class ParseSourceTest(unittest.TestCase):
    def test_full(self):
        src = parse_source(FULL_SOURCE)
        self.assertEqual(src.id, 7)
        self.assertEqual(src.name, "primary")
        self.assertEqual(src.type, "openai")
        self.assertTrue(src.has_key)
        self.assertEqual(src.priority, 10)
        self.assertEqual(src.weight, 100)
        self.assertTrue(src.enabled)
        self.assertEqual(src.models[0].alias, "model-a")
        self.assertEqual(src.health.state, "cooling")
        self.assertEqual(src.health.fails, 3)
        self.assertEqual(src.health.cooling_until, "2026-10-28T23:00:00Z")

    def test_minimal_defaults(self):
        src = parse_source({"id": "2", "name": "x"})
        self.assertEqual(src.id, 2)                 # 数字样字符串 → int
        self.assertEqual(src.type, "openai")
        self.assertFalse(src.has_key)
        self.assertTrue(src.enabled)                # 缺省启用
        self.assertEqual(src.priority, 10)
        self.assertEqual(src.weight, 100)
        self.assertEqual(src.models, [])
        self.assertEqual(src.health, SourceHealth(state="unknown"))

    def test_string_id_passthrough(self):
        src = parse_source({"id": "abc", "name": "x"})
        self.assertEqual(src.id, "abc")

    def test_coercions(self):
        src = parse_source({"id": 1, "name": "x", "priority": "42",
                            "weight": 7.0, "enabled": "false",
                            "has_key": 1})
        self.assertEqual(src.priority, 42)
        self.assertEqual(src.weight, 7)
        self.assertFalse(src.enabled)
        self.assertTrue(src.has_key)

    def test_bad_health_state_normalized(self):
        src = parse_source({"id": 1, "name": "x",
                            "health": {"state": "exploded"}})
        self.assertEqual(src.health.state, "exploded")   # 保留原值，前端兜底


class ParseKeyTest(unittest.TestCase):
    def test_active(self):
        key = parse_key({"id": 1, "name": "opencode", "enabled": True,
                         "suspended": False, "models": ["*"],
                         "rpm": 60, "tpm": 2000000, "daily_tokens": 5000000,
                         "aid": "opencode", "created_at": "2026-10-24",
                         "revoked_at": ""})
        self.assertEqual(key.status, "active")
        self.assertEqual(key.models, ["*"])
        self.assertFalse(key.revoked_at)

    def test_suspended_over_enabled(self):
        key = parse_key({"id": 2, "name": "x", "enabled": True,
                         "suspended": True})
        self.assertEqual(key.status, "suspended")

    def test_disabled_is_suspended(self):
        key = parse_key({"id": 3, "name": "x", "enabled": False,
                         "suspended": False})
        self.assertEqual(key.status, "suspended")

    def test_revoked_wins(self):
        key = parse_key({"id": 4, "name": "x", "enabled": False,
                         "suspended": True, "revoked_at": "2026-10-26"})
        self.assertEqual(key.status, "revoked")

    def test_defaults(self):
        key = parse_key({"id": "9", "name": "y"})
        self.assertEqual(key.id, 9)
        self.assertEqual(key.models, [])
        self.assertEqual(key.status, "active")
        self.assertEqual(key.rpm, 0)


class ParseUsageTest(unittest.TestCase):
    def test_row(self):
        row = parse_usage_row({"ts": 1790000000.5, "ident": "opencode",
                               "model": "model-a", "source": "primary",
                               "agent": "opencode-main", "in_tokens": 100,
                               "out_tokens": 40, "latency_ms": 250,
                               "status": "ok"})
        self.assertEqual(row.tokens, 140)
        self.assertEqual(row.status, "ok")

    def test_row_defaults(self):
        row = parse_usage_row({"ts": "bad", "ident": "x",
                               "in_tokens": None})
        self.assertGreater(row.ts, 0)          # 坏 ts → 当前时间
        self.assertEqual(row.in_tokens, 0)
        self.assertEqual(row.status, "ok")

    def test_summary(self):
        summary = parse_summary({
            "requests": 12, "tokens_in": 100, "tokens_out": 50,
            "active_keys": 2, "sources_health": {"primary": "ok"}},
            since_hours=7)
        self.assertEqual(summary.since_hours, 7)
        self.assertEqual(summary.tokens, 150)
        self.assertEqual(summary.sources_health, {"primary": "ok"})

    def test_summary_aliases_and_defaults(self):
        summary = parse_summary({"in_tokens": 10, "out_tokens": 5})
        self.assertEqual(summary.tokens, 15)
        self.assertEqual(summary.requests, 0)
        self.assertEqual(summary.sources_health, {})


class ParseKeyWindowsTest(unittest.TestCase):
    def test_list(self):
        windows = parse_key_windows([
            {"window": "rpm", "used": 3, "limit": 60},
            {"window": "tpm", "used": 0, "limit": 0},
        ])
        self.assertEqual(len(windows), 2)
        self.assertAlmostEqual(windows[0].pct, 5.0)
        self.assertEqual(windows[1].pct, 0.0)      # limit=0 → 0

    def test_dict_wrapper_and_junk(self):
        windows = parse_key_windows({
            "windows": [{"name": "daily_tokens", "used": 10, "limit": 5},
                        "junk-not-dict"]})
        self.assertEqual(len(windows), 1)
        self.assertEqual(windows[0].window, "daily_tokens")
        self.assertEqual(windows[0].pct, 200.0)

    def test_empty(self):
        self.assertEqual(parse_key_windows(None), [])
        self.assertEqual(parse_key_windows({}), [])


if __name__ == "__main__":
    unittest.main()
