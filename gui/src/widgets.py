#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""widgets.py — 各页共享构件与自绘图表（DrawingArea + cairo/PangoCairo）。

暗色模式：所有颜色经主题解析（draw 回调里现取，跟随 libadwaita 明暗切换）：
  * 强调色 ``accent_color``（失败回退前景色）
  * 文字 = 前景 ``get_color()``；次级 = 前景 alpha 0.6
  * 成功/警告/失败 = ``success_color`` / ``warning_color`` / ``error_color``
不使用模块级 RGB 常量；回退也是主题查询结果（Gdk.RGBA），非固定色值。
GTK4 绘制回调的 cairo 上下文已按设备缩放，HiDPI 自动正确。

  * HealthDot/ColorDot  色点（语义色）
  * Sparkline           KPI 卡迷你趋势线
  * KpiCard             KPI 卡：标题 + 大数值 + 环比角标 + sparkline
  * TrendChart          面积折线主趋势图（悬停数值）
  * StackedBarChart     堆叠柱状图 + stack_legend 图例
  * BarChart            横条排行（可点击选中，row-activated 信号）
  * confirm_async / menu_button / 表单行构造器 / 文本解析
"""

import math

import gi

try:
    gi.require_version("Adw", "1.0")
except ValueError:  # 新 typelib 工具链（libadwaita 1.10）版本号注册为 "1"
    gi.require_version("Adw", "1")
gi.require_version("Gtk", "4.0")
gi.require_version("Pango", "1.0")
gi.require_version("PangoCairo", "1.0")
from gi.repository import Adw, GObject, Gtk, Pango, PangoCairo  # noqa: E402
try:
    from gi.repository import Gdk  # noqa: E402
except ImportError:  # pragma: no cover
    Gdk = None

from fmt import fmt_int, fmt_bucket_label  # noqa: E402
from i18n import _  # noqa: E402

# ---------------------------------------------------------------------------
# 卡片内边距：libadwaita 的 "card" 样式类只给背景/圆角/边框，不含 padding，
# 直接塞内容文字会贴边。这里给自定义卡片统一加 "padded-card" 内边距。
# ---------------------------------------------------------------------------
# 注：columnview 自带不透明底色会以直角盖住卡片的圆角底色，故透明化，
# 由外层 .card 的圆角背景统一呈现。
_APP_CSS = b"""
.padded-card { padding: 12px 14px; }
.table-card { border-radius: 12px; }
.table-card columnview { background-color: transparent; }
.table-card columnview > header { background-color: transparent; }
"""
_css_registered = False


def register_css():
    """把 padded-card 等应用 CSS 挂到默认显示（幂等；无显示时静默跳过）。"""
    global _css_registered
    if _css_registered or Gdk is None:
        return
    display = Gdk.Display.get_default()
    if display is None:
        return
    provider = Gtk.CssProvider()
    provider.load_from_data(_APP_CSS)
    Gtk.StyleContext.add_provider_for_display(
        display, provider, Gtk.STYLE_PROVIDER_PRIORITY_APPLICATION)
    _css_registered = True

# 健康灯：状态 → 主题命名色（ok/cooling/disabled 与 real 后端状态集一致）
HEALTH_COLOR_NAMES = {
    "ok": "success_color",
    "degraded": "warning_color",
    "cooling": "warning_color",
    "down": "error_color",
    "disabled": None,       # 次级前景（dim）
    "unknown": None,
}
HEALTH_LABELS = {
    "ok": "healthy", "degraded": "degraded", "cooling": "cooling",
    "down": "down", "disabled": "disabled", "unknown": "unknown",
}

# 堆叠柱分段：语义色（accent / accent 低透明 / success）
STACK_SEGMENTS = ("accent", "accent-dim", "success")
STACK_NAMES = ("input", "output", "cache read")   # msgid，经 _() 翻译

BAR_ROW_HEIGHT = 30
BAR_PADDING = 6


# ---------------------------------------------------------------------------
# 主题色解析（draw 回调里调用；禁止固定 RGB）
# ---------------------------------------------------------------------------
def theme_color(widget, name=None, alpha=1.0, dim=False):
    """→ (r, g, b, a) 主题色。

    * ``name``：libadwaita 命名色（accent_color/success_color/warning_color/
      error_color…），未命中或未给 → 前景色（widget.get_color）
    * ``dim``：次级文字（前景 alpha × 0.6）
    * ``alpha``：额外透明系数
    """
    rgba = None
    if name:
        try:
            found, color = widget.get_style_context().lookup_color(name)
            if found:
                rgba = color
        except Exception:
            rgba = None
    if rgba is None:
        try:
            rgba = widget.get_color()
        except AttributeError:                      # 极旧 GTK：StyleContext 回退
            rgba = widget.get_style_context().get_color()
    result_alpha = rgba.alpha * alpha * (0.6 if dim else 1.0)
    return (rgba.red, rgba.green, rgba.blue, result_alpha)


def _semantic(widget, kind, alpha=1.0):
    """语义色 kind ∈ accent|accent-dim|success|warning|error|dim。"""
    names = {"accent": "accent_color", "success": "success_color",
             "warning": "warning_color", "error": "error_color"}
    if kind == "accent-dim":
        return theme_color(widget, "accent_color", alpha=alpha * 0.55)
    if kind == "dim":
        return theme_color(widget, None, alpha=alpha, dim=True)
    return theme_color(widget, names.get(kind), alpha=alpha)


# ---------------------------------------------------------------------------
# 色点
# ---------------------------------------------------------------------------
class _DotBase(Gtk.DrawingArea):
    SIZE = 12

    def __init__(self, size=None):
        super().__init__()
        size = size or self.SIZE
        self.set_content_width(size)
        self.set_content_height(size)
        self.set_valign(Gtk.Align.CENTER)
        self.set_draw_func(self._draw, None)

    def _draw(self, area, cr, width, height, data):
        cr.set_source_rgba(*self._color())
        cr.arc(width / 2.0, height / 2.0, min(width, height) / 2.0 - 1.0,
               0, 2 * math.pi)
        cr.fill()

    def _color(self):
        return theme_color(self, None, dim=True)


class HealthDot(_DotBase):
    """12×12 健康灯，颜色随 state 走主题命名色；tooltip 显示翻译后的状态。"""

    def __init__(self, state="unknown"):
        super().__init__()
        self.set_state(state)

    def set_state(self, state):
        self._state = state if state in HEALTH_COLOR_NAMES else "unknown"
        self.set_tooltip_text("%s (%s)" % (
            _(HEALTH_LABELS[self._state]), self._state))
        self.queue_draw()

    def _color(self):
        name = HEALTH_COLOR_NAMES[self._state]
        return theme_color(self, name) if name else \
            theme_color(self, None, dim=True)


class ColorDot(_DotBase):
    """语义色点：kind ∈ accent|accent-dim|success|warning|error|dim。"""

    def __init__(self, kind):
        super().__init__()
        self._kind = kind

    def _color(self):
        return _semantic(self, self._kind)


# ---------------------------------------------------------------------------
# Sparkline — KPI 卡迷你趋势线
# ---------------------------------------------------------------------------
class Sparkline(Gtk.DrawingArea):
    """无轴折线：series=[float]；空/全零画淡基线。"""

    def __init__(self):
        super().__init__()
        self._series = []
        self.set_content_height(28)
        self.set_hexpand(True)
        self.set_draw_func(self._draw, None)

    def set_series(self, series):
        self._series = [float(v) for v in (series or [])]
        self.queue_draw()

    def do_measure(self, orientation, for_size):
        if orientation == Gtk.Orientation.VERTICAL:
            return (28, 28, -1, -1)
        return (48, 240, -1, -1)

    def _draw(self, area, cr, width, height, data):
        pad = 3.0
        cr.set_line_width(1.6)
        if len(self._series) < 2:
            cr.set_source_rgba(*theme_color(self, "accent_color", alpha=0.25))
            cr.move_to(pad, height - pad)
            cr.line_to(width - pad, height - pad)
            cr.stroke()
            return
        values = self._series
        vmax = max(values)
        vmin = min(values)
        span = (vmax - vmin) or 1.0
        usable_h = height - 2 * pad
        step = (width - 2 * pad) / float(len(values) - 1)

        def _y(value):
            return height - pad - (value - vmin) / span * usable_h

        accent = theme_color(self, "accent_color")
        # 同色低透明填充
        cr.move_to(pad, _y(values[0]))
        for i, value in enumerate(values[1:], 1):
            cr.line_to(pad + i * step, _y(value))
        cr.line_to(pad + (len(values) - 1) * step, height - pad)
        cr.line_to(pad, height - pad)
        cr.close_path()
        cr.set_source_rgba(accent[0], accent[1], accent[2], accent[3] * 0.14)
        cr.fill()
        # 折线 + 末点
        cr.move_to(pad, _y(values[0]))
        for i, value in enumerate(values[1:], 1):
            cr.line_to(pad + i * step, _y(value))
        cr.set_source_rgba(*accent)
        cr.stroke()
        cr.arc(pad + (len(values) - 1) * step, _y(values[-1]), 2.6,
               0, 2 * math.pi)
        cr.fill()


# ---------------------------------------------------------------------------
# KpiCard — 标题 + 大数值 + 环比角标 + sparkline
# ---------------------------------------------------------------------------
class KpiCard(Gtk.Box):
    def __init__(self, title, value="—"):
        super().__init__(orientation=Gtk.Orientation.VERTICAL, spacing=4)
        self.add_css_class("card")
        self.add_css_class("padded-card")
        self.set_margin_top(6)
        self.set_margin_bottom(6)

        header = Gtk.Box(orientation=Gtk.Orientation.HORIZONTAL)
        self.title_label = Gtk.Label(label=title, halign=Gtk.Align.START,
                                     hexpand=True)
        self.title_label.add_css_class("dim-label")
        self.delta_label = Gtk.Label(label="", halign=Gtk.Align.END,
                                     visible=False)
        header.append(self.title_label)
        header.append(self.delta_label)

        self.value_label = Gtk.Label(label=value, halign=Gtk.Align.START)
        self.value_label.add_css_class("title-2")
        self.spark = Sparkline()

        self.append(header)
        self.append(self.value_label)
        self.append(self.spark)

    def set_value(self, text):
        self.value_label.set_text(str(text))

    def set_delta(self, text, css_class=None):
        """text=None 隐藏角标；css_class ∈ success|error。"""
        if not text:
            self.delta_label.set_visible(False)
            return
        self.delta_label.set_text(text)
        for cls in ("success", "error"):
            self.delta_label.remove_css_class(cls)
        if css_class:
            self.delta_label.add_css_class(css_class)
        self.delta_label.set_visible(True)


# ---------------------------------------------------------------------------
# 绘制文本工具（Pango 布局 + cairo 前景色；颜色由调用方传入）
# ---------------------------------------------------------------------------
def draw_text(cr, text, x, y, color, align_left=True, small=False):
    """文本绘制；small 用 8pt 标记，颜色走 color(r,g,b,a)。"""
    layout = PangoCairo.create_layout(cr)
    safe = str(text).replace("&", "&amp;").replace("<", "&lt;")
    if small:
        layout.set_markup('<span size="8192">%s</span>' % safe, -1)
    else:
        layout.set_text(str(text))
    w, h = layout.get_pixel_size()
    cr.set_source_rgba(*color)
    cr.move_to(x if align_left else x - w, y - h / 2.0)
    PangoCairo.show_layout(cr, layout)
    return w, h


# ---------------------------------------------------------------------------
# TrendChart — 面积折线主趋势图
# ---------------------------------------------------------------------------
class TrendChart(Gtk.DrawingArea):
    """series=[(label, value)]；强调色折线 + 同色低透明面积 + 极淡水平线，
    悬停（motion controller）显示圆点与 tooltip 数值。"""

    def __init__(self, format_value=None, bucket_kind="hour"):
        super().__init__()
        self._rows = []
        self._format = format_value or fmt_int
        self._bucket_kind = bucket_kind
        self._hover_index = -1
        self._geometry = []
        self.set_content_height(180)
        self.set_hexpand(True)
        self.set_draw_func(self._draw, None)
        motion = Gtk.EventControllerMotion()
        motion.connect("motion", self._on_motion)
        motion.connect("leave", self._on_leave)
        self.add_controller(motion)

    def set_rows(self, rows, format_value=None, bucket_kind=None):
        self._rows = list(rows or [])
        if format_value is not None:
            self._format = format_value
        if bucket_kind is not None:
            self._bucket_kind = bucket_kind
        self._hover_index = -1
        self.queue_draw()

    def do_measure(self, orientation, for_size):
        if orientation == Gtk.Orientation.VERTICAL:
            return (180, 180, -1, -1)
        return (200, 520, -1, -1)

    # ---- 悬停 -------------------------------------------------------------
    def _on_motion(self, controller, x, y):
        best, best_dist = -1, 1e9
        for i, (px, py) in enumerate(self._geometry):
            dist = abs(px - x)
            if dist < best_dist:
                best, best_dist = i, dist
        if best != self._hover_index:
            self._hover_index = best
            self.queue_draw()
        if 0 <= best < len(self._rows):
            label, value = self._rows[best]
            self.set_tooltip_text("%s · %s" % (
                fmt_bucket_label(label), self._format(value)))

    def _on_leave(self, controller):
        self._hover_index = -1
        self.queue_draw()

    # ---- 绘制 -------------------------------------------------------------
    def _draw(self, area, cr, width, height, data):
        self._geometry = []
        dim_fg = theme_color(self, None, dim=True)
        if not self._rows:
            draw_text(cr, _("No data"), width / 2.0, height / 2.0,
                      dim_fg, align_left=False, small=True)
            return
        pad_l, pad_r, pad_t, pad_b = 6.0, 44.0, 8.0, 18.0
        plot_w = max(10.0, width - pad_l - pad_r)
        plot_h = max(10.0, height - pad_t - pad_b)
        values = [float(v) for _, v in self._rows]
        vmax = max(values)
        if vmax <= 0:
            vmax = 1.0
        step = plot_w / float(max(1, len(values) - 1))

        def _px(i):
            return pad_l + i * step

        def _py(value):
            return pad_t + plot_h - value / vmax * plot_h

        # 极淡水平参考线（25/50/75%，前景色低透明——明暗主题都成立）
        grid = theme_color(self, None, alpha=0.12)
        cr.set_line_width(1.0)
        for frac in (0.25, 0.5, 0.75):
            y = pad_t + plot_h * (1.0 - frac)
            cr.set_source_rgba(*grid)
            cr.move_to(pad_l, y)
            cr.line_to(pad_l + plot_w, y)
            cr.stroke()
            draw_text(cr, self._format(vmax * frac), width - 4, y,
                      dim_fg, align_left=False, small=True)

        # 面积 + 折线（单点窗口画不出折线，画一个点避免“空图”观感）
        points = [(_px(i), _py(v)) for i, v in enumerate(values)]
        accent = theme_color(self, "accent_color")
        if len(points) == 1:
            px, py = points[0]
            cr.set_source_rgba(accent[0], accent[1], accent[2],
                               accent[3] * 0.25)
            cr.arc(px, py, 14, 0, 2 * math.pi)
            cr.fill()
            cr.set_source_rgba(*accent)
            cr.arc(px, py, 3.6, 0, 2 * math.pi)
            cr.fill()
            return
        cr.move_to(*points[0])
        for point in points[1:]:
            cr.line_to(*point)
        cr.line_to(points[-1][0], pad_t + plot_h)
        cr.line_to(points[0][0], pad_t + plot_h)
        cr.close_path()
        cr.set_source_rgba(accent[0], accent[1], accent[2], accent[3] * 0.14)
        cr.fill()
        cr.move_to(*points[0])
        for point in points[1:]:
            cr.line_to(*point)
        cr.set_line_width(1.8)
        cr.set_source_rgba(*accent)
        cr.stroke()

        # x 轴标签（首/中/尾，避免拥挤）
        n = len(points)
        label_idx = sorted({0, n // 2, n - 1})
        for i in label_idx:
            draw_text(cr, fmt_bucket_label(self._rows[i][0]),
                      min(max(_px(i), 14), width - pad_r), height - 6,
                      dim_fg, align_left=False, small=True)

        # 悬停指示
        for point in points:
            self._geometry.append(point)
        if 0 <= self._hover_index < len(points):
            px, py = points[self._hover_index]
            cr.set_source_rgba(accent[0], accent[1], accent[2],
                               accent[3] * 0.25)
            cr.set_line_width(1.0)
            cr.move_to(px, pad_t)
            cr.line_to(px, pad_t + plot_h)
            cr.stroke()
            cr.set_source_rgba(*accent)
            cr.arc(px, py, 3.4, 0, 2 * math.pi)
            cr.fill()


# ---------------------------------------------------------------------------
# StackedBarChart — 堆叠柱状图（每桶 input/output[/cache] 分段）
# ---------------------------------------------------------------------------
class StackedBarChart(Gtk.DrawingArea):
    """rows=[(label, [seg1, seg2, (seg3)])]；分段色见 STACK_SEGMENTS。"""

    def __init__(self, format_value=None):
        super().__init__()
        self._rows = []
        self._format = format_value or fmt_int
        self.set_content_height(190)
        self.set_hexpand(True)
        self.set_draw_func(self._draw, None)

    def set_rows(self, rows, format_value=None):
        self._rows = list(rows or [])
        if format_value is not None:
            self._format = format_value
        self.queue_draw()

    def do_measure(self, orientation, for_size):
        if orientation == Gtk.Orientation.VERTICAL:
            return (190, 190, -1, -1)
        return (200, 520, -1, -1)

    def _draw(self, area, cr, width, height, data):
        dim_fg = theme_color(self, None, dim=True)
        if not self._rows:
            draw_text(cr, _("No data"), width / 2.0, height / 2.0,
                      dim_fg, align_left=False, small=True)
            return
        pad_l, pad_r, pad_t, pad_b = 6.0, 52.0, 6.0, 18.0
        plot_w = max(10.0, width - pad_l - pad_r)
        plot_h = max(10.0, height - pad_t - pad_b)
        totals = [sum(max(0.0, float(v)) for v in segs)
                  for _, segs in self._rows]
        vmax = max(totals) or 1.0
        n = len(self._rows)
        slot = plot_w / n
        bar_w = max(4.0, min(34.0, slot * 0.62))
        max_segs = max((len(segs) for _, segs in self._rows), default=2)
        seg_colors = [_semantic(self, kind)
                      for kind in STACK_SEGMENTS[:max(2, max_segs)]]
        for i, (label, segs) in enumerate(self._rows):
            cx = pad_l + slot * (i + 0.5)
            y_bottom = pad_t + plot_h
            y_top = y_bottom
            for j, value in enumerate(segs):
                value = max(0.0, float(value))
                if value <= 0:
                    continue
                seg_h = value / vmax * plot_h
                cr.set_source_rgba(*seg_colors[j % len(seg_colors)])
                cr.rectangle(cx - bar_w / 2.0, y_top - seg_h, bar_w, seg_h)
                cr.fill()
                y_top -= seg_h
            n_idx = sorted({0, n // 2, n - 1})
            if i in n_idx:
                draw_text(cr, fmt_bucket_label(label), cx, height - 6,
                          dim_fg, align_left=False, small=True)
        draw_text(cr, self._format(vmax), width - 4, pad_t + 4,
                  dim_fg, align_left=False, small=True)


def stack_legend(names=None, kinds=None):
    """图例行（Gtk.Box）：ColorDot + 名称；默认输入/输出（+缓存读）分段。"""
    names = names or [_(name) for name in STACK_NAMES[:2]]
    kinds = kinds or STACK_SEGMENTS[:len(names)]
    box = Gtk.Box(orientation=Gtk.Orientation.HORIZONTAL, spacing=12)
    for name, kind in zip(names, kinds):
        item = Gtk.Box(orientation=Gtk.Orientation.HORIZONTAL, spacing=5)
        item.append(ColorDot(kind))
        label = Gtk.Label(label=name)
        label.add_css_class("dim-label")
        item.append(label)
        box.append(item)
    return box


# ---------------------------------------------------------------------------
# BarChart — 横条排行（可点击选中）
# ---------------------------------------------------------------------------
class BarChart(Gtk.DrawingArea):
    """数据：[(label, value)]；每行：标签 | 圆角条 | 数值。

    点击某行 → "row-activated"(label) 信号；set_selected(label) 高亮该行
    （用于“点击条目过滤下方明细表，再点取消”）。
    """

    __gsignals__ = {
        "row-activated": (GObject.SignalFlags.RUN_FIRST, None, (str,)),
    }

    def __init__(self):
        super().__init__()
        self._rows = []
        self._format = fmt_int
        self._selected = None
        self._row_y = []          # (y_top, y_bottom) 绘制时记录，点击命中用
        self.set_draw_func(self._draw, None)
        gesture = Gtk.GestureClick()
        gesture.connect("released", self._on_released)
        self.add_controller(gesture)

    def set_rows(self, rows, format_value=None):
        self._rows = list(rows or [])
        if format_value is not None:
            self._format = format_value
        self._selected = None
        self.queue_resize()
        self.queue_draw()

    def get_selected(self):
        return self._selected

    def set_selected(self, label):
        self._selected = label
        self.queue_draw()

    def do_measure(self, orientation, for_size):
        n = max(len(self._rows), 1)
        natural = n * BAR_ROW_HEIGHT + 2 * BAR_PADDING
        if orientation == Gtk.Orientation.VERTICAL:
            return (natural, natural, -1, -1)
        return (160, 420, -1, -1)

    def _on_released(self, gesture, n_press, x, y):
        for i, (y0, y1) in enumerate(self._row_y):
            if y0 <= y <= y1 and i < len(self._rows):
                self.emit("row-activated", str(self._rows[i][0]))
                break

    @staticmethod
    def _rounded_rect(cr, x, y, w, h, r):
        r = max(0.0, min(r, w / 2.0, h / 2.0))
        cr.new_sub_path()
        cr.arc(x + w - r, y + r, r, -math.pi / 2, 0)
        cr.arc(x + w - r, y + h - r, r, 0, math.pi / 2)
        cr.arc(x + r, y + h - r, r, math.pi / 2, math.pi)
        cr.arc(x + r, y + r, r, math.pi, 3 * math.pi / 2)
        cr.close_path()

    def _draw(self, area, cr, width, height, data):
        self._row_y = []
        fg = theme_color(self)
        dim_fg = theme_color(self, None, dim=True)
        accent = theme_color(self, "accent_color")
        if not self._rows:
            layout = PangoCairo.create_layout(cr)
            layout.set_text(_("No data"))
            cr.set_source_rgba(*dim_fg)
            PangoCairo.show_layout(cr, layout)
            return
        max_value = max(v for _, v in self._rows) or 1
        label_w = int(width * 0.30)
        value_w = max(52, int(width * 0.18))
        bar_x0 = label_w + 8
        bar_max = max(10, width - value_w - bar_x0 - 8)
        bar_h = 12
        for i, (label, value) in enumerate(self._rows):
            y = BAR_PADDING + i * BAR_ROW_HEIGHT
            self._row_y.append((y, y + BAR_ROW_HEIGHT))
            selected = self._selected is not None and label == self._selected
            # 标签（左，省略号）
            lay = PangoCairo.create_layout(cr)
            lay.set_width((label_w - 4) * Pango.SCALE)
            lay.set_ellipsize(Pango.EllipsizeMode.END)
            lay.set_text(str(label))
            cr.set_source_rgba(*fg)
            cr.move_to(0, y + (BAR_ROW_HEIGHT - lay.get_pixel_size()[1]) / 2.0)
            PangoCairo.show_layout(cr, lay)
            # 条（选中=实色，未选=低透明）
            frac = 0.0
            try:
                frac = max(0.0, float(value)) / float(max_value)
            except (TypeError, ValueError):
                frac = 0.0
            frac = min(1.0, frac)
            cr.set_source_rgba(accent[0], accent[1], accent[2],
                               accent[3] * (1.0 if selected else 0.7))
            bar_w = max(3.0, frac * bar_max)
            self._rounded_rect(cr, bar_x0, y + (BAR_ROW_HEIGHT - bar_h) / 2.0,
                               bar_w, bar_h, bar_h / 2.0)
            cr.fill()
            if selected:
                cr.set_line_width(2.0)
                cr.move_to(bar_x0, y + BAR_ROW_HEIGHT - 1)
                cr.line_to(bar_x0 + bar_max, y + BAR_ROW_HEIGHT - 1)
                cr.set_source_rgba(accent[0], accent[1], accent[2],
                                   accent[3] * 0.5)
                cr.stroke()
            # 数值（右）
            vlay = PangoCairo.create_layout(cr)
            vlay.set_text(self._format(value))
            vw, vh = vlay.get_pixel_size()
            cr.move_to(width - vw, y + (BAR_ROW_HEIGHT - vh) / 2.0)
            cr.set_source_rgba(*fg)
            PangoCairo.show_layout(cr, vlay)


# ---------------------------------------------------------------------------
# StatCard（兼容保留）
# ---------------------------------------------------------------------------
class StatCard(Gtk.Box):
    def __init__(self, title, value="—"):
        super().__init__(orientation=Gtk.Orientation.VERTICAL, spacing=4)
        self.add_css_class("card")
        self.add_css_class("padded-card")
        self.set_margin_top(6)
        self.set_margin_bottom(6)
        self.title_label = Gtk.Label(label=title, halign=Gtk.Align.START)
        self.title_label.add_css_class("dim-label")
        self.value_label = Gtk.Label(label=value, halign=Gtk.Align.START)
        self.value_label.add_css_class("title-2")
        self.append(self.title_label)
        self.append(self.value_label)

    def set_value(self, text):
        self.value_label.set_text(str(text))


# ---------------------------------------------------------------------------
# stat_cell — 卡片内统计单元（小标题 + 数值），用量页延迟/缓存卡用
# ---------------------------------------------------------------------------
def stat_cell(caption, value="…"):
    """→ (box, value_label)：caption 一行小字，数值 title-4。"""
    box = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=2)
    cap = Gtk.Label(label=caption, halign=Gtk.Align.START, xalign=0)
    cap.add_css_class("dim-label")
    cap.add_css_class("caption")
    value_label = Gtk.Label(label=value, halign=Gtk.Align.START, xalign=0)
    value_label.add_css_class("title-4")
    box.append(cap)
    box.append(value_label)
    return box, value_label


# ---------------------------------------------------------------------------
# 对话框 / 表单工具
# ---------------------------------------------------------------------------
def confirm_async(parent, heading, body, accept_label, callback,
                  destructive=False):
    """Adw.AlertDialog 确认；callback(ok: bool)；返回 dialog（测试驱动用）。

    响应激活后 AlertDialog 自行关闭——不要调 destroy/close（本机
    libadwaita 的 AlertDialog 无 destroy，回调前抛异常会把确认动作
    整个吞掉：挂起/吊销/删除“确认后无反应”的根源）。
    """
    dialog = Adw.AlertDialog(heading=heading, body=body)
    dialog.add_response("cancel", _("Cancel"))
    dialog.add_response("ok", accept_label)
    dialog.set_response_appearance(
        "ok", Adw.ResponseAppearance.DESTRUCTIVE if destructive
        else Adw.ResponseAppearance.SUGGESTED)
    dialog.set_default_response("cancel")
    dialog.set_close_response("cancel")

    def on_response(dlg, response):
        callback(response == "ok")

    dialog.connect("response", on_response)
    dialog.present(parent)
    return dialog


def button_row(title, on_activated, icon=None, css_class=None):
    """按钮行：libadwaita ≥1.6 用 Adw.ButtonRow，旧版（如 1.5 的
    ubuntu-24.04）回退为 ActionRow + 行尾图标按钮——签名统一，
    on_activated(row) 在激活时回调。"""
    if hasattr(Adw, "ButtonRow"):
        row = Adw.ButtonRow(title=title, start_icon_name=icon or "")
        if css_class:
            row.add_css_class(css_class)
        row.connect("activated", lambda _row: on_activated(_row))
        return row
    row = Adw.ActionRow(title=title)
    if css_class:
        row.add_css_class(css_class)
    button = Gtk.Button(valign=Gtk.Align.CENTER)
    if icon:
        button.set_icon_name(icon)
    button.add_css_class("flat")
    button.connect("clicked", lambda _b: on_activated(row))
    row.add_suffix(button)
    return row


def menu_button(items):
    """kebab 菜单：items=[(label, icon, callback, css_class_or_None)]。

    icon 仅作语义占位（GTK4 Button 设 icon 会吞掉 label，故不设置）。
    """
    box = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=2)
    box.set_margin_top(6)
    box.set_margin_bottom(6)
    box.set_margin_start(6)
    box.set_margin_end(6)
    for label, icon, callback, css in items:
        # 注意：不用 set_icon_name——GTK4 Button 设了 icon 会吞掉文字标签
        button = Gtk.Button(label=label)
        button.add_css_class("flat")
        if css:
            button.add_css_class(css)
        button.connect("clicked", callback)
        box.append(button)
    popover = Gtk.Popover(child=box)
    return Gtk.MenuButton(icon_name="open-menu-symbolic",
                          valign=Gtk.Align.CENTER, popover=popover)


def make_entry_row(title, text="", **kw):
    row = Adw.EntryRow(title=title, **kw)
    if text:
        row.set_text(text)
    return row


def make_password_row(title):
    return Adw.PasswordEntryRow(title=title)


def make_spin_row(title, value, low, high, step, subtitle=""):
    row = Adw.SpinRow(
        title=title, subtitle=subtitle,
        adjustment=Gtk.Adjustment(value=value, lower=low, upper=high,
                                  step_increment=step))
    return row


def make_switch_row(title, active, subtitle=""):
    return Adw.SwitchRow(title=title, subtitle=subtitle, active=active)


def make_textview_row(title, subtitle, height=110):
    """整行多行文本输入（key 白名单逐行输入）。

    长内容输入放 ActionRow 的 suffix 会挤成右侧一个窄胖框；这里标题/提示
    在上、文本框占满整行。返回 (widget, textview)，widget 直接加进
    PreferencesGroup。
    """
    box = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=6)
    box.set_margin_top(6)
    heading = Gtk.Label(label=title, halign=Gtk.Align.START)
    heading.add_css_class("heading")
    box.append(heading)
    if subtitle:
        hint = Gtk.Label(label=subtitle, halign=Gtk.Align.START, xalign=0,
                         wrap=True)
        hint.add_css_class("dim-label")
        hint.add_css_class("caption")
        box.append(hint)
    scrolled = Gtk.ScrolledWindow(height_request=height, hexpand=True)
    scrolled.set_policy(Gtk.PolicyType.AUTOMATIC, Gtk.PolicyType.AUTOMATIC)
    textview = Gtk.TextView(wrap_mode=Gtk.WrapMode.WORD_CHAR)
    textview.set_top_margin(8)
    textview.set_bottom_margin(8)
    textview.set_left_margin(8)
    textview.set_right_margin(8)
    textview.add_css_class("card")
    scrolled.set_child(textview)
    box.append(scrolled)
    return box, textview


def parse_alias_lines(text):
    """“alias=upstream” 逐行 → [(alias, upstream)]；缺 = 时 upstream=alias。"""
    out = []
    for line in (text or "").splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        if "=" in line:
            alias, upstream = line.split("=", 1)
            alias, upstream = alias.strip(), upstream.strip()
        else:
            alias = upstream = line
        if alias:
            out.append((alias, upstream or alias))
    return out


def parse_name_lines(text):
    """逐行名字 → [str]（key 模型白名单用）。"""
    return [line.strip() for line in (text or "").splitlines()
            if line.strip() and not line.strip().startswith("#")]
