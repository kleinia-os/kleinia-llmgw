#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""fmt.py — 数字格式化工具（纯 stdlib，无 GTK；tests/test_fmt.py 覆盖）。

约定（通用用量页惯例）：
  * token：1.2K / 3.4M（大写单位，一位小数）
  * 成本：$0.004（3 位小数）；无价（None）→ "—"
  * 延迟：自适应单位 850 ms / 1.2 s
  * 相对时间：刚刚 / 5 分钟前 / 3 小时前 / 昨天 / 7 天前
  * 错误率/占比：0..1 分数 → 12.3%（一位小数）
  * 环比角标：↑ +10.0% / ↓ -3.2% / 新（前窗口无数据，走 _() 翻译）/
    None（无可比）
"""

import time

from i18n import _  # noqa: E402  （相对时间等单位文案随界面语言）


# ---------------------------------------------------------------------------
# token / 成本
# ---------------------------------------------------------------------------
def fmt_tokens(value):
    """token 数人性化：0、940、1.2K、3.4M、1.1G；坏值 → "—"。"""
    try:
        v = float(value)
    except (TypeError, ValueError):
        return "—"
    if v < 0:
        return "—"
    for unit, factor in (("", 1), ("K", 1e3), ("M", 1e6), ("G", 1e9)):
        if v < 1000 or unit == "G":
            if unit == "":
                return "{:,}".format(int(round(v)))
            return "%.1f%s" % (v, unit)
        v /= 1000.0
    return "%.1fG" % v


def fmt_cost(value):
    """成本 $0.004（3 位小数）；None → "—"（无价表）。"""
    if value is None:
        return "—"
    try:
        v = float(value)
    except (TypeError, ValueError):
        return "—"
    if v < 0:
        return "—"
    return "$%.3f" % v


def fmt_int(value):
    try:
        return "{:,}".format(int(value))
    except (TypeError, ValueError):
        return "—"


# ---------------------------------------------------------------------------
# 延迟 / 占比
# ---------------------------------------------------------------------------
def fmt_latency(ms):
    """自适应：850 ms / 1.2 s / 3.5 分钟；坏值 → "—"。"""
    try:
        ms = float(ms)
    except (TypeError, ValueError):
        return "—"
    if ms < 0:
        return "—"
    if ms < 1000:
        return "%.0f ms" % ms
    if ms < 60_000:
        return "%.1f s" % (ms / 1000.0)
    return _("%.1f min") % (ms / 60_000.0)


def fmt_pct1(fraction):
    """0..1 分数 → "12.3%"（一位小数）。"""
    try:
        return "%.1f%%" % (float(fraction) * 100.0)
    except (TypeError, ValueError):
        return "—"


# ---------------------------------------------------------------------------
# 相对时间
# ---------------------------------------------------------------------------
def fmt_rel_time(ts, now=None):
    """epoch 秒 → 刚刚 / N 分钟前 / N 小时前 / 昨天 / N 天前。"""
    try:
        ts = float(ts)
    except (TypeError, ValueError):
        return "—"
    if now is None:
        now = time.time()
    delta = now - ts
    if delta < 0:
        delta = 0.0
    if delta < 60:
        return _("just now")
    if delta < 3600:
        return _("%d min ago") % int(delta // 60)
    if delta < 86400:
        return _("%d h ago") % int(delta // 3600)
    if delta < 2 * 86400:
        return _("yesterday")
    return _("%d d ago") % int(delta // 86400)


def fmt_bucket_label(label):
    """Histogram 桶标签 → 短标签：小时桶 "14:00"、日桶 "10-28"。"""
    text = str(label)
    if "T" not in text:
        return text
    date_part, _, time_part = text.partition("T")
    time_part = time_part.replace("Z", "")
    if time_part.startswith("00:00"):
        # 日桶（store 格式 …T00:00:00Z）→ 月-日
        if "-" in date_part:
            y, m, d = date_part.split("-")
            return "%s-%s" % (m.lstrip("0"), d.lstrip("0"))
        return date_part
    return time_part[:5]


# ---------------------------------------------------------------------------
# 环比角标
# ---------------------------------------------------------------------------
def fmt_delta(current, previous):
    """环比 → "↑ +10.0%" / "↓ -3.2%" / _("new") / None（无可比）。

    颜色语义（升=好还是升=坏）由调用者按指标决定：
    ``css = "success" if text.startswith("↑") == up_is_good else "error"``；
    "new"（前窗口无数据）为中性角标，不带涨跌色。
    """
    try:
        cur = float(current)
        prev = float(previous)
    except (TypeError, ValueError):
        return None
    if prev < 0 or cur < 0:
        return None
    if prev == 0:
        return _("new") if cur > 0 else None
    pct = (cur - prev) / prev * 100.0
    arrow = "↑" if pct >= 0 else "↓"
    return "%s %+.1f%%" % (arrow, pct)
