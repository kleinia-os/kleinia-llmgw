#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""模型定价页：别名目录 + 每模型定价（USD / 1M tokens，四分量按需启用）。

每行一个别名：哪些源提供、多少把活跃密钥可调用、当前定价
（未定价的模型在用量页不计成本）。点击行弹定价编辑器：
输入/输出/缓存读/缓存写四个分量，各自带启用开关——只配置该模型
实际计费的部分。价格写入 llmgwd 后立即生效（后续请求开始记账
成本）并持久化到 prices.json。

计费语义（与守护进程 server.Pricer 一致）：缓存读是输入 token 的
切出量（启用缓存读价后按自身单价计，未启用则并入输入价）；
输出与缓存写是独立维度，未启用不计费。
"""

import gi

try:
    gi.require_version("Adw", "1.0")
except ValueError:
    gi.require_version("Adw", "1")
gi.require_version("Gtk", "4.0")
from gi.repository import Adw, Gtk  # noqa: E402

from widgets import button_row, confirm_async  # noqa: E402
from i18n import _  # noqa: E402
from backend import BackendError  # noqa: E402

# (字段名, 展示名 msgid, 默认是否启用)
COMPONENTS = (
    ("input_per_mtok", "input", True),
    ("output_per_mtok", "output", True),
    ("cache_read_per_mtok", "cache read", False),
    ("cache_write_per_mtok", "cache write", False),
)


def fmt_price(value):
    """$/1M → "0.14"；0 → "0"（免费也是定价）。"""
    if value is None:
        return "—"
    if float(value) == int(value):
        return "%d" % int(value)
    return "%.4g" % float(value)


def price_summary(price):
    """启用分量的拼接："$0.14 / 1M input · $0.014 / 1M cache read"。"""
    if price is None:
        return None
    parts = []
    for field_name, label, _default in COMPONENTS:
        value = getattr(price, field_name)
        if value is not None:
            parts.append(_("$%s / 1M %s") % (fmt_price(value), _(label)))
    return " · ".join(parts) if parts else None


class ModelsPage:
    name = "models"
    title = "Models"
    icon = "package-x-generic-symbolic"

    def __init__(self, win, backend):
        self.win = win
        self.backend = backend
        self.widget = Gtk.ScrolledWindow(vexpand=True)
        clamp = Adw.Clamp(maximum_size=680)
        self.box = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=12,
                           margin_top=18, margin_bottom=18,
                           margin_start=12, margin_end=12)
        clamp.set_child(self.box)
        self.widget.set_child(clamp)

        self.group = Adw.PreferencesGroup(
            title=_("Model catalog and pricing"),
            description=_("Click a model to set its USD price per 1M "
                          "tokens; priced models get cost estimates on "
                          "the Usage page"))
        self.status_page = Adw.StatusPage(
            title=_("No models yet"),
            description=_("Add a source with model mappings on the Sources "
                          "page and the catalog will appear here."),
            icon_name="package-x-generic-symbolic")
        self.box.append(self.group)
        self.box.append(self.status_page)
        self._alias_rows = 0
        self._row_widgets = []
        self.refresh()

    # ------------------------------------------------------------------
    def row_count(self):
        return self._alias_rows

    def refresh(self):
        try:
            aliases = self.backend.alias_sources()
        except BackendError:
            aliases = {}
        try:
            keys = self.backend.list_keys()
        except BackendError:
            keys = []
        try:
            prices = {p.alias: p for p in self.backend.list_prices()}
        except BackendError:
            prices = {}
        # PreferencesGroup 内部含结构子件，不能遍历直接子控件；
        # 只移除我们自己添加过的行。
        for row in self._row_widgets:
            self.group.remove(row)
        self._row_widgets = []
        if not aliases:
            self.group.set_visible(False)
            self.status_page.set_visible(True)
            self._alias_rows = 0
            return
        self.group.set_visible(True)
        self.status_page.set_visible(False)
        self._alias_rows = 0
        for alias, names in aliases.items():
            allowed = sum(
                1 for k in keys
                if k.status == "active"
                and ("*" in (k.models or ["*"]) or alias in k.models))
            summary = price_summary(prices.get(alias))
            if summary is not None:
                price_text, icon = summary, "emblem-default-symbolic"
            else:
                price_text, icon = _("unpriced"), "dialog-question-symbolic"
            row = Adw.ActionRow(title=alias, activatable=True)
            row.set_subtitle("%s · %s" % (
                _("%d source(s) · %d key(s) can call")
                % (len(names), allowed), price_text))
            row.add_prefix(Gtk.Image(icon_name=icon))
            tooltip = _("Set price") if summary is None else _("Edit price")
            row.set_tooltip_text(tooltip)
            row.connect("activated", lambda _r, a=alias: self._open_price(a))
            self.group.add(row)
            self._row_widgets.append(row)
            self._alias_rows += 1

    # ------------------------------------------------------------------
    def _open_price(self, alias):
        prices = {p.alias: p for p in self.backend.list_prices()}
        dialog = PriceDialog(self.win, self.backend, alias,
                             prices.get(alias))
        dialog.connect("closed", lambda *a: self.refresh())
        dialog.present(self.win.window)


class PriceDialog(Adw.PreferencesDialog):
    """单模型定价编辑：四分量 ×（启用开关 + 单价输入）、保存、清除定价。"""

    def __init__(self, win, backend, alias, price=None):
        super().__init__(title=_("Price: %s") % alias)
        self.win = win
        self.backend = backend
        self.alias = alias
        self.set_size_request(480, -1)
        page = Adw.PreferencesPage()
        self.add(page)

        group = Adw.PreferencesGroup(
            title=alias,
            description=_("Enable only the components this model bills; "
                          "cache read is a carve-out of input tokens"))
        self.rows = {}      # field_name -> (switch, entry)
        for field_name, label, default_on in COMPONENTS:
            current = getattr(price, field_name) if price is not None else None
            row = Adw.ActionRow(title=_(label))
            entry = Gtk.Entry(width_chars=9, hexpand=False,
                              valign=Gtk.Align.CENTER,
                              input_purpose=Gtk.InputPurpose.NUMBER)
            entry.set_text(fmt_price(current if current is not None else 0))
            switch = Gtk.Switch(valign=Gtk.Align.CENTER)
            switch.set_active(current is not None if price is not None
                              else default_on)
            def on_toggle(sw, _pspec, en=entry):
                en.set_sensitive(sw.get_active())
            switch.connect("notify::active", on_toggle)
            entry.set_sensitive(switch.get_active())
            row.add_suffix(entry)
            row.add_suffix(switch)
            group.add(row)
            self.rows[field_name] = (switch, entry)

        page.add(group)

        # 提交按钮独立分组：与输入行同组会被 boxed-list 连成一整块
        save_group = Adw.PreferencesGroup()
        save_group.add(button_row(
            _("Save changes"), self._on_save,
            icon="object-select-symbolic", css_class="suggested-action"))
        page.add(save_group)

        if price is not None:
            remove_group = Adw.PreferencesGroup()
            remove_group.add(button_row(
                _("Remove price"), self._on_remove,
                icon="user-trash-symbolic", css_class="destructive-action"))
            page.add(remove_group)

    def _collect(self):
        """→ (components dict, err)。开关关=分量未启用（不出现在 dict）。"""
        components = {}
        for field_name, label, _default in COMPONENTS:
            switch, entry = self.rows[field_name]
            if not switch.get_active():
                continue
            text = entry.get_text().strip().replace("$", "")
            try:
                value = float(text)
            except ValueError:
                return None, _("Prices must be non-negative numbers, "
                               "e.g. 0.14")
            if value < 0:
                return None, _("Prices must be non-negative numbers, "
                               "e.g. 0.14")
            components[field_name] = value
        if not components:
            return None, _("At least one price component must be enabled")
        return components, None

    def _on_save(self, row):
        components, err = self._collect()
        if err:
            self.win.toast(err)
            return
        try:
            self.backend.set_price(self.alias, components)
        except BackendError as exc:
            self.win.toast(_("Update failed: %s") % exc.message)
            return
        self.win.toast(_("Price saved: %s") % self.alias)
        self.close()

    def _on_remove(self, row):
        def done(ok):
            if not ok:
                return
            try:
                self.backend.remove_price(self.alias)
            except BackendError as err:
                self.win.toast(_("Update failed: %s") % err.message)
                return
            self.win.toast(_("Price removed: %s") % self.alias)
            self.close()

        confirm_async(self.win.window, _("Remove price?"),
                      _("Cost estimates for %s will stop (usage history is "
                        "kept)") % self.alias, _("Remove"), done,
                      destructive=True)
