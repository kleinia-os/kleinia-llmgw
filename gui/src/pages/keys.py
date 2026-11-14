#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""应用与密钥页：key 列表 + 发放/编辑密钥（secret 只显示一次）
+ 挂起/恢复/吊销（操作后立即刷新，真实后端变更无广播信号）。

行布局：状态点+状态字（active/suspended/revoked）在标题行尾部，
副标题按「模型 · 限额 · AID」分字段展示，不再拼一长串句子。
"""

import gi

try:
    gi.require_version("Adw", "1.0")
except ValueError:
    gi.require_version("Adw", "1")
gi.require_version("Gtk", "4.0")
from gi.repository import Adw, Gdk, Gtk  # noqa: E402

from widgets import (button_row, make_entry_row, make_spin_row,  # noqa: E402
                     make_textview_row, parse_name_lines, confirm_async,
                     ColorDot)
from i18n import _  # noqa: E402
from backend import BackendError  # noqa: E402

# 状态 msgid（_() 翻译；zh_CN 表覆盖）
STATUS_LABELS = {"active": "active", "suspended": "suspended",
                 "revoked": "revoked"}
STATUS_CLASSES = {"active": "success", "suspended": "warning",
                  "revoked": "error"}


class KeysPage:
    name = "keys"
    title = "Apps and Keys"
    icon = "system-lock-screen-symbolic"

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
            title=_("Apps and Keys"),
            description=_("Each app/agent holds one sk-kln-* key; model "
                          "allowlists and quotas apply per key"))
        self.issue_button = Gtk.Button(label=_("Issue key"),
                                       icon_name="list-add-symbolic",
                                       valign=Gtk.Align.CENTER)
        self.issue_button.add_css_class("suggested-action")
        self.issue_button.connect("clicked", self._on_issue_clicked)
        self.group.props.header_suffix = self.issue_button
        box.append(self.group)

        self._rows = {}
        self._keys = {}
        self.refresh()

    # ------------------------------------------------------------------
    def refresh(self):
        try:
            keys = self.backend.list_keys()
        except BackendError:
            keys = list(self._keys.values())
        self._keys = {k.id: k for k in keys}
        for kid in list(self._rows):
            if kid not in self._keys:
                self.group.remove(self._rows.pop(kid))
        for key in list(self._keys.values()):
            row = self._rows.get(key.id)
            if row is None:
                row = self._make_row(key)
                self._rows[key.id] = row
            self._update_row(row, key)

    def row_count(self):
        return len(self._rows)

    def _make_row(self, key):
        row = Adw.ActionRow()
        row.set_title_lines(1)
        row.set_subtitle_lines(1)
        row.add_prefix(Gtk.Image(icon_name="system-lock-screen-symbolic"))
        self.group.add(row)
        row._suffix_box = Gtk.Box(orientation=Gtk.Orientation.HORIZONTAL,
                                  spacing=4, valign=Gtk.Align.CENTER)
        row.add_suffix(row._suffix_box)
        return row

    def _update_row(self, row, key):
        row.set_title(key.name)
        # 副标题：模型 · 限额（0=∞） · 可选 AID
        models = key.models or ["*"]
        models_text = (_("all models") if "*" in models
                       else ", ".join(models))
        limits = _("%s rpm · %s tpm · %s tokens/day") % (
            self._fmt(key.rpm), self._fmt(key.tpm),
            self._fmt(key.daily_tokens))
        aid = _(" · AID %s") % key.aid if key.aid else ""
        row.set_subtitle("%s · %s%s" % (models_text, limits, aid))
        box = row._suffix_box
        child = box.get_first_child()
        while child is not None:
            nxt = child.get_next_sibling()
            box.remove(child)
            child = nxt
        # 状态：色点 + 文字
        status = STATUS_LABELS.get(key.status, key.status)
        state = Gtk.Box(orientation=Gtk.Orientation.HORIZONTAL, spacing=5,
                        valign=Gtk.Align.CENTER)
        state.append(ColorDot(STATUS_CLASSES.get(key.status, "dim")))
        state_label = Gtk.Label(label=_(status))
        state_label.add_css_class("caption")
        state_label.add_css_class(STATUS_CLASSES.get(key.status, "dim-label"))
        state.append(state_label)
        box.append(state)
        if key.status != "revoked":
            box.append(self._mini_button(
                _("Edit"), "document-edit-symbolic",
                lambda _b, kid=key.id: self._open_edit(kid)))
        if key.status == "active":
            box.append(self._mini_button(
                _("Suspend"), "media-playback-pause-symbolic",
                lambda _b, kid=key.id: self._confirm_state(
                    kid, "suspend")))
        elif key.status == "suspended":
            box.append(self._mini_button(
                _("Resume"), "media-playback-start-symbolic",
                lambda _b, kid=key.id: self._do_state(kid, "resume")))
        if key.status != "revoked":
            box.append(self._mini_button(
                _("Revoke"), "user-trash-symbolic",
                lambda _b, kid=key.id: self._confirm_state(
                    kid, "revoke"), destructive=True))

    @staticmethod
    def _fmt(value):
        if not value:
            return "∞"
        if value >= 1_000_000:
            return "%.0fM" % (value / 1_000_000.0)
        if value >= 1_000:
            return "%.0fk" % (value / 1_000.0)
        return str(value)

    @staticmethod
    def _mini_button(tooltip, icon, callback, destructive=False):
        button = Gtk.Button(icon_name=icon, valign=Gtk.Align.CENTER)
        button.set_tooltip_text(tooltip)
        if destructive:
            button.add_css_class("destructive-action")
        button.connect("clicked", callback)
        return button

    # ------------------------------------------------------------------
    # 状态变更
    # ------------------------------------------------------------------
    # action → (heading msgid, body msgid, accept msgid, 方法名, destructive)
    ACTIONS = {
        "suspend": ("Suspend key?",
                    "Requests from apps holding this key will be rejected "
                    "(429).", "Suspend", "suspend_key", True),
        "resume": ("Resume key", "Resume using this key?", "Resume",
                   "resume_key", False),
        "revoke": ("Revoke key?",
                   "Revocation cannot be undone; apps holding this key stop "
                   "working immediately.", "Revoke", "revoke_key", True),
    }

    def _confirm_state(self, key_id, action):
        key = self._keys.get(key_id)
        if key is None:
            return
        heading, body, accept, _method, destructive = self.ACTIONS[action]

        def done(ok):
            if ok:
                self._do_state(key_id, action)

        confirm_async(self.win.window, _(heading),
                      "“%s”: %s" % (key.name, _(body)), _(accept), done,
                      destructive=destructive)

    def _do_state(self, key_id, action):
        key = self._keys.get(key_id)
        method_name = self.ACTIONS[action][3]
        try:
            getattr(self.backend, method_name)(key_id)
            if key is not None:
                self.win.toast(_("%s: %s applied") % (
                    key.name, _(self.ACTIONS[action][2])))
        except BackendError as err:
            self.win.toast(_("Action failed: %s") % err.message)
            return
        # 真实后端变更后不广播信号：主动刷新本页（backend 侧缓存已回源）
        self.refresh()

    # ------------------------------------------------------------------
    # 发放 / 编辑
    # ------------------------------------------------------------------
    def _on_issue_clicked(self, button):
        dialog = KeyFormDialog(self.win, self.backend)
        dialog.connect("closed", lambda *a: self.refresh())
        dialog.present(self.win.window)

    def _open_edit(self, key_id):
        key = self._keys.get(key_id)
        if key is None:
            return
        dialog = KeyFormDialog(self.win, self.backend, edit_key=key)
        dialog.connect("closed", lambda *a: self.refresh())
        dialog.present(self.win.window)


class KeyFormDialog(Adw.PreferencesDialog):
    """发放/编辑共用表单。

    发放：app 名 + 模型白名单 + 限额（默认 0 = 不限）+ 可选 AID；
    编辑：app 名只读，其余同上（数值 0 同样表示不限）。
    模型白名单是可能很长的多行内容：整行文本框，而非行尾窄框；
    留空（或单个 *）即不限制。
    """

    def __init__(self, win, backend, edit_key=None):
        self.editing = edit_key is not None
        super().__init__(title=_("Edit key") if self.editing
                         else _("Issue key"))
        self.win = win
        self.backend = backend
        self.key = edit_key
        self.set_size_request(480, -1)
        page = Adw.PreferencesPage()
        self.add(page)

        group = Adw.PreferencesGroup(
            title=_("Key settings") if self.editing else _("New key"))

        if self.editing:
            group.add(Adw.ActionRow(title=_("App name"),
                                    subtitle=edit_key.name))
        else:
            self.name_row = make_entry_row(_("App name (unique)"))
            group.add(self.name_row)

        self.models_row, self.models_view = make_textview_row(
            _("Model allowlist"),
            _("One alias per line; empty or a single * means unrestricted"))
        if self.editing and edit_key.models and "*" not in edit_key.models:
            self.models_view.get_buffer().set_text(
                "\n".join(edit_key.models))
        group.add(self.models_row)

        self.rpm_row = make_spin_row(_("RPM (requests per minute)"),
                                     self._cur("rpm"), 0, 100000, 1,
                                     _("0 = unlimited"))
        group.add(self.rpm_row)
        self.tpm_row = make_spin_row(_("TPM (tokens per minute)"),
                                     self._cur("tpm"), 0, 100000000, 10000,
                                     _("0 = unlimited"))
        group.add(self.tpm_row)
        self.daily_row = make_spin_row(_("Daily token quota"),
                                       self._cur("daily_tokens"),
                                       0, 1000000000, 10000,
                                       _("0 = unlimited"))
        group.add(self.daily_row)

        self.aid_row = make_entry_row(
            _("AID binding (optional, links to agent identity)"))
        if self.editing and edit_key.aid:
            self.aid_row.set_text(edit_key.aid)
        group.add(self.aid_row)

        group.add(button_row(
            _("Save changes") if self.editing else _("Issue"),
            self._on_submit,
            icon=("object-select-symbolic" if self.editing
                  else "list-add-symbolic"),
            css_class="suggested-action"))

        page.add(group)

    def _cur(self, field_name):
        """编辑预填当前值（0 = 不限）；发放默认 0。"""
        if self.editing:
            return int(getattr(self.key, field_name) or 0)
        return 0

    def _buffer_text(self):
        buf = self.models_view.get_buffer()
        return buf.get_text(buf.get_start_iter(), buf.get_end_iter(), False)

    def _on_submit(self, row):
        models = parse_name_lines(self._buffer_text()) or ["*"]
        if self.editing:
            patch = {
                "models": models,
                "rpm": int(self.rpm_row.get_value()),
                "tpm": int(self.tpm_row.get_value()),
                "daily_tokens": int(self.daily_row.get_value()),
                "aid": self.aid_row.get_text().strip(),
            }
            try:
                self.backend.update_key(self.key.id, patch)
            except BackendError as err:
                self.win.toast(_("Update failed: %s") % err.message)
                return
            self.win.toast(_("Key updated: %s") % self.key.name)
            self.close()
            return
        name = self.name_row.get_text().strip()
        if not name:
            self.win.toast(_("App name cannot be empty"))
            return
        opts = {"name": name, "models": models}
        for spin, field_name in ((self.rpm_row, "rpm"),
                                 (self.tpm_row, "tpm"),
                                 (self.daily_row, "daily_tokens")):
            value = int(spin.get_value())
            if value > 0:                       # 0 = 不限：发放时不携带
                opts[field_name] = value
        aid = self.aid_row.get_text().strip()
        if aid:
            opts["aid"] = aid
        try:
            result = self.backend.issue_key(opts)
        except BackendError as err:
            self.win.toast(_("Issue failed: %s") % err.message)
            return
        self.close()
        SecretDialog(self.win, result["secret"], name).present(self.win.window)


class SecretDialog(Adw.Dialog):
    """secret 只显示一次：复选框说明 + 复制按钮。"""

    def __init__(self, win, secret, name):
        super().__init__(title=_("Key issued: %s") % name)
        self.win = win
        self.set_size_request(440, -1)
        toolbar = Adw.ToolbarView()
        toolbar.add_top_bar(Adw.HeaderBar())
        box = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=16,
                      margin_top=24, margin_bottom=24,
                      margin_start=24, margin_end=24)
        warning = Gtk.Label(
            label=_("Save the key now — it cannot be shown again after "
                    "closing."),
            justify=Gtk.Justification.CENTER)
        warning.add_css_class("warning")
        box.append(warning)

        secret_label = Gtk.Label(label=secret, selectable=True)
        secret_label.add_css_class("monospace")
        secret_label.add_css_class("card")
        secret_label.add_css_class("padded-card")
        box.append(secret_label)

        copy_button = Gtk.Button(label=_("Copy to clipboard"))
        copy_button.add_css_class("suggested-action")

        def on_copy(_b):
            display = Gdk.Display.get_default()
            if display is not None:
                display.get_clipboard().set_text(secret)
                self.win.toast(_("Copied to clipboard"))

        copy_button.connect("clicked", on_copy)
        copy_button.set_halign(Gtk.Align.CENTER)
        box.append(copy_button)

        clamp = Adw.Clamp()
        clamp.set_child(box)
        toolbar.set_content(clamp)
        self.set_child(toolbar)
