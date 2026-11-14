#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""fmt.py 格式化工具单测（纯逻辑，无 GTK）。"""

import os
import sys
import unittest

sys.path.insert(0, os.path.join(
    os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "src"))

import i18n  # noqa: E402
from fmt import (fmt_tokens, fmt_cost, fmt_latency, fmt_rel_time,  # noqa: E402
                 fmt_pct1, fmt_int, fmt_delta, fmt_bucket_label)


class FmtTokensTest(unittest.TestCase):
    def test_plain_and_units(self):
        self.assertEqual(fmt_tokens(0), "0")
        self.assertEqual(fmt_tokens(940), "940")
        self.assertEqual(fmt_tokens(1_200), "1.2K")
        self.assertEqual(fmt_tokens(3_400_000), "3.4M")
        self.assertEqual(fmt_tokens(1_100_000_000), "1.1G")

    def test_bad(self):
        self.assertEqual(fmt_tokens(None), "—")
        self.assertEqual(fmt_tokens("x"), "—")
        self.assertEqual(fmt_tokens(-5), "—")


class FmtCostTest(unittest.TestCase):
    def test_three_decimals(self):
        self.assertEqual(fmt_cost(0.004), "$0.004")
        self.assertEqual(fmt_cost(1.2355), "$1.236")   # 进位
        self.assertEqual(fmt_cost(0), "$0.000")

    def test_none_and_bad(self):
        self.assertEqual(fmt_cost(None), "—")
        self.assertEqual(fmt_cost("abc"), "—")


class FmtLatencyTest(unittest.TestCase):
    def tearDown(self):
        i18n.set_locale(None)

    def test_adaptive_units(self):
        self.assertEqual(fmt_latency(850), "850 ms")
        self.assertEqual(fmt_latency(1_200), "1.2 s")
        # 分钟单位随界面语言（msgid "%.1f min"）
        i18n.set_locale("zh_CN")
        self.assertEqual(fmt_latency(90_000), "1.5 分钟")
        i18n.set_locale("en")
        self.assertEqual(fmt_latency(90_000), "1.5 min")

    def test_bad(self):
        self.assertEqual(fmt_latency(None), "—")
        self.assertEqual(fmt_latency(-1), "—")


class FmtRelTimeTest(unittest.TestCase):
    NOW = 1_793_000_000.0

    def tearDown(self):
        i18n.set_locale(None)

    def test_buckets_zh(self):
        i18n.set_locale("zh_CN")
        self.assertEqual(fmt_rel_time(self.NOW - 30, self.NOW), "刚刚")
        self.assertEqual(fmt_rel_time(self.NOW - 5 * 60, self.NOW), "5 分钟前")
        self.assertEqual(fmt_rel_time(self.NOW - 3 * 3600, self.NOW), "3 小时前")
        self.assertEqual(fmt_rel_time(self.NOW - 30 * 3600, self.NOW), "昨天")
        self.assertEqual(fmt_rel_time(self.NOW - 8 * 86400, self.NOW), "8 天前")

    def test_buckets_en(self):
        i18n.set_locale("en")
        self.assertEqual(fmt_rel_time(self.NOW - 30, self.NOW), "just now")
        self.assertEqual(fmt_rel_time(self.NOW - 5 * 60, self.NOW), "5 min ago")
        self.assertEqual(fmt_rel_time(self.NOW - 3 * 3600, self.NOW), "3 h ago")
        self.assertEqual(fmt_rel_time(self.NOW - 30 * 3600, self.NOW),
                         "yesterday")
        self.assertEqual(fmt_rel_time(self.NOW - 8 * 86400, self.NOW),
                         "8 d ago")

    def test_bad(self):
        self.assertEqual(fmt_rel_time(None, self.NOW), "—")


class FmtPctTest(unittest.TestCase):
    def test_fraction_to_percent_1dp(self):
        self.assertEqual(fmt_pct1(0.123), "12.3%")
        self.assertEqual(fmt_pct1(0), "0.0%")
        self.assertEqual(fmt_pct1(1), "100.0%")
        self.assertEqual(fmt_pct1("bad"), "—")


class FmtIntTest(unittest.TestCase):
    def test_thousands(self):
        self.assertEqual(fmt_int(1234), "1,234")
        self.assertEqual(fmt_int(None), "—")


class FmtDeltaTest(unittest.TestCase):
    def tearDown(self):
        i18n.set_locale(None)

    def test_no_baseline(self):
        self.assertIsNone(fmt_delta(0, 0))
        # "新" 徽标走翻译表：zh_CN 下是中文，英文回落 msgid 本身
        i18n.set_locale("zh_CN")
        self.assertEqual(fmt_delta(5, 0), "新")
        i18n.set_locale("en")
        self.assertEqual(fmt_delta(5, 0), "new")

    def test_up_down(self):
        text = fmt_delta(110, 100)
        self.assertTrue(text.startswith("↑"))
        self.assertIn("+10.0%", text)
        text = fmt_delta(90, 100)
        self.assertTrue(text.startswith("↓"))
        self.assertIn("-10.0%", text)

    def test_bad(self):
        self.assertIsNone(fmt_delta(None, 5))
        self.assertIsNone(fmt_delta(5, -1))


class FmtBucketLabelTest(unittest.TestCase):
    def test_hour_and_day(self):
        self.assertEqual(fmt_bucket_label("2026-10-28T14:00:00Z"), "14:00")
        self.assertEqual(fmt_bucket_label("2026-10-28T00:00:00Z"), "10-28")
        self.assertEqual(fmt_bucket_label("2026-10-28"), "2026-10-28")


if __name__ == "__main__":
    unittest.main()
