#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""MockBackend 纯逻辑测试（无 GTK、无 D-Bus、无定时器）。"""

import os
import sys
import unittest

sys.path.insert(0, os.path.join(
    os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "src"))

from backend import (MockBackend, BackendError, SOURCE_TYPES,  # noqa: E402
                     SOURCE_TYPE_LABELS)


class BackendMockTest(unittest.TestCase):
    def setUp(self):
        self.b = MockBackend(live=False)
        self.events = []       # 信号名序列
        self.payloads = {}     # 信号名 -> 最近一次参数

        def recorder(name):
            def rec(_obj, *args):
                self.events.append(name)
                self.payloads[name] = args
            return rec

        for name in ("connected", "disconnected", "sources-changed",
                     "keys-changed", "usage-changed", "prices-changed",
                     "source-health", "quota-warning"):
            self.b.connect(name, recorder(name))

    def count(self, name):
        return self.events.count(name)

    # ---- 种子数据 -------------------------------------------------------
    def test_seed_sources(self):
        sources = self.b.list_sources()
        self.assertEqual(len(sources), 4)   # primary/anthropic-official/hub-b
                                             # /openai-direct(openai-responses)
        names = [s.name for s in sources]
        self.assertIn("primary", names)
        primary = next(s for s in sources if s.name == "primary")
        self.assertEqual(primary.type, "openai")
        self.assertTrue(primary.has_key)
        self.assertEqual(primary.priority, 10)
        self.assertEqual([m.alias for m in primary.models],
                         ["model-a", "test-model",
                          "model-b"])
        self.assertEqual(primary.health.state, "ok")
        responses = next(s for s in sources if s.name == "openai-direct")
        self.assertEqual(responses.type, "openai-responses")
        self.assertEqual(SOURCE_TYPE_LABELS[responses.type],
                         "OpenAI Responses")
        # 三种内部类型都有规范显示名，且 label 不经译文表（专有名词）
        self.assertEqual(len(SOURCE_TYPES), 3)
        self.assertEqual(SOURCE_TYPE_LABELS["openai"],
                         "OpenAI Chat Completions")
        self.assertEqual(SOURCE_TYPE_LABELS["anthropic"], "Anthropic Messages")

    def test_seed_keys_status(self):
        keys = {k.name: k for k in self.b.list_keys()}
        self.assertEqual(keys["opencode"].status, "active")
        self.assertEqual(keys["sysagent"].status, "suspended")
        self.assertEqual(keys["ci-runner"].status, "revoked")

    # ---- 源 CRUD ---------------------------------------------------------
    def test_add_source_ok(self):
        src = self.b.add_source({
            "name": "extra", "type": "anthropic",
            "base_url": "https://x.example", "api_key": "sk",
            "models": [{"alias": "a", "upstream": "b"}],
            "priority": 5, "weight": 7, "enabled": False})
        # openai-responses 类型同样接受
        r2 = self.b.add_source({"name": "resp", "type": "openai-responses",
                                "base_url": "https://r.example",
                                "models": [{"alias": "m", "upstream": "m"}]})
        self.assertEqual(r2.type, "openai-responses")
        self.assertEqual(src.name, "extra")
        self.assertTrue(src.has_key)
        self.assertEqual(src.models[0].upstream, "b")
        self.assertFalse(src.enabled)
        self.assertEqual(len(self.b.list_sources()), 6)
        self.assertGreaterEqual(self.count("sources-changed"), 2)

    def test_add_source_validation(self):
        with self.assertRaises(BackendError):
            self.b.add_source({"name": "", "type": "openai"})
        with self.assertRaises(BackendError):
            self.b.add_source({"name": "bad-type", "type": "openai-response"})
        with self.assertRaises(BackendError):
            self.b.add_source({"name": "x", "type": "weird"})
        with self.assertRaises(BackendError):
            self.b.add_source({"name": "x", "type": "openai",
                               "base_url": "ftp://bad"})
        with self.assertRaises(BackendError):
            self.b.add_source({"name": "primary", "type": "openai"})

    def test_update_and_remove_source(self):
        src = self.b.update_source(1, {"base_url": "https://new/v1",
                                       "enabled": False})
        self.assertEqual(src.base_url, "https://new/v1")
        self.assertFalse(src.enabled)
        self.b.remove_source("primary")
        self.assertEqual(len(self.b.list_sources()), 3)
        with self.assertRaises(BackendError):
            self.b.remove_source(1)

    def test_set_priority_weight(self):
        src = self.b.set_priority(1, 3)
        self.assertEqual(src.priority, 3)
        src = self.b.set_weight(1, 250)
        self.assertEqual(src.weight, 250)

    def test_test_source(self):
        ok = self.b.test_source(1)
        self.assertTrue(ok["ok"])
        self.assertGreaterEqual(ok["latency_ms"], 0)
        cooling = self.b.test_source(3)      # hub-b: cooling
        self.assertFalse(cooling["ok"])
        with self.assertRaises(BackendError):
            self.b.test_source(999)

    # ---- key 生命周期 ------------------------------------------------------
    def test_issue_key(self):
        result = self.b.issue_key({
            "name": "newapp", "models": ["model-a"], "rpm": 5,
            "tpm": 1000, "daily_tokens": 2000, "aid": "agent-1"})
        self.assertTrue(result["secret"].startswith("sk-kln-"))
        self.assertIn("id", result)
        key = next(k for k in self.b.list_keys() if k.name == "newapp")
        self.assertEqual(key.status, "active")
        self.assertEqual(key.models, ["model-a"])
        self.assertEqual(key.aid, "agent-1")
        with self.assertRaises(BackendError):
            self.b.issue_key({"name": "newapp"})

    def test_key_lifecycle(self):
        self.b.suspend_key("opencode")
        self.assertEqual(next(k for k in self.b.list_keys()
                              if k.name == "opencode").status, "suspended")
        with self.assertRaises(BackendError):
            self.b.suspend_key("opencode")     # 重复挂起
        self.b.resume_key("opencode")
        self.assertEqual(next(k for k in self.b.list_keys()
                              if k.name == "opencode").status, "active")
        self.b.revoke_key("opencode")
        revoked = next(k for k in self.b.list_keys()
                       if k.name == "opencode")
        self.assertEqual(revoked.status, "revoked")
        self.assertTrue(revoked.revoked_at)
        with self.assertRaises(BackendError):
            self.b.suspend_key("opencode")     # 吊销后不可挂起
        with self.assertRaises(BackendError):
            self.b.revoke_key("no-such-key")

    # ---- 用量 ---------------------------------------------------------------
    def test_usage_logs_filter_paging(self):
        seed = self.b._usage
        n = len(seed)
        n_err = sum(1 for r in seed if r.get("status") != "ok")
        n_ident = sum(1 for r in seed if r["ident"] == "opencode")
        n_model = sum(1 for r in seed if r["model"] == "model-x")
        # 全量分页
        rows, total = self.b.usage_logs(24, 0, 10)
        self.assertEqual(total, n)
        self.assertEqual(len(rows), 10)
        self.assertGreaterEqual(rows[0].ts, rows[-1].ts)   # 新→旧
        self.assertTrue(rows[0].req_id.startswith("req-mock-"))
        rows2, total = self.b.usage_logs(24, n - 5, 10)
        self.assertEqual(total, n)
        self.assertEqual(len(rows2), 5)
        # ident / 错误 / 搜索过滤（期望直接由种子推导）
        rows, total = self.b.usage_logs(24, 0, 50, {"ident": "opencode"})
        self.assertEqual(total, n_ident)
        self.assertTrue(all(r.ident == "opencode" for r in rows))
        rows, total = self.b.usage_logs(24, 0, 50, {"errors": True})
        self.assertGreater(n_err, 0)
        self.assertEqual(total, n_err)
        self.assertTrue(all(r.is_error for r in rows))
        rows, total = self.b.usage_logs(24, 0, 50, {"errors": False})
        self.assertEqual(total, n - n_err)
        rows, total = self.b.usage_logs(24, 0, 50, {"search": "model-x"})
        self.assertEqual(total, n_model)
        self.assertTrue(all(r.model == "model-x" for r in rows))
        # 组合过滤：ident + 仅错误
        rows, total = self.b.usage_logs(24, 0, 50,
                                        {"ident": "opencode", "search": "x"})
        self.assertLessEqual(total, n_ident)

    def test_prices_crud(self):
        # 种子：model-a 已定价（in/out）
        prices = {p.alias: p for p in self.b.list_prices()}
        self.assertIn("model-a", prices)
        self.assertEqual(prices["model-a"].input_per_mtok, 0.10)
        self.assertIsNone(prices["model-a"].cache_write_per_mtok)
        # 四分量设置：缓存分量可选，未传即未启用
        p = self.b.set_price("model-b", {
            "input_per_mtok": 1.25, "output_per_mtok": 2.5,
            "cache_read_per_mtok": 0.014, "cache_write_per_mtok": 0.175})
        self.assertEqual(
            (p.input_per_mtok, p.output_per_mtok,
             p.cache_read_per_mtok, p.cache_write_per_mtok),
            (1.25, 2.5, 0.014, 0.175))
        self.assertGreaterEqual(self.count("prices-changed"), 1)
        # 只启用部分分量
        p = self.b.set_price("model-c", {"cache_write_per_mtok": 0.5})
        self.assertIsNone(p.input_per_mtok)
        self.assertEqual(p.cache_write_per_mtok, 0.5)
        # 覆盖旧价
        self.b.set_price("model-b", {"input_per_mtok": 0.5})
        prices = {x.alias: x for x in self.b.list_prices()}
        self.assertEqual(prices["model-b"].input_per_mtok, 0.5)
        self.assertIsNone(prices["model-b"].output_per_mtok)
        # 清除 → 不在列表；再清除报错
        self.b.remove_price("model-b")
        self.b.remove_price("model-c")
        self.assertNotIn("model-b",
                         [x.alias for x in self.b.list_prices()])
        with self.assertRaises(BackendError):
            self.b.remove_price("model-b")
        # 校验：空别名 / 负价 / 无分量
        with self.assertRaises(BackendError):
            self.b.set_price("", {"input_per_mtok": 1})
        with self.assertRaises(BackendError):
            self.b.set_price("model-a", {"input_per_mtok": -1})
        with self.assertRaises(BackendError):
            self.b.set_price("model-a", {})

    def test_usage_summary(self):
        summary = self.b.usage_summary(24)
        self.assertGreater(summary.requests, 0)
        self.assertGreater(summary.tokens_in, 0)
        self.assertGreater(summary.tokens_out, 0)
        self.assertEqual(summary.active_keys, 2)   # opencode + client-a
        self.assertEqual(summary.sources_health["primary"], "ok")

    def test_usage_recent_sorted_limited(self):
        rows = self.b.usage_recent(10)
        self.assertEqual(len(rows), 10)
        ts = [r.ts for r in rows]
        self.assertEqual(ts, sorted(ts, reverse=True))

    def test_key_windows(self):
        windows = self.b.key_windows("opencode")
        kinds = [w.window for w in windows]
        self.assertEqual(kinds, ["rpm", "tpm", "daily_tokens"])
        daily = windows[2]
        self.assertGreater(daily.limit, 0)
        self.assertGreaterEqual(daily.pct, 0.0)
        with self.assertRaises(BackendError):
            self.b.key_windows("nobody")

    # ---- tick：确定性模拟数据变化 ---------------------------------------------
    def test_tick_usage_grows(self):
        before = self.b.usage_summary(1).requests
        self.b.tick()
        self.b.tick()
        after = self.b.usage_summary(1).requests
        self.assertGreaterEqual(after, before + 2)
        self.assertGreaterEqual(self.count("usage-changed"), 2)

    def test_tick_health_flip_and_quota(self):
        for _ in range(12):
            self.b.tick()
        self.assertGreaterEqual(self.count("source-health"), 4)   # 每 3 拍
        self.assertGreaterEqual(self.count("quota-warning"), 3)   # 每 4 拍
        ident, window, pct = self.payloads["quota-warning"]
        self.assertTrue(ident)
        self.assertTrue(window)
        self.assertTrue(0 < pct <= 100)

    # ---- Histogram / Stats（确定性新面） ------------------------------------
    def test_histogram_fixed_shape_totals(self):
        """固定 3 个 hour 桶（dimension="" 汇总），值确定。"""
        hist = self.b.histogram(24, "hour", "")
        self.assertEqual(len(hist), 3)
        self.assertEqual([b.requests for b in hist], [10, 14, 16])
        self.assertEqual([b.tokens for b in hist],
                         [14900 + 5000, 19200 + 6700, 22300 + 8100])
        self.assertTrue(all(b.group == "" for b in hist))
        self.assertTrue(all(b.cost is not None and b.cost > 0 for b in hist))
        # label 升序且形如 RFC3339 小时桶
        labels = [b.label for b in hist]
        self.assertEqual(labels, sorted(labels))
        self.assertRegex(labels[0], r"^\d{4}-\d{2}-\d{2}T\d{2}:00:00Z$")

    def test_histogram_by_key_six_rows(self):
        hist = self.b.histogram(24, "hour", "key_name")
        self.assertEqual(len(hist), 6)
        self.assertEqual(sorted({b.group for b in hist}),
                         ["client-a", "opencode"])
        # 每个 label 恰好两组
        by_label = {}
        for b in hist:
            by_label.setdefault(b.label, []).append(b.group)
        self.assertTrue(all(len(v) == 2 for v in by_label.values()))
        # 维度行汇总 == 总量行汇总
        total = sum(b.requests for b in self.b.histogram(24, "hour", ""))
        self.assertEqual(sum(b.requests for b in hist), total)

    def test_histogram_day_bucket_aggregates(self):
        hist = self.b.histogram(24 * 7, "day", "")
        self.assertEqual(len(hist), 1)
        self.assertEqual(hist[0].requests, 40)
        self.assertRegex(hist[0].label, r"^\d{4}-\d{2}-\d{2}T00:00:00Z$")

    def test_histogram_other_dimensions(self):
        hist = self.b.histogram(24, "hour", "model_alias")
        self.assertEqual(sorted({b.group for b in hist}),
                         ["model-a", "test-model"])
        src = self.b.histogram(24, "hour", "source_name")
        self.assertEqual({b.group for b in src}, {"primary"})

    def test_histogram_window_filter_and_validation(self):
        # 窗口极窄（历史桶被排除）→ 空
        self.assertEqual(self.b.histogram(0, "hour", ""), [])
        with self.assertRaises(BackendError):
            self.b.histogram(24, "week", "")
        with self.assertRaises(BackendError):
            self.b.histogram(24, "hour", "bogus")

    def test_histogram_tick_appends_bucket(self):
        before = self.b.histogram(48, "hour", "")
        self.b.tick()
        after = self.b.histogram(48, "hour", "")
        self.assertEqual(len(after), len(before) + 1)
        self.assertGreater(after[-1].label, after[-2].label)

    def test_stats_deterministic_shape(self):
        st = self.b.stats(24)
        self.assertEqual(st.requests, 48)
        self.assertAlmostEqual(st.error_rate, 5 / 48.0)   # i%11 错误 5 行
        self.assertEqual(st.streams, 16)                   # i%3 流式
        self.assertGreater(st.in_tokens, 0)
        self.assertGreater(st.out_tokens, 0)
        self.assertGreater(st.cache_read, 0)
        self.assertGreater(st.cache_write, 0)
        self.assertGreaterEqual(st.ttft_p50_ms, 0)
        self.assertGreaterEqual(st.latency_p95_ms, st.latency_p50_ms)
        self.assertIsNotNone(st.cost)
        self.assertGreater(st.cost, 0)
        self.assertEqual(st.active_ids, 4)                 # 4 个 agent 身份
        # 无窗口样本 → 全 0
        empty = self.b.stats(0)
        self.assertEqual(empty.requests, 0)
        self.assertEqual(empty.error_rate, 0.0)
        self.assertFalse(empty.has_samples)

    def test_stats_reproducible(self):
        from backend import MockBackend as MB
        st1 = MB(live=False).stats(24)
        st2 = MB(live=False).stats(24)
        self.assertEqual(st1.requests, st2.requests)
        self.assertEqual(st1.in_tokens, st2.in_tokens)
        self.assertEqual(st1.error_rate, st2.error_rate)

    # ---- 聚合 / manager ---------------------------------------------------
    def test_alias_sources(self):
        agg = self.b.alias_sources()
        # hub-b 虽映射 model-a 但已停用，不参与聚合
        self.assertEqual(agg["model-a"], ["primary"])
        self.assertEqual(agg["model-x"], ["anthropic-official"])
        # 启用 hub-b 后，别名由两个源提供
        self.b.update_source(3, {"enabled": True})
        agg = self.b.alias_sources()
        self.assertEqual(agg["model-a"], ["hub-b", "primary"])

    def test_manager_info(self):
        info = self.b.manager_info()
        self.assertEqual(info["backend"], "mock")
        self.assertEqual(info["http_addr"], "127.0.0.1:8484")
        self.assertTrue(info["version"])


if __name__ == "__main__":
    unittest.main()
