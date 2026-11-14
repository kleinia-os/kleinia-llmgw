#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""i18n 单测：查表命中/回落/注入/检测 mock/防崩溃 + zh_CN 表全覆盖校验。"""

import ast
import os
import sys
import unittest

SRC = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
                   "src")
sys.path.insert(0, SRC)

import i18n  # noqa: E402


class I18nLookupTest(unittest.TestCase):
    def tearDown(self):
        i18n.set_locale(None)      # 恢复自动检测

    def test_zh_cn_hit(self):
        i18n.set_locale("zh_CN")
        self.assertEqual(i18n._("Model Gateway"), "模型网关")
        self.assertEqual(i18n._("Retry"), "重试")
        self.assertEqual(i18n._("Type a model name and press Enter"),
                         "输入模型名，回车添加")

    def test_zh_base_fallback(self):
        """zh_HK/zh_SG 无全名表项 → 语言部 zh 回落到 zh_CN 表。"""
        i18n.set_locale("zh_HK")
        self.assertEqual(i18n._("Model Gateway"), "模型网关")

    def test_unknown_locale_falls_back_to_msgid(self):
        i18n.set_locale("en")
        self.assertEqual(i18n._("Model Gateway"), "Model Gateway")
        i18n.set_locale("fr")
        self.assertEqual(i18n._("Retry"), "Retry")

    def test_proper_noun_not_in_table(self):
        """专有名词（上游类型显示名）不进译文表：回落原文。"""
        i18n.set_locale("zh_CN")
        self.assertEqual(i18n._("OpenAI Chat Completions"),
                         "OpenAI Chat Completions")
        self.assertEqual(i18n._("OpenAI Responses"), "OpenAI Responses")
        self.assertEqual(i18n._("Anthropic Messages"), "Anthropic Messages")

    def test_guards(self):
        i18n.set_locale("zh_CN")
        self.assertEqual(i18n._(""), "")            # 空 msgid
        self.assertIsNone(i18n._(None))             # None 防崩溃
        self.assertEqual(i18n._("no such key ever"), "no such key ever")

    def test_format_arg(self):
        i18n.set_locale("zh_CN")
        self.assertEqual(i18n._("Deleted source %s", "primary"), "已删除源 primary")
        # 格式化失败不炸（占位符数量不符）
        self.assertEqual(i18n._("Deleted source %s", 1, 2), "已删除源 %s")

    def test_set_locale_reset_restores_autodetect(self):
        i18n.set_locale("zh_CN")
        self.assertEqual(i18n.get_locale(), "zh_CN")
        i18n.set_locale(None)
        # 回到自动检测（宿主 LANG 决定）
        self.assertIn(i18n.get_locale(), [None, "zh_CN", "zh"])


class I18nDetectTest(unittest.TestCase):
    def tearDown(self):
        i18n.set_locale(None)

    def test_detect_matches_full_then_language_part(self):
        from gi.repository import GLib
        original = GLib.get_language_names
        try:
            GLib.get_language_names = lambda: ["zh_CN.UTF-8", "zh_CN",
                                               "zh", "C", "en"]
            self.assertEqual(i18n.detect_locale(), "zh_CN")
            GLib.get_language_names = lambda: ["zh_HK", "zh", "C"]
            self.assertEqual(i18n.detect_locale(), "zh")
            GLib.get_language_names = lambda: ["en_US", "en", "C"]
            self.assertIsNone(i18n.detect_locale())
        finally:
            GLib.get_language_names = original

    def test_detect_lazy_and_responsive_to_injection(self):
        """检测惰性（首调用缓存）且 set_locale 后立即生效（测试可 mock）。"""
        from gi.repository import GLib
        original = GLib.get_language_names
        try:
            GLib.get_language_names = lambda: ["zh_CN", "C"]
            i18n.set_locale(None)
            self.assertEqual(i18n._("Overview"), "概览")
            i18n.set_locale("en")                    # 注入覆盖检测
            self.assertEqual(i18n._("Overview"), "Overview")
        finally:
            GLib.get_language_names = original


class I18nCoverageTest(unittest.TestCase):
    """zh_CN 表必须覆盖源码中全部 ``_("...")`` 字面量 msgid（AST 提取，
    兼容跨行隐式拼接；变量传入的 msgid 由上方用例覆盖）。"""

    FILES = ["main.py", "backend.py", "fmt.py", "widgets.py",
             "pages/overview.py", "pages/sources.py", "pages/models.py",
             "pages/keys.py", "pages/usage.py"]

    def _used_msgids(self):
        used = set()
        for name in self.FILES:
            path = os.path.join(SRC, name)
            tree = ast.parse(open(path, encoding="utf-8").read())
            for node in ast.walk(tree):
                if (isinstance(node, ast.Call)
                        and isinstance(node.func, ast.Name)
                        and node.func.id == "_" and node.args
                        and isinstance(node.args[0], ast.Constant)
                        and isinstance(node.args[0].value, str)):
                    used.add(node.args[0].value)
        return used

    def test_every_msgid_has_zh_cn_translation(self):
        used = self._used_msgids()
        self.assertGreater(len(used), 150, "msgid 提取异常（数量过少）")
        missing = sorted(m for m in used
                         if m not in i18n._TRANSLATIONS["zh_CN"])
        self.assertEqual(missing, [],
                         "zh_CN 表缺少译文: %r" % missing)

    def test_rendering_msgids_covered(self):
        """经变量传入 _() 的固定 msgid（状态/分段/列名/页名）也在表内。"""
        table = i18n._TRANSLATIONS["zh_CN"]
        for msgid in ("healthy", "degraded", "cooling", "down", "disabled",
                      "unknown", "input", "output", "cache read",
                      "Overview", "Sources", "Models", "Apps and Keys",
                      "Usage", "Model Gateway",
                      "active", "suspended", "revoked"):
            self.assertIn(msgid, table, "缺 %r" % msgid)


if __name__ == "__main__":
    unittest.main()
