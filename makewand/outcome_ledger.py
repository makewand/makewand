"""成绩账本与额度标记：makewand 私有状态目录下 routing/ 的持久化。

outcomes.jsonl 只追加，每行一条 JSON（ts、provider、tier、task_type、
passed、seconds、calls）。给了 model / effort 时再在行尾追加这两个字段，
不给则保持原来的字段顺序。写入时用 makewand.filelock 的跨进程文件锁，
保证多进程并发追加不会交错损坏。坏行在读取时跳过，不让整个账本失效。
records() 在锁内读完即释放锁，再产出快照迭代器，慢消费者不会堵住写入。

stats() 按指数时间衰减加权计算 Beta 后验（先验 1/1）、有效样本数
与加权平均耗时/调用次数，now 与 half_life_days 均可注入。
不给 model / effort 时按 provider/tier/task_type 统计（旧记录，以及带
model/effort 的新记录，只要档位相同都算进去）。给了其中之一就按
provider/model/effort/task_type 统计，不再用 tier 做过滤；没有这两个
字段的旧记录不会混进候选级统计。
观察时刻之后的记录不计入，因此注入的历史 now 不会读到“未来”样本。

marks.json 记 provider -> {until, reason}，写入时在锁内读-改-写并
通过临时文件 + os.replace 原子替换；文件损坏时当成空账本。
"""

from __future__ import annotations

import json
import math
import os
import re
import tempfile
import time
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Dict, Iterator, List, Optional

from makewand import filelock

__all__ = [
    "OutcomeLedger",
    "Stats",
    "Mark",
    "routing_dir",
    "default_ledger",
]

_PRIOR_ALPHA = 1.0
_PRIOR_BETA = 1.0
_SECONDS_PER_DAY = 86400.0
_O_NOFOLLOW = getattr(os, "O_NOFOLLOW", 0)
FAILURE_KINDS = frozenset(("startup", "auth", "quota", "timeout", "unknown", "invalid_output", "verification"))
CALLERS = frozenset(("claude", "codex", "agy", "grok", "muse", "human", "automation"))


def _outcome_details(row):
    """Validate bounded metadata; candidate approval never becomes tool quality."""
    details = {}
    if "review_validated" in row:
        value = row["review_validated"]
        if row["task_type"] != "review" or type(value) is not bool or value != row["passed"]:
            raise ValueError("review_validated must equal the review work's pass/fail result")
        details["review_validated"] = value
    if "candidate_approved" in row:
        value = row["candidate_approved"]
        if row["task_type"] != "review" or value is not None and type(value) is not bool:
            raise ValueError("candidate_approved is only a review verdict (true/false/null)")
        if row.get("review_validated") is True and value is None:
            raise ValueError("an unverified candidate verdict cannot validate a review")
        details["candidate_approved"] = value
    for field in ("caller", "request_id", "candidate_hash", "failure_kind"):
        if field not in row:
            continue
        value = row[field]
        if not isinstance(value, str) or not value:
            raise ValueError(f"invalid {field}")
        if field == "caller" and value not in CALLERS:
            raise ValueError("invalid caller")
        if field == "request_id":
            from makewand.execution_contract import identifier
            identifier(value, field)
        if field == "candidate_hash" and not re.fullmatch(r"[0-9a-f]{64}", value):
            raise ValueError("candidate_hash must be a lowercase SHA256")
        if field == "failure_kind" and (value not in FAILURE_KINDS or row["passed"]):
            raise ValueError("failure_kind requires a failed work result and a known category")
        details[field] = value
    return details


def _coerce_ts(value: Any, name: str = "ts") -> float:
    """把 epoch 秒 / datetime 统一成 float epoch 秒。"""
    if isinstance(value, datetime):
        moment = value if value.tzinfo is not None else value.replace(tzinfo=timezone.utc)
        return float(moment.timestamp())
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise TypeError(f"{name} must be an epoch timestamp or datetime, got {type(value).__name__}")
    result = float(value)
    if not math.isfinite(result):
        raise ValueError(f"{name} must be finite")
    return result


def _is_number(value: Any) -> bool:
    return isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(float(value))


def _state_root() -> Path:
    """复用 makewand.config 已有的状态目录逻辑（尊重 XDG_STATE_HOME）。"""
    try:
        from makewand import config as _config

        factory = getattr(_config, "_default_state_dir", None)
        if callable(factory):
            return Path(factory())
    except Exception:
        pass
    xdg_state = os.environ.get("XDG_STATE_HOME", "")
    if xdg_state and Path(xdg_state).expanduser().is_absolute():
        base = Path(xdg_state).expanduser()
    else:
        base = Path.home() / ".local" / "state"
    return base / "makewand"


def routing_dir() -> Path:
    """默认 routing 目录：<私有状态目录>/makewand/routing（每次调用动态解析）。"""
    override = os.environ.get("MAKEWAND_ROUTING_DIR", "")
    if override:
        return Path(os.path.abspath(Path(override).expanduser()))
    return _state_root() / "routing"


@dataclass(frozen=True)
class Mark:
    """一条额度标记：until 为 epoch 秒，reason 为原因说明。"""

    until: float
    reason: str = ""


@dataclass
class Stats:
    """带时间衰减的 Beta 后验统计。没有数据时 alpha/beta 为先验 1/1，
    n 与 count 为 0，平均耗时与平均调用次数为 None。"""

    alpha: float = _PRIOR_ALPHA
    beta: float = _PRIOR_BETA
    n: float = 0.0
    count: int = 0
    avg_seconds: Optional[float] = None
    avg_calls: Optional[float] = None


def _validate_row(row: Any) -> Optional[Dict[str, Any]]:
    """校验一行账本记录；坏行（缺字段、类型错、非有限/负数/档位越界）返回 None。"""
    if not isinstance(row, dict):
        return None
    for key in ("ts", "provider", "tier", "task_type", "passed", "seconds", "calls"):
        if key not in row:
            return None
    if not _is_number(row["ts"]):
        return None
    if not isinstance(row["provider"], str) or not row["provider"].strip():
        return None
    tier = row["tier"]
    if isinstance(tier, bool) or not isinstance(tier, (int, float)):
        return None
    if isinstance(tier, float):
        if not float(tier).is_integer():
            return None
        tier = int(tier)
    if int(tier) < 1:
        return None
    if not isinstance(row["task_type"], str) or not row["task_type"].strip():
        return None
    if type(row["passed"]) is not bool:
        return None
    seconds = row["seconds"]
    if seconds is not None and (not _is_number(seconds) or float(seconds) < 0):
        return None
    calls = row["calls"]
    if calls is not None:
        if isinstance(calls, bool) or not isinstance(calls, (int, float)):
            return None
        if isinstance(calls, float):
            if not float(calls).is_integer():
                return None
            calls = int(calls)
        if int(calls) < 0:
            return None
    normalized = {
        "ts": float(row["ts"]),
        "provider": row["provider"],
        "tier": int(tier),
        "task_type": row["task_type"],
        "passed": row["passed"],
        "seconds": None if seconds is None else float(seconds),
        "calls": None if calls is None else int(calls),
    }
    # model / effort 是后加的可选字段。缺了是旧记录；在了但不是非空字符串则整行作废。
    if "model" in row and row["model"] is not None:
        if not isinstance(row["model"], str) or not row["model"].strip():
            return None
        normalized["model"] = row["model"].strip()
    if "effort" in row and row["effort"] is not None:
        if not isinstance(row["effort"], str) or not row["effort"].strip():
            return None
        normalized["effort"] = row["effort"].strip().lower()
    try:
        normalized.update(_outcome_details(row))
    except (TypeError, ValueError):
        return None
    return normalized


def _normalize_tier(tier: Any) -> int:
    if isinstance(tier, bool):
        raise TypeError("tier must be an int")
    if isinstance(tier, float) and float(tier).is_integer():
        tier = int(tier)
    if isinstance(tier, str) and tier.strip().lstrip("+-").isdigit():
        tier = int(tier.strip())
    if not isinstance(tier, int):
        raise TypeError("tier must be an int")
    if tier < 1:
        raise ValueError("tier must be >= 1")
    return int(tier)


def _normalize_passed(passed: Any) -> bool:
    if type(passed) is bool:
        return passed
    if isinstance(passed, str):
        lowered = passed.strip().lower()
        if lowered in ("pass", "passed", "true", "1", "ok", "success"):
            return True
        if lowered in ("fail", "failed", "false", "0", "error"):
            return False
    if isinstance(passed, int) and passed in (0, 1):
        return bool(passed)
    raise TypeError("passed must be a bool")


def _normalize_seconds(seconds: Any) -> Optional[float]:
    if seconds is None:
        return None
    if not _is_number(seconds):
        raise TypeError("seconds must be a number or None")
    value = float(seconds)
    if value < 0:
        raise ValueError("seconds must be >= 0")
    return value


def _normalize_optional_text(value: Any, name: str, *, lower: bool = False) -> Optional[str]:
    """None 表示“不按这个字段统计/不写入”。空字符串和其他类型拒绝。"""
    if value is None:
        return None
    if not isinstance(value, str) or not value.strip():
        raise ValueError(f"{name} must be a non-empty string or None")
    text = value.strip()
    return text.lower() if lower else text


def _normalize_calls(calls: Any) -> Optional[int]:
    if calls is None:
        return None
    if isinstance(calls, bool) or not isinstance(calls, (int, float)):
        raise TypeError("calls must be an int or None")
    if isinstance(calls, float):
        if not float(calls).is_integer():
            raise ValueError("calls must be an integer")
        calls = int(calls)
    if int(calls) < 0:
        raise ValueError("calls must be >= 0")
    return int(calls)


class OutcomeLedger:
    """routing/ 目录下的成绩账本（outcomes.jsonl）与额度标记（marks.json）。"""

    def __init__(self, base_dir: Any = None):
        if base_dir is not None:
            self._dir = Path(os.path.abspath(Path(os.fspath(base_dir)).expanduser()))
            self._state_root: Optional[Path] = None
        else:
            self._dir = routing_dir()
            override = os.environ.get("MAKEWAND_ROUTING_DIR", "")
            self._state_root = None if override else self._dir.parent
        # Deliberately do not create the directory here: read-only callers
        # (e.g. `makewand route`) must work when the state directory is
        # missing or read-only. Only writes (record/mark) create it.

    @property
    def directory(self) -> Path:
        return self._dir

    @property
    def outcomes_path(self) -> Path:
        return self._dir / "outcomes.jsonl"

    @property
    def marks_path(self) -> Path:
        return self._dir / "marks.json"

    def _ensure_dir(self) -> Path:
        from makewand.config import ensure_private_dir

        if self._state_root is not None and self._state_root != self._dir:
            ensure_private_dir(self._state_root)
        return Path(ensure_private_dir(self._dir))

    def _fsync_dir(self) -> None:
        try:
            dir_fd = os.open(self._dir, os.O_RDONLY)
        except OSError:
            return
        try:
            os.fsync(dir_fd)
        except OSError:
            pass
        finally:
            os.close(dir_fd)

    def record(
        self,
        provider: str,
        tier: Any,
        task_type: str,
        passed: Any,
        seconds: Any = None,
        calls: Any = None,
        ts: Any = None,
        model: Any = None,
        effort: Any = None,
        infer_request_id: Any = None,
        **details,
    ) -> Dict[str, Any]:
        """追加一条成绩记录。ts 可注入（epoch 秒或 datetime），默认真实时钟。

        model / effort 可选。不给就不写入，行的字段和旧账本一致；给了就记在行尾，
        供候选级 stats 使用。
        """
        if not isinstance(provider, str) or not provider.strip():
            raise ValueError("provider must be a non-empty string")
        if not isinstance(task_type, str) or not task_type.strip():
            raise ValueError("task_type must be a non-empty string")
        stamp = time.time() if ts is None else _coerce_ts(ts, "ts")
        row = {
            "ts": stamp,
            "provider": provider,
            "tier": _normalize_tier(tier),
            "task_type": task_type,
            "passed": _normalize_passed(passed),
            "seconds": _normalize_seconds(seconds),
            "calls": _normalize_calls(calls),
        }
        model_text = _normalize_optional_text(model, "model")
        effort_text = _normalize_optional_text(effort, "effort", lower=True)
        if model_text is not None:
            row["model"] = model_text
        if effort_text is not None:
            row["effort"] = effort_text
        if set(details) - {"review_validated", "candidate_approved", "caller", "request_id", "candidate_hash", "failure_kind"}:
            raise TypeError("unsupported outcome metadata")
        row.update(_outcome_details(dict(row, **details)))
        payload = (json.dumps(row, ensure_ascii=False, allow_nan=False) + "\n").encode("utf-8")
        self._ensure_dir()
        if not _O_NOFOLLOW and os.path.islink(self.outcomes_path):
            raise PermissionError(f"refusing symlinked outcomes file: {self.outcomes_path}")
        created = not os.path.lexists(self.outcomes_path)
        fd = os.open(self.outcomes_path, os.O_RDWR | os.O_APPEND | os.O_CREAT | _O_NOFOLLOW, 0o600)
        try:
            if os.name != "nt":
                try:
                    os.fchmod(fd, 0o600)
                except OSError:
                    pass
            filelock.flock(fd, filelock.LOCK_EX)
            try:
                if "request_id" not in details and callable(infer_request_id):
                    inferred = infer_request_id(self._read_rows(strict=False, locked=True))
                    if inferred is not None:
                        details = dict(details, request_id=inferred)
                        row.update(_outcome_details(dict(row, **details)))
                        payload = (json.dumps(row, ensure_ascii=False, allow_nan=False) + "\n").encode("utf-8")
                # A truncated tail line without a trailing newline (crash
                # mid-append, hand-truncated file) must not swallow this
                # record: terminate that line first, under the write lock.
                # The truncated line itself stays unreadable and is skipped
                # by readers, exactly like any other bad line.
                prefix = b""
                try:
                    size = os.fstat(fd).st_size
                    if size > 0:
                        if hasattr(os, "pread"):
                            last = os.pread(fd, 1, size - 1)
                        else:
                            os.lseek(fd, size - 1, os.SEEK_SET)
                            last = os.read(fd, 1)
                        if last != b"\n":
                            prefix = b"\n"
                except OSError:
                    prefix = b""
                view = memoryview(prefix + payload)
                while view:
                    written = os.write(fd, view)
                    if written <= 0:
                        raise OSError("short write while appending to outcomes ledger")
                    view = view[written:]
                os.fsync(fd)
                if created:
                    self._fsync_dir()
            finally:
                filelock.flock(fd, filelock.LOCK_UN)
        finally:
            os.close(fd)
        return dict(row)

    def _read_rows(self, *, strict=False, locked=False) -> List[Dict[str, Any]]:
        try:
            stream = open(self.outcomes_path, "r", encoding="utf-8", errors="strict" if strict else "replace")
        except FileNotFoundError:
            return []
        except OSError:
            if strict:
                raise
            # Missing or unreadable (e.g. read-only state dir): empty ledger.
            return []
        rows: List[Dict[str, Any]] = []
        with stream:
            if not locked:
                try:
                    filelock.flock(stream.fileno(), filelock.LOCK_SH)
                except OSError:
                    if strict:
                        raise
                    pass
            try:
                for line in stream:
                    text = line.strip()
                    if not text:
                        continue
                    try:
                        parsed = json.loads(text)
                    except ValueError:
                        if strict:
                            raise ValueError("cannot verify request cooldowns from corrupt outcome history") from None
                        continue
                    row = _validate_row(parsed)
                    if row is not None:
                        rows.append(row)
                    elif strict:
                        raise ValueError("cannot verify request cooldowns from invalid outcome metadata")
            finally:
                if not locked:
                    try:
                        filelock.flock(stream.fileno(), filelock.LOCK_UN)
                    except OSError:
                        pass
        return rows

    def records(self) -> Iterator[Dict[str, Any]]:
        """产出账本中的有效记录（坏行跳过）；锁内快照读完即释放锁。"""
        return iter(self._read_rows())

    def request_cooldowns(self, request_id, now=None):
        """Only recorded operational failures cool a provider in this request.

        Unknown outcomes/timeouts need explicit retry approval, not a timer.
        A candidate rejection is never an operational provider failure.
        """
        if not request_id:
            return {}
        from makewand.execution_contract import identifier
        identifier(request_id, "request_id")
        moment = time.time() if now is None else _coerce_ts(now)
        latest = {}
        # The append lock orders operational observations even when the wall
        # clock steps backward. Future-dated failures still require approval;
        # accepted work appended afterward may supersede them with a lower ts.
        rows = (row for row in self._read_rows(strict=True) if row.get("request_id") == request_id)
        for row in rows:
            provider = row["provider"]
            if row["passed"]:
                latest.pop(provider, None)
            elif row.get("failure_kind") in ("unknown", "timeout"):
                latest[provider] = row
            elif latest.get(provider, {}).get("failure_kind") not in ("unknown", "timeout"):
                if row.get("failure_kind") in ("startup", "auth", "quota"):
                    latest[provider] = row
        blocked = {}
        for provider, row in latest.items():
            kind = row.get("failure_kind")
            if not row["passed"] and kind in ("unknown", "timeout"):
                blocked[provider] = f"本请求上次结果为 {kind}；需用户批准后使用 --retry-approved"
            elif not row["passed"] and kind in ("startup", "auth", "quota") and moment < row["ts"] + 300:
                blocked[provider] = f"本请求上次 {kind} 失败；冷却 5 分钟"
        return blocked

    def stats(
        self,
        provider: str,
        tier: Any,
        task_type: str,
        now: Any = None,
        half_life_days: float = 14,
        model: Any = None,
        effort: Any = None,
    ) -> Stats:
        """带指数时间衰减的 Beta 后验统计。

        权重 w = 0.5 ** (age / half_life)，age 以 half_life_days 为半衰期；
        alpha = 1 + Σ w(通过)，beta = 1 + Σ w(失败)，n = Σ w（有效样本数），
        平均耗时/调用次数为同一组权重下的加权平均（没有计入的数据时为 None）。
        ts 晚于 now 的记录视为尚未发生，直接跳过。

        model 和 effort 都不给时，按 provider/tier/task_type 过滤（旧行为）。
        给了其中任何一个，就按 provider/model/effort/task_type 过滤，tier 不参与；
        只给了一个字段时，另一个字段不作为条件。
        """
        if isinstance(half_life_days, bool) or not isinstance(half_life_days, (int, float)):
            raise TypeError("half_life_days must be a number")
        if not math.isfinite(float(half_life_days)) or float(half_life_days) <= 0:
            raise ValueError("half_life_days must be a positive finite number")
        moment = time.time() if now is None else _coerce_ts(now, "now")
        wanted_tier = _normalize_tier(tier)
        want_model = _normalize_optional_text(model, "model")
        want_effort = _normalize_optional_text(effort, "effort", lower=True)
        candidate_query = want_model is not None or want_effort is not None
        half_life_seconds = float(half_life_days) * _SECONDS_PER_DAY
        return self._stats_from_rows(
            self._read_rows(), provider, wanted_tier, task_type, moment,
            half_life_seconds, want_model, want_effort, candidate_query)

    @staticmethod
    def _stats_from_rows(rows, provider, wanted_tier, task_type, moment,
                         half_life_seconds, want_model, want_effort, candidate_query) -> Stats:
        alpha = _PRIOR_ALPHA
        beta = _PRIOR_BETA
        n = 0.0
        count = 0
        seconds_weighted = 0.0
        seconds_weights = 0.0
        calls_weighted = 0.0
        calls_weights = 0.0
        for row in rows:
            if row["provider"] != provider or row["task_type"] != task_type:
                continue
            if candidate_query:
                if want_model is not None and row.get("model") != want_model:
                    continue
                if want_effort is not None and row.get("effort") != want_effort:
                    continue
            elif row["tier"] != wanted_tier:
                continue
            age = moment - row["ts"]
            if age < 0:
                continue
            weight = 0.5 ** (age / half_life_seconds)
            count += 1
            n += weight
            if row["passed"]:
                alpha += weight
            else:
                beta += weight
            if row["seconds"] is not None:
                seconds_weighted += weight * row["seconds"]
                seconds_weights += weight
            if row["calls"] is not None:
                calls_weighted += weight * row["calls"]
                calls_weights += weight
        return Stats(
            alpha=alpha,
            beta=beta,
            n=n,
            count=count,
            avg_seconds=(seconds_weighted / seconds_weights) if seconds_weights > 0 else None,
            avg_calls=(calls_weighted / calls_weights) if calls_weights > 0 else None,
        )

    def stats_many(
        self,
        requests,
        now: Any = None,
        half_life_days: float = 14,
    ) -> List[Stats]:
        """批量统计：只读一次账本快照，在内存里算出每个请求的统计。

        每个请求可以是 dict（provider/tier/task_type，可选 model/effort）
        或元组 (provider, tier, task_type[, model[, effort]])。返回与请求
        同序的 Stats 列表，结果与逐个调用 stats() 完全一致。
        """
        if isinstance(half_life_days, bool) or not isinstance(half_life_days, (int, float)):
            raise TypeError("half_life_days must be a number")
        if not math.isfinite(float(half_life_days)) or float(half_life_days) <= 0:
            raise ValueError("half_life_days must be a positive finite number")
        moment = time.time() if now is None else _coerce_ts(now, "now")
        half_life_seconds = float(half_life_days) * _SECONDS_PER_DAY
        prepared = []
        for request in requests:
            if isinstance(request, dict):
                provider = request.get("provider")
                tier = request.get("tier")
                task_type = request.get("task_type")
                model = request.get("model")
                effort = request.get("effort")
            else:
                parts = tuple(request)
                if len(parts) == 3:
                    provider, tier, task_type = parts
                    model = effort = None
                elif len(parts) == 4:
                    provider, tier, task_type, model = parts
                    effort = None
                elif len(parts) == 5:
                    provider, tier, task_type, model, effort = parts
                else:
                    raise ValueError("each request must have provider, tier, task_type[, model[, effort]]")
            wanted_tier = _normalize_tier(tier)
            want_model = _normalize_optional_text(model, "model")
            want_effort = _normalize_optional_text(effort, "effort", lower=True)
            candidate_query = want_model is not None or want_effort is not None
            prepared.append((provider, wanted_tier, task_type, want_model, want_effort, candidate_query))
        rows = self._read_rows()
        return [
            self._stats_from_rows(
                rows, provider, wanted_tier, task_type, moment,
                half_life_seconds, want_model, want_effort, candidate_query)
            for provider, wanted_tier, task_type, want_model, want_effort, candidate_query in prepared
        ]

    # 批量统计的别名（同一实现）。
    batch_stats = stats_many

    def _read_marks(self) -> Dict[str, Mark]:
        try:
            raw = self.marks_path.read_bytes()
        except OSError:
            return {}
        try:
            parsed = json.loads(raw.decode("utf-8"))
        except (ValueError, UnicodeDecodeError):
            return {}
        if not isinstance(parsed, dict):
            return {}
        result: Dict[str, Mark] = {}
        for provider, entry in parsed.items():
            if not isinstance(provider, str) or not provider.strip():
                continue
            if not isinstance(entry, dict):
                continue
            until = entry.get("until")
            reason = entry.get("reason", "")
            if not _is_number(until) or not isinstance(reason, str):
                continue
            result[provider] = Mark(float(until), reason)
        return result

    def mark(self, provider: str, until_ts: Any, reason: str = "") -> Mark:
        """写入一条额度标记：锁内读-改-写，临时文件 + os.replace 原子替换。"""
        if not isinstance(provider, str) or not provider.strip():
            raise ValueError("provider must be a non-empty string")
        if not isinstance(reason, str):
            raise TypeError("reason must be a string")
        until = _coerce_ts(until_ts, "until_ts")
        self._ensure_dir()
        lock_path = self._dir / "marks.json.lock"
        if not _O_NOFOLLOW and os.path.islink(lock_path):
            raise PermissionError(f"refusing symlinked marks lock file: {lock_path}")
        lock_fd = os.open(lock_path, os.O_CREAT | os.O_RDWR | _O_NOFOLLOW, 0o600)
        try:
            if os.name != "nt":
                try:
                    os.fchmod(lock_fd, 0o600)
                except OSError:
                    pass
            filelock.flock(lock_fd, filelock.LOCK_EX)
            try:
                marks = self._read_marks()
                marks[provider] = Mark(until, reason)
                payload = json.dumps(
                    {name: {"until": item.until, "reason": item.reason} for name, item in marks.items()},
                    ensure_ascii=False,
                    allow_nan=False,
                    sort_keys=True,
                )
                fd, temporary = tempfile.mkstemp(prefix=".marks-", suffix=".tmp", dir=str(self._dir))
                temporary_path: Optional[str] = temporary
                try:
                    with os.fdopen(fd, "w", encoding="utf-8") as stream:
                        if os.name != "nt":
                            try:
                                os.fchmod(stream.fileno(), 0o600)
                            except OSError:
                                pass
                        stream.write(payload)
                        stream.flush()
                        os.fsync(stream.fileno())
                    os.replace(temporary, self.marks_path)
                    temporary_path = None
                    self._fsync_dir()
                finally:
                    if temporary_path is not None:
                        try:
                            os.unlink(temporary_path)
                        except FileNotFoundError:
                            pass
            finally:
                filelock.flock(lock_fd, filelock.LOCK_UN)
        finally:
            os.close(lock_fd)
        return Mark(until, reason)

    def active_marks(self, now: Any = None) -> Dict[str, Mark]:
        """只返回没过期的标记（until > now，到点即过期）；marks.json 损坏时当成空。"""
        moment = time.time() if now is None else _coerce_ts(now, "now")
        return {name: item for name, item in self._read_marks().items() if item.until > moment}


def default_ledger() -> OutcomeLedger:
    """按当前环境（XDG_STATE_HOME 等）解析出的默认账本。"""
    return OutcomeLedger()
