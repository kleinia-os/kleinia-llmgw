"""backend.py — io.kleinia.ModelGateway 的后端抽象层。

两类可互换实现（polman app 的 polman-client.py 架构范本）：

  * ``MockBackend``  进程内假数据 + 5s 模拟数据变化（``live=True`` 时），
                     纯 Python/GLib，无 D-Bus、无 GTK——unittest 直接可测；
  * ``DBusBackend``  真实 llmgwd 管理面（``internal/dbusapi``，APIVer 2，
                     已实测联调）。默认 system 总线
                     ``io.kleinia.ModelGateway`` @ ``/io/kleinia/ModelGateway``；
                     ``--dbus-address unix:path=/tmp/x`` 走对等连接（对应
                     llmgwd ``--dbus-p2p=<path>``；宿主无 dbus-daemon 时的
                     联调通道，p2p 跳过 polkit）。

对外（对窗口/页面层）两者完全同构：全部是同步查询/变更方法（失败抛
``BackendError``）+ GObject 信号驱动刷新（禁止轮询）：

  信号：
    connected()                        后端就绪（manager 属性可用）
    disconnected(str reason)           连接断开/失败（UI 显示 Banner + 重试）
    sources-changed()                  源列表/状态变化，应重拉
    keys-changed()                     key 列表变化
    usage-changed()                    用量数据变化
    source-health(str id, str state, str reason)   SourceHealthChanged(u,s,s)
    quota-warning(str ident, str window, float pct) QuotaWarning(s,s,d)

D-Bus 线形状（复杂载荷 JSON 字符串 s/as，其余浅类型参数；详见
``DBusBackend`` 类注释与 internal/dbusapi/api.go）：
ListSources()→as；AddSource(s)→u；UpdateSource(u,s)→()；RemoveSource(u)→()；
SetPriority/SetWeight(u,i)→()；TestSource(u)→s；ListKeys()→as；
IssueKey(s,s)→(s,u)；Suspend/Resume/RevokeKey(s)→()；Summary(s)→as；
Recent(u)→as；KeyWindows(s)→s。变更方法成功后由本客户端主动发
sources-changed/keys-changed 刷新 UI（真实后端变更无返回值也不广播）。
"""

import json
import random
import threading
import time
from dataclasses import dataclass, field

import gi

gi.require_version("Gio", "2.0")
gi.require_version("GLib", "2.0")
gi.require_version("GObject", "2.0")
from gi.repository import Gio, GLib, GObject  # noqa: E402

from i18n import _  # noqa: E402

# ---------------------------------------------------------------------------
# 常量（契约 §：D-Bus 管理 API）
# ---------------------------------------------------------------------------
BUS_NAME = "io.kleinia.ModelGateway"
OBJECT_PATH = "/io/kleinia/ModelGateway"
IFACE_MANAGER = "io.kleinia.Manager1"
IFACE_SOURCES = "io.kleinia.Sources1"
IFACE_KEYS = "io.kleinia.Keys1"
IFACE_USAGE = "io.kleinia.Usage1"
IFACE_MODELS = "io.kleinia.Models1"

# 内部类型键（与 wire 侧 codec 注册一致）：openai=Chat Completions 传输、
# openai-responses=OpenAI Responses 传输、anthropic=Anthropic Messages 传输。
SOURCE_TYPES = ("openai", "openai-responses", "anthropic")
# 规范显示名（大驼峰专有名词，_() 包裹但不进译文表——查表未命中自然回落）
SOURCE_TYPE_LABELS = {
    "openai": "OpenAI Chat Completions",
    "openai-responses": "OpenAI Responses",
    "anthropic": "Anthropic Messages",
}
HEALTH_STATES = ("ok", "degraded", "cooling", "down", "unknown")

GLM_VERSION = "0.1.0"

# DBusBackend 读缓存：call_sync 全部走 UI 线程，六页齐刷一次会把主循环
# 卡住几百毫秒（侧栏点击丢失/切换迟滞的根源）。读方法一律过 _cached()：
# 冷读同步一次（首帧不闪空态），之后 TTL 内直接回缓存值；过期则先回旧值、
# 由工作线程回源，完成后仅在数据变化时于主线程广播 signal 刷新页面。
CACHE_TTL = 2.0            # 秒：读缓存有效期
POLL_USAGE_SECONDS = 5     # 用量类（stats/histogram/recent）主动回源周期
POLL_STATE_SECONDS = 20    # sources/keys 慢轮询（捕捉 llmgwctl 等外部改动）


class BackendError(Exception):
    """后端操作失败（含 polkit 拒绝、D-Bus 错误、校验错误）。

    ``kind`` 为稳定错误短名（NoSuchSource/AccessDenied/...），``message``
    为面向用户的人读文案。
    """

    def __init__(self, message, kind="Failed"):
        super().__init__(message)
        self.kind = kind
        self.message = message


# ---------------------------------------------------------------------------
# 数据模型（纯 dataclass + 解析函数，无 GTK 依赖；tests/test_models.py 覆盖）
# ---------------------------------------------------------------------------
@dataclass
class ModelMapping:
    alias: str
    upstream: str = ""


@dataclass
class SourceHealth:
    state: str = "unknown"       # ok | degraded | cooling | down | unknown
    fails: int = 0
    cooling_until: str = ""      # ISO 时间戳，可空


@dataclass
class Source:
    id: object                   # 后端分配（整数 rowid 或字符串）
    name: str
    type: str = "openai"
    base_url: str = ""
    has_key: bool = False
    priority: int = 10
    weight: int = 100
    enabled: bool = True
    models: list = field(default_factory=list)   # [ModelMapping]
    health: SourceHealth = field(default_factory=SourceHealth)


@dataclass
class Key:
    id: object
    name: str
    enabled: bool = True
    suspended: bool = False
    models: list = field(default_factory=list)   # ["*"] 或别名白名单
    rpm: int = 0                                 # 0 = 不限
    tpm: int = 0
    daily_tokens: int = 0
    aid: str = ""
    created_at: str = ""
    revoked_at: str = ""

    @property
    def status(self):
        if self.revoked_at:
            return "revoked"
        if self.suspended or not self.enabled:
            return "suspended"
        return "active"


@dataclass
class ModelPrice:
    """一个模型别名的定价（USD / 1M tokens，四分量，按需启用）。

    None = 该分量未启用（不计费；缓存读未启用时并入输入价）；
    0 = 启用且免费。计费基数：缓存读是输入的切出量，
    缓存写是独立维度（与守护进程 server.Pricer 一致）。
    """
    alias: str
    input_per_mtok: float = None
    output_per_mtok: float = None
    cache_read_per_mtok: float = None
    cache_write_per_mtok: float = None


@dataclass
class UsageRow:
    ts: float                    # epoch 秒
    ident: str                   # 应用/agent 身份（key 名）
    model: str                   # 别名
    source: str = ""
    agent: str = ""
    in_tokens: int = 0
    out_tokens: int = 0
    latency_ms: int = 0
    status: str = "ok"
    error_kind: str = ""         # real 行的错误分类（错误行红点 tooltip 用）
    # ---- 详情字段（Logs 页详情弹层；mock 部分为合成值） ----
    req_id: str = ""
    upstream_model: str = ""
    client_format: str = ""      # chat | responses | anthropic
    cache_read: int = 0
    cache_write: int = 0
    reasoning: int = 0
    cost: float = None           # USD；None = 未定价
    ttft_ms: int = 0
    stream: bool = False
    degraded: bool = False
    session: str = ""

    @property
    def tokens(self):
        return self.in_tokens + self.out_tokens

    @property
    def is_error(self):
        return bool(self.error_kind) or self.status != "ok"


@dataclass
class UsageSummary:
    since_hours: int = 24
    requests: int = 0
    tokens_in: int = 0
    tokens_out: int = 0
    active_keys: int = 0
    sources_health: dict = field(default_factory=dict)  # name -> state

    @property
    def tokens(self):
        return self.tokens_in + self.tokens_out


@dataclass
class KeyWindow:
    window: str                  # rpm | tpm | daily_tokens
    used: int = 0
    limit: int = 0               # 0 = 不限

    @property
    def pct(self):
        return (100.0 * self.used / self.limit) if self.limit else 0.0


def _as_bool(value, default=False):
    if isinstance(value, bool):
        return value
    if isinstance(value, (int, float)):
        return bool(value)
    if isinstance(value, str):
        return value.lower() in ("1", "true", "yes", "on")
    return default


def _as_int(value, default=0):
    try:
        return int(value)
    except (TypeError, ValueError):
        return default


def _as_float_or_none(value):
    """None/缺失 → None（分量未启用）；其余走 _as_float。"""
    if value is None:
        return None
    return _as_float(value)


def _as_float(value, default=0.0):
    try:
        return float(value)
    except (TypeError, ValueError):
        return default


def _as_str(value, default=""):
    if value is None:
        return default
    return str(value)


def _first(d, *names, default=None):
    """按候选键名顺序取第一个非 None 的字段（兼容 mock/real 两套键名）。"""
    for name in names:
        if name in d and d[name] is not None:
            return d[name]
    return default


def _as_ts(value, default=0.0):
    """epoch 或 RFC3339 字符串 → epoch 秒。"""
    v = _as_float(value, None)
    if v is not None:
        return v
    text = _as_str(value, "")
    if text:
        try:
            from datetime import datetime
            parsed = datetime.fromisoformat(text.replace("Z", "+00:00"))
            if parsed.tzinfo is None:
                from datetime import timezone
                parsed = parsed.replace(tzinfo=timezone.utc)
            return parsed.timestamp()
        except ValueError:
            return default
    return default


def _models_list(value):
    """real 后端 models 是字符串（"*" 或逗号分隔）；mock 是列表。"""
    if isinstance(value, str):
        parts = [p.strip() for p in value.split(",") if p.strip()]
        return parts or ["*"]
    if isinstance(value, (list, tuple)):
        return [_as_str(m) for m in value if _as_str(m)]
    return []


def _status_label(value, default="ok"):
    """real 后端 status 是 HTTP 整数；mock 是字符串。"""
    if isinstance(value, str):
        return value or default
    code = _as_int(value, 0)
    if 200 <= code < 300 or code == 0:
        return "ok"
    return "HTTP %d" % code if code else default


def _source_id(value):
    """id 容错：数字样字符串转 int，其余原样。"""
    if isinstance(value, bool):
        return value
    if isinstance(value, (int, float)):
        return int(value)
    if isinstance(value, str) and value.isdigit():
        return int(value)
    return value


def parse_source(d):
    """Source JSON → Source dataclass；未知字段忽略，缺字段给默认。"""
    health = d.get("health") or {}
    models = [ModelMapping(alias=_as_str(m.get("alias")),
                           upstream=_as_str(m.get("upstream")))
              for m in (d.get("models") or [])
              if isinstance(m, dict)]
    return Source(
        id=_source_id(d.get("id")),
        name=_as_str(d.get("name")),
        type=_as_str(d.get("type"), "openai"),
        base_url=_as_str(d.get("base_url")),
        has_key=_as_bool(d.get("has_key")),
        priority=_as_int(d.get("priority"), 10),
        weight=_as_int(d.get("weight"), 100),
        enabled=_as_bool(d.get("enabled"), True),
        models=models,
        health=SourceHealth(
            state=_as_str(health.get("state"), "unknown") or "unknown",
            fails=_as_int(health.get("fails")),
            cooling_until=_as_str(health.get("cooling_until")),
        ),
    )


def parse_key(d):
    """Key JSON → Key dataclass；status 派生：revoked_at>suspended>active。

    real 后端 models 为字符串（"*" 或逗号分隔），rpm/tpm/daily_tokens 可为
    null（不限）；均做归一化。
    """
    return Key(
        id=_source_id(d.get("id")),
        name=_as_str(d.get("name")),
        enabled=_as_bool(d.get("enabled"), True),
        suspended=_as_bool(d.get("suspended")),
        models=_models_list(d.get("models")),
        rpm=_as_int(_first(d, "rpm", default=0)),
        tpm=_as_int(_first(d, "tpm", default=0)),
        daily_tokens=_as_int(_first(d, "daily_tokens", default=0)),
        aid=_as_str(d.get("aid")),
        created_at=_as_str(d.get("created_at")),
        revoked_at=_as_str(d.get("revoked_at")),
    )


def parse_usage_row(d):
    """UsageRow：兼容 mock（ts epoch/ident/model/...）与 real
    （ts RFC3339/key_name/model_alias/input_tokens/status 整数/...）。"""
    return UsageRow(
        ts=_as_ts(_first(d, "ts", default=0.0), 0.0) or time.time(),
        ident=_as_str(_first(d, "ident", "key_name", default="")),
        model=_as_str(_first(d, "model", "model_alias", default="")),
        source=_as_str(_first(d, "source", "source_name", default="")),
        agent=_as_str(_first(d, "agent", "aid", default="")),
        in_tokens=_as_int(_first(d, "in_tokens", "input_tokens", default=0)),
        out_tokens=_as_int(_first(d, "out_tokens", "output_tokens", default=0)),
        latency_ms=_as_int(d.get("latency_ms")),
        status=_status_label(_first(d, "status", default="ok")),
        error_kind=_as_str(d.get("error_kind")),
        req_id=_as_str(_first(d, "req_id", default="")),
        upstream_model=_as_str(_first(d, "upstream_model", "upstream",
                                       default="")),
        client_format=_as_str(_first(d, "client_format", "via", default="")),
        cache_read=_as_int(_first(d, "cache_read", "cache_read_tokens",
                                   default=0)),
        cache_write=_as_int(_first(d, "cache_write", "cache_write_tokens",
                                    default=0)),
        reasoning=_as_int(_first(d, "reasoning", "reasoning_tokens",
                                  default=0)),
        cost=_as_float_or_none(_first(d, "cost", "cost_est", default=None)),
        ttft_ms=_as_int(_first(d, "ttft_ms", "ttft", default=0)),
        stream=_as_bool(_first(d, "stream", default=False), False),
        degraded=_as_bool(d.get("degraded"), False),
        session=_as_str(d.get("session")),
    )


def parse_summary(d, since_hours=24):
    return UsageSummary(
        since_hours=_as_int(d.get("since_hours"), since_hours),
        requests=_as_int(d.get("requests")),
        tokens_in=_as_int(d.get("tokens_in") or d.get("in_tokens")),
        tokens_out=_as_int(d.get("tokens_out") or d.get("out_tokens")),
        active_keys=_as_int(d.get("active_keys")),
        sources_health=dict(d.get("sources_health") or {}),
    )


def parse_key_windows(d):
    """KeyWindows JSON → [KeyWindow]；兼容 list 或 {"windows": [...]}。"""
    items = d.get("windows") if isinstance(d, dict) else d
    out = []
    for w in items or []:
        if not isinstance(w, dict):
            continue
        out.append(KeyWindow(
            window=_as_str(w.get("window") or w.get("name")),
            used=_as_int(w.get("used")),
            limit=_as_int(w.get("limit")),
        ))
    return out


@dataclass
class HistBucket:
    """Usage1.Histogram 的一个桶（store.HistBucket JSON）。"""
    label: str                   # 桶起点 RFC3339（小时桶 …T10:00:00Z / 日桶 …T00:00:00Z）
    group: str = ""              # 维度值；dimension="" 时恒为 ""
    requests: int = 0
    in_tokens: int = 0
    out_tokens: int = 0
    cost: object = None          # float 或 None（无价表）

    @property
    def tokens(self):
        return self.in_tokens + self.out_tokens


@dataclass
class StatsSummary:
    """Usage1.Stats 的窗口指标（store.Stats JSON）；全 0 = 无样本。"""
    requests: int = 0
    in_tokens: int = 0
    out_tokens: int = 0
    cache_read: int = 0
    cache_write: int = 0
    reasoning: int = 0
    cost: object = None          # float 或 None
    latency_p50_ms: int = 0
    latency_p95_ms: int = 0
    ttft_p50_ms: int = 0
    error_rate: float = 0.0      # 0..1
    active_ids: int = 0
    streams: int = 0

    @property
    def tokens(self):
        return self.in_tokens + self.out_tokens

    @property
    def has_samples(self):
        return self.requests > 0


def parse_hist_bucket(d):
    """HistBucket JSON → dataclass（兼容 in/in_tokens 两套键名）。"""
    return HistBucket(
        label=_as_str(d.get("label")),
        group=_as_str(d.get("group")),
        requests=_as_int(d.get("requests")),
        in_tokens=_as_int(_first(d, "in", "in_tokens", default=0)),
        out_tokens=_as_int(_first(d, "out", "out_tokens", default=0)),
        cost=(None if d.get("cost") is None
              else _as_float(d.get("cost"))),
    )


def parse_stats(d):
    """Stats JSON → StatsSummary（兼容 in/in_tokens；cost null → None）。"""
    return StatsSummary(
        requests=_as_int(d.get("requests")),
        in_tokens=_as_int(_first(d, "in", "in_tokens", default=0)),
        out_tokens=_as_int(_first(d, "out", "out_tokens", default=0)),
        cache_read=_as_int(d.get("cache_read")),
        cache_write=_as_int(d.get("cache_write")),
        reasoning=_as_int(d.get("reasoning")),
        cost=(None if d.get("cost") is None else _as_float(d.get("cost"))),
        latency_p50_ms=_as_int(d.get("latency_p50_ms")),
        latency_p95_ms=_as_int(d.get("latency_p95_ms")),
        ttft_p50_ms=_as_int(d.get("ttft_p50_ms")),
        error_rate=max(0.0, min(1.0, _as_float(d.get("error_rate")))),
        active_ids=_as_int(d.get("active_ids")),
        streams=_as_int(d.get("streams")),
    )


# ---------------------------------------------------------------------------
# 基类：信号面 + 同步方法契约
# ---------------------------------------------------------------------------
class Backend(GObject.Object):
    __gsignals__ = {
        "connected": (GObject.SignalFlags.RUN_FIRST, None, ()),
        "disconnected": (GObject.SignalFlags.RUN_FIRST, None, (str,)),
        "sources-changed": (GObject.SignalFlags.RUN_FIRST, None, ()),
        "keys-changed": (GObject.SignalFlags.RUN_FIRST, None, ()),
        "usage-changed": (GObject.SignalFlags.RUN_FIRST, None, ()),
        "prices-changed": (GObject.SignalFlags.RUN_FIRST, None, ()),
        "source-health": (GObject.SignalFlags.RUN_FIRST, None, (str, str, str)),
        "quota-warning": (GObject.SignalFlags.RUN_FIRST, None, (str, str, float)),
    }

    # ---- 生命周期 -------------------------------------------------------
    def initialize(self):
        raise NotImplementedError

    def reconnect(self):
        raise NotImplementedError

    def manager_info(self):
        """{version, backend, http_addr}；不可用时字段为 "?"。"""
        raise NotImplementedError

    # ---- Sources1 -------------------------------------------------------
    def list_sources(self):
        raise NotImplementedError

    def add_source(self, spec):
        raise NotImplementedError

    def update_source(self, source_id, patch):
        raise NotImplementedError

    def remove_source(self, source_id):
        raise NotImplementedError

    def set_priority(self, source_id, priority):
        raise NotImplementedError

    def set_weight(self, source_id, weight):
        raise NotImplementedError

    def test_source(self, source_id):
        """→ {"ok": bool, "latency_ms": int, "detail": str}"""
        raise NotImplementedError

    # ---- Keys1 ----------------------------------------------------------
    def list_keys(self):
        raise NotImplementedError

    def issue_key(self, opts):
        """→ {"secret": str, "id": ...}；secret 仅此一次可见。"""
        raise NotImplementedError

    def update_key(self, key_id, patch):
        """修改 key 配置（models/rpm/tpm/daily_tokens/aid）。"""
        raise NotImplementedError

    def suspend_key(self, key_id):
        raise NotImplementedError

    def resume_key(self, key_id):
        raise NotImplementedError

    def revoke_key(self, key_id):
        raise NotImplementedError

    # ---- Usage1 ----------------------------------------------------------
    def usage_summary(self, since_hours=24):
        raise NotImplementedError

    def usage_recent(self, limit=100):
        raise NotImplementedError

    def usage_logs(self, since_hours=24, offset=0, limit=50, filters=None):
        """分页调用日志：→ (rows 新→旧, total)。

        filters: {"ident": str, "model": str, "source": str,
                  "search": str, "errors": bool}（缺省=不过滤）。
        """
        raise NotImplementedError

    def key_windows(self, ident):
        raise NotImplementedError

    # ---- Usage1（新：直方图 / 窗口指标） ---------------------------------
    def histogram(self, since_hours=24, bucket="hour", dimension=""):
        """→ [HistBucket]，按 label 升序（再按 group）。

        bucket="hour"|"day"；dimension=""|"key_name"|"model_alias"|
        "source_name"|"aid"（非空时同一 label 多行）。
        """
        raise NotImplementedError

    def stats(self, since_hours=24):
        """→ StatsSummary（无样本时全 0）。"""
        raise NotImplementedError

    # ---- Models1（定价） ---------------------------------------------------
    def list_prices(self):
        """→ [ModelPrice]，按 alias 排序。"""
        raise NotImplementedError

    def set_price(self, alias, components):
        """components: {"input_per_mtok": float|None, ...}（四分量按需）。"""
        raise NotImplementedError

    def remove_price(self, alias):
        raise NotImplementedError

    # ---- 便捷 ------------------------------------------------------------
    def alias_sources(self):
        """聚合：alias → [source name]，模型页数据（/v1/models 语义）。"""
        agg = {}
        for src in self.list_sources():
            if not src.enabled:
                continue
            for m in src.models:
                agg.setdefault(m.alias, [])
                if src.name not in agg[m.alias]:
                    agg[m.alias].append(src.name)
        return {alias: sorted(names) for alias, names in sorted(agg.items())}


# ---------------------------------------------------------------------------
# MockBackend
# ---------------------------------------------------------------------------
class MockBackend(Backend):
    """进程内假后端。

    * ``live=False``（默认）：完全确定性、无定时器——单测用；
      需要推进时间时手工调 ``tick()``。
    * ``live=True``：每 5s ``tick()`` 一次（模拟新请求、健康翻转、
      配额告警信号），GUI 演示用。
    """

    TICK_SECONDS = 5

    def __init__(self, live=False, seed=42):
        GObject.Object.__init__(self)
        self._live = live
        self._rand = random.Random(seed)
        self._sources = []          # [dict 内部 JSON 形态]
        self._next_source_id = 1
        self._keys = []
        self._next_key_id = 1
        self._usage = []            # [dict]
        self._hist = []             # 直方图确定性序列（见 _seed_histogram）
        self._tick_no = 0
        self._seed_data()
        if live:
            GLib.timeout_add_seconds(self.TICK_SECONDS, self._live_tick)

    # ---- 初始数据 --------------------------------------------------------
    def _seed_data(self):
        now = time.time()
        self._sources = [
            {
                "id": 1, "name": "primary", "type": "openai",
                "base_url": "https://upstream.example.test/v1", "has_key": True,
                "priority": 10, "weight": 100, "enabled": True,
                "models": [
                    {"alias": "model-a", "upstream": "model-a"},
                    {"alias": "test-model", "upstream": "test-model"},
                    {"alias": "model-b", "upstream": "model-b"},
                ],
                "health": {"state": "ok", "fails": 0, "cooling_until": ""},
            },
            {
                "id": 2, "name": "anthropic-official", "type": "anthropic",
                "base_url": "https://upstream-anthropic.example.test", "has_key": True,
                "priority": 20, "weight": 50, "enabled": True,
                "models": [
                    {"alias": "model-x", "upstream": "model-x-up"},
                    {"alias": "model-y", "upstream": "model-y-up"},
                ],
                "health": {"state": "degraded", "fails": 2, "cooling_until": ""},
            },
            {
                "id": 3, "name": "hub-b", "type": "openai",
                "base_url": "https://upstream-hub.example.test/v1", "has_key": False,
                "priority": 20, "weight": 30, "enabled": False,
                "models": [
                    {"alias": "model-a", "upstream": "vendor-b/model-b"},
                ],
                "health": {"state": "cooling", "fails": 5,
                           "cooling_until": "2026-10-28T23:00:00Z"},
            },
        ]
        self._prices = {"model-a": {"input_per_mtok": 0.10,
                                 "output_per_mtok": 0.30}}
        self._sources.append({
            "id": 4, "name": "openai-direct", "type": "openai-responses",
            "base_url": "https://upstream-openai.example.test/v1", "has_key": True,
            "priority": 30, "weight": 20, "enabled": True,
            "models": [
                {"alias": "model-c", "upstream": "model-c"},
                {"alias": "model-d", "upstream": "model-d"},
            ],
            "health": {"state": "ok", "fails": 0, "cooling_until": ""},
        })
        self._next_source_id = 5

        def _mkkey(name, models, rpm, tpm, daily, aid, suspended=False):
            self._keys.append({
                "id": self._next_key_id, "name": name,
                "enabled": True, "suspended": suspended, "models": models,
                "rpm": rpm, "tpm": tpm, "daily_tokens": daily, "aid": aid,
                "created_at": "2026-10-24T09:00:00Z", "revoked_at": "",
            })
            self._next_key_id += 1

        _mkkey("opencode", ["*"], 60, 2_000_000, 5_000_000, "opencode")
        _mkkey("client-a", ["model-a", "test-model"],
               10, 400_000, 1_000_000, "client-a")
        _mkkey("sysagent", ["*"], 30, 1_000_000, 2_000_000, "sysagent",
               suspended=True)
        _mkkey("ci-runner", ["model-b"], 5, 100_000, 200_000, "",
               )
        # 演示 revoked 状态
        self._keys[-1]["revoked_at"] = "2026-10-26T18:30:00Z"

        idents = [("opencode", "opencode-main"), ("opencode", "opencode-bg"),
                  ("client-a", "client-a"), ("sysagent", "sysd")]
        models = ["model-a", "test-model", "model-b",
                  "model-x", "model-y"]
        sources = {"model-a": "primary", "test-model": "primary",
                   "model-b": "primary", "model-x": "anthropic-official",
                   "model-y": "anthropic-official"}
        for i in range(48):
            ident, agent = idents[i % len(idents)]
            model = models[i % len(models)]
            status = "ok" if i % 11 else "rate_limited"
            self._usage.append({
                "ts": now - i * 1700 - self._rand.randrange(0, 60),
                "ident": ident, "model": model, "source": sources[model],
                "agent": agent,
                "in_tokens": self._rand.randrange(120, 3600),
                "out_tokens": self._rand.randrange(80, 900),
                "latency_ms": self._rand.randrange(140, 3800),
                "status": status,
                # Stats 字段（确定性公式，供 stats()/单测）：
                "cache_read": 240 + i * 7 if i % 5 == 0 else 0,
                "cache_write": 120 + i * 3 if i % 7 == 0 else 0,
                "ttft_ms": 90 + i * 15,
                "stream": i % 3 == 0,
                "error_kind": "rate_limited" if status != "ok" else "",
            })
        self._usage.sort(key=lambda r: r["ts"], reverse=True)
        self._seed_histogram()

    # ---- 生命周期 --------------------------------------------------------
    def initialize(self):
        self.emit("connected")

    def reconnect(self):
        self.emit("connected")

    def manager_info(self):
        return {"version": GLM_VERSION, "backend": "mock",
                "http_addr": "127.0.0.1:8484"}

    # ---- Sources1 --------------------------------------------------------
    def list_sources(self):
        return [parse_source(s) for s in self._sources]

    def _find_source(self, source_id):
        for s in self._sources:
            if s["id"] == _source_id(source_id) or s["name"] == source_id:
                return s
        raise BackendError(_("No such source: %s") % (source_id,), "NoSuchSource")

    def add_source(self, spec):
        name = (spec.get("name") or "").strip()
        if not name:
            raise BackendError(_("Name must not be empty"), "InvalidArg")
        if any(s["name"] == name for s in self._sources):
            raise BackendError(_("A source with this name already exists: %s") % name, "SourceExists")
        if spec.get("type") not in SOURCE_TYPES:
            raise BackendError(
                _("Type must be one of %s") % ", ".join(SOURCE_TYPES),
                "InvalidArg")
        base_url = (spec.get("base_url") or "").strip()
        if base_url and not base_url.startswith(("http://", "https://")):
            raise BackendError(_("base_url must start with http(s)://"), "InvalidArg")
        src = {
            "id": self._next_source_id, "name": name,
            "type": spec.get("type"),
            "base_url": base_url,
            "has_key": bool(spec.get("api_key")),
            "priority": _as_int(spec.get("priority"), 10),
            "weight": _as_int(spec.get("weight"), 100),
            "enabled": _as_bool(spec.get("enabled"), True),
            "models": [{"alias": _as_str(m.get("alias")),
                        "upstream": _as_str(m.get("upstream"))}
                       for m in spec.get("models") or []],
            "health": {"state": "unknown", "fails": 0, "cooling_until": ""},
        }
        self._next_source_id += 1
        self._sources.append(src)
        self.emit("sources-changed")
        return parse_source(src)

    def update_source(self, source_id, patch):
        src = self._find_source(source_id)
        for field_name in ("name", "type", "base_url"):
            if field_name in patch:
                src[field_name] = _as_str(patch[field_name])
        if "api_key" in patch and patch["api_key"]:
            src["has_key"] = True
        if "priority" in patch:
            src["priority"] = _as_int(patch["priority"], src["priority"])
        if "weight" in patch:
            src["weight"] = _as_int(patch["weight"], src["weight"])
        if "enabled" in patch:
            src["enabled"] = _as_bool(patch["enabled"], src["enabled"])
        if "models" in patch:
            src["models"] = [{"alias": _as_str(m.get("alias")),
                              "upstream": _as_str(m.get("upstream"))}
                             for m in patch["models"] or []]
        self.emit("sources-changed")
        return parse_source(src)

    def remove_source(self, source_id):
        src = self._find_source(source_id)
        self._sources.remove(src)
        self.emit("sources-changed")

    def set_priority(self, source_id, priority):
        src = self._find_source(source_id)
        src["priority"] = int(priority)
        self.emit("sources-changed")
        return parse_source(src)

    def set_weight(self, source_id, weight):
        src = self._find_source(source_id)
        src["weight"] = int(weight)
        self.emit("sources-changed")
        return parse_source(src)

    def test_source(self, source_id):
        src = self._find_source(source_id)
        state = src["health"]["state"]
        latency = self._rand.randrange(80, 320)
        if state in ("cooling", "down"):
            return {"ok": False, "latency_ms": 0,
                    "detail": _("Source is in %s state, probe failed") % state}
        if state == "degraded":
            return {"ok": True, "latency_ms": latency,
                    "detail": _("Reachable (degraded: %d consecutive failures)")
                              % src["health"]["fails"]}
        return {"ok": True, "latency_ms": latency, "detail": _("Probe succeeded")}

    # ---- Keys1 ------------------------------------------------------------
    def list_keys(self):
        return [parse_key(k) for k in self._keys]

    def _find_key(self, key_id):
        for k in self._keys:
            if k["id"] == _source_id(key_id) or k["name"] == key_id:
                return k
        raise BackendError(_("No such key: %s") % (key_id,), "NoSuchKey")

    def issue_key(self, opts):
        name = (opts.get("name") or "").strip()
        if not name:
            raise BackendError(_("App name cannot be empty"), "InvalidArg")
        if any(k["name"] == name and not k["revoked_at"] for k in self._keys):
            raise BackendError(_("A key with this name already exists: %s") % name, "KeyExists")
        models = [m.strip() for m in opts.get("models") or [] if m.strip()]
        if not models:
            models = ["*"]
        key = {
            "id": self._next_key_id, "name": name,
            "enabled": True, "suspended": False, "models": models,
            "rpm": _as_int(opts.get("rpm"), 60),
            "tpm": _as_int(opts.get("tpm"), 1_000_000),
            "daily_tokens": _as_int(opts.get("daily_tokens"), 2_000_000),
            "aid": _as_str(opts.get("aid")),
            "created_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
            "revoked_at": "",
        }
        self._next_key_id += 1
        self._keys.append(key)
        self.emit("keys-changed")
        secret = "sk-kln-%s-%s" % (name.replace("_", "-").lower(),
                                   self._rand.getrandbits(64).to_bytes(8, "big").hex())
        return {"secret": secret, "id": key["id"]}

    def update_key(self, key_id, patch):
        key = self._find_key(key_id)
        if key["revoked_at"]:
            raise BackendError(_("Key cannot be resumed after revocation"),
                               "InvalidState")
        if "models" in patch:
            models = [m.strip() for m in patch.get("models") or [] if m.strip()]
            key["models"] = models or ["*"]
        for field_name in ("rpm", "tpm", "daily_tokens"):
            if field_name in patch:
                key[field_name] = _as_int(patch.get(field_name))
        if "aid" in patch:
            key["aid"] = _as_str(patch.get("aid"))
        self.emit("keys-changed")
        return parse_key(key)

    def suspend_key(self, key_id):
        key = self._find_key(key_id)
        if key["revoked_at"]:
            raise BackendError(_("Key cannot be suspended after revocation"), "InvalidState")
        if key["suspended"]:
            raise BackendError(_("Key is already suspended"), "InvalidState")
        key["suspended"] = True
        self.emit("keys-changed")
        return parse_key(key)

    def resume_key(self, key_id):
        key = self._find_key(key_id)
        if key["revoked_at"]:
            raise BackendError(_("Key cannot be resumed after revocation"), "InvalidState")
        if not key["suspended"]:
            raise BackendError(_("Key is not suspended"), "InvalidState")
        key["suspended"] = False
        self.emit("keys-changed")
        return parse_key(key)

    def revoke_key(self, key_id):
        key = self._find_key(key_id)
        if key["revoked_at"]:
            raise BackendError(_("Key already revoked"), "InvalidState")
        key["revoked_at"] = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
        self.emit("keys-changed")
        return parse_key(key)

    # ---- Models1（定价） ---------------------------------------------------
    def list_prices(self):
        return [ModelPrice(alias, **comps)
                for alias, comps in sorted(self._prices.items())]

    def set_price(self, alias, components):
        alias = str(alias).strip()
        if not alias:
            raise BackendError(_("Alias must not be empty"), "InvalidArg")
        comps = {}
        for name in ("input_per_mtok", "output_per_mtok",
                     "cache_read_per_mtok", "cache_write_per_mtok"):
            value = components.get(name)
            if value is None:
                continue
            value = float(value)
            if value < 0:
                raise BackendError(_("Price must not be negative"),
                                   "InvalidArg")
            comps[name] = value
        if not comps:
            raise BackendError(
                _("At least one price component must be enabled"),
                "InvalidArg")
        self._prices[alias] = comps
        self.emit("prices-changed")
        return ModelPrice(alias, **comps)

    def remove_price(self, alias):
        if str(alias) not in self._prices:
            raise BackendError(_("No price set for %s") % alias, "NoSuchPrice")
        del self._prices[str(alias)]
        self.emit("prices-changed")

    # ---- Usage1（旧面：汇总 / 最近 / 配额窗口） ------------------------------
    def usage_summary(self, since_hours=24):
        cutoff = time.time() - since_hours * 3600
        rows = [r for r in self._usage if r["ts"] >= cutoff]
        health = {s["name"]: s["health"]["state"] for s in self._sources}
        return UsageSummary(
            since_hours=since_hours,
            requests=len(rows),
            tokens_in=sum(r["in_tokens"] for r in rows),
            tokens_out=sum(r["out_tokens"] for r in rows),
            active_keys=sum(1 for k in self._keys
                            if not k["revoked_at"] and not k["suspended"]),
            sources_health=health,
        )

    def usage_recent(self, limit=100):
        return [parse_usage_row(r) for r in self._usage[:limit]]

    def usage_logs(self, since_hours=24, offset=0, limit=50, filters=None):
        filters = filters or {}
        cutoff = time.time() - since_hours * 3600
        needle = _as_str(filters.get("search")).casefold()
        rows = []
        for i, r in enumerate(self._usage):
            row = parse_usage_row(r)
            row.req_id = row.req_id or "req-mock-%d" % (len(self._usage) - i)
            if row.ts < cutoff:
                continue
            if filters.get("ident") and row.ident != filters["ident"]:
                continue
            if filters.get("model") and row.model != filters["model"]:
                continue
            if filters.get("source") and row.source != filters["source"]:
                continue
            if filters.get("errors") is True and not row.is_error:
                continue
            if filters.get("errors") is False and row.is_error:
                continue
            if needle:
                hay = " ".join((row.ident, row.agent, row.model, row.source,
                                row.req_id, row.error_kind)).casefold()
                if needle not in hay:
                    continue
            rows.append(row)
        total = len(rows)
        return rows[offset:offset + max(1, int(limit))], total

    def key_windows(self, ident):
        key = None
        for k in self._keys:
            if k["name"] == ident or k["id"] == _source_id(ident):
                key = k
                break
        if key is None:
            raise BackendError(_("No such key: %s") % (ident,), "NoSuchKey")
        now = time.time()
        day = time.strftime("%Y-%m-%d", time.localtime(now))
        rows = [r for r in self._usage if r["ident"] == key["name"]]

        def _tok(rows_subset):
            return sum(r["in_tokens"] + r["out_tokens"] for r in rows_subset)

        minute_rows = [r for r in rows if now - r["ts"] < 60]
        today_rows = [r for r in rows
                      if time.strftime("%Y-%m-%d", time.localtime(r["ts"])) == day]
        return [
            KeyWindow("rpm", len(minute_rows), key["rpm"]),
            KeyWindow("tpm", _tok(minute_rows), key["tpm"]),
            KeyWindow("daily_tokens", _tok(today_rows), key["daily_tokens"]),
        ]

    # ---- Usage1 新面：确定性直方图 / 窗口指标 ---------------------------------
    # 直方图与 _usage 行是 mock 里两条独立序列（真后端同源于 SQLite usage 表）：
    # 直方图保证“固定 3 个 hour 桶 × 2 个 key”的确定形状供单测；量级与
    # _usage（48 请求）对齐，GUI 演示不突兀。
    _HIST_DIM_MAPS = {
        "key_name": {"opencode": "opencode", "client-a": "client-a"},
        "model_alias": {"opencode": "model-a",
                        "client-a": "test-model"},
        "source_name": {"opencode": "primary", "client-a": "primary"},
        "aid": {"opencode": "opencode", "client-a": "client-a"},
    }

    @staticmethod
    def _hist_label(ts):
        return time.strftime("%Y-%m-%dT%H:00:00Z", time.gmtime(ts))

    def _seed_histogram(self):
        """固定 3 个 hour 桶（锚定当前整点回推）× 2 个 key 组，值确定。"""
        hour = 3600
        anchor = int(time.time() // hour) * hour
        # (回推小时数, group, requests, in, out)
        series = [
            (2, "opencode", 6, 9800, 3200),
            (2, "client-a", 4, 5100, 1800),
            (1, "opencode", 8, 12200, 4100),
            (1, "client-a", 6, 7000, 2600),
            (0, "opencode", 9, 14100, 5000),
            (0, "client-a", 7, 8200, 3100),
        ]
        self._hist = []
        for back, group, reqs, in_tok, out_tok in series:
            self._hist.append({
                "ts": anchor - back * hour,
                "label": self._hist_label(anchor - back * hour),
                "group": group, "requests": reqs,
                "in": in_tok, "out": out_tok,
                "cost": round((in_tok * 2 + out_tok * 6) * 1e-6, 6),
            })
        self._hist.sort(key=lambda b: (b["ts"], b["group"]))

    def _hist_append_tick(self):
        """live tick：追加一个更新的 hour 桶（值按拍号递增，保持确定性）。"""
        last_ts = max(b["ts"] for b in self._hist) if self._hist else \
            int(time.time() // 3600) * 3600 - 3600
        n = self._tick_no
        hour = 3600
        for group, reqs, in_tok, out_tok in (
                ("opencode", 3 + n % 3, 4200 + 100 * (n % 4), 1500),
                ("client-a", 2 + n % 2, 2600 + 80 * (n % 3), 900)):
            ts = last_ts + hour
            self._hist.append({
                "ts": ts, "label": self._hist_label(ts), "group": group,
                "requests": reqs, "in": in_tok, "out": out_tok,
                "cost": round((in_tok * 2 + out_tok * 6) * 1e-6, 6),
            })
        del self._hist[:-96]            # 上限 96 行（2 组 × 48 桶）

    def histogram(self, since_hours=24, bucket="hour", dimension=""):
        """确定性直方图：见 _seed_histogram / _hist_append_tick。"""
        if bucket not in ("hour", "day"):
            raise BackendError(_("bucket must be hour|day"), "InvalidArg")
        if dimension not in ("", "key_name", "model_alias", "source_name",
                             "aid"):
            raise BackendError(_("unknown dimension: %s") % dimension, "InvalidArg")
        cutoff = time.time() - since_hours * 3600
        rows = [b for b in self._hist if b["ts"] >= cutoff]
        dim_map = self._HIST_DIM_MAPS.get(dimension)

        def _cells():
            """展平为 (桶键, 排序键, group, requests, in, out, cost)。"""
            for b in rows:
                group = "" if dim_map is None else \
                    dim_map.get(b["group"], b["group"])
                if bucket == "day":
                    key = time.strftime("%Y-%m-%d", time.gmtime(b["ts"]))
                    sort_key = (key, group)
                else:
                    key = b["label"]
                    sort_key = (b["ts"], group)
                yield sort_key, key, group, b["requests"], b["in"], \
                    b["out"], (b["cost"] or 0.0)

        # 聚合：dimension="" 时同一桶的多组并成一格；day 桶按日期并格
        agg = {}
        for sort_key, key, group, reqs, in_tok, out_tok, cost in _cells():
            cell = agg.setdefault(sort_key,
                                  {"key": key, "group": group, "requests": 0,
                                   "in": 0, "out": 0, "cost": 0.0})
            cell["requests"] += reqs
            cell["in"] += in_tok
            cell["out"] += out_tok
            cell["cost"] += cost
        out = []
        for sort_key in sorted(agg):
            cell = agg[sort_key]
            label = cell["key"]
            if bucket == "day":
                label = "%sT00:00:00Z" % cell["key"]
            out.append(HistBucket(
                label=label, group=cell["group"],
                requests=cell["requests"], in_tokens=cell["in"],
                out_tokens=cell["out"], cost=round(cell["cost"], 6)))
        return out

    @staticmethod
    def _percentile(sorted_values, q):
        """nearest-rank（对齐 store.percentilesSince）。"""
        if not sorted_values:
            return 0
        rank = max(1, int(q * len(sorted_values) + 0.999999))
        return sorted_values[min(rank, len(sorted_values)) - 1]

    def stats(self, since_hours=24):
        cutoff = time.time() - since_hours * 3600
        rows = [r for r in self._usage if r["ts"] >= cutoff]
        if not rows:
            return StatsSummary()
        latencies = sorted(r["latency_ms"] for r in rows
                           if r["latency_ms"] > 0)
        ttfts = sorted(r.get("ttft_ms", 0) for r in rows
                       if r.get("ttft_ms", 0) > 0)
        in_sum = sum(r["in_tokens"] for r in rows)
        out_sum = sum(r["out_tokens"] for r in rows)
        errors = sum(1 for r in rows if r["status"] != "ok")
        ids = set(r["agent"] if r.get("agent") else "key:" + r["ident"]
                  for r in rows)
        return StatsSummary(
            requests=len(rows),
            in_tokens=in_sum,
            out_tokens=out_sum,
            cache_read=sum(r.get("cache_read", 0) for r in rows),
            cache_write=sum(r.get("cache_write", 0) for r in rows),
            reasoning=sum(r.get("reasoning", 0) for r in rows),
            cost=round(in_sum * 2e-6 + out_sum * 6e-6, 6),
            latency_p50_ms=self._percentile(latencies, 0.50),
            latency_p95_ms=self._percentile(latencies, 0.95),
            ttft_p50_ms=self._percentile(ttfts, 0.50),
            error_rate=errors / float(len(rows)),
            active_ids=len(ids),
            streams=sum(1 for r in rows if r.get("stream")),
        )

    # ---- 模拟数据变化（5s 一拍） ---------------------------------------------
    def _live_tick(self):
        self.tick()
        return True  # GLib.SOURCE_CONTINUE

    def tick(self):
        """推进一拍模拟世界：新请求+新直方图桶 →（每 3 拍）健康翻转 →
        （每 4 拍）配额告警。

        确定性规则，单测直接断言；GUI 的 live 模式由定时器驱动本方法。
        """
        self._tick_no += 1
        self._hist_append_tick()
        n = self._tick_no
        now = time.time()
        for _ in range(self._rand.randrange(1, 4)):
            model = self._rand.choice(["model-a", "test-model",
                                       "model-b", "model-x"])
            ident, agent = self._rand.choice(
                [("opencode", "opencode-main"), ("opencode", "opencode-bg"),
                 ("client-a", "client-a"), ("sysagent", "sysd")])
            self._usage.insert(0, {
                "ts": now - self._rand.randrange(0, 5),
                "ident": ident, "model": model,
                "source": "primary" if "model-c" not in model else "anthropic-official",
                "agent": agent,
                "in_tokens": self._rand.randrange(120, 3600),
                "out_tokens": self._rand.randrange(80, 900),
                "latency_ms": self._rand.randrange(140, 3800),
                "status": "ok",
                # Stats 字段（与种子行同构，防 stats() KeyError）
                "cache_read": self._rand.randrange(200, 600)
                if self._rand.random() < 0.2 else 0,
                "cache_write": 0,
                "ttft_ms": self._rand.randrange(80, 600),
                "stream": self._rand.random() < 0.34,
                "error_kind": "",
            })
        self.emit("usage-changed")

        if n % 3 == 0 and self._sources:
            src = self._sources[n % len(self._sources)]
            state = "degraded" if src["health"]["state"] == "ok" else "ok"
            src["health"] = {"state": state,
                             "fails": 2 if state == "degraded" else 0,
                             "cooling_until": ""}
            self.emit("source-health", str(src["id"]), state,
                      "连续探测失败" if state == "degraded" else "恢复")
            self.emit("sources-changed")

        if n % 4 == 0:
            hot = max((k for k in self._keys if not k["revoked_at"]),
                      key=lambda k: k["name"], default=None)
            if hot is not None:
                pct = round(self._rand.uniform(78.0, 96.0), 1)
                self.emit("quota-warning", hot["name"], "daily_tokens", pct)


# ---------------------------------------------------------------------------
# DBusBackend —— llmgwd 真实管理面（internal/dbusapi 契约，APIVer 2，实测）
# ---------------------------------------------------------------------------
# 线形状（与 Go 端 internal/dbusapi/api.go 完全一致；复杂载荷为 JSON 字符串，
# 其余为浅类型参数）：
#   Sources1  ListSources() → as        每项 Source JSON
#             AddSource(s spec) → u id
#             UpdateSource(u id, s patch) → ()
#             RemoveSource(u id) → ()
#             SetPriority(u id, i v) / SetWeight(u id, i v) → ()
#             TestSource(u id) → s {ok,latency_ms,model_count,error}
#   Keys1     ListKeys() → as           每项 Key JSON（models 为逗号字符串）
#             IssueKey(s app, s limits) → (s secret, u id)
#             SuspendKey/ResumeKey/RevokeKey(s name) → ()
#   Usage1    Summary(s since_rfc3339) → as [byKey,byModel,bySource,byAid]
#             Recent(u limit) → as      每项 Usage JSON
#             KeyWindows(s "key:<name>"|"aid:<aid>") → s {rpm,tpm,daily,limits}
#             Histogram(s since, s hour|day, s dimension) → as
#                 每项 {label,group,requests,in,out,cost|null}
#             Stats(s since) → s {requests,in,out,cache_read,cache_write,
#                 reasoning,cost|null,latency_p50_ms,latency_p95_ms,
#                 ttft_p50_ms,error_rate(0..1),active_ids,streams}
#   Manager1  属性 Version(u)/Backend(s)/HTTPAddr(s)
#   信号      Sources1.SourceHealthChanged(u,s,s)
#             Quota1.QuotaWarning(s ident, s window, d pct)  pct 为分数(0-1)
FRIENDLY_ERRORS = {
    "io.kleinia.ModelGateway.NotAuthorized":
        _("Permission denied (polkit authorization failed)"),
    "org.freedesktop.DBus.Error.AccessDenied":
        _("Permission denied (polkit authorization failed)"),
    "org.freedesktop.PolicyKit1.Error.Cancelled":
        _("Authentication cancelled"),
    "org.freedesktop.PolicyKit1.Error.NotAuthorized":
        _("Not authorized for this operation (admin consent required)"),
    "org.freedesktop.DBus.Error.ServiceUnknown":
        _("llmgwd is not registered on the bus"),
    "org.freedesktop.DBus.Error.InvalidArgs":
        _("Backend rejected the arguments"),
}

IFACE_QUOTA = "io.kleinia.Quota1"


def friendly_dbus_error(err):
    """GLib.Error(GDBus) → BackendError（polkit 拒绝有专门文案）。"""
    msg = getattr(err, "message", None) or str(err)
    name, detail = None, msg
    if msg.startswith("GDBus.Error:"):
        rest = msg[len("GDBus.Error:"):].strip()
        if ":" in rest:
            name, detail = rest.split(":", 1)
            name, detail = name.strip(), detail.strip()
        else:
            name = rest
    kind = "Failed"
    if name and name.startswith("io.kleinia.ModelGateway."):
        kind = name.rsplit(".", 1)[-1]
    if name in FRIENDLY_ERRORS:
        return BackendError(FRIENDLY_ERRORS[name], kind)
    if kind != "Failed":
        return BackendError("%s：%s" % (kind, detail or name), kind)
    return BackendError(detail or msg, kind)


class DBusBackend(Backend):
    """llmgwd 真实管理面客户端。

    * ``address=None``：system 总线，watch ``io.kleinia.ModelGateway``
      名称出现/消失（掉线 → disconnected 信号 → Banner）；
    * ``address="unix:path=..."``：对等连接
      （Gio.DBusConnection.new_for_address_sync + AUTHENTICATION_CLIENT，
      对应 llmgwd 的 ``--dbus-p2p=<path>``；宿主无 dbus-daemon 时的通道，
      p2p 信任边界是 socket 文件权限，跳过 polkit）。
    """

    def __init__(self, bus_type=Gio.BusType.SYSTEM, address=None):
        GObject.Object.__init__(self)
        self._bus_type = bus_type
        self._address = address
        self._conn = None
        self._dest = None            # 总线模式为 BUS_NAME；p2p 为 None
        self._watch_id = None
        self._sig_ids = []
        self._info = {"version": "?", "backend": "?", "http_addr": "?"}
        self._ready = False
        # 读缓存：key → [expiry, value, ttl, fetch, signal]
        self._cache = {}
        self._inflight = set()       # 正在后台回源的 cache key
        self._cache_lock = threading.Lock()
        self._poll_ids = []

    # ---- 生命周期 -------------------------------------------------------
    def initialize(self):
        if self._address:
            try:
                conn = Gio.DBusConnection.new_for_address_sync(
                    self._address,
                    Gio.DBusConnectionFlags.AUTHENTICATION_CLIENT,
                    None, None)
                self._attach(conn, None)
            except GLib.Error as err:
                self.emit("disconnected",
                          _("Peer connection failed: %s") % (err.message or err))
        else:
            self._watch_id = Gio.bus_watch_name(
                self._bus_type, BUS_NAME, Gio.BusNameWatcherFlags.NONE,
                self._on_name_appeared, self._on_name_vanished)

    def reconnect(self):
        self._teardown()
        self.initialize()

    def _teardown(self):
        if self._watch_id is not None:
            Gio.bus_unwatch_name(self._watch_id)
            self._watch_id = None
        for poll_id in self._poll_ids:
            GLib.Source.remove(poll_id)
        self._poll_ids = []
        for sid in self._sig_ids:
            try:
                if self._conn is not None:
                    self._conn.signal_unsubscribe(sid)
            except Exception:
                pass
        self._sig_ids = []
        self._conn = None
        self._dest = None
        self._ready = False

    def _on_name_appeared(self, conn, name, owner):
        self._attach(conn, BUS_NAME)

    def _on_name_vanished(self, conn, name):
        was_ready = self._ready
        self._teardown()
        if was_ready:
            self.emit("disconnected", _("llmgwd has left the bus"))

    def _attach(self, conn, dest):
        self._conn = conn
        self._dest = dest
        try:
            conn.connect("closed", self._on_closed)
        except Exception:
            pass
        for iface, member, cb in (
                (IFACE_SOURCES, "SourceHealthChanged", self._on_source_health),
                (IFACE_QUOTA, "QuotaWarning", self._on_quota_warning)):
            self._sig_ids.append(conn.signal_subscribe(
                None, iface, member, OBJECT_PATH, None,
                Gio.DBusSignalFlags.NONE, cb, None))
        self._load_manager_info()
        self._ready = True
        # 主动轮询：用量族 5s、状态族 20s，只回源页面已用过的缓存键；
        # 数据有变化才广播 signal（页面随之刷新，无变化零开销）。
        self._poll_ids.append(GLib.timeout_add_seconds(
            POLL_USAGE_SECONDS, self._poll_tick, ("hist", "stats", "recent")))
        self._poll_ids.append(GLib.timeout_add_seconds(
            POLL_STATE_SECONDS, self._poll_tick, ("sources", "keys")))
        self.emit("connected")

    # ------------------------------------------------------------------
    # 读缓存（见模块 docstring 与 CACHE_TTL 注释）
    # ------------------------------------------------------------------
    def _cached(self, key, ttl, fetch, signal):
        """读一个缓存键：新鲜直回；冷读同步取一次；过期回旧值 + 后台回源。"""
        now = time.monotonic()
        with self._cache_lock:
            entry = self._cache.get(key)
            if entry is not None and entry[0] > now:
                return entry[1]
        if entry is None:
            value = fetch()          # 冷读：同步（首帧不闪空态）
            with self._cache_lock:
                self._cache[key] = [now + ttl, value, ttl, fetch, signal]
            return value
        self._refill_bg(key)
        return entry[1]

    def _refill_bg(self, key):
        """过期键的后台回源（去重：同键并发只跑一个线程）。"""
        with self._cache_lock:
            if key in self._inflight or key not in self._cache:
                return
            self._inflight.add(key)
        threading.Thread(target=self._refill_worker, args=(key,),
                         daemon=True).start()

    def _refill_worker(self, key):
        with self._cache_lock:
            entry = self._cache.get(key)
        ok = False
        fresh = None
        changed = False
        if entry is not None:
            _exp, old, ttl, fetch, signal = entry
            try:
                fresh = fetch()
                ok = True
            except Exception:
                ok = False
            with self._cache_lock:
                self._inflight.discard(key)
                current = self._cache.get(key)
                if ok and current is not None:
                    changed = current[1] != fresh
                    current[0] = time.monotonic() + ttl
                    current[1] = fresh
        else:
            with self._cache_lock:
                self._inflight.discard(key)
        if ok and changed and entry is not None and entry[4]:
            GLib.idle_add(self._emit_if_alive, entry[4])

    def _emit_if_alive(self, signal):
        if self._conn is not None:
            self.emit(signal)
        return GLib.SOURCE_REMOVE

    def _after_write(self, prefix):
        """写操作后同步回源该族缓存（emit→refresh 读到的即新值）。"""
        with self._cache_lock:
            keys = [k for k in self._cache if k.startswith(prefix)]
        for key in keys:
            with self._cache_lock:
                entry = self._cache.get(key)
            if entry is None:
                continue
            try:
                value = entry[3]()
            except Exception:
                with self._cache_lock:
                    self._cache.pop(key, None)
                continue
            with self._cache_lock:
                if key in self._cache:
                    entry[0] = time.monotonic() + entry[2]
                    entry[1] = value

    def _poll_tick(self, prefixes):
        if self._conn is None:
            return GLib.SOURCE_REMOVE
        with self._cache_lock:
            keys = [k for k in self._cache
                    if any(k == p or k.startswith(p) for p in prefixes)]
        for key in keys:
            self._refill_bg(key)
        return GLib.SOURCE_CONTINUE

    def _on_closed(self, *args):
        # "closed" 信号参数随 GLib 版本有差（reason/remote_error 变长），
        # 统一 *args 吸收，避免断连回调抛 TypeError 拖垮 GUI。
        self._teardown()
        self.emit("disconnected", _("D-Bus connection closed"))

    # ---- 信号回调 --------------------------------------------------------
    def _on_source_health(self, conn, sender, path, iface, signal, params,
                          user_data):
        sid, state, reason = params.unpack()
        self.emit("source-health", str(sid), str(state), str(reason))
        self.emit("sources-changed")

    def _on_quota_warning(self, conn, sender, path, iface, signal, params,
                          user_data):
        ident, window, pct = params.unpack()
        self.emit("quota-warning", str(ident), str(window), float(pct))
        self.emit("usage-changed")

    # ---- manager 属性 ------------------------------------------------------
    def _load_manager_info(self):
        try:
            result = self._conn.call_sync(
                self._dest, OBJECT_PATH, "org.freedesktop.DBus.Properties",
                "GetAll", GLib.Variant("(s)", (IFACE_MANAGER,)),
                GLib.VariantType("(a{sv})"), Gio.DBusCallFlags.NONE, -1, None)
            props = result.unpack()[0]
        except GLib.Error:
            props = {}
        for prop, key in (("Version", "version"), ("Backend", "backend"),
                          ("HTTPAddr", "http_addr")):
            value = props.get(prop)
            if value is not None:
                self._info[key] = str(value)

    def manager_info(self):
        return dict(self._info)

    # ---- 调用核心 -----------------------------------------------------------
    def _call(self, iface, method, params, reply_type):
        """call_sync 包装：GLib.Error → BackendError。"""
        if self._conn is None:
            raise BackendError(_("Not connected to the llmgwd backend yet"), "NotConnected")
        try:
            return self._conn.call_sync(
                self._dest, OBJECT_PATH, iface, method, params, reply_type,
                Gio.DBusCallFlags.NONE, -1, None)
        except GLib.Error as err:
            raise friendly_dbus_error(err) from None

    @staticmethod
    def _v(sig):
        return GLib.VariantType.new(sig) if sig else None

    @staticmethod
    def _json_list(result):
        """(as) 返回 → [dict]（逐项 json.loads，坏项跳过）。"""
        out = []
        for item in (result.unpack()[0] if result else []):
            try:
                out.append(json.loads(item))
            except (TypeError, ValueError):
                continue
        return out

    # ---- Sources1 ------------------------------------------------------------
    def list_sources(self):
        return self._cached("sources", CACHE_TTL, self._fetch_sources,
                            "sources-changed")

    def _fetch_sources(self):
        result = self._call(IFACE_SOURCES, "ListSources", None, self._v("(as)"))
        return [parse_source(d) for d in self._json_list(result)]

    def add_source(self, spec):
        payload = {
            "name": spec.get("name"), "type": spec.get("type"),
            "base_url": spec.get("base_url"),
            "api_key": spec.get("api_key") or "",
            "models": spec.get("models") or [],
        }
        for key in ("priority", "weight"):
            if spec.get(key) is not None:
                payload[key] = int(spec[key])
        result = self._call(IFACE_SOURCES, "AddSource",
                            GLib.Variant("(s)", (json.dumps(payload),)),
                            self._v("(u)"))
        new_id = _source_id(result.unpack()[0])
        self._after_write("sources")
        self.emit("sources-changed")
        return Source(id=new_id, name=payload["name"], type=payload["type"],
                      base_url=payload["base_url"],
                      has_key=bool(payload["api_key"]),
                      priority=payload.get("priority", 10),
                      weight=payload.get("weight", 100), enabled=True)

    def update_source(self, source_id, patch):
        payload = {}
        for key in ("name", "type", "base_url"):
            if key in patch:
                payload[key] = patch[key]
        if patch.get("api_key"):
            payload["api_key"] = patch["api_key"]
        for key in ("priority", "weight", "enabled"):
            if key in patch:
                payload[key] = patch[key]
        if "models" in patch:
            payload["models"] = patch["models"]
        self._call(IFACE_SOURCES, "UpdateSource",
                   GLib.Variant("(us)", (_as_int(source_id),
                                         json.dumps(payload))), None)
        self._after_write("sources")
        self.emit("sources-changed")

    def remove_source(self, source_id):
        self._call(IFACE_SOURCES, "RemoveSource",
                   GLib.Variant("(u)", (_as_int(source_id),)), None)
        self._after_write("sources")
        self.emit("sources-changed")

    def set_priority(self, source_id, priority):
        self._call(IFACE_SOURCES, "SetPriority",
                   GLib.Variant("(ui)", (_as_int(source_id), int(priority))),
                   None)
        self._after_write("sources")
        self.emit("sources-changed")

    def set_weight(self, source_id, weight):
        self._call(IFACE_SOURCES, "SetWeight",
                   GLib.Variant("(ui)", (_as_int(source_id), int(weight))),
                   None)
        self._after_write("sources")
        self.emit("sources-changed")

    def test_source(self, source_id):
        result = self._call(IFACE_SOURCES, "TestSource",
                            GLib.Variant("(u)", (_as_int(source_id),)),
                            self._v("(s)"))
        try:
            raw = json.loads(result.unpack()[0])
        except (TypeError, ValueError):
            raw = {}
        ok = bool(raw.get("ok"))
        detail = _as_str(raw.get("error"))
        if ok and not detail:
            detail = "发现 %d 个模型" % _as_int(raw.get("model_count"))
        return {"ok": ok, "latency_ms": _as_int(raw.get("latency_ms")),
                "detail": detail or ("探测失败" if not ok else "探测成功")}

    # ---- Keys1 -----------------------------------------------------------------
    def list_keys(self):
        return self._cached("keys", CACHE_TTL, self._fetch_keys,
                            "keys-changed")

    def _fetch_keys(self):
        result = self._call(IFACE_KEYS, "ListKeys", None, self._v("(as)"))
        return [parse_key(d) for d in self._json_list(result)]

    def _key_name(self, key_id):
        for key in self.list_keys():
            if key.id == _source_id(key_id) or key.name == key_id:
                return key.name
        raise BackendError(_("No such key: %s") % (key_id,), "NoSuchKey")

    def issue_key(self, opts):
        limits = {}
        models = opts.get("models") or ["*"]
        limits["models"] = (",".join(models) if isinstance(models, (list, tuple))
                            else str(models))
        for key in ("rpm", "tpm", "daily_tokens"):
            if opts.get(key) is not None:
                limits[key] = int(opts[key])
        if opts.get("aid"):
            limits["aid"] = opts["aid"]
        result = self._call(
            IFACE_KEYS, "IssueKey",
            GLib.Variant("(ss)", (opts.get("name") or "",
                                  json.dumps(limits))),
            self._v("(su)"))
        secret, kid = result.unpack()
        self._after_write("keys")
        self.emit("keys-changed")
        return {"secret": str(secret), "id": _source_id(kid)}

    def update_key(self, key_id, patch):
        """Keys1.UpdateKey(name, patch JSON)。数值 0 = 不限；缺省字段保持不变。"""
        payload = {}
        if "models" in patch:
            models = patch.get("models") or []
            payload["models"] = (",".join(models)
                                 if isinstance(models, (list, tuple))
                                 else str(models))
        for field_name in ("rpm", "tpm", "daily_tokens"):
            if patch.get(field_name) is not None:
                payload[field_name] = int(patch[field_name])
        if "aid" in patch:
            payload["aid"] = patch.get("aid") or ""
        self._call(IFACE_KEYS, "UpdateKey",
                   GLib.Variant("(ss)", (self._key_name(key_id),
                                         json.dumps(payload))), None)
        self._after_write("keys")
        self.emit("keys-changed")

    def suspend_key(self, key_id):
        self._call(IFACE_KEYS, "SuspendKey",
                   GLib.Variant("(s)", (self._key_name(key_id),)), None)
        self._after_write("keys")
        self.emit("keys-changed")

    def resume_key(self, key_id):
        self._call(IFACE_KEYS, "ResumeKey",
                   GLib.Variant("(s)", (self._key_name(key_id),)), None)
        self._after_write("keys")
        self.emit("keys-changed")

    def revoke_key(self, key_id):
        self._call(IFACE_KEYS, "RevokeKey",
                   GLib.Variant("(s)", (self._key_name(key_id),)), None)
        self._after_write("keys")
        self.emit("keys-changed")

    # ---- Models1（定价） ---------------------------------------------------
    def list_prices(self):
        return self._cached("prices", CACHE_TTL, self._fetch_prices,
                            "prices-changed")

    def _fetch_prices(self):
        result = self._call(IFACE_MODELS, "ListPrices", None,
                            self._v("(as)"))
        out = []
        for d in self._json_list(result):
            out.append(ModelPrice(
                alias=_as_str(d.get("alias")),
                input_per_mtok=_as_float_or_none(d.get("input_per_mtok")),
                output_per_mtok=_as_float_or_none(d.get("output_per_mtok")),
                cache_read_per_mtok=_as_float_or_none(
                    d.get("cache_read_per_mtok")),
                cache_write_per_mtok=_as_float_or_none(
                    d.get("cache_write_per_mtok"))))
        return out

    def set_price(self, alias, components):
        """Keys1.SetPrice(alias, JSON)。JSON 只带启用的分量（None=未启用）。"""
        payload = {}
        for name in ("input_per_mtok", "output_per_mtok",
                     "cache_read_per_mtok", "cache_write_per_mtok"):
            value = components.get(name)
            if value is not None:
                payload[name] = float(value)
        self._call(IFACE_MODELS, "SetPrice",
                   GLib.Variant("(ss)", (str(alias), json.dumps(payload))),
                   None)
        self._after_write("prices")
        self.emit("prices-changed")

    def remove_price(self, alias):
        self._call(IFACE_MODELS, "RemovePrice",
                   GLib.Variant("(s)", (str(alias),)), None)
        self._after_write("prices")
        self.emit("prices-changed")

    # ---- Usage1 -------------------------------------------------------------------
    def _rfc3339(self, epoch):
        from datetime import datetime, timezone
        return datetime.fromtimestamp(epoch, timezone.utc).strftime(
            "%Y-%m-%dT%H:%M:%SZ")

    def usage_summary(self, since_hours=24):
        since = self._rfc3339(time.time() - since_hours * 3600)
        result = self._call(IFACE_USAGE, "Summary",
                            GLib.Variant("(s)", (since,)), self._v("(as)"))
        groups = self._json_list(result)     # [byKey, byModel, bySource, byAid]
        by_key = []
        if groups and isinstance(groups[0], list):
            by_key = groups[0]
        summary = UsageSummary(
            since_hours=since_hours,
            requests=_as_int(sum(_as_int(g.get("requests"))
                                 for g in by_key if isinstance(g, dict))),
            tokens_in=_as_int(sum(_as_int(_first(g, "tokens_in", "in_tokens",
                                                 default=0))
                                  for g in by_key if isinstance(g, dict))),
            tokens_out=_as_int(sum(_as_int(_first(g, "tokens_out",
                                                  "out_tokens", default=0))
                                   for g in by_key if isinstance(g, dict))))
        try:
            summary.sources_health = {s.name: s.health.state
                                      for s in self.list_sources()}
        except BackendError:
            pass
        try:
            summary.active_keys = sum(1 for k in self.list_keys()
                                      if k.status == "active")
        except BackendError:
            pass
        return summary

    def usage_recent(self, limit=100):
        return self._cached("recent:%d" % int(limit), CACHE_TTL,
                            lambda: self._fetch_recent(limit), "usage-changed")

    def _fetch_recent(self, limit):
        result = self._call(IFACE_USAGE, "Recent",
                            GLib.Variant("(u)", (int(limit),)), self._v("(as)"))
        return [parse_usage_row(d) for d in self._json_list(result)]

    def usage_logs(self, since_hours=24, offset=0, limit=50, filters=None):
        """Usage1.Logs(s, u offset, u limit, s filter) → (rows, total)。"""
        filters = filters or {}
        payload = {k: filters[k] for k in
                   ("ident", "model", "source", "search", "errors")
                   if filters.get(k) is not None}
        key = "logs:%d:%d:%d:%s" % (int(since_hours), int(offset),
                                    int(limit), json.dumps(payload,
                                                           sort_keys=True))
        return self._cached(
            key, CACHE_TTL,
            lambda: self._fetch_logs(since_hours, offset, limit, payload),
            "usage-changed")

    def _fetch_logs(self, since_hours, offset, limit, payload):
        since = self._rfc3339(time.time() - since_hours * 3600)
        result = self._call(
            IFACE_USAGE, "Logs",
            GLib.Variant("(suus)", (since, int(offset), int(limit),
                                    json.dumps(payload))),
            self._v("(asu)"))
        raw, total = result.unpack()
        return [parse_usage_row(d) for d in self._json_list_result(raw)], total

    @staticmethod
    def _json_list_result(items):
        out = []
        for item in items or []:
            try:
                out.append(json.loads(item))
            except (TypeError, ValueError):
                continue
        return out

    def key_windows(self, ident):
        if not str(ident).startswith(("key:", "aid:")):
            ident = "key:%s" % ident
        result = self._call(IFACE_USAGE, "KeyWindows",
                            GLib.Variant("(s)", (str(ident),)), self._v("(s)"))
        try:
            raw = json.loads(result.unpack()[0])
        except (TypeError, ValueError):
            raw = {}
        limits = raw.get("limits") or {}
        return [
            KeyWindow("rpm", _as_int(raw.get("rpm")),
                      _as_int(limits.get("rpm"))),
            KeyWindow("tpm", _as_int(raw.get("tpm")),
                      _as_int(limits.get("tpm"))),
            KeyWindow("daily_tokens", _as_int(raw.get("daily")),
                      _as_int(limits.get("daily_tokens"))),
        ]

    def histogram(self, since_hours=24, bucket="hour", dimension=""):
        """Usage1.Histogram(s since, s bucket, s dimension) → as JSON 桶。"""
        return self._cached(
            "hist:%d:%s:%s" % (int(since_hours), bucket, dimension),
            CACHE_TTL,
            lambda: self._fetch_histogram(since_hours, bucket, dimension),
            "usage-changed")

    def _fetch_histogram(self, since_hours, bucket, dimension):
        since = self._rfc3339(time.time() - since_hours * 3600)
        result = self._call(
            IFACE_USAGE, "Histogram",
            GLib.Variant("(sss)", (since, bucket, dimension)),
            self._v("(as)"))
        return [parse_hist_bucket(d) for d in self._json_list(result)]

    def stats(self, since_hours=24):
        """Usage1.Stats(s since) → s Stats JSON。"""
        return self._cached("stats:%d" % int(since_hours), CACHE_TTL,
                            lambda: self._fetch_stats(since_hours),
                            "usage-changed")

    def _fetch_stats(self, since_hours):
        since = self._rfc3339(time.time() - since_hours * 3600)
        result = self._call(IFACE_USAGE, "Stats",
                            GLib.Variant("(s)", (since,)), self._v("(s)"))
        try:
            raw = json.loads(result.unpack()[0])
        except (TypeError, ValueError):
            raw = {}
        return parse_stats(raw or {})
