#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""模型源页：源列表（健康灯/优先级/权重）+ 行 kebab 菜单（编辑/测试/删除）
+ 详情弹层（摘要 + 编辑入口 + TestSource）+ 添加/编辑共用表单弹层。

表单（new-api 风格两组模型配置）：
  「模型」组——主交互，简单到不需要解释：底部 Entry（回车/按钮添加），
  每行 模型名 + 行尾 flat trash；语义即 alias=upstream=模型名，
  界面不出现“映射/别名”字眼。
  「自定义映射（可选）」组——仅当同一模型在上游叫不同名字时才需要；
  「添加映射」ButtonRow + 行 [别名 entry → 上游 entry + trash]。
  _collect() 合并两组，跨组别名去重报错；编辑预填按 alias==upstream 分流。
添加与编辑走同一个 SourceFormDialog；API 密钥留空（编辑模式）= 保持现有。"""

import gi

try:
    gi.require_version("Adw", "1.0")
except ValueError:
    gi.require_version("Adw", "1")
gi.require_version("Gtk", "4.0")
from gi.repository import Adw, Gtk  # noqa: E402

from widgets import (HealthDot, HEALTH_LABELS, button_row, menu_button,  # noqa: E402
                     make_entry_row, make_password_row, make_spin_row,
                     make_switch_row, confirm_async)
from i18n import _  # noqa: E402
from backend import (SOURCE_TYPES, SOURCE_TYPE_LABELS,  # noqa: E402
                     BackendError)

ROW_MARGIN = 2          # 行距修复：动态行上下 margin，避免视觉粘连


class SourcesPage:
    name = "sources"
    title = "Sources"
    icon = "network-server-symbolic"

    def __init__(self, win, backend):
        self.win = win
        self.backend = backend
        self.widget = Gtk.ScrolledWindow(vexpand=True)
        clamp = Adw.Clamp(maximum_size=680)
        box = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=12,
                      margin_top=18, margin_bottom=18,
                      margin_start=12, margin_end=12)
        clamp.set_child(box)
        self.widget.set_child(clamp)

        self.group = Adw.PreferencesGroup(
            title=_("Sources"),
            description=_("Upstream aggregation: each source maps aliases "
                          "to upstream models"))
        self.add_button = Gtk.Button(icon_name="list-add-symbolic",
                                     valign=Gtk.Align.CENTER)
        self.add_button.set_tooltip_text(_("Add source"))
        self.add_button.connect("clicked", self._on_add_clicked)
        self.group.props.header_suffix = self.add_button
        box.append(self.group)

        self._rows = {}      # source.id -> Adw.ActionRow
        self._sources = {}   # source.id -> Source
        self.refresh()

    # ------------------------------------------------------------------
    def refresh(self):
        try:
            sources = self.backend.list_sources()
        except BackendError:
            sources = list(self._sources.values())
        self._sources = {s.id: s for s in sources}
        for sid in list(self._rows):
            if sid not in self._sources:
                self.group.remove(self._rows.pop(sid))
        for source in list(self._sources.values()):
            row = self._rows.get(source.id)
            if row is None:
                row = self._make_row(source)
                self._rows[source.id] = row
            self._update_row(row, source)

    def _make_row(self, source):
        row = Adw.ActionRow()
        row.set_activatable(True)
        dot = HealthDot()
        row.add_prefix(dot)
        menu = menu_button([
            (_("Edit…"), "document-edit-symbolic",
             lambda _b, sid=source.id: self._open_edit(sid), None),
            (_("Test connectivity"), "network-transmit-symbolic",
             lambda _b, sid=source.id: self._test_source(sid), None),
            (_("Remove source"), "user-trash-symbolic",
             lambda _b, sid=source.id: self._confirm_remove(sid),
             "destructive-action"),
        ])
        row.add_suffix(menu)
        row.connect("activated", lambda _r, sid=source.id:
                    self._open_detail(sid))
        self.group.add(row)
        row._dot = dot
        return row

    def _update_row(self, row, source):
        row.set_title(source.name)
        key_mark = _("API key set") if source.has_key else _("no key")
        row.set_subtitle("%s · %s" % (
            SOURCE_TYPE_LABELS.get(source.type, source.type),
            _("priority %d · weight %d · %s%s") % (
                source.priority, source.weight, key_mark,
                _(" · disabled") if not source.enabled else "")))
        row._dot.set_state(source.health.state)

    # ------------------------------------------------------------------
    def row_count(self):
        return len(self._rows)

    def _on_add_clicked(self, button):
        self._open_form(None)

    def _open_edit(self, source_id):
        source = self._sources.get(source_id)
        if source is not None:
            self._open_form(source)

    def _open_form(self, source):
        dialog = SourceFormDialog(self.win, self.backend, edit_source=source,
                                  on_saved=self.refresh)
        dialog.present(self.win.window)

    def _test_source(self, source_id):
        source = self._sources.get(source_id)
        name = source.name if source else str(source_id)
        try:
            result = self.backend.test_source(source_id)
        except BackendError as err:
            self.win.toast(_("Test of %s failed: %s") % (name, err.message))
            return
        if result.get("ok"):
            self.win.toast(_("%s reachable, %d ms latency")
                           % (name, result.get("latency_ms", 0)))
        else:
            self.win.toast(_("Probe failed: %s")
                           % (result.get("detail", "")))

    def _confirm_remove(self, source_id):
        source = self._sources.get(source_id)
        if source is None:
            return

        def done(ok):
            if not ok:
                return
            try:
                self.backend.remove_source(source.id)
                self.win.toast(_("Deleted source %s") % source.name)
            except BackendError as err:
                self.win.toast(_("Delete failed: %s") % err.message)

        confirm_async(self.win.window, _("Delete source?"),
                      _("Delete “%s”? Keys in use will no longer route to "
                        "this source.") % source.name,
                      _("Delete"), done, destructive=True)

    # ------------------------------------------------------------------
    # 详情弹层
    # ------------------------------------------------------------------
    def _open_detail(self, source_id):
        dialog = SourceDetailDialog(self.win, self.backend, source_id,
                                    on_edit=self._open_edit)
        dialog.present(self.win.window)


class SourceDetailDialog(Adw.Dialog):
    """单源详情摘要：编辑入口 + 启用开关 + TestSource + 删除。"""

    def __init__(self, win, backend, source_id, on_edit=None):
        super().__init__(title="")
        self.win = win
        self.backend = backend
        self.on_edit = on_edit
        self.set_size_request(420, -1)
        try:
            sources = backend.list_sources()
        except BackendError:
            sources = []
        self.source = next((s for s in sources if s.id == source_id), None)
        self.build()

    def build(self):
        src = self.source
        if src is None:
            self.set_child(Adw.StatusPage(
                title=_("Source not found"),
                icon_name="dialog-warning-symbolic"))
            return
        toolbar = Adw.ToolbarView()
        header = Adw.HeaderBar()
        header.set_title_widget(
            Adw.WindowTitle(title=src.name, subtitle=src.base_url))
        toolbar.add_top_bar(header)
        scrolled = Gtk.ScrolledWindow(height_request=360)
        clamp = Adw.Clamp(maximum_size=560)
        page = Adw.PreferencesPage()
        clamp.set_child(page)
        scrolled.set_child(clamp)
        toolbar.set_content(scrolled)
        self.set_child(toolbar)

        actions = Adw.PreferencesGroup()
        actions.add(button_row(
            _("Edit…"), self._on_edit, icon="document-edit-symbolic",
            css_class="suggested-action"))
        page.add(actions)

        info = Adw.PreferencesGroup(title=_("Basic information"))
        health_text = _(HEALTH_LABELS.get(src.health.state, "unknown"))
        if src.health.fails or src.health.cooling_until:
            health_text = _("%s (%d consecutive failures%s)") % (
                health_text, src.health.fails,
                _(", cooling until %s") % src.health.cooling_until
                if src.health.cooling_until else "")
        for title, value in (
                (_("Type"), SOURCE_TYPE_LABELS.get(src.type, src.type)),
                (_("Base URL"), src.base_url or "—"),
                (_("API key"),
                 _("Configured") if src.has_key else _("Not configured")),
                (_("Priority / weight"),
                 "%d / %d" % (src.priority, src.weight)),
                (_("Health"), health_text)):
            info.add(Adw.ActionRow(title=title, subtitle=str(value)))
        switch_row = make_switch_row(_("Enabled"), src.enabled)
        switch_row.connect("notify::active", self._on_enabled_toggled)
        info.add(switch_row)
        page.add(info)

        mapping = Adw.PreferencesGroup(
            title=_("Model mappings"), description=_("alias = upstream"))
        if src.models:
            for m in src.models:
                mapping.add(Adw.ActionRow(title=m.alias, subtitle=m.upstream))
        else:
            mapping.add(Adw.ActionRow(title=_("(no mappings)")))
        page.add(mapping)

        more = Adw.PreferencesGroup()
        more.add(button_row(_("Test connectivity"), self._on_test,
                            icon="network-transmit-symbolic"))
        more.add(button_row(_("Remove source"), self._on_remove,
                            icon="user-trash-symbolic",
                            css_class="destructive-action"))
        page.add(more)

    # ------------------------------------------------------------------
    def _on_edit(self, row):
        if self.source is None or self.on_edit is None:
            return
        self.close()
        self.on_edit(self.source.id)

    def _on_enabled_toggled(self, row, pspec):
        if self.source is None:
            return
        try:
            self.backend.update_source(
                self.source.id, {"enabled": row.get_active()})
            self.win.toast(_("%s enabled" if row.get_active()
                             else "%s disabled") % self.source.name)
        except BackendError as err:
            row.set_active(not row.get_active())
            self.win.toast(_("Update failed: %s") % err.message)

    def _on_test(self, row):
        if self.source is None:
            return
        try:
            result = self.backend.test_source(self.source.id)
        except BackendError as err:
            self.win.toast(_("Test of %s failed: %s")
                           % (self.source.name, err.message))
            return
        if result.get("ok"):
            self.win.toast(_("%s reachable, %d ms latency")
                           % (self.source.name, result.get("latency_ms", 0)))
        else:
            self.win.toast(_("Probe failed: %s")
                           % (result.get("detail", "")))

    def _on_remove(self, row):
        if self.source is None:
            return
        name = self.source.name
        sid = self.source.id

        def done(ok):
            if not ok:
                return
            try:
                self.backend.remove_source(sid)
                self.win.toast(_("Deleted source %s") % name)
            except BackendError as err:
                self.win.toast(_("Delete failed: %s") % err.message)
                return
            self.close()

        confirm_async(self.win.window, _("Delete source?"),
                      _("Delete “%s”? Keys in use will no longer route to "
                        "this source.") % name,
                      _("Delete"), done, destructive=True)


class SourceFormDialog(Adw.PreferencesDialog):
    """添加/编辑共用表单（两组模型配置，见模块 docstring）。

    添加模式：edit_source=None → add_source；
    编辑模式：预填全部字段（模型按 alias==upstream 分流到两组），
    密钥留空 = 保持现有（update_source 逐字段 PATCH）。
    """

    def __init__(self, win, backend, edit_source=None, on_saved=None):
        self.editing = edit_source is not None
        super().__init__(title=_("Edit source") if self.editing
                         else _("Add source"))
        self.win = win
        self.backend = backend
        self.source = edit_source
        self.on_saved = on_saved
        self._model_rows = []     # [(Adw.ActionRow, name)]
        self._map_rows = []       # [dict(row, alias_entry, upstream_entry)]
        self.set_size_request(470, -1)
        self.build()

    # ------------------------------------------------------------------
    def build(self):
        src = self.source
        page = Adw.PreferencesPage()
        self.add(page)

        # ---- 基本信息 --------------------------------------------------
        basic = Adw.PreferencesGroup(title=_("Basic information"))
        self.name_row = make_entry_row(_("Name (unique)"),
                                       text=src.name if src else "")
        basic.add(self.name_row)

        self.type_row = Adw.ComboRow(title=_("Type"),
                                     subtitle=_("Upstream protocol"))
        self.type_row.set_model(Gtk.StringList(
            strings=[SOURCE_TYPE_LABELS[k] for k in SOURCE_TYPES]))
        self.type_row.set_expression(
            Gtk.PropertyExpression.new(Gtk.StringObject, None, "string"))
        if src and src.type in SOURCE_TYPES:
            self.type_row.set_selected(SOURCE_TYPES.index(src.type))
        basic.add(self.type_row)

        self.base_row = Adw.EntryRow(
            title=_("Base URL, e.g. https://api.example.com/v1"))
        if src and src.base_url:
            self.base_row.set_text(src.base_url)
        basic.add(self.base_row)

        if self.editing:
            self.key_row = Adw.PasswordEntryRow(
                title=_("New API key (leave empty to keep the current one)"))
        else:
            self.key_row = make_password_row(_("API key"))
        basic.add(self.key_row)
        page.add(basic)

        # ---- 调度 --------------------------------------------------------
        schedule = Adw.PreferencesGroup(
            title=_("Scheduling"),
            description=_("Higher priority number wins; a failed tier "
                          "falls through to the next"))
        self.priority_row = make_spin_row(
            _("Priority"), src.priority if src else 0, 0, 1000, 1,
            _("Higher number wins; equal numbers share a tier"))
        schedule.add(self.priority_row)
        self.weight_row = make_spin_row(
            _("Weight"), src.weight if src else 0, 0, 1000, 1,
            _("Share of weighted random picks within a tier "
              "(effective weight = weight + 10)"))
        schedule.add(self.weight_row)
        self.enabled_row = make_switch_row(_("Enabled"),
                                           src.enabled if src else True)
        schedule.add(self.enabled_row)
        page.add(schedule)

        # ---- 模型组（主交互：不需要解释的简单组） ---------------------------
        self.models_group = Adw.PreferencesGroup(
            title=_("Models"),
            description=_("Model names offered through this source"))
        entry_row = Adw.ActionRow()
        self.model_entry = Gtk.Entry(
            placeholder_text=_("Type a model name and press Enter"),
            hexpand=True, valign=Gtk.Align.CENTER)
        self.model_entry.connect("activate", lambda _e: self._add_model_name())
        entry_row.add_suffix(self.model_entry)
        add_button = Gtk.Button(label=_("Add"), valign=Gtk.Align.CENTER)
        add_button.add_css_class("suggested-action")
        add_button.connect("clicked", lambda _b: self._add_model_name())
        entry_row.add_suffix(add_button)
        self.models_group.add(entry_row)
        page.add(self.models_group)

        # ---- 自定义映射组（可选） ------------------------------------------
        self.mapping_group = Adw.PreferencesGroup(
            title=_("Custom mappings (optional)"),
            description=_("Only needed when the same model is named "
                          "differently upstream (public alias → actual "
                          "upstream model name)"))
        self.mapping_group.add(button_row(
            _("Add mapping"), lambda _r: self._add_mapping_row("", ""),
            icon="list-add-symbolic"))
        page.add(self.mapping_group)

        # ---- 编辑预填：alias==upstream → 模型组，不等则映射组 --------------
        if src and src.models:
            for m in src.models:
                if m.alias == m.upstream:
                    self._add_model_name(m.alias, quiet=True)
                else:
                    self._add_mapping_row(m.alias, m.upstream)

        # ---- 提交 ----------------------------------------------------------
        submit_group = Adw.PreferencesGroup()
        submit_group.add(button_row(
            _("Save changes") if self.editing else _("Add"),
            self._on_submit, icon="object-select-symbolic",
            css_class="suggested-action"))
        page.add(submit_group)

    # ------------------------------------------------------------------
    # 模型组行管理
    # ------------------------------------------------------------------
    def _add_model_name(self, name=None, quiet=False):
        """从 Entry（name=None）或程序化添加一个模型行；组内重名给提示。"""
        from_entry = name is None
        if from_entry:
            name = self.model_entry.get_text().strip()
        else:
            name = str(name).strip()
        if not name:
            return False
        if any(existing == name for _row, existing in self._model_rows):
            if not quiet:
                self.win.toast(_("Duplicate model name: %s") % name)
            return False
        row = Adw.ActionRow(title=name, margin_top=ROW_MARGIN,
                            margin_bottom=ROW_MARGIN)
        trash = Gtk.Button(icon_name="user-trash-symbolic",
                           valign=Gtk.Align.CENTER)
        trash.add_css_class("flat")
        trash.set_tooltip_text(_("Remove this model"))
        trash.connect("clicked", self._make_model_remover(name))
        row.add_suffix(trash)
        self.models_group.add(row)
        self._model_rows.append((row, name))
        if from_entry:
            self.model_entry.set_text("")
        return True

    def _make_model_remover(self, name):
        def remove(_button):
            for i, (row, existing) in enumerate(list(self._model_rows)):
                if existing == name:
                    self.models_group.remove(row)
                    self._model_rows.pop(i)
                    return
        return remove

    # ------------------------------------------------------------------
    # 自定义映射组行管理
    # ------------------------------------------------------------------
    def _add_mapping_row(self, alias, upstream):
        row = Adw.ActionRow(title=_("Custom mapping"),
                            margin_top=ROW_MARGIN, margin_bottom=ROW_MARGIN)
        box = Gtk.Box(orientation=Gtk.Orientation.HORIZONTAL, spacing=6,
                      valign=Gtk.Align.CENTER)
        alias_entry = Gtk.Entry(placeholder_text=_("alias"),
                                width_chars=12, hexpand=True)
        alias_entry.set_text(alias or "")
        arrow = Gtk.Label(label="→", valign=Gtk.Align.CENTER)
        arrow.add_css_class("dim-label")
        upstream_entry = Gtk.Entry(placeholder_text=_("upstream"),
                                   width_chars=12, hexpand=True)
        upstream_entry.set_text(upstream or "")
        trash = Gtk.Button(icon_name="user-trash-symbolic",
                           valign=Gtk.Align.CENTER)
        trash.add_css_class("flat")
        trash.set_tooltip_text(_("Remove this mapping"))
        trash.connect("clicked", lambda _b: self._remove_mapping_row(row))
        box.append(alias_entry)
        box.append(arrow)
        box.append(upstream_entry)
        box.append(trash)
        row.add_suffix(box)
        self.mapping_group.add(row)
        record = {"row": row, "alias": alias_entry,
                  "upstream": upstream_entry}
        self._map_rows.append(record)
        return record

    def _remove_mapping_row(self, row):
        self.mapping_group.remove(row)
        self._map_rows = [m for m in self._map_rows if m["row"] is not row]

    # ------------------------------------------------------------------
    # 收集 / 提交
    # ------------------------------------------------------------------
    def _collect(self):
        """合并两组 → (spec, err_message)。跨组别名去重 → err。"""
        name = self.name_row.get_text().strip()
        if not name:
            return None, _("Name must not be empty")
        base_url = self.base_row.get_text().strip()
        if not base_url:
            return None, _("Base URL must not be empty")
        models, seen = [], set()
        # 模型组：alias = upstream = 模型名
        for _row, model_name in self._model_rows:
            model_name = model_name.strip()
            if not model_name:
                continue
            if model_name in seen:
                return None, _("Duplicate model name: %s") % model_name
            seen.add(model_name)
            models.append({"alias": model_name, "upstream": model_name})
        # 自定义映射组：显式 别名 → 上游
        for record in self._map_rows:
            alias = record["alias"].get_text().strip()
            if not alias:
                continue
            if alias in seen:
                return None, _("Duplicate alias across groups: %s") % alias
            seen.add(alias)
            upstream = record["upstream"].get_text().strip() or alias
            models.append({"alias": alias, "upstream": upstream})
        if not models:
            return None, _("At least one model is required")
        spec = {
            "name": name,
            "type": SOURCE_TYPES[self.type_row.get_selected()],
            "base_url": base_url,
            "models": models,
            "priority": int(self.priority_row.get_value()),
            "weight": int(self.weight_row.get_value()),
            "enabled": self.enabled_row.get_active(),
        }
        # 密钥：添加模式总是携带（可为空）；编辑模式仅当用户输入了新值
        key = self.key_row.get_text()
        if not self.editing or key:
            spec["api_key"] = key
        return spec, None

    def _on_submit(self, row):
        spec, err = self._collect()
        if err:
            self.win.toast(err)
            return
        try:
            if self.editing:
                self.backend.update_source(self.source.id, spec)
                self.win.toast(_("Changes to %s saved") % spec["name"])
            else:
                source = self.backend.add_source(spec)
                self.win.toast(_("Added source %s") % source.name)
        except BackendError as e:
            self.win.toast(_("Save failed: %s") % e.message)
            return
        if self.on_saved is not None:
            self.on_saved()
        self.close()
