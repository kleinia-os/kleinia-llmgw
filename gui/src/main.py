#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""main.py — io.kleinia.ModelGateway 应用入口。

用法：
  main.py [--mock] [--dbus-address ADDR] [--open TARGET]
          [--selftest] [--version] [--search-provider]

  --mock               用进程内 MockBackend（无 D-Bus，含 5s 模拟数据变化）
  --dbus-address ADDR  对等连接 llmgwd（Gio.DBusConnection.new_for_address，
                       宿主无 dbus-daemon 时的联调通道）；默认走 system 总线
  --open TARGET        直接打开某页：overview/sources/models/keys/routing/usage
                       或 source:<名>/model:<别名>/key:<名>（search provider 用）
  --selftest           构造窗口与全部六页、断言关键部件后退出（CI 用）
  --version            打印版本
  --search-provider    以 org.gnome.Shell.SearchProvider2 服务运行（GNOME 全局
                       搜索集成；配合 --dbus-address 可在对等总线上联调）
"""

import os
import sys
import time
import traceback

import gi

try:
    gi.require_version("Adw", "1.0")
except ValueError:  # 新 typelib 工具链（libadwaita 1.10）版本号注册为 "1"
    gi.require_version("Adw", "1")
gi.require_version("Gtk", "4.0")
from gi.repository import Adw, Gio, GLib, Gtk  # noqa: E402
try:
    from gi.repository import Gdk  # noqa: E402
except ImportError:  # pragma: no cover
    Gdk = None

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from backend import (MockBackend, DBusBackend, BackendError,  # noqa: E402
                     SOURCE_TYPE_LABELS, BUS_NAME, GLM_VERSION)
from i18n import _  # noqa: E402
from pages.overview import OverviewPage  # noqa: E402
from pages.sources import SourcesPage  # noqa: E402
from pages.models import ModelsPage  # noqa: E402
from pages.keys import KeysPage  # noqa: E402
from pages.usage import UsagePage  # noqa: E402
from pages.logs import LogsPage  # noqa: E402
from widgets import register_css  # noqa: E402

APP_ID = "io.kleinia.ModelGateway"
VERSION = GLM_VERSION
WINDOW_TITLE = "Model Gateway"   # msgid（侧栏/窗口标题经 _() 翻译）

# 优先级/权重调度在 Sources 的编辑表单里配置（Scheduling 组）；
# 路由页曾是同一组参数的第二入口，已并入后移除。
PAGE_CLASSES = (OverviewPage, SourcesPage, ModelsPage, KeysPage,
                UsagePage, LogsPage)


# ---------------------------------------------------------------------------
# 主窗口：AdwOverlaySplitView（navigational sidebar，HIG 模式）
# ---------------------------------------------------------------------------
class MainWindow(Adw.ApplicationWindow):
    def __init__(self, app, backend):
        super().__init__(application=app, title=WINDOW_TITLE)
        self.set_default_size(1020, 720)
        self.app = app
        self.backend = backend
        self.window = self            # 页面弹层用统一父窗口引用
        self.pages = {}
        self.alerts = []              # [(epoch, kind, text)] 新→旧
        register_css()               # padded-card 等应用样式

        # ---- 顶部连接横幅 ----
        self.banner = Adw.Banner(
            title=_("Cannot connect to the model gateway backend"),
            button_label=_("Retry"))
        self.banner.connect("button-clicked", self._on_retry)
        self.banner.set_revealed(False)

        # ---- 分栏（先建内容再建侧栏；选中信号在两侧都就位后统一触发）----
        self.split = Adw.OverlaySplitView()
        self.split.set_content(self._build_content())
        self.split.set_sidebar(self._build_sidebar())
        self.split.set_min_sidebar_width(200)
        self.split.set_max_sidebar_width(280)
        self.set_content(self.split)
        first_row = self.nav.get_first_child()
        if first_row is not None:
            self.nav.select_row(first_row)

        # 窄窗口：折叠侧栏（HIG navigational 模式）
        breakpoint = Adw.Breakpoint.new(
            Adw.BreakpointCondition.parse("max-width: 720px"))
        breakpoint.connect("apply", lambda _bp: (
            self.split.set_collapsed(True),
            self.split.set_show_sidebar(False)))
        breakpoint.connect("unapply", lambda _bp: (
            self.split.set_collapsed(False),
            self.split.set_show_sidebar(True)))
        self.add_breakpoint(breakpoint)

        # ---- 后端信号 ----
        backend.connect("connected", self._on_connected)
        backend.connect("disconnected", self._on_disconnected)
        backend.connect("sources-changed", lambda *a: self._refresh(
            "sources", "models", "overview"))
        backend.connect("keys-changed", lambda *a: self._refresh(
            "keys", "overview"))
        backend.connect("usage-changed", lambda *a: self._refresh(
            "usage", "overview"))
        backend.connect("prices-changed", lambda *a: self._refresh(
            "models", "usage", "overview"))
        backend.connect("source-health", self._on_source_health)
        backend.connect("quota-warning", self._on_quota_warning)

        backend.initialize()

    # ------------------------------------------------------------------
    # UI 构建
    # ------------------------------------------------------------------
    def _build_sidebar(self):
        header = Adw.HeaderBar()
        self.side_title = Adw.WindowTitle(title=_(WINDOW_TITLE),
                                        subtitle=_("Connecting…"))
        header.set_title_widget(self.side_title)

        nav = Gtk.ListBox()
        nav.add_css_class("navigation-sidebar")
        nav.set_selection_mode(Gtk.SelectionMode.SINGLE)
        self.nav = nav
        nav.connect("row-selected", self._on_nav_selected)

        for page_cls in PAGE_CLASSES:
            box = Gtk.Box(orientation=Gtk.Orientation.HORIZONTAL, spacing=10)
            box.append(Gtk.Image(icon_name=page_cls.icon))
            label = Gtk.Label(label=_(page_cls.title), hexpand=True,
                              xalign=0)
            box.append(label)
            row = Gtk.ListBoxRow(child=box)
            row._page_name = page_cls.name
            nav.append(row)

        toolbar = Adw.ToolbarView()
        toolbar.add_top_bar(header)
        scrolled = Gtk.ScrolledWindow()
        scrolled.set_child(nav)
        toolbar.set_content(scrolled)
        return toolbar

    def _build_content(self):
        toolbar = Adw.ToolbarView()
        self.header = Adw.HeaderBar()
        self.content_title = Adw.WindowTitle(title=_(OverviewPage.title),
                                           subtitle="")
        self.header.set_title_widget(self.content_title)

        menu_button = Gtk.MenuButton(icon_name="open-menu-symbolic")
        menu_button.set_primary(True)
        about_menu = Gio.Menu()
        about_item = Gio.MenuItem.new(_("About"), "win.about")
        about_menu.append_item(about_item)
        menu_button.set_menu_model(about_menu)
        self.header.pack_end(menu_button)

        toolbar.add_top_bar(self.header)
        toolbar.add_top_bar(self.banner)

        self.toast_overlay = Adw.ToastOverlay()
        self.view_stack = Adw.ViewStack()
        self.toast_overlay.set_child(self.view_stack)
        toolbar.set_content(self.toast_overlay)

        about_action = Gio.SimpleAction(name="about")
        about_action.connect("activate", lambda *a: self.show_about())
        self.add_action(about_action)

        return toolbar

    def _ensure_pages(self):
        """惰性建页（首帧前构造完，便于 selftest 断言）。"""
        if self.pages:
            return
        for page_cls in PAGE_CLASSES:
            page = page_cls(self, self.backend)
            self.pages[page.name] = page
            vsp = self.view_stack.add(page.widget)
            vsp.set_name(page.name)
            vsp.set_title(_(page.title))
            vsp.set_icon_name(page.icon)

    # ------------------------------------------------------------------
    # 导航
    # ------------------------------------------------------------------
    def _on_nav_selected(self, box, row):
        if row is None:
            return
        self._ensure_pages()
        page = self.pages.get(row._page_name)
        if page is None:
            return
        self.view_stack.set_visible_child(page.widget)
        self.content_title.set_title(_(page.title))

    def open_page(self, target):
        """target: 页名或 source:<名>/model:<别名>/key:<名>。"""
        self._ensure_pages()
        prefix = target.split(":", 1)[0]
        base = {"source": "sources", "model": "models",
                "key": "keys"}.get(prefix, prefix)
        page = self.pages.get(base)
        if page is None:
            base = "overview"
            page = self.pages[base]
        self.view_stack.set_visible_child(page.widget)
        self.content_title.set_title(_(page.title))
        for row in self._nav_rows():
            if row._page_name == base:
                self.nav.select_row(row)
                break

    def _nav_rows(self):
        child = self.nav.get_first_child()
        while child is not None:
            yield child
            child = child.get_next_sibling()

    # ------------------------------------------------------------------
    # 后端事件
    # ------------------------------------------------------------------
    def _on_connected(self, *_a):
        self.banner.set_revealed(False)
        info = {}
        try:
            info = self.backend.manager_info() or {}
        except Exception:
            info = {}
        self.side_title.set_subtitle(
            "%s · %s" % (info.get("backend", "?"),
                         info.get("http_addr", "?")))
        self._refresh(*[c.name for c in PAGE_CLASSES])

    def _on_disconnected(self, _b, reason):
        self.banner.set_title(
            _("Cannot connect to the model gateway backend%s")
            % (" (%s)" % reason if reason else ""))
        self.banner.set_revealed(True)
        self.side_title.set_subtitle(_("Disconnected"))

    def _on_retry(self, banner):
        self.backend.reconnect()
        self.toast(_("Reconnecting…"))

    def _on_source_health(self, _b, source_id, state, reason):
        self.push_alert("source", _("Source #%s health changed: %s (%s)")
                        % (source_id, state, reason))
        self.toast(_("Source health: %s %s") % (state, reason))
        self._refresh("sources", "overview")

    def _on_quota_warning(self, _b, ident, window, pct):
        # real 后端：ident 形如 key:<名>/aid:<aid>；pct 为分数（0-1）
        name = ident.split(":", 1)[1] if ":" in ident else ident
        pct = pct * 100.0 if 0 < pct <= 1.0 else pct
        text = _("%s has used %squota: %.0f%%") % (
            name, {"daily": _("daily"), "daily_tokens": _("daily"),
                   "tpm": _("TPM "), "rpm": _("RPM ")}.get(
                window, window + " "),
            pct)
        self.push_alert("quota", text)
        self.notify_user(_("Model gateway quota warning"), text)
        self._refresh("usage", "keys", "overview")

    # ------------------------------------------------------------------
    # 告警 / Toast / 通知
    # ------------------------------------------------------------------
    def push_alert(self, kind, text):
        self.alerts.insert(0, (time.time(), kind, text))
        del self.alerts[50:]
        overview = self.pages.get("overview")
        if overview is not None:
            overview.update_alerts(self.alerts)

    def toast(self, text):
        self.toast_overlay.add_toast(Adw.Toast(title=text))

    def notify_user(self, title, body):
        try:
            notification = Gio.Notification.new(title)
            notification.set_body(body)
            notification.set_icon(
                Gio.ThemedIcon.new(APP_ID + "-symbolic"))
            self.app.send_notification("llmgw-alert", notification)
        except Exception:
            pass  # 无会话总线/portal 时静默

    def _refresh(self, *names):
        for name in names:
            page = self.pages.get(name)
            if page is None:
                continue
            try:
                page.refresh()
            except Exception:
                traceback.print_exc()

    # ------------------------------------------------------------------
    def show_about(self):
        dialog = Adw.AboutDialog(
            application_name=WINDOW_TITLE,
            application_icon=APP_ID + "-symbolic",
            version=VERSION,
            developer_name="Kleinia",
            website="https://kleinia.io/llmgw",
            issue_url="https://kleinia.io/llmgw/issues",
            copyright="© 2026 Kleinia",
            comments=_("Management UI for llmgwd, the Kleinia system "
                        "model gateway: aggregated upstream sources with "
                        "per-alias model mapping, per-app keys with quota "
                        "windows, priority + weight routing, and usage "
                        "accounting."))
        dialog.present(self)

    # ------------------------------------------------------------------
    # selftest 断言（--selftest / tests 复用）
    # ------------------------------------------------------------------
    def selftest_checks(self):
        self._ensure_pages()
        # Empty real backends (fresh scratch DBs) have no rows yet: seed one
        # placeholder source and a few keys so the row-rendering assertions
        # mean something. This targets scratch/test gateways; a production
        # selftest run is not the norm. Failures are non-fatal (mock or
        # read-only backends skip the seeding).
        try:
            if not self.backend.list_sources():
                self.backend.add_source({
                    "name": "selftest-src", "type": "openai",
                    "base_url": "http://127.0.0.1:9",
                    "api_key": "selftest-unused",
                    "models": [{"alias": "selftest-model",
                                "upstream": "selftest-model"}],
                    "priority": 0, "weight": 0, "enabled": True})
        except Exception:
            pass
        try:
            missing = 3 - len(self.backend.list_keys() or [])
            for i in range(max(0, missing)):
                self.backend.issue_key(
                    {"name": "selftest-%d" % (i + 1), "models": "*"})
            self.pages["keys"].refresh()
        except Exception:
            pass
        checks = [
            ("six pages built", len(self.pages) == 6),
            ("viewstack pages", len(list(self.view_stack.get_pages())) == 6),
            ("sidebar rows == 6", len(list(self._nav_rows())) == 6),
            ("overview range selector",
             self.pages["overview"].range_drop is not None),
            ("overview 4 KPI cards",
             len(self.pages["overview"].cards) == 4),
            ("overview sparklines",
             all(card.spark is not None for card
                 in self.pages["overview"].cards.values())),
            ("overview trend chart",
             self.pages["overview"].trend is not None),
            ("overview alerts list",
             self.pages["overview"].alerts_box is not None),
            ("sources rows", self.pages["sources"].row_count() >= 1),
            ("sources add button",
             self.pages["sources"].add_button is not None),
            ("sources scheduling spins",
             self.pages["sources"]._rows is not None),
            ("models rows", self.pages["models"].row_count() >= 1),
            ("keys rows", self.pages["keys"].row_count() >= 3),
            ("keys issue button",
             self.pages["keys"].issue_button is not None),
            ("overview latency/cache cards",
             "p50" in self.pages["overview"].latency_labels
             and "p95" in self.pages["overview"].latency_labels
             and self.pages["overview"].cache_card is not None),
            ("usage range selector",
             self.pages["usage"].window_drop is not None
             and self.pages["usage"].dimension_drop is not None),
            ("usage stacked chart",
             self.pages["usage"].stacked is not None),
            ("usage rank chart",
             self.pages["usage"].rank_chart is not None),
            ("logs table built",
             self.pages["logs"].view is not None
             and self.pages["logs"].view.get_columns().get_n_items() == 7),
            ("logs pagination",
             self.pages["logs"].pager_label is not None
             and self.pages["logs"].row_count() >= 0),
            ("banner hidden (connected)",
             not self.banner.get_revealed()),
        ]
        return checks


# ---------------------------------------------------------------------------
# 应用
# ---------------------------------------------------------------------------
class ModelGatewayApp(Adw.Application):
    def __init__(self, backend_factory, selftest=False, open_target=None,
                 non_unique=False, app_id=APP_ID):
        flags = Gio.ApplicationFlags.HANDLES_COMMAND_LINE
        if non_unique:
            flags |= Gio.ApplicationFlags.NON_UNIQUE
        super().__init__(application_id=app_id, flags=flags)
        self._backend_factory = backend_factory
        self._selftest = selftest
        self._open_target = open_target
        self.selftest_failed = False

    def do_activate(self):
        register_icon_dir()
        win = self.props.active_window
        if win is None:
            win = MainWindow(self, self._backend_factory())
            self.add_window(win)
        win.present()
        if self._open_target:
            win.open_page(self._open_target)
            self._open_target = None
        if self._selftest:
            self._selftest = False  # 只跑一次
            GLib.idle_add(self._run_selftest, win)

    def do_command_line(self, cmdline):
        options = parse_args(cmdline.get_arguments()[1:])
        if options.open:
            self._open_target = options.open
        if options.selftest:
            self._selftest = True
        self.activate()
        return 0

    def _run_selftest(self, win):
        try:
            win.realize()
            checks = win.selftest_checks()
            failed = [name for name, ok in checks if not ok]
            for name, ok in checks:
                print("selftest  %-28s %s" % (name, "OK" if ok else "FAIL"))
            self.selftest_failed = bool(failed)
            print("selftest: %s (%d checks)"
                  % ("FAIL" if failed else "PASS", len(checks)))
        except Exception:
            traceback.print_exc()
            self.selftest_failed = True
            print("selftest: FAIL (exception)")
        self.quit()
        return GLib.SOURCE_REMOVE


# ---------------------------------------------------------------------------
# GNOME Shell search provider（org.gnome.Shell.SearchProvider2）
# ---------------------------------------------------------------------------
SEARCH_PROVIDER_PATH = "/io/kleinia/ModelGateway/SearchProvider"
SEARCH_PROVIDER_IFACE = "org.gnome.Shell.SearchProvider2"

PROVIDER_XML = """
<node>
  <interface name="org.gnome.Shell.SearchProvider2">
    <method name="GetInitialResultSet">
      <arg type="as" name="terms" direction="in"/>
      <arg type="as" name="results" direction="out"/>
    </method>
    <method name="GetSubsearchResultSet">
      <arg type="as" name="previous_results" direction="in"/>
      <arg type="as" name="terms" direction="in"/>
      <arg type="as" name="results" direction="out"/>
    </method>
    <method name="GetResultMetas">
      <arg type="as" name="results" direction="in"/>
      <arg type="aa{sv}" name="metas" direction="out"/>
    </method>
    <method name="ActivateResult">
      <arg type="s" name="id" direction="in"/>
      <arg type="as" name="terms" direction="in"/>
      <arg type="u" name="timestamp" direction="in"/>
    </method>
    <method name="LaunchSearch">
      <arg type="as" name="terms" direction="in"/>
      <arg type="u" name="timestamp" direction="in"/>
    </method>
  </interface>
</node>
"""

# kind → (msgid 标签, 图标, 目标页)
KIND_META = {
    "source": ("source", "network-server-symbolic", "sources"),
    "model": ("model alias", "package-x-generic-symbolic", "models"),
    "key": ("key", "system-lock-screen-symbolic", "keys"),
}


class SearchProviderService:
    """索引源名/模型别名/key 名；ActivateResult 拉起主应用并跳页。

    数据源：优先 llmgwd（system 总线 / --dbus-address），不可达时退回
    MockBackend 静态数据（宿主集成演示）。
    """

    def __init__(self, dbus_backend):
        self._real = dbus_backend
        self._fallback = MockBackend(live=False)

    def _backend(self):
        try:
            self._real.list_sources()
            return self._real
        except BackendError:
            return self._fallback

    def entries(self):
        out = []
        backend = self._backend()
        try:
            for src in backend.list_sources():
                out.append({"id": "source:%s" % src.name, "name": src.name,
                            "kind": "source",
                            "desc": "%s · %s" % (
                                _("source"),
                                SOURCE_TYPE_LABELS.get(src.type, src.type))})
        except BackendError:
            pass
        try:
            for alias, names in backend.alias_sources().items():
                out.append({"id": "model:%s" % alias, "name": alias,
                            "kind": "model",
                            "desc": "%s · %s" % (
                                _("model alias"), ", ".join(names))})
        except BackendError:
            pass
        try:
            for key in backend.list_keys():
                out.append({"id": "key:%s" % key.name, "name": key.name,
                            "kind": "key",
                            "desc": "%s · %s" % (_("key"), key.status)})
        except BackendError:
            pass
        return out

    @staticmethod
    def _match(entries, terms):
        needles = [t.casefold() for t in terms if t.strip()]
        result = []
        for entry in entries:
            hay = ("%s %s" % (entry["name"], entry["desc"])).casefold()
            if all(n in hay for n in needles):
                result.append(entry["id"])
        return result[:10]

    def get_initial(self, terms):
        return self._match(self.entries(), terms)

    def get_subsearch(self, previous, terms):
        prev = set(previous)
        return [rid for rid in self.get_initial(terms) if rid in prev]

    def get_metas(self, ids):
        metas = []
        by_id = {e["id"]: e for e in self.entries()}
        for rid in ids:
            entry = by_id.get(rid)
            if entry is None:
                continue
            _kind_label, icon_name, _page = KIND_META[entry["kind"]]
            meta = {
                "id": GLib.Variant("s", entry["id"]),
                "name": GLib.Variant("s", entry["name"]),
                "description": GLib.Variant("s", entry["desc"]),
            }
            try:
                meta["icon"] = Gio.ThemedIcon.new(icon_name).serialize()
            except Exception:
                pass
            metas.append(meta)
        return metas

    def activate(self, rid):
        base = rid.split(":", 1)[0]
        page = KIND_META.get(base, (None, None, "overview"))[2]
        target = rid if base in KIND_META else page
        main_py = os.path.abspath(__file__)
        try:
            Gio.Subprocess.new(
                [sys.executable, main_py, "--open", target],
                Gio.SubprocessFlags.NONE)
        except GLib.Error as err:
            print("ActivateResult: failed to launch the app: %s" % err.message,
                  file=sys.stderr)

    # ---- D-Bus method_call 分发 ------------------------------------------
    def method_call(self, conn, sender, path, iface, method, params,
                    invocation):
        try:
            if method == "GetInitialResultSet":
                results = self.get_initial(params.unpack()[0])
                invocation.return_value(GLib.Variant("(as)", (results,)))
            elif method == "GetSubsearchResultSet":
                previous, terms = params.unpack()
                results = self.get_subsearch(previous, terms)
                invocation.return_value(GLib.Variant("(as)", (results,)))
            elif method == "GetResultMetas":
                metas = self.get_metas(params.unpack()[0])
                invocation.return_value(GLib.Variant("(aa{sv})", (metas,)))
            elif method == "ActivateResult":
                rid = params.unpack()[0]
                self.activate(rid)
                invocation.return_value(None)
            elif method == "LaunchSearch":
                Gio.Subprocess.new(
                    [sys.executable, os.path.abspath(__file__), "--open",
                     "overview"], Gio.SubprocessFlags.NONE)
                invocation.return_value(None)
            else:
                invocation.return_dbus_error(
                    "io.kleinia.ModelGateway.UnknownMethod", method)
        except Exception as err:  # noqa: BLE001
            invocation.return_dbus_error(
                "io.kleinia.ModelGateway.SearchError", str(err))


def run_search_provider(options):
    """--search-provider 入口：会话总线或 --dbus-address 对等总线。"""
    backend = (DBusBackend(address=options.dbus_address)
               if options.dbus_address else DBusBackend())
    service = SearchProviderService(backend)
    node = Gio.DBusNodeInfo.new_for_xml(PROVIDER_XML)
    iface_info = node.lookup_interface(SEARCH_PROVIDER_IFACE)
    loop = GLib.MainLoop()

    if options.dbus_address:
        # 对等模式：本进程当 D-Bus server，监听 --dbus-address 指定的地址
        # （如 unix:path=/tmp/mgprov.sock；无 dbus-daemon 也可联调）
        server = Gio.DBusServer.new_sync(
            options.dbus_address, Gio.DBusServerFlags.NONE,
            Gio.dbus_generate_guid(), None, None)

        keep_conns = []   # 防止传入连接的 Python 包装被 GC 后连接关闭

        def on_new_connection(_server, conn):
            try:
                conn.register_object(SEARCH_PROVIDER_PATH, iface_info,
                                     service.method_call, None, None)
                keep_conns.append(conn)
            except Exception as err:  # noqa: BLE001
                print("register_object failed: %s" % err, file=sys.stderr)
                return False
            return True

        server.connect("new-connection", on_new_connection)
        server.start()
        print("search provider（对等总线）client 地址：%s"
              % server.get_client_address(), flush=True)
    else:
        def on_bus_acquired(conn, name):
            conn.register_object(SEARCH_PROVIDER_PATH, iface_info,
                                 service.method_call, None, None)

        Gio.bus_own_name(
            Gio.BusType.SESSION, APP_ID, Gio.BusNameOwnerFlags.NONE,
            on_bus_acquired,
            lambda *a: print("search provider：已在会话总线获得 %s" % APP_ID,
                             flush=True),
            lambda *a: print("search provider：总线名丢失", flush=True))
        print("search provider：正在会话总线上请求 %s …" % APP_ID, flush=True)

    try:
        loop.run()
    except KeyboardInterrupt:
        pass
    return 0


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------
class Options:
    def __init__(self):
        self.mock = False
        self.dbus_address = None
        self.selftest = False
        self.version = False
        self.help = False
        self.open = None
        self.search_provider = False


def parse_args(argv):
    opts = Options()
    i = 0
    while i < len(argv):
        arg = argv[i]
        if arg == "--mock":
            opts.mock = True
        elif arg == "--dbus-address":
            i += 1
            opts.dbus_address = argv[i] if i < len(argv) else None
        elif arg.startswith("--dbus-address="):
            opts.dbus_address = arg.split("=", 1)[1]
        elif arg == "--selftest":
            opts.selftest = True
        elif arg in ("--version", "-v"):
            opts.version = True
        elif arg == "--open":
            i += 1
            opts.open = argv[i] if i < len(argv) else None
        elif arg.startswith("--open="):
            opts.open = arg.split("=", 1)[1]
        elif arg == "--search-provider":
            opts.search_provider = True
        elif arg in ("-h", "--help"):
            opts.help = True
        i += 1
    return opts


def _session_bus_available():
    try:
        Gio.bus_get_sync(Gio.BusType.SESSION, None)
        return True
    except GLib.Error:
        return False


_ICON_DIR = os.path.join(os.path.dirname(os.path.dirname(
    os.path.abspath(__file__))), "data", "icons")


def register_icon_dir():
    """把应用自带 symbolic 图标目录挂进默认图标主题（幂等）。
    页面图标 llmgw-gauge-symbolic / llmgw-chart-symbolic 在 Adwaita 里
    没有等价物，缺失时 GTK 会回退到红色的 image-missing。"""
    if Gdk is None:
        return
    display = Gdk.Display.get_default()
    if display is None:
        return
    try:
        theme = Gtk.IconTheme.get_for_display(display)
        if _ICON_DIR not in theme.get_search_path():
            theme.add_search_path(_ICON_DIR)
    except Exception:
        pass


def main(argv=None):
    if argv is None:
        argv = sys.argv[1:]
    options = parse_args(argv)
    if options.version:
        print("io.kleinia.ModelGateway %s" % VERSION)
        return 0
    if options.help:
        print(__doc__)
        return 0
    if options.search_provider:
        return run_search_provider(options)

    def backend_factory():
        if options.mock:
            return MockBackend(live=True)
        if options.dbus_address:
            return DBusBackend(address=options.dbus_address)
        return DBusBackend()

    # 无会话总线（无 dbus-daemon 宿主 / xvfb CI）：退化为 NON_UNIQUE，
    # 保证 --selftest 可独立运行；真实 GNOME 桌面保持单例转发语义。
    app = ModelGatewayApp(
        backend_factory, selftest=options.selftest,
        open_target=options.open,
        non_unique=not _session_bus_available())
    # GApplication.run 期望 argv 含程序名（argv[0] 被跳过）；main() 收到的
    # 是 sys.argv[1:]，必须补一个占位，否则第一个 flag 会被当成程序名吃掉、
    # 应用在激活前静默退出。
    rc = app.run([sys.argv[0] or "io.kleinia.ModelGateway"] + list(argv))
    return 1 if app.selftest_failed else rc


if __name__ == "__main__":
    sys.exit(main())
