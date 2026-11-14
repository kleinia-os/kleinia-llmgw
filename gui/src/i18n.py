#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""i18n.py — 轻量查表翻译（零外部工具：不用 gettext/.mo，本机无 msgfmt）。

  * ``_(msgid)``：英文 msgid → 当前 locale 译文；无匹配回落 msgid 本身。
  * 检测：``GLib.get_language_names()``（形如 ['zh_CN', 'zh', 'C', 'en']）
    依次匹配表 key——先全名（zh_CN）再语言部（zh）；惰性（首个 ``_()``
    调用时做一次并缓存），import 时无副作用，便于测试 mock/注入。
  * ``set_locale(None)`` 恢复自动检测；``set_locale("en")`` 强制英文
    （无表项 → 回落 msgid）。
  * 表即 {locale: {msgid: msgstr}}；打包进发行版时可原样迁正式 gettext
    （po 文件，msgid/译文一一对应）。
  * 专有名词（如 "OpenAI Chat Completions"）``_()`` 包裹但不进译文表——
    规范格式名不翻译，查表未命中自然回落原文。
"""

from gi.repository import GLib

# ---------------------------------------------------------------------------
# zh_CN 译文表（msgid 必须是源码中的英文原文；test_i18n 会 grep 全覆盖校验）
# ---------------------------------------------------------------------------
_ZH_CN = {
    " · AID %s": " · AID %s",
    " · disabled": " · 已停用",
    "$%s / 1M %s": "$%s / 1M %s",
    "%.1f min": "%.1f 分钟",
    "%d d ago": "%d 天前",
    "%d h ago": "%d 小时前",
    "%d min ago": "%d 分钟前",
    "%d source(s) · %d key(s) can call": "%d 个来源 · %d 把密钥可调用",
    "%d–%d of %d": "第 %d–%d 条，共 %d 条",
    "%s (%d consecutive failures%s)": "%s（连续失败 %d%s）",
    "%s has used %squota: %.0f%%": "%s 已用%s配额 %.0f%%",
    "%s reachable, %d ms latency": "%s 可达，延迟 %d ms",
    "%s rpm · %s tpm · %s tokens/day": "%s rpm · %s tpm · %s tok/日",
    "%s: %s": "%s：%s",
    "%s: %s applied": "已对 %s 执行 %s",
    "(no mappings)": "（无映射）",
    "(unknown)": "（未知）",
    ", cooling until %s": "，冷却至 %s",
    "0 = unlimited": "0 = 不限",
    "A key with this name already exists: %s": "已存在同名密钥：%s",
    "A source with this name already exists: %s": "已存在同名模型源：%s",
    "AID binding (optional, links to agent identity)": "AID 绑定（可选，关联到 agent 身份）",
    "Alias must not be empty": "别名不能为空",
    "All": "全部",
    "all models": "全部模型",
    "All time": "全部时间",
    "API key": "上游密钥",
    "API key set": "已配密钥",
    "About": "关于",
    "Action failed: %s": "操作失败：%s",
    "Add a source with model mappings on the Sources page and the catalog will appear here.": "先在“模型源”页添加一个源并配置模型映射，目录会出现在这里。",
    "Add": "添加",
    "Add mapping": "添加映射",
    "Add source": "添加源",
    "Add the first upstream on the Sources page": "在“模型源”页添加第一个上游",
    "Added source %s": "已添加源 %s",
    "Analysis window": "分析窗口",
    "App name": "应用名",
    "App name (unique)": "应用名（唯一）",
    "App name cannot be empty": "应用名不能为空",
    "Apps and Keys": "应用与密钥",
    "At least one model is required": "至少需要一个模型",
    "At least one price component must be enabled": "至少需要启用一个价格分量",
    "Authentication cancelled": "认证已取消",
    "Backend rejected the arguments": "后端拒绝了参数",
    "Base URL": "Base URL",
    "Base URL must not be empty": "Base URL 不能为空",
    "Base URL, e.g. https://api.example.com/v1": "Base URL，如 https://api.example.com/v1",
    "Basic information": "基本信息",
    "Bucketing follows the window": "分桶随窗口自适应",
    "Cache": "缓存",
    "Cache hit rate": "缓存命中率",
    "cache write": "缓存写",
    "Cache writes": "缓存写入",
    "Call logs": "调用日志",
    "Cancel": "取消",
    "Cannot connect to the model gateway backend": "无法连接模型网关后端",
    "Cannot connect to the model gateway backend%s": "无法连接模型网关后端%s",
    "Changes to %s saved": "已保存对 %s 的更改",
    "Click a model to set its USD price per 1M tokens; priced models get cost estimates on the Usage page": "点击模型设置每 1M token 的美元单价；已定价的模型会在用量页估算成本",
    "Client format": "客户端协议",
    "Configured": "已配置",
    "Connecting…": "连接中…",
    "Copied to clipboard": "已复制到剪贴板",
    "Copy to clipboard": "复制到剪贴板",
    "Cost": "成本",
    "Cost estimates for %s will stop (usage history is kept)": "%s 将停止成本估算（历史用量保留）",
    "Custom mapping": "自定义映射",
    "Custom mappings (optional)": "自定义映射（可选）",
    "D-Bus connection closed": "D-Bus 连接已断开",
    "Daily token quota": "日 token 配额",
    "Degraded": "降级路由",
    "Delete": "删除",
    "Delete failed: %s": "删除失败：%s",
    "Delete source?": "删除模型源",
    "Delete “%s”? Keys in use will no longer route to this source.": "删除 “%s”？已发放密钥的请求将不再路由到该源。",
    "Deleted source %s": "已删除源 %s",
    "Dimension": "维度",
    "Dimension ranking": "维度排行",
    "Disconnected": "未连接",
    "Duplicate alias across groups: %s": "两组中存在重复别名：%s",
    "Duplicate model name: %s": "重复的模型名：%s",
    "Each app/agent holds one sk-kln-* key; model allowlists and quotas apply per key": "每个应用/agent 持有一把 sk-kln-* 密钥，模型白名单与限额按 key 生效",
    "Edit": "编辑",
    "Edit key": "编辑密钥",
    "Edit price": "修改定价",
    "Edit source": "编辑模型源",
    "Edit…": "编辑…",
    "Enable only the components this model bills; cache read is a carve-out of input tokens": "只启用该模型实际计费的分量；缓存读是输入 token 的切出量",
    "Enabled": "启用",
    "Error kind": "错误类型",
    "Error rate": "错误率",
    "Errors": "错误",
    "Filtered: %s (click the same item to clear)": "已筛选：%s（点击同一条目取消）",
    "Health": "健康状态",
    "Higher number wins; equal numbers share a tier": "大者优先，同优先级为同层",
    "Higher priority number wins; a failed tier falls through to the next": "优先级数值大者优先；整层失败时降级到下一层",
    "In / out stacked per bucket (filtered by the ranking below)": "每桶输入 / 输出堆叠（受下方排行筛选）",
    "Issue": "发放",
    "Issue failed: %s": "发放失败：%s",
    "Issue key": "发放密钥",
    "Key already revoked": "密钥已吊销",
    "Key cannot be resumed after revocation": "密钥已吊销，无法恢复",
    "Key cannot be suspended after revocation": "密钥已吊销，无法挂起",
    "Key is already suspended": "密钥已是挂起状态",
    "Key is not suspended": "密钥未处于挂起状态",
    "Key issued: %s": "密钥已发放：%s",
    "Key settings": "密钥设置",
    "Key updated: %s": "已更新密钥：%s",
    "Latency": "延迟",
    "Latest %d requests (click for usage details)": "最近 %d 条请求（点击查看用量明细）",
    "Management UI for llmgwd, the Kleinia system model gateway: aggregated upstream sources with per-alias model mapping, per-app keys with quota windows, priority + weight routing, and usage accounting.": "llmgwd（Kleinia 系统级模型网关）的管理界面：多上游模型源与别名映射、按应用发放的密钥与配额、优先级 + 权重的分层路由、用量记账。",
    "Metadata": "元信息",
    "Metric": "指标",
    "Model catalog": "模型目录",
    "Model catalog and pricing": "模型目录与定价",
    "Model Gateway": "模型网关",
    "Model allowlist": "模型白名单",
    "Model gateway quota warning": "模型网关配额告警",
    "Model mappings": "模型映射",
    "Model names offered through this source": "该源对外提供的模型名",
    "Models": "模型",
    "Name (unique)": "名称（唯一）",
    "Name must not be empty": "名称不能为空",
    "new": "新",
    "New API key (leave empty to keep the current one)": "新 API 密钥（留空 = 保持现有密钥）",
    "New key": "新密钥",
    "Next": "下一页",
    "no": "否",
    "No alerts": "暂无告警",
    "No cache activity in this window": "所选窗口无缓存活动",
    "No data": "暂无数据",
    "No matching requests": "没有匹配的请求",
    "No models yet": "还没有模型",
    "No price set for %s": "%s 未设置定价",
    "No requests yet": "暂无请求",
    "No sources yet": "尚无模型源",
    "No such key: %s": "没有这个密钥：%s",
    "No such source: %s": "没有这个模型源：%s",
    "Not authorized for this operation (admin consent required)": "无权执行该操作（需要管理员授权）",
    "Not configured": "未配置",
    "Not connected to the llmgwd backend yet": "尚未连接到 llmgwd 后端",
    "OK": "成功",
    "One alias per line; a single * means unrestricted": "每行一个别名；单独一个 * 表示不限",
    "One alias per line; empty or a single * means unrestricted": "每行一个别名；留空或单独一个 * 表示不限",
    "Only needed when the same model is named differently upstream (public alias → actual upstream model name)": "仅当同一模型在该上游叫不同名字时才需要（对外别名 → 上游实际模型名）",
    "Open call logs": "打开调用日志",
    "Overview": "概览",
    "p50 latency": "p50 延迟",
    "p95 latency": "p95 延迟",
    "Peer connection failed: %s": "对等连接失败：%s",
    "Performance": "性能",
    "Permission denied (polkit authorization failed)": "权限被拒绝（polkit 授权失败）",
    "Previous": "上一页",
    "Price must not be negative": "价格不能为负数",
    "Price removed: %s": "已清除定价：%s",
    "Price saved: %s": "已保存定价：%s",
    "Price: %s": "定价：%s",
    "Prices must be non-negative numbers, e.g. 0.14": "价格必须是非负数字，如 0.14",
    "Priority": "优先级",
    "Priority / weight": "优先级 / 权重",
    "Probe failed: %s": "探测失败：%s",
    "Probe succeeded": "探测成功",
    "Quota warnings and source health changes (this session)": "配额告警与源健康变化（本会话内记录）",
    "read %s / in %s": "读 %s / 入 %s",
    "Reasoning": "推理 token",
    "Remove": "删除",
    "Remove price?": "清除定价？",
    "Remove price": "清除定价",
    "Request details": "请求详情",
    "Request ID": "请求 ID",
    "RPM ": "RPM ",
    "RPM (requests per minute)": "RPM（每分钟请求数）",
    "Reachable (degraded: %d consecutive failures)": "可达（degraded：%d 次连续失败）",
    "Recent activity": "最近活动",
    "Recent alerts": "最近告警",
    "Recent requests": "最近请求",
    "Reconnecting…": "正在重试连接…",
    "Remove source": "删除源",
    "Remove this mapping": "删除此映射",
    "Remove this model": "删除此模型",
    "Requests": "请求数",
    "Requests sent through the gateway will appear here": "经网关发出请求后将显示在这里",
    "Resume": "恢复",
    "Retry": "重试",
    "Revoke": "吊销",
    "Save changes": "保存更改",
    "Save failed: %s": "保存失败：%s",
    "Save the key now — it cannot be shown again after closing.": "请立即保存密钥——关闭本窗口后将无法再次查看。",
    "Scheduling": "调度",
    "Session": "会话",
    "Set price": "设置定价",
    "Share of weighted random picks within a tier (effective weight = weight + 10)": "同层内加权随机的份额（有效权重 = 权重 + 10）",
    "Source #%s health changed: %s (%s)": "源 #%s 健康变化：%s（%s）",
    "Source health": "来源健康",
    "Source health: %s %s": "源健康：%s %s",
    "Source is in %s state, probe failed": "源处于 %s 状态，探测失败",
    "Source not found": "源不存在",
    "Sources": "模型源",
    "Streaming": "流式占比",
    "Summary": "摘要",
    "Suspend": "挂起",
    "Time": "时间",
    "Tokens": "Token",
    "TPM ": "TPM ",
    "TPM (tokens per minute)": "TPM（每分钟 token）",
    "Test connectivity": "测试连通性",
    "Test of %s failed: %s": "测试 %s 失败：%s",
    "Time range": "时间范围",
    "Token usage": "Token 用量",
    "Tokens (in / out)": "Token（入 / 出）",
    "Top %d (by tokens); click an item to filter the chart, click again to clear": "Top %d（按 token）；点击条目过滤图表，再点取消",
    "Top %d (by tokens); click an item to filter the chart and details, click again to clear": "Top %d（按 token）；点击条目过滤图表与明细，再点取消",
    "Trend": "趋势",
    "TTFT": "TTFT",
    "TTFT median": "TTFT 中位",
    "Type": "类型",
    "Type a model name and press Enter": "输入模型名，回车添加",
    "Type must be one of %s": "类型必须是 %s 之一",
    "unpriced": "未定价",
    "Update failed: %s": "更新失败：%s",
    "Upstream aggregation: each source maps aliases to upstream models": "上游聚合：每个源提供若干别名→上游模型的映射",
    "Upstream model": "上游模型",
    "Upstream protocol": "上游协议",
    "Usage": "用量",
    "Weight": "权重",
    "active": "活跃",
    "alias": "别名",
    "alias = upstream": "别名 = 上游模型名",
    "base_url must start with http(s)://": "base_url 必须以 http(s):// 开头",
    "bucket must be hour|day": "bucket 必须是 hour|day",
    "cache read": "缓存读",
    "cooling": "冷却中",
    "daily": "今日",
    "degraded": "降级",
    "disabled": "已停用",
    "down": "不可用",
    "healthy": "正常",
    "ident": "ident",
    "in": "入",
    "input": "输入",
    "just now": "刚刚",
    "key": "密钥",
    "latency": "延迟",
    "llmgwd has left the bus": "llmgwd 已从总线消失",
    "llmgwd is not registered on the bus": "llmgwd 服务未在总线上注册",
    "model": "模型",
    "model alias": "模型别名",
    "no key": "无密钥",
    "out": "出",
    "output": "输出",
    "priority %d · weight %d · %s%s": "优先级 %d · 权重 %d · %s%s",
    "priority %d · weight %d%s": "优先级 %d · 权重 %d%s",
    "revoked": "已吊销",
    "source": "模型源",
    "status": "状态",
    "suspended": "已挂起",
    "time": "时间",
    "time / ident / model / tokens in·out / latency / status": "时间 / ident / 模型 / 入出 token / 延迟 / 状态",
    "unknown": "未知",
    "unknown dimension: %s": "未知 dimension：%s",
    "upstream": "上游",
    "What clients can request: one row per alias, with the sources serving it and how many active keys may call it": "客户端可请求的模型目录：每行一个别名，标注由哪些源提供、多少把活跃密钥可调用",
    "yes": "是",
    "yesterday": "昨天",
}

_TRANSLATIONS = {
    "zh_CN": _ZH_CN,
}
# 语言部回落：zh/zh_HK/zh_SG … 都先用 zh_CN 表
_TRANSLATIONS["zh"] = _ZH_CN

_NO_LOCALE = object()
_current = _NO_LOCALE


def detect_locale():
    """GLib.get_language_names() 依次匹配表 key；无匹配 → None（回落英文）。"""
    try:
        names = GLib.get_language_names() or []
    except Exception:
        return None
    for name in names:
        key = str(name).split(".")[0].split("@")[0]
        if key in _TRANSLATIONS:
            return key
        base = key.split("_", 1)[0]
        if base in _TRANSLATIONS:
            return base
    return None


def set_locale(locale):
    """注入/覆盖当前 locale（测试用）；``None``/空串恢复自动检测。

    与 detect_locale 相同的归一化：先全名（zh_CN）再语言部（zh），
    均无匹配则原样保留（查表未命中 → 回落英文 msgid）。
    """
    global _current
    if not locale:
        _current = _NO_LOCALE
        return
    key = str(locale).split(".")[0].split("@")[0]
    if key in _TRANSLATIONS:
        _current = key
        return
    base = key.split("_", 1)[0]
    _current = base if base in _TRANSLATIONS else key


def get_locale():
    """当前生效 locale（惰性检测并缓存；未匹配返回 None）。"""
    global _current
    if _current is _NO_LOCALE:
        _current = detect_locale()
    return _current if _current != _NO_LOCALE else None


def _(msgid, *args):
    """查表翻译；未命中/无 locale 回落 msgid；args 提供时做 % 格式化。"""
    if not msgid:
        return msgid
    table = _TRANSLATIONS.get(get_locale())
    text = msgid
    if table:
        translated = table.get(msgid)
        if translated is not None:
            text = translated
    if args:
        try:
            text = text % (args if len(args) > 1 else args[0])
        except (TypeError, ValueError):
            pass
    return text
