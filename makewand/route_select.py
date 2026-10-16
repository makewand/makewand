"""选择器：在起始档的候选里去掉不可用的，再用 Thompson 采样排序。

``select`` 不写账本、不调用模型。policy、quota_reader、ledger、rng、now
都可以注入。额度读不到的一家不排除，留给派活时的失败处理。

不可用的原因按这个顺序取第一条：

- policy 的 PAUSED 里还没到期的；
- 账本里还没过期的额度标记；
- codex / claude 的官方周额度达到策略阈值；
- ``not_tool`` 的同家族候选。

可用候选按目录顺序各采样一次 Beta(alpha, beta)，采样值大的排前面。
``is_self`` 的整体排在所有非 self 之后。采样值相同则保持目录顺序。
"""

from __future__ import annotations

import math
import os
import random
import re
import subprocess
import time
from dataclasses import dataclass
from datetime import datetime
from pathlib import Path
from typing import Mapping, Optional, Sequence, Union

from makewand.outcome_ledger import OutcomeLedger
from makewand.routing_catalog import candidates, escalate, family, starting_tier

__all__ = [
    "DEFAULT_CLAUDE_MAX_WEEKLY",
    "DEFAULT_CODEX_MAX_WEEKLY",
    "DEFAULT_REQUEST_BUDGET",
    "HALF_LIFE_DAYS",
    "QUOTA_ARGV",
    "QUOTA_TIMEOUT_SECONDS",
    "Pause",
    "Policy",
    "RouteCandidate",
    "RouteResult",
    "default_policy_path",
    "default_quota_reader",
    "load_policy",
    "parse_quota_text",
    "select",
]

DEFAULT_CODEX_MAX_WEEKLY = 90
DEFAULT_CLAUDE_MAX_WEEKLY = 85
DEFAULT_REQUEST_BUDGET = 3
HALF_LIFE_DAYS = 14

# 与现有 mw-hub 相同的官方额度命令。文件不存在、超时或输出不可用时当作没有数据。
QUOTA_ARGV = ("/mnt/data/makewand/bin/makewand-server", "quota")
QUOTA_TIMEOUT_SECONDS = 60

_QUOTA_PROVIDERS = ("codex", "claude")
_POLICY_KEY = re.compile(r"[A-Za-z_][A-Za-z0-9_]*\Z")
_PAUSE_ENTRY = re.compile(r"([A-Za-z][A-Za-z0-9_-]*):(\d{4}-\d{2}-\d{2}T\d{2}:\d{2})\Z")
_QUOTA_HEADER = re.compile(r"(?:[\u2500\u2014\-]){2,}\s*([A-Za-z][A-Za-z0-9_-]*)\b")
_QUOTA_WEEKLY = re.compile(
    r"weekly\s+([0-9]+(?:\.[0-9]+)?)%\s*used(?:\s*\(\s*resets\s+([^)]*?)\s*\))?",
    re.IGNORECASE,
)
_HEADER_MARK = "\u2500\u2014-"


@dataclass(frozen=True)
class Pause:
    """一条 PAUSED。``until`` 是本地时间对应的 epoch 秒，``text`` 是原文时间。"""

    name: str
    until: float
    text: str


@dataclass(frozen=True)
class Policy:
    """分派策略。缺文件或缺字段时用默认阈值，暂停列表为空。"""

    codex_max_weekly: float = DEFAULT_CODEX_MAX_WEEKLY
    claude_max_weekly: float = DEFAULT_CLAUDE_MAX_WEEKLY
    paused: tuple = ()
    request_budget: int = DEFAULT_REQUEST_BUDGET

    def __post_init__(self) -> None:
        object.__setattr__(self, "codex_max_weekly", float(self.codex_max_weekly))
        object.__setattr__(self, "claude_max_weekly", float(self.claude_max_weekly))
        object.__setattr__(self, "paused", tuple(self.paused))
        object.__setattr__(self, "request_budget", _coerce_budget(self.request_budget))

    def pause_for(self, provider: str) -> Optional[Pause]:
        key = provider.strip().lower()
        for item in self.paused:
            if item.name == key:
                return item
        return None


@dataclass(frozen=True)
class RouteCandidate:
    """一个排好序的可用候选。``args`` 是传给 makewand 的参数。"""

    provider: str
    args: tuple
    label: str
    is_self: bool
    sampled_score: float


@dataclass(frozen=True)
class RouteResult:
    """一次选择的结果。``escalate_tier`` 是下一档，已经是 3 档时为 None。"""

    tier: int
    candidates: list
    skipped: list
    escalate_tier: Optional[int]


def default_policy_path() -> Path:
    """策略文件路径。MW_HUB_POLICY 优先，否则是 ~/.config/mw-hub/policy。"""
    override = os.environ.get("MW_HUB_POLICY", "").strip()
    if override:
        return Path(override).expanduser()
    return Path.home() / ".config" / "mw-hub" / "policy"


def load_policy(path: Union[str, Path, None] = None) -> Policy:
    """读取 bash 风格的 KEY=VALUE 策略。

    支持空行、``#`` 注释、可选的 ``export`` 前缀，以及单引号或双引号。
    显式 ``path`` 优先于 MW_HUB_POLICY。文件不存在、不可读或无法解码时用默认值。

    PAUSED 是空格分隔的 ``name:YYYY-MM-DDTHH:MM``（本地时间）。解析失败的
    条目忽略，其余保留；同名时以后出现的为准。
    """
    file_path = Path(path).expanduser() if path is not None else default_policy_path()
    try:
        text = file_path.read_text(encoding="utf-8")
    except (OSError, UnicodeError):
        return Policy()
    values = _parse_policy_text(text)
    return Policy(
        codex_max_weekly=_parse_limit(values.get("CODEX_MAX_WEEKLY"), DEFAULT_CODEX_MAX_WEEKLY),
        claude_max_weekly=_parse_limit(values.get("CLAUDE_MAX_WEEKLY"), DEFAULT_CLAUDE_MAX_WEEKLY),
        paused=_parse_pauses(values.get("PAUSED", "")),
        request_budget=_parse_budget(values.get("REQUEST_BUDGET")),
    )


def parse_quota_text(text: Optional[str]) -> dict:
    """解析 ``makewand-server quota`` 的人类可读输出。

    返回 ``{provider: (used_percent, resets_text)}``。段落标题形如
    ``── claude``，周额度行形如 ``weekly  84% used  (resets 10-20 15:04)``。
    没有 resets 时第二项是空字符串。认不出的文本返回空 dict，不抛异常。
    同一段里只取第一条 weekly 用量，避免被 “per-model weekly cap” 盖掉。
    """
    if not isinstance(text, str) or not text.strip():
        return {}
    found = {}
    provider = None
    for line in text.splitlines():
        header = _header_provider(line)
        if header is not None:
            provider = header
            continue
        if provider is None or provider in found:
            continue
        match = _QUOTA_WEEKLY.search(line)
        if match is None:
            continue
        try:
            percent = float(match.group(1))
        except (TypeError, ValueError):
            continue
        if not math.isfinite(percent):
            continue
        resets = match.group(2)
        resets_text = f"resets {resets.strip()}" if resets and resets.strip() else ""
        found[provider] = (percent, resets_text)
    return found


def _header_provider(line: str) -> Optional[str]:
    stripped = line.strip()
    if not stripped or stripped[0] not in _HEADER_MARK:
        return None
    match = _QUOTA_HEADER.match(stripped)
    if match is None:
        return None
    return match.group(1).strip().lower()


def default_quota_reader() -> dict:
    """运行官方 quota 命令并解析。失败时返回空 dict，不抛异常。"""
    try:
        completed = subprocess.run(
            QUOTA_ARGV,
            capture_output=True,
            text=True,
            encoding="utf-8",
            errors="replace",
            timeout=QUOTA_TIMEOUT_SECONDS,
            check=False,
            stdin=subprocess.DEVNULL,
        )
    except (OSError, subprocess.SubprocessError):
        return {}
    stdout = completed.stdout if isinstance(getattr(completed, "stdout", None), str) else ""
    try:
        return parse_quota_text(stdout)
    except Exception:
        return {}


def select(
    task_type: str,
    difficulty: str,
    risk: str = "low",
    self_tool: Optional[str] = None,
    not_tool: Optional[str] = None,
    tier_override: Optional[Union[int, str]] = None,
    policy: Optional[Policy] = None,
    quota_reader=None,
    ledger: Optional[OutcomeLedger] = None,
    rng: Optional[random.Random] = None,
    now: Optional[Union[float, datetime]] = None,
) -> RouteResult:
    """选择这一档要派的候选。

    采样用 ``rng.betavariate(alpha, beta)``，只对还可用的候选、按目录顺序
    各调用一次。``rng`` 用 ``random.Random(seed)`` 时排序可以复现。
    """
    if policy is None:
        policy = load_policy()
    elif not isinstance(policy, Policy):
        raise TypeError("policy must be a Policy or None")
    if ledger is None:
        ledger = OutcomeLedger()
    if rng is None:
        rng = random.Random()
    if quota_reader is None:
        quota_reader = default_quota_reader
    moment = _resolve_now(now)
    _validate_self_tool(self_tool)
    _validate_not_tool(not_tool)
    tier = _resolve_tier(task_type, difficulty, risk, tier_override)
    rows = candidates(tier)

    marks = {}
    for name, mark in ledger.active_marks(moment).items():
        if isinstance(name, str) and name.strip():
            marks[name.strip().lower()] = mark

    blocked_early = []
    for row in rows:
        if _pause_reason(policy, row.provider, moment) or _mark_reason(marks.get(row.provider), moment):
            blocked_early.append(row.provider)
    quota = _read_quota(rows, blocked_early, quota_reader)

    skipped = []
    available = []
    for index, row in enumerate(rows):
        reason = (
            _pause_reason(policy, row.provider, moment)
            or _mark_reason(marks.get(row.provider), moment)
            or _quota_reason(row.provider, quota, policy)
            or _family_reason(row.provider, not_tool)
        )
        if reason:
            skipped.append((row.provider, reason))
            continue
        available.append((index, row))

    scored = []
    for index, row in available:
        stats = ledger.stats(row.provider, tier, task_type, now=moment, half_life_days=HALF_LIFE_DAYS)
        alpha = _positive_param(stats.alpha)
        beta = _positive_param(stats.beta)
        score = float(rng.betavariate(alpha, beta))
        is_self = _is_self(row.provider, self_tool)
        scored.append((1 if is_self else 0, -score, index, row, score, is_self))
    scored.sort()

    chosen = [
        RouteCandidate(
            provider=row.provider,
            args=tuple(row.args),
            label=row.label,
            is_self=is_self,
            sampled_score=score,
        )
        for _self_flag, _neg_score, _index, row, score, is_self in scored
    ]
    return RouteResult(
        tier=tier,
        candidates=chosen,
        skipped=skipped,
        escalate_tier=escalate(tier),
    )


def _resolve_now(now: Optional[Union[float, datetime]]) -> float:
    if now is None:
        return time.time()
    moment = _as_epoch(now)
    if moment is None:
        raise TypeError("now must be an epoch timestamp or datetime")
    return moment


def _resolve_tier(task_type: str, difficulty: str, risk: str, tier_override) -> int:
    # 覆盖档位也先校验类型、难度和风险，非法值照样说明合法值。
    base = starting_tier(task_type, difficulty, risk)
    if tier_override is None:
        return base
    if isinstance(tier_override, str):
        text = tier_override.strip()
        if text not in {"1", "2", "3"}:
            raise ValueError(f"invalid tier {tier_override!r}; legal values: 1, 2, 3")
        tier_override = int(text)
    candidates(tier_override)
    return tier_override


def _read_quota(rows: Sequence, blocked_early: Sequence[str], quota_reader) -> dict:
    """还有 codex/claude 可能被阈值排除时才读额度。

    返回值不是映射、或某家没有读数时，这家不排除。命令失败由
    ``default_quota_reader`` 收成空映射。
    """
    blocked = set(blocked_early)
    if not any(row.provider in _QUOTA_PROVIDERS and row.provider not in blocked for row in rows):
        return {}
    raw = quota_reader()
    if not isinstance(raw, Mapping):
        return {}
    quota = {}
    for name, entry in raw.items():
        if isinstance(name, str) and name.strip():
            quota[name.strip().lower()] = entry
    return quota


def _pause_reason(policy: Policy, provider: str, now: float) -> Optional[str]:
    pause = policy.pause_for(provider)
    # 到点即到期：until == now 不再跳过，和额度标记同一规则。
    if pause is None or pause.until <= now:
        return None
    return f"暂停到 {pause.text}"


def _mark_reason(mark, now: float) -> Optional[str]:
    if mark is None:
        return None
    until = _as_epoch(mark.until)
    if until is None or until <= now:
        return None
    reason = mark.reason
    text = reason.strip() if isinstance(reason, str) else ""
    if text:
        return f"额度标记未过期：{text}"
    return "额度标记未过期"


def _quota_reason(provider: str, quota: Mapping, policy: Policy) -> Optional[str]:
    if provider == "codex":
        limit = policy.codex_max_weekly
    elif provider == "claude":
        limit = policy.claude_max_weekly
    else:
        return None
    # 这家没有读数时不排除。派活失败再由调用方记标记。
    if provider not in quota:
        return None
    percent, resets = _coerce_quota_entry(quota[provider])
    if percent is None or percent < limit:
        return None
    reason = f"周额度已用 {_format_percent(percent)}%，达到阈值 {_format_percent(limit)}%"
    if resets:
        reason += f"（{resets}）"
    return reason


def _validate_not_tool(not_tool: Optional[str]) -> None:
    if not_tool is None:
        return
    if not isinstance(not_tool, str):
        raise TypeError("not_tool must be a string or None")
    if not_tool.strip():
        family(not_tool)


def _validate_self_tool(self_tool: Optional[str]) -> None:
    if self_tool is None:
        return
    if not isinstance(self_tool, str):
        raise TypeError("self_tool must be a string or None")


def _family_reason(provider: str, not_tool: Optional[str]) -> Optional[str]:
    if not_tool is None:
        return None
    name = not_tool.strip()
    if not name:
        return None
    blocked = family(name)
    if family(provider) != blocked:
        return None
    return f"与 {name.strip().lower()} 同属 {blocked} 家族"


def _is_self(provider: str, self_tool: Optional[str]) -> bool:
    if not self_tool or not isinstance(self_tool, str):
        return False
    name = self_tool.strip().lower()
    return bool(name) and provider.strip().lower() == name


def _positive_param(value) -> float:
    try:
        number = float(value)
    except (TypeError, ValueError):
        return 1.0
    if not math.isfinite(number) or number <= 0:
        return 1.0
    return number


def _as_epoch(value) -> Optional[float]:
    if isinstance(value, datetime):
        moment = value.timestamp()
    elif isinstance(value, bool) or not isinstance(value, (int, float)):
        return None
    else:
        moment = float(value)
    if not math.isfinite(moment):
        return None
    return moment


def _coerce_quota_entry(entry) -> tuple:
    resets = ""
    if isinstance(entry, (tuple, list)):
        if not entry:
            return None, ""
        percent = entry[0]
        if len(entry) > 1 and entry[1] is not None:
            resets = str(entry[1]).strip()
    elif isinstance(entry, bool) or not isinstance(entry, (int, float, str)):
        return None, ""
    else:
        percent = entry
    if isinstance(percent, bool):
        return None, ""
    try:
        number = float(percent)
    except (TypeError, ValueError):
        return None, ""
    if not math.isfinite(number):
        return None, ""
    return number, resets


def _format_percent(value: float) -> str:
    number = float(value)
    if number.is_integer():
        return str(int(number))
    return f"{number:g}"


def _parse_policy_text(text: str) -> dict:
    values = {}
    for raw_line in text.splitlines():
        line = raw_line.strip()
        if not line or line.startswith("#"):
            continue
        if line.startswith("export "):
            line = line[len("export "):].strip()
        if "=" not in line:
            continue
        key, _, raw_value = line.partition("=")
        key = key.strip()
        if _POLICY_KEY.fullmatch(key) is None:
            continue
        values[key] = _parse_policy_value(raw_value.strip())
    return values


def _parse_policy_value(text: str) -> str:
    if not text or text[0] == "#":
        return ""
    if text[0] in "\"'":
        return _unquote(text, text[0]).strip()
    comment = re.search(r"[ \t]#", text)
    if comment:
        text = text[:comment.start()]
    return text.strip()


def _unquote(text: str, quote: str) -> str:
    chars = []
    index = 1
    length = len(text)
    while index < length:
        char = text[index]
        if quote == "'":
            if char == "'":
                return "".join(chars)
            chars.append(char)
            index += 1
            continue
        if char == "\\" and index + 1 < length and text[index + 1] in "\\\"$`":
            chars.append(text[index + 1])
            index += 2
            continue
        if char == quote:
            return "".join(chars)
        chars.append(char)
        index += 1
    return "".join(chars)


def _coerce_budget(value) -> int:
    """每个请求默认最多调用次数。非法或非正数时用默认值。"""
    if isinstance(value, bool):
        return DEFAULT_REQUEST_BUDGET
    try:
        number = float(str(value).strip()) if isinstance(value, str) else float(value)
    except (TypeError, ValueError):
        return DEFAULT_REQUEST_BUDGET
    if not math.isfinite(number) or number <= 0:
        return DEFAULT_REQUEST_BUDGET
    return int(number)


def _parse_budget(value: Optional[str]) -> int:
    if value is None or value.strip() == "":
        return DEFAULT_REQUEST_BUDGET
    return _coerce_budget(value)


def _parse_limit(value: Optional[str], default: float) -> float:
    if value is None or value.strip() == "":
        return float(default)
    try:
        number = float(value.strip())
    except (TypeError, ValueError):
        return float(default)
    if not math.isfinite(number) or number < 0:
        return float(default)
    return number


def _parse_pauses(value: str) -> tuple:
    found = {}
    order = []
    for token in value.split():
        match = _PAUSE_ENTRY.fullmatch(token)
        if match is None:
            continue
        name = match.group(1).lower()
        text = match.group(2)
        try:
            until = datetime.strptime(text, "%Y-%m-%dT%H:%M").timestamp()
        except ValueError:
            continue
        if not math.isfinite(until):
            continue
        if name not in found:
            order.append(name)
        found[name] = Pause(name, until, text)
    return tuple(found[name] for name in order)
