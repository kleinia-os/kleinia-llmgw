#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""调用日志页：全量请求日志的分页表格（Gtk.ColumnView）。

  筛选（ident/模型/来源/状态/时间窗，每行一个自描述 ComboRow）
  + 可点表头排序（本页内）+ 分页（50/页）+ 点击行看完整详情。
  数据走 Usage1.Logs（守护进程侧过滤/分页，返回行数+总数）。
"""

import time
from datetime import datetime

import gi

try:
    gi.require_version("Adw", "1.0")
except ValueError:
    gi.require_version("Adw", "1")
gi.require_version("Gtk", "4.0")
from gi.repository import Adw, Gio, GLib, GObject, Gtk, Pango  # noqa: E402

from fmt import fmt_int, fmt_tokens, fmt_latency, fmt_rel_time, fmt_cost  # noqa: E402
from widgets import ColorDot  # noqa: E402
from i18n import _  # noqa: E402
from backend import BackendError  # noqa: E402

PAGE_SIZE = 50

# (标签 msgid, 小时数)；最后一项“全部时间”用足够大的窗口
WINDOW_CHOICES = [(24, "24 hours"), (24 * 7, "7 days"),
                  (24 * 30, "30 days"), (24 * 36500, "All time")]
STATUS_CHOICES = ("All", "OK", "Errors")


class LogItem(GObject.Object):
    """ColumnView 行模型：排序用的强类型属性 + 展示字段。"""

    def __init__(self, row, now=None):
        super().__init__()
        self.row = row
        now = now or time.time()
        self.prop_ts = row.ts
        self.prop_time = fmt_rel_time(row.ts, now)
        self.prop_ident = row.ident
        self.prop_model = row.model
        self.prop_in = row.in_tokens
        self.prop_out = row.out_tokens
        self.prop_latency = row.latency_ms

    # GObject 属性（列排序表达式用；ts=int 毫秒避免浮点 sorter 兼容问题）
    prop_ts = GObject.Property(type=int)
    prop_time = GObject.Property(type=str, default="")
    prop_ident = GObject.Property(type=str, default="")
    prop_model = GObject.Property(type=str, default="")
    prop_in = GObject.Property(type=int)
    prop_out = GObject.Property(type=int)
    prop_latency = GObject.Property(type=int)


class LogsPage:
    name = "logs"
    title = "Call logs"
    icon = "text-editor-symbolic"

    def __init__(self, win, backend):
        self.win = win
        self.backend = backend
        self._page = 0
        self._total = 0
        self._ready = False          # 构造期信号（set_model/选中初始化）不触发刷新
        self._loading = False

        # 页面骨架：筛选、表格、分页全部进同一个 Clamp —— 等宽对齐；
        # ColumnView 自带滚动，占满剩余高度
        self.widget = Gtk.Box(orientation=Gtk.Orientation.VERTICAL,
                              vexpand=True)
        self.clamp = Adw.Clamp(maximum_size=1000)
        inner = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=12,
                        margin_top=14, margin_bottom=12,
                        margin_start=12, margin_end=12, vexpand=True)
        self.clamp.set_child(inner)
        self.widget.append(self.clamp)
        self._build_controls(inner)
        self._build_table(inner)
        self._build_pager(inner)
        self._ready = True
        self.refresh()

    # ------------------------------------------------------------------
    # 控件区：两行紧凑筛选（标签 + Gtk.DropDown），不占整行
    # ------------------------------------------------------------------
    def _build_controls(self, top):
        group = Adw.PreferencesGroup(title=_("Call logs"))

        filters_row = Adw.ActionRow(title=_("Filters"))
        box = Gtk.Box(orientation=Gtk.Orientation.HORIZONTAL, spacing=10,
                      valign=Gtk.Align.CENTER)
        self.ident_drop = self._labeled_dropdown(box, _("ident"))
        self.model_drop = self._labeled_dropdown(box, _("model"))
        self.source_drop = self._labeled_dropdown(box, _("source"))
        filters_row.add_suffix(box)
        group.add(filters_row)

        status_row = Adw.ActionRow(title=_("Status"))
        sbox = Gtk.Box(orientation=Gtk.Orientation.HORIZONTAL, spacing=10,
                       valign=Gtk.Align.CENTER)
        self.status_drop = self._labeled_dropdown(
            sbox, _("status"), [_("All"), _("OK"), _("Errors")])
        self.window_drop = self._labeled_dropdown(
            sbox, _("Time range"),
            [_(label) for _h, label in WINDOW_CHOICES])
        status_row.add_suffix(sbox)
        group.add(status_row)
        top.append(group)
        # 目录值填充放最后：set_model 会触发 notify::selected→refresh，
        # 此时全部控件（含 window_drop）必须已就位
        self._populate_filters()

    def _labeled_dropdown(self, box, label, strings=None):
        """标签 + DropDown 一组；返回 DropDown。"""
        caption = Gtk.Label(label=label)
        caption.add_css_class("dim-label")
        caption.add_css_class("caption")
        box.append(caption)
        drop = Gtk.DropDown.new_from_strings(strings or [])
        drop.set_valign(Gtk.Align.CENTER)
        drop.connect("notify::selected",
                     lambda *a: self._on_filters_changed())
        box.append(drop)
        return drop

    def _populate_filters(self):
        """ident/model/source 下拉：首项“全部”+ 后端目录值。"""
        idents, models, sources = [], [], []
        try:
            idents = [k.name for k in self.backend.list_keys()]
        except BackendError:
            pass
        try:
            models = list(self.backend.alias_sources().keys())
        except BackendError:
            pass
        try:
            sources = [s.name for s in self.backend.list_sources()]
        except BackendError:
            pass
        for drop, values in ((self.ident_drop, idents),
                             (self.model_drop, models),
                             (self.source_drop, sources)):
            drop.set_model(Gtk.StringList(
                strings=[_("All")] + sorted(set(values))))

    # ------------------------------------------------------------------
    # 表格
    # ------------------------------------------------------------------
    def _build_table(self, parent):
        self.items = Gio.ListStore.new(LogItem)
        # 先建视图与列：view.get_sorter() 是稳定的 ColumnViewSorter 聚合
        # 对象（点击列头只改其内部状态、不发 notify::sorter），因此必须
        # 把它直接交给 SortListModel 持有，表头点击才会生效。
        self.view = Gtk.ColumnView()
        self.view.set_vexpand(True)
        self.view.set_single_click_activate(False)
        self.view.connect("activate", self._on_row_activated)

        def expr(prop):
            return Gtk.PropertyExpression.new(LogItem, None, prop)

        self._add_text_column(_("time"), "prop_time", 92,
                              sorter=Gtk.NumericSorter.new(expr("prop_ts")))
        self._add_text_column(_("ident"), "prop_ident", 110,
                              sorter=Gtk.StringSorter.new(expr("prop_ident")))
        model_col = self._add_text_column(
            _("model"), "prop_model", 150,
            sorter=Gtk.StringSorter.new(expr("prop_model")))
        model_col.set_expand(True)
        self._add_num_column(_("in"), "prop_in",
                             sorter=Gtk.NumericSorter.new(expr("prop_in")))
        self._add_num_column(_("out"), "prop_out",
                             sorter=Gtk.NumericSorter.new(expr("prop_out")))
        self._add_num_column(_("latency"), "prop_latency",
                             sorter=Gtk.NumericSorter.new(
                                 expr("prop_latency")))
        self._add_status_column(_("status"))

        self.sort_model = Gtk.SortListModel(model=self.items,
                                            sorter=self.view.get_sorter())
        self.view.set_model(Gtk.SingleSelection(model=self.sort_model))

        # ColumnView 必须放进 ScrolledWindow 才渲染表头（GTK4 惯例）
        self.view.set_show_row_separators(True)
        scrolled = Gtk.ScrolledWindow(vexpand=True, hexpand=True)
        scrolled.set_policy(Gtk.PolicyType.NEVER, Gtk.PolicyType.AUTOMATIC)
        scrolled.set_min_content_height(320)
        scrolled.set_child(self.view)
        scrolled.add_css_class("card")
        scrolled.add_css_class("table-card")
        parent.append(scrolled)

    def _add_text_column(self, title, prop, width, sorter=None):
        def setup(_f, li):
            label = Gtk.Label(xalign=0, ellipsize=Pango.EllipsizeMode.END)
            label.add_css_class("dim-label")
            li.set_child(label)

        def bind(_f, li):
            li.get_child().set_text(str(getattr(li.get_item(), prop)))

        factory = Gtk.SignalListItemFactory()
        factory.connect("setup", setup)
        factory.connect("bind", bind)
        col = Gtk.ColumnViewColumn(title=title, factory=factory,
                                   sorter=sorter, resizable=True)
        col.set_fixed_width(width)
        self.view.append_column(col)
        return col

    def _add_num_column(self, title, prop, sorter=None):
        def setup(_f, li):
            label = Gtk.Label(xalign=1.0)
            li.set_child(label)

        def bind(_f, li):
            value = getattr(li.get_item(), prop)
            li.get_child().set_text(
                fmt_latency(value) if title == _("latency") else fmt_int(value))

        factory = Gtk.SignalListItemFactory()
        factory.connect("setup", setup)
        factory.connect("bind", bind)
        col = Gtk.ColumnViewColumn(title=title, factory=factory,
                                   sorter=sorter, resizable=True)
        col.set_fixed_width(80)
        self.view.append_column(col)
        return col

    def _add_status_column(self, title):
        def setup(_f, li):
            box = Gtk.Box(orientation=Gtk.Orientation.HORIZONTAL, spacing=5)
            box.set_halign(Gtk.Align.END)
            li._status_box = box
            li.set_child(box)

        def bind(_f, li):
            item = li.get_item()
            box = li._status_box
            child = box.get_first_child()
            while child is not None:
                nxt = child.get_next_sibling()
                box.remove(child)
                child = nxt
            box.append(ColorDot("error" if item.row.is_error else "success"))
            label = Gtk.Label(label=item.row.error_kind or item.row.status,
                              ellipsize=Pango.EllipsizeMode.END)
            label.set_tooltip_text(item.row.status)
            box.append(label)

        factory = Gtk.SignalListItemFactory()
        factory.connect("setup", setup)
        factory.connect("bind", bind)
        col = Gtk.ColumnViewColumn(title=title, factory=factory, resizable=True)
        col.set_fixed_width(110)
        self.view.append_column(col)

    def _on_row_activated(self, view, position):
        item = self.sort_model.get_item(position)
        if item is not None:
            LogDetailDialog(self.win, item.row).present(self.win.window)

    # ------------------------------------------------------------------
    # 分页
    # ------------------------------------------------------------------
    def _build_pager(self, parent):
        box = Gtk.Box(orientation=Gtk.Orientation.HORIZONTAL, spacing=8,
                      halign=Gtk.Align.CENTER,
                      margin_top=2, margin_bottom=14)
        self.prev_button = Gtk.Button(icon_name="go-previous-symbolic")
        self.prev_button.set_tooltip_text(_("Previous"))
        self.prev_button.connect("clicked", lambda _b: self._turn_page(-1))
        self.next_button = Gtk.Button(icon_name="go-next-symbolic")
        self.next_button.set_tooltip_text(_("Next"))
        self.next_button.connect("clicked", lambda _b: self._turn_page(1))
        self.pager_label = Gtk.Label(label="—")
        self.pager_label.add_css_class("dim-label")
        box.append(self.prev_button)
        box.append(self.pager_label)
        box.append(self.next_button)
        parent.append(box)

    def _turn_page(self, delta):
        self._page = max(0, self._page + delta)
        self.refresh()

    def _on_filters_changed(self):
        if not self._ready:
            return          # 构造期（目录填充/选中初始化）触发的信号：忽略
        self._page = 0
        self.refresh()

    # ------------------------------------------------------------------
    def _current_filters(self):
        def sel(drop):
            idx = drop.get_selected()
            return None if idx <= 0 else drop.get_model().get_string(idx)

        status_idx = self.status_drop.get_selected()
        return {
            "ident": sel(self.ident_drop),
            "model": sel(self.model_drop),
            "source": sel(self.source_drop),
            "search": None,
            "errors": (True if status_idx == 2
                       else False if status_idx == 1 else None),
        }

    def refresh(self):
        hours = WINDOW_CHOICES[self.window_drop.get_selected()][0]
        filters = self._current_filters()
        try:
            rows, total = self.backend.usage_logs(
                hours, self._page * PAGE_SIZE, PAGE_SIZE, filters)
        except BackendError:
            rows, total = [], 0
        self._total = total
        now = time.time()
        self.items.splice(0, self.items.get_n_items(),
                          [LogItem(r, now) for r in rows])
        shown = self.items.get_n_items()
        start = self._page * PAGE_SIZE + 1 if total else 0
        self.pager_label.set_text(
            _("%d–%d of %d") % (start, start + shown - 1, total)
            if total else _("No matching requests"))
        self.prev_button.set_sensitive(self._page > 0)
        self.next_button.set_sensitive(
            (self._page + 1) * PAGE_SIZE < total)

    def row_count(self):
        return self._total


class LogDetailDialog(Adw.Dialog):
    """单条调用日志的完整详情。"""

    def __init__(self, win, row):
        super().__init__(title=_("Request details"))
        self.set_size_request(460, -1)
        page = Adw.PreferencesPage()
        toolbar = Adw.ToolbarView()
        toolbar.add_top_bar(Adw.HeaderBar())
        scrolled = Gtk.ScrolledWindow(height_request=360)
        scrolled.set_child(page)
        toolbar.set_content(scrolled)
        self.set_child(toolbar)

        def yes_no(value):
            return _("yes") if value else _("no")

        def group(title):
            g = Adw.PreferencesGroup(title=title)
            page.add(g)
            return g

        def add(g, title, value):
            r = Adw.ActionRow(title=title, subtitle=str(value))
            g.add(r)
            return r

        main = group(_("Summary"))
        local = datetime.fromtimestamp(row.ts).strftime("%Y-%m-%d %H:%M:%S")
        add(main, _("Status"),
            row.error_kind or row.status)
        add(main, _("Time"), local)
        add(main, _("ident"), row.ident + (
            " · %s" % row.agent if row.agent else ""))
        add(main, _("model"), "%s → %s" % (
            row.model, row.upstream_model or row.model))
        add(main, _("source"), row.source or "—")

        tokens = group(_("Tokens"))
        add(tokens, _("in"), fmt_tokens(row.in_tokens))
        add(tokens, _("out"), fmt_tokens(row.out_tokens))
        add(tokens, _("cache read"), fmt_tokens(row.cache_read))
        add(tokens, _("cache write"), fmt_tokens(row.cache_write))
        add(tokens, _("Reasoning"), fmt_tokens(row.reasoning))
        add(tokens, _("Cost"), fmt_cost(row.cost))

        perf = group(_("Performance"))
        add(perf, _("Latency"), fmt_latency(row.latency_ms))
        add(perf, _("TTFT"), fmt_latency(row.ttft_ms) if row.ttft_ms else "—")
        add(perf, _("Streaming"), yes_no(row.stream))
        add(perf, _("Degraded"), yes_no(row.degraded))

        meta = group(_("Metadata"))
        add(meta, _("Request ID"), row.req_id or "—")
        add(meta, _("Client format"), row.client_format or "—")
        if row.session:
            add(meta, _("Session"), row.session)
        if row.error_kind:
            add(meta, _("Error kind"), row.error_kind)
