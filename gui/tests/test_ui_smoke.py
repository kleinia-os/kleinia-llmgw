#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""UI 冒烟测试：xvfb 下构造主窗口与五页（不进 main loop），断言关键子部件。

断言不依赖具体语言文案（i18n 随环境）；setUpClass 显式注入英文 locale
保证确定性，另设 zh_CN 局部用例验证翻译生效。
暗色模式：构造+刷新+短暂主循环（真实绘制回调）在 FORCE_DARK 下不炸，
以及 widgets.py 源码不含硬编码 RGB。
"""

import os
import re
import sys
import unittest

SRC = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
                   "src")
sys.path.insert(0, SRC)


def _display_available():
    try:
        import gi
        try:
            gi.require_version("Adw", "1.0")
        except ValueError:
            gi.require_version("Adw", "1")
        gi.require_version("Gtk", "4.0")
        gi.require_version("Gdk", "4.0")
        from gi.repository import Gtk, Gdk
        Gtk.init()
        return Gdk.Display.get_default() is not None
    except Exception:
        return False


if not _display_available():
    raise unittest.SkipTest(
        "no display (run with: xvfb-run -a python3 -m unittest discover tests)")

from gi.repository import Adw, Gtk  # noqa: E402

import i18n  # noqa: E402
import main as app_main  # noqa: E402
from backend import MockBackend, SOURCE_TYPES, SOURCE_TYPE_LABELS  # noqa: E402


class UISmokeTest(unittest.TestCase):
    _seq = 0

    @classmethod
    def setUpClass(cls):
        i18n.set_locale("en")       # 断言确定性（文案无关设计，双保险）

    @classmethod
    def tearDownClass(cls):
        i18n.set_locale(None)

    def setUp(self):
        # 每个用例独立 app id：宿主存在真实会话总线时，GtkApplication 会在
        # 总线上以 <app-id> 为路径导出 org.gtk.Application，同 id 的第二个
        # 实例会撞路径；测试用唯一 id 规避（不影响生产行为）。
        UISmokeTest._seq += 1
        self.app = app_main.ModelGatewayApp(
            lambda: MockBackend(live=False), non_unique=True,
            app_id="io.kleinia.ModelGateway.Test%d" % UISmokeTest._seq)
        self.app.register(None)
        self.win = app_main.MainWindow(self.app, MockBackend(live=False))
        self.app.add_window(self.win)
        self.win._ensure_pages()

    def tearDown(self):
        self.win.destroy()
        del self.win
        del self.app

    @staticmethod
    def _count_children(widget):
        n = 0
        child = widget.get_first_child()
        while child is not None:
            n += 1
            child = child.get_next_sibling()
        return n

    # ---- 窗口骨架 -------------------------------------------------------
    def test_six_pages_and_sidebar(self):
        self.assertEqual(len(self.win.pages), 6)
        self.assertEqual(
            sorted(p.get_name() for p in self.win.view_stack.get_pages()),
            sorted(["overview", "sources", "models", "keys", "usage",
                    "logs"]))
        rows = list(self.win._nav_rows())
        self.assertEqual(len(rows), 6)
        self.assertTrue(all(hasattr(r, "_page_name") for r in rows))

    def test_selftest_checks_all_pass(self):
        for name, ok in self.win.selftest_checks():
            self.assertTrue(ok, "selftest check failed: %s" % name)

    # ---- i18n：zh_CN 注入下界面文案翻译 ------------------------------------
    def test_sidebar_translated_under_zh(self):
        i18n.set_locale("zh_CN")
        try:
            win = app_main.MainWindow(
                self.app, MockBackend(live=False))
            win._ensure_pages()
            texts = []
            for row in win._nav_rows():
                box = row.get_child()
                child = box.get_first_child()
                while child is not None:
                    if isinstance(child, Gtk.Label):
                        texts.append(child.get_text())
                    child = child.get_next_sibling()
            self.assertIn("模型源", texts)
            self.assertIn("应用与密钥", texts)
            self.assertIn("用量", texts)
            win.destroy()
        finally:
            i18n.set_locale("en")

    # ---- 概览 -----------------------------------------------------------
    def test_overview_kpi_cards_filled(self):
        overview = self.win.pages["overview"]
        self.assertEqual(sorted(overview.cards),
                         ["cost", "error", "requests", "tokens"])
        overview.refresh()
        self.assertNotEqual(overview.cards["requests"].value_label.get_text(),
                            "—")
        self.assertIn(" / ", overview.cards["tokens"].value_label.get_text())
        self.assertIn("%", overview.cards["error"].value_label.get_text())
        self.assertEqual(overview.cards["cost"].value_label.get_text(),
                         "$0.312")

    def test_overview_range_and_trend(self):
        overview = self.win.pages["overview"]
        self.assertEqual(overview.window_hours, 24)
        self.assertEqual(overview.window_bucket, "hour")
        overview.refresh()
        self.assertEqual(len(overview.trend._rows), 3)   # mock 固定 3 桶
        self.assertEqual(
            len(overview.cards["requests"].spark._series), 3)
        overview.range_drop.set_selected(1)
        self.assertEqual(overview.window_hours, 168)
        self.assertEqual(overview.window_bucket, "day")
        self.assertEqual(len(overview.trend._rows), 1)
        overview.range_drop.set_selected(0)

    def test_overview_quality_cards(self):
        """延迟/缓存概览卡（自用量页移入）。"""
        overview = self.win.pages["overview"]
        overview.refresh()
        for key in ("p50", "p95", "ttft"):
            self.assertRegex(overview.latency_labels[key].get_text(),
                             r"\d+(\.\d+)? (ms|s)")
        self.assertRegex(overview.latency_labels["streams"].get_text(),
                         r"%")
        self.assertIn("%", overview.cache_labels["hit"].get_text())
        self.assertNotEqual(overview.cache_labels["write"].get_text(), "—")

    def test_overview_health_and_recent(self):
        overview = self.win.pages["overview"]
        overview.refresh()
        self.assertEqual(self._count_children(overview.health_box), 4)
        self.assertEqual(self._count_children(overview.recent_list), 8)

    def test_alerts_flow_to_overview(self):
        overview = self.win.pages["overview"]
        self.win.push_alert("quota", "OpenCode 82%")
        self.win.push_alert("source", "degraded")
        self.assertEqual(len(self.win.alerts), 2)
        self.assertEqual(self._count_children(overview.alerts_box), 2)

    # ---- 模型源 -----------------------------------------------------------
    def test_sources_rows_show_type_label(self):
        page = self.win.pages["sources"]
        self.assertGreaterEqual(page.row_count(), 4)
        subtitles = [row.get_subtitle() for row in page._rows.values()]
        # 规范显示名（专有名词，不随语言翻译）
        self.assertTrue(any(SOURCE_TYPE_LABELS["openai"] in sub
                            for sub in subtitles))
        self.assertTrue(any(SOURCE_TYPE_LABELS["openai-responses"] in sub
                            for sub in subtitles))
        self.assertTrue(any(SOURCE_TYPE_LABELS["anthropic"] in sub
                            for sub in subtitles))

    def test_add_remove_source_updates_rows(self):
        page = self.win.pages["sources"]
        before = page.row_count()
        self.win.backend.add_source({
            "name": "tmp", "type": "openai",
            "base_url": "https://t.example",
            "models": [{"alias": "m1", "upstream": "m1"}]})
        page.refresh()
        self.assertEqual(page.row_count(), before + 1)
        page.refresh()  # 幂等
        self.assertEqual(page.row_count(), before + 1)

    # ---- 模型 -----------------------------------------------------------
    def test_models_aggregation(self):
        page = self.win.pages["models"]
        self.assertGreaterEqual(page.row_count(), 5)
        page.refresh()
        self.assertFalse(page.status_page.get_visible())

    # ---- 密钥 -----------------------------------------------------------
    def test_keys_rows(self):
        page = self.win.pages["keys"]
        self.assertGreaterEqual(page.row_count(), 4)
        row = page._rows[1]
        self.assertEqual(row.get_title(), "opencode")
        self.assertIn("60", row.get_subtitle())   # rpm

    # ---- 模型定价 -----------------------------------------------------------
    def test_models_pricing_flow(self):
        from pages.models import PriceDialog
        page = self.win.pages["models"]
        backend = self.win.backend
        page.refresh()
        # mock 种子：model-a 已定价（$0.10/$0.30），缓存分量未启用
        priced = {p.alias: p for p in backend.list_prices()}
        self.assertIn("model-a", priced)
        subtitles = [row.get_subtitle() for row in page._row_widgets]
        self.assertTrue(any("$0.1 / 1M input · $0.3 / 1M output" in s
                            for s in subtitles), subtitles)
        self.assertTrue(any("unpriced" in s for s in subtitles), subtitles)
        # 四分量定价 → 副标题包含缓存分量
        backend.set_price("model-b", {"input_per_mtok": 1.25,
                                      "output_per_mtok": 2.5,
                                      "cache_read_per_mtok": 0.014,
                                      "cache_write_per_mtok": 0.175})
        page.refresh()
        subtitles = [row.get_subtitle() for row in page._row_widgets]
        self.assertTrue(any(
            "$1.25 / 1M input · $2.5 / 1M output · "
            "$0.014 / 1M cache read · $0.175 / 1M cache write" in s
            for s in subtitles), subtitles)
        # 对话框：预填（含缓存开关已开）+ 保存路径生效
        dlg = PriceDialog(self.win, backend, "model-b",
                          next(p for p in backend.list_prices()
                               if p.alias == "model-b"))
        self.assertEqual(dlg.rows["input_per_mtok"][1].get_text(), "1.25")
        self.assertTrue(dlg.rows["cache_write_per_mtok"][0].get_active())
        dlg.rows["input_per_mtok"][1].set_text("0.5")
        # 关掉输出分量 → 保存后仅剩其余分量
        dlg.rows["output_per_mtok"][0].set_active(False)
        dlg._on_save(None)
        fresh = {p.alias: p for p in backend.list_prices()}["model-b"]
        self.assertEqual(fresh.input_per_mtok, 0.5)
        self.assertIsNone(fresh.output_per_mtok)
        self.assertEqual(fresh.cache_write_per_mtok, 0.175)
        # 非法输入被拒绝（保存前校验，后端值不变）
        dlg2 = PriceDialog(self.win, backend, "model-b", fresh)
        dlg2.rows["input_per_mtok"][1].set_text("abc")
        dlg2._on_save(None)
        self.assertEqual(
            {p.alias: p for p in backend.list_prices()}["model-b"]
            .input_per_mtok, 0.5)
        backend.remove_price("model-b")

    # ---- 密钥编辑 -----------------------------------------------------------
    def test_confirm_dialog_response_reaches_callback(self):
        """回归：AlertDialog 响应必须触发 callback（曾因 destroy() 不存在
        在回调前抛异常，挂起/吊销/删除确认后毫无反应）。"""
        from widgets import confirm_async
        got = []
        dialog = confirm_async(self.win.window, "h", "b", "ok",
                               lambda ok: got.append(ok))
        dialog.emit("response", "ok")
        self.assertEqual(got, [True])
        dialog.emit("response", "cancel")
        self.assertEqual(got, [True, False])

    def test_key_form_edit_and_update(self):
        from pages.keys import KeyFormDialog
        page = self.win.pages["keys"]
        backend = self.win.backend
        key = next(k for k in backend.list_keys() if k.name == "opencode")
        dlg = KeyFormDialog(self.win, backend, edit_key=key)
        # 预填当前值；0 = 不限
        self.assertEqual(dlg.rpm_row.get_value(), key.rpm or 0)
        self.assertEqual(dlg.tpm_row.get_value(), key.tpm or 0)
        # 白名单为 * 时输入框留空（空 = 不限）
        self.assertEqual(dlg._buffer_text(), "")
        # 修改限额并提交 → 后端值变化
        dlg.rpm_row.set_value(77)
        dlg._on_submit(None)
        fresh = next(k for k in backend.list_keys()
                     if k.name == "opencode")
        self.assertEqual(fresh.rpm, 77)
        page.refresh()

    def test_key_suspend_updates_row_state(self):
        page = self.win.pages["keys"]
        backend = self.win.backend
        key = next(k for k in backend.list_keys()
                   if k.name == "client-a" and k.status == "active")
        page._do_state(key.id, "suspend")

        def all_texts(widget):
            texts = []

            def walk(w):
                if isinstance(w, Gtk.Label):
                    texts.append(w.get_text())
                child = w.get_first_child()
                while child is not None:
                    walk(child)
                    child = child.get_next_sibling()

            walk(widget)
            return " ".join(texts)

        # 行内状态字（suffix 处的 caption）随挂起变为 suspended
        self.assertIn("suspended", all_texts(page._rows[key.id]))
        page._do_state(key.id, "resume")
        self.assertIn("active", all_texts(page._rows[key.id]))

    # ---- 用量 -----------------------------------------------------------
    def test_usage_stacked_and_rank(self):
        page = self.win.pages["usage"]
        self.assertEqual(page.dimension, "ident")
        self.assertEqual(page.since_hours, 24)
        page.refresh()
        self.assertEqual(len(page.stacked._rows), 3)
        self.assertEqual(len(page.rank_chart._rows), 2)
        for _label, segs in page.stacked._rows:
            self.assertEqual(len(segs), 2)
        page.dimension_drop.set_selected(1)
        self.assertEqual(page.dimension, "model")
        self.assertEqual(len(page.stacked._rows), 3)
        page.window_drop.set_selected(1)
        self.assertEqual(page.since_hours, 168)
        self.assertEqual(len(page.stacked._rows), 1)

    def test_usage_rank_click_filters(self):
        page = self.win.pages["usage"]
        page.refresh()
        first_label = page.rank_chart._rows[0][0]
        page._on_rank_clicked(page.rank_chart, first_label)
        self.assertEqual(page._selected_group, first_label)
        self.assertTrue(page.filter_label.get_visible())
        self.assertEqual(page.rank_chart.get_selected(), first_label)
        page._on_rank_clicked(page.rank_chart, first_label)   # 再点取消
        self.assertIsNone(page._selected_group)
        self.assertFalse(page.filter_label.get_visible())

    # ---- 调用日志 -----------------------------------------------------------
    def test_logs_page_build_and_paging(self):
        page = self.win.pages["logs"]
        page.refresh()
        # mock 48 行 < 50/页 → 单页，分页标签形如 “第 1–48 条，共 48 条”
        self.assertGreaterEqual(page.row_count(), 20)
        self.assertEqual(page.items.get_n_items(),
                         min(page.row_count(), 50))
        self.assertIn(str(page.row_count()), page.pager_label.get_text())
        self.assertFalse(page.prev_button.get_sensitive())
        self.assertFalse(page.next_button.get_sensitive())

    def test_logs_filters(self):
        page = self.win.pages["logs"]
        page.refresh()
        # 状态筛选：仅错误 → 只剩错误行
        page.status_drop.set_selected(2)
        self.assertLess(page.row_count(), 10)   # mock 每 11 行 1 个错误
        self.assertTrue(all(page.items.get_item(i).row.is_error
                            for i in range(page.items.get_n_items())))
        page.status_drop.set_selected(0)
        # ident 筛选（ComboRow，首项=全部）：选表格里实际出现的 ident
        model = page.ident_drop.get_model()
        wanted = page.items.get_item(0).row.ident
        idx = next(i for i in range(model.get_n_items())
                   if model.get_string(i) == wanted)
        page.ident_drop.set_selected(idx)
        idents = {page.items.get_item(i).row.ident
                  for i in range(page.items.get_n_items())}
        self.assertEqual(idents, {wanted})
        page.ident_drop.set_selected(0)
        # 模型筛选同理
        mmodel = page.model_drop.get_model()
        wanted_m = page.items.get_item(0).row.model
        midx = next(i for i in range(mmodel.get_n_items())
                    if mmodel.get_string(i) == wanted_m)
        page.model_drop.set_selected(midx)
        models = {page.items.get_item(i).row.model
                  for i in range(page.items.get_n_items())}
        self.assertEqual(models, {wanted_m})
        page.model_drop.set_selected(0)

    def test_logs_column_sort(self):
        """表头点击排序：ColumnViewSorter 直挂 SortListModel（回归：
        notify::sorter 不触发导致排序失效）。"""
        from gi.repository import Gtk
        page = self.win.pages["logs"]
        page.refresh()
        cols = [page.view.get_columns().get_item(i)
                for i in range(page.view.get_columns().get_n_items())]
        lat = next(c for c in cols if c.get_title() == "latency")
        values = lambda: [page.sort_model.get_item(i).prop_latency
                          for i in range(page.sort_model.get_n_items())]
        page.view.sort_by_column(lat, Gtk.SortType.ASCENDING)
        self.assertEqual(values(), sorted(values()))
        page.view.sort_by_column(lat, Gtk.SortType.DESCENDING)
        self.assertEqual(values(), sorted(values(), reverse=True))

    def test_logs_detail_dialog_construct(self):
        from pages.logs import LogDetailDialog
        page = self.win.pages["logs"]
        page.refresh()
        row = page.items.get_item(0).row
        self.assertIsNotNone(row.req_id)     # mock 合成 req id
        LogDetailDialog(self.win, row)

    # ---- 导航 / 弹层构造 ----------------------------------------------------
    def test_open_page_targets(self):
        self.win.open_page("keys")
        visible = self.win.view_stack.get_visible_child()
        self.assertIs(visible, self.win.pages["keys"].widget)
        self.win.open_page("source:primary")
        visible = self.win.view_stack.get_visible_child()
        self.assertIs(visible, self.win.pages["sources"].widget)

    def test_dialogs_construct(self):
        from pages.sources import SourceFormDialog, SourceDetailDialog
        from pages.keys import KeyFormDialog, SecretDialog
        backend = self.win.backend
        SourceFormDialog(self.win, backend)
        src = backend.list_sources()[0]
        edit = SourceFormDialog(self.win, backend, edit_source=src)
        self.assertEqual(edit.name_row.get_text(), src.name)
        SourceDetailDialog(self.win, backend, 1)
        KeyFormDialog(self.win, backend)          # 发放
        key = backend.list_keys()[0]
        KeyFormDialog(self.win, backend, edit_key=key)   # 编辑
        SecretDialog(self.win, "sk-kln-test", "testapp")

    # ---- SourceFormDialog：两组模型配置 + 三类型 -------------------------------
    def test_source_form_type_combo_three_types(self):
        from pages.sources import SourceFormDialog
        dlg = SourceFormDialog(self.win, self.win.backend)
        model = dlg.type_row.get_model()
        items = [model.get_string(i) for i in range(model.get_n_items())]
        self.assertEqual(items, [SOURCE_TYPE_LABELS[k] for k in SOURCE_TYPES])
        # 默认选中第 0 项（openai）
        self.assertEqual(dlg.type_row.get_selected(), 0)
        # 编辑预填：openai 类型源 → 选中索引 0
        primary = next(s for s in self.win.backend.list_sources()
                    if s.type == "openai")
        edit = SourceFormDialog(self.win, self.win.backend, edit_source=primary)
        self.assertEqual(edit.type_row.get_selected(), 0)
        anthropic = next(s for s in self.win.backend.list_sources()
                         if s.type == "anthropic")
        edit2 = SourceFormDialog(self.win, self.win.backend,
                                 edit_source=anthropic)
        self.assertEqual(edit2.type_row.get_selected(),
                         SOURCE_TYPES.index("anthropic"))

    def test_source_form_collect(self):
        """新 API：模型组快速添加 + 自定义映射两组 + 跨组去重。"""
        from pages.sources import SourceFormDialog
        backend = self.win.backend
        dlg = SourceFormDialog(self.win, backend)
        dlg.name_row.set_text("extra")
        dlg.base_row.set_text("https://example.com/v1")
        # 模型组（语义 alias=upstream=同名）
        self.assertTrue(dlg._add_model_name("model-a"))
        self.assertEqual(len(dlg._model_rows), 1)
        # 组内重复被拒（返回 False，行数不变）
        self.assertFalse(dlg._add_model_name("model-a"))
        self.assertEqual(len(dlg._model_rows), 1)
        # Entry 路径（快速添加）：设置文本后调用 → 成组并清空输入
        dlg.model_entry.set_text("model-b")
        self.assertTrue(dlg._add_model_name())
        self.assertEqual(dlg.model_entry.get_text(), "")
        # 自定义映射组
        dlg._add_mapping_row("model-x", "model-x-up")
        spec, err = dlg._collect()
        self.assertIsNone(err)
        got = {(m["alias"], m["upstream"]) for m in spec["models"]}
        self.assertIn(("model-a", "model-a"), got)
        self.assertIn(("model-b", "model-b"), got)
        self.assertIn(("model-x", "model-x-up"), got)
        self.assertEqual(len(spec["models"]), 3)
        self.assertEqual(spec["type"], "openai")   # combo 默认索引 0
        # 添加模式空密钥也携带（可空串）
        self.assertIn("api_key", spec)
        # 删除模型行后再收集
        dlg._make_model_remover("model-b")(None)
        spec, _ = dlg._collect()
        self.assertEqual(len(spec["models"]), 2)
        # 跨组别名重复被拒
        dlg._add_mapping_row("model-a", "other")
        spec, err = dlg._collect()
        self.assertIsNone(spec)
        self.assertIn("Duplicate alias across groups", err)
        # 校验：空名 / 空 Base URL / 无模型
        dlg2 = SourceFormDialog(self.win, backend)
        spec, err = dlg2._collect()
        self.assertIsNone(spec)
        self.assertIn("Name", err)
        dlg2.name_row.set_text("x")
        dlg2.base_row.set_text("https://x/v1")
        spec, err = dlg2._collect()
        self.assertIn("At least one model", err)

    def test_source_form_prefill_split(self):
        """编辑预填：alias==upstream → 模型组，不等 → 映射组。"""
        from pages.sources import SourceFormDialog
        # anthropic-official：model-y 同名；model-x →
        # model-x-up 不同名
        src = next(s for s in self.win.backend.list_sources()
                   if s.name == "anthropic-official")
        same = [m for m in src.models if m.alias == m.upstream]
        diff = [m for m in src.models if m.alias != m.upstream]
        dlg = SourceFormDialog(self.win, self.win.backend, edit_source=src)
        model_names = [name for _row, name in dlg._model_rows]
        self.assertEqual(sorted(model_names), sorted(m.alias for m in same))
        self.assertEqual(len(dlg._map_rows), len(diff))
        for record, m in zip(sorted(dlg._map_rows,
                                    key=lambda r: r["alias"].get_text()),
                             sorted(diff, key=lambda m: m.alias)):
            self.assertEqual(record["alias"].get_text(), m.alias)
            self.assertEqual(record["upstream"].get_text(), m.upstream)
        # 编辑模式：密钥留空不携带（=保持现有）
        spec, err = dlg._collect()
        self.assertIsNone(err)
        self.assertNotIn("api_key", spec)
        self.assertEqual(spec["name"], "anthropic-official")

    def test_menu_button_keeps_labels(self):
        from widgets import menu_button
        fired = []
        mb = menu_button([
            ("Edit…", "document-edit-symbolic",
             lambda _b: fired.append(1), None),
            ("Remove source", "user-trash-symbolic",
             lambda _b: fired.append(2), "destructive-action"),
        ])
        popover = mb.get_popover()
        labels = []

        def walk(w):
            if isinstance(w, Gtk.Label):
                labels.append(w.get_text())
            child = w.get_first_child()
            while child is not None:
                walk(child)
                child = child.get_next_sibling()

        walk(popover.get_child())
        self.assertIn("Edit…", labels)
        self.assertIn("Remove source", labels)

    # ---- 暗色模式 ---------------------------------------------------------
    def test_dark_mode_smoke(self):
        """FORCE_DARK 下构造+刷新+真实绘制（短暂主循环）不炸。"""
        from gi.repository import GLib
        manager = Adw.StyleManager.get_default()
        original = manager.get_color_scheme()
        try:
            manager.set_color_scheme(Adw.ColorScheme.FORCE_DARK)
            for page in self.win.pages.values():
                page.refresh()
            self.win.present()
            loop = GLib.MainLoop()
            GLib.timeout_add(300, loop.quit)
            loop.run()      # 跑几帧：draw 回调（主题色 lookup）真实执行
            manager.set_color_scheme(Adw.ColorScheme.FORCE_LIGHT)
            for page in self.win.pages.values():
                page.refresh()
            GLib.timeout_add(150, loop.quit)
            loop.run()
        finally:
            manager.set_color_scheme(original)
        # 窗口仍存活且页面数据仍在
        self.assertEqual(len(self.win.pages), 6)

    def test_theme_color_resolution(self):
        """theme_color 返回主题查询结果（不炸、分量合法、非固定黑）。"""
        from widgets import theme_color, HealthDot, ColorDot
        for name in (None, "accent_color", "success_color",
                     "warning_color", "error_color"):
            rgba = theme_color(self.win.window, name)
            self.assertEqual(len(rgba), 4)
        dot = HealthDot("ok")
        color = dot._color()
        self.assertEqual(len(color), 4)
        dot2 = ColorDot("error")
        self.assertEqual(len(dot2._color()), 4)
        # 广色域 CSS 颜色可略超 [0,1]（cairo 渲染时裁剪），只断言有限值
        import math as _math
        for name in ("accent_color", "error_color"):
            for component in theme_color(self.win.window, name):
                self.assertTrue(_math.isfinite(component))

    def test_widgets_no_hardcoded_rgb(self):
        """代码级断言：widgets.py 无 set_source_rgb(0,0,0) 与裸 RGB 元组常量。"""
        path = os.path.join(SRC, "widgets.py")
        with open(path, encoding="utf-8") as f:
            source = f.read()
        self.assertNotIn("set_source_rgb(0.0, 0.0, 0.0)", source)
        self.assertNotIn("set_source_rgb(0, 0, 0)", source)
        # set_source_rgb 不得携带任何数字字面量（只能喂主题色分量变量）
        bare = re.findall(r"set_source_rgb\([^)]*\d", source)
        self.assertEqual(bare, [], "set_source_rgb 只应使用主题色分量变量")
        # 主题色之外的裸 RGB 元组常量（形如 (0.1, 0.2, 0.3)）
        rgb_tuples = re.findall(r"[=,(]\s*\(0?\.\d+,\s*0?\.\d+,\s*"
                                r"0?\.\d+\)", source)
        self.assertEqual(rgb_tuples, [], "widgets.py 残留裸 RGB 元组: %r"
                         % rgb_tuples[:3])
        for gone in ("ACCENT =", "ACCENT_LIGHT", "ACCENT_GREEN",
                     "ERROR_RED", "HEALTH_COLORS = {"):
            self.assertNotIn(gone, source)


if __name__ == "__main__":
    unittest.main()
