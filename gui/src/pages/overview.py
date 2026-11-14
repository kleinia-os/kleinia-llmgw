#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""概览页。

  时间范围选择（24h/7d/30d → hour/day/day 分桶，页面内状态）
  4 张 KPI 卡（请求 / Token / 成本 / 错误率）× 内嵌 sparkline + 环比角标
  主趋势面积折线（requests↔tokens 切换）
  来源健康卡（健康灯行）+ 最近 8 条请求流（点击跳用量页）+ 告警列表
"""

import time

import gi

try:
    gi.require_version("Adw", "1.0")
except ValueError:
    gi.require_version("Adw", "1")
gi.require_version("Gtk", "4.0")
from gi.repository import Adw, Gtk, Pango  # noqa: E402

from fmt import (fmt_tokens, fmt_cost, fmt_latency, fmt_rel_time,  # noqa: E402
                 fmt_pct1, fmt_int, fmt_delta)
from widgets import (HealthDot, KpiCard, TrendChart, ColorDot,  # noqa: E402
                     stat_cell)
from i18n import _  # noqa: E402
from backend import SOURCE_TYPE_LABELS, BackendError  # noqa: E402

# (hours, bucket, 标签)
WINDOW_CHOICES = [(24, "hour", "24 hours"), (24 * 7, "day", "7 days"),
                  (24 * 30, "day", "30 days")]

METRIC_CHOICES = [("requests", "Request count"), ("tokens", "Token")]
RECENT_LIMIT = 8


class OverviewPage:
    name = "overview"
    title = "Overview"
    icon = "llmgw-gauge-symbolic"

    def __init__(self, win, backend):
        self.win = win
        self.backend = backend
        self.widget = Gtk.ScrolledWindow(vexpand=True)
        clamp = Adw.Clamp(maximum_size=760)
        self.box = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=18,
                           margin_top=14, margin_bottom=18,
                           margin_start=12, margin_end=12)
        clamp.set_child(self.box)
        self.widget.set_child(clamp)

        # ---- 时间范围选择（页面内状态） ---------------------------------
        controls = Adw.PreferencesGroup()
        range_row = Adw.ActionRow(title=_("Time range"),
                                  subtitle=_("Bucketing follows the window"))
        self.range_drop = Gtk.DropDown.new_from_strings(
            [label for _, _, label in WINDOW_CHOICES])
        self.range_drop.set_valign(Gtk.Align.CENTER)
        self.range_drop.connect("notify::selected", lambda *a: self.refresh())
        range_row.add_suffix(self.range_drop)
        controls.add(range_row)
        self.box.append(controls)

        # ---- KPI 卡 2×2 ---------------------------------------------------
        self.cards = {}
        grid = Gtk.Grid(column_spacing=12, row_spacing=12)
        for i, key in enumerate(("requests", "tokens", "cost", "error")):
            card = KpiCard("…")
            self.cards[key] = card
            grid.attach(card, i % 2, i // 2, 1, 1)
        self.box.append(grid)

        # ---- 延迟 / 缓存 概览卡（自用量页移入：窗口级聚合属于概览） ------------
        quality_grid = Gtk.Grid(column_spacing=12, row_spacing=12)
        self.latency_card = Gtk.Box(orientation=Gtk.Orientation.VERTICAL,
                                    spacing=6)
        self.latency_card.add_css_class("card")
        self.latency_card.add_css_class("padded-card")
        self.latency_card.set_hexpand(True)
        title = Gtk.Label(label=_("Latency"), halign=Gtk.Align.START)
        title.add_css_class("heading")
        self.latency_card.append(title)
        lat_grid = Gtk.Grid(column_spacing=16, row_spacing=10)
        self.latency_labels = {}
        for i, (key, caption) in enumerate((
                ("p50", _("p50 latency")), ("p95", _("p95 latency")),
                ("ttft", _("TTFT median")), ("streams", _("Streaming")))):
            cell, value_label = stat_cell(caption)
            self.latency_labels[key] = value_label
            lat_grid.attach(cell, i % 2, i // 2, 1, 1)
        self.latency_card.append(lat_grid)
        quality_grid.attach(self.latency_card, 0, 0, 1, 1)

        self.cache_card = Gtk.Box(orientation=Gtk.Orientation.VERTICAL,
                                  spacing=6)
        self.cache_card.add_css_class("card")
        self.cache_card.add_css_class("padded-card")
        self.cache_card.set_hexpand(True)
        cache_title = Gtk.Label(label=_("Cache"), halign=Gtk.Align.START)
        cache_title.add_css_class("heading")
        self.cache_card.append(cache_title)
        cache_grid = Gtk.Grid(column_spacing=16, row_spacing=10)
        self.cache_labels = {}
        for i, (key, caption) in enumerate((
                ("hit", _("Cache hit rate")), ("write", _("Cache writes")))):
            cell, value_label = stat_cell(caption)
            self.cache_labels[key] = value_label
            cache_grid.attach(cell, i % 2, i // 2, 1, 1)
        self.cache_card.append(cache_grid)
        self.cache_empty = Gtk.Label(label=_("No cache activity in this "
                                             "window"),
                                     halign=Gtk.Align.START, xalign=0)
        self.cache_empty.add_css_class("dim-label")
        self.cache_empty.set_visible(False)
        self.cache_card.append(self.cache_empty)
        quality_grid.attach(self.cache_card, 1, 0, 1, 1)
        self.box.append(quality_grid)

        # ---- 主趋势图 ------------------------------------------------------
        trend_group = Adw.PreferencesGroup(title=_("Trend"))
        metric_row = Adw.ActionRow(title=_("Metric"))
        self.metric_drop = Gtk.DropDown.new_from_strings(
            [label for _, label in METRIC_CHOICES])
        self.metric_drop.set_valign(Gtk.Align.CENTER)
        self.metric_drop.connect("notify::selected", lambda *a: self.refresh())
        metric_row.add_suffix(self.metric_drop)
        trend_group.add(metric_row)
        self.trend = TrendChart(format_value=fmt_int)
        trend_group.add(self.trend)
        self.box.append(trend_group)

        # ---- 来源健康卡 -----------------------------------------------------
        self.health_group = Adw.PreferencesGroup(title=_("Source health"))
        self.health_box = Gtk.ListBox()
        self.health_box.add_css_class("boxed-list")
        self.health_box.set_selection_mode(Gtk.SelectionMode.NONE)
        self.health_group.add(self.health_box)
        self.box.append(self.health_group)

        # ---- 最近 8 条请求（点击跳用量页） -----------------------------------
        self.recent_group = Adw.PreferencesGroup(
            title=_("Recent activity"),
            description=_("Latest %d requests (click for usage details)")
            % RECENT_LIMIT)
        self.recent_list = Gtk.ListBox()
        self.recent_list.add_css_class("boxed-list")
        self.recent_list.set_selection_mode(Gtk.SelectionMode.NONE)
        self.recent_list.connect("row-activated",
                                 lambda *a: self.win.open_page("logs"))
        self.recent_group.add(self.recent_list)
        self.box.append(self.recent_group)

        # ---- 告警（配额/健康，本会话） ----------------------------------------
        self.alerts_group = Adw.PreferencesGroup(
            title=_("Recent alerts"),
            description=_("Quota warnings and source health changes "
                          "(this session)"))
        self.alerts_box = Gtk.ListBox()
        self.alerts_box.add_css_class("boxed-list")
        self.alerts_box.set_selection_mode(Gtk.SelectionMode.NONE)
        self.alerts_group.add(self.alerts_box)
        self.box.append(self.alerts_group)

        self.update_alerts([])
        self.refresh()

    # ------------------------------------------------------------------
    @property
    def window_hours(self):
        return WINDOW_CHOICES[self.range_drop.get_selected()][0]

    @property
    def window_bucket(self):
        return WINDOW_CHOICES[self.range_drop.get_selected()][1]

    # ------------------------------------------------------------------
    # 刷新
    # ------------------------------------------------------------------
    def refresh(self):
        hours = self.window_hours
        try:
            stats = self.backend.stats(hours)
        except BackendError:
            stats = None
        try:
            prev_stats = self.backend.stats(hours * 2) if stats else None
        except BackendError:
            prev_stats = None
        try:
            hist = self.backend.histogram(hours, self.window_bucket, "")
        except BackendError:
            hist = []
        self._refresh_kpis(stats, prev_stats, hist)
        self._refresh_quality(stats)
        self._refresh_trend(hist)
        self._refresh_health()
        self._refresh_recent()
        if stats is None:
            for card in self.cards.values():
                card.set_value("—")

    # ---- KPI ----------------------------------------------------------
    def _refresh_kpis(self, stats, prev, hist):
        if stats is None:
            return
        # 环比：Stats 的 sums/counts 可加，前窗口 = stats(2W) − stats(W)
        def _prev_of(cur_value, prev_value):
            if prev is None:
                return None
            return max(0, prev_value - cur_value)

        requests_series = [b.requests for b in hist]
        tokens_series = [b.tokens for b in hist]
        cost_series = [b.cost or 0.0 for b in hist]

        # 请求数
        card = self.cards["requests"]
        card.title_label.set_text(_("Requests"))
        card.set_value(fmt_int(stats.requests))
        self._set_delta(card, stats.requests,
                        _prev_of(stats.requests, prev.requests
                                 if prev else None), up_is_good=True)
        card.spark.set_series(requests_series)
        # Token（入+出）
        card = self.cards["tokens"]
        card.title_label.set_text(_("Tokens (in / out)"))
        card.set_value("%s / %s" % (fmt_tokens(stats.in_tokens),
                                    fmt_tokens(stats.out_tokens)))
        self._set_delta(card, stats.tokens,
                        _prev_of(stats.tokens, prev.tokens
                                 if prev else None), up_is_good=True)
        card.spark.set_series(tokens_series)
        # 成本（无价 "—"，无对比不显角标）
        card = self.cards["cost"]
        card.title_label.set_text(_("Cost"))
        card.set_value(fmt_cost(stats.cost))
        prev_cost = prev.cost if (prev is not None and prev.cost is not None
                                  and stats.cost is not None) else None
        self._set_delta(card, stats.cost if stats.cost is not None else 0,
                        _prev_of(stats.cost or 0, prev_cost or 0)
                        if prev_cost is not None else None, up_is_good=True)
        card.spark.set_series(cost_series if stats.cost is not None else [])
        # 错误率（升=坏）
        card = self.cards["error"]
        card.title_label.set_text(_("Error rate"))
        card.set_value(fmt_pct1(stats.error_rate))
        prev_window_reqs = (prev.requests - stats.requests) \
            if prev is not None else 0
        if prev_window_reqs > 0:
            errors_now = stats.error_rate * stats.requests
            errors_prev = prev.error_rate * prev.requests - errors_now
            prev_rate = max(0.0, errors_prev / float(prev_window_reqs))
            self._set_delta(card, stats.error_rate, prev_rate,
                            up_is_good=False)
        else:
            card.set_delta(None)
        card.spark.set_series(requests_series)   # 样本量趋势

    @staticmethod
    def _set_delta(card, current, previous, up_is_good):
        text = fmt_delta(current, previous)
        if text is None:
            card.set_delta(None)
            return
        if not text.startswith(("↑", "↓")):
            card.set_delta(text)          # "新"：中性角标，不带涨跌色
            return
        going_up = text.startswith("↑")
        css = "success" if going_up == up_is_good else "error"
        card.set_delta(text, css)

    # ---- 延迟 / 缓存 ------------------------------------------------------
    def _refresh_quality(self, stats):
        if stats is None:
            for label in self.latency_labels.values():
                label.set_text("—")
            for label in self.cache_labels.values():
                label.set_text("—")
            return
        self.latency_labels["p50"].set_text(
            fmt_latency(stats.latency_p50_ms))
        self.latency_labels["p95"].set_text(
            fmt_latency(stats.latency_p95_ms))
        self.latency_labels["ttft"].set_text(
            fmt_latency(stats.ttft_p50_ms))
        self.latency_labels["streams"].set_text(fmt_pct1(
            stats.streams / stats.requests if stats.requests else 0.0))
        has_cache = stats.cache_read > 0 or stats.cache_write > 0
        self.cache_empty.set_visible(not has_cache)
        # 缓存网格是 card 的第二个子件（标题后）；显隐随 has_cache
        grid = self.cache_card.get_first_child().get_next_sibling()
        grid.set_visible(has_cache)
        denom = stats.in_tokens + stats.cache_read
        hit = stats.cache_read / float(denom) if denom else 0.0
        self.cache_labels["hit"].set_text(fmt_pct1(hit))
        self.cache_labels["write"].set_text(fmt_tokens(stats.cache_write))

    # ---- 趋势 ------------------------------------------------------------
    def _refresh_trend(self, hist):
        metric = METRIC_CHOICES[self.metric_drop.get_selected()][0]
        if metric == "tokens":
            series = [(b.label, b.tokens) for b in hist]
            self.trend.set_rows(series, format_value=fmt_tokens,
                                bucket_kind=self.window_bucket)
        else:
            series = [(b.label, b.requests) for b in hist]
            self.trend.set_rows(series, format_value=fmt_int,
                                bucket_kind=self.window_bucket)

    # ---- 来源健康 ----------------------------------------------------------
    def _refresh_health(self):
        list_box = self.health_box
        child = list_box.get_first_child()
        while child is not None:
            nxt = child.get_next_sibling()
            list_box.remove(child)
            child = nxt
        try:
            sources = self.backend.list_sources()
        except BackendError:
            sources = []
        if not sources:
            row = Adw.ActionRow(title=_("No sources yet"),
                                subtitle=_("Add the first upstream on the "
                                           "Sources page"))
            row.add_css_class("dim-label")
            list_box.append(row)
            return
        for source in sources:
            row = Adw.ActionRow(
                title=source.name,
                subtitle="%s · %s" % (
                    SOURCE_TYPE_LABELS.get(source.type, source.type),
                    _("priority %d · weight %d%s")
                    % (source.priority, source.weight,
                       _(" · disabled") if not source.enabled else "")))
            row.add_prefix(HealthDot(source.health.state))
            list_box.append(row)

    # ---- 最近 8 条请求 -------------------------------------------------------
    def _refresh_recent(self):
        list_box = self.recent_list
        child = list_box.get_first_child()
        while child is not None:
            nxt = child.get_next_sibling()
            list_box.remove(child)
            child = nxt
        try:
            rows = self.backend.usage_recent(RECENT_LIMIT)
        except BackendError:
            rows = []
        now = time.time()
        if not rows:
            row = Adw.ActionRow(title=_("No requests yet"),
                                subtitle=_("Requests sent through the gateway "
                                           "will appear here"))
            row.add_css_class("dim-label")
            list_box.append(row)
            return
        for r in rows:
            box = Gtk.Box(orientation=Gtk.Orientation.HORIZONTAL, spacing=8,
                          margin_top=8, margin_bottom=8,
                          margin_start=8, margin_end=8)
            box.append(ColorDot("error" if r.is_error else "success"))
            label = Gtk.Label(label="%s · %s" % (r.ident, r.model),
                              hexpand=True, xalign=0)
            label.set_ellipsize(Pango.EllipsizeMode.END)
            box.append(label)
            tail = Gtk.Label(label="%s · %s" % (fmt_rel_time(r.ts, now),
                                                fmt_latency(r.latency_ms)))
            tail.add_css_class("dim-label")
            box.append(tail)
            list_row = Gtk.ListBoxRow(child=box, activatable=True)
            list_row.set_tooltip_text(
                ("%s · %s" % (r.status, r.error_kind)) if r.is_error
                else r.status)
            list_box.append(list_row)

    # ------------------------------------------------------------------
    def update_alerts(self, alerts):
        """alerts: [(epoch, kind, text)]，新→旧；窗口层维护并调用。"""
        list_box = self.alerts_box
        child = list_box.get_first_child()
        while child is not None:
            nxt = child.get_next_sibling()
            list_box.remove(child)
            child = nxt
        if not alerts:
            row = Adw.ActionRow(title=_("No alerts"))
            row.add_css_class("dim-label")
            list_box.append(row)
            return
        for epoch, kind, text in alerts[:20]:
            row = Adw.ActionRow(title=text)
            stamp = time.strftime("%H:%M:%S", time.localtime(epoch)) \
                if epoch else ""
            row.set_subtitle(stamp)
            dot_state = "cooling" if kind == "quota" else "down"
            if kind == "ok":
                dot_state = "ok"
            row.add_prefix(HealthDot(dot_state))
            list_box.append(row)
