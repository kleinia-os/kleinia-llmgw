#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""用量分析页（图表与聚合）。

  时间范围选择（24h/7d/30d）× 维度（应用/模型/来源/agent）
  Token 堆叠柱状图（每桶 输入/输出 分段，受排行筛选）
  维度排行 Top-5 横条（点击过滤图表，再点取消）
单条请求明细在“调用日志”页；延迟/缓存概览卡在“概览”页。
"""

import time
from collections import defaultdict

import gi

try:
    gi.require_version("Adw", "1.0")
except ValueError:
    gi.require_version("Adw", "1")
gi.require_version("Gtk", "4.0")
from gi.repository import Adw, Gtk  # noqa: E402

from fmt import fmt_tokens  # noqa: E402
from widgets import (BarChart, StackedBarChart, stack_legend,  # noqa: E402
                     button_row)
from i18n import _  # noqa: E402
from backend import BackendError  # noqa: E402

# (UI 维度键, 标签, Histogram dimension)
DIMENSIONS = [("ident", "By app", "key_name"),
              ("model", "By model", "model_alias"),
              ("source", "By source", "source_name"),
              ("agent", "By agent", "aid")]
# (hours, bucket, 标签)
WINDOW_CHOICES = [(24, "hour", "24 hours"), (24 * 7, "day", "7 days"),
                  (24 * 30, "day", "30 days")]
TOP_N = 5


class UsagePage:
    name = "usage"
    title = "Usage"
    icon = "llmgw-chart-symbolic"

    def __init__(self, win, backend):
        self.win = win
        self.backend = backend
        self._selected_group = None      # 排行点击过滤（None=不过滤）

        self.widget = Gtk.ScrolledWindow(vexpand=True)
        clamp = Adw.Clamp(maximum_size=820)
        box = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=18,
                      margin_top=14, margin_bottom=18,
                      margin_start=12, margin_end=12)
        clamp.set_child(box)
        self.widget.set_child(clamp)

        # ---- 时间范围 × 维度 ------------------------------------------------
        controls = Adw.PreferencesGroup(title=_("Analysis window"))
        range_row = Adw.ActionRow(title=_("Time range"),
                                  subtitle=_("Bucketing follows the window"))
        self.window_drop = Gtk.DropDown.new_from_strings(
            [label for _, _, label in WINDOW_CHOICES])
        self.window_drop.set_valign(Gtk.Align.CENTER)
        self.window_drop.connect("notify::selected",
                                 lambda *a: self._on_controls_changed())
        range_row.add_suffix(self.window_drop)
        controls.add(range_row)

        dim_row = Adw.ActionRow(title=_("Dimension"))
        self.dimension_drop = Gtk.DropDown.new_from_strings(
            [label for _, label, _ in DIMENSIONS])
        self.dimension_drop.set_valign(Gtk.Align.CENTER)
        self.dimension_drop.connect("notify::selected",
                                    lambda *a: self._on_controls_changed())
        dim_row.add_suffix(self.dimension_drop)
        controls.add(dim_row)
        box.append(controls)

        # ---- Token 堆叠柱状图 + 图例 ------------------------------------------
        chart_group = Adw.PreferencesGroup(
            title=_("Token usage"),
            description=_("In / out stacked per bucket (filtered by the "
                          "ranking below)"))
        self.stacked = StackedBarChart(format_value=fmt_tokens)
        chart_group.add(self.stacked)
        self.legend = stack_legend()
        self.legend.set_halign(Gtk.Align.END)
        chart_group.add(self.legend)
        box.append(chart_group)

        # ---- 维度排行 Top-5（点击过滤） ------------------------------------------
        rank_group = Adw.PreferencesGroup(
            title=_("Dimension ranking"),
            description=_("Top %d (by tokens); click an item to filter the "
                          "chart, click again to clear") % TOP_N)
        self.rank_chart = BarChart()
        self.rank_chart.connect("row-activated", self._on_rank_clicked)
        rank_group.add(self.rank_chart)
        self.filter_label = Gtk.Label(label="", halign=Gtk.Align.END)
        self.filter_label.add_css_class("dim-label")
        self.filter_label.set_visible(False)
        rank_group.add(self.filter_label)
        box.append(rank_group)

        # 单条明细去“调用日志”页看
        hint = Adw.PreferencesGroup()
        hint.add(button_row(
            _("Open call logs"), lambda _r: self.win.open_page("logs"),
            icon="text-editor-symbolic"))
        box.append(hint)

        self.refresh()

    # ------------------------------------------------------------------
    # 状态属性
    # ------------------------------------------------------------------
    @property
    def dimension(self):
        return DIMENSIONS[self.dimension_drop.get_selected()][0]

    @property
    def since_hours(self):
        return WINDOW_CHOICES[self.window_drop.get_selected()][0]

    @property
    def window_bucket(self):
        return WINDOW_CHOICES[self.window_drop.get_selected()][1]

    def _on_controls_changed(self):
        self._selected_group = None
        self.refresh()

    # ------------------------------------------------------------------
    # 刷新
    # ------------------------------------------------------------------
    def refresh(self):
        hours = self.since_hours
        bucket = self.window_bucket
        hist_dim = DIMENSIONS[self.dimension_drop.get_selected()][2]
        try:
            hist = self.backend.histogram(hours, bucket, hist_dim)
        except BackendError:
            hist = []

        # ---- 堆叠柱：按桶聚合（受排行筛选） ----
        selected = self._selected_group
        buckets = defaultdict(lambda: [0, 0])
        for b in hist:
            if selected is not None and b.group != selected:
                continue
            cell = buckets[b.label]
            cell[0] += b.in_tokens
            cell[1] += b.out_tokens
        self.stacked.set_rows(
            [(label, segs) for label, segs in sorted(buckets.items())],
            format_value=fmt_tokens)

        # ---- 排行 Top-5 ----
        by_group = defaultdict(int)
        for b in hist:
            by_group[b.group or _("(unknown)")] += b.tokens
        top = sorted(by_group.items(), key=lambda kv: -kv[1])[:TOP_N]
        self.rank_chart.set_rows(top, format_value=fmt_tokens)
        self.rank_chart.set_selected(selected)
        if selected is not None:
            self.filter_label.set_text(
                _("Filtered: %s (click the same item to clear)") % selected)
            self.filter_label.set_visible(True)
        else:
            self.filter_label.set_visible(False)

    # ------------------------------------------------------------------
    def _on_rank_clicked(self, chart, label):
        if self._selected_group == label:
            self._selected_group = None
        else:
            self._selected_group = label
        self.refresh()
